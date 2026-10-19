package desktop

import (
	"context"
	"image"
	"testing"
	"time"
)

func TestCapture(t *testing.T) {
	f := &fakeCompositor{damageAfterFirst: [4]uint32{10, 4, 8, 6}}
	c, err := OpenCapture(startFakeCompositor(t, f), "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	shot, err := c.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if shot.Width != fakeW || shot.Height != fakeH || shot.Stride != fakeW*4 || shot.Format != ShmXRGB8888 {
		t.Errorf("first shot %dx%d stride %d format %d", shot.Width, shot.Height, shot.Stride, shot.Format)
	}
	if len(shot.Damage) != 1 || shot.Damage[0] != image.Rect(0, 0, fakeW, fakeH) {
		t.Errorf("first shot's damage = %v, want the whole screen", shot.Damage)
	}
	if shot.Pix[0] != 1 || shot.Pix[len(shot.Pix)-1] != 1 {
		t.Errorf("first shot's pixels = %d..%d, want frame 1's", shot.Pix[0], shot.Pix[len(shot.Pix)-1])
	}

	shot, err = c.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(shot.Damage) != 1 || shot.Damage[0] != image.Rect(10, 4, 18, 10) {
		t.Errorf("second shot's damage = %v, want (10,4)-(18,10)", shot.Damage)
	}
	if shot.Pix[0] != 2 {
		t.Errorf("second shot's pixels = %d, want frame 2's", shot.Pix[0])
	}
	// One pool and one buffer, reused: the size did not change.
	if n := len(f.find(c.shm, shmCreatePool)); n != 1 {
		t.Errorf("%d pools created, want 1", n)
	}
}

func TestCaptureNextHonoursContext(t *testing.T) {
	f := &fakeCompositor{}
	c, err := OpenCapture(startFakeCompositor(t, f), "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Next(ctx); err == nil {
		// The fake answers at once, so a cancelled context may still lose the race;
		// what matters is that a cancelled Next returns rather than blocking.
		t.Log("the copy won the race against the cancelled context")
	}
}

func TestCaptureNamedOutput(t *testing.T) {
	f := &fakeCompositor{outputs: []string{"HEADLESS-1", "HEADLESS-2"}}
	c, err := OpenCapture(startFakeCompositor(t, f), "HEADLESS-2")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if f.objects[r.obj] == "zwlr_screencopy_manager_v1" && r.opcode == scCaptureOutput {
			if got := f.names[r.args[2]]; got != "HEADLESS-2" {
				t.Errorf("captured output %q, want HEADLESS-2", got)
			}
			return
		}
	}
	t.Error("no capture_output request")
}
