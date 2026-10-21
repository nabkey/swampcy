//go:build linux || darwin

package codec

import (
	"encoding/binary"
	"fmt"
	"math"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// OpenH264 is Cisco's H.264 encoder, a C++ library with a C face: an encoder is a
// pointer to a vtable pointer (codec_api.h, `typedef const ISVCEncoderVtbl*
// ISVCEncoder`), and every entry takes that object first. Its parameters are C
// structs, which are kept here as byte buffers with the offsets of the fields swampcy
// touches, computed for x86-64 (System V) from the v2.6.0 headers
// (codec/api/wels/codec_app_def.h, codec_def.h); arm64 Linux (AAPCS64) lays them
// out the same way. v2.5 has the same layout except
// SLayerBSInfo, which v2.6 grew by its rPsnr[3]; the library's own version picks
// the stride.

// Library names, the soname first: v2.6.0's Makefile builds libopenh264.so.8, its
// meson build (and v2.5) .so.7.
var h264Libs = map[string][]string{
	"linux":  {"libopenh264.so.8", "libopenh264.so.7", "libopenh264.so"},
	"darwin": {"libopenh264.8.dylib", "libopenh264.7.dylib", "libopenh264.dylib"},
}

// ISVCEncoderVtbl entries (codec_api.h).
const (
	h264VtInitializeExt    = 1
	h264VtGetDefaultParams = 2
	h264VtUninitialize     = 3
	h264VtEncodeFrame      = 4
	h264VtForceIntraFrame  = 6
	h264VtSetOption        = 7
)

// ENCODER_OPTION values (codec_app_def.h).
const (
	h264OptBitrate    = 5  // ENCODER_OPTION_BITRATE
	h264OptMaxBitrate = 6  // ENCODER_OPTION_MAX_BITRATE
	h264OptTraceLevel = 25 // ENCODER_OPTION_TRACE_LEVEL
)

// Enum values (codec_app_def.h, codec_def.h).
const (
	h264CameraRealTime     = 0  // CAMERA_VIDEO_REAL_TIME
	h264ScreenRealTime     = 1  // SCREEN_CONTENT_REAL_TIME
	h264RCQuality          = 0  // RC_QUALITY_MODE
	h264RCBitrate          = 1  // RC_BITRATE_MODE
	h264LowComplexity      = 0  // LOW_COMPLEXITY
	h264ConstantID         = 0  // CONSTANT_ID
	h264IncreasingID       = 1  // INCREASING_ID
	h264SingleSlice        = 0  // SM_SINGLE_SLICE
	h264ProfileBaseline    = 66 // PRO_BASELINE
	h264ProfileHigh        = 100
	h264VFUndef            = 5 // VF_UNDEF
	h264BT709              = 1 // CP_BT709, TRC_BT709, CM_BT709
	h264Undef              = 2 // CP_UNDEF, TRC_UNDEF, CM_UNDEF
	h264FormatI420         = 23
	h264FrameIDR           = 1 // videoFrameTypeIDR
	h264FrameSkip          = 4 // videoFrameTypeSkip
	h264SpatialLayerAll    = 4 // SPATIAL_LAYER_ALL
	h264LogError           = 1 // WELS_LOG_ERROR
	h264QualityBaseQP      = 26
	h264DefaultSliceSize   = 1500
	h264DefaultIDRRatio    = 400 // IDR_BITRATE_RATIO * 100
	h264DefaultLtrMark     = 30
	h264DefaultMaxFrameHz  = 60 // MAX_FRAME_RATE
	h264MaxSpatialLayers   = 4  // MAX_SPATIAL_LAYER_NUM
	h264MaxLayersPerFrame  = 128
	h264SpatialLayerStride = 200 // sizeof(SSpatialLayerConfig)
)

// SEncParamExt (sizeof 924 in v2.6, 920 in v2.5).
const (
	h264ParamSize = 1024 // a little room past the struct

	h264PUsageType           = 0
	h264PPicWidth            = 4
	h264PPicHeight           = 8
	h264PTargetBitrate       = 12
	h264PRCMode              = 16
	h264PMaxFrameRate        = 20 // float
	h264PTemporalLayerNum    = 24
	h264PSpatialLayerNum     = 28
	h264PSpatialLayers       = 32 // SSpatialLayerConfig[4], 200 bytes each
	h264PComplexityMode      = 832
	h264PIntraPeriod         = 836
	h264PNumRefFrame         = 840
	h264PSpsPpsIDStrategy    = 844
	h264PEntropyCodingMode   = 856
	h264PEnableFrameSkip     = 860 // bool
	h264PMaxBitrate          = 864
	h264PLtrMarkPeriod       = 888
	h264PMultipleThreadIdc   = 892 // unsigned short
	h264PUseLoadBalancing    = 894 // bool
	h264PEnableBackgroundDet = 909 // bool
	h264PEnableAdaptiveQuant = 910 // bool
	h264PEnableFrameCropping = 911 // bool
	h264PEnableSceneChange   = 912 // bool
	h264PFixRCOverShoot      = 914 // bool
	h264PIdrBitrateRatio     = 916

	// SSpatialLayerConfig, relative to its start.
	h264LVideoWidth         = 0
	h264LVideoHeight        = 4
	h264LFrameRate          = 8 // float
	h264LSpatialBitrate     = 12
	h264LMaxSpatialBitrate  = 16
	h264LProfileIdc         = 20
	h264LLevelIdc           = 24
	h264LDLayerQp           = 28
	h264LSliceMode          = 32  // sSliceArgument.uiSliceMode
	h264LSliceSizeLimit     = 180 // sSliceArgument.uiSliceSizeConstraint (8 + 35*4 + 32)
	h264LVideoSignalType    = 184 // bool bVideoSignalTypePresent
	h264LVideoFormat        = 185
	h264LFullRange          = 186 // bool
	h264LColorDescription   = 187 // bool
	h264LColorPrimaries     = 188
	h264LTransferCharacters = 189
	h264LColorMatrix        = 190
)

// SSourcePicture (sizeof 80 in v2.6, 72 in v2.5).
const (
	h264SrcSize      = 80
	h264SColorFormat = 0
	h264SStride      = 4  // int[4]
	h264SData        = 24 // unsigned char*[4]
	h264SPicWidth    = 56
	h264SPicHeight   = 60
	h264STimeStampMs = 64 // long long
	h264BsHeader     = 8  // SFrameBSInfo.iLayerNum and padding before sLayerInfo
	h264LbNalCount   = 16 // SLayerBSInfo.iNalCount
	h264LbNalLengths = 24 // SLayerBSInfo.pNalLengthInByte (int*)
	h264LbBsBuf      = 32 // SLayerBSInfo.pBsBuf
	h264LbFrameType  = 4  // SLayerBSInfo.eFrameType
	h264MaxNalCnt    = 1 << 16
)

type h264Library struct {
	create, destroy uintptr
	version         [4]uint32 // major, minor, revision, reserved
	layerStride     int       // sizeof(SLayerBSInfo)
}

var (
	h264Once sync.Once
	h264Lib  *h264Library
	h264Err  error
)

func loadH264() (*h264Library, error) {
	h264Once.Do(func() {
		var handle uintptr
		var errs []string
		for _, name := range h264Libs[runtime.GOOS] {
			h, err := purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_LOCAL)
			if err == nil {
				handle = h
				break
			}
			errs = append(errs, err.Error())
		}
		if handle == 0 {
			h264Err = fmt.Errorf("openh264: %w: %s", ErrUnavailable, strings.Join(errs, "; "))
			return
		}
		lib := &h264Library{}
		var err error
		if lib.create, err = purego.Dlsym(handle, "WelsCreateSVCEncoder"); err != nil {
			h264Err = fmt.Errorf("openh264: %w", err)
			return
		}
		if lib.destroy, err = purego.Dlsym(handle, "WelsDestroySVCEncoder"); err != nil {
			h264Err = fmt.Errorf("openh264: %w", err)
			return
		}
		// WelsGetCodecVersion returns the struct by value; the Ex form fills a pointer.
		verFn, err := purego.Dlsym(handle, "WelsGetCodecVersionEx")
		if err != nil {
			h264Err = fmt.Errorf("openh264: %w", err)
			return
		}
		ver := new([4]uint32)
		var pin runtime.Pinner
		pin.Pin(ver)
		purego.SyscallN(verFn, uintptr(unsafe.Pointer(ver)))
		pin.Unpin()
		lib.version = *ver
		switch {
		case ver[0] == 2 && ver[1] == 6:
			lib.layerStride = 56 // v2.6 added float rPsnr[3]
		case ver[0] == 2 && ver[1] == 5:
			lib.layerStride = 40
		default:
			h264Err = fmt.Errorf("openh264 %d.%d.%d is not supported (want 2.5 or 2.6, whose struct layouts this binding knows)", ver[0], ver[1], ver[2])
			return
		}
		h264Lib = lib
	})
	return h264Lib, h264Err
}

// h264Encoder is not safe for concurrent use: one goroutine encodes.
type h264Encoder struct {
	lib     *h264Library
	enc     unsafe.Pointer // ISVCEncoder*
	cfg     Config
	bitrate int // bit/s
	src     []byte
	bs      []byte
	closed  bool
}

// NewH264 opens an OpenH264 encoder in its screen-content real-time mode.
func NewH264(cfg Config) (Encoder, error) {
	lib, err := loadH264()
	if err != nil {
		return nil, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width%2 != 0 || cfg.Height%2 != 0 {
		return nil, fmt.Errorf("openh264: size %dx%d must be positive and even", cfg.Width, cfg.Height)
	}
	if cfg.FPS <= 0 {
		cfg.FPS = 60
	}
	if cfg.BitrateKbps <= 0 {
		cfg.BitrateKbps = 4000
	}
	if cfg.Threads <= 0 {
		cfg.Threads = 4
	}
	if cfg.H264Profile == "" {
		cfg.H264Profile = "baseline"
	}
	if cfg.H264Profile != "baseline" && cfg.H264Profile != "high" {
		return nil, fmt.Errorf("openh264: profile %q is neither baseline nor high", cfg.H264Profile)
	}

	encp := new(unsafe.Pointer)
	var pin runtime.Pinner
	pin.Pin(encp)
	r, _, _ := purego.SyscallN(lib.create, uintptr(unsafe.Pointer(encp)))
	pin.Unpin()
	if int32(r) != 0 || *encp == nil {
		return nil, fmt.Errorf("openh264: WelsCreateSVCEncoder failed (%d)", int32(r))
	}
	e := &h264Encoder{
		lib: lib, enc: *encp, cfg: cfg, bitrate: cfg.BitrateKbps * 1000,
		src: make([]byte, h264SrcSize),
		bs:  make([]byte, h264BsHeader+h264MaxLayersPerFrame*lib.layerStride+16),
	}
	if err := e.init(); err != nil {
		e.destroy()
		return nil, err
	}
	return e, nil
}

func (e *h264Encoder) call(entry int, args ...uintptr) int32 {
	vtbl := *(*unsafe.Pointer)(e.enc)
	fn := *(*uintptr)(unsafe.Add(vtbl, entry*int(unsafe.Sizeof(uintptr(0)))))
	r, _, _ := purego.SyscallN(fn, append([]uintptr{uintptr(e.enc)}, args...)...)
	return int32(r)
}

// callWith calls an entry with pointers to Go buffers, pinned for the call.
func (e *h264Encoder) callWith(entry int, bufs ...[]byte) int32 {
	var pin runtime.Pinner
	defer pin.Unpin()
	args := make([]uintptr, len(bufs))
	for i, b := range bufs {
		pin.Pin(&b[0])
		args[i] = uintptr(unsafe.Pointer(&b[0]))
	}
	return e.call(entry, args...)
}

func (e *h264Encoder) setOption(opt int, value []byte) int32 {
	var pin runtime.Pinner
	defer pin.Unpin()
	pin.Pin(&value[0])
	return e.call(h264VtSetOption, uintptr(opt), uintptr(unsafe.Pointer(&value[0])))
}

func (e *h264Encoder) init() error {
	// Warnings and info go to stderr otherwise, which is the MCP log.
	if r := e.setOption(h264OptTraceLevel, h264LE32(h264LogError)); r != 0 {
		return fmt.Errorf("openh264: set trace level: %d", r)
	}
	p := make([]byte, h264ParamSize)
	if r := e.callWith(h264VtGetDefaultParams, p); r != 0 {
		return fmt.Errorf("openh264: GetDefaultParams: %d", r)
	}
	if err := h264CheckDefaults(p); err != nil {
		return err
	}

	c := e.cfg
	h264Put32(p, h264PUsageType, h264ScreenRealTime)
	h264Put32(p, h264PPicWidth, int32(c.Width))
	h264Put32(p, h264PPicHeight, int32(c.Height))
	h264Put32(p, h264PTargetBitrate, int32(e.bitrate))
	h264Put32(p, h264PRCMode, h264RCBitrate)
	h264PutF32(p, h264PMaxFrameRate, float32(c.FPS))
	h264Put32(p, h264PTemporalLayerNum, 1)
	h264Put32(p, h264PSpatialLayerNum, 1)
	h264Put32(p, h264PComplexityMode, h264LowComplexity)
	h264Put32(p, h264PIntraPeriod, 0) // IDR only at the start and on request
	h264Put32(p, h264PSpsPpsIDStrategy, h264ConstantID)
	p[h264PEnableFrameSkip] = 0
	h264Put32(p, h264PMaxBitrate, int32(e.bitrate))
	binary.LittleEndian.PutUint16(p[h264PMultipleThreadIdc:], uint16(c.Threads))
	profile := int32(h264ProfileBaseline)
	if c.H264Profile == "high" {
		profile = h264ProfileHigh
		h264Put32(p, h264PEntropyCodingMode, 1) // CABAC
	} else {
		h264Put32(p, h264PEntropyCodingMode, 0) // CAVLC
	}

	l := h264PSpatialLayers
	h264Put32(p, l+h264LVideoWidth, int32(c.Width))
	h264Put32(p, l+h264LVideoHeight, int32(c.Height))
	h264PutF32(p, l+h264LFrameRate, float32(c.FPS))
	h264Put32(p, l+h264LSpatialBitrate, int32(e.bitrate))
	h264Put32(p, l+h264LMaxSpatialBitrate, int32(e.bitrate))
	h264Put32(p, l+h264LProfileIdc, profile)
	h264Put32(p, l+h264LLevelIdc, 0) // the encoder picks the level
	h264Put32(p, l+h264LSliceMode, h264SingleSlice)
	p[l+h264LVideoSignalType] = 1
	p[l+h264LVideoFormat] = h264VFUndef
	p[l+h264LFullRange] = 1
	p[l+h264LColorDescription] = 1
	p[l+h264LColorPrimaries] = h264BT709
	p[l+h264LTransferCharacters] = h264BT709
	p[l+h264LColorMatrix] = h264BT709

	if r := e.callWith(h264VtInitializeExt, p); r != 0 {
		return fmt.Errorf("openh264: InitializeExt %dx%d: %d", c.Width, c.Height, r)
	}
	return nil
}

// h264CheckDefaults compares what GetDefaultParams wrote with SWelsSvcCodingParam::
// FillDefault in v2.6.0 (codec/encoder/core/inc/param_svc.h). A field in the wrong
// place means the offsets above do not fit this library.
func h264CheckDefaults(p []byte) error {
	type want struct {
		name string
		got  int64
		want int64
	}
	last := h264PSpatialLayers + (h264MaxSpatialLayers-1)*h264SpatialLayerStride
	checks := []want{
		{"iUsageType", int64(h264Get32(p, h264PUsageType)), h264CameraRealTime},
		{"iTargetBitrate", int64(h264Get32(p, h264PTargetBitrate)), 0},
		{"iRCMode", int64(h264Get32(p, h264PRCMode)), h264RCQuality},
		{"fMaxFrameRate", int64(h264GetF32(p, h264PMaxFrameRate)), h264DefaultMaxFrameHz},
		{"iTemporalLayerNum", int64(h264Get32(p, h264PTemporalLayerNum)), 1},
		{"iSpatialLayerNum", int64(h264Get32(p, h264PSpatialLayerNum)), 1},
		{"sSpatialLayers[0].fFrameRate", int64(h264GetF32(p, h264PSpatialLayers+h264LFrameRate)), h264DefaultMaxFrameHz},
		{"sSpatialLayers[0].iDLayerQp", int64(h264Get32(p, h264PSpatialLayers+h264LDLayerQp)), h264QualityBaseQP},
		{"sSpatialLayers[0].uiSliceSizeConstraint", int64(h264Get32(p, h264PSpatialLayers+h264LSliceSizeLimit)), h264DefaultSliceSize},
		{"sSpatialLayers[0].uiVideoFormat", int64(p[h264PSpatialLayers+h264LVideoFormat]), h264VFUndef},
		{"sSpatialLayers[0].uiColorPrimaries", int64(p[h264PSpatialLayers+h264LColorPrimaries]), h264Undef},
		{"sSpatialLayers[0].uiColorMatrix", int64(p[h264PSpatialLayers+h264LColorMatrix]), h264Undef},
		{"sSpatialLayers[3].iDLayerQp", int64(h264Get32(p, last+h264LDLayerQp)), h264QualityBaseQP},
		{"sSpatialLayers[3].uiSliceSizeConstraint", int64(h264Get32(p, last+h264LSliceSizeLimit)), h264DefaultSliceSize},
		{"sSpatialLayers[3].uiTransferCharacteristics", int64(p[last+h264LTransferCharacters]), h264Undef},
		{"iComplexityMode", int64(h264Get32(p, h264PComplexityMode)), h264LowComplexity},
		{"uiIntraPeriod", int64(h264Get32(p, h264PIntraPeriod)), 0},
		{"iNumRefFrame", int64(h264Get32(p, h264PNumRefFrame)), -1},
		{"eSpsPpsIdStrategy", int64(h264Get32(p, h264PSpsPpsIDStrategy)), h264IncreasingID},
		{"bEnableFrameSkip", int64(p[h264PEnableFrameSkip]), 1},
		{"iLtrMarkPeriod", int64(h264Get32(p, h264PLtrMarkPeriod)), h264DefaultLtrMark},
		{"iMultipleThreadIdc", int64(binary.LittleEndian.Uint16(p[h264PMultipleThreadIdc:])), 1},
		{"bUseLoadBalancing", int64(p[h264PUseLoadBalancing]), 1},
		{"bEnableBackgroundDetection", int64(p[h264PEnableBackgroundDet]), 1},
		{"bEnableAdaptiveQuant", int64(p[h264PEnableAdaptiveQuant]), 1},
		{"bEnableFrameCroppingFlag", int64(p[h264PEnableFrameCropping]), 1},
		{"bEnableSceneChangeDetect", int64(p[h264PEnableSceneChange]), 1},
		{"bFixRCOverShoot", int64(p[h264PFixRCOverShoot]), 1},
		{"iIdrBitrateRatio", int64(h264Get32(p, h264PIdrBitrateRatio)), h264DefaultIDRRatio},
	}
	for _, c := range checks {
		if c.got != c.want {
			return fmt.Errorf("openh264 ABI mismatch: default %s is %d, want %d", c.name, c.got, c.want)
		}
	}
	return nil
}

func (e *h264Encoder) Name() string { return "H264" }

func (e *h264Encoder) Encode(f *Frame, forceKey bool) (Packet, error) {
	if e.closed {
		return Packet{}, fmt.Errorf("openh264: encoder closed")
	}
	w, h := e.cfg.Width, e.cfg.Height
	cw, ch := (w+1)/2, (h+1)/2
	if f.YStride < w || f.UStride < cw || len(f.Y) < f.YStride*(h-1)+w ||
		len(f.U) < f.UStride*(ch-1)+cw || len(f.V) < f.UStride*(ch-1)+cw {
		return Packet{}, fmt.Errorf("openh264: frame is smaller than %dx%d", w, h)
	}
	if forceKey {
		// The C++ method is ForceIntraFrame(bool bIDR, int iLayerId = -1); the C
		// vtable entry omits the default argument, so it is passed here.
		if r := e.call(h264VtForceIntraFrame, 1, ^uintptr(0)); r != 0 {
			return Packet{}, fmt.Errorf("openh264: ForceIntraFrame: %d", r)
		}
	}

	var pin runtime.Pinner
	defer pin.Unpin()
	pin.Pin(&f.Y[0])
	pin.Pin(&f.U[0])
	pin.Pin(&f.V[0])
	pin.Pin(&e.src[0])
	pin.Pin(&e.bs[0])
	s := e.src
	clear(s)
	h264Put32(s, h264SColorFormat, h264FormatI420)
	h264Put32(s, h264SStride, int32(f.YStride))
	h264Put32(s, h264SStride+4, int32(f.UStride))
	h264Put32(s, h264SStride+8, int32(f.UStride))
	// Addresses go in as integers: the planes are pinned, so they neither move nor
	// die before the call returns.
	binary.LittleEndian.PutUint64(s[h264SData:], uint64(uintptr(unsafe.Pointer(&f.Y[0]))))
	binary.LittleEndian.PutUint64(s[h264SData+8:], uint64(uintptr(unsafe.Pointer(&f.U[0]))))
	binary.LittleEndian.PutUint64(s[h264SData+16:], uint64(uintptr(unsafe.Pointer(&f.V[0]))))
	h264Put32(s, h264SPicWidth, int32(w))
	h264Put32(s, h264SPicHeight, int32(h))
	binary.LittleEndian.PutUint64(s[h264STimeStampMs:], uint64(f.PTS.Milliseconds()))
	clear(e.bs)

	r := e.call(h264VtEncodeFrame, uintptr(unsafe.Pointer(&s[0])), uintptr(unsafe.Pointer(&e.bs[0])))
	// Drop the addresses of Go memory before the planes are unpinned.
	clear(s[h264SData : h264SData+32])
	if r != 0 {
		return Packet{}, fmt.Errorf("openh264: EncodeFrame: %d", r)
	}
	return e.collect(f)
}

// collect copies every layer's NAL units out of the encoder's buffers, which it
// reuses on the next call.
func (e *h264Encoder) collect(f *Frame) (Packet, error) {
	bs := e.bs
	stride := e.lib.layerStride
	tail := h264BsHeader + h264MaxLayersPerFrame*stride
	frameType := h264Get32(bs, tail)
	pkt := Packet{PTS: f.PTS, Keyframe: frameType == h264FrameIDR}
	if frameType == h264FrameSkip {
		return pkt, nil
	}
	layers := int(h264Get32(bs, 0))
	if layers < 0 || layers > h264MaxLayersPerFrame {
		return Packet{}, fmt.Errorf("openh264: %d layers", layers)
	}
	size := int(h264Get32(bs, tail+4))
	if size < 0 {
		return Packet{}, fmt.Errorf("openh264: frame size %d", size)
	}
	out := make([]byte, 0, size)
	for i := range layers {
		lb := h264BsHeader + i*stride
		nals := int(h264Get32(bs, lb+h264LbNalCount))
		if nals <= 0 {
			continue
		}
		if nals > h264MaxNalCnt {
			return Packet{}, fmt.Errorf("openh264: layer %d has %d NAL units", i, nals)
		}
		lengths := *(*unsafe.Pointer)(unsafe.Pointer(&bs[lb+h264LbNalLengths]))
		buf := *(*unsafe.Pointer)(unsafe.Pointer(&bs[lb+h264LbBsBuf]))
		if lengths == nil || buf == nil {
			continue
		}
		total := 0
		for _, n := range unsafe.Slice((*int32)(lengths), nals) {
			if n < 0 {
				return Packet{}, fmt.Errorf("openh264: NAL length %d", n)
			}
			total += int(n)
		}
		if total > 0 {
			out = append(out, unsafe.Slice((*byte)(buf), total)...)
		}
		if t := h264Get32(bs, lb+h264LbFrameType); t == h264FrameIDR {
			pkt.Keyframe = true
		}
	}
	pkt.Data = out
	if !pkt.Keyframe {
		pkt.Keyframe = h264HasNAL(out, 5)
	}
	return pkt, nil
}

func (e *h264Encoder) SetBitrate(kbps int) error {
	if e.closed {
		return fmt.Errorf("openh264: encoder closed")
	}
	if kbps <= 0 {
		return fmt.Errorf("openh264: bitrate %d kbit/s", kbps)
	}
	bps := kbps * 1000
	if bps > math.MaxInt32 {
		bps = math.MaxInt32
	}
	set := func(opt int, layer int32) error {
		v := make([]byte, 8) // SBitrateInfo{iLayer, iBitrate}
		h264Put32(v, 0, layer)
		h264Put32(v, 4, int32(bps))
		if r := e.setOption(opt, v); r != 0 {
			return fmt.Errorf("openh264: set bitrate option %d (layer %d) to %d: %d", opt, layer, bps, r)
		}
		return nil
	}
	// MAX_BITRATE for SPATIAL_LAYER_ALL sets only the stream's cap; the target is
	// checked against the layer's own cap, so layer 0's is set too. The target
	// stays at or below the caps at every step.
	type step struct {
		opt   int
		layer int32
	}
	order := []step{{h264OptBitrate, h264SpatialLayerAll}, {h264OptMaxBitrate, 0}, {h264OptMaxBitrate, h264SpatialLayerAll}}
	if bps > e.bitrate {
		order = []step{{h264OptMaxBitrate, h264SpatialLayerAll}, {h264OptMaxBitrate, 0}, {h264OptBitrate, h264SpatialLayerAll}}
	}
	for _, st := range order {
		if err := set(st.opt, st.layer); err != nil {
			return err
		}
	}
	e.bitrate = bps
	return nil
}

func (e *h264Encoder) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true
	e.call(h264VtUninitialize)
	e.destroy()
	return nil
}

func (e *h264Encoder) destroy() {
	if e.enc != nil {
		purego.SyscallN(e.lib.destroy, uintptr(e.enc))
		e.enc = nil
	}
}

// h264HasNAL reports whether an Annex B stream holds a NAL unit of type t.
func h264HasNAL(b []byte, t byte) bool {
	for _, n := range h264NALTypes(b) {
		if n == t {
			return true
		}
	}
	return false
}

// h264NALTypes lists the NAL unit types in an Annex B stream, in order.
func h264NALTypes(b []byte) []byte {
	var types []byte
	for i := 0; i+3 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			types = append(types, b[i+3]&0x1f)
			i += 3
		}
	}
	return types
}

func h264Put32(b []byte, off int, v int32) { binary.LittleEndian.PutUint32(b[off:], uint32(v)) }
func h264Get32(b []byte, off int) int32    { return int32(binary.LittleEndian.Uint32(b[off:])) }
func h264PutF32(b []byte, off int, v float32) {
	binary.LittleEndian.PutUint32(b[off:], math.Float32bits(v))
}
func h264GetF32(b []byte, off int) float32 {
	return math.Float32frombits(binary.LittleEndian.Uint32(b[off:]))
}

func h264LE32(v int32) []byte {
	b := make([]byte, 4)
	h264Put32(b, 0, v)
	return b
}
