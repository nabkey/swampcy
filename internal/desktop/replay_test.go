package desktop

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func newReplayDesk(t *testing.T) (*fakeDesk, *Desktop, *[]int) {
	t.Helper()
	f, _, d := newDesk(true)
	var encoded []int
	d.Replay = &Replay{Screen: f, Dir: filepath.Join(t.TempDir(), "media"), Width: 640, Size: 5, Keep: 7 * 24 * time.Hour,
		Encode: func(_ context.Context, frames []Frame, fps int, out string) (string, error) {
			encoded = append(encoded, len(frames))
			return "webp", os.WriteFile(out, []byte("RIFF....WEBP"), 0o644)
		}}
	return f, d, &encoded
}

func TestReplayCapturesAfterActingTools(t *testing.T) {
	f, d, encoded := newReplayDesk(t)
	cs := connect(t, d)
	call(t, cs, "screenshot", nil)
	call(t, cs, "move", map[string]any{"x": 1, "y": 1})
	if n := len(d.Replay.Recent(false)); n != 0 {
		t.Fatalf("read-only tools captured %d frames", n)
	}
	call(t, cs, "click", map[string]any{"x": 10, "y": 10})
	call(t, cs, "type", map[string]any{"text": "secret"})
	call(t, cs, "key", map[string]any{"keys": "Return"})
	if res := call(t, cs, "click", map[string]any{"x": 99999, "y": 0}); !res.IsError {
		t.Fatal("clicked off screen")
	}
	d.Replay.Flush()
	// One frame before the first action, one after each that succeeded.
	if n := len(d.Replay.Recent(false)); n != 4 {
		t.Errorf("frames = %d, want 4", n)
	}
	for _, s := range f.shots[1:] {
		if s != "&{0 0 2560 1600} 0.25" {
			t.Errorf("frame capture = %q, want the output at 640 wide", s)
		}
	}

	res := call(t, cs, "replay", map[string]any{"name": "job-42"})
	if res.IsError {
		t.Fatal(textOf(res))
	}
	want := filepath.Join(d.Replay.Dir, "job-42.webp")
	if got := textOf(res); !strings.Contains(got, want) || !strings.Contains(got, "4 frames") || !strings.Contains(got, "7 days") {
		t.Errorf("result = %q, want the path %s", got, want)
	}
	if fi, err := os.Stat(want); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("clip = %v, %v; want mode 0600", fi, err)
	}
	if fi, err := os.Stat(d.Replay.Dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("dir = %v, %v; want mode 0700", fi, err)
	}
	if !equalInts(*encoded, []int{4}) {
		t.Errorf("encoded = %v", *encoded)
	}
	if n := len(d.Replay.Recent(false)); n != 0 {
		t.Errorf("ring holds %d frames after a replay, want it cleared", n)
	}
	if res := call(t, cs, "replay", nil); !res.IsError {
		t.Error("replayed with no frames")
	}
}

func TestReplayRefusesBadArguments(t *testing.T) {
	_, d, _ := newReplayDesk(t)
	cs := connect(t, d)
	call(t, cs, "key", map[string]any{"keys": "a"})
	for _, args := range []map[string]any{
		{"name": "../etc/passwd"}, {"name": "a.webp"}, {"name": strings.Repeat("a", 65)}, {"fps": 10}, {"fps": 1},
	} {
		if res := call(t, cs, "replay", args); !res.IsError {
			t.Errorf("replay %v succeeded", args)
		}
	}
	ents, _ := os.ReadDir(d.Replay.Dir)
	if len(ents) != 0 {
		t.Errorf("dir holds %v after refusals", ents)
	}
}

func TestReplayRingIsBounded(t *testing.T) {
	_, d, _ := newReplayDesk(t)
	cs := connect(t, d)
	for range 9 {
		call(t, cs, "scroll", map[string]any{"x": 1, "y": 1, "direction": "down"})
	}
	d.Replay.Flush()
	if n := len(d.Replay.Recent(false)); n != 5 {
		t.Errorf("frames = %d, want the ring's 5", n)
	}
}

func TestReplayDropsOldFrames(t *testing.T) {
	r := &Replay{frames: []Frame{{At: time.Now().Add(-time.Hour)}, {At: time.Now()}}}
	if n := len(r.Recent(true)); n != 1 {
		t.Errorf("recent = %d, want 1", n)
	}
}

func TestReplayExpiresOnlyOldClips(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-8 * 24 * time.Hour)
	files := map[string]bool{ // name: should survive
		"old.webp": false, "old.gif": false, "new.webp": true, "notes.txt": true, "old.png": true,
	}
	for name := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(name, "old") || name == "notes.txt" {
			_ = os.Chtimes(p, old, old)
		}
	}
	r := &Replay{Dir: dir, Keep: 7 * 24 * time.Hour}
	n, err := r.Expire()
	if err != nil || n != 2 {
		t.Errorf("expired %d, %v; want 2", n, err)
	}
	for name, survive := range files {
		_, err := os.Stat(filepath.Join(dir, name))
		if (err == nil) != survive {
			t.Errorf("%s: exists=%v, want %v", name, err == nil, survive)
		}
	}
}

func TestEncodeClip(t *testing.T) {
	if _, err := exec.LookPath("img2webp"); err != nil {
		if _, err := exec.LookPath("ffmpeg"); err != nil {
			t.Skip("neither img2webp nor ffmpeg is installed")
		}
	}
	var frames []Frame
	for i := range 4 {
		img := image.NewRGBA(image.Rect(0, 0, 64, 40))
		for x := range 64 {
			for y := range 40 {
				img.Set(x, y, color.RGBA{uint8(i * 60), uint8(x * 4), uint8(y * 6), 255})
			}
		}
		var b bytes.Buffer
		if err := png.Encode(&b, img); err != nil {
			t.Fatal(err)
		}
		frames = append(frames, Frame{Data: b.Bytes(), MIME: "image/png", At: time.Now()})
	}
	out := filepath.Join(t.TempDir(), "clip")
	ext, err := EncodeClip(context.Background(), frames, 3, out)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	switch ext {
	case "webp":
		if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
			t.Errorf("not a WebP: % x", data[:min(len(data), 16)])
		}
	case "gif":
		if !bytes.HasPrefix(data, []byte("GIF89a")) {
			t.Errorf("not a GIF: % x", data[:min(len(data), 16)])
		}
	default:
		t.Errorf("ext = %q", ext)
	}
	if ents, _ := os.ReadDir(filepath.Dir(out)); len(ents) != 1 {
		t.Errorf("left behind %v", ents)
	}
}

func equalInts(a, b []int) bool {
	return slices.Equal(a, b)
}
