package desktop

import (
	"cmp"
	"context"
	"log/slog"
	"os"
	"sync"
	"syscall"
	"time"
)

// Live finds the session afresh whenever the one it knows stops answering, and
// acts through it. swampcy is started by the agent, not by the session, so sway
// may start after swampcy does, or restart under it.
type Live struct {
	Getenv    func(string) string
	JPEG      bool
	TypeDelay time.Duration
	AppLog    string // where launched apps' output goes; "" for the runtime dir
	Log       *slog.Logger
	// Seat is the seat the pointer and keyboard join, and Output the screen this
	// swampcy sees and points at: empty for the first seat and the top-left output.
	Seat   string
	Output string

	mu   sync.Mutex
	sess *Session

	inMu    sync.Mutex
	in      *VirtualInput
	inSess  Session
	inPlace InputPlace

	clipMu   sync.Mutex
	clip     *Clipboard
	clipSess Session

	capMu   sync.Mutex
	cap     *Capture
	capSess Session
	capOut  string

	exitMu sync.Mutex
	exits  map[int]Exit
}

// Exit is how a launched command's shell ended, and what it had written.
type Exit struct {
	Err    error
	Output string
}

// Session returns the current session, discovering it if needed.
func (l *Live) Session() (Session, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sess != nil {
		if _, err := (&Sway{Socket: l.sess.SwaySock}).Version(); err == nil {
			return *l.sess, nil
		}
		l.sess = nil
	}
	s, err := Discover(l.Getenv)
	if err != nil {
		return Session{}, err
	}
	if l.Log != nil {
		l.Log.Info("session", "swaysock", s.SwaySock, "wayland_display", s.WaylandDisplay)
	}
	l.sess = &s
	return s, nil
}

func (l *Live) sway() (*Sway, error) {
	s, err := l.Session()
	if err != nil {
		return nil, err
	}
	return &Sway{Socket: s.SwaySock}, nil
}

func (l *Live) Tree() (*Node, error) {
	sw, err := l.sway()
	if err != nil {
		return nil, err
	}
	return sw.Tree()
}

func (l *Live) Outputs() ([]Output, error) {
	sw, err := l.sway()
	if err != nil {
		return nil, err
	}
	return sw.Outputs()
}

func (l *Live) Seats() ([]Seat, error) {
	sw, err := l.sway()
	if err != nil {
		return nil, err
	}
	return sw.Seats()
}

func (l *Live) Command(cmd string) error {
	sw, err := l.sway()
	if err != nil {
		return err
	}
	return sw.Command(cmd)
}

// Screen is the output this swampcy sees and points at.
func (l *Live) Screen() (Output, error) {
	outs, err := l.Outputs()
	if err != nil {
		return Output{}, err
	}
	return PickOutput(outs, l.Output)
}

// Input returns the session's virtual pointer and keyboard, on l's seat with the
// pointer tied to l's output, creating them when the session is new, the old ones
// died with it, or the output is another. Newly created devices get a moment for
// apps to bind them before anything is sent.
func (l *Live) Input() (*VirtualInput, error) {
	s, err := l.Session()
	if err != nil {
		return nil, err
	}
	out, err := l.Screen()
	if err != nil {
		return nil, err
	}
	place := InputPlace{Seat: l.Seat, Output: out.Name}
	l.inMu.Lock()
	defer l.inMu.Unlock()
	if l.in != nil && l.in.Err() == nil && l.inSess == s && l.inPlace == place {
		return l.in, nil
	}
	if l.in != nil {
		l.in.Close()
	}
	in, err := OpenVirtualInput(s, place)
	if err != nil {
		l.in = nil
		return nil, err
	}
	l.in, l.inSess, l.inPlace = in, s, place
	if l.Log != nil {
		l.Log.Info("virtual pointer and keyboard created", "seat", place.Seat, "output", place.Output)
	}
	time.Sleep(200 * time.Millisecond)
	return in, nil
}

// MoveTo puts the pointer at (x, y) in layout coordinates, which must be on l's
// output: the pointer is tied to it, and its motion is relative to it.
func (l *Live) MoveTo(x, y int) error {
	in, err := l.Input()
	if err != nil {
		return err
	}
	out, err := l.Screen()
	if err != nil {
		return err
	}
	r := out.Rect
	return in.MoveTo(min(max(x-r.X, 0), r.Width-1), min(max(y-r.Y, 0), r.Height-1), r.Width, r.Height)
}

func (l *Live) Press(button string) error {
	in, err := l.Input()
	if err != nil {
		return err
	}
	return in.Press(button)
}

func (l *Live) Release(button string) error {
	in, err := l.Input()
	if err != nil {
		return err
	}
	return in.Release(button)
}

func (l *Live) Scroll(direction string, clicks int) error {
	in, err := l.Input()
	if err != nil {
		return err
	}
	return in.Scroll(direction, clicks)
}

func (l *Live) Capture(ctx context.Context, region *Rect, scale float64) ([]byte, string, error) {
	s, err := l.Session()
	if err != nil {
		return nil, "", err
	}
	return Grim{Env: s.Env(), JPEG: l.JPEG}.Capture(ctx, region, scale)
}

func (l *Live) Type(ctx context.Context, text string) error {
	in, err := l.Input()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return in.TypeText(text, l.TypeDelay)
}

func (l *Live) Keys(ctx context.Context, combos []Combo) error {
	in, err := l.Input()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return in.Keys(combos, l.TypeDelay)
}

// Launch starts an app on the session's display.
func (l *Live) Launch(command string) (int, error) {
	s, err := l.Session()
	if err != nil {
		return 0, err
	}
	logPath := l.AppLog
	if logPath == "" {
		logPath = s.RuntimeDir + "/swampcy/apps.log"
	}
	return Launcher{Env: s.Env(), LogPath: logPath, Log: l.Log, Exited: func(pid int, err error, output string) {
		l.exitMu.Lock()
		defer l.exitMu.Unlock()
		if l.exits == nil || len(l.exits) > 256 {
			l.exits = map[int]Exit{}
		}
		l.exits[pid] = Exit{Err: err, Output: output}
	}}.Start(command)
}

// Exited reports whether the shell of a command Launch started has exited, and how.
func (l *Live) Exited(pid int) (Exit, bool) {
	l.exitMu.Lock()
	defer l.exitMu.Unlock()
	e, ok := l.exits[pid]
	return e, ok
}

func (l *Live) HoldMods(ctx context.Context, mods []string, down bool) error {
	in, err := l.Input()
	if err != nil {
		return err
	}
	return in.HoldMods(mods, down)
}

func (l *Live) HoldKey(ctx context.Context, c Combo, d time.Duration) error {
	in, err := l.Input()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return in.HoldKey(c, d)
}

// clipboard is the data-control device on l's seat, opened when first needed and
// again when the session is new. A selection set through it lasts as long as it does.
func (l *Live) clipboard() (*Clipboard, error) {
	s, err := l.Session()
	if err != nil {
		return nil, err
	}
	l.clipMu.Lock()
	defer l.clipMu.Unlock()
	if l.clip != nil && l.clip.Err() == nil && l.clipSess == s {
		return l.clip, nil
	}
	if l.clip != nil {
		l.clip.Close()
	}
	c, err := OpenClipboard(s, l.Seat)
	if err != nil {
		l.clip = nil
		return nil, err
	}
	l.clip, l.clipSess = c, s
	return c, nil
}

// ClipboardText reads the seat's clipboard.
func (l *Live) ClipboardText() (string, error) {
	c, err := l.clipboard()
	if err != nil {
		return "", err
	}
	return c.Text()
}

// SetClipboardText puts text on the seat's clipboard.
func (l *Live) SetClipboardText(text string) error {
	c, err := l.clipboard()
	if err != nil {
		return err
	}
	return c.SetText(text)
}

// Settle waits until l's screen has gone quiet without a change, or most has
// passed: screencopy reports each change as it is drawn, so a screen that an
// action left redrawing is waited out, and one that did not change costs quiet.
// It reports whether the screen went quiet.
func (l *Live) Settle(ctx context.Context, quiet, most time.Duration) bool {
	deadline := time.Now().Add(most)
	s, err := l.Session()
	if err != nil {
		return false
	}
	out, err := l.Screen()
	if err != nil {
		return false
	}
	l.capMu.Lock()
	defer l.capMu.Unlock()
	if l.cap == nil || l.cap.Err() != nil || l.capSess != s || l.capOut != out.Name {
		if l.cap != nil {
			l.cap.Close()
		}
		if l.cap, err = OpenCapture(s, out.Name); err != nil {
			l.cap = nil
			return false
		}
		l.capSess, l.capOut = s, out.Name
	}
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return false
		}
		cctx, cancel := context.WithTimeout(ctx, min(quiet, left))
		_, err := l.cap.Next(cctx)
		cancel()
		switch {
		case err == nil:
			continue // something changed: wait for quiet again
		case ctx.Err() != nil:
			return false
		case l.cap.Err() != nil:
			l.cap.Close()
			l.cap = nil
			return false
		default:
			return left >= quiet // quiet passed without a change
		}
	}
}

// LockInput takes the seat's input lock, shared by every swampcy on the session,
// so that two agents' clicks and keys do not interleave. It gives up after ten
// seconds and goes ahead: a lock is not worth a hung tool.
func (l *Live) LockInput() func() {
	s, err := l.Session()
	if err != nil {
		return nil
	}
	dir := s.RuntimeDir + "/swampcy"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil
	}
	f, err := os.OpenFile(dir+"/input-"+cmp.Or(l.Seat, "seat")+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil
	}
	for deadline := time.Now().Add(10 * time.Second); ; {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return func() { f.Close() } // closing releases it
		}
		if time.Now().After(deadline) {
			if l.Log != nil {
				l.Log.Warn("input lock held by another swampcy for 10s; going ahead without it")
			}
			f.Close()
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}
