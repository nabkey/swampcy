package codec

import (
	"image"
	"testing"
)

func yuvSolid(w, h int, r, g, b byte) []byte {
	px := make([]byte, w*h*4)
	for i := 0; i < len(px); i += 4 {
		px[i], px[i+1], px[i+2] = b, g, r
	}
	return px
}

func TestConvertXRGBColours(t *testing.T) {
	for _, tc := range []struct {
		name    string
		r, g, b byte
		y, u, v byte
	}{
		{"white", 255, 255, 255, 255, 128, 128},
		{"black", 0, 0, 0, 0, 128, 128},
		{"grey", 128, 128, 128, 128, 128, 128},
		{"red", 255, 0, 0, 54, 99, 255},
		{"green", 0, 255, 0, 182, 30, 12},
		{"blue", 0, 0, 255, 18, 255, 116},
	} {
		f := NewI420(6, 4)
		ConvertXRGB(f, yuvSolid(6, 4, tc.r, tc.g, tc.b), 6*4, 6, 4, []image.Rectangle{image.Rect(0, 0, 6, 4)}, false)
		if f.Y[0] != tc.y || f.U[0] != tc.u || f.V[0] != tc.v || f.Y[23] != tc.y || f.V[5] != tc.v {
			t.Errorf("%s: Y %d U %d V %d, want %d %d %d", tc.name, f.Y[0], f.U[0], f.V[0], tc.y, tc.u, tc.v)
		}
	}
}

func TestConvertXRGBOnlyDamage(t *testing.T) {
	const w, h = 64, 48
	f := NewI420(w, h)
	src := yuvSolid(w, h, 255, 255, 255)
	// A damage rectangle at odd coordinates is widened to the even grid around it.
	ConvertXRGB(f, src, w*4, w, h, []image.Rectangle{image.Rect(11, 7, 14, 9)}, false)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			inside := x >= 10 && x < 14 && y >= 6 && y < 10
			if got := f.Y[y*w+x] == 255; got != inside {
				t.Fatalf("pixel (%d,%d) converted=%v, want %v", x, y, got, inside)
			}
		}
	}
}

func TestConvertXRGBParallelMatchesSerial(t *testing.T) {
	const w, h = 1280, 800
	src := make([]byte, w*h*4)
	for i := range src {
		src[i] = byte(i*7 + i/4096)
	}
	whole := []image.Rectangle{image.Rect(0, 0, w, h)}
	a := NewI420(w, h)
	ConvertXRGB(a, src, w*4, w, h, whole, false) // above parallelArea: split across cores
	b := NewI420(w, h)
	for y := 0; y < h; y += 2 { // the same, one small rectangle at a time
		ConvertXRGB(b, src, w*4, w, h, []image.Rectangle{image.Rect(0, y, w, y+2)}, false)
	}
	for i := range a.Y {
		if a.Y[i] != b.Y[i] {
			t.Fatalf("Y[%d] differs", i)
		}
	}
	for i := range a.U {
		if a.U[i] != b.U[i] || a.V[i] != b.V[i] {
			t.Fatalf("chroma[%d] differs", i)
		}
	}
}

func TestConvertXRGBInverted(t *testing.T) {
	const w, h = 4, 4
	src := yuvSolid(w, h, 0, 0, 0)
	for x := 0; x < w; x++ { // a white top row in the source
		src[x*4], src[x*4+1], src[x*4+2] = 255, 255, 255
	}
	f := NewI420(w, h)
	ConvertXRGB(f, src, w*4, w, h, []image.Rectangle{image.Rect(0, 0, w, h)}, true)
	if f.Y[0] != 0 || f.Y[(h-1)*w] != 255 {
		t.Errorf("y-inverted: top %d bottom %d, want 0 and 255", f.Y[0], f.Y[(h-1)*w])
	}
}

func BenchmarkConvertXRGBFull(b *testing.B) {
	const w, h = 1280, 800
	src := make([]byte, w*h*4)
	f := NewI420(w, h)
	b.SetBytes(w * h * 4)
	for b.Loop() {
		ConvertXRGB(f, src, w*4, w, h, []image.Rectangle{image.Rect(0, 0, w, h)}, false)
	}
}
