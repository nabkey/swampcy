package codec

import (
	"image"
	"runtime"
	"sync"
)

// Full-range BT.709 in 16.16 fixed point. Each chroma row sums to zero, so grey
// stays at 128 exactly.
const (
	yR, yG, yB    = 13933, 46871, 4732
	cbR, cbG, cbB = -7509, -25259, 32768
	crR, crG, crB = 32768, -29763, -3005
	half          = 1 << 15
)

// parallelArea is the damage, in pixels, above which conversion is split across
// cores; below it the goroutines cost more than they save.
const parallelArea = 64 * 1024

// ConvertXRGB converts the given rectangles of an XRGB8888 picture (little-endian,
// so bytes B, G, R, X: wl_shm's format 1, and ARGB8888's layout too) into f, which
// has the same size. Rectangles are widened to even coordinates so every 2x2
// chroma block they touch is recomputed whole. yInvert reads the source bottom up.
func ConvertXRGB(f *Frame, src []byte, stride, width, height int, rects []image.Rectangle, yInvert bool) {
	bounds := image.Rect(0, 0, width, height)
	var area int
	for i, r := range rects {
		r = image.Rect(r.Min.X&^1, r.Min.Y&^1, (r.Max.X+1)&^1, (r.Max.Y+1)&^1).Intersect(bounds)
		rects[i] = r
		area += r.Dx() * r.Dy()
	}
	workers := 1
	if area > parallelArea {
		workers = runtime.GOMAXPROCS(0)
	}
	var wg sync.WaitGroup
	for _, r := range rects {
		if r.Empty() {
			continue
		}
		// Split each rectangle into bands of whole 2-row pairs.
		pairs := (r.Dy() + 1) / 2
		n := min(workers, pairs)
		for w := range n {
			y0 := r.Min.Y + (pairs*w/n)*2
			y1 := min(r.Min.Y+(pairs*(w+1)/n)*2, r.Max.Y)
			band := image.Rect(r.Min.X, y0, r.Max.X, y1)
			if n == 1 {
				convertBand(f, src, stride, height, band, yInvert)
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				convertBand(f, src, stride, height, band, yInvert)
			}()
		}
	}
	wg.Wait()
}

func clamp8(v int32) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

// convertBand converts r, whose corners are even (except at an odd-sized edge).
func convertBand(f *Frame, src []byte, stride, height int, r image.Rectangle, yInvert bool) {
	row := func(y int) []byte {
		if yInvert {
			y = height - 1 - y
		}
		return src[y*stride:]
	}
	for y := r.Min.Y; y < r.Max.Y; y += 2 {
		s0 := row(y)
		s1 := s0
		if y+1 < r.Max.Y {
			s1 = row(y + 1)
		}
		y0 := f.Y[y*f.YStride:]
		y1 := y0
		if y+1 < r.Max.Y {
			y1 = f.Y[(y+1)*f.YStride:]
		}
		u := f.U[(y/2)*f.UStride:]
		v := f.V[(y/2)*f.UStride:]
		for x := r.Min.X; x < r.Max.X; x += 2 {
			x1 := x + 1
			if x1 >= r.Max.X {
				x1 = x
			}
			a, b := x*4, x1*4
			b0, g0, r0 := int32(s0[a]), int32(s0[a+1]), int32(s0[a+2])
			b1, g1, r1 := int32(s0[b]), int32(s0[b+1]), int32(s0[b+2])
			b2, g2, r2 := int32(s1[a]), int32(s1[a+1]), int32(s1[a+2])
			b3, g3, r3 := int32(s1[b]), int32(s1[b+1]), int32(s1[b+2])
			y0[x] = byte((yR*r0 + yG*g0 + yB*b0 + half) >> 16)
			y0[x1] = byte((yR*r1 + yG*g1 + yB*b1 + half) >> 16)
			y1[x] = byte((yR*r2 + yG*g2 + yB*b2 + half) >> 16)
			y1[x1] = byte((yR*r3 + yG*g3 + yB*b3 + half) >> 16)
			// Chroma from the block's average colour (the sums are 4x, hence >> 18).
			sr, sg, sb := r0+r1+r2+r3, g0+g1+g2+g3, b0+b1+b2+b3
			u[x/2] = clamp8((cbR*sr + cbG*sg + cbB*sb + (128 << 18) + (1 << 17)) >> 18)
			v[x/2] = clamp8((crR*sr + crG*sg + crB*sb + (128 << 18) + (1 << 17)) >> 18)
		}
	}
}
