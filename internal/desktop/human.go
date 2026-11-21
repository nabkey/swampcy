package desktop

import (
	"math"
	"math/rand/v2"
	"sync"
	"time"
	"unicode"
)

// Human plans input as a person's hand would make it: a pointer that travels to
// its target along a bowed, wavering path, sometimes past it and back; buttons
// and keys held for a moment; typing at a person's uneven pace, with the odd
// wrong key noticed and backspaced. It only plans: the Desktop and the keyboard
// carry the plans out. Sites that watch how they are used (and one's own UI
// under test) then see events a person could have made, where the other tools'
// input arrives in no time at exact pixels.
type Human struct {
	// Sleep, when set, replaces time.Sleep for the pauses a plan asks for.
	Sleep func(time.Duration)

	mu  sync.Mutex
	rng *rand.Rand
	// pace scales every duration: this person is a little quicker or slower than
	// the median one, and stays so for the life of the server.
	pace float64
}

// NewHuman returns a Human whose choices follow from seed; 0 picks one at random.
func NewHuman(seed uint64) *Human {
	if seed == 0 {
		seed = rand.Uint64() | 1
	}
	h := &Human{rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
	h.pace = h.between(0.88, 1.18)
	return h
}

// Nap pauses for d.
func (h *Human) Nap(d time.Duration) {
	if d <= 0 {
		return
	}
	if h.Sleep != nil {
		h.Sleep(d)
		return
	}
	time.Sleep(d)
}

func (h *Human) between(lo, hi float64) float64 { return lo + (hi-lo)*h.rng.Float64() }

func (h *Human) normal(mean, sd float64) float64 { return mean + sd*h.rng.NormFloat64() }

// ms draws a duration around median milliseconds, log-normally (pauses have a
// long tail and no negative side), scaled by the pace and kept within lo and hi.
func (h *Human) ms(median, sigma, lo, hi float64) time.Duration {
	// Drawn again when out of range, not clamped: a clamp piles draws up on the
	// bounds, and a pile of pauses of exactly one length is no person's.
	v := median * math.Exp(sigma*h.rng.NormFloat64())
	for n := 0; (v < lo || v > hi) && n < 16; n++ {
		v = median * math.Exp(sigma*h.rng.NormFloat64())
	}
	v = min(max(v, lo), hi) * h.pace
	return time.Duration(v * float64(time.Millisecond))
}

// Step is one motion of a pointer's path: wait, then be at (X, Y).
type Step struct {
	X, Y int
	Wait time.Duration
}

type vec struct{ x, y float64 }

func (a vec) add(b vec) vec       { return vec{a.x + b.x, a.y + b.y} }
func (a vec) sub(b vec) vec       { return vec{a.x - b.x, a.y - b.y} }
func (a vec) scale(k float64) vec { return vec{a.x * k, a.y * k} }
func (a vec) length() float64     { return math.Hypot(a.x, a.y) }
func (a vec) round(in Rect) (x, y int) {
	x, y = int(math.Round(a.x)), int(math.Round(a.y))
	if in.Width > 0 && in.Height > 0 {
		x = min(max(x, in.X), in.X+in.Width-1)
		y = min(max(y, in.Y), in.Y+in.Height-1)
	}
	return x, y
}

// motionTick is the time between a path's points: a mouse reporting 125 times a second.
const motionTick = 8 * time.Millisecond

// Path plans the pointer's way from one point to another, ending exactly on the
// second. width is how wide the target is, in the same pixels: a small one is
// approached more slowly (Fitts's law). The path stays within in. A careful path,
// for a drag, is slower and does not miss; any other may run past its target, or
// stop short of it, and come back. Two points less than a pixel apart need no path.
func (h *Human) Path(from, to [2]int, width float64, in Rect, careful bool) []Step {
	h.mu.Lock()
	defer h.mu.Unlock()
	a, b := vec{float64(from[0]), float64(from[1])}, vec{float64(to[0]), float64(to[1])}
	dist := b.sub(a).length()
	if dist < 1 {
		return nil
	}
	width = max(width, 4)
	var steps []Step
	var pause time.Duration
	if !careful && dist > 40 && h.rng.Float64() < min(0.2+dist/1500, 0.55) {
		// The first movement is aimed by eye and lands near the target; a second,
		// smaller one corrects it.
		u := b.sub(a).scale(1 / dist)
		along := min(max(h.normal(0.07, 0.03)*dist, 3), 48)
		if h.rng.Float64() < 0.25 {
			along = -along // short of it, not past it
		}
		across := h.normal(0, 0.35*math.Abs(along))
		miss := b.add(u.scale(along)).add(vec{-u.y, u.x}.scale(across))
		steps = h.segment(steps, a, miss, in, h.travel(dist, width*2, careful), 0)
		if n := len(steps); n > 0 {
			a = vec{float64(steps[n-1].X), float64(steps[n-1].Y)}
		}
		pause = h.ms(95, 0.35, 40, 260)
	}
	steps = h.segment(steps, a, b, in, h.travel(b.sub(a).length(), width, careful), pause)
	// Whatever the wavering did, the path ends on the target.
	if n := len(steps); n == 0 || steps[n-1].X != to[0] || steps[n-1].Y != to[1] {
		steps = append(steps, Step{to[0], to[1], motionTick})
	}
	return steps
}

// travel is how long a movement of dist to a target of width takes.
func (h *Human) travel(dist, width float64, careful bool) time.Duration {
	t := 90 + 115*math.Log2(dist/width+1)
	if careful {
		t *= 1.35
	}
	return h.ms(t, 0.15, 60, 2500)
}

// segment appends one movement from a to b lasting about d: along a bowed curve,
// fast in the middle and slow at the ends, the hand's tremor added to it.
func (h *Human) segment(steps []Step, a, b vec, in Rect, d, wait time.Duration) []Step {
	ab := b.sub(a)
	dist := ab.length()
	if dist < 0.5 {
		return steps
	}
	perp := vec{-ab.y / dist, ab.x / dist}
	bow := h.normal(0, 0.06) * dist
	c1 := a.add(ab.scale(h.between(0.2, 0.4))).add(perp.scale(bow * h.between(0.6, 1.2)))
	c2 := a.add(ab.scale(h.between(0.6, 0.8))).add(perp.scale(bow * h.between(0.3, 0.9)))
	early := h.between(0.75, 1) // below 1, the peak speed comes before halfway
	tremor := min(max(dist/200, 0.25), 1.2)
	n := max(int(d/motionTick), 2)
	lastX, lastY := a.round(in)
	var shake vec
	for i := 1; i <= n; i++ {
		t := float64(i) / float64(n)
		s := math.Pow(t, early)
		s = s * s * s * (10 - 15*s + 6*s*s) // minimum jerk
		r := 1 - s
		p := a.scale(r * r * r).add(c1.scale(3 * r * r * s)).add(c2.scale(3 * r * s * s)).add(b.scale(s * s * s))
		// Wavering that drifts over a tenth of a second or two, as a hand's does,
		// not from one report to the next, and dies away at both ends.
		shake = shake.scale(0.95).add(vec{h.normal(0, 0.31*tremor), h.normal(0, 0.31*tremor)})
		x, y := p.add(shake.scale(math.Sin(math.Pi * t))).round(in)
		wait += time.Duration(h.between(0.8, 1.2) * float64(motionTick))
		if x == lastX && y == lastY {
			continue // a pointer that has not moved reports nothing
		}
		steps = append(steps, Step{x, y, wait})
		lastX, lastY, wait = x, y, 0
	}
	return steps
}

// Start is where the pointer is taken to be when nobody knows: somewhere in the
// middle of the screen.
func (h *Human) Start(in Rect) (x, y int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return in.X + int(float64(in.Width)*h.between(0.25, 0.75)), in.Y + int(float64(in.Height)*h.between(0.25, 0.75))
}

// Near is a point a hand aiming at (x, y) would hit: within spread pixels of it.
func (h *Human) Near(x, y int, spread float64) (int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	off := func() int { return int(math.Round(min(max(h.normal(0, spread/2), -spread), spread))) }
	return x + off(), y + off()
}

// Within is where a hand aiming at a widget of bounds r would click: around its
// centre, never near its edges.
func (h *Human) Within(r Rect) (int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	off := func(size int) int {
		s := float64(size)
		return int(math.Round(min(max(h.normal(0, s/8), -0.3*s), 0.3*s)))
	}
	return r.X + r.Width/2 + off(r.Width), r.Y + r.Height/2 + off(r.Height)
}

// Dwell is the pause between the pointer arriving and the button going down.
func (h *Human) Dwell() time.Duration { return h.draw(110, 0.4, 50, 350) }

// Hold is how long a button stays down in a click.
func (h *Human) Hold() time.Duration { return h.draw(85, 0.25, 45, 180) }

// Between is the pause between the clicks of a double-click.
func (h *Human) Between() time.Duration { return h.draw(110, 0.2, 70, 200) }

// Beat is the pause between one action and the next.
func (h *Human) Beat() time.Duration { return h.draw(380, 0.4, 180, 1200) }

func (h *Human) draw(median, sigma, lo, hi float64) time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ms(median, sigma, lo, hi)
}

// Ticks is the pause before each of n clicks of a wheel: a finger rolls it a few
// clicks at a time and then lifts.
func (h *Human) Ticks(n int) []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]time.Duration, n)
	roll := 0
	for i := 1; i < n; i++ {
		if roll == 0 {
			roll = 2 + h.rng.IntN(4)
		}
		if roll--; roll == 0 {
			out[i] = h.ms(260, 0.4, 120, 700)
		} else {
			out[i] = h.ms(55, 0.3, 25, 120)
		}
	}
	return out
}

// qwertyRows are the letter keys, for the key a finger hits beside the one it meant.
var qwertyRows = []string{"qwertyuiop", "asdfghjkl", "zxcvbnm"}

// neighbours maps each lower-case letter to the letters beside it on the keyboard.
var neighbours = func() map[rune][]rune {
	m := map[rune][]rune{}
	at := func(row, i int) (rune, bool) {
		if row < 0 || row >= len(qwertyRows) || i < 0 || i >= len(qwertyRows[row]) {
			return 0, false
		}
		return rune(qwertyRows[row][i]), true
	}
	for r, row := range qwertyRows {
		for i, c := range row {
			// The same row twice: a finger slips sideways more than up or down.
			for _, p := range [][2]int{{r, i - 1}, {r, i + 1}, {r, i - 1}, {r, i + 1}, {r - 1, i}, {r - 1, i + 1}, {r + 1, i - 1}, {r + 1, i}} {
				if n, ok := at(p[0], p[1]); ok {
					m[c] = append(m[c], n)
				}
			}
		}
	}
	return m
}()

// plainRune reports whether c is typed by one key that only inserts it, so that a
// BackSpace takes it away again: not a newline or a tab, which may send a form.
func plainRune(c rune) bool {
	_, us := usKeys[c]
	return us && c != '\n' && c != '\t'
}

// Typing plans the keystrokes that type text. With slips, now and then a wrong
// letter is typed (the key beside the right one, or two letters the wrong way
// round), noticed at once or a letter or two later, and backspaced away; what is
// left in the end is always exactly text. It reports how many slips it made.
func (h *Human) Typing(text string, slips bool) ([]Stroke, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs := []rune(text)
	out := make([]Stroke, 0, len(rs)+8)
	var prev rune
	char := func(c rune) {
		s := Stroke{Char: c, Before: h.flight(prev, c, len(out) == 0), Hold: h.ms(95, 0.28, 40, 190)}
		if k, ok := usKeys[c]; ok && k.shift {
			s.Lead, s.Lag = h.ms(65, 0.35, 25, 160), h.ms(40, 0.5, 5, 140)
		}
		out = append(out, s)
		prev = c
	}
	slips = slips && len(rs) >= 4
	made := 0
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if slips && i > 0 && h.rng.Float64() < 0.03 {
			if wrong, skip := h.slip(rs, i); len(wrong) > 0 {
				// How many more right letters go by before the eye catches it.
				more := 0
				switch p := h.rng.Float64(); {
				case p > 0.9:
					more = 2
				case p > 0.65:
					more = 1
				}
				for more > 0 && (i+skip+more > len(rs) || !allPlain(rs[i+skip:i+skip+more])) {
					more--
				}
				for _, w := range wrong {
					char(w)
				}
				for _, r := range rs[i+skip : i+skip+more] {
					char(r)
				}
				for n := range len(wrong) + more {
					before := h.ms(185, 0.3, 110, 400)
					if n == 0 {
						before = h.ms(390, 0.4, 200, 1000) // noticing
					}
					out = append(out, Stroke{Key: Combo{Key: "BackSpace"}, Before: before, Hold: h.ms(75, 0.25, 40, 160)})
				}
				made++
				prev = 0 // the next letter starts afresh, after a look at the screen
				char(c)
				out[len(out)-1].Before += h.ms(180, 0.3, 80, 500)
				continue
			}
		}
		char(c)
	}
	return out, made
}

func allPlain(rs []rune) bool {
	for _, c := range rs {
		if !plainRune(c) {
			return false
		}
	}
	return true
}

// slip is what a finger might type instead of the letter at i: the key beside it,
// or it and the next letter swapped (skip is how many of text's letters that
// used up). It returns nothing where a slip could not be undone with BackSpace,
// or does not happen: outside letters.
func (h *Human) slip(rs []rune, i int) (wrong []rune, skip int) {
	c := rs[i]
	near := neighbours[unicode.ToLower(c)]
	if len(near) == 0 {
		return nil, 0
	}
	if i+1 < len(rs) && rs[i+1] != c && unicode.IsLower(c) && unicode.IsLower(rs[i+1]) && plainRune(rs[i+1]) && h.rng.Float64() < 0.2 {
		return []rune{rs[i+1], c}, 2
	}
	w := near[h.rng.IntN(len(near))]
	if unicode.IsUpper(c) {
		w = unicode.ToUpper(w)
	}
	return []rune{w}, 1
}

// flight is how long after prev went down c goes down. It is drawn apart from
// how long prev is held, so now and then a key goes down before the last is up.
func (h *Human) flight(prev, c rune, first bool) time.Duration {
	if first {
		return h.ms(120, 0.4, 40, 400)
	}
	k := 1.0
	switch {
	case prev == '\n':
		k = 2.5
	case prev == '.' || prev == ',' || prev == '!' || prev == '?' || prev == ';' || prev == ':':
		k = 1.7
	case prev == ' ':
		k = 1.25 // finding the next word
	case prev == c:
		k = 0.85 // the same finger, already there
	}
	if u, ok := usKeys[c]; !ok || u.shift || unicode.IsDigit(c) {
		k *= 1.3 // a reach
	}
	d := h.ms(165*k, 0.4, 55, 1100)
	if prev == ' ' && h.rng.Float64() < 0.03 {
		d += h.ms(600, 0.5, 250, 1800) // a thought
	}
	return d
}

// Keys plans presses of keys and shortcuts, as the key tool makes them.
func (h *Human) Keys(combos []Combo) []Stroke {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Stroke, len(combos))
	for i, c := range combos {
		before := h.ms(250, 0.4, 130, 800)
		switch {
		case i == 0:
			before = h.ms(60, 0.4, 20, 200)
		case c.Key == combos[i-1].Key:
			before = h.ms(190, 0.3, 120, 400) // the same key again
		}
		out[i] = Stroke{Key: c, Before: before, Hold: h.ms(90, 0.25, 45, 190)}
		if len(c.Mods) > 0 {
			out[i].Lead, out[i].Lag = h.ms(110, 0.35, 50, 300), h.ms(60, 0.4, 15, 200)
		}
	}
	return out
}
