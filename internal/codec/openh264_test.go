//go:build linux || darwin

package codec

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// h264DesktopFrame draws a desktop-like picture: a light background, rows of
// dark text-like strokes, a coloured panel, and a rectangle that moves with i.
func h264DesktopFrame(w, h, i int) *Frame {
	f := NewI420(w, h)
	for y := range h {
		row := f.Y[y*f.YStride:]
		for x := range w {
			v := byte(235)
			if y%24 >= 8 && y%24 < 18 && (x/7+y/24)%5 != 0 && x%7 < 4 && x > 40 && x < w-200 {
				v = 30 // "glyphs"
			}
			row[x] = v
		}
	}
	bx, by := 100+i*20, 300+i*5
	for y := by; y < by+120 && y < h; y++ {
		for x := bx; x < bx+200 && x < w; x++ {
			f.Y[y*f.YStride+x] = 80
		}
	}
	for y := range (h + 1) / 2 {
		for x := range (w + 1) / 2 {
			u, v := byte(128), byte(128)
			if x > w/2-150 && y < 60 {
				u, v = 90, 200 // a reddish title bar
			}
			f.U[y*f.UStride+x], f.V[y*f.UStride+x] = u, v
		}
	}
	return f
}

// h264Open opens an encoder or skips the test when OpenH264 is not installed.
func h264Open(t *testing.T, cfg Config) Encoder {
	t.Helper()
	enc, err := NewH264(cfg)
	if errors.Is(err, ErrUnavailable) {
		t.Skipf("OpenH264 not available: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

// h264SPSProfile returns profile_idc from the first SPS in an Annex B stream.
func h264SPSProfile(b []byte) int {
	for i := 0; i+4 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 && b[i+3]&0x1f == 7 {
			return int(b[i+4])
		}
	}
	return -1
}

func TestH264Encodes(t *testing.T) {
	for _, tc := range []struct {
		profile string
		idc     int
	}{{"baseline", 66}, {"high", 100}} {
		t.Run(tc.profile, func(t *testing.T) {
			const w, h, n = 1280, 800, 20
			enc := h264Open(t, Config{Width: w, Height: h, FPS: 60, BitrateKbps: 2000, Threads: 4, H264Profile: tc.profile})
			defer enc.Close()
			if enc.Name() != "H264" {
				t.Errorf("Name = %q", enc.Name())
			}
			var stream bytes.Buffer
			for i := range n {
				if i == 10 {
					if err := enc.SetBitrate(500); err != nil {
						t.Fatal(err)
					}
				}
				if i == 12 {
					if err := enc.SetBitrate(3000); err != nil {
						t.Fatal(err)
					}
				}
				pkt, err := enc.Encode(h264DesktopFrame(w, h, i), i == 15)
				if err != nil {
					t.Fatalf("frame %d: %v", i, err)
				}
				if pkt.PTS != time.Duration(0) && i == 0 {
					t.Errorf("frame 0 PTS = %v", pkt.PTS)
				}
				types := h264NALTypes(pkt.Data)
				switch i {
				case 0:
					if !pkt.Keyframe || !h264HasNAL(pkt.Data, 7) || !h264HasNAL(pkt.Data, 8) || !h264HasNAL(pkt.Data, 5) {
						t.Fatalf("first frame: keyframe=%v NAL types %v, want SPS, PPS and IDR", pkt.Keyframe, types)
					}
					if !bytes.HasPrefix(pkt.Data, []byte{0, 0, 0, 1}) && !bytes.HasPrefix(pkt.Data, []byte{0, 0, 1}) {
						t.Errorf("first frame does not start with a start code: % x", pkt.Data[:min(8, len(pkt.Data))])
					}
					if got := h264SPSProfile(pkt.Data); got != tc.idc {
						t.Errorf("SPS profile_idc = %d, want %d", got, tc.idc)
					}
				case 15:
					if !pkt.Keyframe || !h264HasNAL(pkt.Data, 5) {
						t.Errorf("forced frame: keyframe=%v NAL types %v, want IDR", pkt.Keyframe, types)
					}
				default:
					if pkt.Keyframe || h264HasNAL(pkt.Data, 5) {
						t.Errorf("frame %d: unexpected keyframe (NAL types %v)", i, types)
					}
				}
				stream.Write(pkt.Data)
			}
			if err := enc.Close(); err != nil {
				t.Fatal(err)
			}
			if err := enc.Close(); err != nil {
				t.Errorf("second Close: %v", err)
			}
			if _, err := enc.Encode(h264DesktopFrame(w, h, 0), false); err == nil {
				t.Error("Encode after Close succeeded")
			}

			ffprobe, err := exec.LookPath("ffprobe")
			if err != nil {
				t.Log("no ffprobe; stream not decoded")
				return
			}
			path := filepath.Join(t.TempDir(), "out.h264")
			if err := os.WriteFile(path, stream.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(ffprobe, "-v", "error", "-count_frames", "-select_streams", "v:0",
				"-show_entries", "stream=codec_name,width,height,nb_read_frames", "-of", "default=nw=1", path).CombinedOutput()
			if err != nil {
				t.Fatalf("ffprobe: %v: %s", err, out)
			}
			for _, want := range []string{"codec_name=h264", "width=1280", "height=800", "nb_read_frames=20"} {
				if !strings.Contains(string(out), want) {
					t.Errorf("ffprobe output lacks %q:\n%s", want, out)
				}
			}
		})
	}
}

func TestH264RejectsBadConfig(t *testing.T) {
	for _, cfg := range []Config{
		{Width: 1279, Height: 800},
		{Width: 0, Height: 800},
		{Width: 1280, Height: 800, H264Profile: "main"},
	} {
		enc, err := NewH264(cfg)
		if errors.Is(err, ErrUnavailable) {
			t.Skipf("OpenH264 not available: %v", err)
		}
		if err == nil {
			enc.Close()
			t.Errorf("NewH264(%+v) succeeded", cfg)
		}
	}
}

func TestH264FrameTooSmall(t *testing.T) {
	enc := h264Open(t, Config{Width: 640, Height: 480})
	defer enc.Close()
	if _, err := enc.Encode(NewI420(320, 240), false); err == nil {
		t.Error("encoded a frame smaller than the encoder")
	}
}

// h264FakeDefaults writes what FillDefault writes, at this binding's offsets.
func h264FakeDefaults() []byte {
	p := make([]byte, h264ParamSize)
	h264PutF32(p, h264PMaxFrameRate, h264DefaultMaxFrameHz)
	h264Put32(p, h264PTemporalLayerNum, 1)
	h264Put32(p, h264PSpatialLayerNum, 1)
	for i := range h264MaxSpatialLayers {
		l := h264PSpatialLayers + i*h264SpatialLayerStride
		h264PutF32(p, l+h264LFrameRate, h264DefaultMaxFrameHz)
		h264Put32(p, l+h264LDLayerQp, h264QualityBaseQP)
		h264Put32(p, l+h264LSliceSizeLimit, h264DefaultSliceSize)
		p[l+h264LVideoFormat] = h264VFUndef
		p[l+h264LColorPrimaries] = h264Undef
		p[l+h264LTransferCharacters] = h264Undef
		p[l+h264LColorMatrix] = h264Undef
	}
	h264Put32(p, h264PNumRefFrame, -1)
	h264Put32(p, h264PSpsPpsIDStrategy, h264IncreasingID)
	p[h264PEnableFrameSkip] = 1
	h264Put32(p, h264PLtrMarkPeriod, h264DefaultLtrMark)
	p[h264PMultipleThreadIdc] = 1
	p[h264PUseLoadBalancing] = 1
	p[h264PEnableBackgroundDet] = 1
	p[h264PEnableAdaptiveQuant] = 1
	p[h264PEnableFrameCropping] = 1
	p[h264PEnableSceneChange] = 1
	p[h264PFixRCOverShoot] = 1
	h264Put32(p, h264PIdrBitrateRatio, h264DefaultIDRRatio)
	return p
}

func TestH264CheckDefaults(t *testing.T) {
	p := h264FakeDefaults()
	if err := h264CheckDefaults(p); err != nil {
		t.Fatalf("defaults at the expected offsets rejected: %v", err)
	}
	// A struct shifted by four bytes, as a wrong header would give, is caught.
	shifted := make([]byte, len(p))
	copy(shifted[4:], p)
	if err := h264CheckDefaults(shifted); err == nil || !strings.Contains(err.Error(), "ABI mismatch") {
		t.Errorf("shifted defaults: err = %v, want an ABI mismatch", err)
	}
	p[h264PIdrBitrateRatio] = 0
	if err := h264CheckDefaults(p); err == nil {
		t.Error("a wrong iIdrBitrateRatio was accepted")
	}
}

func TestH264NALTypes(t *testing.T) {
	b := []byte{0, 0, 0, 1, 0x67, 1, 2, 0, 0, 0, 1, 0x68, 3, 0, 0, 1, 0x65, 9, 9}
	got := h264NALTypes(b)
	if string(got) != string([]byte{7, 8, 5}) {
		t.Errorf("NAL types = %v, want [7 8 5]", got)
	}
	if !h264HasNAL(b, 5) || h264HasNAL(b, 1) {
		t.Error("h264HasNAL is wrong")
	}
	if h264SPSProfile(b) != 1 {
		t.Errorf("SPS profile = %d", h264SPSProfile(b))
	}
}
