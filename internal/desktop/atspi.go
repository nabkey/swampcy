package desktop

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

// AT-SPI lives on its own D-Bus bus, whose address the session bus's org.a11y.Bus
// hands out. Every accessible object is a (bus name, object path) pair; the
// registry's root lists one per application.
const (
	ifAccessible   = "org.a11y.atspi.Accessible"
	ifComponent    = "org.a11y.atspi.Component"
	ifAction       = "org.a11y.atspi.Action"
	ifText         = "org.a11y.atspi.Text"
	ifEditableText = "org.a11y.atspi.EditableText"
	ifValue        = "org.a11y.atspi.Value"
	ifSelection    = "org.a11y.atspi.Selection"

	scrollTopEdge  uint32 = 2 // ATSPI_SCROLL_TOP_EDGE
	scrollAnywhere uint32 = 6 // ATSPI_SCROLL_ANYWHERE

	coordWindow uint32 = 1 // ATSPI_COORD_TYPE_WINDOW; screen coordinates mean nothing under Wayland
)

var registryRoot = accRef{Bus: "org.a11y.atspi.Registry", Path: "/org/a11y/atspi/accessible/root"}

// stateNames is AtspiStateType in order (at-spi2-core's atspi-constants.h).
var stateNames = [...]string{
	"invalid", "active", "armed", "busy", "checked", "collapsed", "defunct", "editable",
	"enabled", "expandable", "expanded", "focusable", "focused", "has-tooltip", "horizontal",
	"iconified", "modal", "multi-line", "multiselectable", "opaque", "pressed", "resizable",
	"selectable", "selected", "sensitive", "showing", "single-line", "stale", "transient",
	"vertical", "visible", "manages-descendants", "indeterminate", "required", "truncated",
	"animated", "invalid-entry", "supports-autocompletion", "selectable-text", "is-default",
	"visited", "checkable", "has-popup", "read-only",
}

const (
	stateEnabled   = 8
	stateSensitive = 24
	stateShowing   = 25
	stateActive    = 1
)

// shownStates are the states worth a model's attention; the rest (enabled,
// visible, opaque, ...) are true of nearly everything and only cost tokens.
var shownStates = map[string]bool{
	"checked": true, "collapsed": true, "expanded": true, "focused": true, "selected": true,
	"pressed": true, "editable": true, "read-only": true, "indeterminate": true, "modal": true,
	"has-popup": true, "invalid-entry": true, "busy": true, "required": true, "multi-line": true,
}

// genericRoles are containers that, unnamed and without actions, are left out of
// the outline while their children are kept. GTK 4 calls most of its boxes
// "generic" and "group".
var genericRoles = map[string]bool{
	"filler": true, "panel": true, "section": true, "unknown": true, "redundant object": true,
	"invalid": true, "layered pane": true, "viewport": true, "grouping": true, "generic": true,
	"group": true,
}

// toolkitActions are the prefixes of actions GTK 4 gives every label and text
// widget (copy, select all, open a link, the context menu). They are left out of
// the outline, though click_element still performs one when it is named.
//
// Chrome gives every element showContextMenu, its text clickAncestor and its
// containers doDefault, which say nothing either; click_element performs a
// button's doDefault without its being listed.
var toolkitActions = []string{"clipboard.", "selection.", "menu.", "link.", "text.", "showContextMenu", "clickAncestor", "doDefault"}

// namingRoles are elements whose name is the text inside them, so that text's
// own elements under them add nothing.
var namingRoles = map[string]bool{
	"link": true, "button": true, "heading": true, "table cell": true, "menu item": true,
	"page tab": true, "list item": true, "row header": true, "column header": true,
}

// textRoles are the roles whose text get_text is worth pointing at. Chrome gives
// every element the Text interface, and a named label's text is its name, so the
// mark is kept for these and for unnamed leaves.
var textRoles = map[string]bool{
	"text": true, "paragraph": true, "terminal": true, "document text": true, "document web": true,
	"document frame": true, "document email": true, "entry": true, "password text": true,
}

// popupRoles are an application's top-level children that are not windows of
// their own in sway's tree: Chrome's bubbles, alerts and menus. They are drawn
// over the window and outlined with it.
var popupRoles = map[string]bool{
	"alert": true, "notification": true, "dialog": true, "menu": true, "popup menu": true,
	"tool tip": true, "list box": true, "window": true, "panel": true,
}

// webContainers are the roles that, inside a web document, can be off the screen
// while something in them is on it: a page's wrapper keeps the box it had before
// the page was scrolled, and Chrome then says it is not showing though the text
// in it is. Such an element is not outlined but is looked into. Anything else
// that is not showing (a paragraph, a link, a table row) has nothing showing in it.
var webContainers = map[string]bool{
	"section": true, "panel": true, "generic": true, "group": true, "grouping": true, "filler": true,
	"landmark": true, "list": true, "form": true, "header": true, "footer": true, "article": true,
	"table": true, "scroll pane": true, "internal frame": true, "document web": true, "document frame": true,
	"content info": true, "block quote": true, "unknown": true,
}

// fetchWorkers is how many elements are read at once: each takes several D-Bus
// round trips, and a web page has thousands.
const fetchWorkers = 8

// maxShownActions bounds the actions listed on one line.
const maxShownActions = 6

// redundant reports whether an element adds nothing to the outline: an unnamed
// generic container without actions of its own, or text that repeats the name of
// the nearest element above it that the outline shows (parent).
func redundant(n, parent *node) bool {
	if n.name == "" && len(shownActions(n.actions)) == 0 && genericRoles[n.role] {
		return true
	}
	if n.role != "label" && n.role != "static" {
		return false
	}
	if n.name == "" {
		return len(n.children) == 0
	}
	return parent != nil && (n.name == parent.name || (namingRoles[parent.role] && strings.Contains(parent.name, n.name)))
}

func shownActions(actions []string) []string {
	var out []string
	for _, a := range actions {
		if !slices.ContainsFunc(toolkitActions, func(p string) bool { return strings.HasPrefix(a, p) }) {
			out = append(out, a)
		}
	}
	if len(out) > maxShownActions {
		out = append(out[:maxShownActions], fmt.Sprintf("+%d more", len(out)-maxShownActions))
	}
	return out
}

// gtkClickLands is how long a GTK button takes to act on a click action. GTK
// shows the button pressed for 250 ms (ACTIVATE_TIMEOUT in gtkbutton.c), emits
// clicked only then, and ignores another activation until it has. Returning at
// once let a second press of the same button vanish, and a screenshot taken
// straight after showed the click not done yet.
const gtkClickLands = 300 * time.Millisecond

// preferredActions are tried in order when click_element names no action.
var preferredActions = []string{"click", "press", "activate", "doDefault", "toggle", "jump", "open", "select", "switch"}

type accRef struct {
	Bus  string
	Path dbus.ObjectPath
}

// stateSet is AT-SPI's state bitfield: bit i of the 64 is stateNames[i].
type stateSet []uint32

func (s stateSet) has(i int) bool {
	return i/32 < len(s) && s[i/32]&(1<<(i%32)) != 0
}

// shown lists the interesting states, and "disabled" for an element that is
// neither enabled nor sensitive.
func (s stateSet) shown() []string {
	var out []string
	for i, name := range stateNames {
		if s.has(i) && shownStates[name] {
			out = append(out, name)
		}
	}
	if len(s) > 0 && !s.has(stateEnabled) && !s.has(stateSensitive) {
		out = append(out, "disabled")
	}
	return out
}

// Target is what an element reference stands for: the element, the frame it was
// found under, and the sway window holding that frame. Coordinates are worked out
// again each time a target is used, because windows move.
type Target struct {
	Elem  accRef
	Frame accRef
	ConID int64
}

// A11y reads and acts on applications through AT-SPI.
type A11y struct {
	BusAddress string        // the session bus, where org.a11y.Bus answers
	Timeout    time.Duration // per D-Bus call; a hung app must not hang the tool

	connMu sync.Mutex
	conn   *dbus.Conn

	refMu sync.Mutex
	refs  map[string]Target
	ids   map[accRef]string
	next  int

	toolkitMu sync.Mutex
	toolkits  map[string]string // an application's bus name → its ToolkitName
}

// errNoAction is returned by Activate for an element that has no usable action,
// so the caller can click its centre instead.
var errNoAction = errors.New("element has no action")

// Enable sets org.a11y.Status.IsEnabled, which toolkits read at startup to decide
// whether to expose their widgets. Apps already running keep their decision.
func (a *A11y) Enable(ctx context.Context) error {
	sess, err := dbus.Connect(a.BusAddress)
	if err != nil {
		return fmt.Errorf("session bus: %w", err)
	}
	defer sess.Close()
	obj := sess.Object("org.a11y.Bus", "/org/a11y/bus")
	ctx, cancel := context.WithTimeout(ctx, a.timeout())
	defer cancel()
	return obj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Set", 0, "org.a11y.Status", "IsEnabled", dbus.MakeVariant(true)).Err
}

func (a *A11y) timeout() time.Duration {
	if a.Timeout > 0 {
		return a.Timeout
	}
	return 3 * time.Second
}

// bus returns a connection to the accessibility bus, dialling it on first use and
// again after it drops (at-spi-bus-launcher restarts with the session).
func (a *A11y) bus(ctx context.Context) (*dbus.Conn, error) {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	if a.conn != nil && a.conn.Connected() {
		return a.conn, nil
	}
	sess, err := dbus.Connect(a.BusAddress)
	if err != nil {
		return nil, fmt.Errorf("session bus: %w", err)
	}
	defer sess.Close()
	cctx, cancel := context.WithTimeout(ctx, a.timeout())
	defer cancel()
	var addr string
	if err := sess.Object("org.a11y.Bus", "/org/a11y/bus").CallWithContext(cctx, "org.a11y.Bus.GetAddress", 0).Store(&addr); err != nil {
		return nil, fmt.Errorf("accessibility bus address: %w (is at-spi2-core installed?)", err)
	}
	conn, err := dbus.Connect(addr)
	if err != nil {
		return nil, fmt.Errorf("accessibility bus: %w", err)
	}
	a.conn = conn
	return conn, nil
}

func (a *A11y) call(ctx context.Context, conn *dbus.Conn, r accRef, method string, out any, args ...any) error {
	ctx, cancel := context.WithTimeout(ctx, a.timeout())
	defer cancel()
	c := conn.Object(r.Bus, r.Path).CallWithContext(ctx, method, 0, args...)
	if c.Err != nil || out == nil {
		return c.Err
	}
	return c.Store(out)
}

func (a *A11y) prop(ctx context.Context, conn *dbus.Conn, r accRef, iface, name string, out any) error {
	var v dbus.Variant
	if err := a.call(ctx, conn, r, "org.freedesktop.DBus.Properties.Get", &v, iface, name); err != nil {
		return err
	}
	return v.Store(out)
}

// node is what one round of calls learns about an element.
type node struct {
	ref      accRef
	role     string
	name     string
	states   stateSet
	ifaces   []string
	ext      *Rect // window coordinates
	actions  []string
	value    string // "current (min..max)" for a slider, spin button or progress bar
	children []accRef
	more     int // children not fetched
	// hidden marks a web container that is not showing, visited only for what is
	// in it: it is neither outlined nor matched.
	hidden bool
}

func (n *node) has(iface string) bool { return slices.Contains(n.ifaces, iface) }

// frameWait is how long a window's tree is waited for before giving up.
const frameWait = 3 * time.Second

// maxChildren bounds how many children of one element are read; a table with
// thousands of rows is summarised rather than walked.
const maxChildren = 200

func (a *A11y) fetch(ctx context.Context, conn *dbus.Conn, r accRef, children bool) (*node, error) {
	n, err := a.fetchLite(ctx, conn, r, children)
	if err != nil {
		return nil, err
	}
	a.fill(ctx, conn, n)
	return n, nil
}

// fetchLite reads what a search needs to match an element and walk on: its role,
// name, states and children. fill reads the rest.
func (a *A11y) fetchLite(ctx context.Context, conn *dbus.Conn, r accRef, children bool) (*node, error) {
	n := &node{ref: r}
	if err := a.call(ctx, conn, r, ifAccessible+".GetRoleName", &n.role); err != nil {
		return nil, err
	}
	_ = a.prop(ctx, conn, r, ifAccessible, "Name", &n.name)
	var st []uint32
	if a.call(ctx, conn, r, ifAccessible+".GetState", &st) == nil {
		n.states = st
	}
	if children {
		_ = a.call(ctx, conn, r, ifAccessible+".GetChildren", &n.children)
		if len(n.children) > maxChildren {
			n.more = len(n.children) - maxChildren
			n.children = n.children[:maxChildren]
		}
	}
	return n, nil
}

// fill reads an element's interfaces, bounds, actions and value.
func (a *A11y) fill(ctx context.Context, conn *dbus.Conn, n *node) {
	r := n.ref
	_ = a.call(ctx, conn, r, ifAccessible+".GetInterfaces", &n.ifaces)
	if n.has(ifComponent) {
		var e struct{ X, Y, W, H int32 }
		if a.call(ctx, conn, r, ifComponent+".GetExtents", &e, coordWindow) == nil && e.W > 0 && e.H > 0 {
			n.ext = &Rect{X: int(e.X), Y: int(e.Y), Width: int(e.W), Height: int(e.H)}
		}
	}
	if n.has(ifAction) {
		var acts []struct{ Name, Description, KeyBinding string }
		if a.call(ctx, conn, r, ifAction+".GetActions", &acts) == nil {
			for i, act := range acts {
				// Chrome leaves GetActions' names empty; GetName has them.
				if act.Name == "" {
					_ = a.call(ctx, conn, r, ifAction+".GetName", &act.Name, int32(i))
				}
				n.actions = append(n.actions, act.Name)
			}
		}
	}
	if n.has(ifValue) {
		var cur, lo, hi float64
		if a.prop(ctx, conn, r, ifValue, "CurrentValue", &cur) == nil {
			_ = a.prop(ctx, conn, r, ifValue, "MinimumValue", &lo)
			_ = a.prop(ctx, conn, r, ifValue, "MaximumValue", &hi)
			n.value = fmt.Sprintf("%s (%s..%s)", num(cur), num(lo), num(hi))
		}
	}
}

func num(f float64) string { return strconv.FormatFloat(f, 'g', 6, 64) }

// fetchAll reads several elements at once, keeping their order; one that has
// gone since its parent listed it is nil.
func (a *A11y) fetchAll(ctx context.Context, conn *dbus.Conn, refs []accRef, lite, all bool) []*node {
	out := make([]*node, len(refs))
	sem := make(chan struct{}, fetchWorkers)
	var wg sync.WaitGroup
	for i, r := range refs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			n, err := a.fetchLite(ctx, conn, r, true)
			if err != nil {
				return
			}
			// What is not showing is at most looked into, never shown: the rest of
			// it need not be read.
			if !lite && (all || len(n.states) == 0 || n.states.has(stateShowing)) {
				a.fill(ctx, conn, n)
			}
			out[i] = n
		}()
	}
	wg.Wait()
	return out
}

// frame finds the AT-SPI frame for a sway window: the application whose bus
// connection belongs to the window's process, then its top-level child whose
// name is the window's title. When no process matches (an app behind a D-Bus
// proxy), a unique title match across all applications is accepted.
//
// It also returns the application's other top-level children that are popups
// (Chrome's bubbles and alerts), which sway's tree does not list as windows.
func (a *A11y) frame(ctx context.Context, conn *dbus.Conn, w Window) (accRef, []accRef, error) {
	var apps []accRef
	if err := a.call(ctx, conn, registryRoot, ifAccessible+".GetChildren", &apps); err != nil {
		return accRef{}, nil, fmt.Errorf("accessibility registry: %w", err)
	}
	var byTitle []accRef
	for _, app := range apps {
		var pid uint32
		cctx, cancel := context.WithTimeout(ctx, a.timeout())
		err := conn.BusObject().CallWithContext(cctx, "org.freedesktop.DBus.GetConnectionUnixProcessID", 0, app.Bus).Store(&pid)
		cancel()
		var frames []accRef
		if a.call(ctx, conn, app, ifAccessible+".GetChildren", &frames) != nil || len(frames) == 0 {
			continue
		}
		var named, active []accRef
		for _, f := range frames {
			var name string
			_ = a.prop(ctx, conn, f, ifAccessible, "Name", &name)
			if name == w.Name {
				named = append(named, f)
			}
			var st []uint32
			if a.call(ctx, conn, f, ifAccessible+".GetState", &st) == nil && stateSet(st).has(stateActive) {
				active = append(active, f)
			}
		}
		if err == nil && int(pid) == w.PID {
			var f accRef
			switch {
			case len(named) > 0:
				f = named[0]
			case len(frames) == 1:
				f = frames[0]
			case len(active) > 0:
				f = active[0]
			default:
				f = frames[0]
			}
			var popups []accRef
			for _, o := range frames {
				var role string
				if o != f && a.call(ctx, conn, o, ifAccessible+".GetRoleName", &role) == nil && popupRoles[role] {
					popups = append(popups, o)
				}
			}
			return f, popups, nil
		}
		byTitle = append(byTitle, named...)
	}
	if len(byTitle) == 1 {
		return byTitle[0], nil, nil
	}
	return accRef{}, nil, fmt.Errorf("window %d (%s, pid %d) has no accessibility tree: the app may not support AT-SPI, or started before accessibility was on; start it again with launch", w.ID, w.App(), w.PID)
}

// toLayout maps an element's window coordinates onto the layout: its offset from
// the frame, added to the window's content origin in sway.
//
// A client that draws its own shadow (Chrome on Wayland, even tiled) reports a
// frame the size of its whole surface, shadow included, and element positions
// relative to that surface, while sway places only the window geometry inside
// it. When the frame is larger than sway's content area, the difference is the
// shadow, taken to be the same on both sides, and comes off.
func toLayout(el, frame Rect, content Rect) Rect {
	dx, dy := shadowInset(frame, content)
	return Rect{X: content.X + el.X - frame.X - dx, Y: content.Y + el.Y - frame.Y - dy, Width: el.Width, Height: el.Height}
}

// maxShadow bounds the inset toLayout takes off: a frame much larger than its
// window is stale (mid-resize), not a shadow.
const maxShadow = 64

func shadowInset(frame, content Rect) (int, int) {
	if content.Width <= 0 || content.Height <= 0 {
		return 0, 0
	}
	dw, dh := frame.Width-content.Width, frame.Height-content.Height
	if dw <= 0 || dh <= 0 || dw > 2*maxShadow || dh > 2*maxShadow {
		return 0, 0
	}
	return dw / 2, dh / 2
}

func (a *A11y) refFor(t Target) string {
	a.refMu.Lock()
	defer a.refMu.Unlock()
	if a.refs == nil || len(a.refs) > 50000 {
		a.refs, a.ids = map[string]Target{}, map[accRef]string{}
	}
	if id, ok := a.ids[t.Elem]; ok {
		a.refs[id] = t
		return id
	}
	a.next++
	id := "e" + strconv.Itoa(a.next)
	a.refs[id], a.ids[t.Elem] = t, id
	return id
}

func (a *A11y) lookup(ref string) (Target, error) {
	a.refMu.Lock()
	defer a.refMu.Unlock()
	t, ok := a.refs[ref]
	if !ok {
		return Target{}, fmt.Errorf("unknown element %q; refs come from accessibility_tree and find_elements", ref)
	}
	return t, nil
}

// TreeOptions bounds an outline.
type TreeOptions struct {
	MaxDepth int
	MaxNodes int
	All      bool // include elements that are not showing
}

// walker walks one window's tree, writing an outline or collecting matches.
type walker struct {
	a       *A11y
	conn    *dbus.Conn
	w       Window
	g       Geometry
	frame   accRef
	popups  []accRef
	frameXY Rect
	opts    TreeOptions
	visited int
	// maxVisit bounds the elements read; 0 means opts.MaxNodes. An outline reads
	// more than it shows, since it leaves many out.
	maxVisit int
	full     bool
	// lite has the walk read only what matching needs; the visitor fills in what
	// it shows.
	lite bool
	// inDoc counts the web documents the walk is inside.
	inDoc int
}

func (a *A11y) walker(ctx context.Context, w Window, g Geometry, opts TreeOptions) (*walker, *node, error) {
	conn, err := a.bus(ctx)
	if err != nil {
		return nil, nil, err
	}
	// An app registers with AT-SPI a moment after its window appears, so a window
	// launched just now gets a little time.
	var f accRef
	var popups []accRef
	for deadline := time.Now().Add(frameWait); ; {
		if f, popups, err = a.frame(ctx, conn, w); err == nil || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	if err != nil {
		return nil, nil, err
	}
	root, err := a.fetch(ctx, conn, f, true)
	if err != nil {
		return nil, nil, fmt.Errorf("window %d: %w", w.ID, err)
	}
	wk := &walker{a: a, conn: conn, w: w, g: g, frame: f, popups: popups, opts: opts}
	if root.ext != nil {
		wk.frameXY = *root.ext
	}
	return wk, root, nil
}

// visit walks depth-first, calling fn for each element that is showing (or every
// element with All). fn returns whether to descend.
func (wk *walker) visit(ctx context.Context, n, parent *node, depth int, fn func(n, parent *node, depth int) bool) {
	limit := cmp.Or(wk.maxVisit, wk.opts.MaxNodes)
	if wk.full || wk.visited >= limit {
		wk.full = true
		return
	}
	wk.visited++
	if !fn(n, parent, depth) || depth >= wk.opts.MaxDepth {
		return
	}
	if strings.HasPrefix(n.role, "document") {
		wk.inDoc++
		defer func() { wk.inDoc-- }()
	}
	for _, cn := range wk.a.fetchAll(ctx, wk.conn, n.children, wk.lite, wk.opts.All) {
		if ctx.Err() != nil || wk.full || wk.visited >= limit {
			wk.full = wk.full || wk.visited >= limit
			return
		}
		if cn == nil {
			continue // gone since its parent listed it
		}
		if !wk.opts.All && len(cn.states) > 0 && !cn.states.has(stateShowing) {
			if wk.inDoc == 0 || !webContainers[cn.role] || len(cn.children) == 0 {
				continue
			}
			cn.hidden = true
		}
		wk.visit(ctx, cn, n, depth+1, fn)
	}
}

func (wk *walker) ref(n *node) string {
	return wk.a.refFor(Target{Elem: n.ref, Frame: wk.frame, ConID: wk.w.ID})
}

// line is one element of an outline or a search result.
func (wk *walker) line(n *node) string {
	var b strings.Builder
	b.WriteString(wk.ref(n))
	b.WriteString(" ")
	b.WriteString(n.role)
	if n.name != "" {
		b.WriteString(" " + strconv.Quote(clip(n.name, 80)))
	}
	if n.ext != nil {
		r := wk.g.ToShot(toLayout(*n.ext, wk.frameXY, wk.w.Content()))
		fmt.Fprintf(&b, " (%d,%d %dx%d)", r.X, r.Y, r.Width, r.Height)
	}
	marks := n.states.shown()
	if n.has(ifEditableText) {
		marks = append(marks, "settable")
	} else if n.has(ifText) && !slices.Contains(marks, "editable") && (textRoles[n.role] || (n.name == "" && len(n.children) == 0)) {
		marks = append(marks, "text")
	}
	if n.value != "" {
		marks = append(marks, "value="+n.value)
	}
	if len(marks) > 0 {
		b.WriteString(" [" + strings.Join(marks, ",") + "]")
	}
	if acts := shownActions(n.actions); len(acts) > 0 {
		b.WriteString(" actions: " + strings.Join(acts, ","))
	}
	if n.more > 0 {
		fmt.Fprintf(&b, " (+%d more children)", n.more)
	}
	return b.String()
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// Tree outlines a window's accessible elements, each with a reference that the
// other element tools take, its role and name, and its bounds in screenshot
// pixels.
func (a *A11y) Tree(ctx context.Context, w Window, g Geometry, opts TreeOptions) (string, error) {
	wk, root, err := a.walker(ctx, w, g, opts)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "window %d %q (%s)\n", w.ID, w.Name, w.App())
	// An outline shows at most MaxNodes elements, and reads up to eight times as
	// many to find them: a web page is mostly containers that are left out.
	wk.maxVisit = opts.MaxNodes * 8
	shown := 0
	// indentAt[d] is the indent for elements at depth d: one more than their nearest
	// shown ancestor's, which is aboveAt[d]. The walk is depth-first, so a parent
	// sets both just before its children are visited.
	indentAt, aboveAt := []int{0}, []*node{nil}
	outline := func(n, _ *node, depth int) bool {
		indent, child, above := indentAt[depth], indentAt[depth], aboveAt[depth]
		if depth == 0 || (!n.hidden && !redundant(n, above)) {
			if shown >= opts.MaxNodes {
				wk.full = true
				return false
			}
			shown++
			b.WriteString(strings.Repeat("  ", indent))
			b.WriteString(wk.line(n))
			b.WriteByte('\n')
			child, above = indent+1, n
		}
		if len(indentAt) <= depth+1 {
			indentAt, aboveAt = append(indentAt, child), append(aboveAt, above)
		} else {
			indentAt[depth+1], aboveAt[depth+1] = child, above
		}
		return true
	}
	// Popups first: they are drawn over the window, and are what an agent must
	// deal with before anything under them.
	for _, p := range wk.popupRoots(ctx) {
		b.WriteString("popup over this window:\n")
		indentAt, aboveAt = []int{1}, []*node{nil}
		wk.visit(ctx, p, nil, 0, outline)
	}
	indentAt, aboveAt = []int{0}, []*node{nil}
	wk.visit(ctx, root, nil, 0, outline)
	if wk.full {
		fmt.Fprintf(&b, "… stopped after %d elements; narrow with find_elements or raise max_nodes\n", shown)
	}
	return b.String(), nil
}

// popupRoots are the application's popups that are showing.
func (wk *walker) popupRoots(ctx context.Context) []*node {
	var out []*node
	for _, n := range wk.a.fetchAll(ctx, wk.conn, wk.popups, wk.lite, true) {
		if n != nil && n.states.has(stateShowing) {
			out = append(out, n)
		}
	}
	return out
}

// Query selects elements by case-insensitive substrings of their name and role.
type Query struct {
	Name, Role string
	Limit      int
}

// Find searches the given windows' trees and lists matching elements.
// It also returns how many matched.
func (a *A11y) Find(ctx context.Context, ws []Window, g Geometry, q Query, opts TreeOptions) (string, int, error) {
	name, role := strings.ToLower(q.Name), strings.ToLower(q.Role)
	var b strings.Builder
	found := 0
	var errs []string
	for _, w := range ws {
		wk, root, err := a.walker(ctx, w, g, opts)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		wk.lite = true
		match := func(n, _ *node, _ int) bool {
			if found >= q.Limit {
				return false
			}
			if !n.hidden && strings.Contains(strings.ToLower(n.name), name) && strings.Contains(strings.ToLower(n.role), role) {
				found++
				a.fill(ctx, wk.conn, n)
				fmt.Fprintf(&b, "%s — window %d", wk.line(n), w.ID)
				if len(n.states) > 0 && !n.states.has(stateShowing) {
					b.WriteString(" (not showing: scroll_to brings it into view)")
				}
				b.WriteByte('\n')
			}
			return true
		}
		wk.visit(ctx, root, nil, 0, match)
		for _, p := range wk.popupRoots(ctx) {
			wk.visit(ctx, p, nil, 0, match)
		}
		if wk.full {
			errs = append(errs, fmt.Sprintf("window %d: stopped after %d elements", w.ID, wk.visited))
		}
	}
	if found == 0 {
		b.WriteString("no element matches")
		if len(errs) > 0 {
			b.WriteString("; " + strings.Join(errs, "; "))
		}
		b.WriteByte('\n')
	} else if found >= q.Limit {
		fmt.Fprintf(&b, "… stopped at %d matches\n", q.Limit)
	}
	return b.String(), found, nil
}

// Activate performs an element's action: the one named, or the first of
// preferredActions it offers. It returns the action's name, or errNoAction.
func (a *A11y) Activate(ctx context.Context, ref, action string) (string, error) {
	t, err := a.lookup(ref)
	if err != nil {
		return "", err
	}
	conn, err := a.bus(ctx)
	if err != nil {
		return "", err
	}
	n, err := a.fetch(ctx, conn, t.Elem, false)
	if err != nil {
		return "", fmt.Errorf("%s is gone: %w", ref, err)
	}
	idx := -1
	if action != "" {
		idx = slices.IndexFunc(n.actions, func(s string) bool { return strings.EqualFold(s, action) })
		if idx < 0 {
			return "", fmt.Errorf("%s has no action %q (it has: %s)", ref, action, strings.Join(n.actions, ", "))
		}
	} else {
		for _, p := range preferredActions {
			if idx = slices.IndexFunc(n.actions, func(s string) bool { return strings.EqualFold(s, p) }); idx >= 0 {
				break
			}
		}
		if idx < 0 {
			if a.selectInParent(ctx, conn, t.Elem) {
				return "select", nil
			}
			return "", errNoAction
		}
	}
	var ok bool
	if err := a.call(ctx, conn, t.Elem, ifAction+".DoAction", &ok, int32(idx)); err != nil {
		return "", fmt.Errorf("%s: %s: %w", ref, n.actions[idx], err)
	}
	if !ok {
		return "", fmt.Errorf("%s refused %s", ref, n.actions[idx])
	}
	if strings.EqualFold(n.actions[idx], "click") && a.toolkit(ctx, conn, t.Elem.Bus) == "GTK" {
		select {
		case <-time.After(gtkClickLands):
		case <-ctx.Done():
		}
	}
	return n.actions[idx], nil
}

// selectInParent selects an element that has no action of its own through its
// parent's Selection interface: an item of a list, a combo box or a tab strip.
func (a *A11y) selectInParent(ctx context.Context, conn *dbus.Conn, r accRef) bool {
	var parent accRef
	if a.prop(ctx, conn, r, ifAccessible, "Parent", &parent) != nil || parent.Path == "" {
		return false
	}
	var ifaces []string
	if a.call(ctx, conn, parent, ifAccessible+".GetInterfaces", &ifaces) != nil || !slices.Contains(ifaces, ifSelection) {
		return false
	}
	var idx int32
	if a.call(ctx, conn, r, ifAccessible+".GetIndexInParent", &idx) != nil || idx < 0 {
		return false
	}
	var ok bool
	return a.call(ctx, conn, parent, ifSelection+".SelectChild", &ok, idx) == nil && ok
}

// ScrollTo asks the app to scroll an element into view.
func (a *A11y) ScrollTo(ctx context.Context, ref string) error {
	t, err := a.lookup(ref)
	if err != nil {
		return err
	}
	conn, err := a.bus(ctx)
	if err != nil {
		return err
	}
	// To the top of its view, so that what follows it shows too; anywhere in
	// view when the app will not do that.
	var ok bool
	if a.call(ctx, conn, t.Elem, ifComponent+".ScrollTo", &ok, scrollTopEdge) == nil && ok {
		return nil
	}
	if err := a.call(ctx, conn, t.Elem, ifComponent+".ScrollTo", &ok, scrollAnywhere); err != nil {
		return fmt.Errorf("%s cannot be scrolled to: %w", ref, err)
	}
	if !ok {
		return fmt.Errorf("%s's app would not scroll to it", ref)
	}
	return nil
}

// SetValue sets the value of a slider, spin button or scroll bar, and returns
// what the element then reports.
func (a *A11y) SetValue(ctx context.Context, ref string, v float64) (string, error) {
	t, err := a.lookup(ref)
	if err != nil {
		return "", err
	}
	conn, err := a.bus(ctx)
	if err != nil {
		return "", err
	}
	if err := a.call(ctx, conn, t.Elem, "org.freedesktop.DBus.Properties.Set", nil, ifValue, "CurrentValue", dbus.MakeVariant(v)); err != nil {
		return "", fmt.Errorf("%s: %w (does it have a value?)", ref, err)
	}
	n, err := a.fetch(ctx, conn, t.Elem, false)
	if err != nil {
		return "", err
	}
	return n.value, nil
}

// Line describes one element as the outline would, with where it is now.
func (a *A11y) Line(ctx context.Context, ref string, tree *Node, g Geometry) (string, error) {
	t, err := a.lookup(ref)
	if err != nil {
		return "", err
	}
	w := tree.Find(t.ConID)
	if w == nil {
		return "", fmt.Errorf("%s's window %d is gone", ref, t.ConID)
	}
	conn, err := a.bus(ctx)
	if err != nil {
		return "", err
	}
	n, err := a.fetch(ctx, conn, t.Elem, false)
	if err != nil {
		return "", fmt.Errorf("%s is gone: %w", ref, err)
	}
	wk := &walker{a: a, conn: conn, w: *w, g: g, frame: t.Frame}
	if f, err := a.fetch(ctx, conn, t.Frame, false); err == nil && f.ext != nil {
		wk.frameXY = *f.ext
	}
	return wk.line(n), nil
}

// toolkit is the ToolkitName of the application on a bus name, such as "GTK" or
// "Qt", or "" when it does not say. It is asked once per application.
func (a *A11y) toolkit(ctx context.Context, conn *dbus.Conn, bus string) string {
	a.toolkitMu.Lock()
	defer a.toolkitMu.Unlock()
	if name, ok := a.toolkits[bus]; ok {
		return name
	}
	var name string
	if err := a.prop(ctx, conn, accRef{Bus: bus, Path: registryRoot.Path}, "org.a11y.atspi.Application", "ToolkitName", &name); err != nil {
		return ""
	}
	if a.toolkits == nil {
		a.toolkits = map[string]string{}
	}
	a.toolkits[bus] = name
	return name
}

// Bounds is an element's rectangle in layout coordinates, worked out from where
// its window is now.
func (a *A11y) Bounds(ctx context.Context, ref string, tree *Node) (Rect, error) {
	t, err := a.lookup(ref)
	if err != nil {
		return Rect{}, err
	}
	w := tree.Find(t.ConID)
	if w == nil {
		return Rect{}, fmt.Errorf("%s's window %d is gone", ref, t.ConID)
	}
	conn, err := a.bus(ctx)
	if err != nil {
		return Rect{}, err
	}
	n, err := a.fetch(ctx, conn, t.Elem, false)
	if err != nil {
		return Rect{}, fmt.Errorf("%s is gone: %w", ref, err)
	}
	if n.ext == nil {
		return Rect{}, fmt.Errorf("%s has no on-screen bounds", ref)
	}
	f, err := a.fetch(ctx, conn, t.Frame, false)
	if err != nil || f.ext == nil {
		return Rect{}, fmt.Errorf("%s's frame has no bounds", ref)
	}
	return toLayout(*n.ext, *f.ext, w.Content()), nil
}

// SetText replaces an editable element's text.
func (a *A11y) SetText(ctx context.Context, ref, text string) error {
	t, err := a.lookup(ref)
	if err != nil {
		return err
	}
	conn, err := a.bus(ctx)
	if err != nil {
		return err
	}
	var ok bool
	if err := a.call(ctx, conn, t.Elem, ifEditableText+".SetTextContents", &ok, text); err != nil {
		return fmt.Errorf("%s: %w (is it settable?)", ref, err)
	}
	if !ok {
		return fmt.Errorf("%s refused the text", ref)
	}
	return nil
}

// maxText bounds what get_text returns.
const maxText = 20000

// Text reads an element's text.
func (a *A11y) Text(ctx context.Context, ref string) (string, error) {
	t, err := a.lookup(ref)
	if err != nil {
		return "", err
	}
	conn, err := a.bus(ctx)
	if err != nil {
		return "", err
	}
	var count int32
	if err := a.prop(ctx, conn, t.Elem, ifText, "CharacterCount", &count); err != nil {
		return "", fmt.Errorf("%s has no text: %w", ref, err)
	}
	var s string
	if err := a.call(ctx, conn, t.Elem, ifText+".GetText", &s, int32(0), min(count, maxText)); err != nil {
		return "", fmt.Errorf("%s: %w", ref, err)
	}
	if count > maxText {
		s += fmt.Sprintf("\n… %d more characters", count-maxText)
	}
	return s, nil
}
