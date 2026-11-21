package desktop

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

var humanScreen = Rect{X: 0, Y: 0, Width: 1280, Height: 800}

func TestHumanPathEndsOnItsTarget(t *testing.T) {
	for seed := uint64(1); seed <= 300; seed++ {
		h := NewHuman(seed)
		for _, tc := range [][2][2]int{
			{{100, 100}, {900, 600}}, {{900, 600}, {910, 598}}, {{640, 400}, {0, 0}},
			{{5, 790}, {1279, 3}}, {{300, 300}, {301, 300}}, {{50, 50}, {50, 400}},
		} {
			from, to := tc[0], tc[1]
			steps := h.Path(from, to, 20, humanScreen, seed%2 == 0)
			if len(steps) == 0 {
				t.Fatalf("seed %d: no path from %v to %v", seed, from, to)
			}
			if last := steps[len(steps)-1]; last.X != to[0] || last.Y != to[1] {
				t.Fatalf("seed %d: path from %v ends at (%d,%d), want %v", seed, from, last.X, last.Y, to)
			}
			var total time.Duration
			at := from
			for i, s := range steps {
				if s.X < 0 || s.X >= 1280 || s.Y < 0 || s.Y >= 800 {
					t.Fatalf("seed %d: step %d (%d,%d) is off the screen", seed, i, s.X, s.Y)
				}
				if s.X == at[0] && s.Y == at[1] {
					t.Fatalf("seed %d: step %d does not move", seed, i)
				}
				if jump := math.Hypot(float64(s.X-at[0]), float64(s.Y-at[1])); jump > 90 {
					t.Fatalf("seed %d: step %d jumps %.0f pixels in one report", seed, i, jump)
				}
				if s.Wait <= 0 {
					t.Fatalf("seed %d: step %d takes no time", seed, i)
				}
				at = [2]int{s.X, s.Y}
				total += s.Wait
			}
			if total > 4*time.Second {
				t.Fatalf("seed %d: %v to %v took %s", seed, from, to, total)
			}
			if dist := math.Hypot(float64(to[0]-from[0]), float64(to[1]-from[1])); dist > 300 && total < 150*time.Millisecond {
				t.Fatalf("seed %d: %.0f pixels in %s is no hand", seed, dist, total)
			}
		}
	}
	if steps := NewHuman(1).Path([2]int{7, 7}, [2]int{7, 7}, 20, humanScreen, false); steps != nil {
		t.Errorf("a path to where the pointer is = %v, want none", steps)
	}
}

// A path to the right along y=400: how far past x=800 it went, and how far it
// strayed from the straight line.
func pastAndAside(steps []Step) (past, aside int) {
	for _, s := range steps {
		past = max(past, s.X-800)
		aside = max(aside, s.Y-400, 400-s.Y)
	}
	return past, aside
}

func TestHumanPathMissesAndWavers(t *testing.T) {
	overshot, bowed := 0, 0
	const n = 400
	for seed := uint64(1); seed <= n; seed++ {
		h := NewHuman(seed)
		past, aside := pastAndAside(h.Path([2]int{200, 400}, [2]int{800, 400}, 20, humanScreen, false))
		if past >= 3 {
			overshot++
		}
		if aside >= 3 {
			bowed++
		}
		// With care (a drag), the pointer never runs past where it is going.
		if past, _ := pastAndAside(h.Path([2]int{200, 400}, [2]int{800, 400}, 20, humanScreen, true)); past > 2 {
			t.Fatalf("seed %d: a careful path ran %d pixels past its target", seed, past)
		}
	}
	if overshot < n/5 || overshot > 3*n/4 {
		t.Errorf("%d of %d paths ran past the target, want some and not all", overshot, n)
	}
	if bowed < 3*n/4 {
		t.Errorf("only %d of %d paths left the straight line", bowed, n)
	}
}

// typed is what strokes leave in a text field.
func typed(t *testing.T, strokes []Stroke) (text string, backspaces int) {
	t.Helper()
	var buf []rune
	for _, s := range strokes {
		switch {
		case s.Char != 0:
			buf = append(buf, s.Char)
		case s.Key.Key == "BackSpace" && len(s.Key.Mods) == 0:
			if len(buf) == 0 {
				t.Fatal("a BackSpace with nothing typed")
			}
			if c := buf[len(buf)-1]; c == '\n' || c == '\t' {
				t.Fatalf("a BackSpace after %q, which may have sent a form", c)
			}
			buf = buf[:len(buf)-1]
			backspaces++
		default:
			t.Fatalf("unexpected stroke %+v", s)
		}
		if s.Hold < 30*time.Millisecond || s.Hold > 300*time.Millisecond {
			t.Fatalf("a key held for %s", s.Hold)
		}
		if s.Before < 15*time.Millisecond {
			t.Fatalf("a key %s after the last", s.Before)
		}
	}
	return string(buf), backspaces
}

func TestHumanTypingLeavesExactlyTheText(t *testing.T) {
	texts := []string{
		"wireless mechanical keyboard with brown switches",
		"The quick brown fox jumps over the lazy dog.\nThen it does so again, twice.\n",
		"name@example.com\tHunter2!x\n",
		"naïve café 東京 😀 and back to ascii",
		"ab", "", "aaaa bbbb",
	}
	slips, corrected := 0, 0
	for seed := uint64(1); seed <= 400; seed++ {
		h := NewHuman(seed)
		for _, want := range texts {
			strokes, made := h.Typing(want, true)
			got, backspaces := typed(t, strokes)
			if got != want {
				t.Fatalf("seed %d: typed %q, want %q", seed, got, want)
			}
			if (made == 0) != (backspaces == 0) {
				t.Fatalf("seed %d: %d slips but %d backspaces", seed, made, backspaces)
			}
			if len([]rune(want)) < 4 && made > 0 {
				t.Fatalf("seed %d: a slip in the short text %q", seed, want)
			}
			slips += made
			corrected += backspaces

			strokes, made = h.Typing(want, false)
			if got, backspaces := typed(t, strokes); got != want || backspaces != 0 || made != 0 {
				t.Fatalf("seed %d: without slips typed %q with %d backspaces, want %q exactly", seed, got, backspaces, want)
			}
		}
	}
	// About three letters in a hundred.
	if slips < 500 || slips > 5000 {
		t.Errorf("%d slips over 400 runs, want a few each", slips)
	}
	if corrected <= slips {
		t.Errorf("%d backspaces for %d slips: none was noticed late or swapped two letters", corrected, slips)
	}
}

func TestHumanTicksAndKeys(t *testing.T) {
	h := NewHuman(3)
	ticks := h.Ticks(12)
	if len(ticks) != 12 || ticks[0] != 0 {
		t.Fatalf("ticks = %v, want 12 with the first at once", ticks)
	}
	long := 0
	for _, d := range ticks[1:] {
		if d < 20*time.Millisecond {
			t.Errorf("a wheel click %s after the last", d)
		}
		if d > 120*time.Millisecond {
			long++
		}
	}
	if long == 0 {
		t.Error("twelve wheel clicks in one roll, with no lift of the finger")
	}
	combos, _ := ParseKeys("ctrl+a Down Down Return")
	strokes := h.Keys(combos)
	if len(strokes) != 4 || !reflect.DeepEqual(strokes[0].Key, combos[0]) || strokes[3].Key.Key != "Return" || strokes[1].Hold < 40*time.Millisecond {
		t.Errorf("key strokes = %+v", strokes)
	}
}

func TestStrokes(t *testing.T) {
	f := &fakeCompositor{}
	v, err := OpenVirtualInput(startFakeCompositor(t, f), InputPlace{})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	ms := time.Millisecond
	start := time.Now()
	err = v.Strokes(context.Background(), []Stroke{
		{Char: 'a', Hold: 30 * ms},
		{Char: 'A', Before: 10 * ms, Hold: 30 * ms}, {Char: 'B', Before: 10 * ms, Hold: 30 * ms}, // Shift stays down
		{Char: 'c', Before: 10 * ms},
		{Char: 'D'}, {Char: 'E', Before: 400 * ms}, // too long apart to keep Shift down
		{Key: Combo{Key: "BackSpace"}, Hold: 10 * ms},
		{Key: Combo{Mods: []string{"ctrl"}, Key: "a"}},
		{Char: 'é', Hold: 10 * ms},
		{Key: Combo{Key: "XF86AudioPlay"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < 500*ms {
		t.Errorf("strokes took %s, less than their pauses and holds", took)
	}
	_ = v.Sync()
	want := [][2]uint32{
		{30, 1}, {30, 0}, // a
		{42, 1}, {30, 1}, {30, 0}, {48, 1}, {48, 0}, {42, 0}, // AB under one Shift
		{46, 1}, {46, 0}, // c
		{42, 1}, {32, 1}, {32, 0}, {42, 0}, // D
		{42, 1}, {18, 1}, {18, 0}, {42, 0}, // E
		{14, 1}, {14, 0}, // BackSpace
		{29, 1}, {30, 1}, {30, 0}, {29, 0}, // ctrl+a
		{18, 1}, {18, 0}, // é, on a keymap of its own, on the E key
		{86, 1}, {86, 0}, // XF86AudioPlay, likewise, on the spare key
	}
	if got := keysSent(f, v.keyboard); !reflect.DeepEqual(got, want) {
		t.Errorf("keys =\n%v\nwant\n%v", got, want)
	}
	if n := len(f.gotKeymaps()); n != 5 { // US, é, US, XF86AudioPlay, US
		t.Errorf("%d keymaps sent, want 5", n)
	}

	// Fast letters overlap: the second goes down before the first is up. The same
	// letter twice cannot, and a capital waits for every key to be up.
	f.mu.Lock()
	f.requests = nil
	f.mu.Unlock()
	if err := v.Strokes(context.Background(), []Stroke{
		{Char: 'a', Hold: 120 * ms}, {Char: 's', Before: 30 * ms, Hold: 20 * ms}, {Char: 'a', Before: 30 * ms, Hold: 80 * ms},
		{Char: 'D', Before: 30 * ms, Hold: 10 * ms},
	}); err != nil {
		t.Fatal(err)
	}
	_ = v.Sync()
	want = [][2]uint32{{30, 1}, {31, 1}, {31, 0}, {30, 0}, {30, 1}, {30, 0}, {42, 1}, {32, 1}, {32, 0}, {42, 0}}
	if got := keysSent(f, v.keyboard); !reflect.DeepEqual(got, want) {
		t.Errorf("overlapping keys =\n%v\nwant\n%v", got, want)
	}

	// A cancelled call stops between strokes and leaves nothing down.
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(60 * ms); cancel() }()
	err = v.Strokes(ctx, []Stroke{{Char: 'X', Hold: 5 * ms}, {Char: 'Y', Before: 250 * ms}, {Char: 'Z', Before: 5 * time.Second}})
	if err != context.Canceled {
		t.Errorf("cancelled strokes = %v, want context.Canceled", err)
	}
	if len(v.keys) != 0 || v.mods != 0 {
		t.Errorf("after a cancelled call, keys %v and modifiers %#x are held", v.keys, v.mods)
	}
	if err := v.Strokes(context.Background(), []Stroke{{Key: Combo{Key: "no such key!"}}}); err == nil {
		t.Error("a bad keysym name was accepted")
	}
}

// humanDesk is a desk whose Human follows from a seed and never sleeps.
func humanDesk(scaled, byDefault bool) (*fakeDesk, *fakeA11y, *Desktop) {
	f, a, d := newDesk(scaled)
	d.Human = NewHuman(11)
	d.Human.Sleep = func(time.Duration) {}
	d.AsHuman = byDefault
	return f, a, d
}

// moves splits a fake's events into the leading pointer motion and the rest.
func moves(events []string) (motion, rest []string) {
	i := 0
	for i < len(events) && strings.HasPrefix(events[i], "move ") {
		i++
	}
	return events[:i], events[i:]
}

func TestClickAsHuman(t *testing.T) {
	f, _, d := humanDesk(true, false)
	cs := connect(t, d)
	res := call(t, cs, "click", map[string]any{"x": 100, "y": 50, "count": 2, "as_human": true})
	if res.IsError {
		t.Fatal(textOf(res))
	}
	motion, rest := moves(f.events)
	if len(motion) < 10 {
		t.Fatalf("the pointer got there in %d moves: %q", len(motion), motion)
	}
	var x, y int
	if _, err := fmtSscan(motion[len(motion)-1], &x, &y); err != nil || x < 198 || x > 202 || y < 98 || y > 102 {
		t.Errorf("the pointer ended at %q, want within 2 pixels of (200,100)", motion[len(motion)-1])
	}
	if want := []string{"press left", "release left", "press left", "release left"}; !reflect.DeepEqual(rest, want) {
		t.Errorf("after moving = %q, want %q", rest, want)
	}
	if d.at != [2]int{x, y} {
		t.Errorf("the pointer is remembered at %v, not where it went (%d,%d)", d.at, x, y)
	}

	// The next path starts where this one ended, not across the screen.
	f.events = nil
	call(t, cs, "move", map[string]any{"x": 110, "y": 50, "as_human": true})
	var nx, ny int
	if _, err := fmtSscan(f.events[0], &nx, &ny); err != nil || math.Hypot(float64(nx-x), float64(ny-y)) > 12 {
		t.Errorf("the next move starts at %q, far from (%d,%d)", f.events[0], x, y)
	}
	if last := f.events[len(f.events)-1]; last != "move 220 100" {
		t.Errorf("move ended at %q, want exactly move 220 100", last)
	}
}

func TestAsHumanByDefault(t *testing.T) {
	f, a, d := humanDesk(false, true)
	a.bounds = Rect{X: 400, Y: 300, Width: 200, Height: 40}
	cs := connect(t, d)
	if !strings.Contains(cs.InitializeResult().Instructions, "-as-human") {
		t.Error("the instructions do not say the server acts as a person")
	}

	// An exact call when asked for one.
	call(t, cs, "click", map[string]any{"x": 100, "y": 50, "as_human": false})
	if want := []string{"move 99 50", "move 100 50", "press left", "release left"}; !reflect.DeepEqual(f.events, want) {
		t.Errorf("as_human:false events = %q, want %q", f.events, want)
	}

	// click_element moves the pointer onto the widget instead of asking the app.
	f.events = nil
	got := textOf(call(t, cs, "click_element", map[string]any{"ref": "e1"}))
	motion, rest := moves(f.events)
	var x, y int
	_, _ = fmtSscan(motion[len(motion)-1], &x, &y)
	if !strings.Contains(got, "with the pointer") || len(motion) < 10 || x < 440 || x >= 560 || y < 308 || y >= 332 ||
		!reflect.DeepEqual(rest, []string{"press left", "release left"}) {
		t.Errorf("click_element = %q, ending %q then %q", got, motion[len(motion)-1], rest)
	}
	// A named action is still the app's to perform.
	if got := textOf(call(t, cs, "click_element", map[string]any{"ref": "e1", "action": "expand"})); got != "Performed click on e1." {
		t.Errorf("click_element with an action = %q", got)
	}

	// type and key go out as paced strokes; set_text clicks, selects and types.
	f.events = nil
	got = textOf(call(t, cs, "type", map[string]any{"text": "hi", "typos": false}))
	call(t, cs, "key", map[string]any{"keys": "ctrl+s Return"})
	if want := []string{"strokes hi", "strokes <ctrl+s><Return>"}; !reflect.DeepEqual(f.events, want) || got != "Typed 2 characters as a person would." {
		t.Errorf("type and key = %q (%q), want %q", f.events, got, want)
	}
	f.events = nil
	got = textOf(call(t, cs, "set_text", map[string]any{"ref": "e1", "text": ""}))
	_, rest = moves(f.events)
	if want := []string{"press left", "release left", "strokes <ctrl+a><BackSpace>", "strokes "}; !reflect.DeepEqual(rest, want) || !strings.Contains(got, "typed 0 characters over it") {
		t.Errorf("set_text = %q (%q), want %q", rest, got, want)
	}

	// scroll turns the wheel a click at a time, and a pointer already there stays.
	f.events = nil
	call(t, cs, "scroll", map[string]any{"x": 300, "y": 300, "direction": "down", "amount": 4})
	motion, rest = moves(f.events)
	if want := []string{"scroll down 1", "scroll down 1", "scroll down 1", "scroll down 1"}; len(motion) < 5 || !reflect.DeepEqual(rest, want) {
		t.Errorf("scroll = %d moves then %q", len(motion), rest)
	}
	f.events = nil
	call(t, cs, "scroll", map[string]any{"x": 302, "y": 301, "direction": "up", "amount": 1})
	if want := []string{"scroll up 1"}; !reflect.DeepEqual(f.events, want) {
		t.Errorf("a second scroll at the same place = %q, want %q", f.events, want)
	}

	// A drag starts and ends on the pixels asked for.
	f.events = nil
	call(t, cs, "drag", map[string]any{"from_x": 10, "from_y": 10, "to_x": 500, "to_y": 400})
	press := -1
	for i, e := range f.events {
		if e == "press left" {
			press = i
		}
	}
	n := len(f.events)
	if press < 1 || f.events[press-1] != "move 10 10" || f.events[n-1] != "release left" || f.events[n-2] != "move 500 400" || n-press < 20 {
		t.Errorf("drag = %q", f.events)
	}
}

func TestPointerPlaceIsShared(t *testing.T) {
	f, _, d := humanDesk(false, true)
	var kept [][2]int
	d.PointerAt = func() (int, int, bool) { return 1000, 700, true } // where another server left it
	d.SetPointerAt = func(x, y int) { kept = append(kept, [2]int{x, y}) }
	cs := connect(t, d)
	call(t, cs, "move", map[string]any{"x": 900, "y": 650})
	var x, y int
	if _, err := fmtSscan(f.events[0], &x, &y); err != nil || math.Hypot(float64(x-1000), float64(y-700)) > 12 {
		t.Errorf("the path starts at %q, not from (1000,700) where the pointer was left", f.events[0])
	}
	if want := [][2]int{{900, 650}}; !reflect.DeepEqual(kept, want) {
		t.Errorf("kept %v, want %v once the move was done", kept, want)
	}
	// Within one action the pointer is where this server has just put it, not
	// where the last action left it: a drag's second leg starts at its first's end.
	d.PointerAt = func() (int, int, bool) { n := len(kept) - 1; return kept[n][0], kept[n][1], true }
	f.events = nil
	call(t, cs, "batch", map[string]any{"actions": []map[string]any{
		{"tool": "drag", "args": map[string]any{"from_x": 100, "from_y": 100, "to_x": 600, "to_y": 120}},
		{"tool": "click", "args": map[string]any{"x": 300, "y": 500}},
	}})
	at := [2]int{900, 650}
	for _, e := range f.events {
		if _, err := fmtSscan(e, &x, &y); err != nil {
			continue
		}
		if jump := math.Hypot(float64(x-at[0]), float64(y-at[1])); jump > 90 {
			t.Fatalf("the pointer jumped %.0f pixels, from %v to (%d,%d)", jump, at, x, y)
		}
		at = [2]int{x, y}
	}
	if n := len(kept); n != 2 || kept[1] != at {
		t.Errorf("kept %v, want one more place, %v, when the batch was done", kept, at)
	}

	// A place on another screen is no place to start from: the pointer is put somewhere here.
	d.PointerAt = func() (int, int, bool) { return 5000, 100, true }
	f.events = nil
	call(t, cs, "move", map[string]any{"x": 10, "y": 10})
	if _, err := fmtSscan(f.events[0], &x, &y); err != nil || x < 320 || x > 960 || y < 200 || y > 600 {
		t.Errorf("with the pointer off this screen, the path starts at %q", f.events[0])
	}
}

func TestAsHumanIsInTheSchemas(t *testing.T) {
	_, _, d := humanDesk(false, false)
	cs := connect(t, d)
	if strings.Contains(cs.InitializeResult().Instructions, "-as-human") {
		t.Error("the instructions say the server acts as a person when it does not")
	}
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"click": true, "move": true, "drag": true, "mouse_down": true, "mouse_up": true,
		"scroll": true, "type": true, "key": true, "click_element": true, "set_text": true, "scroll_to": true, "set_value": true}
	for _, tool := range res.Tools {
		schema, _ := tool.InputSchema.(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		if _, has := props["as_human"]; has != want[tool.Name] {
			t.Errorf("%s: as_human in its schema = %v, want %v", tool.Name, has, want[tool.Name])
		}
	}
}

// fmtSscan reads the coordinates out of a fake's "move x y" event.
func fmtSscan(event string, x, y *int) (int, error) {
	return fmt.Sscanf(event, "move %d %d", x, y)
}
