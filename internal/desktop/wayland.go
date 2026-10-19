package desktop

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// A headless sway has no input devices, so its seat offers clients neither a
// pointer nor a keyboard, and nothing sent to the seat reaches an app. VirtualInput
// holds a Wayland connection with a virtual pointer and a virtual keyboard for as
// long as the session lives: the seat then always has both, and the pointer and
// keys are driven through them. With several seats (an agent's and a person's),
// each VirtualInput joins the one it is told to.
//
// The Wayland wire protocol is small enough to speak with the standard library:
// each message is the object id, then the message size and opcode packed in one
// word, then the arguments, 32-bit aligned; file descriptors travel alongside as
// SCM_RIGHTS.

const (
	wlDisplayID = 1

	// wl_display requests and events
	wlDisplaySync        = 0
	wlDisplayGetRegistry = 1
	wlDisplayError       = 0
	// wl_registry
	wlRegistryBind   = 0
	wlRegistryGlobal = 0
	// wl_callback
	wlCallbackDone = 0
	// zwlr_virtual_pointer_manager_v1 and zwlr_virtual_pointer_v1
	vpManagerCreate   = 0
	vpManagerCreateOn = 2 // create_virtual_pointer_with_output, since version 2 (1 is destroy)
	vpMotionAbsolute  = 1
	vpButton          = 2
	vpFrame           = 4
	vpAxisSource      = 5
	vpAxisDiscrete    = 7
	vpDestroy         = 8
	axisVertical      = 0
	axisHorizontal    = 1
	axisSourceWheel   = 0
	buttonPressed     = 1
	buttonReleased    = 0
	wheelStepDistance = 15 // what libinput reports for one click of a wheel
	// zwp_virtual_keyboard_manager_v1 and zwp_virtual_keyboard_v1
	vkManagerCreate = 0
	vkKeymap        = 0
	vkKey           = 1
	vkModifiers     = 2
	keymapXKBv1     = 1
	shiftMask       = 1 // Shift is the first modifier in usKeymap
)

// evdev button codes (linux/input-event-codes.h).
var evdevButtons = map[string]uint32{"left": 0x110, "right": 0x111, "middle": 0x112}

// usKeymap gives the virtual keyboard a keymap: the web viewer's keys are typed
// on it (keyboard.go), the protocol refuses a keyboard without one, and it is the
// keymap apps are sent while this keyboard is the seat's (see Activate). The
// compositor resolves the includes from its own xkeyboard-config.
const usKeymap = `xkb_keymap {
	xkb_keycodes { include "evdev+aliases(qwerty)" };
	xkb_types { include "complete" };
	xkb_compat { include "complete" };
	xkb_symbols { include "pc+us+inet(evdev)" };
};
`

type fixed float64 // wl_fixed_t, 24.8

// wlConn is one client connection. Requests are written under mu; a goroutine
// reads events until the connection fails, and err then holds why.
type wlConn struct {
	c      *net.UnixConn
	wmu    sync.Mutex // one request on the socket at a time
	mu     sync.Mutex // ids, globals, callbacks
	nextID uint32

	globals  map[string][]wlGlobal    // by interface, in the order announced
	done     map[uint32]chan struct{} // sync callbacks
	handlers map[uint32]func(opcode uint16, body []byte)

	// File descriptors the compositor sent, oldest first, for an event's handler to
	// take (takeFD). They arrive with or before the message they belong to, never
	// after. Only the reading goroutine touches this.
	fds []*os.File

	stateMu sync.Mutex
	err     error
	closed  chan struct{}
}

type wlGlobal struct {
	name, version uint32
}

func dialWayland(runtimeDir, display string) (*wlConn, error) {
	path := display
	if !filepath.IsAbs(path) {
		path = filepath.Join(runtimeDir, display)
	}
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("wayland: %w", err)
	}
	w := &wlConn{c: c, nextID: 2, globals: map[string][]wlGlobal{}, done: map[uint32]chan struct{}{},
		handlers: map[uint32]func(uint16, []byte){}, closed: make(chan struct{})}
	registry := w.newID()
	if err := w.send(wlDisplayID, wlDisplayGetRegistry, nil, registry); err != nil {
		c.Close()
		return nil, err
	}
	go w.read(registry)
	if err := w.roundtrip(); err != nil {
		w.Close()
		return nil, err
	}
	return w, nil
}

func (w *wlConn) newID() uint32 {
	id := w.nextID
	w.nextID++
	return id
}

// send writes one request. Arguments are uint32, int32, fixed or string; fd, when
// not nil, is passed alongside.
func (w *wlConn) send(obj uint32, opcode uint16, fd *os.File, args ...any) error {
	body := make([]byte, 0, 32)
	for _, a := range args {
		switch v := a.(type) {
		case uint32:
			body = binary.NativeEndian.AppendUint32(body, v)
		case int32:
			body = binary.NativeEndian.AppendUint32(body, uint32(v))
		case fixed:
			body = binary.NativeEndian.AppendUint32(body, uint32(int32(math.Round(float64(v)*256))))
		case string:
			body = binary.NativeEndian.AppendUint32(body, uint32(len(v)+1))
			body = append(body, v...)
			body = append(body, 0)
			for len(body)%4 != 0 {
				body = append(body, 0)
			}
		default:
			return fmt.Errorf("wayland: cannot encode %T", a)
		}
	}
	msg := make([]byte, 8, 8+len(body))
	binary.NativeEndian.PutUint32(msg[0:], obj)
	binary.NativeEndian.PutUint32(msg[4:], uint32(8+len(body))<<16|uint32(opcode))
	msg = append(msg, body...)
	if err := w.Err(); err != nil {
		return err
	}
	var oob []byte
	if fd != nil {
		oob = syscall.UnixRights(int(fd.Fd()))
	}
	w.wmu.Lock()
	_, _, err := w.c.WriteMsgUnix(msg, oob, nil)
	w.wmu.Unlock()
	if err != nil {
		w.fail(fmt.Errorf("wayland: %w", err))
		return w.Err()
	}
	return nil
}

func (w *wlConn) read(registry uint32) {
	defer func() {
		for _, f := range w.fds {
			f.Close()
		}
		w.fds = nil
	}()
	chunk := make([]byte, 4096)
	oob := make([]byte, syscall.CmsgSpace(28*4)) // libwayland sends at most 28 at once
	var buf []byte
	for {
		n, oobn, _, _, err := w.c.ReadMsgUnix(chunk, oob)
		if oobn > 0 {
			if msgs, perr := syscall.ParseSocketControlMessage(oob[:oobn]); perr == nil {
				for _, m := range msgs {
					got, _ := syscall.ParseUnixRights(&m)
					for _, fd := range got {
						w.fds = append(w.fds, os.NewFile(uintptr(fd), "wayland"))
					}
				}
			}
		}
		if err == nil && n == 0 {
			err = io.EOF
		}
		if err != nil {
			w.fail(fmt.Errorf("wayland: connection closed: %w", err))
			return
		}
		buf = append(buf, chunk[:n]...)
		for len(buf) >= 8 {
			obj := binary.NativeEndian.Uint32(buf[0:])
			word := binary.NativeEndian.Uint32(buf[4:])
			size, opcode := word>>16, uint16(word&0xffff)
			if size < 8 {
				w.fail(fmt.Errorf("wayland: bad message size %d", size))
				return
			}
			if uint32(len(buf)) < size {
				break // the rest comes with the next read
			}
			body := append([]byte(nil), buf[8:size]...)
			buf = buf[size:]
			if !w.dispatch(registry, obj, opcode, body) {
				return
			}
		}
	}
}

// dispatch handles one event, and says whether the connection is still usable.
func (w *wlConn) dispatch(registry, obj uint32, opcode uint16, body []byte) bool {
	switch {
	case obj == wlDisplayID && opcode == wlDisplayError:
		msg := ""
		if len(body) >= 12 {
			msg, _ = wireString(body[8:])
		}
		w.fail(fmt.Errorf("wayland protocol error on object %d (code %d): %s",
			binary.NativeEndian.Uint32(body[0:]), binary.NativeEndian.Uint32(body[4:]), msg))
		return false
	case obj == registry && opcode == wlRegistryGlobal && len(body) >= 12:
		name := binary.NativeEndian.Uint32(body[0:])
		iface, rest := wireString(body[4:])
		if len(rest) >= 4 {
			w.mu.Lock()
			w.globals[iface] = append(w.globals[iface], wlGlobal{name: name, version: binary.NativeEndian.Uint32(rest)})
			w.mu.Unlock()
		}
	default:
		w.mu.Lock()
		ch, isCallback := w.done[obj]
		if isCallback && opcode == wlCallbackDone {
			close(ch)
			delete(w.done, obj)
		}
		h := w.handlers[obj]
		w.mu.Unlock()
		if h != nil {
			h(opcode, body)
		}
	}
	// Anything without a handler (the seat's name and capabilities, delete_id,
	// shm formats, output modes) is not needed.
	return true
}

// takeFD hands an event's handler the oldest file descriptor received, which is
// then the handler's to close; nil when none came. Handlers run on the reading
// goroutine, the only one that touches the queue.
func (w *wlConn) takeFD() *os.File {
	if len(w.fds) == 0 {
		return nil
	}
	f := w.fds[0]
	w.fds = w.fds[1:]
	return f
}

// wireString decodes a string argument and returns what follows it.
func wireString(b []byte) (string, []byte) {
	if len(b) < 4 {
		return "", nil
	}
	n := int(binary.NativeEndian.Uint32(b))
	padded := (n + 3) &^ 3
	if n == 0 || 4+padded > len(b) {
		return "", nil
	}
	return string(b[4 : 4+n-1]), b[4+padded:]
}

// roundtrip waits until the compositor has handled every request sent so far,
// which is when a protocol error from one of them would have arrived.
func (w *wlConn) roundtrip() error {
	w.mu.Lock()
	cb := w.newID()
	ch := make(chan struct{})
	w.done[cb] = ch
	w.mu.Unlock()
	if err := w.send(wlDisplayID, wlDisplaySync, nil, cb); err != nil {
		return err
	}
	select {
	case <-ch:
		return w.Err()
	case <-w.closed:
		return w.Err()
	case <-time.After(5 * time.Second):
		return errors.New("wayland: compositor did not answer")
	}
}

func (w *wlConn) bind(iface string, maxVersion uint32) (uint32, error) {
	id, _, err := w.bindVersion(iface, maxVersion)
	return id, err
}

// bindVersion binds a global at the highest version both sides know, and says
// which that was. Where the compositor offers several (seats, outputs), it takes
// the first; bindNamed chooses one by name.
func (w *wlConn) bindVersion(iface string, maxVersion uint32) (uint32, uint32, error) {
	w.mu.Lock()
	gs := w.globals[iface]
	w.mu.Unlock()
	if len(gs) == 0 {
		return 0, 0, fmt.Errorf("the compositor does not offer %s", iface)
	}
	return w.bindGlobal(iface, gs[0], maxVersion)
}

func (w *wlConn) bindGlobal(iface string, g wlGlobal, maxVersion uint32) (uint32, uint32, error) {
	id := w.alloc()
	v := min(g.version, maxVersion)
	// The registry is object 2, the first id dialWayland allocated.
	return id, v, w.send(2, wlRegistryBind, nil, g.name, iface, v, id)
}

// Globals that announce their name in an event once bound: a seat from version 2,
// an output from version 4.
var nameEvents = map[string]struct {
	since  uint32
	opcode uint16
}{
	"wl_seat":   {2, 1},
	"wl_output": {4, 4},
}

// bindNamed binds the global of iface whose name is want: sway offers one wl_seat
// per seat and one wl_output per output, and which is announced last is not
// which one anyone means. Each is bound to hear its name; the others stay bound
// but unused. With want empty, the only one is taken, and with several, the first.
func (w *wlConn) bindNamed(iface, want string, maxVersion uint32) (uint32, error) {
	w.mu.Lock()
	gs := slices.Clone(w.globals[iface])
	w.mu.Unlock()
	if len(gs) == 0 {
		return 0, fmt.Errorf("the compositor does not offer %s", iface)
	}
	if want == "" {
		id, _, err := w.bindGlobal(iface, gs[0], maxVersion)
		return id, err
	}
	ev := nameEvents[iface]
	var mu sync.Mutex
	names := map[uint32]string{}
	var ids []uint32
	for _, g := range gs {
		if g.version < ev.since {
			continue
		}
		id, _, err := w.bindGlobal(iface, g, max(maxVersion, ev.since))
		if err != nil {
			return 0, err
		}
		ids = append(ids, id)
		w.handle(id, func(op uint16, body []byte) {
			if op == ev.opcode {
				name, _ := wireString(body)
				mu.Lock()
				names[id] = name
				mu.Unlock()
			}
		})
	}
	err := w.roundtrip()
	var seen []string
	found := uint32(0)
	mu.Lock()
	for _, id := range ids {
		w.handle(id, nil)
		if names[id] == want && found == 0 {
			found = id
		}
		seen = append(seen, names[id])
	}
	mu.Unlock()
	if err != nil {
		return 0, err
	}
	if found == 0 {
		return 0, fmt.Errorf("no %s named %q (there are: %s)", strings.TrimPrefix(iface, "wl_"), want, strings.Join(seen, ", "))
	}
	return found, nil
}

// handle routes the events of object id to h, from the reading goroutine; nil
// removes the route.
func (w *wlConn) handle(id uint32, h func(opcode uint16, body []byte)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if h == nil {
		delete(w.handlers, id)
	} else {
		w.handlers[id] = h
	}
}

// alloc returns a new object id for a request that creates one.
func (w *wlConn) alloc() uint32 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.newID()
}

func (w *wlConn) fail(err error) {
	w.stateMu.Lock()
	defer w.stateMu.Unlock()
	if w.err == nil {
		w.err = err
		close(w.closed)
		w.c.Close()
	}
}

// Err is why the connection failed, or nil while it works.
func (w *wlConn) Err() error {
	w.stateMu.Lock()
	defer w.stateMu.Unlock()
	return w.err
}

func (w *wlConn) Close() { w.fail(errors.New("wayland: closed")) }

// VirtualInput is a virtual pointer and keyboard on the session's seat.
type VirtualInput struct {
	w        *wlConn
	dir      string // where keymap files are written
	pointer  uint32
	keyboard uint32
	start    time.Time
	mu       sync.Mutex

	// What this connection holds down, so it can all be let go at once.
	keys    map[uint32]bool
	buttons map[uint32]bool
	mods    uint32 // depressed modifier mask
	locked  uint32 // Caps Lock
}

// InputPlace says where a VirtualInput's devices go: the seat they join (empty
// for the only one, or the first) and the output the pointer's absolute motion
// covers (empty for the whole layout).
type InputPlace struct {
	Seat   string
	Output string
}

// OpenVirtualInput connects to the session and creates the devices.
func OpenVirtualInput(s Session, at InputPlace) (*VirtualInput, error) {
	w, err := dialWayland(s.RuntimeDir, s.WaylandDisplay)
	if err != nil {
		return nil, err
	}
	v := &VirtualInput{w: w, dir: s.RuntimeDir, start: time.Now(), keys: map[uint32]bool{}, buttons: map[uint32]bool{}}
	if err := v.create(at); err != nil {
		w.Close()
		return nil, err
	}
	return v, nil
}

func (v *VirtualInput) create(at InputPlace) error {
	w := v.w
	seat, err := w.bindNamed("wl_seat", at.Seat, 1)
	if err != nil {
		return err
	}
	pmVersion := uint32(1)
	if at.Output != "" {
		pmVersion = 2
	}
	pm, got, err := w.bindVersion("zwlr_virtual_pointer_manager_v1", pmVersion)
	if err != nil {
		return err
	}
	if got < pmVersion {
		return errors.New("the compositor's virtual pointer cannot be tied to one output (version 1)")
	}
	var output uint32
	if at.Output != "" {
		if output, err = w.bindNamed("wl_output", at.Output, 4); err != nil {
			return err
		}
	}
	km, err := w.bind("zwp_virtual_keyboard_manager_v1", 1)
	if err != nil {
		return err
	}
	v.pointer, v.keyboard = w.alloc(), w.alloc()
	if output != 0 {
		err = w.send(pm, vpManagerCreateOn, nil, seat, output, v.pointer)
	} else {
		err = w.send(pm, vpManagerCreate, nil, seat, v.pointer)
	}
	if err != nil {
		return err
	}
	if err := w.send(km, vkManagerCreate, nil, seat, v.keyboard); err != nil {
		return err
	}
	if err := v.setKeymap(usKeymap); err != nil {
		return err
	}
	return v.activate()
}

// setKeymap gives the keyboard a keymap; apps get it with the next key. It goes
// over as a file: written, sent, unlinked.
func (v *VirtualInput) setKeymap(km string) error {
	f, err := os.CreateTemp(v.dir, "swampcy-keymap-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.WriteString(km + "\x00"); err != nil {
		return err
	}
	return v.w.send(v.keyboard, vkKeymap, f, uint32(keymapXKBv1), uint32(len(km)+1))
}

// Activate makes this keyboard the seat's active one. Sway gives apps the active
// keyboard's keymap, and a keyboard becomes active only by sending something; a
// seat with no active keyboard sends apps that take focus modifiers with no
// keymap (Chrome dereferences a null xkb state on them and crashes). A modifiers
// event makes this one active and sends its keymap to every app, but wlroots
// passes one on only when the state changes, so Shift is flipped and flipped
// back, with no key, ending on the modifiers held.
func (v *VirtualInput) Activate() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.activate()
}

func (v *VirtualInput) activate() error {
	for _, depressed := range []uint32{v.mods ^ shiftMask, v.mods} {
		if err := v.w.send(v.keyboard, vkModifiers, nil, depressed, uint32(0), v.locked, uint32(0)); err != nil {
			return err
		}
	}
	return v.w.roundtrip()
}

// Err is why the devices stopped working (the session went away), or nil.
func (v *VirtualInput) Err() error { return v.w.Err() }

// Close removes the devices from the seat.
func (v *VirtualInput) Close() { v.w.Close() }

func (v *VirtualInput) now() uint32 { return uint32(time.Since(v.start).Milliseconds()) }

// MoveTo puts the pointer at (x, y) within a layout of the given size.
func (v *VirtualInput) MoveTo(x, y, width, height int) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.w.send(v.pointer, vpMotionAbsolute, nil, v.now(), uint32(x), uint32(y), uint32(width), uint32(height)); err != nil {
		return err
	}
	return v.w.send(v.pointer, vpFrame, nil)
}

func (v *VirtualInput) button(button string, state uint32) error {
	code, ok := evdevButtons[button]
	if !ok {
		return fmt.Errorf("unknown button %q", button)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.w.send(v.pointer, vpButton, nil, v.now(), code, state); err != nil {
		return err
	}
	v.buttons[code] = state == buttonPressed
	return v.w.send(v.pointer, vpFrame, nil)
}

func (v *VirtualInput) Press(button string) error   { return v.button(button, buttonPressed) }
func (v *VirtualInput) Release(button string) error { return v.button(button, buttonReleased) }

// Scroll sends wheel clicks: positive is down or right.
func (v *VirtualInput) Scroll(direction string, clicks int) error {
	axis, sign := uint32(axisVertical), 1
	switch direction {
	case "down":
	case "up":
		sign = -1
	case "right":
		axis = axisHorizontal
	case "left":
		axis, sign = axisHorizontal, -1
	default:
		return fmt.Errorf("unknown scroll direction %q", direction)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for range clicks {
		if err := v.w.send(v.pointer, vpAxisSource, nil, uint32(axisSourceWheel)); err != nil {
			return err
		}
		if err := v.w.send(v.pointer, vpAxisDiscrete, nil, v.now(), axis, fixed(sign*wheelStepDistance), int32(sign)); err != nil {
			return err
		}
		if err := v.w.send(v.pointer, vpFrame, nil); err != nil {
			return err
		}
	}
	return nil
}

// Sync waits until the compositor has handled everything sent.
func (v *VirtualInput) Sync() error { return v.w.roundtrip() }
