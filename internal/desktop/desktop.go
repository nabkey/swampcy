// Package swampcy is an MCP server that lets a coding agent see and drive GUI apps
// on a headless sway session: screenshots through grim, the pointer and keys
// through a virtual pointer and keyboard of its own, windows through sway's IPC,
// and widgets through AT-SPI.
package desktop

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Compositor is the part of sway's IPC the tools use.
type Compositor interface {
	Tree() (*Node, error)
	Outputs() ([]Output, error)
	Command(string) error
}

// Accessibility is the part of AT-SPI the tools use.
type Accessibility interface {
	Tree(ctx context.Context, w Window, g Geometry, opts TreeOptions) (string, error)
	Find(ctx context.Context, ws []Window, g Geometry, q Query, opts TreeOptions) (string, int, error)
	Activate(ctx context.Context, ref, action string) (string, error)
	Bounds(ctx context.Context, ref string, tree *Node) (Rect, error)
	SetText(ctx context.Context, ref, text string) error
	Text(ctx context.Context, ref string) (string, error)
	ScrollTo(ctx context.Context, ref string) error
	SetValue(ctx context.Context, ref string, v float64) (string, error)
	Line(ctx context.Context, ref string, tree *Node, g Geometry) (string, error)
}

// Desktop holds what the tools act through.
type Desktop struct {
	Sway     Compositor
	Screen   Screen
	Pointer  Pointer
	Keyboard Keyboard
	A11y     Accessibility
	Launch   func(command string) (int, error)
	MaxEdge  int    // screenshots' long edge, in pixels
	Output   string // the output this server sees and points at; "" for the top-left one
	Log      *slog.Logger
	// Replay, when set, keeps a frame after each acting tool for the replay tool.
	Replay *Replay
	// Exited, when set, says whether a launched command's shell has exited, and how.
	Exited func(pid int) (Exit, bool)
	// Settle, when set, waits until the screen has been quiet for quiet, or most has
	// passed, and reports whether it went quiet. Without it, tools sleep instead.
	Settle func(ctx context.Context, quiet, most time.Duration) bool
	// LockInput, when set, takes a lock shared with the other swampcy processes on
	// the seat, and returns what releases it (nil when it could not be taken).
	LockInput func() func()
	// ClipboardText and SetClipboardText, when set, read and set the seat's clipboard.
	ClipboardText    func() (string, error)
	SetClipboardText func(string) error

	// tools are the handlers by name, for batch.
	tools map[string]func(context.Context, json.RawMessage) (*mcp.CallToolResult, error)

	// input serialises everything that moves the pointer or presses keys: an agent
	// may call tools in parallel, and two clicks must not interleave.
	input sync.Mutex
}

const instructions = `swampcy drives a headless sway desktop: a screen nobody else uses, for GUI apps you start with launch.
- Look with screenshot (and zoom for small text). Coordinates for click, move, drag, scroll and zoom are always pixels of the full screenshot.
- Prefer the accessibility tools when the app supports AT-SPI (GTK, Qt, Firefox, LibreOffice; Electron and Chromium with launch): find_elements or accessibility_tree, then click_element, set_text or get_text. They act by element, not by pixel.
- Start apps with launch so they get the display and accessibility; apps started another way may have no accessibility tree.
- Actions return without a screenshot. Pass screenshot:true on the action whose result you need to see: it waits for the screen to settle and returns the picture in the same call. Do not take one after every step.
- When several steps need no look in between (click a field, type, press Return), send them as one batch.
- After something slow (a page load, a dialog opening), use wait_for rather than repeated screenshots.
- There may be other screens, such as the owner's: windows says which screen each window is on, and move_window moves one between screens (to hand the owner a window, or take one back).`

// Server builds the MCP server.
func (d *Desktop) Server(version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "swampcy", Title: "swampcy", Version: version}, &mcp.ServerOptions{Instructions: instructions})
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}

	add(d, s, &mcp.Tool{Name: "screenshot", Annotations: ro,
		Description: "Capture the whole screen. Its pixels are the coordinate space for click, move, drag, scroll and zoom."},
		d.screenshot)
	add(d, s, &mcp.Tool{Name: "zoom", Annotations: ro,
		Description: "Magnify part of the screen, to read small text or aim at a small target. The rectangle is in screenshot pixels; coordinates for other tools stay in the full screenshot's pixels."},
		d.zoom)
	add(d, s, &mcp.Tool{Name: "click",
		Description: "Click at a point in screenshot pixels. button is left (default), right or middle; count 2 double-clicks, 3 triple-clicks; modifiers (ctrl, shift, ctrl+shift) are held for the click."},
		d.click)
	add(d, s, &mcp.Tool{Name: "move",
		Description: "Move the pointer to a point in screenshot pixels, to hover."},
		d.move)
	add(d, s, &mcp.Tool{Name: "drag",
		Description: "Press a button at one point, move to another and release, in screenshot pixels. via adds points to pass through on the way; hold_ms pauses after pressing, for apps that start a drag only after a moment."},
		d.drag)
	add(d, s, &mcp.Tool{Name: "mouse_down",
		Description: "Move to a point and press a button without releasing it. Follow with move and mouse_up, for drags that drag cannot do. Always release with mouse_up."},
		d.mouseDown)
	add(d, s, &mcp.Tool{Name: "mouse_up",
		Description: "Move to a point (the pointer's place when none is given) and release a button pressed with mouse_down."},
		d.mouseUp)
	add(d, s, &mcp.Tool{Name: "scroll",
		Description: "Scroll at a point in screenshot pixels: direction up, down, left or right, amount in wheel clicks (default 3). modifiers are held meanwhile (ctrl to zoom)."},
		d.scroll)
	add(d, s, &mcp.Tool{Name: "type",
		Description: "Type text into the focused window. Any Unicode works; use key for Enter, Tab and shortcuts."},
		d.typeText)
	add(d, s, &mcp.Tool{Name: "key",
		Description: `Press keys or shortcuts, separated by spaces: "Return", "ctrl+s", "ctrl+shift+t", "alt+F4", "Down Down Return". Modifiers: ctrl, shift, alt, super. Keys: names like Return, Escape, Tab, BackSpace, Delete, Up, Page_Down, Home, F5, or a single character. repeat presses them that many times; hold_ms holds one key or shortcut down that long.`},
		d.key)
	add(d, s, &mcp.Tool{Name: "wait", Annotations: ro,
		Description: "Wait ms milliseconds (at most 30000). With screenshot, it then waits for the screen to settle and returns it."},
		d.wait)
	add(d, s, &mcp.Tool{Name: "wait_for", Annotations: ro,
		Description: "Wait until something is there: a widget whose name and role contain the given text (as find_elements), or with title a window whose title or app contains it. With gone, wait until it is no longer there. Fails after timeout_ms (default 5000)."},
		d.waitFor)
	add(d, s, &mcp.Tool{Name: "batch",
		Description: `Run several tools in order in one call, stopping at the first that fails: [{"tool":"click","args":{"x":400,"y":300}},{"tool":"type","args":{"text":"hello"}},{"tool":"key","args":{"keys":"Return"}}]. Any tool but batch and replay. Use it for steps that need no look in between; screenshot returns the screen after the last.`},
		d.batch)
	add(d, s, &mcp.Tool{Name: "windows", Annotations: ro,
		Description: "List the windows: id, title, app, process id, bounds in screenshot pixels, workspace, and which has focus."},
		d.windows)
	add(d, s, &mcp.Tool{Name: "focus_window",
		Description: "Focus a window by id, raising it to the visible workspace."},
		d.focusWindow)
	add(d, s, &mcp.Tool{Name: "move_window",
		Description: "Move a window to another screen (output), such as the owner's, or bring one back to yours (the default). windows says which screen each is on."},
		d.moveWindow)
	add(d, s, &mcp.Tool{Name: "close_window",
		Description: "Close a window by id, as its close button would. An app with unsaved work may ask first: the window then stays, with a dialog."},
		d.closeWindow)
	add(d, s, &mcp.Tool{Name: "launch",
		Description: "Start a GUI app on the desktop with a shell command (\"gnome-calculator\", \"firefox https://example.com\"). It gets the display and accessibility turned on, and outlives this server. Waits up to wait_ms (default 5000) for a new window and reports it."},
		d.launch)
	add(d, s, &mcp.Tool{Name: "accessibility_tree", Annotations: ro,
		Description: "Outline a window's widgets (default: the focused window): a ref (e12) for each, its role, name, bounds in screenshot pixels, notable states and actions. Pass refs to click_element, set_text and get_text."},
		d.accessibilityTree)
	add(d, s, &mcp.Tool{Name: "find_elements", Annotations: ro,
		Description: "Find widgets whose name and role contain the given text (case-insensitive), in one window or all of them. Returns refs for click_element, set_text and get_text. With all, widgets scrolled out of view are found too; scroll_to brings one into view."},
		d.findElements)
	add(d, s, &mcp.Tool{Name: "click_element",
		Description: "Activate a widget by ref: its named action, or click/press/activate/toggle. With pointer, or when it has no action, click the centre of its bounds instead."},
		d.clickElement)
	add(d, s, &mcp.Tool{Name: "set_text",
		Description: "Replace the text of an editable widget (marked settable) by ref, without typing."},
		d.setText)
	add(d, s, &mcp.Tool{Name: "get_text", Annotations: ro,
		Description: "Read the text of a widget by ref: a text field, a document, a terminal, a label."},
		d.getText)
	add(d, s, &mcp.Tool{Name: "scroll_to",
		Description: "Scroll a widget into view by ref (one find_elements found with all), and report where it now is."},
		d.scrollTo)
	add(d, s, &mcp.Tool{Name: "set_value",
		Description: "Set the value of a slider, spin button or scroll bar by ref (one the outline shows a value= for)."},
		d.setValue)
	if d.ClipboardText != nil {
		add(d, s, &mcp.Tool{Name: "clipboard_get", Annotations: ro,
			Description: "Read the clipboard's text: what an app copied with ctrl+c."},
			d.clipboardGet)
	}
	if d.SetClipboardText != nil {
		add(d, s, &mcp.Tool{Name: "clipboard_set",
			Description: "Put text on the clipboard, for an app to paste with ctrl+v. Long or multi-line text pastes faster and more exactly than type types it."},
			d.clipboardSet)
	}
	if d.Replay != nil {
		add(d, s, &mcp.Tool{Name: "replay",
			Description: "Save a short animated clip (WebP, or GIF) of what you did on the screen: a frame after each click, key, type, scroll, drag, launch, click_element and set_text in the last 30 minutes. Returns the file's path. Call it once at the end of a task that acted on the screen; it starts the next clip afresh. The clip may show signed-in pages: never send or upload it anywhere."},
			d.replay)
	}
	return s
}

// After is embedded in the arguments of tools that can return the screen as they
// leave it.
type After struct {
	Screenshot bool `json:"screenshot,omitempty" jsonschema:"also return a screenshot, taken once the screen has settled after this; saves a separate screenshot call"`
}

func (a After) shotAfter() bool { return a.Screenshot }

// add registers a tool whose handler returns a result, logging each call's
// duration and error but never its arguments, which may hold typed secrets. The
// handler is also kept by name, for batch.
func add[In any](d *Desktop, s *mcp.Server, t *mcp.Tool, h func(context.Context, In) (*mcp.CallToolResult, error)) {
	run := func(ctx context.Context, in In) (*mcp.CallToolResult, error) {
		start := time.Now()
		var g Geometry
		recording := false
		if d.Replay != nil && actingTools[t.Name] {
			var err error
			if g, err = d.geometry(); err == nil {
				recording = true
				d.Replay.Before(g)
			}
		}
		res, err := h(ctx, in)
		if recording && err == nil {
			d.Replay.After(g)
		}
		if a, ok := any(in).(interface{ shotAfter() bool }); ok && a.shotAfter() && err == nil && res != nil {
			err = d.appendShot(ctx, res)
		}
		if d.Log != nil {
			d.Log.Info("tool", "name", t.Name, "ms", time.Since(start).Milliseconds(), "err", err)
		}
		return res, err
	}
	if t.Name != "batch" && t.Name != "replay" {
		var resolved *jsonschema.Resolved
		if schema, err := jsonschema.For[In](nil); err == nil {
			resolved, _ = schema.Resolve(nil)
		}
		if d.tools == nil {
			d.tools = map[string]func(context.Context, json.RawMessage) (*mcp.CallToolResult, error){}
		}
		d.tools[t.Name] = func(ctx context.Context, raw json.RawMessage) (*mcp.CallToolResult, error) {
			if s := bytes.TrimSpace(raw); len(s) == 0 || string(s) == "null" {
				raw = json.RawMessage("{}")
			}
			if resolved != nil {
				var v map[string]any
				if err := json.Unmarshal(raw, &v); err != nil {
					return nil, fmt.Errorf("args: %w", err)
				}
				if err := resolved.Validate(v); err != nil {
					return nil, fmt.Errorf("args: %w", err)
				}
			}
			var in In
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, fmt.Errorf("args: %w", err)
			}
			return run(ctx, in)
		}
	}
	mcp.AddTool(s, t, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		res, err := run(ctx, in)
		return res, nil, err
	})
}

// The screen is taken to have settled once nothing on it has changed for
// settleQuiet, and is shown as it is after settleMost whatever it is doing. An
// app needs settleFirst before it has drawn anything of its answer.
const (
	settleFirst = 120 * time.Millisecond
	settleQuiet = 200 * time.Millisecond
	settleMost  = 2500 * time.Millisecond
)

// settle waits for the screen to stop changing after an action.
func (d *Desktop) settle(ctx context.Context) (settled bool) {
	if d.Settle == nil {
		sleep(ctx, settleFirst+settleQuiet)
		return true
	}
	sleep(ctx, settleFirst)
	return d.Settle(ctx, settleQuiet, settleMost)
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// appendShot adds the settled screen to a tool's result.
func (d *Desktop) appendShot(ctx context.Context, res *mcp.CallToolResult) error {
	settled := d.settle(ctx)
	shot, err := d.screenshot(ctx, noArgs{})
	if err != nil {
		return err
	}
	res.Content = append(res.Content, shot.Content...)
	if !settled {
		res.Content = append(res.Content, &mcp.TextContent{Text: "The screen was still changing when this was taken."})
	}
	return nil
}

type heldKey struct{}

// lock serialises everything that moves the pointer, presses keys or changes
// focus, within this process and (with LockInput) across the swampcy processes on
// the seat. It returns what unlocks. Inside a batch, which holds the lock for all
// its steps, it does nothing.
func (d *Desktop) lock(ctx context.Context) func() {
	if ctx.Value(heldKey{}) != nil {
		return func() {}
	}
	d.input.Lock()
	var release func()
	if d.LockInput != nil {
		release = d.LockInput()
	}
	return func() {
		if release != nil {
			release()
		}
		d.input.Unlock()
	}
}

func text(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}}}
}

func (d *Desktop) geometry() (Geometry, error) {
	outs, err := d.Sway.Outputs()
	if err != nil {
		return Geometry{}, err
	}
	out, err := PickOutput(outs, d.Output)
	if err != nil {
		return Geometry{}, err
	}
	g := NewGeometry(out.Rect, d.MaxEdge)
	g.Output = out.Name
	g.Native = max(out.Scale, 1)
	return g, nil
}

func describe(w *Window, g Geometry) string {
	r := g.ToShot(w.Rect)
	s := fmt.Sprintf("%d %q app=%s pid=%d (%d,%d %dx%d) workspace=%s", w.ID, w.Name, w.App(), w.PID, r.X, r.Y, r.Width, r.Height, w.Workspace)
	if w.Floating {
		s += " floating"
	}
	if w.Focused {
		s += " focused"
	}
	if w.Output != "" && g.Output != "" && w.Output != g.Output {
		s += " on " + w.Output + " (another screen: not in screenshots, and not clickable)"
	}
	return s
}

type noArgs struct{}

func (d *Desktop) screenshot(ctx context.Context, _ noArgs) (*mcp.CallToolResult, error) {
	g, err := d.geometry()
	if err != nil {
		return nil, err
	}
	r := g.Layout // this output, not the whole layout
	img, mime, err := d.Screen.Capture(ctx, &r, g.Scale)
	if err != nil {
		return nil, err
	}
	w, h := g.Size()
	summary := fmt.Sprintf("Screen %dx%d.", w, h)
	if g.Scale < 1 {
		summary += fmt.Sprintf(" (Scaled from %dx%d; swampcy converts coordinates.)", g.Layout.Width, g.Layout.Height)
	}
	if tree, err := d.Sway.Tree(); err == nil {
		if f := tree.FocusedWindow(); f != nil {
			summary += " Focused: " + describe(f, g)
		} else {
			summary += " No window has focus."
		}
	}
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.ImageContent{Data: img, MIMEType: mime},
		&mcp.TextContent{Text: summary},
	}}, nil
}

type zoomArgs struct {
	X      int `json:"x" jsonschema:"left edge, in screenshot pixels"`
	Y      int `json:"y" jsonschema:"top edge, in screenshot pixels"`
	Width  int `json:"width" jsonschema:"width, in screenshot pixels"`
	Height int `json:"height" jsonschema:"height, in screenshot pixels"`
}

func (d *Desktop) zoom(ctx context.Context, in zoomArgs) (*mcp.CallToolResult, error) {
	g, err := d.geometry()
	if err != nil {
		return nil, err
	}
	r, err := g.RectToLayout(Rect{X: in.X, Y: in.Y, Width: in.Width, Height: in.Height})
	if err != nil {
		return nil, err
	}
	// Magnify up to zoomMost, or to the output's own scale when that is finer: on
	// an output of scale 2 the extra pixels are real, and otherwise they are
	// interpolated, which still makes small text larger for the model.
	scale := max(zoomMost*g.Scale, g.Native)
	if long := max(r.Width, r.Height); d.MaxEdge > 0 && float64(long)*scale > float64(d.MaxEdge) {
		scale = float64(d.MaxEdge) / float64(long)
	}
	img, mime, err := d.Screen.Capture(ctx, &r, scale)
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.ImageContent{Data: img, MIMEType: mime},
		&mcp.TextContent{Text: fmt.Sprintf("Region (%d,%d %dx%d) at %.1fx the screenshot's scale%s. Coordinates for other tools are still full-screenshot pixels.", in.X, in.Y, in.Width, in.Height, scale/g.Scale, zoomNote(scale, g.Native))},
	}}, nil
}

// zoomMost is how far zoom magnifies a small region.
const zoomMost = 3.0

func zoomNote(scale, native float64) string {
	if scale > max(native, 1)+0.01 {
		return " (enlarged: the screen has no finer detail than " + strconv.FormatFloat(max(native, 1), 'g', 3, 64) + "x)"
	}
	return ""
}

type clickArgs struct {
	X      int    `json:"x" jsonschema:"in screenshot pixels"`
	Y      int    `json:"y" jsonschema:"in screenshot pixels"`
	Button string `json:"button,omitempty" jsonschema:"left (default), right or middle"`
	Count  int    `json:"count,omitempty" jsonschema:"1 (default), 2 for a double-click, 3 for a triple-click"`
	Mods   string `json:"modifiers,omitempty" jsonschema:"keys held during the click: ctrl, shift, alt, super, or several joined with +"`
	After
}

func (d *Desktop) click(ctx context.Context, in clickArgs) (*mcp.CallToolResult, error) {
	g, err := d.geometry()
	if err != nil {
		return nil, err
	}
	x, y, err := g.ToLayout(in.X, in.Y)
	if err != nil {
		return nil, err
	}
	button := cmp.Or(in.Button, "left")
	count := cmp.Or(in.Count, 1)
	if count < 1 || count > 3 {
		return nil, fmt.Errorf("count must be 1, 2 or 3")
	}
	mods, err := ParseMods(in.Mods)
	if err != nil {
		return nil, err
	}
	if err := d.clickAt(ctx, g, x, y, button, count, mods); err != nil {
		return nil, err
	}
	with := ""
	if len(mods) > 0 {
		with = " with " + in.Mods
	}
	return text("Clicked %s ×%d at (%d,%d)%s.", button, count, in.X, in.Y, with), nil
}

// withMods runs fn with modifier keys held, letting them go whatever happens.
func (d *Desktop) withMods(ctx context.Context, mods []string, fn func() error) error {
	if len(mods) == 0 {
		return fn()
	}
	if err := d.Keyboard.HoldMods(ctx, mods, true); err != nil {
		_ = d.Keyboard.HoldMods(ctx, mods, false)
		return err
	}
	time.Sleep(settleAfterMove)
	err := fn()
	if e := d.Keyboard.HoldMods(ctx, mods, false); err == nil {
		err = e
	}
	return err
}

// A press and release with no time between them is lost on GTK 4: its click
// gesture sees nothing happen. These pauses are well inside a double-click.
const (
	settleAfterMove = 20 * time.Millisecond
	holdButton      = 40 * time.Millisecond
	betweenClicks   = 60 * time.Millisecond
)

// arrive moves the pointer to (x, y) by way of the pixel beside it, so the window
// there always sees the pointer move onto it. A window that opens under a pointer
// that has not moved gets no motion, and an XWayland app ignores the first press
// that follows; moving to where the pointer already is sends nothing either. The
// pixel beside it is on the same output, so no other screen sees the pointer.
func (d *Desktop) arrive(g Geometry, x, y int) error {
	nx := x - 1
	if nx < g.Layout.X {
		nx = x + 1
	}
	if err := d.Pointer.MoveTo(nx, y); err != nil {
		return err
	}
	return d.Pointer.MoveTo(x, y)
}

func (d *Desktop) clickAt(ctx context.Context, g Geometry, x, y int, button string, count int, mods []string) error {
	defer d.lock(ctx)()
	if err := d.arrive(g, x, y); err != nil {
		return err
	}
	time.Sleep(settleAfterMove)
	return d.withMods(ctx, mods, func() error {
		for i := range count {
			if i > 0 {
				time.Sleep(betweenClicks)
			}
			if err := d.Pointer.Press(button); err != nil {
				return err
			}
			time.Sleep(holdButton)
			if err := d.Pointer.Release(button); err != nil {
				return err
			}
		}
		return nil
	})
}

type pointArgs struct {
	X int `json:"x" jsonschema:"in screenshot pixels"`
	Y int `json:"y" jsonschema:"in screenshot pixels"`
	After
}

type viaPoint struct {
	X int `json:"x" jsonschema:"in screenshot pixels"`
	Y int `json:"y" jsonschema:"in screenshot pixels"`
}

func (d *Desktop) move(ctx context.Context, in pointArgs) (*mcp.CallToolResult, error) {
	g, err := d.geometry()
	if err != nil {
		return nil, err
	}
	x, y, err := g.ToLayout(in.X, in.Y)
	if err != nil {
		return nil, err
	}
	defer d.lock(ctx)()
	if err := d.Pointer.MoveTo(x, y); err != nil {
		return nil, err
	}
	return text("Pointer at (%d,%d).", in.X, in.Y), nil
}

type dragArgs struct {
	FromX  int        `json:"from_x" jsonschema:"in screenshot pixels"`
	FromY  int        `json:"from_y" jsonschema:"in screenshot pixels"`
	ToX    int        `json:"to_x" jsonschema:"in screenshot pixels"`
	ToY    int        `json:"to_y" jsonschema:"in screenshot pixels"`
	Button string     `json:"button,omitempty" jsonschema:"left (default), right or middle"`
	Via    []viaPoint `json:"via,omitempty" jsonschema:"points to pass through between the start and the end, in order"`
	HoldMS int        `json:"hold_ms,omitempty" jsonschema:"pause after pressing and before moving, at most 5000"`
	After
}

// dragSteps is how many intermediate points a drag passes through; apps that
// track motion (sliders, text selection) need more than the two ends.
const dragSteps = 12

func (d *Desktop) drag(ctx context.Context, in dragArgs) (*mcp.CallToolResult, error) {
	g, err := d.geometry()
	if err != nil {
		return nil, err
	}
	x1, y1, err := g.ToLayout(in.FromX, in.FromY)
	if err != nil {
		return nil, err
	}
	if in.HoldMS < 0 || in.HoldMS > 5000 {
		return nil, errors.New("hold_ms must be between 0 and 5000")
	}
	path := [][2]int{{x1, y1}}
	for _, p := range append(slices.Clone(in.Via), viaPoint{in.ToX, in.ToY}) {
		x, y, err := g.ToLayout(p.X, p.Y)
		if err != nil {
			return nil, err
		}
		path = append(path, [2]int{x, y})
	}
	button := cmp.Or(in.Button, "left")
	defer d.lock(ctx)()
	if err := d.arrive(g, x1, y1); err != nil {
		return nil, err
	}
	time.Sleep(settleAfterMove)
	if err := d.Pointer.Press(button); err != nil {
		return nil, err
	}
	time.Sleep(time.Duration(in.HoldMS) * time.Millisecond)
	for seg := 1; seg < len(path); seg++ {
		from, to := path[seg-1], path[seg]
		for i := 1; i <= dragSteps; i++ {
			x := from[0] + int(math.Round(float64(to[0]-from[0])*float64(i)/dragSteps))
			y := from[1] + int(math.Round(float64(to[1]-from[1])*float64(i)/dragSteps))
			if err := d.Pointer.MoveTo(x, y); err != nil {
				_ = d.Pointer.Release(button)
				return nil, err
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if err := d.Pointer.Release(button); err != nil {
		return nil, err
	}
	return text("Dragged from (%d,%d) to (%d,%d).", in.FromX, in.FromY, in.ToX, in.ToY), nil
}

type buttonAtArgs struct {
	X      *int   `json:"x,omitempty" jsonschema:"in screenshot pixels"`
	Y      *int   `json:"y,omitempty" jsonschema:"in screenshot pixels"`
	Button string `json:"button,omitempty" jsonschema:"left (default), right or middle"`
	After
}

func (d *Desktop) mouseDown(ctx context.Context, in buttonAtArgs) (*mcp.CallToolResult, error) {
	return d.button(ctx, in, true)
}

func (d *Desktop) mouseUp(ctx context.Context, in buttonAtArgs) (*mcp.CallToolResult, error) {
	return d.button(ctx, in, false)
}

func (d *Desktop) button(ctx context.Context, in buttonAtArgs, down bool) (*mcp.CallToolResult, error) {
	if (in.X == nil) != (in.Y == nil) {
		return nil, errors.New("give both x and y, or neither")
	}
	if down && in.X == nil {
		return nil, errors.New("mouse_down needs x and y")
	}
	button := cmp.Or(in.Button, "left")
	defer d.lock(ctx)()
	where := ""
	if in.X != nil {
		g, err := d.geometry()
		if err != nil {
			return nil, err
		}
		x, y, err := g.ToLayout(*in.X, *in.Y)
		if err != nil {
			return nil, err
		}
		// A press arrives as a click does; a release moves straight there, since the
		// way to it is part of the drag.
		move := func() error { return d.Pointer.MoveTo(x, y) }
		if down {
			move = func() error { return d.arrive(g, x, y) }
		}
		if err := move(); err != nil {
			return nil, err
		}
		time.Sleep(settleAfterMove)
		where = fmt.Sprintf(" at (%d,%d)", *in.X, *in.Y)
	}
	if down {
		if err := d.Pointer.Press(button); err != nil {
			return nil, err
		}
		return text("Pressed %s%s; it is held until mouse_up.", button, where), nil
	}
	if err := d.Pointer.Release(button); err != nil {
		return nil, err
	}
	return text("Released %s%s.", button, where), nil
}

type scrollArgs struct {
	X         int    `json:"x" jsonschema:"in screenshot pixels"`
	Y         int    `json:"y" jsonschema:"in screenshot pixels"`
	Direction string `json:"direction" jsonschema:"up, down, left or right"`
	Amount    int    `json:"amount,omitempty" jsonschema:"wheel clicks, default 3"`
	Mods      string `json:"modifiers,omitempty" jsonschema:"keys held while scrolling, e.g. ctrl"`
	After
}

func (d *Desktop) scroll(ctx context.Context, in scrollArgs) (*mcp.CallToolResult, error) {
	g, err := d.geometry()
	if err != nil {
		return nil, err
	}
	x, y, err := g.ToLayout(in.X, in.Y)
	if err != nil {
		return nil, err
	}
	amount := cmp.Or(in.Amount, 3)
	if amount < 1 || amount > 50 {
		return nil, fmt.Errorf("amount must be between 1 and 50")
	}
	mods, err := ParseMods(in.Mods)
	if err != nil {
		return nil, err
	}
	defer d.lock(ctx)()
	if err := d.arrive(g, x, y); err != nil {
		return nil, err
	}
	if err := d.withMods(ctx, mods, func() error { return d.Pointer.Scroll(in.Direction, amount) }); err != nil {
		return nil, err
	}
	return text("Scrolled %s %d at (%d,%d).", in.Direction, amount, in.X, in.Y), nil
}

type typeArgs struct {
	Text string `json:"text" jsonschema:"the text to type"`
	After
}

func (d *Desktop) typeText(ctx context.Context, in typeArgs) (*mcp.CallToolResult, error) {
	defer d.lock(ctx)()
	if err := d.Keyboard.Type(ctx, in.Text); err != nil {
		return nil, err
	}
	return text("Typed %d characters.", len([]rune(in.Text))), nil
}

type keyArgs struct {
	Keys   string `json:"keys" jsonschema:"one or more keys or shortcuts separated by spaces, e.g. ctrl+s or Down Return"`
	Repeat int    `json:"repeat,omitempty" jsonschema:"press them this many times, 1 (default) to 100"`
	HoldMS int    `json:"hold_ms,omitempty" jsonschema:"hold the one key or shortcut given down this long, at most 10000"`
	After
}

func (d *Desktop) key(ctx context.Context, in keyArgs) (*mcp.CallToolResult, error) {
	combos, err := ParseKeys(in.Keys)
	if err != nil {
		return nil, err
	}
	repeat := cmp.Or(in.Repeat, 1)
	if repeat < 1 || repeat > 100 {
		return nil, errors.New("repeat must be between 1 and 100")
	}
	if in.HoldMS < 0 || in.HoldMS > 10000 {
		return nil, errors.New("hold_ms must be between 0 and 10000")
	}
	defer d.lock(ctx)()
	if in.HoldMS > 0 {
		if len(combos) != 1 || repeat != 1 {
			return nil, errors.New("hold_ms holds one key or shortcut, once")
		}
		if err := d.Keyboard.HoldKey(ctx, combos[0], time.Duration(in.HoldMS)*time.Millisecond); err != nil {
			return nil, err
		}
		return text("Held %s for %d ms.", in.Keys, in.HoldMS), nil
	}
	all := make([]Combo, 0, len(combos)*repeat)
	for range repeat {
		all = append(all, combos...)
	}
	if err := d.Keyboard.Keys(ctx, all); err != nil {
		return nil, err
	}
	if repeat > 1 {
		return text("Pressed %s ×%d.", in.Keys, repeat), nil
	}
	return text("Pressed %s.", in.Keys), nil
}

func (d *Desktop) windows(ctx context.Context, _ noArgs) (*mcp.CallToolResult, error) {
	g, err := d.geometry()
	if err != nil {
		return nil, err
	}
	tree, err := d.Sway.Tree()
	if err != nil {
		return nil, err
	}
	ws := tree.Windows()
	if len(ws) == 0 {
		return text("No windows. Start an app with launch."), nil
	}
	var b strings.Builder
	for _, w := range ws {
		b.WriteString(describe(&w, g))
		b.WriteByte('\n')
	}
	return text("%s", b.String()), nil
}

type windowArgs struct {
	Window int64 `json:"window" jsonschema:"window id, from windows"`
	After
}

func (d *Desktop) focusWindow(ctx context.Context, in windowArgs) (*mcp.CallToolResult, error) {
	tree, err := d.Sway.Tree()
	if err != nil {
		return nil, err
	}
	if tree.Find(in.Window) == nil {
		return nil, fmt.Errorf("no window %d", in.Window)
	}
	defer d.lock(ctx)()
	if err := d.Sway.Command(fmt.Sprintf("[con_id=%d] focus", in.Window)); err != nil {
		return nil, err
	}
	return text("Focused window %d.", in.Window), nil
}

func (d *Desktop) closeWindow(ctx context.Context, in windowArgs) (*mcp.CallToolResult, error) {
	tree, err := d.Sway.Tree()
	if err != nil {
		return nil, err
	}
	if tree.Find(in.Window) == nil {
		return nil, fmt.Errorf("no window %d", in.Window)
	}
	unlock := d.lock(ctx)
	err = d.Sway.Command(fmt.Sprintf("[con_id=%d] kill", in.Window))
	unlock()
	if err != nil {
		return nil, err
	}
	for deadline := time.Now().Add(1500 * time.Millisecond); time.Now().Before(deadline); {
		sleep(ctx, 100*time.Millisecond)
		if tree, err := d.Sway.Tree(); err == nil && tree.Find(in.Window) == nil {
			return text("Closed window %d.", in.Window), nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return text("Asked window %d to close, but it is still open: it may be asking whether to save. Look at the screen.", in.Window), nil
}

type moveArgs struct {
	Window int64  `json:"window" jsonschema:"window id, from windows"`
	Output string `json:"output,omitempty" jsonschema:"the output to move it to, from windows; default this screen's"`
}

// outputName is what an output's name must look like to go into a sway command.
var outputName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

func (d *Desktop) moveWindow(ctx context.Context, in moveArgs) (*mcp.CallToolResult, error) {
	g, err := d.geometry()
	if err != nil {
		return nil, err
	}
	outs, err := d.Sway.Outputs()
	if err != nil {
		return nil, err
	}
	to := cmp.Or(in.Output, g.Output)
	if _, err := PickOutput(outs, to); err != nil || !outputName.MatchString(to) {
		return nil, cmp.Or(err, fmt.Errorf("bad output name %q", to))
	}
	tree, err := d.Sway.Tree()
	if err != nil {
		return nil, err
	}
	w := tree.Find(in.Window)
	if w == nil {
		return nil, fmt.Errorf("no window %d", in.Window)
	}
	if w.Output == to {
		return text("Window %d is already on %s.", w.ID, to), nil
	}
	defer d.lock(ctx)()
	cmd := fmt.Sprintf("[con_id=%d] move container to output %s", w.ID, to)
	if to == g.Output {
		cmd += fmt.Sprintf("; [con_id=%d] focus", w.ID)
	} else if f := tree.FocusedWindow(); f != nil && f.ID != w.ID && f.Output == g.Output {
		// Sway's focus can follow the window away; keep this screen's where it was.
		cmd += fmt.Sprintf("; [con_id=%d] focus", f.ID)
	} else {
		// No other window here to keep it on: keep it on this screen.
		cmd += "; focus output " + g.Output
	}
	if err := d.Sway.Command(cmd); err != nil {
		return nil, err
	}
	return text("Moved window %d to %s.", w.ID, to), nil
}

// launchSettle is how long launch waits after a new window appears.
const launchSettle = 500 * time.Millisecond

type launchArgs struct {
	Command string `json:"command" jsonschema:"shell command that starts the app"`
	WaitMS  int    `json:"wait_ms,omitempty" jsonschema:"how long to wait for a new window, default 5000"`
	After
}

func (d *Desktop) launch(ctx context.Context, in launchArgs) (*mcp.CallToolResult, error) {
	if strings.TrimSpace(in.Command) == "" {
		return nil, errors.New("command is empty")
	}
	before := map[int64]bool{}
	if tree, err := d.Sway.Tree(); err == nil {
		for _, w := range tree.Windows() {
			before[w.ID] = true
		}
	}
	// Chrome segfaults at start-up (in xkb_state_update_mask) when the seat's
	// last-used keyboard is gone, which is the case after any other swampcy
	// session has exited: a no-op key makes this server's keyboard the seat's.
	unlock := d.lock(ctx)
	if err := d.Keyboard.Keys(ctx, activateKeyboard); err != nil && d.Log != nil {
		d.Log.Warn("launch: activating the keyboard", "err", err)
	}
	unlock()
	pid, err := d.Launch(withChromeA11y(in.Command))
	if err != nil {
		return nil, err
	}
	wait := time.Duration(cmp.Or(in.WaitMS, 5000)) * time.Millisecond
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
		tree, err := d.Sway.Tree()
		if err != nil {
			continue
		}
		// A command that failed at once (not found, a syntax error) is reported as
		// that, not waited out. One that exited cleanly has usually handed off to a
		// running instance, and its window may still come.
		if d.Exited != nil && !slices.ContainsFunc(tree.Windows(), func(w Window) bool { return !before[w.ID] }) {
			if e, ok := d.Exited(pid); ok && e.Err != nil {
				return nil, fmt.Errorf("the command failed (%v) without opening a window:\n%s", e.Err, cmp.Or(e.Output, "(it wrote nothing)"))
			}
		}
		var fresh []Window
		for _, w := range tree.Windows() {
			if !before[w.ID] {
				fresh = append(fresh, w)
			}
		}
		if len(fresh) > 0 {
			// The window is mapped before its first frame is drawn; a screenshot
			// taken at once shows it black.
			time.Sleep(launchSettle)
			g, err := d.geometry()
			if err != nil {
				return nil, err
			}
			// Prefer the window of the process started, though apps that hand off
			// to a running instance or a child show up under another pid.
			slices.SortStableFunc(fresh, func(a, b Window) int {
				return boolInt(b.PID == pid) - boolInt(a.PID == pid)
			})
			w := &fresh[0]
			// A new window opens where sway's focus is, which may be another screen.
			if g.Output != "" && w.Output != "" && w.Output != g.Output {
				if err := d.Sway.Command(fmt.Sprintf("[con_id=%d] move container to output %s; [con_id=%d] focus", w.ID, g.Output, w.ID)); err != nil {
					return nil, fmt.Errorf("window %d opened on %s and could not be moved here: %w", w.ID, w.Output, err)
				}
				if tree, err := d.Sway.Tree(); err == nil {
					if moved := tree.Find(w.ID); moved != nil {
						w = moved
					}
				}
			}
			return text("Started pid %d. New window: %s", pid, describe(w, g)), nil
		}
	}
	if d.Exited != nil {
		if e, ok := d.Exited(pid); ok && e.Err == nil {
			return text("Started pid %d, which exited cleanly; no new window within %s. It may have handed off to an app already running: see windows.", pid, wait), nil
		}
	}
	return text("Started pid %d; no new window within %s (output is in swampcy's app log).", pid, wait), nil
}

// activateKeyboard is pressed before launch: a modifier alone, which no app acts on.
var activateKeyboard = []Combo{{Key: "Shift_L"}}

// chromeBrowsers are the commands that start Chrome or Chromium.
var chromeBrowsers = []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome"}

// withChromeA11y adds --force-renderer-accessibility to a command that starts
// Chrome or Chromium. Without it Chrome 154 shows AT-SPI only its frame, even
// with ACCESSIBILITY_ENABLED=1, and the element tools see no page.
func withChromeA11y(command string) string {
	trimmed := strings.TrimLeft(command, " \t")
	first, rest, _ := strings.Cut(trimmed, " ")
	if !slices.Contains(chromeBrowsers, path.Base(first)) || strings.Contains(command, "--force-renderer-accessibility") {
		return command
	}
	return strings.TrimRight(first+" --force-renderer-accessibility "+rest, " ")
}

type treeArgs struct {
	Window   int64 `json:"window,omitempty" jsonschema:"window id, default the focused window"`
	MaxDepth int   `json:"max_depth,omitempty" jsonschema:"default 25"`
	MaxNodes int   `json:"max_nodes,omitempty" jsonschema:"default 400"`
	All      bool  `json:"all,omitempty" jsonschema:"include widgets that are not showing"`
}

func (d *Desktop) target(id int64) (*Node, *Window, error) {
	tree, err := d.Sway.Tree()
	if err != nil {
		return nil, nil, err
	}
	var w *Window
	if id == 0 {
		w = tree.FocusedWindow()
		if w == nil {
			return nil, nil, errors.New("no window has focus; pass a window id")
		}
	} else if w = tree.Find(id); w == nil {
		return nil, nil, fmt.Errorf("no window %d", id)
	}
	return tree, w, nil
}

func (d *Desktop) accessibilityTree(ctx context.Context, in treeArgs) (*mcp.CallToolResult, error) {
	if d.A11y == nil {
		return nil, errNoA11y
	}
	g, err := d.geometry()
	if err != nil {
		return nil, err
	}
	_, w, err := d.target(in.Window)
	if err != nil {
		return nil, err
	}
	out, err := d.A11y.Tree(ctx, *w, g, TreeOptions{MaxDepth: cmp.Or(in.MaxDepth, 25), MaxNodes: cmp.Or(in.MaxNodes, 400), All: in.All})
	if err != nil {
		return nil, err
	}
	return text("%s", out), nil
}

type findArgs struct {
	Name   string `json:"name,omitempty" jsonschema:"text the widget's name contains"`
	Role   string `json:"role,omitempty" jsonschema:"text the widget's role contains, e.g. button, entry, menu item, check box"`
	Window int64  `json:"window,omitempty" jsonschema:"window id, default all windows"`
	Limit  int    `json:"limit,omitempty" jsonschema:"most matches to return, default 30"`
	All    bool   `json:"all,omitempty" jsonschema:"also find widgets that are not showing, such as those scrolled out of view"`
}

func (d *Desktop) findElements(ctx context.Context, in findArgs) (*mcp.CallToolResult, error) {
	if d.A11y == nil {
		return nil, errNoA11y
	}
	out, _, err := d.find(ctx, in)
	if err != nil {
		return nil, err
	}
	return text("%s", out), nil
}

// findNodes bounds a search: a long web page has thousands of elements.
const (
	findNodes    = 3000
	findNodesAll = 12000
)

func (d *Desktop) find(ctx context.Context, in findArgs) (string, int, error) {
	if in.Name == "" && in.Role == "" {
		return "", 0, errors.New("give a name, a role or both")
	}
	g, err := d.geometry()
	if err != nil {
		return "", 0, err
	}
	var ws []Window
	if in.Window != 0 {
		_, w, err := d.target(in.Window)
		if err != nil {
			return "", 0, err
		}
		ws = []Window{*w}
	} else {
		tree, err := d.Sway.Tree()
		if err != nil {
			return "", 0, err
		}
		ws = tree.Windows()
	}
	opts := TreeOptions{MaxDepth: 40, MaxNodes: findNodes, All: in.All}
	if in.All {
		opts.MaxNodes = findNodesAll
	}
	return d.A11y.Find(ctx, ws, g, Query{Name: in.Name, Role: in.Role, Limit: cmp.Or(in.Limit, 30)}, opts)
}

type elementArgs struct {
	Ref     string `json:"ref" jsonschema:"element ref, e.g. e12, from accessibility_tree or find_elements"`
	Action  string `json:"action,omitempty" jsonschema:"the action to perform, from the element's actions"`
	Pointer bool   `json:"pointer,omitempty" jsonschema:"click the centre of the element with the pointer instead of performing an action"`
	After
}

func (d *Desktop) clickElement(ctx context.Context, in elementArgs) (*mcp.CallToolResult, error) {
	if d.A11y == nil {
		return nil, errNoA11y
	}
	if !in.Pointer {
		unlock := d.lock(ctx)
		action, err := d.A11y.Activate(ctx, in.Ref, in.Action)
		unlock()
		if err == nil {
			return text("Performed %s on %s.", action, in.Ref), nil
		}
		if !errors.Is(err, errNoAction) {
			return nil, err
		}
	}
	tree, err := d.Sway.Tree()
	if err != nil {
		return nil, err
	}
	r, err := d.A11y.Bounds(ctx, in.Ref, tree)
	if err != nil {
		return nil, err
	}
	g, err := d.geometry()
	if err != nil {
		return nil, err
	}
	x, y := r.X+r.Width/2, r.Y+r.Height/2
	if err := d.clickAt(ctx, g, x, y, "left", 1, nil); err != nil {
		return nil, err
	}
	s := g.ToShot(Rect{X: x, Y: y})
	return text("Clicked the centre of %s at (%d,%d).", in.Ref, s.X, s.Y), nil
}

type setTextArgs struct {
	Ref  string `json:"ref" jsonschema:"element ref of an editable widget"`
	Text string `json:"text" jsonschema:"the widget's new text"`
	After
}

func (d *Desktop) setText(ctx context.Context, in setTextArgs) (*mcp.CallToolResult, error) {
	if d.A11y == nil {
		return nil, errNoA11y
	}
	defer d.lock(ctx)()
	if err := d.A11y.SetText(ctx, in.Ref, in.Text); err != nil {
		return nil, err
	}
	return text("Set the text of %s.", in.Ref), nil
}

type refArgs struct {
	Ref string `json:"ref" jsonschema:"element ref"`
}

type refAfterArgs struct {
	Ref string `json:"ref" jsonschema:"element ref"`
	After
}

func (d *Desktop) scrollTo(ctx context.Context, in refAfterArgs) (*mcp.CallToolResult, error) {
	if d.A11y == nil {
		return nil, errNoA11y
	}
	unlock := d.lock(ctx)
	err := d.A11y.ScrollTo(ctx, in.Ref)
	unlock()
	if err != nil {
		return nil, err
	}
	sleep(ctx, 150*time.Millisecond) // the app scrolls a moment after it says it will
	g, err := d.geometry()
	if err != nil {
		return nil, err
	}
	tree, err := d.Sway.Tree()
	if err != nil {
		return nil, err
	}
	line, err := d.A11y.Line(ctx, in.Ref, tree, g)
	if err != nil {
		return text("Scrolled to %s.", in.Ref), nil
	}
	return text("Scrolled to it: %s", line), nil
}

type valueArgs struct {
	Ref   string  `json:"ref" jsonschema:"element ref of a widget with a value"`
	Value float64 `json:"value" jsonschema:"the new value, within the range the outline shows"`
	After
}

func (d *Desktop) setValue(ctx context.Context, in valueArgs) (*mcp.CallToolResult, error) {
	if d.A11y == nil {
		return nil, errNoA11y
	}
	defer d.lock(ctx)()
	now, err := d.A11y.SetValue(ctx, in.Ref, in.Value)
	if err != nil {
		return nil, err
	}
	return text("%s is now %s.", in.Ref, cmp.Or(now, "set")), nil
}

func (d *Desktop) clipboardGet(ctx context.Context, _ noArgs) (*mcp.CallToolResult, error) {
	s, err := d.ClipboardText()
	if err != nil {
		return nil, err
	}
	if s == "" {
		return text("The clipboard holds no text."), nil
	}
	if r := []rune(s); len(r) > maxText {
		s = string(r[:maxText]) + fmt.Sprintf("\n… %d more characters", len(r)-maxText)
	}
	return text("%s", s), nil
}

type clipboardArgs struct {
	Text string `json:"text" jsonschema:"the text to put on the clipboard"`
}

func (d *Desktop) clipboardSet(ctx context.Context, in clipboardArgs) (*mcp.CallToolResult, error) {
	defer d.lock(ctx)()
	if err := d.SetClipboardText(in.Text); err != nil {
		return nil, err
	}
	return text("The clipboard holds %d characters; paste with ctrl+v.", len([]rune(in.Text))), nil
}

type waitArgs struct {
	MS int `json:"ms,omitempty" jsonschema:"milliseconds to wait, 0 to 30000"`
	After
}

func (d *Desktop) wait(ctx context.Context, in waitArgs) (*mcp.CallToolResult, error) {
	if in.MS < 0 || in.MS > 30000 {
		return nil, errors.New("ms must be between 0 and 30000")
	}
	sleep(ctx, time.Duration(in.MS)*time.Millisecond)
	return text("Waited %d ms.", in.MS), ctx.Err()
}

type waitForArgs struct {
	Name      string `json:"name,omitempty" jsonschema:"text the widget's name contains"`
	Role      string `json:"role,omitempty" jsonschema:"text the widget's role contains"`
	Window    int64  `json:"window,omitempty" jsonschema:"window id to look for the widget in, default all windows"`
	Title     string `json:"title,omitempty" jsonschema:"instead of a widget, wait for a window whose title or app contains this"`
	Gone      bool   `json:"gone,omitempty" jsonschema:"wait until it is no longer there"`
	TimeoutMS int    `json:"timeout_ms,omitempty" jsonschema:"give up after this long, default 5000, at most 60000"`
	After
}

func (d *Desktop) waitFor(ctx context.Context, in waitForArgs) (*mcp.CallToolResult, error) {
	timeout := cmp.Or(in.TimeoutMS, 5000)
	if timeout < 0 || timeout > 60000 {
		return nil, errors.New("timeout_ms must be between 0 and 60000")
	}
	if in.Title == "" && in.Name == "" && in.Role == "" {
		return nil, errors.New("give a name, a role or a title to wait for")
	}
	if in.Title == "" && d.A11y == nil {
		return nil, errNoA11y
	}
	what := fmt.Sprintf("a widget matching name %q role %q", in.Name, in.Role)
	if in.Title != "" {
		what = fmt.Sprintf("a window matching %q", in.Title)
	}
	look := func() (string, bool, error) {
		if in.Title != "" {
			g, err := d.geometry()
			if err != nil {
				return "", false, err
			}
			tree, err := d.Sway.Tree()
			if err != nil {
				return "", false, err
			}
			want := strings.ToLower(in.Title)
			for _, w := range tree.Windows() {
				if strings.Contains(strings.ToLower(w.Name), want) || strings.Contains(strings.ToLower(w.App()), want) {
					return describe(&w, g), true, nil
				}
			}
			return "", false, nil
		}
		out, n, err := d.find(ctx, findArgs{Name: in.Name, Role: in.Role, Window: in.Window, Limit: 5})
		return out, n > 0, err
	}
	start := time.Now()
	for {
		out, found, err := look()
		ms := time.Since(start).Milliseconds()
		switch {
		case err != nil && in.Window != 0:
			return nil, err // a window that is not there will not come back
		case err == nil && found && !in.Gone:
			return text("There after %d ms:\n%s", ms, out), nil
		case err == nil && !found && in.Gone:
			return text("Gone after %d ms.", ms), nil
		}
		if ms >= int64(timeout) {
			if in.Gone {
				return nil, fmt.Errorf("%s was still there after %d ms", what, timeout)
			}
			return nil, fmt.Errorf("%s did not appear within %d ms", what, timeout)
		}
		sleep(ctx, 250*time.Millisecond)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
}

type batchStep struct {
	Tool string         `json:"tool" jsonschema:"the tool's name"`
	Args map[string]any `json:"args,omitempty" jsonschema:"the tool's arguments, as it takes them"`
}

type batchArgs struct {
	Actions []batchStep `json:"actions" jsonschema:"the steps, in order; at most 25"`
	After
}

func (d *Desktop) batch(ctx context.Context, in batchArgs) (*mcp.CallToolResult, error) {
	if len(in.Actions) == 0 || len(in.Actions) > 25 {
		return nil, errors.New("give 1 to 25 actions")
	}
	for i, st := range in.Actions {
		if _, ok := d.tools[st.Tool]; !ok {
			return nil, fmt.Errorf("action %d: no tool %q to batch", i+1, st.Tool)
		}
	}
	// One hold of the input lock for the whole batch, so another agent's actions
	// cannot fall between its steps.
	unlock := d.lock(ctx)
	defer unlock()
	ctx = context.WithValue(ctx, heldKey{}, true)
	res := &mcp.CallToolResult{}
	for i, st := range in.Actions {
		raw, err := json.Marshal(st.Args)
		var out *mcp.CallToolResult
		if err == nil {
			out, err = d.tools[st.Tool](ctx, raw)
		}
		if err != nil {
			res.Content = append(res.Content, &mcp.TextContent{Text: fmt.Sprintf("%d. %s failed: %v\nStopped: the %d actions after it were not run.", i+1, st.Tool, err, len(in.Actions)-i-1)})
			res.IsError = true
			return res, nil
		}
		for j, c := range out.Content {
			if tc, ok := c.(*mcp.TextContent); ok && j == 0 {
				c = &mcp.TextContent{Text: fmt.Sprintf("%d. %s", i+1, tc.Text)}
			}
			res.Content = append(res.Content, c)
		}
		// Apps act on input a moment after they get it; the next step waits that out.
		if i < len(in.Actions)-1 && actingTools[st.Tool] {
			sleep(ctx, betweenSteps)
		}
	}
	return res, nil
}

// betweenSteps is the pause between a batch's acting steps.
const betweenSteps = 80 * time.Millisecond

func (d *Desktop) getText(ctx context.Context, in refArgs) (*mcp.CallToolResult, error) {
	if d.A11y == nil {
		return nil, errNoA11y
	}
	s, err := d.A11y.Text(ctx, in.Ref)
	if err != nil {
		return nil, err
	}
	return text("%s", s), nil
}

type replayArgs struct {
	Name string `json:"name,omitempty" jsonschema:"the clip's file name without extension: 1 to 64 letters, digits, - and _ (default a random one)"`
	FPS  int    `json:"fps,omitempty" jsonschema:"frames a second, 2 to 4 (default 3)"`
	Keep bool   `json:"keep,omitempty" jsonschema:"keep the frames for the next clip instead of starting afresh"`
}

func (d *Desktop) replay(ctx context.Context, in replayArgs) (*mcp.CallToolResult, error) {
	fps := cmp.Or(in.FPS, 3)
	if fps < 2 || fps > 4 {
		return nil, errors.New("fps must be 2, 3 or 4")
	}
	if in.Name != "" && !clipName.MatchString(in.Name) {
		return nil, errors.New("name must be 1 to 64 letters, digits, - and _")
	}
	d.Replay.Flush()
	frames := d.Replay.Recent(false)
	p, err := d.Replay.Save(ctx, frames, in.Name, fps)
	if err != nil {
		return nil, err
	}
	if !in.Keep {
		d.Replay.Recent(true)
	}
	var size int64
	if fi, err := os.Stat(p); err == nil {
		size = fi.Size()
	}
	if d.Log != nil {
		d.Log.Info("replay saved", "frames", len(frames), "bytes", size)
	}
	return text("Saved a replay of %d frames at %d fps (%d KB) to %s. It is deleted after %s.", len(frames), fps, (size+1023)/1024, p, keepText(d.Replay.Keep)), nil
}

func keepText(k time.Duration) string {
	if k >= 24*time.Hour && k%(24*time.Hour) == 0 {
		return fmt.Sprintf("%d days", k/(24*time.Hour))
	}
	return k.String()
}

var errNoA11y = errors.New("accessibility is off (swampcy was started with -a11y=false)")

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
