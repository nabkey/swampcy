package web

import (
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/nabkey/swampcy/internal/desktop"
)

// Single-seat apps. Chromium's Wayland backend binds only the first
// wl_seat the compositor offers, sway's seat0, so input on the owner's seat never
// reaches Chrome: sway has no pointer or keyboard of Chrome's on that seat to send
// it to. A viewer's input to such an app's window goes through a second pair of
// devices on the first seat instead, and seat0's focus is lent to the window while
// the owner uses it, then given back to where the agents had it.

// DefaultSingleSeatApps matches the app IDs and X11 classes of Chromium-based
// browsers, and of Chrome's app windows (chrome-<url>-Default).
const DefaultSingleSeatApps = `(?i)^(google-chrome|chrome|chromium|brave|microsoft-edge)`

// firstSeat is the seat sway creates first, at start: the first wl_seat it
// offers, the one a single-seat app binds.
const firstSeat = "seat0"

const (
	// lendIdle is how long seat0's focus stays lent after the owner's last input
	// to a single-seat app; then it goes back to the agents.
	lendIdle = 1500 * time.Millisecond
	// treeTTL is how long a read of sway's tree serves for finding windows.
	treeTTL = 250 * time.Millisecond
	// pressSettle is the pause between moving the pointer and pressing a button,
	// as swampcy's click does: GTK 4 can ignore a press that comes with its move.
	pressSettle = 20 * time.Millisecond
)

// router decides which seat's devices each of a viewer's inputs goes through.
type router struct {
	apps    *regexp.Regexp // single-seat apps; nil routes nothing
	output  string         // the session's output
	agents  string         // the agents' output, for giving focus back
	origin  desktop.Rect   // the output in the layout, in logical pixels
	scale   float64        // device pixels per logical pixel
	tree    func() (*desktop.Node, error)
	seats   func() ([]desktop.Seat, error)
	command func(string) error

	cached *desktop.Node
	readAt time.Time

	keysTo  int64 // the single-seat window the keyboard goes to, 0 for the owner's seat
	lent    int64 // the window seat0's focus is lent to, 0 when it is not
	prev    int64 // seat0's focus before it was lent: a container id, 0 for none
	lastUse time.Time
}

func (r *router) windows() []desktop.Window {
	if r.cached == nil || time.Since(r.readAt) > treeTTL {
		t, err := r.tree()
		if err != nil {
			return nil
		}
		r.cached, r.readAt = t, time.Now()
	}
	return r.cached.Windows()
}

// at is the window whose content is under a frame pixel on the session's output,
// or nil.
func (r *router) at(x, y int) *desktop.Window {
	scale := r.scale
	if scale <= 0 {
		scale = 1
	}
	lx := r.origin.X + int(float64(x)/scale)
	ly := r.origin.Y + int(float64(y)/scale)
	ws := r.windows()
	for i := len(ws) - 1; i >= 0; i-- { // floating windows, listed last, are on top
		w := ws[i]
		if w.Output != r.output || w.Visible == nil || !*w.Visible {
			continue
		}
		c := w.Content()
		if lx >= c.X && ly >= c.Y && lx < c.X+c.Width && ly < c.Y+c.Height {
			return &w
		}
	}
	return nil
}

func (r *router) single(w *desktop.Window) bool {
	return r.apps != nil && w != nil && r.apps.MatchString(w.App())
}

// pointer says whether input at a frame pixel goes through the first seat, and
// the window there. While seat0's focus is lent, a point over no window does too:
// it may be one of Chrome's popups (a menu, a list), which sway's tree does not show.
func (r *router) pointer(x, y int) (first bool, w *desktop.Window) {
	if r.apps == nil {
		return false, nil
	}
	w = r.at(x, y)
	if r.single(w) {
		return true, w
	}
	return w == nil && r.lent != 0, nil
}

// pressed notes a press, which picks where the keyboard goes: to the window
// pressed when it is a single-seat app's, and to the owner's seat otherwise.
// Pressing a window of a single-seat app with seat0 lends seat0's focus to it.
func (r *router) pressed(first bool, w *desktop.Window) {
	switch {
	case w != nil && first:
		r.keysTo = w.ID
		r.lend(w.ID, false) // the click itself focuses it
	case !first:
		r.keysTo = 0
	}
	if first {
		r.lastUse = time.Now()
	}
}

// keyboard says whether keys go through the first seat, lending seat0's focus to
// their window first. The window gone, they go to the owner's seat again.
func (r *router) keyboard() bool {
	if r.keysTo == 0 {
		return false
	}
	if !slices.ContainsFunc(r.windows(), func(w desktop.Window) bool { return w.ID == r.keysTo }) {
		r.cached = nil // it may be new since the last read
		if !slices.ContainsFunc(r.windows(), func(w desktop.Window) bool { return w.ID == r.keysTo }) {
			r.keysTo = 0
			return false
		}
	}
	r.lend(r.keysTo, true)
	r.lastUse = time.Now()
	return true
}

// seat0Focus is the container seat0's focus is on, 0 for none.
func (r *router) seat0Focus() int64 {
	seats, err := r.seats()
	if err != nil {
		return 0
	}
	for _, s := range seats {
		if s.Name == firstSeat {
			return s.Focus
		}
	}
	return 0
}

// lend gives seat0's focus to window id, remembering where it was the first time.
func (r *router) lend(id int64, focus bool) {
	cur := r.seat0Focus()
	if r.lent == 0 {
		r.prev = cur
	}
	r.lent = id
	if focus && cur != id {
		_ = r.command(fmt.Sprintf("[con_id=%d] focus", id))
	}
}

// due says whether seat0's focus has been lent and unused for lendIdle.
func (r *router) due() bool {
	return r.lent != 0 && time.Since(r.lastUse) >= lendIdle
}

// giveBack returns seat0's focus to where the agents had it, unless something
// else (an agent's focus_window) has moved it since it was lent.
func (r *router) giveBack() error {
	lent, prev := r.lent, r.prev
	r.lent, r.prev = 0, 0
	if lent == 0 || r.seat0Focus() != lent || prev == lent {
		return nil
	}
	r.cached = nil
	if prev != 0 && slices.ContainsFunc(r.windows(), func(w desktop.Window) bool { return w.ID == prev }) {
		return r.command(fmt.Sprintf("[con_id=%d] focus", prev))
	}
	if r.agents == "" {
		return nil
	}
	return r.command("focus output " + r.agents)
}
