package desktop

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// fakeSway answers sway IPC requests on a Unix socket from a table of replies,
// recording the commands it is sent.
type fakeSway struct {
	sock     string
	replies  map[uint32]string
	commands []string
}

func newFakeSway(t *testing.T, replies map[uint32]string) *fakeSway {
	t.Helper()
	// Unix socket paths are short; t.TempDir() can exceed the limit on macOS.
	dir, err := os.MkdirTemp("", "swampcy")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &fakeSway{sock: filepath.Join(dir, "s.sock"), replies: replies}
	ln, err := net.Listen("unix", f.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			typ, body, err := decodeIPC(c)
			if err == nil {
				if typ == ipcRunCommand {
					f.commands = append(f.commands, string(body))
				}
				reply, ok := f.replies[typ]
				if !ok && typ == ipcRunCommand {
					reply = `[{"success":true}]`
				}
				c.Write(encodeIPC(typ, []byte(reply)))
			}
			c.Close()
		}
	}()
	return f
}

const testTree = `{"id":1,"type":"root","name":"root","rect":{"x":0,"y":0,"width":1280,"height":800},"nodes":[
 {"id":3,"type":"output","name":"HEADLESS-1","rect":{"x":0,"y":0,"width":1280,"height":800},"nodes":[
  {"id":4,"type":"workspace","name":"1","rect":{"x":0,"y":0,"width":1280,"height":800},"nodes":[
   {"id":7,"type":"con","name":"Calculator","app_id":"org.gnome.Calculator","pid":4242,"focused":true,
    "rect":{"x":0,"y":0,"width":640,"height":800},"window_rect":{"x":2,"y":24,"width":636,"height":774},"nodes":[]},
   {"id":8,"type":"con","name":"xterm","pid":5151,"window_properties":{"class":"XTerm"},
    "rect":{"x":640,"y":0,"width":640,"height":800},"nodes":[]}
  ],"floating_nodes":[
   {"id":9,"type":"floating_con","name":"Dialog","app_id":"","pid":4242,"rect":{"x":400,"y":300,"width":300,"height":200},"nodes":[]}
  ]}]}]}`

const testOutputs = `[{"name":"HEADLESS-1","active":true,"rect":{"x":0,"y":0,"width":1280,"height":800},"scale":1},
 {"name":"HEADLESS-2","active":false,"rect":{"x":1280,"y":0,"width":1920,"height":1080},"scale":1}]`

func TestSwayIPC(t *testing.T) {
	f := newFakeSway(t, map[uint32]string{
		ipcGetTree:    testTree,
		ipcGetOutputs: testOutputs,
		ipcGetVersion: `{"human_readable":"1.11"}`,
	})
	s := &Sway{Socket: f.sock}

	if v, err := s.Version(); err != nil || v != "1.11" {
		t.Fatalf("Version = %q, %v", v, err)
	}
	tree, err := s.Tree()
	if err != nil {
		t.Fatal(err)
	}
	ws := tree.Windows()
	if len(ws) != 3 {
		t.Fatalf("got %d windows, want 3", len(ws))
	}
	if ws[0].App() != "org.gnome.Calculator" || ws[1].App() != "XTerm" || !ws[2].Floating || ws[2].Workspace != "1" {
		t.Errorf("windows = %+v %+v %+v", ws[0], ws[1], ws[2])
	}
	if f := tree.FocusedWindow(); f == nil || f.ID != 7 {
		t.Errorf("focused = %v, want 7", f)
	}
	if got, want := tree.Find(7).Content(), (Rect{X: 2, Y: 24, Width: 636, Height: 774}); got != want {
		t.Errorf("Content = %+v, want %+v", got, want)
	}
	if got := tree.Find(8).Content(); got != tree.Find(8).Rect {
		t.Errorf("Content without window_rect = %+v, want rect", got)
	}

	outs, err := s.Outputs()
	if err != nil || len(outs) != 1 {
		t.Fatalf("Outputs = %v, %v; want only the active one", outs, err)
	}

	if err := s.Command("[con_id=7] focus"); err != nil {
		t.Fatal(err)
	}
	f.replies[ipcRunCommand] = `[{"success":true},{"success":false,"error":"Unknown command"}]`
	if err := s.Command("a; b"); err == nil {
		t.Error("Command succeeded although sway reported a failure")
	}
}

func TestDecodeIPCRejectsBadMagic(t *testing.T) {
	b := encodeIPC(1, []byte("{}"))
	copy(b, "xx-ipc")
	if _, _, err := decodeIPC(bytes.NewReader(b)); err == nil {
		t.Error("decoded a reply with bad magic")
	}
}

func TestLayout(t *testing.T) {
	var outs []Output
	if err := json.Unmarshal([]byte(`[{"rect":{"x":0,"y":0,"width":1280,"height":800}},{"rect":{"x":1280,"y":100,"width":800,"height":600}}]`), &outs); err != nil {
		t.Fatal(err)
	}
	r, err := Layout(outs)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Rect{X: 0, Y: 0, Width: 2080, Height: 800}); r != want {
		t.Errorf("Layout = %+v, want %+v", r, want)
	}
	if _, err := Layout(nil); err == nil {
		t.Error("Layout of no outputs succeeded")
	}
}
