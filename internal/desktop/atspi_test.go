package desktop

import (
	"reflect"
	"testing"
)

func TestStateSet(t *testing.T) {
	// focused (12), enabled (8), sensitive (24), showing (25), checked (4); and
	// read-only (43), in the second word.
	s := stateSet{1<<12 | 1<<8 | 1<<24 | 1<<25 | 1<<4, 1 << (43 - 32)}
	if !s.has(stateShowing) || s.has(stateActive) {
		t.Error("has() is wrong")
	}
	if got, want := s.shown(), []string{"checked", "focused", "read-only"}; !reflect.DeepEqual(got, want) {
		t.Errorf("shown = %q, want %q", got, want)
	}
	if got := (stateSet{1 << 25}).shown(); !reflect.DeepEqual(got, []string{"disabled"}) {
		t.Errorf("neither enabled nor sensitive: shown = %q, want disabled", got)
	}
	if got := (stateSet{}).shown(); got != nil {
		t.Errorf("no states: shown = %q, want none", got)
	}
}

func TestStateNamesMatchAtspi(t *testing.T) {
	// Spot-check positions against AtspiStateType.
	for i, want := range map[int]string{1: "active", 8: "enabled", 12: "focused", 24: "sensitive", 25: "showing", 30: "visible", 43: "read-only"} {
		if stateNames[i] != want {
			t.Errorf("stateNames[%d] = %q, want %q", i, stateNames[i], want)
		}
	}
}

func TestToLayout(t *testing.T) {
	// A button 10px right of and 40px below the frame's origin, in a window whose
	// client area starts at (2,24) on the layout.
	got := toLayout(Rect{X: 10, Y: 40, Width: 50, Height: 20}, Rect{X: 0, Y: 0, Width: 636, Height: 774}, Rect{X: 2, Y: 24, Width: 636, Height: 774})
	if want := (Rect{X: 12, Y: 64, Width: 50, Height: 20}); got != want {
		t.Errorf("toLayout = %+v, want %+v", got, want)
	}
	// A frame that reports a shadow offset is subtracted.
	got = toLayout(Rect{X: 30, Y: 60, Width: 5, Height: 5}, Rect{X: 20, Y: 20, Width: 600, Height: 700}, Rect{X: 100, Y: 100})
	if want := (Rect{X: 110, Y: 140, Width: 5, Height: 5}); got != want {
		t.Errorf("toLayout with frame offset = %+v, want %+v", got, want)
	}
	// Chrome 154 on sway, measured 2026-10-12: a 636x771 content area at (642,27)
	// on the layout, a 656x791 frame (a 10px shadow each side), and an element
	// drawn at page (200,200), 114px below the window's top, reported at (210,297).
	got = toLayout(Rect{X: 210, Y: 297, Width: 20, Height: 20}, Rect{Width: 656, Height: 791}, Rect{X: 642, Y: 27, Width: 636, Height: 771})
	if want := (Rect{X: 842, Y: 314, Width: 20, Height: 20}); got != want {
		t.Errorf("toLayout with a drawn shadow = %+v, want %+v", got, want)
	}
	// A frame smaller than the window, or far larger (a stale size), is not a shadow.
	for _, f := range []Rect{{Width: 600, Height: 700}, {Width: 636 + 300, Height: 771 + 300}, {Width: 656, Height: 771}} {
		if dx, dy := shadowInset(f, Rect{Width: 636, Height: 771}); dx != 0 || dy != 0 {
			t.Errorf("shadowInset(%v) = %d,%d, want 0,0", f, dx, dy)
		}
	}
}

func TestRefsAreStable(t *testing.T) {
	a := &A11y{}
	t1 := Target{Elem: accRef{":1.5", "/a/1"}, ConID: 7}
	t2 := Target{Elem: accRef{":1.5", "/a/2"}, ConID: 7}
	r1, r2 := a.refFor(t1), a.refFor(t2)
	if r1 == r2 {
		t.Fatal("two elements share a ref")
	}
	moved := t1
	moved.ConID = 9
	if again := a.refFor(moved); again != r1 {
		t.Errorf("the same element got a new ref: %s then %s", r1, again)
	}
	if got, err := a.lookup(r1); err != nil || got.ConID != 9 {
		t.Errorf("lookup = %+v, %v; want the latest window", got, err)
	}
	if _, err := a.lookup("e999"); err == nil {
		t.Error("looked up an unknown ref")
	}
}

func TestClip(t *testing.T) {
	if got := clip("  two\n words  ", 80); got != "two words" {
		t.Errorf("clip = %q", got)
	}
	if got := clip("abcdef", 4); got != "abc…" {
		t.Errorf("clip = %q", got)
	}
}

func TestShownActions(t *testing.T) {
	got := shownActions([]string{"clipboard.copy", "Click", "selection.select-all", "menu.popup", "win.close"})
	if want := []string{"Click", "win.close"}; !reflect.DeepEqual(got, want) {
		t.Errorf("shownActions = %q, want %q", got, want)
	}
	many := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	if got := shownActions(many); len(got) != maxShownActions+1 || got[maxShownActions] != "+2 more" {
		t.Errorf("shownActions(8) = %q", got)
	}
}

func TestRedundant(t *testing.T) {
	button := &node{role: "button", name: "7", actions: []string{"Click"}}
	for _, tc := range []struct {
		n, parent *node
		want      bool
	}{
		{&node{role: "generic"}, nil, true},
		{&node{role: "generic", actions: []string{"clipboard.copy"}}, nil, true},
		{&node{role: "generic", actions: []string{"cal.solve"}}, nil, false},
		{&node{role: "group", name: "Keypad"}, nil, false},
		{&node{role: "label", name: "7"}, button, true},
		{&node{role: "label", name: "seven"}, button, false},
		{button, nil, false},
	} {
		if got := redundant(tc.n, tc.parent); got != tc.want {
			t.Errorf("redundant(%+v, parent %+v) = %v, want %v", tc.n, tc.parent, got, tc.want)
		}
	}
}
