package desktop

import (
	"context"
	"log/slog"
	"sync"
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
	return Launcher{Env: s.Env(), LogPath: logPath, Log: l.Log}.Start(command)
}
