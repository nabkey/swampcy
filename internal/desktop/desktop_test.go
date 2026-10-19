package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type fakeDesk struct {
	mu      sync.Mutex
	tree    string
	outputs string
	events  []string
	shots   []string
}

func (f *fakeDesk) log(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, fmt.Sprintf(format, args...))
}

func (f *fakeDesk) Tree() (*Node, error) {
	var n Node
	return &n, json.Unmarshal([]byte(f.tree), &n)
}

func (f *fakeDesk) Outputs() ([]Output, error) {
	var o []Output
	return o, json.Unmarshal([]byte(f.outputs), &o)
}

func (f *fakeDesk) Command(c string) error                 { f.log("sway %s", c); return nil }
func (f *fakeDesk) MoveTo(x, y int) error                  { f.log("move %d %d", x, y); return nil }
func (f *fakeDesk) Press(b string) error                   { f.log("press %s", b); return nil }
func (f *fakeDesk) Release(b string) error                 { f.log("release %s", b); return nil }
func (f *fakeDesk) Scroll(d string, n int) error           { f.log("scroll %s %d", d, n); return nil }
func (f *fakeDesk) Type(_ context.Context, s string) error { f.log("type %q", s); return nil }
func (f *fakeDesk) Keys(_ context.Context, cs []Combo) error {
	f.log("keys %v", cs)
	return nil
}

func (f *fakeDesk) Capture(_ context.Context, r *Rect, scale float64) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shots = append(f.shots, fmt.Sprintf("%v %.2f", r, scale))
	return []byte("\x89PNG fake"), "image/png", nil
}

type fakeA11y struct {
	activateErr error
	bounds      Rect
}

func (f *fakeA11y) Tree(_ context.Context, w Window, _ Geometry, o TreeOptions) (string, error) {
	return fmt.Sprintf("tree of %d depth %d nodes %d", w.ID, o.MaxDepth, o.MaxNodes), nil
}
func (f *fakeA11y) Find(_ context.Context, ws []Window, _ Geometry, q Query, _ TreeOptions) (string, error) {
	return fmt.Sprintf("find %q %q in %d windows, limit %d", q.Name, q.Role, len(ws), q.Limit), nil
}
func (f *fakeA11y) Activate(_ context.Context, ref, action string) (string, error) {
	if f.activateErr != nil {
		return "", f.activateErr
	}
	return "click", nil
}
func (f *fakeA11y) Bounds(context.Context, string, *Node) (Rect, error) { return f.bounds, nil }
func (f *fakeA11y) SetText(context.Context, string, string) error       { return nil }
func (f *fakeA11y) Text(context.Context, string) (string, error)        { return "hello", nil }

func connect(t *testing.T, d *Desktop) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := d.Server("test").Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func textOf(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func newDesk(scaled bool) (*fakeDesk, *fakeA11y, *Desktop) {
	f := &fakeDesk{tree: testTree, outputs: `[{"name":"HEADLESS-1","active":true,"rect":{"x":0,"y":0,"width":1280,"height":800}}]`}
	if scaled {
		f.outputs = `[{"name":"HEADLESS-1","active":true,"rect":{"x":0,"y":0,"width":2560,"height":1600}}]`
	}
	a := &fakeA11y{}
	d := &Desktop{Sway: f, Screen: f, Pointer: f, Keyboard: f, A11y: a, MaxEdge: 1280,
		Launch: func(cmd string) (int, error) { f.log("launch %s", cmd); return 4242, nil }}
	return f, a, d
}

func TestToolsAreListed(t *testing.T) {
	_, _, d := newDesk(false)
	cs := connect(t, d)
	var names []string
	for tool, err := range cs.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tool.Name)
	}
	want := []string{"screenshot", "zoom", "click", "move", "drag", "scroll", "type", "key", "windows", "focus_window",
		"move_window", "launch", "accessibility_tree", "find_elements", "click_element", "set_text", "get_text"}
	if len(names) != len(want) {
		t.Errorf("tools = %v, want %v", names, want)
	}
}

func TestScreenshotIsAnImage(t *testing.T) {
	f, _, d := newDesk(true)
	res := call(t, connect(t, d), "screenshot", nil)
	if res.IsError {
		t.Fatal(textOf(res))
	}
	img, ok := res.Content[0].(*mcp.ImageContent)
	if !ok || img.MIMEType != "image/png" || string(img.Data) != "\x89PNG fake" {
		t.Fatalf("first content = %#v, want the PNG", res.Content[0])
	}
	if got := textOf(res); !strings.Contains(got, "Screen 1280x800") || !strings.Contains(got, "Scaled from 2560x1600") || !strings.Contains(got, `Focused: 7 "Calculator"`) {
		t.Errorf("summary = %q", got)
	}
	if want := []string{"&{0 0 2560 1600} 0.50"}; !reflect.DeepEqual(f.shots, want) { // the output, not the layout
		t.Errorf("captures = %q, want %q", f.shots, want)
	}
}

func TestClickConvertsCoordinates(t *testing.T) {
	f, _, d := newDesk(true)
	cs := connect(t, d)
	res := call(t, cs, "click", map[string]any{"x": 100, "y": 50, "count": 2})
	if res.IsError {
		t.Fatal(textOf(res))
	}
	want := []string{"move 200 100", "press left", "release left", "press left", "release left"}
	if !reflect.DeepEqual(f.events, want) {
		t.Errorf("events = %q, want %q", f.events, want)
	}
	if res := call(t, cs, "click", map[string]any{"x": 1280, "y": 0}); !res.IsError {
		t.Error("clicked off screen")
	}
	if res := call(t, cs, "click", map[string]any{"x": 1, "y": 1, "button": "thumb"}); res.IsError {
		// the fake accepts any button; the real pointer refuses it
		t.Log(textOf(res))
	}
}

func TestKeyAndType(t *testing.T) {
	f, _, d := newDesk(false)
	cs := connect(t, d)
	call(t, cs, "type", map[string]any{"text": "-héllo"})
	call(t, cs, "key", map[string]any{"keys": "ctrl+s Return"})
	want := []string{`type "-héllo"`, "keys [{[ctrl] s} {[] Return}]"}
	if !reflect.DeepEqual(f.events, want) {
		t.Errorf("events = %q, want %q", f.events, want)
	}
	if res := call(t, cs, "key", map[string]any{"keys": "hyper+x"}); !res.IsError {
		t.Error("pressed an unknown modifier")
	}
}

func TestZoom(t *testing.T) {
	f, _, d := newDesk(true)
	res := call(t, connect(t, d), "zoom", map[string]any{"x": 100, "y": 100, "width": 200, "height": 100})
	if res.IsError {
		t.Fatal(textOf(res))
	}
	if want := []string{"&{200 200 400 200} 1.00"}; !reflect.DeepEqual(f.shots, want) {
		t.Errorf("captures = %q, want %q", f.shots, want)
	}
	if !strings.Contains(textOf(res), "2.0x") {
		t.Errorf("summary = %q", textOf(res))
	}
}

func TestWindowsAndFocus(t *testing.T) {
	f, _, d := newDesk(false)
	cs := connect(t, d)
	got := textOf(call(t, cs, "windows", nil))
	for _, want := range []string{`7 "Calculator" app=org.gnome.Calculator pid=4242 (0,0 640x800) workspace=1 focused`, `8 "xterm" app=XTerm`, `9 "Dialog"`, "floating"} {
		if !strings.Contains(got, want) {
			t.Errorf("windows missing %q:\n%s", want, got)
		}
	}
	call(t, cs, "focus_window", map[string]any{"window": 8})
	if res := call(t, cs, "focus_window", map[string]any{"window": 99}); !res.IsError {
		t.Error("focused a window that does not exist")
	}
	if want := []string{"sway [con_id=8] focus"}; !reflect.DeepEqual(f.events, want) {
		t.Errorf("events = %q, want %q", f.events, want)
	}
}

func TestLaunchReportsTheNewWindow(t *testing.T) {
	f, _, d := newDesk(false)
	before := f.tree
	f.tree = strings.Replace(before, `"id":7,`, `"id":70,`, 1) // the calculator's window is new after launching
	d.Launch = func(cmd string) (int, error) {
		f.mu.Lock()
		f.tree = before
		f.mu.Unlock()
		return 4242, nil
	}
	got := textOf(call(t, connect(t, d), "launch", map[string]any{"command": "gnome-calculator", "wait_ms": 2000}))
	if !strings.Contains(got, `Started pid 4242. New window: 7 "Calculator"`) {
		t.Errorf("launch = %q", got)
	}
}

func TestElementTools(t *testing.T) {
	f, a, d := newDesk(false)
	cs := connect(t, d)
	if got := textOf(call(t, cs, "accessibility_tree", nil)); got != "tree of 7 depth 25 nodes 400" {
		t.Errorf("tree of the focused window = %q", got)
	}
	if got := textOf(call(t, cs, "find_elements", map[string]any{"name": "equals"})); got != `find "equals" "" in 3 windows, limit 30` {
		t.Errorf("find = %q", got)
	}
	if res := call(t, cs, "find_elements", nil); !res.IsError {
		t.Error("find with no name or role succeeded")
	}
	if got := textOf(call(t, cs, "click_element", map[string]any{"ref": "e1"})); got != "Performed click on e1." {
		t.Errorf("click_element = %q", got)
	}
	a.activateErr = errNoAction
	a.bounds = Rect{X: 100, Y: 100, Width: 40, Height: 20}
	if got := textOf(call(t, cs, "click_element", map[string]any{"ref": "e1"})); got != "Clicked the centre of e1 at (120,110)." {
		t.Errorf("click_element without an action = %q", got)
	}
	if want := []string{"move 120 110", "press left", "release left"}; !reflect.DeepEqual(f.events, want) {
		t.Errorf("events = %q, want %q", f.events, want)
	}
	if got := textOf(call(t, cs, "get_text", map[string]any{"ref": "e1"})); got != "hello" {
		t.Errorf("get_text = %q", got)
	}
}

func TestA11yOff(t *testing.T) {
	_, _, d := newDesk(false)
	d.A11y = nil
	if res := call(t, connect(t, d), "accessibility_tree", nil); !res.IsError || !strings.Contains(textOf(res), "accessibility is off") {
		t.Errorf("tree with a11y off = %q", textOf(res))
	}
}

// With two outputs, the tools see and point at one: the first by default, or the
// one named.
func TestOneOutputOfTwo(t *testing.T) {
	// sway lists HEADLESS-2 first; the default is the top-left output all the same.
	const two = `[{"name":"HEADLESS-2","active":true,"rect":{"x":2000,"y":0,"width":1600,"height":900}},
		{"name":"HEADLESS-1","active":true,"rect":{"x":0,"y":0,"width":1280,"height":800}}]`
	f, _, d := newDesk(false)
	f.outputs = two
	cs := connect(t, d)
	if got := textOf(call(t, cs, "screenshot", nil)); !strings.Contains(got, "Screen 1280x800.") || strings.Contains(got, "Scaled") {
		t.Errorf("first output's summary = %q", got)
	}
	if res := call(t, cs, "click", map[string]any{"x": 1500, "y": 10}); !res.IsError {
		t.Error("clicked beyond the first output, on the second")
	}
	call(t, cs, "click", map[string]any{"x": 100, "y": 50})

	f2, _, d2 := newDesk(false)
	f2.outputs = two
	d2.Output = "HEADLESS-2"
	cs2 := connect(t, d2)
	if got := textOf(call(t, cs2, "screenshot", nil)); !strings.Contains(got, "Screen 1280x720.") || !strings.Contains(got, "Scaled from 1600x900") {
		t.Errorf("second output's summary = %q", got)
	}
	call(t, cs2, "click", map[string]any{"x": 0, "y": 0})
	if want := []string{"&{0 0 1280 800} 1.00", "&{2000 0 1600 900} 0.80"}; !reflect.DeepEqual(append(f.shots, f2.shots...), want) {
		t.Errorf("captures = %q, want %q", append(f.shots, f2.shots...), want)
	}
	if !slices.Contains(f.events, "move 100 50") || !slices.Contains(f2.events, "move 2000 0") {
		t.Errorf("moves = %q / %q, want 100,50 on the first and the second's origin", f.events, f2.events)
	}
	// The calculator is on HEADLESS-1: the second output's server says so.
	if got := textOf(call(t, cs2, "windows", nil)); !strings.Contains(got, "on HEADLESS-1 (another screen") {
		t.Errorf("windows from HEADLESS-2 = %q", got)
	}
	d2.Output = "HEADLESS-9"
	if res := call(t, cs2, "screenshot", nil); !res.IsError || !strings.Contains(textOf(res), "HEADLESS-2, HEADLESS-1") {
		t.Errorf("unknown output: %q", textOf(res))
	}
}

// Two screens: the agent's calculator (focused) and an editor on HEADLESS-1, and
// the owner's Steam on HEADLESS-2.
const twoTree = `{"id":1,"type":"root","name":"root","nodes":[
 {"id":3,"type":"output","name":"HEADLESS-1","rect":{"x":0,"y":0,"width":1280,"height":800},"nodes":[
  {"id":4,"type":"workspace","name":"1","nodes":[
   {"id":7,"type":"con","name":"Calculator","app_id":"org.gnome.Calculator","pid":4242,"focused":true,"rect":{"x":0,"y":0,"width":640,"height":800}},
   {"id":8,"type":"con","name":"Editor","app_id":"org.gnome.TextEditor","pid":4343,"rect":{"x":640,"y":0,"width":640,"height":800}}]}]},
 {"id":5,"type":"output","name":"HEADLESS-2","rect":{"x":1280,"y":0,"width":390,"height":844},"nodes":[
  {"id":6,"type":"workspace","name":"2","nodes":[
   {"id":9,"type":"con","name":"Steam","app_id":"steam","pid":4444,"rect":{"x":1280,"y":0,"width":390,"height":844}}]}]}]}`

const twoOutputs = `[{"name":"HEADLESS-1","active":true,"rect":{"x":0,"y":0,"width":1280,"height":800}},
	{"name":"HEADLESS-2","active":true,"rect":{"x":1280,"y":0,"width":390,"height":844}}]`

func TestMoveWindow(t *testing.T) {
	f, _, d := newDesk(false)
	f.tree, f.outputs = twoTree, twoOutputs
	cs := connect(t, d)
	for _, tc := range []struct {
		args map[string]any
		want string // the sway command, or "" for none
	}{
		{map[string]any{"window": 9}, "sway [con_id=9] move container to output HEADLESS-1; [con_id=9] focus"},
		{map[string]any{"window": 8, "output": "HEADLESS-2"}, "sway [con_id=8] move container to output HEADLESS-2; [con_id=7] focus"},
		{map[string]any{"window": 7, "output": "HEADLESS-2"}, "sway [con_id=7] move container to output HEADLESS-2; focus output HEADLESS-1"}, // focus stays on this screen
		{map[string]any{"window": 9, "output": "HEADLESS-2"}, ""},                                                                             // already there
	} {
		f.events = nil
		if res := call(t, cs, "move_window", tc.args); res.IsError {
			t.Errorf("move_window %v: %s", tc.args, textOf(res))
			continue
		}
		var got string
		if len(f.events) > 0 {
			got = f.events[len(f.events)-1]
		}
		if got != tc.want {
			t.Errorf("move_window %v sent %q, want %q", tc.args, got, tc.want)
		}
	}
	for _, bad := range []map[string]any{{"window": 99}, {"window": 9, "output": "HEADLESS-9"}, {"window": 9, "output": "HEADLESS-1; exec rm"}} {
		if res := call(t, cs, "move_window", bad); !res.IsError {
			t.Errorf("move_window %v was not refused", bad)
		}
	}
}

func TestLaunchBringsTheWindowHere(t *testing.T) {
	f, _, d := newDesk(false)
	f.outputs = twoOutputs
	f.tree = strings.Replace(twoTree, `"id":9,`, `"id":90,`, 1) // Steam's window is new after launching
	d.Launch = func(cmd string) (int, error) {
		f.mu.Lock()
		f.tree = twoTree
		f.mu.Unlock()
		return 4444, nil
	}
	got := textOf(call(t, connect(t, d), "launch", map[string]any{"command": "steam", "wait_ms": 2000}))
	if !strings.Contains(got, "Started pid 4444. New window: 9") {
		t.Errorf("launch = %q", got)
	}
	if !slices.Contains(f.events, "sway [con_id=9] move container to output HEADLESS-1; [con_id=9] focus") {
		t.Errorf("events = %q, want the new window moved to this screen", f.events)
	}
}

func TestLaunchActivatesTheKeyboardFirst(t *testing.T) {
	f, _, d := newDesk(false)
	call(t, connect(t, d), "launch", map[string]any{"command": "gnome-calculator", "wait_ms": 1})
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.events) < 2 || !strings.HasPrefix(f.events[0], "keys [{[] Shift_L}]") || f.events[1] != "launch gnome-calculator" {
		t.Errorf("events = %q, want a Shift_L press before the launch", f.events)
	}
}

func TestWithChromeA11y(t *testing.T) {
	for in, want := range map[string]string{
		"google-chrome https://example.com":                                 "google-chrome --force-renderer-accessibility https://example.com",
		"/usr/bin/google-chrome-stable":                                     "/usr/bin/google-chrome-stable --force-renderer-accessibility",
		"chromium --force-renderer-accessibility https://example.com":       "chromium --force-renderer-accessibility https://example.com",
		"google-chrome --user-data-dir=/x --no-first-run https://a.b/c?d=e": "google-chrome --force-renderer-accessibility --user-data-dir=/x --no-first-run https://a.b/c?d=e",
		"gnome-calculator":         "gnome-calculator",
		"code --enable-features=x": "code --enable-features=x",
	} {
		if got := withChromeA11y(in); got != want {
			t.Errorf("withChromeA11y(%q) = %q, want %q", in, got, want)
		}
	}
}
