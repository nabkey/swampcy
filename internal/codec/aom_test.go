//go:build linux || darwin

package codec

import (
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func aomOpen(t testing.TB, cfg Config) Encoder {
	t.Helper()
	e, err := NewAV1(cfg)
	if errors.Is(err, ErrUnavailable) {
		t.Skipf("libaom not available: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

// aomDesktop draws a desktop-like picture: a light background, rows of dark
// strokes like text, and a coloured window that moves with i.
func aomDesktop(f *Frame, w, h, i int) {
	for y := range h {
		row := f.Y[y*f.YStride : y*f.YStride+w]
		for x := range row {
			row[x] = 235
			if y%20 < 12 && y%20 > 2 && (x/7)%3 != 0 && (x+y/20*13)%97 < 60 {
				row[x] = 30 // "glyphs"
			}
		}
	}
	cw, ch := (w+1)/2, (h+1)/2
	for y := range ch {
		for x := range cw {
			f.U[y*f.UStride+x], f.V[y*f.UStride+x] = 128, 128
		}
	}
	rx, ry := 100+i*16, 200
	for y := ry; y < ry+240 && y < h; y++ {
		for x := rx; x < rx+320 && x < w; x++ {
			f.Y[y*f.YStride+x] = 90
			f.U[(y/2)*f.UStride+x/2], f.V[(y/2)*f.UStride+x/2] = 90, 200
		}
	}
}

// aomOBUTypes lists the OBU types in a low-overhead bitstream.
func aomOBUTypes(t *testing.T, b []byte) []int {
	t.Helper()
	var types []int
	for len(b) > 0 {
		h := b[0]
		if h&0x80 != 0 {
			t.Fatalf("forbidden bit set in OBU header %#x", h)
		}
		typ, ext, hasSize := int(h>>3)&0xf, h&0x04 != 0, h&0x02 != 0
		types = append(types, typ)
		n := 1
		if ext {
			n++
		}
		if !hasSize {
			break // the rest is this OBU
		}
		size, read := binary.Uvarint(b[n:]) // leb128 is unsigned varint
		if read <= 0 || n+read+int(size) > len(b) {
			t.Fatalf("bad OBU size at type %d", typ)
		}
		b = b[n+read+int(size):]
	}
	return types
}

func aomHas(types []int, want int) bool {
	for _, t := range types {
		if t == want {
			return true
		}
	}
	return false
}

// aomWriteIVF writes packets as an IVF file, which ffprobe reads.
func aomWriteIVF(t *testing.T, path string, w, h int, pkts []Packet) {
	t.Helper()
	hdr := make([]byte, 32)
	copy(hdr, "DKIF")
	binary.LittleEndian.PutUint16(hdr[4:], 0)
	binary.LittleEndian.PutUint16(hdr[6:], 32)
	copy(hdr[8:], "AV01")
	binary.LittleEndian.PutUint16(hdr[12:], uint16(w))
	binary.LittleEndian.PutUint16(hdr[14:], uint16(h))
	binary.LittleEndian.PutUint32(hdr[16:], 1000) // timebase denominator
	binary.LittleEndian.PutUint32(hdr[20:], 1)
	binary.LittleEndian.PutUint32(hdr[24:], uint32(len(pkts)))
	out := hdr
	for _, p := range pkts {
		fh := make([]byte, 12)
		binary.LittleEndian.PutUint32(fh, uint32(len(p.Data)))
		binary.LittleEndian.PutUint64(fh[4:], uint64(p.PTS.Milliseconds()))
		out = append(append(out, fh...), p.Data...)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAV1Encode(t *testing.T) {
	const w, h, n = 1280, 800, 20
	e := aomOpen(t, Config{Width: w, Height: h, FPS: 60, BitrateKbps: 2000, Threads: 4})
	if e.Name() != "AV1" {
		t.Errorf("Name = %q", e.Name())
	}
	f := NewI420(w, h)
	var pkts []Packet
	for i := range n {
		aomDesktop(f, w, h, i)
		f.PTS = time.Duration(i) * 16 * time.Millisecond
		p, err := e.Encode(f, i == 10)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if len(p.Data) == 0 {
			t.Fatalf("frame %d: empty packet", i)
		}
		pkts = append(pkts, p)
		if i == 5 {
			if err := e.SetBitrate(500); err != nil {
				t.Fatalf("SetBitrate: %v", err)
			}
		}
	}

	if !pkts[0].Keyframe {
		t.Error("first packet is not a keyframe")
	}
	if types := aomOBUTypes(t, pkts[0].Data); !aomHas(types, 1) {
		t.Errorf("first packet has no sequence header OBU: types %v", types)
	}
	for i, p := range pkts[1:] {
		i++
		if i == 10 {
			continue
		}
		if p.Keyframe {
			t.Errorf("frame %d is a keyframe", i)
		}
		if len(p.Data) >= len(pkts[0].Data) {
			t.Errorf("frame %d (%d bytes) is not smaller than the keyframe (%d)", i, len(p.Data), len(pkts[0].Data))
		}
		aomOBUTypes(t, p.Data)
	}
	if !pkts[10].Keyframe {
		t.Error("forced frame 10 is not a keyframe")
	}
	t.Logf("keyframe %d bytes, frame 1 %d bytes, forced keyframe %d bytes", len(pkts[0].Data), len(pkts[1].Data), len(pkts[10].Data))

	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, err := e.Encode(f, false); err == nil {
		t.Error("Encode after Close succeeded")
	}

	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Log("no ffprobe; skipping the bitstream check")
		return
	}
	path := filepath.Join(t.TempDir(), "out.ivf")
	aomWriteIVF(t, path, w, h, pkts)
	out, err := exec.Command(ffprobe, "-v", "error", "-count_frames", "-select_streams", "v",
		"-show_entries", "stream=codec_name,width,height,nb_read_frames,color_range,color_primaries,color_transfer,color_space",
		"-of", "default=nw=1", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe: %v\n%s", err, out)
	}
	got := string(out)
	t.Logf("ffprobe:\n%s", got)
	for _, want := range []string{"codec_name=av1", "width=1280", "height=800", "nb_read_frames=20", "color_range=pc", "color_primaries=bt709", "color_transfer=bt709", "color_space=bt709"} {
		if !strings.Contains(got, want) {
			t.Errorf("ffprobe output lacks %q", want)
		}
	}
}

func TestAV1RejectsShortPlanes(t *testing.T) {
	e := aomOpen(t, Config{Width: 64, Height: 64, Threads: 1})
	f := NewI420(64, 64)
	f.Y = f.Y[:100]
	if _, err := e.Encode(f, false); err == nil {
		t.Error("encoded a frame whose Y plane is too short")
	}
}

func TestAV1PTSMustIncrease(t *testing.T) {
	e := aomOpen(t, Config{Width: 64, Height: 64, Threads: 1})
	f := NewI420(64, 64)
	for range 3 { // same PTS three times: swampcy bumps it rather than failing
		if _, err := e.Encode(f, false); err != nil {
			t.Fatal(err)
		}
	}
}

func BenchmarkAV1Encode1280x800(b *testing.B) {
	const w, h = 1280, 800
	e := aomOpen(b, Config{Width: w, Height: h, FPS: 60, BitrateKbps: 4000, Threads: 4})
	f := NewI420(w, h)
	b.ResetTimer()
	for i := range b.N {
		b.StopTimer()
		aomDesktop(f, w, h, i%60)
		f.PTS = time.Duration(i) * 16 * time.Millisecond
		b.StartTimer()
		if _, err := e.Encode(f, false); err != nil {
			b.Fatal(err)
		}
	}
}
