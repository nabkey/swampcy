package desktop

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"os"
	"syscall"
)

// Capture copies the session's screen into shared memory through wlroots'
// screencopy protocol. copy_with_damage makes the compositor hold each copy until
// something on the screen has changed, and say which rectangles did, so an idle
// screen costs nothing.

const (
	// zwlr_screencopy_manager_v1
	scCaptureOutput = 0
	// zwlr_screencopy_frame_v1 requests
	scCopy           = 0
	scFrameDestroy   = 1
	scCopyWithDamage = 2
	// zwlr_screencopy_frame_v1 events
	scBuffer     = 0
	scFlags      = 1
	scReady      = 2
	scFailed     = 3
	scDamage     = 4
	scBufferDone = 6
	scYInvert    = 1
	// wl_shm, wl_shm_pool, wl_buffer
	shmCreatePool    = 0
	poolCreateBuffer = 0
	poolDestroy      = 1
	bufferDestroy    = 0
	// wl_shm formats whose bytes are B, G, R, X in memory.
	ShmARGB8888 = 0
	ShmXRGB8888 = 1
)

// Shot is one copy of the screen. Pix belongs to the Capture and is overwritten
// by the next call to Next.
type Shot struct {
	Pix                   []byte
	Width, Height, Stride int
	Format                uint32
	YInvert               bool
	// Damage is what changed since the previous shot; the first shot's is the whole
	// screen.
	Damage []image.Rectangle
}

type scEvent struct {
	op         uint16
	a, b, c, d uint32
}

// Capture is one screencopy client. It is not safe for concurrent use.
type Capture struct {
	w       *wlConn
	dir     string
	manager uint32
	version uint32
	output  uint32
	shm     uint32

	pool, buffer          uint32
	file                  *os.File
	mem                   []byte
	format, width, height uint32
	stride                uint32
	frame                 uint32 // the copy in flight, 0 when none
	events                chan scEvent
	shot                  Shot
	copied                bool
}

// OpenCapture connects to the session's compositor, to copy the output named
// output (with output empty, the first sway announces).
func OpenCapture(s Session, output string) (*Capture, error) {
	w, err := dialWayland(s.RuntimeDir, s.WaylandDisplay)
	if err != nil {
		return nil, err
	}
	c := &Capture{w: w, dir: s.RuntimeDir, events: make(chan scEvent, 256)}
	if c.manager, c.version, err = w.bindVersion("zwlr_screencopy_manager_v1", 3); err == nil {
		if c.output, err = w.bindNamed("wl_output", output, 1); err == nil {
			c.shm, err = w.bind("wl_shm", 1)
		}
	}
	if err == nil {
		err = w.roundtrip()
	}
	if err != nil {
		w.Close()
		return nil, err
	}
	if c.version < 2 {
		w.Close()
		return nil, errors.New("the compositor's screencopy is too old for damage tracking (version 1)")
	}
	return c, nil
}

// Err is why the capture stopped working, or nil.
func (c *Capture) Err() error { return c.w.Err() }

// Close ends the capture and frees its memory.
func (c *Capture) Close() {
	c.w.Close()
	c.unmap()
}

func (c *Capture) unmap() {
	if c.mem != nil {
		_ = syscall.Munmap(c.mem)
		c.mem = nil
	}
	if c.file != nil {
		c.file.Close()
		c.file = nil
	}
}

// Next returns the next copy of the screen: at once for the first, and after
// that only once something has changed. When ctx ends first, the copy stays in
// flight and the next call picks it up.
func (c *Capture) Next(ctx context.Context) (*Shot, error) {
	if c.frame == 0 {
		c.frame = c.w.alloc()
		frame := c.frame
		c.w.handle(frame, func(op uint16, body []byte) {
			var ev scEvent
			ev.op = op
			for i, p := range []*uint32{&ev.a, &ev.b, &ev.c, &ev.d} {
				if len(body) >= 4*(i+1) {
					*p = binary.NativeEndian.Uint32(body[4*i:])
				}
			}
			c.events <- ev
		})
		if err := c.w.send(c.manager, scCaptureOutput, nil, frame, int32(0), c.output); err != nil {
			return nil, err
		}
		c.shot.Damage = c.shot.Damage[:0]
		c.shot.YInvert = false
	}
	for {
		var ev scEvent
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.w.closed:
			return nil, c.w.Err()
		case ev = <-c.events:
		}
		switch ev.op {
		case scBuffer:
			c.format, c.width, c.height, c.stride = ev.a, ev.b, ev.c, ev.d
			if c.version < 3 { // no buffer_done: every buffer type is announced already
				if err := c.copy(); err != nil {
					return nil, err
				}
			}
		case scBufferDone:
			if err := c.copy(); err != nil {
				return nil, err
			}
		case scFlags:
			c.shot.YInvert = ev.a&scYInvert != 0
		case scDamage:
			c.shot.Damage = append(c.shot.Damage, image.Rect(int(ev.a), int(ev.b), int(ev.a+ev.c), int(ev.b+ev.d)))
		case scReady, scFailed:
			c.w.handle(c.frame, nil)
			_ = c.w.send(c.frame, scFrameDestroy, nil)
			c.frame = 0
			if ev.op == scFailed {
				return nil, errors.New("screencopy failed")
			}
			if !c.copied { // the first copy shows everything
				c.shot.Damage = append(c.shot.Damage[:0], image.Rect(0, 0, int(c.width), int(c.height)))
				c.copied = true
			}
			c.shot.Pix, c.shot.Width, c.shot.Height = c.mem, int(c.width), int(c.height)
			c.shot.Stride, c.shot.Format = int(c.stride), c.format
			return &c.shot, nil
		}
	}
}

// copy points the frame at a shared-memory buffer of the size and format the
// compositor asked for, making one when the old one does not fit.
func (c *Capture) copy() error {
	if c.format != ShmXRGB8888 && c.format != ShmARGB8888 {
		return fmt.Errorf("screencopy offers wl_shm format %#x; only XRGB8888 and ARGB8888 are handled", c.format)
	}
	size := int(c.stride * c.height)
	if c.buffer == 0 || len(c.mem) != size {
		if err := c.makeBuffer(size); err != nil {
			return err
		}
	}
	// The first copy is a plain one, which makes the compositor draw a frame: a
	// headless sway draws nothing while the screen is idle, and copy_with_damage
	// would wait for a change that may never come. After that, copies wait for
	// damage, so an idle screen costs nothing.
	op := uint16(scCopyWithDamage)
	if !c.copied {
		op = scCopy
	}
	return c.w.send(c.frame, op, nil, c.buffer)
}

func (c *Capture) makeBuffer(size int) error {
	if c.buffer != 0 {
		_ = c.w.send(c.buffer, bufferDestroy, nil)
		_ = c.w.send(c.pool, poolDestroy, nil)
		c.buffer, c.pool = 0, 0
	}
	c.unmap()
	f, err := os.CreateTemp(c.dir, "swampcy-shm-")
	if err != nil {
		return err
	}
	_ = os.Remove(f.Name()) // the descriptor is all anyone needs
	if err := f.Truncate(int64(size)); err != nil {
		f.Close()
		return err
	}
	mem, err := syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		return fmt.Errorf("mmap: %w", err)
	}
	c.file, c.mem = f, mem
	c.pool, c.buffer = c.w.alloc(), c.w.alloc()
	if err := c.w.send(c.shm, shmCreatePool, f, c.pool, int32(size)); err != nil {
		return err
	}
	return c.w.send(c.pool, poolCreateBuffer, nil, c.buffer, int32(0), int32(c.width), int32(c.height), int32(c.stride), c.format)
}
