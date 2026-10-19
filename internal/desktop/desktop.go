// Package swampcy is an MCP server that lets a coding agent see and drive GUI apps
// on a headless sway session: screenshots through grim, the pointer and keys
// through a virtual pointer and keyboard of its own, windows through sway's IPC,
// and widgets through AT-SPI.
package desktop

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

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
	Find(ctx context.Context, ws []Window, g Geometry, q Query, opts TreeOptions) (string, error)
	Activate(ctx context.Context, ref, action string) (string, error)
	Bounds(ctx context.Context, ref string, tree *Node) (Rect, error)
	SetText(ctx context.Context, ref, text string) error
	Text(ctx context.Context, ref string) (string, error)
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

	// input serialises everything that moves the pointer or presses keys: an agent
	// may call tools in parallel, and two clicks must not interleave.
	input sync.Mutex
}

const instructions = `swampcy drives a headless sway desktop: a screen nobody else uses, for GUI apps you start with launch.
- Look with screenshot (and zoom for small text). Coordinates for click, move, drag, scroll and zoom are always pixels of the full screenshot.
- Prefer the accessibility tools when the app supports AT-SPI (GTK, Qt, Firefox, LibreOffice; Electron and Chromium with launch): find_elements or accessibility_tree, then click_element, set_text or get_text. They act by element, not by pixel.
- Start apps with launch so they get the display and accessibility; apps started another way may have no accessibility tree.
- Actions return without a screenshot; take one when you need to see the result.
- There may be other screens, such as the owner's: windows says which screen each window is on, and move_window moves one between screens (to hand the owner a window, or take one back).`

// Server builds the MCP server.
func (d *Desktop) Server(version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "swampcy", Title: "swampcy", Version: version}, &mcp.ServerOptions{Instructions: instructions})
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}

	add(d, s, &mcp.Tool{Name: "screenshot", Annotations: ro,
		Description: "Capture the whole screen. Its pixels are the coordinate space for click, move, drag, scroll and zoom."},
		d.screenshot)
	add(d, s, &mcp.Tool{Name: "zoom", Annotations: ro,
		Description: "Capture part of the screen at full resolution, to read small text. The rectangle is in screenshot pixels; coordinates for other tools stay in the full screenshot's pixels."},
		d.zoom)
	add(d, s, &mcp.Tool{Name: "click",
		Description: "Click at a point in screenshot pixels. button is left (default), right or middle; count 2 double-clicks, 3 triple-clicks."},
		d.click)
	add(d, s, &mcp.Tool{Name: "move",
		Description: "Move the pointer to a point in screenshot pixels, to hover."},
		d.move)
	add(d, s, &mcp.Tool{Name: "drag",
		Description: "Press a button at one point, move to another and release, in screenshot pixels."},
		d.drag)
	add(d, s, &mcp.Tool{Name: "scroll",
		Description: "Scroll at a point in screenshot pixels: direction up, down, left or right, amount in wheel clicks (default 3)."},
		d.scroll)
	add(d, s, &mcp.Tool{Name: "type",
		Description: "Type text into the focused window. Any Unicode works; use key for Enter, Tab and shortcuts."},
		d.typeText)
	add(d, s, &mcp.Tool{Name: "key",
		Description: `Press keys or shortcuts, separated by spaces: "Return", "ctrl+s", "ctrl+shift+t", "alt+F4", "Down Down Return". Modifiers: ctrl, shift, alt, super. Keys: names like Return, Escape, Tab, BackSpace, Delete, Up, Page_Down, Home, F5, or a single character.`},
		d.key)
	add(d, s, &mcp.Tool{Name: "windows", Annotations: ro,
		Description: "List the windows: id, title, app, process id, bounds in screenshot pixels, workspace, and which has focus."},
		d.windows)
	add(d, s, &mcp.Tool{Name: "focus_window",
		Description: "Focus a window by id, raising it to the visible workspace."},
		d.focusWindow)
	add(d, s, &mcp.Tool{Name: "move_window",
		Description: "Move a window to another screen (output), such as the owner's, or bring one back to yours (the default). windows says which screen each is on."},
		d.moveWindow)
	add(d, s, &mcp.Tool{Name: "launch",
		Description: "Start a GUI app on the desktop with a shell command (\"gnome-calculator\", \"firefox https://example.com\"). It gets the display and accessibility turned on, and outlives this server. Waits up to wait_ms (default 5000) for a new window and reports it."},
		d.launch)
	add(d, s, &mcp.Tool{Name: "accessibility_tree", Annotations: ro,
		Description: "Outline a window's widgets (default: the focused window): a ref (e12) for each, its role, name, bounds in screenshot pixels, notable states and actions. Pass refs to click_element, set_text and get_text."},
		d.accessibilityTree)
	add(d, s, &mcp.Tool{Name: "find_elements", Annotations: ro,
		Description: "Find widgets whose name and role contain the given text (case-insensitive), in one window or all of them. Returns refs for click_element, set_text and get_text."},
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
	if d.Replay != nil {
		add(d, s, &mcp.Tool{Name: "replay",
			Description: "Save a short animated clip (WebP, or GIF) of what you did on the screen: a frame after each click, key, type, scroll, drag, launch, click_element and set_text in the last 30 minutes. Returns the file's path. Call it once at the end of a task that acted on the screen; it starts the next clip afresh. The clip may show signed-in pages: never send or upload it anywhere."},
			d.replay)
	}
	return s
}

// add registers a tool whose handler returns a result, logging each call's
// duration and error but never its arguments, which may hold typed secrets.
func add[In any](d *Desktop, s *mcp.Server, t *mcp.Tool, h func(context.Context, In) (*mcp.CallToolResult, error)) {
	mcp.AddTool(s, t, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
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
		if d.Log != nil {
			d.Log.Info("tool", "name", t.Name, "ms", time.Since(start).Milliseconds(), "err", err)
		}
		return res, nil, err
	})
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
	scale := 1.0
	if long := max(r.Width, r.Height); d.MaxEdge > 0 && long > d.MaxEdge {
		scale = float64(d.MaxEdge) / float64(long)
	}
	img, mime, err := d.Screen.Capture(ctx, &r, scale)
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.ImageContent{Data: img, MIMEType: mime},
		&mcp.TextContent{Text: fmt.Sprintf("Region (%d,%d %dx%d) at %.1fx the screenshot's scale. Coordinates for other tools are still full-screenshot pixels.", in.X, in.Y, in.Width, in.Height, scale/g.Scale)},
	}}, nil
}

type clickArgs struct {
	X      int    `json:"x" jsonschema:"in screenshot pixels"`
	Y      int    `json:"y" jsonschema:"in screenshot pixels"`
	Button string `json:"button,omitempty" jsonschema:"left (default), right or middle"`
	Count  int    `json:"count,omitempty" jsonschema:"1 (default), 2 for a double-click, 3 for a triple-click"`
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
	if err := d.clickAt(x, y, button, count); err != nil {
		return nil, err
	}
	return text("Clicked %s ×%d at (%d,%d).", button, count, in.X, in.Y), nil
}

// A press and release with no time between them is lost on GTK 4: its click
// gesture sees nothing happen. These pauses are well inside a double-click.
const (
	settleAfterMove = 20 * time.Millisecond
	holdButton      = 40 * time.Millisecond
	betweenClicks   = 60 * time.Millisecond
)

func (d *Desktop) clickAt(x, y int, button string, count int) error {
	d.input.Lock()
	defer d.input.Unlock()
	if err := d.Pointer.MoveTo(x, y); err != nil {
		return err
	}
	time.Sleep(settleAfterMove)
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
}

type pointArgs struct {
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
	d.input.Lock()
	defer d.input.Unlock()
	if err := d.Pointer.MoveTo(x, y); err != nil {
		return nil, err
	}
	return text("Pointer at (%d,%d).", in.X, in.Y), nil
}

type dragArgs struct {
	FromX  int    `json:"from_x" jsonschema:"in screenshot pixels"`
	FromY  int    `json:"from_y" jsonschema:"in screenshot pixels"`
	ToX    int    `json:"to_x" jsonschema:"in screenshot pixels"`
	ToY    int    `json:"to_y" jsonschema:"in screenshot pixels"`
	Button string `json:"button,omitempty" jsonschema:"left (default), right or middle"`
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
	x2, y2, err := g.ToLayout(in.ToX, in.ToY)
	if err != nil {
		return nil, err
	}
	button := cmp.Or(in.Button, "left")
	d.input.Lock()
	defer d.input.Unlock()
	if err := d.Pointer.MoveTo(x1, y1); err != nil {
		return nil, err
	}
	time.Sleep(settleAfterMove)
	if err := d.Pointer.Press(button); err != nil {
		return nil, err
	}
	for i := 1; i <= dragSteps; i++ {
		x := x1 + int(math.Round(float64(x2-x1)*float64(i)/dragSteps))
		y := y1 + int(math.Round(float64(y2-y1)*float64(i)/dragSteps))
		if err := d.Pointer.MoveTo(x, y); err != nil {
			_ = d.Pointer.Release(button)
			return nil, err
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := d.Pointer.Release(button); err != nil {
		return nil, err
	}
	return text("Dragged from (%d,%d) to (%d,%d).", in.FromX, in.FromY, in.ToX, in.ToY), nil
}

type scrollArgs struct {
	X         int    `json:"x" jsonschema:"in screenshot pixels"`
	Y         int    `json:"y" jsonschema:"in screenshot pixels"`
	Direction string `json:"direction" jsonschema:"up, down, left or right"`
	Amount    int    `json:"amount,omitempty" jsonschema:"wheel clicks, default 3"`
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
	d.input.Lock()
	defer d.input.Unlock()
	if err := d.Pointer.MoveTo(x, y); err != nil {
		return nil, err
	}
	if err := d.Pointer.Scroll(in.Direction, amount); err != nil {
		return nil, err
	}
	return text("Scrolled %s %d at (%d,%d).", in.Direction, amount, in.X, in.Y), nil
}

type typeArgs struct {
	Text string `json:"text" jsonschema:"the text to type"`
}

func (d *Desktop) typeText(ctx context.Context, in typeArgs) (*mcp.CallToolResult, error) {
	d.input.Lock()
	defer d.input.Unlock()
	if err := d.Keyboard.Type(ctx, in.Text); err != nil {
		return nil, err
	}
	return text("Typed %d characters.", len([]rune(in.Text))), nil
}

type keyArgs struct {
	Keys string `json:"keys" jsonschema:"one or more keys or shortcuts separated by spaces, e.g. ctrl+s or Down Return"`
}

func (d *Desktop) key(ctx context.Context, in keyArgs) (*mcp.CallToolResult, error) {
	combos, err := ParseKeys(in.Keys)
	if err != nil {
		return nil, err
	}
	d.input.Lock()
	defer d.input.Unlock()
	if err := d.Keyboard.Keys(ctx, combos); err != nil {
		return nil, err
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
}

func (d *Desktop) focusWindow(ctx context.Context, in windowArgs) (*mcp.CallToolResult, error) {
	tree, err := d.Sway.Tree()
	if err != nil {
		return nil, err
	}
	if tree.Find(in.Window) == nil {
		return nil, fmt.Errorf("no window %d", in.Window)
	}
	d.input.Lock()
	defer d.input.Unlock()
	if err := d.Sway.Command(fmt.Sprintf("[con_id=%d] focus", in.Window)); err != nil {
		return nil, err
	}
	return text("Focused window %d.", in.Window), nil
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
	d.input.Lock()
	defer d.input.Unlock()
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
	d.input.Lock()
	if err := d.Keyboard.Keys(ctx, activateKeyboard); err != nil && d.Log != nil {
		d.Log.Warn("launch: activating the keyboard", "err", err)
	}
	d.input.Unlock()
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
}

func (d *Desktop) findElements(ctx context.Context, in findArgs) (*mcp.CallToolResult, error) {
	if d.A11y == nil {
		return nil, errNoA11y
	}
	if in.Name == "" && in.Role == "" {
		return nil, errors.New("give a name, a role or both")
	}
	g, err := d.geometry()
	if err != nil {
		return nil, err
	}
	var ws []Window
	if in.Window != 0 {
		_, w, err := d.target(in.Window)
		if err != nil {
			return nil, err
		}
		ws = []Window{*w}
	} else {
		tree, err := d.Sway.Tree()
		if err != nil {
			return nil, err
		}
		ws = tree.Windows()
	}
	out, err := d.A11y.Find(ctx, ws, g, Query{Name: in.Name, Role: in.Role, Limit: cmp.Or(in.Limit, 30)}, TreeOptions{MaxDepth: 40, MaxNodes: 3000})
	if err != nil {
		return nil, err
	}
	return text("%s", out), nil
}

type elementArgs struct {
	Ref     string `json:"ref" jsonschema:"element ref, e.g. e12, from accessibility_tree or find_elements"`
	Action  string `json:"action,omitempty" jsonschema:"the action to perform, from the element's actions"`
	Pointer bool   `json:"pointer,omitempty" jsonschema:"click the centre of the element with the pointer instead of performing an action"`
}

func (d *Desktop) clickElement(ctx context.Context, in elementArgs) (*mcp.CallToolResult, error) {
	if d.A11y == nil {
		return nil, errNoA11y
	}
	if !in.Pointer {
		d.input.Lock()
		action, err := d.A11y.Activate(ctx, in.Ref, in.Action)
		d.input.Unlock()
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
	x, y := r.X+r.Width/2, r.Y+r.Height/2
	if err := d.clickAt(x, y, "left", 1); err != nil {
		return nil, err
	}
	g, _ := d.geometry()
	s := g.ToShot(Rect{X: x, Y: y})
	return text("Clicked the centre of %s at (%d,%d).", in.Ref, s.X, s.Y), nil
}

type setTextArgs struct {
	Ref  string `json:"ref" jsonschema:"element ref of an editable widget"`
	Text string `json:"text" jsonschema:"the widget's new text"`
}

func (d *Desktop) setText(ctx context.Context, in setTextArgs) (*mcp.CallToolResult, error) {
	if d.A11y == nil {
		return nil, errNoA11y
	}
	d.input.Lock()
	defer d.input.Unlock()
	if err := d.A11y.SetText(ctx, in.Ref, in.Text); err != nil {
		return nil, err
	}
	return text("Set the text of %s.", in.Ref), nil
}

type refArgs struct {
	Ref string `json:"ref" jsonschema:"element ref"`
}

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
