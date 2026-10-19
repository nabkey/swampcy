package desktop

import "testing"

func TestGeometryUnscaled(t *testing.T) {
	g := NewGeometry(Rect{Width: 1280, Height: 800}, 1568)
	if g.Scale != 1 {
		t.Fatalf("Scale = %v, want 1", g.Scale)
	}
	if w, h := g.Size(); w != 1280 || h != 800 {
		t.Errorf("Size = %dx%d", w, h)
	}
	if x, y, err := g.ToLayout(640, 400); err != nil || x != 640 || y != 400 {
		t.Errorf("ToLayout = %d,%d,%v", x, y, err)
	}
	for _, p := range [][2]int{{-1, 0}, {0, -1}, {1280, 0}, {0, 800}} {
		if _, _, err := g.ToLayout(p[0], p[1]); err == nil {
			t.Errorf("ToLayout(%v) accepted a point off screen", p)
		}
	}
}

func TestGeometryScaled(t *testing.T) {
	// A 2560x1600 layout at x=100 fits 1280 on its long edge: half scale.
	g := NewGeometry(Rect{X: 100, Width: 2560, Height: 1600}, 1280)
	if g.Scale != 0.5 {
		t.Fatalf("Scale = %v, want 0.5", g.Scale)
	}
	if w, h := g.Size(); w != 1280 || h != 800 {
		t.Errorf("Size = %dx%d", w, h)
	}
	if x, y, _ := g.ToLayout(10, 20); x != 120 || y != 40 {
		t.Errorf("ToLayout(10,20) = %d,%d, want 120,40", x, y)
	}
	if x, y, _ := g.ToLayout(1279, 799); x != 100+2558 || y != 1598 {
		t.Errorf("ToLayout of the last pixel = %d,%d, want inside the layout", x, y)
	}
	if got, want := g.ToShot(Rect{X: 300, Y: 400, Width: 200, Height: 100}), (Rect{X: 100, Y: 200, Width: 100, Height: 50}); got != want {
		t.Errorf("ToShot = %+v, want %+v", got, want)
	}
	r, err := g.RectToLayout(Rect{X: 100, Y: 200, Width: 2000, Height: 50})
	if err != nil {
		t.Fatal(err)
	}
	if want := (Rect{X: 300, Y: 400, Width: 2360, Height: 100}); r != want {
		t.Errorf("RectToLayout clipped = %+v, want %+v", r, want)
	}
	if _, err := g.RectToLayout(Rect{X: 1, Y: 1}); err == nil {
		t.Error("RectToLayout accepted an empty rectangle")
	}
}
