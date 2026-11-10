package desktop

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func images(res *mcp.CallToolResult) int {
	n := 0
	for _, c := range res.Content {
		if _, ok := c.(*mcp.ImageContent); ok {
			n++
		}
	}
	return n
}

func TestScreenshotAfterAnAction(t *testing.T) {
	f, _, d := newDesk(false)
	settled := 0
	d.Settle = func(context.Context, time.Duration, time.Duration) bool { settled++; return true }
	cs := connect(t, d)
	if res := call(t, cs, "click", map[string]any{"x": 10, "y": 10}); images(res) != 0 {
		t.Error("a click returned a screenshot nobody asked for")
	}
	res := call(t, cs, "click", map[string]any{"x": 10, "y": 10, "screenshot": true})
	if images(res) != 1 || settled != 1 || len(f.shots) != 1 {
		t.Errorf("images %d, settled %d, captures %d; want one of each", images(res), settled, len(f.shots))
	}
	if got := textOf(res); !strings.Contains(got, "Clicked left") || !strings.Contains(got, "Screen 1280x800") {
		t.Errorf("result = %q", got)
	}
	// A screen that never settles is shown anyway, and said to be moving.
	d.Settle = func(context.Context, time.Duration, time.Duration) bool { return false }
	if got := textOf(call(t, cs, "key", map[string]any{"keys": "Return", "screenshot": true})); !strings.Contains(got, "still changing") {
		t.Errorf("unsettled result = %q", got)
	}
}

func TestClickWithModifiers(t *testing.T) {
	f, _, d := newDesk(false)
	cs := connect(t, d)
	call(t, cs, "click", map[string]any{"x": 10, "y": 20, "modifiers": "ctrl+shift"})
	want := []string{"move 10 20", "mods [ctrl shift] true", "press left", "release left", "mods [ctrl shift] false"}
	if !slices.Equal(f.events, want) {
		t.Errorf("events = %q, want %q", f.events, want)
	}
	if res := call(t, cs, "click", map[string]any{"x": 1, "y": 1, "modifiers": "hyper"}); !res.IsError {
		t.Error("an unknown modifier was accepted")
	}
}

func TestKeyRepeatAndHold(t *testing.T) {
	f, _, d := newDesk(false)
	cs := connect(t, d)
	call(t, cs, "key", map[string]any{"keys": "Down", "repeat": 3})
	call(t, cs, "key", map[string]any{"keys": "shift+Right", "hold_ms": 5})
	want := []string{"keys [{[] Down} {[] Down} {[] Down}]", "hold {[shift] Right} 5ms"}
	if !slices.Equal(f.events, want) {
		t.Errorf("events = %q, want %q", f.events, want)
	}
	if res := call(t, cs, "key", map[string]any{"keys": "a b", "hold_ms": 5}); !res.IsError {
		t.Error("holding two keys was accepted")
	}
}

func TestMouseDownUpAndDragVia(t *testing.T) {
	f, _, d := newDesk(false)
	cs := connect(t, d)
	call(t, cs, "mouse_down", map[string]any{"x": 5, "y": 5})
	call(t, cs, "mouse_up", map[string]any{})
	if want := []string{"move 5 5", "press left", "release left"}; !slices.Equal(f.events, want) {
		t.Errorf("events = %q, want %q", f.events, want)
	}
	if res := call(t, cs, "mouse_down", map[string]any{}); !res.IsError {
		t.Error("mouse_down with no point was accepted")
	}
	f.events = nil
	call(t, cs, "drag", map[string]any{"from_x": 0, "from_y": 0, "to_x": 100, "to_y": 0, "via": []any{map[string]any{"x": 0, "y": 100}}})
	if !slices.Contains(f.events, "move 0 100") || f.events[len(f.events)-2] != "move 100 0" || f.events[len(f.events)-1] != "release left" {
		t.Errorf("drag via (0,100) = %q", f.events)
	}
}

func TestBatchRunsInOrderAndStopsAtAFailure(t *testing.T) {
	f, _, d := newDesk(false)
	locks := 0
	d.LockInput = func() func() { locks++; return func() {} }
	cs := connect(t, d)
	res := call(t, cs, "batch", map[string]any{"screenshot": true, "actions": []any{
		map[string]any{"tool": "click", "args": map[string]any{"x": 10, "y": 10}},
		map[string]any{"tool": "type", "args": map[string]any{"text": "hi"}},
		map[string]any{"tool": "key", "args": map[string]any{"keys": "Return"}},
	}})
	want := []string{"move 10 10", "press left", "release left", `type "hi"`, "keys [{[] Return}]"}
	if !slices.Equal(f.events, want) {
		t.Errorf("events = %q, want %q", f.events, want)
	}
	if got := textOf(res); !strings.Contains(got, "1. Clicked") || !strings.Contains(got, "3. Pressed Return") || images(res) != 1 {
		t.Errorf("batch = %q with %d images", got, images(res))
	}
	if locks != 1 {
		t.Errorf("the input lock was taken %d times for one batch, want once", locks)
	}
	// The second step is refused (a point off the screen); the third never runs.
	f.events = nil
	res = call(t, cs, "batch", map[string]any{"actions": []any{
		map[string]any{"tool": "type", "args": map[string]any{"text": "a"}},
		map[string]any{"tool": "click", "args": map[string]any{"x": 99999, "y": 10}},
		map[string]any{"tool": "type", "args": map[string]any{"text": "never"}},
	}})
	if !res.IsError || !slices.Equal(f.events, []string{`type "a"`}) || !strings.Contains(textOf(res), "2. click failed") {
		t.Errorf("failed batch: error %v, events %q, text %q", res.IsError, f.events, textOf(res))
	}
	// Arguments are checked as the tool's own are: a click needs its point.
	for _, bad := range []map[string]any{
		{"tool": "click", "args": map[string]any{"x": 1}},
		{"tool": "batch", "args": map[string]any{}},
		{"tool": "nope"},
	} {
		if res := call(t, cs, "batch", map[string]any{"actions": []any{bad}}); !res.IsError {
			t.Errorf("batch accepted %v", bad)
		}
	}
}

func TestWaitFor(t *testing.T) {
	_, a, d := newDesk(false)
	cs := connect(t, d)
	a.findAfter = 3 // there on the third look
	if got := textOf(call(t, cs, "wait_for", map[string]any{"name": "Save", "timeout_ms": 3000})); !strings.Contains(got, "There after") || a.finds != 3 {
		t.Errorf("wait_for = %q after %d looks", got, a.finds)
	}
	a.finds, a.findAfter = 0, 1000
	if res := call(t, cs, "wait_for", map[string]any{"name": "Never", "timeout_ms": 300}); !res.IsError || !strings.Contains(textOf(res), "did not appear") {
		t.Errorf("timeout = %q", textOf(res))
	}
	if got := textOf(call(t, cs, "wait_for", map[string]any{"title": "calcul"})); !strings.Contains(got, `"Calculator"`) {
		t.Errorf("wait_for a window = %q", got)
	}
	if got := textOf(call(t, cs, "wait_for", map[string]any{"title": "no such window", "gone": true})); !strings.Contains(got, "Gone") {
		t.Errorf("wait_for gone = %q", got)
	}
}

func TestLaunchReportsACommandThatFailed(t *testing.T) {
	_, _, d := newDesk(false)
	d.Exited = func(pid int) (Exit, bool) {
		return Exit{Err: errors.New("exit status 2"), Output: "sh: syntax error near `('"}, true
	}
	start := time.Now()
	res := call(t, connect(t, d), "launch", map[string]any{"command": "google-chrome a(b)", "wait_ms": 5000})
	if got := textOf(res); !res.IsError || !strings.Contains(got, "syntax error") || !strings.Contains(got, "exit status 2") {
		t.Errorf("launch = %q", got)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("launch waited out the timeout for a command that had already failed")
	}
}

func TestCloseWindowAndValueAndClipboard(t *testing.T) {
	f, _, d := newDesk(false)
	clip := ""
	d.ClipboardText = func() (string, error) { return clip, nil }
	d.SetClipboardText = func(s string) error { clip = s; return nil }
	cs := connect(t, d)
	call(t, cs, "close_window", map[string]any{"window": 8})
	if !slices.Contains(f.events, "sway [con_id=8] kill") {
		t.Errorf("events = %q", f.events)
	}
	if got := textOf(call(t, cs, "set_value", map[string]any{"ref": "e1", "value": 40})); got != "e1 is now 40 (0..100)." {
		t.Errorf("set_value = %q", got)
	}
	if got := textOf(call(t, cs, "scroll_to", map[string]any{"ref": "e1"})); !strings.Contains(got, `e1 button "OK"`) {
		t.Errorf("scroll_to = %q", got)
	}
	call(t, cs, "clipboard_set", map[string]any{"text": "two\nlines"})
	if got := textOf(call(t, cs, "clipboard_get", nil)); got != "two\nlines" {
		t.Errorf("clipboard_get = %q", got)
	}
}

func TestZoomMagnifies(t *testing.T) {
	f, _, d := newDesk(false)
	res := call(t, connect(t, d), "zoom", map[string]any{"x": 100, "y": 100, "width": 200, "height": 100})
	if want := []string{"&{100 100 200 100} 3.00"}; !slices.Equal(f.shots, want) {
		t.Errorf("captures = %q, want %q", f.shots, want)
	}
	if !strings.Contains(textOf(res), "3.0x") {
		t.Errorf("summary = %q", textOf(res))
	}
	// A large region is magnified only as far as max-edge allows.
	f.shots = nil
	call(t, connect(t, d), "zoom", map[string]any{"x": 0, "y": 0, "width": 640, "height": 400})
	if want := []string{"&{0 0 640 400} 2.00"}; !slices.Equal(f.shots, want) {
		t.Errorf("captures = %q, want %q", f.shots, want)
	}
}

func TestOutlineLeavesOutNoise(t *testing.T) {
	// Chrome's context-menu action is on everything and is not listed.
	if got := shownActions([]string{"press", "showContextMenu"}); !slices.Equal(got, []string{"press"}) {
		t.Errorf("shownActions = %q", got)
	}
	// So an unnamed section with only that action is left out, its children kept.
	if !redundant(&node{role: "section", actions: []string{"showContextMenu"}}, nil) {
		t.Error("an unnamed section with only a context menu was kept")
	}
	if redundant(&node{role: "section", actions: []string{"click"}}, nil) {
		t.Error("an unnamed section that can be clicked was left out")
	}
	// A link's static text repeats its name.
	link := &node{role: "link", name: "Talk"}
	if !redundant(&node{role: "static", name: "Talk"}, link) || redundant(&node{role: "static", name: "Other"}, link) {
		t.Error("static text under a link: only a repeat of its name is left out")
	}
}
