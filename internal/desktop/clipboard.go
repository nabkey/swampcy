package desktop

import (
	"encoding/binary"
	"errors"
	"sync"
)

// Clipboard owns a seat's selection, the clipboard Ctrl+V pastes from, through
// wlroots' data-control protocol, which lets a client with no window set it. swampcy
// web puts a viewer's pasted text there and then presses Ctrl+V for them.
//
// A Wayland selection is an offer, not a copy: the text stays with the client that
// set it, and each app that pastes gets a pipe to read it from. So the selection
// lasts as long as this connection does, or until something else on the seat sets
// another.

const (
	// zwlr_data_control_manager_v1
	dcManagerCreateSource = 0
	dcManagerGetDevice    = 1
	// zwlr_data_control_device_v1
	dcDeviceSetSelection = 0
	dcDeviceDataOffer    = 0 // event
	dcDeviceSelection    = 1 // event
	dcDeviceFinished     = 2 // event
	// zwlr_data_control_source_v1
	dcSourceOffer     = 0
	dcSourceDestroy   = 1
	dcSourceSend      = 0 // event
	dcSourceCancelled = 1 // event
	// zwlr_data_control_offer_v1
	dcOfferDestroy = 1
)

// textMimeTypes are what a text selection is offered as: the MIME types GTK, Qt and
// Chromium ask for, and the X11 targets XWayland apps do.
var textMimeTypes = []string{"text/plain;charset=utf-8", "text/plain", "UTF8_STRING", "STRING", "TEXT"}

type Clipboard struct {
	w      *wlConn
	mgr    uint32
	device uint32

	mu  sync.Mutex
	err error // the device went away (finished)
}

// OpenClipboard connects to the session and takes a data-control device on the
// named seat (empty for the only one, or the first).
func OpenClipboard(s Session, seat string) (*Clipboard, error) {
	w, err := dialWayland(s.RuntimeDir, s.WaylandDisplay)
	if err != nil {
		return nil, err
	}
	c := &Clipboard{w: w}
	if err := c.open(seat); err != nil {
		w.Close()
		return nil, err
	}
	return c, nil
}

func (c *Clipboard) open(seat string) error {
	w := c.w
	seatID, err := w.bindNamed("wl_seat", seat, 1)
	if err != nil {
		return err
	}
	if c.mgr, err = w.bind("zwlr_data_control_manager_v1", 1); err != nil {
		return err
	}
	c.device = w.alloc()
	// The device tells this client about every selection on the seat, each as a new
	// offer object. None is read here, so each is destroyed once the selection
	// event names it, rather than left to pile up in the compositor.
	w.handle(c.device, func(op uint16, body []byte) {
		switch op {
		case dcDeviceSelection:
			if len(body) >= 4 {
				if offer := binary.NativeEndian.Uint32(body); offer != 0 {
					_ = w.send(offer, dcOfferDestroy, nil)
				}
			}
		case dcDeviceFinished:
			c.mu.Lock()
			c.err = errors.New("clipboard: the compositor withdrew the data-control device")
			c.mu.Unlock()
		}
	})
	if err := w.send(c.mgr, dcManagerGetDevice, nil, c.device, seatID); err != nil {
		return err
	}
	return w.roundtrip()
}

// SetText makes text the seat's selection. It returns once the compositor has it,
// so a Ctrl+V sent afterwards pastes it.
func (c *Clipboard) SetText(text string) error {
	if err := c.Err(); err != nil {
		return err
	}
	w := c.w
	data := []byte(text)
	src := w.alloc()
	w.handle(src, func(op uint16, body []byte) {
		switch op {
		case dcSourceSend:
			// An app is pasting: write the text into the pipe it sent and close it.
			// Off the reading goroutine, since a reader that is slow to drain the
			// pipe would otherwise hold up every other event.
			if f := w.takeFD(); f != nil {
				go func() {
					_, _ = f.Write(data)
					f.Close()
				}()
			}
		case dcSourceCancelled:
			// Something else set the selection: this text is no longer offered.
			w.handle(src, nil)
			_ = w.send(src, dcSourceDestroy, nil)
		}
	})
	if err := w.send(c.mgr, dcManagerCreateSource, nil, src); err != nil {
		return err
	}
	for _, m := range textMimeTypes {
		if err := w.send(src, dcSourceOffer, nil, m); err != nil {
			return err
		}
	}
	if err := w.send(c.device, dcDeviceSetSelection, nil, src); err != nil {
		return err
	}
	return w.roundtrip()
}

// Err is why the clipboard stopped working (the session or its device went away),
// or nil.
func (c *Clipboard) Err() error {
	if err := c.w.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close gives up the selection along with the connection.
func (c *Clipboard) Close() { c.w.Close() }
