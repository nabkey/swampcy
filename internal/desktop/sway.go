package desktop

import (
	"cmp"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"time"
)

// Sway speaks i3's IPC protocol on a Unix socket: a message is the magic
// "i3-ipc", the payload's length and the message type as native-endian uint32s,
// then the payload. A reply has the same header with the request's type.
const (
	ipcMagic      = "i3-ipc"
	ipcRunCommand = 0
	ipcGetOutputs = 3
	ipcGetTree    = 4
	ipcGetSeats   = 101
	ipcGetVersion = 7
)

// Rect is a rectangle in sway's layout coordinates, which are the pixels of the
// screenshots grim takes when the output scale is 1.
type Rect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// Node is one container in sway's tree. Only the fields swampcy reads are decoded.
type Node struct {
	ID            int64   `json:"id"`
	Name          string  `json:"name"`
	Type          string  `json:"type"` // root, output, workspace, con, floating_con
	Rect          Rect    `json:"rect"`
	WindowRect    Rect    `json:"window_rect"` // the client's area, relative to Rect
	Focused       bool    `json:"focused"`
	Visible       *bool   `json:"visible"`
	AppID         *string `json:"app_id"`
	PID           int     `json:"pid"`
	Shell         string  `json:"shell"`
	Nodes         []*Node `json:"nodes"`
	FloatingNodes []*Node `json:"floating_nodes"`
	WindowProps   *struct {
		Class string `json:"class"`
	} `json:"window_properties"`
}

// IsWindow reports whether the node holds a client: an xdg-shell window has an
// app_id (possibly empty), an Xwayland one window properties.
func (n *Node) IsWindow() bool {
	return (n.Type == "con" || n.Type == "floating_con") && (n.AppID != nil || n.WindowProps != nil)
}

// App is the window's app_id, or its X11 class under Xwayland.
func (n *Node) App() string {
	if n.AppID != nil && *n.AppID != "" {
		return *n.AppID
	}
	if n.WindowProps != nil {
		return n.WindowProps.Class
	}
	return ""
}

// Content is the client's own area in layout coordinates: the container's rect
// less sway's border and title bar.
func (n *Node) Content() Rect {
	if n.WindowRect.Width == 0 || n.WindowRect.Height == 0 {
		return n.Rect
	}
	return Rect{X: n.Rect.X + n.WindowRect.X, Y: n.Rect.Y + n.WindowRect.Y, Width: n.WindowRect.Width, Height: n.WindowRect.Height}
}

// Window is a window in the tree, with the workspace it is on.
type Window struct {
	*Node
	Output    string
	Workspace string
	Floating  bool
}

// Windows walks the tree and returns every window, tiled ones before floating
// ones within each workspace.
func (n *Node) Windows() []Window {
	var out []Window
	var walk func(n *Node, output, ws string, floating bool)
	walk = func(n *Node, output, ws string, floating bool) {
		switch n.Type {
		case "output":
			output = n.Name
		case "workspace":
			ws = n.Name
		}
		if n.IsWindow() {
			out = append(out, Window{Node: n, Output: output, Workspace: ws, Floating: floating})
		}
		for _, c := range n.Nodes {
			walk(c, output, ws, floating)
		}
		for _, c := range n.FloatingNodes {
			walk(c, output, ws, true)
		}
	}
	walk(n, "", "", false)
	return out
}

// Find returns the window with this container id, or nil.
func (n *Node) Find(id int64) *Window {
	for _, w := range n.Windows() {
		if w.ID == id {
			return &w
		}
	}
	return nil
}

// Focused returns the focused window, or nil when focus is on an empty workspace.
func (n *Node) FocusedWindow() *Window {
	for _, w := range n.Windows() {
		if w.Focused {
			return &w
		}
	}
	return nil
}

// Output is one of sway's outputs; a headless sway has HEADLESS-1.
type Output struct {
	Name   string  `json:"name"`
	Active bool    `json:"active"`
	Rect   Rect    `json:"rect"` // in the layout: logical pixels
	Scale  float64 `json:"scale"`
	Mode   struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"current_mode"` // device pixels: what a capture of it holds
}

// Sway is a client for one sway's IPC socket. Each request opens its own
// connection, so a Sway is safe for concurrent use and survives sway restarting.
type Sway struct {
	Socket  string
	Timeout time.Duration
}

func (s *Sway) request(typ uint32, payload string) ([]byte, error) {
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	c, err := net.DialTimeout("unix", s.Socket, timeout)
	if err != nil {
		return nil, fmt.Errorf("sway ipc: %w", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))
	if _, err := c.Write(encodeIPC(typ, []byte(payload))); err != nil {
		return nil, fmt.Errorf("sway ipc: %w", err)
	}
	gotType, body, err := decodeIPC(c)
	if err != nil {
		return nil, fmt.Errorf("sway ipc: %w", err)
	}
	if gotType != typ {
		return nil, fmt.Errorf("sway ipc: reply type %d to request type %d", gotType, typ)
	}
	return body, nil
}

func encodeIPC(typ uint32, payload []byte) []byte {
	b := make([]byte, 0, len(ipcMagic)+8+len(payload))
	b = append(b, ipcMagic...)
	b = binary.NativeEndian.AppendUint32(b, uint32(len(payload)))
	b = binary.NativeEndian.AppendUint32(b, typ)
	return append(b, payload...)
}

// maxIPCReply bounds a reply; a tree with hundreds of windows is well under it.
const maxIPCReply = 64 << 20

func decodeIPC(r io.Reader) (uint32, []byte, error) {
	var h [len(ipcMagic) + 8]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	if string(h[:len(ipcMagic)]) != ipcMagic {
		return 0, nil, errors.New("bad magic")
	}
	n := binary.NativeEndian.Uint32(h[len(ipcMagic):])
	typ := binary.NativeEndian.Uint32(h[len(ipcMagic)+4:])
	if n > maxIPCReply {
		return 0, nil, fmt.Errorf("reply of %d bytes", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return typ, body, nil
}

// Command runs sway commands (several may be joined with ";" or ",") and returns
// the first failure's message as an error.
func (s *Sway) Command(cmd string) error {
	body, err := s.request(ipcRunCommand, cmd)
	if err != nil {
		return err
	}
	var results []struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(body, &results); err != nil {
		return fmt.Errorf("sway ipc: command reply: %w", err)
	}
	for _, r := range results {
		if !r.Success {
			return fmt.Errorf("sway: %s: %s", cmd, r.Error)
		}
	}
	return nil
}

// Tree returns sway's layout tree.
func (s *Sway) Tree() (*Node, error) {
	body, err := s.request(ipcGetTree, "")
	if err != nil {
		return nil, err
	}
	var root Node
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("sway ipc: tree: %w", err)
	}
	return &root, nil
}

// Seat is one of sway's seats, from get_seats.
type Seat struct {
	Name  string `json:"name"`
	Focus int64  `json:"focus"` // the container focused on it, 0 for none
}

// Seats returns sway's seats.
func (s *Sway) Seats() ([]Seat, error) {
	body, err := s.request(ipcGetSeats, "")
	if err != nil {
		return nil, err
	}
	var seats []Seat
	if err := json.Unmarshal(body, &seats); err != nil {
		return nil, fmt.Errorf("sway ipc: seats: %w", err)
	}
	return seats, nil
}

// Outputs returns the active outputs.
func (s *Sway) Outputs() ([]Output, error) {
	body, err := s.request(ipcGetOutputs, "")
	if err != nil {
		return nil, err
	}
	var all []Output
	if err := json.Unmarshal(body, &all); err != nil {
		return nil, fmt.Errorf("sway ipc: outputs: %w", err)
	}
	var out []Output
	for _, o := range all {
		if o.Active {
			out = append(out, o)
		}
	}
	return out, nil
}

// Version returns sway's human-readable version, which also proves the socket
// is a sway that answers.
func (s *Sway) Version() (string, error) {
	body, err := s.request(ipcGetVersion, "")
	if err != nil {
		return "", err
	}
	var v struct {
		HumanReadable string `json:"human_readable"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return "", fmt.Errorf("sway ipc: version: %w", err)
	}
	return v.HumanReadable, nil
}

// Layout is the bounding box of the active outputs: the area grim captures with
// no -o or -g.
// PickOutput returns the output named name, or with name empty the one at the
// layout's top left (leftmost, then topmost). Not the first sway lists: it lists
// a headless sway's HEADLESS-2 before HEADLESS-1.
func PickOutput(outputs []Output, name string) (Output, error) {
	if len(outputs) == 0 {
		return Output{}, errors.New("sway has no active output")
	}
	if name == "" {
		return slices.MinFunc(outputs, func(a, b Output) int {
			return cmp.Or(cmp.Compare(a.Rect.X, b.Rect.X), cmp.Compare(a.Rect.Y, b.Rect.Y))
		}), nil
	}
	var names []string
	for _, o := range outputs {
		if o.Name == name {
			return o, nil
		}
		names = append(names, o.Name)
	}
	return Output{}, fmt.Errorf("no active output named %q (there are: %s)", name, strings.Join(names, ", "))
}

func Layout(outputs []Output) (Rect, error) {
	if len(outputs) == 0 {
		return Rect{}, errors.New("sway has no active output")
	}
	r := outputs[0].Rect
	x2, y2 := r.X+r.Width, r.Y+r.Height
	for _, o := range outputs[1:] {
		r.X = min(r.X, o.Rect.X)
		r.Y = min(r.Y, o.Rect.Y)
		x2 = max(x2, o.Rect.X+o.Rect.Width)
		y2 = max(y2, o.Rect.Y+o.Rect.Height)
	}
	r.Width, r.Height = x2-r.X, y2-r.Y
	return r, nil
}
