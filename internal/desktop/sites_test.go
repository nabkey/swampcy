package desktop

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conf", "sites.json")
	s := &Sites{Path: path}
	yes, no := true, false
	if on, rule, err := s.For("www.amazon.com"); err != nil || on || rule != "" {
		t.Fatalf("with no file: %v %q %v, want no setting", on, rule, err)
	}
	for site, v := range map[string]*bool{"amazon.com": &yes, "smile.amazon.com": &no, "localhost:3000": &yes, EverySite: &no} {
		if err := s.Set(site, v); err != nil {
			t.Fatal(err)
		}
	}
	// Another server reads what this one wrote.
	other := &Sites{Path: path}
	for _, tc := range []struct {
		host string
		on   bool
		rule string
	}{
		{"amazon.com", true, "amazon.com"},
		{"www.amazon.com", true, "amazon.com"},
		{"www.amazon.com:8443", true, "amazon.com"},
		{"smile.amazon.com", false, "smile.amazon.com"}, // the more specific site
		{"notamazon.com", false, EverySite},
		{"localhost:3000", true, "localhost:3000"},
		{"localhost:8080", false, EverySite},
		{"", false, EverySite}, // a window that shows no page
	} {
		if on, rule, err := other.For(tc.host); err != nil || on != tc.on || rule != tc.rule {
			t.Errorf("For(%q) = %v by %q (%v), want %v by %q", tc.host, on, rule, err, tc.on, tc.rule)
		}
	}
	sites, by, err := other.All()
	if want := []string{"*", "amazon.com", "localhost:3000", "smile.amazon.com"}; err != nil || !reflect.DeepEqual(sites, want) || !by["amazon.com"] {
		t.Errorf("All = %v %v (%v), want %v", sites, by, err, want)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the file's mode = %v (%v), want 0600", fi.Mode().Perm(), err)
	}

	// A setting taken away by one is gone for the other.
	if err := other.Set(EverySite, nil); err != nil {
		t.Fatal(err)
	}
	if err := other.Set("amazon.com", nil); err != nil {
		t.Fatal(err)
	}
	if on, rule, _ := s.For("www.amazon.com"); on || rule != "" {
		t.Errorf("after removing: %v by %q, want no setting", on, rule)
	}

	if err := s.Set("example.com", &yes); err != nil {
		t.Fatal(err)
	}
	if on, _, _ := s.For("example.com"); !on {
		t.Fatal("example.com was not set")
	}

	// A file that does not parse is an error, is not written over, and leaves
	// what was last read in force.
	if err := os.WriteFile(path, []byte(`{"as_human": {"amazon.com": tru`), 0o600); err != nil {
		t.Fatal(err)
	}
	if on, rule, err := s.For("www.example.com"); err == nil || !on || rule != "example.com" {
		t.Errorf("with a broken file: %v by %q (%v), want the last setting and an error", on, rule, err)
	}
	if err := s.Set("example.org", &yes); err == nil {
		t.Error("a broken file was written over")
	}
	if b, _ := os.ReadFile(path); !strings.HasSuffix(string(b), "tru") {
		t.Errorf("the broken file is now %q", b)
	}
}

func TestSiteName(t *testing.T) {
	for in, want := range map[string]string{
		"Amazon.com": "amazon.com", "https://www.amazon.com/dp/B01?x=1": "amazon.com", " localhost:3000 ": "localhost:3000",
		"http://127.0.0.1:8765/": "127.0.0.1:8765", "*": "*", "www.example.co.uk": "example.co.uk",
	} {
		if got, err := SiteName(in); err != nil || got != want {
			t.Errorf("SiteName(%q) = %q (%v), want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "*.amazon.com", "two words", "https://"} {
		if got, err := SiteName(bad); err == nil {
			t.Errorf("SiteName(%q) = %q, want an error", bad, got)
		}
	}
	if got := siteOfURL("https://WWW.Amazon.com:8443/dp/1"); got != "www.amazon.com:8443" {
		t.Errorf("siteOfURL = %q", got)
	}
	if got := siteOfURL("about:blank"); got != "" {
		t.Errorf("siteOfURL(about:blank) = %q, want none", got)
	}
}

func TestSiteSettingsDecideHowToAct(t *testing.T) {
	f, a, d := humanDesk(false, false)
	d.Sites = &Sites{Path: filepath.Join(t.TempDir(), "sites.json")}
	cs := connect(t, d)
	if !strings.Contains(cs.InitializeResult().Instructions, "set_site_settings") {
		t.Error("the instructions do not mention the site settings")
	}
	exact := []string{"move 99 50", "move 100 50", "press left", "release left"}
	click := func(args map[string]any) []string {
		t.Helper()
		f.events, d.pages = nil, nil // a page looked at afresh
		args["x"], args["y"] = 100, 50
		if res := call(t, cs, "click", args); res.IsError {
			t.Fatal(textOf(res))
		}
		return f.events
	}
	a.url = "https://www.amazon.com/dp/B01"
	if got := click(map[string]any{}); !reflect.DeepEqual(got, exact) {
		t.Errorf("with no setting: %q, want %q", got, exact)
	}

	got := textOf(call(t, cs, "set_site_settings", map[string]any{"site": "https://www.Amazon.com/", "as_human": true}))
	if got != "swampcy now acts as a person on amazon.com, in this and later sessions." {
		t.Errorf("set_site_settings = %q", got)
	}
	if got := click(map[string]any{}); reflect.DeepEqual(got, exact) { // a short path: the pointer is beside it already
		t.Errorf("on amazon.com the click was %q, not a person's", got)
	}
	if got := click(map[string]any{"as_human": false}); !reflect.DeepEqual(got, exact) {
		t.Errorf("as_human:false on amazon.com: %q, want %q", got, exact)
	}
	got = textOf(call(t, cs, "site_settings", nil))
	for _, want := range []string{"The focused window shows www.amazon.com: there swampcy acts as a person (set for amazon.com).",
		"  amazon.com: as a person", "Anywhere else: at once and exactly (the server runs without -as-human).", d.Sites.Path} {
		if !strings.Contains(got, want) {
			t.Errorf("site_settings = %q, want it to hold %q", got, want)
		}
	}

	// Another site, and a window that shows no page, are not amazon.com.
	a.url = "http://localhost:3000/cart"
	if got := click(map[string]any{}); !reflect.DeepEqual(got, exact) {
		t.Errorf("on localhost: %q, want %q", got, exact)
	}
	// The setting for every site is over the server's flag, and a site's own over that.
	d.AsHuman = true
	call(t, cs, "set_site_settings", map[string]any{"site": "*", "as_human": false})
	if got := click(map[string]any{}); !reflect.DeepEqual(got, exact) {
		t.Errorf("with * off and -as-human: %q, want %q", got, exact)
	}
	a.url = "https://smile.amazon.com/"
	f.events, d.pages = nil, nil
	call(t, cs, "type", map[string]any{"text": "ok"})
	if want := []string{"strokes ok"}; !reflect.DeepEqual(f.events, want) {
		t.Errorf("typing on amazon.com = %q, want %q", f.events, want)
	}
	if got := textOf(call(t, cs, "site_settings", map[string]any{"site": "example.org"})); !strings.Contains(got, "On example.org swampcy acts at once and exactly (the setting for every site).") {
		t.Errorf("site_settings for example.org = %q", got)
	}

	call(t, cs, "set_site_settings", map[string]any{"site": "amazon.com", "remove": true})
	call(t, cs, "set_site_settings", map[string]any{"site": "*", "remove": true})
	if got := textOf(call(t, cs, "site_settings", nil)); !strings.Contains(got, "No site has a setting.") || !strings.Contains(got, "Anywhere else: as a person") {
		t.Errorf("site_settings with nothing set = %q", got)
	}
	for _, bad := range []map[string]any{{"site": "amazon.com"}, {"site": "amazon.com", "as_human": true, "remove": true}, {"site": "not a site", "as_human": true}} {
		if res := call(t, cs, "set_site_settings", bad); !res.IsError {
			t.Errorf("set_site_settings %v succeeded", bad)
		}
	}
}

// count is how many of a fake's events are exactly event.
func count(f *fakeDesk, event string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.events {
		if e == event {
			n++
		}
	}
	return n
}

func TestScrollToAsHuman(t *testing.T) {
	f, a, d := humanDesk(false, true)
	// A page in window 7 (content 2,24 636x774) that each wheel click moves 110 pixels.
	a.boundsFn = func() Rect {
		return Rect{X: 300, Y: 2500 - 110*count(f, "scroll down 1") + 110*count(f, "scroll up 1"), Width: 80, Height: 20}
	}
	cs := connect(t, d)
	got := textOf(call(t, cs, "scroll_to", map[string]any{"ref": "e1"}))
	y := a.boundsFn().Y + 10
	if a.scrolledTo != 0 || y < 24+774/4 || y > 24+774*3/4 || !strings.HasPrefix(got, "Scrolled to it: ") {
		t.Errorf("scroll_to left the element at y=%d after asking the app %d times: %q", y, a.scrolledTo, got)
	}
	motion, rest := moves(f.events)
	var x, py int
	_, _ = fmtSscan(motion[len(motion)-1], &x, &py)
	if x < 2 || x >= 638 || py < 24 || py >= 798 {
		t.Errorf("the wheel was turned with the pointer at %q, outside the window", motion[len(motion)-1])
	}
	for _, e := range rest {
		if e != "scroll down 1" {
			t.Fatalf("after the pointer moved: %q, want only single wheel clicks down", rest)
		}
	}
	// Back up the page, without moving the pointer again.
	f.events = nil
	a.boundsFn = func() Rect { return Rect{X: 300, Y: -900 + 110*count(f, "scroll up 1"), Width: 80, Height: 20} }
	call(t, cs, "scroll_to", map[string]any{"ref": "e1"})
	if n := count(f, "scroll up 1"); n != len(f.events) || n < 9 || n > 14 {
		t.Errorf("scrolling up = %q", f.events)
	}

	// An element the wheel does not move is the app's to scroll, and the result says so.
	a.boundsFn = func() Rect { return Rect{X: 300, Y: 3000, Width: 80, Height: 20} }
	got = textOf(call(t, cs, "scroll_to", map[string]any{"ref": "e1"}))
	if a.scrolledTo != 1 || !strings.Contains(got, "not as a person would") {
		t.Errorf("a stuck element: asked the app %d times, %q", a.scrolledTo, got)
	}
	// One at the top of a page that goes no further is where it is.
	a.boundsFn = func() Rect { return Rect{X: 300, Y: 40, Width: 80, Height: 20} }
	got = textOf(call(t, cs, "scroll_to", map[string]any{"ref": "e1"}))
	if a.scrolledTo != 1 || strings.Contains(got, "not as a person") {
		t.Errorf("an element already showing at the top: asked the app %d times, %q", a.scrolledTo, got)
	}
	// Not as a person, the app is asked at once.
	f.events = nil
	call(t, cs, "scroll_to", map[string]any{"ref": "e1", "as_human": false})
	if a.scrolledTo != 2 || len(f.events) != 0 {
		t.Errorf("as_human:false asked the app %d times and sent %q", a.scrolledTo, f.events)
	}
}

func TestAsHumanReachesWhatItClicks(t *testing.T) {
	f, a, d := humanDesk(false, true)
	// A field below the fold of window 7, which each wheel click brings 110 pixels up.
	a.boundsFn = func() Rect {
		return Rect{X: 200, Y: 1900 - 110*count(f, "scroll down 1"), Width: 300, Height: 30}
	}
	cs := connect(t, d)
	got := textOf(call(t, cs, "set_text", map[string]any{"ref": "e1", "text": "below the fold", "typos": false}))
	press := -1
	for i, e := range f.events {
		if e == "press left" {
			press = i
		}
	}
	var x, y int
	if press < 2 || count(f, "scroll down 1") < 10 {
		t.Fatalf("set_text on a field out of view = %q, events %q", got, f.events)
	}
	_, _ = fmtSscan(f.events[press-1], &x, &y)
	r := a.boundsFn()
	if x < r.X || x >= r.X+r.Width || y < r.Y || y >= r.Y+r.Height || y >= 798 {
		t.Errorf("the click went to (%d,%d), not onto the field at %+v", x, y, r)
	}
	// The fake's field holds "hello": it is selected and typed over.
	if n := len(f.events); f.events[n-2] != "strokes <ctrl+a>" || f.events[n-1] != "strokes below the fold" {
		t.Errorf("after the click = %q", f.events[press:])
	}

	// One that cannot be brought into view is not clicked at all.
	f.events = nil
	a.boundsFn = func() Rect { return Rect{X: 200, Y: 5000, Width: 300, Height: 30} }
	if res := call(t, cs, "click_element", map[string]any{"ref": "e1"}); !res.IsError || count(f, "press left") != 0 {
		t.Errorf("click_element on what cannot be reached = %q, events %q", textOf(res), f.events)
	}
	// A line break would be Return: refused, with the way round it.
	a.boundsFn = nil
	f.events = nil
	if res := call(t, cs, "set_text", map[string]any{"ref": "e1", "text": "two\nlines"}); !res.IsError || len(f.events) != 0 || !strings.Contains(textOf(res), "clipboard_set") {
		t.Errorf("set_text with a line break = %q, events %q", textOf(res), f.events)
	}

	// Modifiers go down before the click and up after, and a move with a button
	// held does not run past its point.
	a.bounds = Rect{X: 100, Y: 100, Width: 50, Height: 50}
	f.events = nil
	call(t, cs, "click", map[string]any{"x": 300, "y": 300, "modifiers": "ctrl+shift"})
	_, rest := moves(f.events)
	if want := []string{"mods [ctrl] true", "mods [shift] true", "press left", "release left", "mods [shift] false", "mods [ctrl] false"}; !reflect.DeepEqual(rest, want) {
		t.Errorf("a click with modifiers = %q, want %q", rest, want)
	}
	for seed := uint64(1); seed <= 40; seed++ {
		d.Human = NewHuman(seed)
		d.Human.Sleep = func(time.Duration) {}
		call(t, cs, "mouse_down", map[string]any{"x": 100, "y": 400})
		f.events = nil
		call(t, cs, "move", map[string]any{"x": 600, "y": 400})
		for _, e := range f.events {
			if _, err := fmtSscan(e, &x, &y); err == nil && x > 602 {
				t.Fatalf("seed %d: with a button held the pointer ran past its point, to %q", seed, e)
			}
		}
		call(t, cs, "mouse_up", nil)
	}
}

func TestSetValueAsHuman(t *testing.T) {
	f, a, d := humanDesk(false, true)
	a.bounds = Rect{X: 100, Y: 400, Width: 400, Height: 20}
	// A slider whose track takes clicks to the nearest 5 below, and arrow keys by 1.
	slider := func(start float64, more, less string, track bool) func() float64 {
		return func() float64 {
			f.mu.Lock()
			defer f.mu.Unlock()
			v, x, y := start, 0, 0
			for _, e := range f.events {
				switch {
				case strings.HasPrefix(e, "move "):
					fmt.Sscanf(e, "move %d %d", &x, &y)
				case e == "press left" && track:
					v = float64((x - 100) * 100 / 399 / 5 * 5)
				case e == "strokes <"+more+">":
					v++
				case e == "strokes <"+less+">":
					v--
				}
			}
			return v
		}
	}
	a.valueFn = slider(10, "Right", "Left", true)
	cs := connect(t, d)
	got := textOf(call(t, cs, "set_value", map[string]any{"ref": "e1", "value": 63}))
	if got != "e1 is now 63 (0..100)." || len(a.valuesSet) != 0 || count(f, "press left") != 1 || count(f, "strokes <Right>") != 3 {
		t.Errorf("set_value = %q, app asked %v, events %q", got, a.valuesSet, f.events)
	}
	// Already there: nothing to do.
	f.events = nil
	a.valueFn = func() float64 { return 63 }
	if got := textOf(call(t, cs, "set_value", map[string]any{"ref": "e1", "value": 63})); got != "e1 is now 63 (0..100)." || len(f.events) != 0 {
		t.Errorf("set_value to what it has = %q with %q", got, f.events)
	}
	// One that counts the other way round, and a spin button, which takes Up and Down.
	f.events = nil
	a.valueFn = slider(60, "Left", "Right", false)
	if got := textOf(call(t, cs, "set_value", map[string]any{"ref": "e1", "value": 57})); got != "e1 is now 57 (0..100)." || len(a.valuesSet) != 0 {
		t.Errorf("a slider that counts leftwards = %q, app asked %v", got, a.valuesSet)
	}
	f.events = nil
	a.valueFn = slider(10, "Up", "Down", false)
	if got := textOf(call(t, cs, "set_value", map[string]any{"ref": "e1", "value": 14})); got != "e1 is now 14 (0..100)." || count(f, "strokes <Up>") != 4 || len(a.valuesSet) != 0 {
		t.Errorf("a spin button = %q, app asked %v, events %q", got, a.valuesSet, f.events)
	}
	// Chrome's slider: a click on the track changes it, but the bus goes on saying
	// the old value until a key is pressed.
	f.events = nil
	live := slider(63, "Right", "Left", true)
	a.valueFn = func() float64 {
		f.mu.Lock()
		keyed := len(f.events) > 0 && strings.HasPrefix(f.events[len(f.events)-1], "strokes ")
		f.mu.Unlock()
		if count(f, "press left") == 0 || keyed || count(f, "strokes <Right>")+count(f, "strokes <Left>") > 0 {
			return live()
		}
		return 63
	}
	if got := textOf(call(t, cs, "set_value", map[string]any{"ref": "e1", "value": 22})); got != "e1 is now 22 (0..100)." || len(a.valuesSet) != 0 {
		t.Errorf("a slider that reports its clicks late = %q, app asked %v, events %q", got, a.valuesSet, f.events)
	}
	// Too far for keys: the app sets it, and the result says so.
	f.events = nil
	a.valueFn = slider(10, "Up", "Down", false)
	got = textOf(call(t, cs, "set_value", map[string]any{"ref": "e1", "value": 90}))
	if !reflect.DeepEqual(a.valuesSet, []float64{90}) || !strings.Contains(got, "not as a person would") || count(f, "strokes <Up>") > 3 {
		t.Errorf("a far value = %q, app asked %v after %d key presses", got, a.valuesSet, count(f, "strokes <Up>"))
	}
	if res := call(t, cs, "set_value", map[string]any{"ref": "e1", "value": 101}); !res.IsError {
		t.Error("a value out of range was accepted")
	}
}
