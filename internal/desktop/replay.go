package desktop

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Replays: after each acting tool, swampcy keeps a small frame of its
// screen in a ring in memory, and the replay tool encodes the recent frames into
// a short animated WebP (or GIF) on disk, for a chat to show what the agent did.
// The frames can show signed-in pages, so the directory is 0700, the clips 0600,
// nothing is logged but counts, and clips are deleted after Keep.

// Frame is one capture of the screen after an action.
type Frame struct {
	Data []byte
	MIME string
	At   time.Time
}

// Replay is a ring of recent frames and what encodes them.
type Replay struct {
	Screen Screen
	Dir    string        // where clips go
	Width  int           // frames' width in pixels, at most the screen's
	Size   int           // frames held; older ones are dropped
	Keep   time.Duration // clips older than this are deleted
	Settle time.Duration // wait after an action before capturing it
	Encode Encoder
	Log    *slog.Logger

	mu     sync.Mutex
	frames []Frame
	queue  chan capture
	once   sync.Once
	queued int // captures queued and not yet taken, under mu
}

type capture struct {
	at     time.Time
	region Rect
	scale  float64
}

// Encoder writes frames, all one format, to an animated image at out. It returns
// the extension the file should have ("webp" or "gif").
type Encoder func(ctx context.Context, frames []Frame, fps int, out string) (string, error)

const (
	// replayMaxAge is how old the oldest frame in a clip may be: frames left from
	// an earlier task that never asked for its replay are not shown with the next.
	replayMaxAge = 30 * time.Minute
	// replayBeforeGap is how long without a frame before an action also captures
	// the screen as it was before it, so a clip starts with the scene.
	replayBeforeGap = time.Minute
)

// actingTools are the tools after which a frame is captured.
var actingTools = map[string]bool{
	"click": true, "click_element": true, "key": true, "type": true,
	"scroll": true, "launch": true, "drag": true, "set_text": true,
}

// clipName is what a clip's name must look like: it becomes a file name, and a
// server that hands clips out can safely serve only names of this shape.
var clipName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// clipFile matches the clips this package writes, and only those are expired.
var clipFile = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}\.(webp|gif)$`)

func (r *Replay) start() {
	r.once.Do(func() {
		r.queue = make(chan capture, 16)
		go r.run()
	})
}

func (r *Replay) run() {
	for c := range r.queue {
		if d := time.Until(c.at.Add(r.Settle)); d > 0 {
			time.Sleep(d)
		}
		r.grab(c)
		r.mu.Lock()
		r.queued--
		r.mu.Unlock()
	}
}

func (r *Replay) grab(c capture) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, mime, err := r.Screen.Capture(ctx, &c.region, c.scale)
	if err != nil {
		if r.Log != nil {
			r.Log.Warn("replay: capture", "err", err)
		}
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames = append(r.frames, Frame{Data: data, MIME: mime, At: time.Now()})
	if n := len(r.frames) - max(r.Size, 1); n > 0 {
		r.frames = append(r.frames[:0:0], r.frames[n:]...)
	}
}

// frameScale is the capture scale that makes the output Width pixels wide.
func (r *Replay) frameScale(g Geometry) float64 {
	if r.Width <= 0 || g.Layout.Width <= r.Width {
		return 1
	}
	return float64(r.Width) / float64(g.Layout.Width)
}

// Before captures the screen as it is, before an action, when the ring has had
// no frame for a while. It blocks, so the frame is taken before the action.
func (r *Replay) Before(g Geometry) {
	r.mu.Lock()
	stale := len(r.frames) == 0 || time.Since(r.frames[len(r.frames)-1].At) > replayBeforeGap
	r.mu.Unlock()
	if stale {
		r.grab(capture{region: g.Layout, scale: r.frameScale(g)})
	}
}

// After queues a capture of the screen once the action has had time to show.
// It never blocks: when the queue is full the frame is dropped.
func (r *Replay) After(g Geometry) {
	r.start()
	r.mu.Lock()
	r.queued++
	r.mu.Unlock()
	select {
	case r.queue <- capture{at: time.Now(), region: g.Layout, scale: r.frameScale(g)}:
	default:
		r.mu.Lock()
		r.queued--
		r.mu.Unlock()
	}
}

// Flush waits, for a few seconds at most, for queued captures to be taken.
func (r *Replay) Flush() {
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		n := r.queued
		r.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Recent returns the frames no older than replayMaxAge, and with clear empties
// the ring.
func (r *Replay) Recent(clear bool) []Frame {
	r.mu.Lock()
	defer r.mu.Unlock()
	cut := time.Now().Add(-replayMaxAge)
	var out []Frame
	for _, f := range r.frames {
		if f.At.After(cut) {
			out = append(out, f)
		}
	}
	if clear {
		r.frames = nil
	}
	return out
}

// PrepareDir creates the clip directory, or narrows an existing one, to 0700.
func (r *Replay) PrepareDir() error {
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(r.Dir, 0o700)
}

// Expire deletes clips, and leftover frame directories, older than Keep. It
// touches nothing in the directory that this package did not write.
func (r *Replay) Expire() (int, error) {
	ents, err := os.ReadDir(r.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range ents {
		info, err := e.Info()
		if err != nil {
			continue
		}
		age := time.Since(info.ModTime())
		switch {
		case e.Type().IsRegular() && clipFile.MatchString(e.Name()) && age > r.Keep:
			if os.Remove(filepath.Join(r.Dir, e.Name())) == nil {
				n++
			}
		case e.IsDir() && strings.HasPrefix(e.Name(), ".frames-") && age > time.Hour:
			_ = os.RemoveAll(filepath.Join(r.Dir, e.Name()))
		}
	}
	return n, nil
}

// Save encodes frames to a clip named name (a random one when empty) and
// returns its path.
func (r *Replay) Save(ctx context.Context, frames []Frame, name string, fps int) (string, error) {
	if name == "" {
		var b [6]byte
		_, _ = rand.Read(b[:])
		name = "swampcy-" + time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
	}
	if !clipName.MatchString(name) {
		return "", fmt.Errorf("name must be 1 to 64 letters, digits, - and _")
	}
	if len(frames) == 0 {
		return "", errors.New("no frames to replay: nothing has been done on the screen in the last 30 minutes")
	}
	if err := r.PrepareDir(); err != nil {
		return "", err
	}
	if _, err := r.Expire(); err != nil && r.Log != nil {
		r.Log.Warn("replay: expire", "err", err)
	}
	tmp, err := os.CreateTemp(r.Dir, ".clip-*")
	if err != nil {
		return "", err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	ext, err := r.Encode(ctx, frames, fps, tmp.Name())
	if err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return "", err
	}
	out := filepath.Join(r.Dir, name+"."+ext)
	if err := os.Rename(tmp.Name(), out); err != nil {
		return "", err
	}
	return out, nil
}

// replayHold is how many extra copies of the last frame end a clip, so it
// rests on the result before looping.
const replayHold = 3

// EncodeClip encodes with libwebp's img2webp when it is installed, else
// ffmpeg's animated WebP encoder, else ffmpeg's GIF encoder.
func EncodeClip(ctx context.Context, frames []Frame, fps int, out string) (string, error) {
	if len(frames) == 0 {
		return "", errors.New("no frames")
	}
	dir, err := os.MkdirTemp(filepath.Dir(out), ".frames-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	ext := ".png"
	if frames[0].MIME == "image/jpeg" {
		ext = ".jpg"
	}
	var files []string
	for i := range len(frames) + replayHold {
		f := frames[min(i, len(frames)-1)]
		p := filepath.Join(dir, fmt.Sprintf("f%04d%s", i, ext))
		if err := os.WriteFile(p, f.Data, 0o600); err != nil {
			return "", err
		}
		files = append(files, p)
	}
	if p, err := exec.LookPath("img2webp"); err == nil {
		args := []string{"-loop", "0", "-lossy", "-q", "60", "-m", "4", "-d", strconv.Itoa(1000 / fps)}
		args = append(args, files...)
		return "webp", run(ctx, p, append(args, "-o", out)...)
	}
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", errors.New("neither img2webp nor ffmpeg is installed")
	}
	in := []string{"-hide_banner", "-loglevel", "error", "-y", "-framerate", strconv.Itoa(fps), "-i", filepath.Join(dir, "f%04d"+ext)}
	webp := slices.Concat(in, []string{"-c:v", "libwebp_anim", "-lossless", "0", "-quality", "60", "-loop", "0", "-an", "-f", "webp", out})
	err = run(ctx, ff, webp...)
	if err == nil {
		return "webp", nil
	}
	gif := slices.Concat(in, []string{"-vf", "split[a][b];[a]palettegen=max_colors=128:stats_mode=diff[p];[b][p]paletteuse=dither=bayer:diff_mode=rectangle",
		"-loop", "0", "-f", "gif", out})
	if gerr := run(ctx, ff, gif...); gerr != nil {
		return "", fmt.Errorf("webp: %v; gif: %w", err, gerr)
	}
	return "gif", nil
}

func run(ctx context.Context, path string, args ...string) error {
	cmd := exec.CommandContext(ctx, path, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return fmt.Errorf("%s: %w: %s", filepath.Base(path), err, msg)
	}
	return nil
}
