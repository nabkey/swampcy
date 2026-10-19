package desktop

import (
	"encoding/binary"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeCompositor speaks enough of the Wayland server side for VirtualInput and
// Capture: it announces the globals they bind, answers wl_display.sync, plays
// screencopy (a W x H XRGB8888 screen whose every byte is the frame's number, with
// damageAfterFirst as the damage of later frames), and records every other
// request, with the keymap sent along.
type fakeCompositor struct {
	mu       sync.Mutex
	requests []wlRequest
	keymap   string   // the last keymap sent
	keymaps  []string // every keymap sent, in order
	errorOn  uint16   // send a protocol error on this opcode of the virtual pointer, if set
	objects  map[uint32]string
	names    map[uint32]string // the name of each bound seat and output
	// The seats and outputs announced, by name: one of each (seat0, HEADLESS-1)
	// when nil.
	seats, outputs []string

	damageAfterFirst [4]uint32

	// Data control: what was pasted from each selection set (read from the pipe the
	// fake sends the source, as an app pasting would), and the sources set.
	pasted  []string
	sources []uint32
}

const fakeW, fakeH = 64, 32

// orNames is names, or the one name def when there are none.
func orNames(names []string, def string) []string {
	if len(names) == 0 {
		return []string{def}
	}
	return names
}

type wlRequest struct {
	obj    uint32
	opcode uint16
	args   []uint32
}

func (f *fakeCompositor) serve(t *testing.T, c *net.UnixConn) {
	defer c.Close()
	write := func(obj uint32, opcode uint16, body []byte) {
		h := make([]byte, 8)
		binary.NativeEndian.PutUint32(h, obj)
		binary.NativeEndian.PutUint32(h[4:], uint32(8+len(body))<<16|uint32(opcode))
		c.Write(append(h, body...))
	}
	str := func(b []byte, s string) []byte {
		b = binary.NativeEndian.AppendUint32(b, uint32(len(s)+1))
		b = append(append(b, s...), 0)
		for len(b)%4 != 0 {
			b = append(b, 0)
		}
		return b
	}
	words := func(vs ...uint32) []byte {
		var b []byte
		for _, v := range vs {
			b = binary.NativeEndian.AppendUint32(b, v)
		}
		return b
	}
	f.mu.Lock()
	f.objects = map[uint32]string{}
	f.names = map[uint32]string{}
	f.mu.Unlock()
	var registry uint32
	var globalNames map[uint32]string // by global, for wl_seat and wl_output
	var fds []int
	pools := map[uint32][]byte{}
	buffers := map[uint32][]byte{}
	frames := 0
	var pending []byte // the start of a message the last read cut off
	for {
		buf := make([]byte, 4096)
		oob := make([]byte, 64)
		n, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
		if err != nil {
			return
		}
		if oobn > 0 {
			msgs, _ := syscall.ParseSocketControlMessage(oob[:oobn])
			for _, m := range msgs {
				got, _ := syscall.ParseUnixRights(&m)
				fds = append(fds, got...)
			}
		}
		b := append(pending, buf[:n]...)
		for len(b) >= 8 {
			obj := binary.NativeEndian.Uint32(b)
			word := binary.NativeEndian.Uint32(b[4:])
			size, opcode := int(word>>16), uint16(word&0xffff)
			if size < 8 || len(b) < size {
				break // the rest arrives with the next read
			}
			body := b[8:size]
			b = b[size:]
			var args []uint32
			for i := 0; i+4 <= len(body); i += 4 {
				args = append(args, binary.NativeEndian.Uint32(body[i:]))
			}
			f.mu.Lock()
			kind := f.objects[obj]
			f.mu.Unlock()
			switch {
			case obj == wlDisplayID && opcode == wlDisplayGetRegistry:
				registry = args[0]
				type global struct {
					iface   string
					version uint32
					name    string
				}
				gs := []global{}
				f.mu.Lock()
				for _, n := range orNames(f.seats, "seat0") {
					gs = append(gs, global{"wl_seat", 7, n})
				}
				gs = append(gs, global{"zwlr_virtual_pointer_manager_v1", 2, ""}, global{"zwp_virtual_keyboard_manager_v1", 1, ""},
					global{"zwlr_screencopy_manager_v1", 3, ""}, global{"zwlr_data_control_manager_v1", 2, ""})
				for _, n := range orNames(f.outputs, "HEADLESS-1") {
					gs = append(gs, global{"wl_output", 4, n})
				}
				gs = append(gs, global{"wl_shm", 1, ""})
				globalNames = map[uint32]string{}
				for i, g := range gs {
					globalNames[uint32(i+1)] = g.name
				}
				f.mu.Unlock()
				for i, g := range gs {
					write(registry, wlRegistryGlobal, binary.NativeEndian.AppendUint32(str(words(uint32(i+1)), g.iface), g.version))
				}
				continue
			case obj == wlDisplayID && opcode == wlDisplaySync:
				write(args[0], wlCallbackDone, words(0))
				continue
			case obj == registry && opcode == wlRegistryBind:
				iface, rest := wireString(body[4:])
				id, version := binary.NativeEndian.Uint32(rest[4:]), binary.NativeEndian.Uint32(rest)
				f.mu.Lock()
				f.objects[id] = iface
				name := globalNames[args[0]]
				f.names[id] = name
				f.mu.Unlock()
				// A seat says its name from version 2, an output from 4.
				if iface == "wl_seat" && version >= 2 {
					write(id, 1, str(nil, name))
				}
				if iface == "wl_output" && version >= 4 {
					write(id, 4, str(nil, name))
				}
				continue
			}
			f.mu.Lock()
			f.requests = append(f.requests, wlRequest{obj, opcode, args})
			switch {
			case kind == "zwlr_virtual_pointer_manager_v1" && opcode == vpManagerCreate:
				f.objects[args[1]] = "pointer"
			case kind == "zwlr_virtual_pointer_manager_v1" && opcode == vpManagerCreateOn:
				f.objects[args[2]] = "pointer"
			case kind == "zwlr_data_control_manager_v1" && opcode == dcManagerCreateSource:
				f.objects[args[0]] = "source"
			case kind == "zwlr_data_control_manager_v1" && opcode == dcManagerGetDevice:
				f.objects[args[0]] = "device"
			case kind == "device" && opcode == dcDeviceSetSelection && len(args) == 1:
				src := args[0]
				// The previous source is cancelled, as when anything replaces a selection.
				if n := len(f.sources); n > 0 {
					write(f.sources[n-1], dcSourceCancelled, nil)
				}
				f.sources = append(f.sources, src)
				// The seat's new selection is announced to the device as an offer
				// (0xff000001: a server-made id), then an app pastes it: the source is
				// sent a pipe and the text read back from it.
				write(obj, dcDeviceDataOffer, words(0xff000001))
				write(obj, dcDeviceSelection, words(0xff000001))
				var p [2]int
				if err := syscall.Pipe(p[:]); err == nil {
					body := str(nil, "text/plain;charset=utf-8")
					h := make([]byte, 8)
					binary.NativeEndian.PutUint32(h, src)
					binary.NativeEndian.PutUint32(h[4:], uint32(8+len(body))<<16|uint32(dcSourceSend))
					c.WriteMsgUnix(append(h, body...), syscall.UnixRights(p[1]), nil)
					syscall.Close(p[1])
					go func() {
						r := os.NewFile(uintptr(p[0]), "paste")
						got, _ := io.ReadAll(r)
						r.Close()
						f.mu.Lock()
						f.pasted = append(f.pasted, string(got))
						f.mu.Unlock()
					}()
				}
			case kind == "zwp_virtual_keyboard_manager_v1" && opcode == vkManagerCreate:
				f.objects[args[1]] = "keyboard"
			case kind == "keyboard" && opcode == vkKeymap && len(fds) > 0:
				file := os.NewFile(uintptr(fds[0]), "keymap")
				fds = fds[1:]
				file.Seek(0, io.SeekStart)
				km, _ := io.ReadAll(file)
				file.Close()
				f.keymap = string(km)
				f.keymaps = append(f.keymaps, f.keymap)
			case kind == "wl_shm" && opcode == shmCreatePool && len(fds) > 0:
				mem, err := syscall.Mmap(fds[0], 0, int(args[1]), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				syscall.Close(fds[0])
				fds = fds[1:]
				if err == nil {
					pools[args[0]] = mem
					f.objects[args[0]] = "pool"
				}
			case kind == "pool" && opcode == poolCreateBuffer:
				buffers[args[0]] = pools[obj][args[1]:]
				f.objects[args[0]] = "buffer"
			case kind == "zwlr_screencopy_manager_v1" && opcode == scCaptureOutput:
				f.objects[args[0]] = "frame"
				write(args[0], scBuffer, words(ShmXRGB8888, fakeW, fakeH, fakeW*4))
				write(args[0], scBufferDone, nil)
			case kind == "frame" && opcode == scCopyWithDamage && frames == 0:
				// Like a headless sway: nothing has been drawn, nothing changes, so a
				// copy that waits for damage never completes. Only a plain copy does.
			case kind == "frame" && (opcode == scCopy || opcode == scCopyWithDamage):
				frames++
				px := buffers[args[0]]
				for i := 0; i < fakeW*fakeH*4 && i < len(px); i++ {
					px[i] = byte(frames)
				}
				if frames > 1 {
					d := f.damageAfterFirst
					write(obj, scDamage, words(d[0], d[1], d[2], d[3]))
				}
				write(obj, scReady, words(0, 0, 0))
			}
			pointer := kind == "pointer"
			f.mu.Unlock()
			if f.errorOn != 0 && pointer && opcode == f.errorOn {
				write(wlDisplayID, wlDisplayError, str(words(obj, 1), "test error"))
			}
		}
		pending = append([]byte(nil), b...)
	}
}

func startFakeCompositor(t *testing.T, f *fakeCompositor) Session {
	t.Helper()
	dir, err := os.MkdirTemp("", "swampcy-wl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "wayland-9"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.AcceptUnix()
			if err != nil {
				return
			}
			go f.serve(t, c)
		}
	}()
	return Session{RuntimeDir: dir, WaylandDisplay: "wayland-9"}
}

// gotKeymaps is every keymap sent so far. The fake appends under its lock, and
// reading under it too is what orders the two for the race detector: the socket
// between them does not.
func (f *fakeCompositor) gotKeymaps() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keymaps...)
}

func (f *fakeCompositor) find(obj uint32, opcode uint16) []wlRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []wlRequest
	for _, r := range f.requests {
		if r.obj == obj && r.opcode == opcode {
			out = append(out, r)
		}
	}
	return out
}

func TestVirtualInput(t *testing.T) {
	f := &fakeCompositor{}
	v, err := OpenVirtualInput(startFakeCompositor(t, f), InputPlace{})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()

	f.mu.Lock()
	km := f.keymap
	f.mu.Unlock()
	if !strings.Contains(km, `xkb_symbols { include "pc+us+inet(evdev)" }`) || !strings.HasSuffix(km, "\x00") {
		t.Errorf("keymap = %q", km)
	}
	if v.pointer == 0 || v.keyboard == 0 || v.pointer == v.keyboard {
		t.Fatalf("pointer %d, keyboard %d", v.pointer, v.keyboard)
	}
	// The keyboard makes itself the seat's as soon as it has a keymap, and again
	// on Activate, by holding and letting go of Shift with no key.
	mods := f.find(v.keyboard, vkModifiers)
	if len(mods) != 2 || len(mods[0].args) != 4 || mods[0].args[0] != shiftMask || mods[1].args[0]|mods[1].args[1]|mods[1].args[2]|mods[1].args[3] != 0 {
		t.Errorf("modifiers after create = %+v, want Shift then nothing", mods)
	}
	if err := v.Activate(); err != nil {
		t.Fatal(err)
	}
	if mods := f.find(v.keyboard, vkModifiers); len(mods) != 4 {
		t.Errorf("%d modifiers requests after Activate, want 4", len(mods))
	}

	if err := v.MoveTo(100, 50, 1280, 800); err != nil {
		t.Fatal(err)
	}
	if err := v.Press("left"); err != nil {
		t.Fatal(err)
	}
	if err := v.Release("left"); err != nil {
		t.Fatal(err)
	}
	if err := v.Scroll("up", 2); err != nil {
		t.Fatal(err)
	}
	if err := v.Press("thumb"); err == nil {
		t.Error("pressed an unknown button")
	}
	if err := v.Sync(); err != nil {
		t.Fatal(err)
	}

	moves := f.find(v.pointer, vpMotionAbsolute)
	if len(moves) != 1 || moves[0].args[1] != 100 || moves[0].args[2] != 50 || moves[0].args[3] != 1280 || moves[0].args[4] != 800 {
		t.Errorf("motion_absolute = %+v", moves)
	}
	presses := f.find(v.pointer, vpButton)
	if len(presses) != 2 || presses[0].args[1] != 0x110 || presses[0].args[2] != buttonPressed || presses[1].args[2] != buttonReleased {
		t.Errorf("buttons = %+v", presses)
	}
	wheel := f.find(v.pointer, vpAxisDiscrete)
	if len(wheel) != 2 {
		t.Fatalf("axis_discrete = %+v", wheel)
	}
	if axis, value, discrete := wheel[0].args[1], int32(wheel[0].args[2]), int32(wheel[0].args[3]); axis != axisVertical || discrete != -1 || math.Abs(float64(value)/256+wheelStepDistance) > 0.01 {
		t.Errorf("scroll up = axis %d value %v discrete %d", axis, float64(value)/256, discrete)
	}
	if frames := f.find(v.pointer, vpFrame); len(frames) != 5 {
		t.Errorf("%d frames, want one after each motion, button and wheel click", len(frames))
	}
}

func TestVirtualInputReportsProtocolErrors(t *testing.T) {
	f := &fakeCompositor{errorOn: vpButton}
	v, err := OpenVirtualInput(startFakeCompositor(t, f), InputPlace{})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	_ = v.Press("left")
	deadline := time.Now().Add(2 * time.Second)
	for v.Err() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := v.Err(); err == nil || !strings.Contains(err.Error(), "test error") {
		t.Errorf("Err = %v, want the compositor's protocol error", err)
	}
	if err := v.MoveTo(1, 1, 10, 10); err == nil {
		t.Error("a dead connection accepted a request")
	}
}

func TestWireString(t *testing.T) {
	b := binary.NativeEndian.AppendUint32(nil, 8)
	b = append(b, "wl_seat\x00"...)
	b = binary.NativeEndian.AppendUint32(b, 7)
	s, rest := wireString(b)
	if s != "wl_seat" || len(rest) != 4 || binary.NativeEndian.Uint32(rest) != 7 {
		t.Errorf("wireString = %q, %v", s, rest)
	}
	if s, _ := wireString([]byte{9, 0, 0, 0, 'x'}); s != "" {
		t.Errorf("truncated string decoded as %q", s)
	}
}

func TestClipboard(t *testing.T) {
	f := &fakeCompositor{seats: []string{"seat0", "owner"}}
	c, err := OpenClipboard(startFakeCompositor(t, f), "seat0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	text := "line one\nline two: \u00e9 \u65e5\u672c \U0001F600"
	if err := c.SetText(text); err != nil {
		t.Fatal(err)
	}
	if err := c.SetText("second"); err != nil {
		t.Fatal(err)
	}
	// Each paste's pipe is written off the reading goroutine.
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		n := len(f.pasted)
		f.mu.Unlock()
		if n == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := c.w.roundtrip(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	pasted, sources := append([]string(nil), f.pasted...), append([]uint32(nil), f.sources...)
	f.mu.Unlock()
	// The fake reads each pipe on its own goroutine, so either may finish first.
	slices.Sort(pasted)
	want := []string{text, "second"}
	slices.Sort(want)
	if !slices.Equal(pasted, want) {
		t.Fatalf("pasted %q, want the two texts exactly, newlines kept", pasted)
	}
	if len(sources) != 2 {
		t.Fatalf("selections set from %d sources, want 2", len(sources))
	}
	// Offered as every text type, in order.
	offers := f.find(sources[0], dcSourceOffer)
	if len(offers) != len(textMimeTypes) {
		t.Errorf("%d offers, want %d", len(offers), len(textMimeTypes))
	}
	// The first source, cancelled when the second replaced it, is destroyed; the
	// second, still the selection, is not.
	if len(f.find(sources[0], dcSourceDestroy)) != 1 || len(f.find(sources[1], dcSourceDestroy)) != 0 {
		t.Errorf("destroyed: first %d, second %d; want 1 and 0",
			len(f.find(sources[0], dcSourceDestroy)), len(f.find(sources[1], dcSourceDestroy)))
	}
	// The offers the device was told about are not left in the compositor.
	if n := len(f.find(0xff000001, dcOfferDestroy)); n != 2 {
		t.Errorf("offer destroyed %d times, want once per selection", n)
	}
	// On the seat asked for.
	f.mu.Lock()
	var seat string
	for id, iface := range f.objects {
		if iface == "device" {
			for _, r := range f.requests {
				if r.opcode == dcManagerGetDevice && len(r.args) == 2 && r.args[0] == id {
					seat = f.names[r.args[1]]
				}
			}
		}
	}
	f.mu.Unlock()
	if seat != "seat0" {
		t.Errorf("device on seat %q, want seat0", seat)
	}
}
