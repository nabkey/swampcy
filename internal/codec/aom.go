//go:build linux || darwin

package codec

import (
	"fmt"
	"strconv"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// libaom is loaded at run time and called through purego, so its C structs are
// laid out here by hand. Every offset below is from the libaom v3.13.3 headers
// (aom/aom_image.h, aom/aom_codec.h, aom/aom_encoder.h, aom/aomcx.h; Fedora 44
// ships 3.13.3) for x86-64 System V, and the realtime defaults the self-check
// compares against are encoder_usage_cfg[] in av1/av1_cx_iface.c of the same tag.
// arm64 Linux (AAPCS64) lays these structs out the same way; the self-check and
// the encoder tests pass there too, against libaom 3.15.2.
// A layout that does not match fails the self-check at open rather than
// corrupting memory later.

const (
	aomImageABIVersion             = 9                           // AOM_IMAGE_ABI_VERSION
	aomCodecABIVersion             = 7 + aomImageABIVersion      // AOM_CODEC_ABI_VERSION
	aomEncoderABIVersion           = 10 + aomCodecABIVersion + 3 // AOM_ENCODER_ABI_VERSION (3 is the old AOM_EXT_PART_ABI_VERSION)
	aomUsageRealtime               = 1                           // AOM_USAGE_REALTIME
	aomImgFmtI420                  = 0x100 | 2                   // AOM_IMG_FMT_I420 = AOM_IMG_FMT_PLANAR | 2
	aomEflagForceKF                = 1 << 0                      // AOM_EFLAG_FORCE_KF
	aomFrameIsKey                  = 0x1                         // AOM_FRAME_IS_KEY
	aomCodecCxFramePkt             = 0                           // AOM_CODEC_CX_FRAME_PKT
	aomRCOnePass                   = 0                           // AOM_RC_ONE_PASS
	aomCBR                         = 1                           // enum aom_rc_mode: AOM_VBR, AOM_CBR, AOM_CQ, AOM_Q
	aomCQ                          = 2                           //
	aomKFDisabled                  = 0                           // AOM_KF_DISABLED
	aomKFAuto                      = 1                           // AOM_KF_AUTO
	aomBits8                       = 8                           // AOM_BITS_8
	aomCICPBT709                   = 1                           // AOM_CICP_CP_BT_709, AOM_CICP_TC_BT_709, AOM_CICP_MC_BT_709
	aomCRFullRange                 = 1                           // AOM_CR_FULL_RANGE
	av1eSetColorPrimaries          = 45                          // AV1E_SET_COLOR_PRIMARIES
	av1eSetTransferCharacteristics = 46                          // AV1E_SET_TRANSFER_CHARACTERISTICS
	av1eSetMatrixCoefficients      = 47                          // AV1E_SET_MATRIX_COEFFICIENTS
	av1eSetColorRange              = 52                          // AV1E_SET_COLOR_RANGE
)

// Sizes of the structs swampcy allocates for libaom. Each is the exact sizeof;
// the buffers are rounded up to whole 8-byte words.
const (
	aomCtxSize = 56  // aom_codec_ctx_t: name, iface, err (+4 pad), err_detail, init_flags, config, priv
	aomCfgSize = 904 // aom_codec_enc_cfg_t, cfg_options_t (35 uints) included
	aomImgSize = 168 // aom_image_t
)

// Offsets in aom_codec_enc_cfg_t.
const (
	cfgUsage            = 0   // unsigned int g_usage
	cfgThreads          = 4   // unsigned int g_threads
	cfgProfile          = 8   // unsigned int g_profile
	cfgW                = 12  // unsigned int g_w
	cfgH                = 16  // unsigned int g_h
	cfgBitDepth         = 32  // aom_bit_depth_t g_bit_depth
	cfgInputBitDepth    = 36  // unsigned int g_input_bit_depth
	cfgTimebaseNum      = 40  // struct aom_rational g_timebase .num
	cfgTimebaseDen      = 44  // .den
	cfgErrorResilient   = 48  // aom_codec_er_flags_t g_error_resilient
	cfgPass             = 52  // enum aom_enc_pass g_pass
	cfgLagInFrames      = 56  // unsigned int g_lag_in_frames
	cfgDropframeThresh  = 60  // unsigned int rc_dropframe_thresh
	cfgEndUsage         = 96  // enum aom_rc_mode rc_end_usage (then 4 bytes of padding before two aom_fixed_buf_t)
	cfgTargetBitrate    = 136 // unsigned int rc_target_bitrate
	cfgMaxQuantizer     = 144 // unsigned int rc_max_quantizer
	cfgUndershootPct    = 148 // unsigned int rc_undershoot_pct
	cfgOvershootPct     = 152 // unsigned int rc_overshoot_pct
	cfgBufSz            = 156 // unsigned int rc_buf_sz
	cfgBufInitialSz     = 160 // unsigned int rc_buf_initial_sz
	cfgBufOptimalSz     = 164 // unsigned int rc_buf_optimal_sz
	cfgVBRBiasPct       = 168 // unsigned int rc_2pass_vbr_bias_pct
	cfgVBRMaxSectionPct = 176 // unsigned int rc_2pass_vbr_maxsection_pct
	cfgKFMode           = 184 // enum aom_kf_mode kf_mode
	cfgKFMaxDist        = 192 // unsigned int kf_max_dist
	cfgSframeMode       = 200 // unsigned int sframe_mode
)

// Offsets in aom_image_t.
const (
	imgPlanes = 64 // unsigned char *planes[3]
	imgStride = 88 // int stride[3]
)

// Offsets in aom_codec_cx_pkt_t.
const (
	pktKind  = 0  // enum aom_codec_cx_pkt_kind kind (then 4 bytes of padding before the union)
	pktBuf   = 8  // data.frame.buf
	pktSz    = 16 // data.frame.sz
	pktFlags = 40 // data.frame.flags (aom_codec_frame_flags_t)
)

// aomRealtimeDefaults are what aom_codec_enc_config_default fills in for
// AOM_USAGE_REALTIME (encoder_usage_cfg[] in av1/av1_cx_iface.c, v3.13.3). They
// span the struct, including the padding before rc_twopass_stats_in, so a wrong
// offset or a changed layout shows up as a mismatch.
var aomRealtimeDefaults = []struct {
	off  uintptr
	name string
	want uint32
}{
	{cfgUsage, "g_usage", aomUsageRealtime},
	{cfgW, "g_w", 320},
	{cfgH, "g_h", 240},
	{cfgBitDepth, "g_bit_depth", aomBits8},
	{cfgInputBitDepth, "g_input_bit_depth", 8},
	{cfgTimebaseNum, "g_timebase.num", 1},
	{cfgTimebaseDen, "g_timebase.den", 30},
	{cfgPass, "g_pass", aomRCOnePass},
	{cfgLagInFrames, "g_lag_in_frames", 0},
	{cfgEndUsage, "rc_end_usage", aomCBR},
	{cfgTargetBitrate, "rc_target_bitrate", 256},
	{cfgMaxQuantizer, "rc_max_quantizer", 63},
	// rc_undershoot_pct, rc_overshoot_pct and the rc_buf sizes are left out: their
	// real-time defaults changed between libaom releases (25/25 and 6000/4000/5000
	// in 3.8; 50/50 and 1000/600/600 in 3.13) while the layout did not. The fields
	// after them pin the layout instead.
	{cfgVBRBiasPct, "rc_2pass_vbr_bias_pct", 50},
	{cfgVBRMaxSectionPct, "rc_2pass_vbr_maxsection_pct", 2000},
	{cfgKFMode, "kf_mode", aomKFAuto},
	{cfgKFMaxDist, "kf_max_dist", 9999},
	{cfgSframeMode, "sframe_mode", 1},
}

// aomLib is libaom's encoder API.
type aomLib struct {
	av1Cx         func() unsafe.Pointer
	configDefault func(iface, cfg unsafe.Pointer, usage uint32) int32
	encInitVer    func(ctx, iface, cfg unsafe.Pointer, flags int64, ver int32) int32
	setOption     func(ctx unsafe.Pointer, name, value string) int32
	control       func(ctx unsafe.Pointer, id int32, value int32) int32 // aom_codec_control is variadic; an int vararg goes in the same register
	configSet     func(ctx, cfg unsafe.Pointer) int32
	imgWrap       func(img unsafe.Pointer, fmt uint32, w, h, align uint32, data unsafe.Pointer) unsafe.Pointer
	encode        func(ctx, img unsafe.Pointer, pts int64, duration uint64, flags int64) int32
	getCxData     func(ctx unsafe.Pointer, iter *uintptr) unsafe.Pointer
	destroy       func(ctx unsafe.Pointer) int32
	codecError    func(ctx unsafe.Pointer) *byte
	errorDetail   func(ctx unsafe.Pointer) *byte
}

var (
	aomOnce   sync.Once
	aomLoaded *aomLib
	aomErr    error
)

func loadAOM() (*aomLib, error) {
	aomOnce.Do(func() {
		var lib uintptr
		var err error
		for _, name := range []string{"libaom.so.3", "libaom.so"} {
			if lib, err = purego.Dlopen(name, purego.RTLD_NOW); err == nil {
				break
			}
		}
		if err != nil {
			aomErr = fmt.Errorf("%w: libaom: %v", ErrUnavailable, err)
			return
		}
		l := &aomLib{}
		for _, s := range []struct {
			fn   any
			name string
		}{
			{&l.av1Cx, "aom_codec_av1_cx"},
			{&l.configDefault, "aom_codec_enc_config_default"},
			{&l.encInitVer, "aom_codec_enc_init_ver"},
			{&l.setOption, "aom_codec_set_option"},
			{&l.control, "aom_codec_control"},
			{&l.configSet, "aom_codec_enc_config_set"},
			{&l.imgWrap, "aom_img_wrap"},
			{&l.encode, "aom_codec_encode"},
			{&l.getCxData, "aom_codec_get_cx_data"},
			{&l.destroy, "aom_codec_destroy"},
			{&l.codecError, "aom_codec_error"},
			{&l.errorDetail, "aom_codec_error_detail"},
		} {
			sym, err := purego.Dlsym(lib, s.name)
			if err != nil {
				aomErr = fmt.Errorf("%w: libaom: %s: %v", ErrUnavailable, s.name, err)
				return
			}
			purego.RegisterFunc(s.fn, sym)
		}
		aomLoaded = l
	})
	return aomLoaded, aomErr
}

// cstring copies a C string; nil is "".
func cstring(p *byte) string {
	if p == nil {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Add(unsafe.Pointer(p), n)) != 0 {
		n++
	}
	return string(unsafe.Slice(p, n))
}

// words allocates n bytes of 8-byte-aligned, zeroed Go memory that libaom reads
// and writes. Go's collector does not move heap objects, and the encoder keeps a
// reference to each buffer for as long as libaom may use it.
func words(n int) unsafe.Pointer {
	return unsafe.Pointer(&make([]uint64, (n+7)/8)[0])
}

func getU32(base unsafe.Pointer, off uintptr) uint32 { return *(*uint32)(unsafe.Add(base, off)) }
func setU32(base unsafe.Pointer, off uintptr, v uint32) {
	*(*uint32)(unsafe.Add(base, off)) = v
}

// aomEncoder is an AV1 encoder. It is not safe for concurrent use: one
// goroutine encodes, changes the bitrate and closes it.
type aomEncoder struct {
	lib      *aomLib
	ctx      unsafe.Pointer // aom_codec_ctx_t
	cfg      unsafe.Pointer // aom_codec_enc_cfg_t, kept for SetBitrate
	img      unsafe.Pointer // aom_image_t wrapping buf
	buf      []byte         // the I420 picture libaom reads: Y, then U, then V
	w, h     int
	cw, ch   int
	duration uint64 // one frame at the planned rate, in timebase units (ms)
	lastPTS  int64
	started  bool
	closed   bool
}

// NewAV1 opens libaom's AV1 encoder in realtime mode with its screen-content
// tools, tuned as swampcy's codec benchmark found best for a desktop:
// speed 10, palette and intra block copy on, no lookahead, constrained quality
// at cfg.Quality with cfg.BitrateKbps as the cap, keyframes only on request,
// BT.709 full-range colour.
func NewAV1(cfg Config) (Encoder, error) {
	lib, err := loadAOM()
	if err != nil {
		return nil, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, fmt.Errorf("av1: bad size %dx%d", cfg.Width, cfg.Height)
	}
	threads := cfg.Threads
	if threads <= 0 {
		threads = 4
	}
	fps := cfg.FPS
	if fps <= 0 {
		fps = 60
	}
	kbps := cfg.BitrateKbps
	if kbps <= 0 {
		kbps = 4000
	}
	quality := cfg.Quality
	if quality <= 0 {
		quality = 24
	}
	if quality > 63 {
		quality = 63
	}

	e := &aomEncoder{
		lib: lib, ctx: words(aomCtxSize), cfg: words(aomCfgSize), img: words(aomImgSize),
		w: cfg.Width, h: cfg.Height, cw: (cfg.Width + 1) / 2, ch: (cfg.Height + 1) / 2,
		duration: uint64(max(1000/fps, 1)),
	}
	iface := lib.av1Cx()
	if iface == nil {
		return nil, fmt.Errorf("av1: aom_codec_av1_cx returned no interface")
	}
	if rc := lib.configDefault(iface, e.cfg, aomUsageRealtime); rc != 0 {
		return nil, fmt.Errorf("av1: aom_codec_enc_config_default: error %d", rc)
	}
	for _, d := range aomRealtimeDefaults {
		if got := getU32(e.cfg, d.off); got != d.want {
			return nil, fmt.Errorf("av1: libaom ABI mismatch: realtime default %s at offset %d is %d, want %d (swampcy's layout is libaom 3.x's, encoder ABI 29)", d.name, d.off, got, d.want)
		}
	}

	setU32(e.cfg, cfgThreads, uint32(threads))
	setU32(e.cfg, cfgProfile, 0) // main: 8-bit 4:2:0
	setU32(e.cfg, cfgW, uint32(e.w))
	setU32(e.cfg, cfgH, uint32(e.h))
	setU32(e.cfg, cfgTimebaseNum, 1)
	setU32(e.cfg, cfgTimebaseDen, 1000) // pts in milliseconds
	setU32(e.cfg, cfgErrorResilient, 0)
	setU32(e.cfg, cfgLagInFrames, 0)
	setU32(e.cfg, cfgDropframeThresh, 0) // every frame produces a packet
	setU32(e.cfg, cfgEndUsage, aomCQ)
	setU32(e.cfg, cfgTargetBitrate, uint32(kbps))
	setU32(e.cfg, cfgKFMode, aomKFDisabled) // the first frame, and frames asked for

	if rc := lib.encInitVer(e.ctx, iface, e.cfg, 0, aomEncoderABIVersion); rc != 0 {
		return nil, e.fail("aom_codec_enc_init_ver", rc)
	}
	opts := [][2]string{
		{"cpu-used", "10"},
		{"tune-content", "screen"},
		{"enable-palette", "1"},
		{"enable-intrabc", "1"},
		{"row-mt", "1"},
		{"cq-level", strconv.Itoa(quality)},
	}
	if threads > 1 {
		opts = append(opts, [2]string{"tile-columns", "1"}) // log2: two tile columns
	}
	for _, o := range opts {
		if rc := lib.setOption(e.ctx, o[0], o[1]); rc != 0 {
			err := e.fail("aom_codec_set_option "+o[0]+"="+o[1], rc)
			e.Close()
			return nil, err
		}
	}
	for _, c := range []struct {
		id    int32
		name  string
		value int32
	}{
		{av1eSetColorPrimaries, "AV1E_SET_COLOR_PRIMARIES", aomCICPBT709},
		{av1eSetTransferCharacteristics, "AV1E_SET_TRANSFER_CHARACTERISTICS", aomCICPBT709},
		{av1eSetMatrixCoefficients, "AV1E_SET_MATRIX_COEFFICIENTS", aomCICPBT709},
		{av1eSetColorRange, "AV1E_SET_COLOR_RANGE", aomCRFullRange},
	} {
		if rc := lib.control(e.ctx, c.id, c.value); rc != 0 {
			err := e.fail("aom_codec_control "+c.name, rc)
			e.Close()
			return nil, err
		}
	}

	// One contiguous I420 picture that Encode copies each frame into, wrapped
	// once; the planes and strides are then set explicitly so they match the
	// copy whatever aom_img_wrap's own alignment would have been.
	ySize, cSize := e.w*e.h, e.cw*e.ch
	e.buf = make([]byte, ySize+2*cSize)
	base := unsafe.Pointer(&e.buf[0])
	if lib.imgWrap(e.img, aomImgFmtI420, uint32(e.w), uint32(e.h), 1, base) == nil {
		e.Close()
		return nil, fmt.Errorf("av1: aom_img_wrap failed")
	}
	planes := [3]uintptr{uintptr(base), uintptr(base) + uintptr(ySize), uintptr(base) + uintptr(ySize+cSize)}
	strides := [3]int32{int32(e.w), int32(e.cw), int32(e.cw)}
	for i := range 3 {
		*(*uintptr)(unsafe.Add(e.img, imgPlanes+8*uintptr(i))) = planes[i]
		*(*int32)(unsafe.Add(e.img, imgStride+4*uintptr(i))) = strides[i]
	}
	return e, nil
}

func (e *aomEncoder) fail(op string, rc int32) error {
	msg := cstring(e.lib.codecError(e.ctx))
	if d := cstring(e.lib.errorDetail(e.ctx)); d != "" {
		msg += ": " + d
	}
	return fmt.Errorf("av1: %s: error %d: %s", op, rc, msg)
}

func (e *aomEncoder) Name() string { return "AV1" }

// Encode copies f into libaom's picture and encodes it. With no lookahead every
// frame comes out at once, as one temporal unit.
func (e *aomEncoder) Encode(f *Frame, forceKey bool) (Packet, error) {
	if e.closed {
		return Packet{}, fmt.Errorf("av1: encoder is closed")
	}
	ySize, cSize := e.w*e.h, e.cw*e.ch
	if err := copyPlane(e.buf[:ySize], e.w, f.Y, f.YStride, e.w, e.h); err != nil {
		return Packet{}, fmt.Errorf("av1: Y: %w", err)
	}
	if err := copyPlane(e.buf[ySize:ySize+cSize], e.cw, f.U, f.UStride, e.cw, e.ch); err != nil {
		return Packet{}, fmt.Errorf("av1: U: %w", err)
	}
	if err := copyPlane(e.buf[ySize+cSize:], e.cw, f.V, f.UStride, e.cw, e.ch); err != nil {
		return Packet{}, fmt.Errorf("av1: V: %w", err)
	}

	// libaom wants timestamps that increase; two frames within one millisecond
	// get consecutive ones.
	pts := f.PTS.Milliseconds()
	if e.started && pts <= e.lastPTS {
		pts = e.lastPTS + 1
	}
	e.lastPTS, e.started = pts, true

	var flags int64
	if forceKey {
		flags |= aomEflagForceKF
	}
	if rc := e.lib.encode(e.ctx, e.img, pts, e.duration, flags); rc != 0 {
		return Packet{}, e.fail("aom_codec_encode", rc)
	}

	var out []byte
	key := false
	var iter uintptr
	for {
		p := e.lib.getCxData(e.ctx, &iter)
		if p == nil {
			break
		}
		if *(*int32)(unsafe.Add(p, pktKind)) != aomCodecCxFramePkt {
			continue
		}
		buf := *(*unsafe.Pointer)(unsafe.Add(p, pktBuf))
		sz := *(*uintptr)(unsafe.Add(p, pktSz))
		if *(*uint32)(unsafe.Add(p, pktFlags))&aomFrameIsKey != 0 {
			key = true
		}
		if buf != nil && sz > 0 {
			out = append(out, unsafe.Slice((*byte)(buf), sz)...)
		}
	}
	return Packet{Data: out, Keyframe: key, PTS: f.PTS}, nil
}

// copyPlane copies a w×h plane between strides, refusing a source that is too
// small rather than reading past it.
func copyPlane(dst []byte, dstStride int, src []byte, srcStride, w, h int) error {
	if h == 0 || w == 0 {
		return nil
	}
	if srcStride < w || len(src) < (h-1)*srcStride+w {
		return fmt.Errorf("plane of %d bytes with stride %d is too small for %dx%d", len(src), srcStride, w, h)
	}
	if srcStride == w && dstStride == w {
		copy(dst[:w*h], src[:w*h])
		return nil
	}
	for y := range h {
		copy(dst[y*dstStride:y*dstStride+w], src[y*srcStride:y*srcStride+w])
	}
	return nil
}

// SetBitrate changes rc_target_bitrate, the cap on constrained quality.
func (e *aomEncoder) SetBitrate(kbps int) error {
	if e.closed {
		return fmt.Errorf("av1: encoder is closed")
	}
	if kbps <= 0 {
		return fmt.Errorf("av1: bitrate %d kbit/s", kbps)
	}
	setU32(e.cfg, cfgTargetBitrate, uint32(kbps))
	if rc := e.lib.configSet(e.ctx, e.cfg); rc != 0 {
		return e.fail("aom_codec_enc_config_set", rc)
	}
	return nil
}

// Close frees the encoder. Closing twice is fine.
func (e *aomEncoder) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true
	if rc := e.lib.destroy(e.ctx); rc != 0 {
		return e.fail("aom_codec_destroy", rc)
	}
	return nil
}
