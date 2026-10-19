package desktop

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
)

// Screen captures the layout, or a region of it, as an image.
type Screen interface {
	// Capture returns the image bytes and their MIME type. region is in layout
	// coordinates (nil for all of it); scale is output pixels per layout pixel.
	Capture(ctx context.Context, region *Rect, scale float64) ([]byte, string, error)
}

// Grim captures through wlroots' screencopy protocol, which a headless sway with
// the pixman renderer supports without a GPU.
type Grim struct {
	Path    string
	Env     []string
	JPEG    bool // smaller than PNG; the model sees the same pixels either way
	Quality int  // JPEG quality, 0 for grim's default
}

func (g Grim) Capture(ctx context.Context, region *Rect, scale float64) ([]byte, string, error) {
	path := g.Path
	if path == "" {
		path = "grim"
	}
	args := []string{"-s", strconv.FormatFloat(scale, 'f', 4, 64)}
	mime := "image/png"
	if g.JPEG {
		args = append(args, "-t", "jpeg")
		if g.Quality > 0 {
			args = append(args, "-q", strconv.Itoa(g.Quality))
		}
		mime = "image/jpeg"
	} else {
		args = append(args, "-t", "png", "-l", "1") // fastest compression; size barely matters locally
	}
	if region != nil {
		args = append(args, "-g", fmt.Sprintf("%d,%d %dx%d", region.X, region.Y, region.Width, region.Height))
	}
	args = append(args, "-")
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = g.Env
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return nil, "", fmt.Errorf("grim: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if out.Len() == 0 {
		return nil, "", fmt.Errorf("grim wrote no image")
	}
	return out.Bytes(), mime, nil
}

// Geometry relates screenshot pixels, the coordinates the model sees and clicks
// in, to sway's layout coordinates. Screenshots are scaled down so their long
// edge is at most MaxEdge; every coordinate a tool takes or returns is in the
// scaled space, and swampcy converts, because a model reliably clicks where it
// saw something, not where a stated scale factor says it was.
type Geometry struct {
	Layout Rect    // the output's rectangle in sway's layout
	Scale  float64 // screenshot pixels per layout pixel, at most 1
	Output string  // the output's name, when known
}

// NewGeometry fits the layout into maxEdge pixels on its long edge.
func NewGeometry(layout Rect, maxEdge int) Geometry {
	long := max(layout.Width, layout.Height)
	scale := 1.0
	if maxEdge > 0 && long > maxEdge {
		scale = float64(maxEdge) / float64(long)
	}
	return Geometry{Layout: layout, Scale: scale}
}

// Size is the screenshot's size in pixels.
func (g Geometry) Size() (int, int) {
	return int(math.Round(float64(g.Layout.Width) * g.Scale)), int(math.Round(float64(g.Layout.Height) * g.Scale))
}

// ToLayout converts a screenshot point to the layout, refusing points outside it.
func (g Geometry) ToLayout(x, y int) (int, int, error) {
	w, h := g.Size()
	if x < 0 || y < 0 || x >= w || y >= h {
		return 0, 0, fmt.Errorf("point (%d,%d) is outside the %dx%d screen", x, y, w, h)
	}
	lx := g.Layout.X + int(math.Round(float64(x)/g.Scale))
	ly := g.Layout.Y + int(math.Round(float64(y)/g.Scale))
	return min(lx, g.Layout.X+g.Layout.Width-1), min(ly, g.Layout.Y+g.Layout.Height-1), nil
}

// RectToLayout converts a screenshot rectangle to the layout, clipped to it.
func (g Geometry) RectToLayout(r Rect) (Rect, error) {
	if r.Width <= 0 || r.Height <= 0 {
		return Rect{}, fmt.Errorf("rectangle %dx%d is empty", r.Width, r.Height)
	}
	x1, y1, err := g.ToLayout(r.X, r.Y)
	if err != nil {
		return Rect{}, err
	}
	w, h := g.Size()
	x2 := g.Layout.X + int(math.Round(float64(min(r.X+r.Width, w))/g.Scale))
	y2 := g.Layout.Y + int(math.Round(float64(min(r.Y+r.Height, h))/g.Scale))
	return Rect{X: x1, Y: y1, Width: max(x2-x1, 1), Height: max(y2-y1, 1)}, nil
}

// ToShot converts a layout rectangle to screenshot pixels.
func (g Geometry) ToShot(r Rect) Rect {
	return Rect{
		X:      int(math.Round(float64(r.X-g.Layout.X) * g.Scale)),
		Y:      int(math.Round(float64(r.Y-g.Layout.Y) * g.Scale)),
		Width:  int(math.Round(float64(r.Width) * g.Scale)),
		Height: int(math.Round(float64(r.Height) * g.Scale)),
	}
}
