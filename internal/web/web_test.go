package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/nabkey/swampcy/internal/desktop"
)

func TestCheckListen(t *testing.T) {
	for addr, ok := range map[string]bool{"127.0.0.1:7682": true, "[::1]:7682": true, "localhost:7682": false, "0.0.0.0:7682": false, "100.64.1.2:7682": false} {
		if err := CheckListen(addr); (err == nil) != ok {
			t.Errorf("CheckListen(%q) = %v", addr, err)
		}
	}
}

func TestChooseCodec(t *testing.T) {
	safari := "m=video 9 UDP/TLS/RTP/SAVPF 96 97 98\r\na=rtpmap:96 AV1/90000\r\na=rtpmap:97 H264/90000\r\na=fmtp:97 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640c1f\r\na=rtpmap:98 H264/90000\r\na=fmtp:98 packetization-mode=1;profile-level-id=42e01f\r\n"
	chrome := "a=rtpmap:102 H264/90000\r\na=fmtp:102 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f\r\na=fmtp:112 profile-level-id=64001f;packetization-mode=1\r\n"
	for _, tc := range []struct {
		offer, want, mime, fmtp string
	}{
		{safari, "auto", webrtc.MimeTypeAV1, ""},
		{safari, "h264", webrtc.MimeTypeH264, "profile-level-id=640c1f"},
		{strings.ReplaceAll(safari, "640c1f", "4d001f"), "h264", webrtc.MimeTypeH264, "profile-level-id=42e01f"},
		{chrome, "auto", webrtc.MimeTypeH264, "profile-level-id=64001f"},
	} {
		c, err := chooseCodec(tc.offer, tc.want)
		if err != nil || c.MimeType != tc.mime || !strings.Contains(c.SDPFmtpLine, tc.fmtp) {
			t.Errorf("chooseCodec(-codec %s) = %s %q, %v; want %s %q", tc.want, c.MimeType, c.SDPFmtpLine, err, tc.mime, tc.fmtp)
		}
	}
	if _, err := chooseCodec("a=rtpmap:96 VP8/90000\r\n", "auto"); err == nil {
		t.Error("chose a codec from a VP8-only offer")
	}
	if _, err := chooseCodec(chrome, "av1"); err == nil {
		t.Error("-codec av1 accepted an offer without AV1")
	}
}

func TestHandlerChecksLoginAndOrigin(t *testing.T) {
	s := &Server{opt: Options{Login: "owner@example.com", Origin: "https://desk.example:8443"}}
	h := s.Handler()
	get := func(path string, hdr map[string]string) *http.Response {
		r := httptest.NewRequest("GET", path, nil)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Result()
	}
	if res := get("/", nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("no login: %d, want 403", res.StatusCode)
	}
	if res := get("/", map[string]string{LoginHeader: "someone@else.com"}); res.StatusCode != http.StatusForbidden {
		t.Errorf("wrong login: %d, want 403", res.StatusCode)
	}
	res := get("/", map[string]string{LoginHeader: "owner@example.com"})
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "app.js") {
		t.Errorf("page: %d %q", res.StatusCode, body[:min(len(body), 80)])
	}
	if p := res.Header.Get("Content-Security-Policy"); !strings.Contains(p, "script-src 'self'") || !strings.Contains(p, "frame-ancestors 'none'") {
		t.Errorf("CSP = %q", p)
	}
	if res := get("/ws", map[string]string{LoginHeader: "owner@example.com", "Origin": "https://evil.example"}); res.StatusCode != http.StatusForbidden {
		t.Errorf("wrong origin on /ws: %d, want 403", res.StatusCode)
	}
}

// fakeInputs records what a viewer's input did.
type fakeInputs struct {
	calls   []string
	onPress func()
}

func (f *fakeInputs) log(format string, a ...any) error {
	f.calls = append(f.calls, fmt.Sprintf(format, a...))
	return nil
}
func (f *fakeInputs) MoveTo(x, y, w, h int) error { return f.log("move %d %d", x, y) }
func (f *fakeInputs) Press(b string) error {
	if f.onPress != nil {
		f.onPress()
	}
	return f.log("press %s", b)
}
func (f *fakeInputs) Release(b string) error       { return f.log("release %s", b) }
func (f *fakeInputs) Scroll(d string, n int) error { return f.log("scroll %s %d", d, n) }
func (f *fakeInputs) Key(code uint32, down bool) error {
	return f.log("key %d %v", code, down)
}
func (f *fakeInputs) TypeText(s string, _ time.Duration) error { return f.log("type %q", s) }
func (f *fakeInputs) ReleaseAll() error                        { return f.log("release all") }
func (f *fakeInputs) Err() error                               { return nil }
func (f *fakeInputs) Close()                                   {}

func TestInputMessages(t *testing.T) {
	f := &fakeInputs{}
	s := &Server{}
	se := &session{s: s, ctx: context.Background(), width: 1280, height: 800,
		newInputs: func(bool) (inputs, error) { return f, nil }}
	for _, m := range [][]byte{
		{inMove, 0x10, 0x00, 0x20, 0x00},              // move 16,32
		{inButton, 1, 1, 0x00, 0x05, 0x20, 0x03},      // right press at (1280,800): off the frame, dropped
		{inButton, 0, 1, 0xff, 0x04, 0x1f, 0x03},      // left press at 1279,799
		{inButton, 0, 0, 0xff, 0x04, 0x1f, 0x03},      // left release
		{inButton, 3, 1, 0, 0, 0, 0},                  // no button 3
		{inWheel, 0xfe, 0x03, 0x0a, 0x00, 0x0a, 0x00}, // dx -2, dy 3 at 10,10
		{inKey, 29, 0, 1},                             // ctrl down
		{inKey, 0x00, 0x03, 1},                        // keycode 768: refused
		{inText, 'h', 'i'},                            // ASCII
		{inText, 0xff, 0xfe},                          // not UTF-8
		{inRelease},
		{0x7f, 1, 2, 3}, // unknown kind
		{inMove, 1},     // short
	} {
		se.input(m)
	}
	want := []string{
		"move 16 32",
		"move 1279 799", "press left",
		"move 1279 799", "release left",
		"move 10 10", "scroll down 3", "scroll left 2",
		"key 29 true",
		`type "hi"`,
		"release all",
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls =\n%q\nwant\n%q", f.calls, want)
	}
}

// routedSession is a session on HEADLESS-2 (at 1280,0 in the layout, scale 2,
// 800x800 pixels) with Chrome's window on its left half and foot's on its right;
// seat0's focus is on the agents' window 99, on HEADLESS-1.
func routedSession(t *testing.T) (se *session, owner, first *fakeInputs, cmds *[]string) {
	t.Helper()
	owner, first = &fakeInputs{}, &fakeInputs{}
	visible := true
	str := func(s string) *string { return &s }
	win := func(id int64, app string, x int) *desktop.Node {
		return &desktop.Node{ID: id, Type: "con", AppID: str(app), Visible: &visible, Rect: desktop.Rect{X: x, Y: 0, Width: 200, Height: 400}}
	}
	tree := &desktop.Node{Type: "root", Nodes: []*desktop.Node{
		{Type: "output", Name: "HEADLESS-1", Nodes: []*desktop.Node{{Type: "workspace", Name: "1", Nodes: []*desktop.Node{win(99, "foot", 0)}}}},
		{Type: "output", Name: "HEADLESS-2", Nodes: []*desktop.Node{{Type: "workspace", Name: "2", Nodes: []*desktop.Node{
			win(1, "google-chrome", 1280), win(2, "foot", 1480)}}}},
	}}
	focus := int64(99)
	cmds = &[]string{}
	se = &session{s: &Server{}, ctx: context.Background(), width: 800, height: 800,
		newInputs: func(f bool) (inputs, error) {
			if f {
				return first, nil
			}
			return owner, nil
		},
		route: &router{apps: regexp.MustCompile(DefaultSingleSeatApps), output: "HEADLESS-2", agents: "HEADLESS-1",
			origin: desktop.Rect{X: 1280, Y: 0, Width: 400, Height: 400}, scale: 2,
			tree: func() (*desktop.Node, error) { return tree, nil },
			seats: func() ([]desktop.Seat, error) {
				return []desktop.Seat{{Name: "owner", Focus: 2}, {Name: "seat0", Focus: focus}}, nil
			},
			command: func(c string) error {
				*cmds = append(*cmds, c)
				fmt.Sscanf(c, "[con_id=%d] focus", &focus)
				return nil
			}},
	}
	// A click with seat0 focuses what it presses, as sway does.
	first.onPress = func() { focus = 1 }
	t.Cleanup(func() {
		if se.giveBack != nil {
			se.giveBack.Stop()
		}
	})
	return se, owner, first, cmds
}

func TestInputToSingleSeatAppsGoesThroughTheFirstSeat(t *testing.T) {
	se, owner, first, cmds := routedSession(t)
	for _, m := range [][]byte{
		{inMove, 100, 0, 100, 0},             // over Chrome
		{inButton, 0, 1, 100, 0, 100, 0},     // press Chrome
		{inButton, 0, 0, 0x58, 0x02, 100, 0}, // release over foot: where the press went
		{inKey, 30, 0, 1}, {inKey, 30, 0, 0}, // "a" to Chrome
		{inText, 'h', 'i'},                   // to Chrome
		{inWheel, 0, 1, 0x58, 0x02, 100, 0},  // over foot
		{inButton, 0, 1, 0x58, 0x02, 100, 0}, // press foot
		{inButton, 0, 0, 0x58, 0x02, 100, 0},
		{inKey, 30, 0, 1}, {inKey, 30, 0, 0}, // "a" to foot
	} {
		se.input(m)
	}
	wantFirst := []string{"move 100 100", "move 100 100", "press left", "move 600 100", "release left",
		"key 30 true", "key 30 false", `type "hi"`}
	wantOwner := []string{"move 600 100", "scroll down 1", "move 600 100", "press left", "move 600 100", "release left",
		"key 30 true", "key 30 false"}
	if !reflect.DeepEqual(first.calls, wantFirst) {
		t.Errorf("first seat:\n%q\nwant\n%q", first.calls, wantFirst)
	}
	if !reflect.DeepEqual(owner.calls, wantOwner) {
		t.Errorf("owner's seat:\n%q\nwant\n%q", owner.calls, wantOwner)
	}
	if len(*cmds) != 0 {
		t.Errorf("commands before giving back = %q; the click focused Chrome", *cmds)
	}
	se.route.lastUse = time.Now().Add(-lendIdle)
	se.tryGiveBack()
	if want := []string{"[con_id=99] focus"}; !reflect.DeepEqual(*cmds, want) {
		t.Errorf("commands = %q, want %q", *cmds, want)
	}
	if se.route.lent != 0 {
		t.Error("seat0's focus still lent after giving it back")
	}
}

func TestKeysToSingleSeatAppLendSeat0Focus(t *testing.T) {
	se, _, first, cmds := routedSession(t)
	se.input([]byte{inButton, 0, 1, 100, 0, 100, 0})
	se.input([]byte{inButton, 0, 0, 100, 0, 100, 0})
	se.route.lastUse = time.Now().Add(-lendIdle)
	se.tryGiveBack()                  // back to the agents' window
	se.input([]byte{inKey, 29, 0, 1}) // ctrl, still to Chrome: focus it again
	se.route.lastUse = time.Now().Add(-lendIdle)
	se.tryGiveBack() // held: not yet
	se.input([]byte{inKey, 29, 0, 0})
	se.route.lastUse = time.Now().Add(-lendIdle)
	se.tryGiveBack()
	want := []string{"[con_id=99] focus", "[con_id=1] focus", "[con_id=99] focus"}
	if !reflect.DeepEqual(*cmds, want) {
		t.Errorf("commands = %q, want %q", *cmds, want)
	}
	if got := first.calls[len(first.calls)-2:]; !reflect.DeepEqual(got, []string{"key 29 true", "key 29 false"}) {
		t.Errorf("first seat ended with %q", got)
	}
}

func TestGiveBackLeavesFocusAnAgentMoved(t *testing.T) {
	se, _, _, cmds := routedSession(t)
	se.input([]byte{inButton, 0, 1, 100, 0, 100, 0})
	se.input([]byte{inButton, 0, 0, 100, 0, 100, 0})
	se.route.command("[con_id=77] focus") // an agent's focus_window meanwhile
	*cmds = nil
	se.route.lastUse = time.Now().Add(-lendIdle)
	se.tryGiveBack()
	if len(*cmds) != 0 {
		t.Errorf("gave focus back over an agent's move: %q", *cmds)
	}
}

func TestOwnMode(t *testing.T) {
	for _, tc := range []struct {
		cssW, cssH      int
		dpr, zoom       float64
		w, h            int
		scale, maxScale float64
	}{
		{390, 844, 3, 0, 1170, 2532, 3, 3}, // iPhone, portrait: 3x fits, drawn at its own size
		{390, 844, 3, 1, 1170, 2532, 1, 3}, // zoomed out: the same pixels, three times the room
		{390, 844, 3, 1.5, 1170, 2532, 1.5, 3},
		{390, 844, 3, 5, 1170, 2532, 3, 3},      // no more than the pixel ratio
		{390, 844, 3, 0.4, 1170, 2532, 1, 3},    // nor less than 1
		{1512, 982, 2, 0, 2268, 1474, 1.5, 1.5}, // MacBook: 2x and 1.75x would pass 2560x1600 worth of pixels
		{1512, 982, 2, 2, 2268, 1474, 1.5, 1.5}, // and the zoom cannot go past what fits
		{1280, 800, 1, 0, 1280, 800, 1, 1},
		{1279, 801, 1, 0, 1280, 802, 1, 1},           // even sizes for the encoder
		{800, 600, 0, 0, 800, 600, 1, 1},             // no ratio given: 1
		{390, 844, 2.625, 0, 1072, 2322, 2.75, 2.75}, // to the nearest quarter, halves up
	} {
		w, h, scale, maxScale, err := ownMode(tc.cssW, tc.cssH, tc.dpr, tc.zoom)
		if err != nil || w != tc.w || h != tc.h || scale != tc.scale || maxScale != tc.maxScale {
			t.Errorf("ownMode(%d, %d, %v, %v) = %dx%d @%v max %v, %v; want %dx%d @%v max %v",
				tc.cssW, tc.cssH, tc.dpr, tc.zoom, w, h, scale, maxScale, err, tc.w, tc.h, tc.scale, tc.maxScale)
		}
	}
	for _, bad := range [][2]int{{100, 800}, {800, 5000}} {
		if _, _, _, _, err := ownMode(bad[0], bad[1], 1, 0); err == nil {
			t.Errorf("ownMode(%v) accepted", bad)
		}
	}
}

func TestDesktops(t *testing.T) {
	outs := []desktop.Output{{Name: "HEADLESS-2", Active: true, Rect: desktop.Rect{X: 1280, Width: 390, Height: 844}},
		{Name: "HEADLESS-1", Active: true, Rect: desktop.Rect{Width: 1280, Height: 800}}}
	for _, tc := range []struct {
		opt  Options
		want map[string]string
	}{
		{Options{}, map[string]string{"agent": "HEADLESS-1"}},
		{Options{OwnOutput: "HEADLESS-2"}, map[string]string{"agent": "HEADLESS-1", "own": "HEADLESS-2"}},
		{Options{OwnOutput: "HEADLESS-3"}, map[string]string{"agent": "HEADLESS-1"}},                            // not there yet
		{Options{AgentOutput: "HEADLESS-2", OwnOutput: "HEADLESS-2"}, map[string]string{"agent": "HEADLESS-2"}}, // not both
	} {
		s := &Server{opt: tc.opt}
		got, err := s.desktops(outs)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("desktops with %+v = %v, %v; want %v", tc.opt, got, err, tc.want)
		}
	}
}

func TestScanApps(t *testing.T) {
	user, sys := t.TempDir(), t.TempDir()
	write := func(dir, name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entry := func(name, extra string) string {
		return "[Desktop Entry]\nType=Application\nName=" + name + "\nExec=" + strings.ToLower(name) + " %U\n" + extra +
			"\n[Desktop Action new]\nName=New Window\nExec=x\n"
	}
	write(sys, "steam.desktop", entry("Steam", "StartupWMClass=Steam"))
	write(sys, "org.gnome.Calculator.desktop", entry("Calculator", "Name[de]=Rechner"))
	write(sys, "hidden.desktop", entry("Hidden", "NoDisplay=true"))
	write(sys, "term.desktop", entry("Htop", "Terminal=true"))
	write(sys, "link.desktop", "[Desktop Entry]\nType=Link\nName=Docs\nURL=https://example.com\n")
	write(sys, "bad name.desktop", entry("Bad", ""))
	write(sys, "game.desktop", entry("System Game", ""))
	write(user, "game.desktop", entry("RimWorld", "")) // the user's own wins
	write(user, "notes.txt", "not an entry")
	got := scanApps([]string{user, sys, filepath.Join(sys, "missing")})
	want := []appInfo{{ID: "org.gnome.Calculator.desktop", Name: "Calculator"}, {ID: "game.desktop", Name: "RimWorld"},
		{ID: "steam.desktop", Name: "Steam", wmClass: "Steam"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("scanApps = %+v, want %+v", got, want)
	}
	for _, bad := range []string{"x; rm -rf ~.desktop", "../steam.desktop", "steam", ".desktop"} {
		if appID.MatchString(bad) {
			t.Errorf("appID accepted %q", bad)
		}
	}
	env := map[string]string{"HOME": "/home/u"}
	if got := appDirs(func(k string) string { return env[k] }); !reflect.DeepEqual(got,
		[]string{"/home/u/.local/share/applications", "/usr/local/share/applications", "/usr/share/applications"}) {
		t.Errorf("appDirs = %v", got)
	}
}

func TestKeepAgentsFocus(t *testing.T) {
	tree := func(focused string) *desktop.Node {
		var n desktop.Node
		js := `{"type":"root","nodes":[
 {"type":"output","name":"HEADLESS-1","nodes":[{"type":"workspace","name":"1","nodes":[
  {"id":7,"type":"con","pid":1,"app_id":"calc"` + map[bool]string{true: `,"focused":true`}[focused == "7"] + `}]}]},
 {"type":"output","name":"HEADLESS-2","nodes":[{"type":"workspace","name":"2","nodes":[
  {"id":9,"type":"con","pid":2,"app_id":"steam"` + map[bool]string{true: `,"focused":true`}[focused == "9"] + `}]}]}]}`
		if err := json.Unmarshal([]byte(js), &n); err != nil {
			t.Fatal(err)
		}
		return &n
	}
	for _, tc := range []struct {
		focused  string
		moved    int64
		toAgents bool
		want     string
	}{
		{"7", 9, false, "; [con_id=7] focus"},        // their window keeps it
		{"", 9, false, "; focus output HEADLESS-1"},  // no window of theirs: their output (the Steam case)
		{"7", 7, false, "; focus output HEADLESS-1"}, // their focused window goes: their output
		{"9", 9, false, "; focus output HEADLESS-1"}, // focus was over here already
		{"7", 9, true, "; [con_id=7] focus"},         // bringing one back keeps theirs
		{"", 9, true, ""},
	} {
		if got := keepAgentsFocus(tree(tc.focused), tc.moved, tc.toAgents, "HEADLESS-1"); got != tc.want {
			t.Errorf("focused %q, moving %d (to agents %v): %q, want %q", tc.focused, tc.moved, tc.toAgents, got, tc.want)
		}
	}
}

func TestAppOwnsItsWindows(t *testing.T) {
	steam := appInfo{ID: "steam.desktop", Name: "Steam", wmClass: "Steam"}
	chrome := appInfo{ID: "google-chrome.desktop", Name: "Google Chrome"}
	calc := appInfo{ID: "org.gnome.Calculator.desktop", Name: "Calculator"}
	for _, tc := range []struct {
		app    appInfo
		window string
		want   bool
	}{
		{steam, "steam", true}, {steam, "Steam", true}, {steam, "steamwebhelper", false},
		{chrome, "Google-chrome", true}, {chrome, "google-chrome", true}, {chrome, "foot", false},
		{calc, "org.gnome.Calculator", true}, {calc, "", false},
	} {
		if got := tc.app.owns(tc.window); got != tc.want {
			t.Errorf("%s owns %q = %v, want %v", tc.app.Name, tc.window, got, tc.want)
		}
	}
}

func TestPasteSetsTheClipboardThenPressesCtrlV(t *testing.T) {
	f := &fakeInputs{}
	var clips []string
	se := &session{s: &Server{}, ctx: context.Background(), width: 1280, height: 800,
		newInputs: func(bool) (inputs, error) { return f, nil },
		clipboard: func(first bool, text string) error {
			clips = append(clips, fmt.Sprintf("%v %q", first, text))
			f.log("clipboard set")
			return nil
		}}
	se.input(append([]byte{inPaste}, "two\nlines"...))
	se.input([]byte{inPaste})             // empty: dropped
	se.input([]byte{inPaste, 0xff, 0xfe}) // not UTF-8: dropped
	if want := []string{`false "two\nlines"`}; !reflect.DeepEqual(clips, want) {
		t.Errorf("clipboard = %q, want %q", clips, want)
	}
	want := []string{"clipboard set", "key 29 true", "key 47 true", "key 47 false", "key 29 false"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls = %q, want %q: the selection set before Ctrl+V", f.calls, want)
	}

	// When the clipboard cannot be set, nothing is pressed: Ctrl+V would paste
	// whatever was there before.
	f.calls = nil
	se.clipboard = func(bool, string) error { return fmt.Errorf("no data control") }
	se.input(append([]byte{inPaste}, "x"...))
	if len(f.calls) != 0 {
		t.Errorf("calls after a failed clipboard = %q, want none", f.calls)
	}
}

func TestPasteToSingleSeatAppUsesTheFirstSeat(t *testing.T) {
	se, owner, first, _ := routedSession(t)
	var seats []bool
	se.clipboard = func(f bool, _ string) error { seats = append(seats, f); return nil }
	se.input([]byte{inButton, 0, 1, 100, 0, 100, 0}) // click Chrome: its keys go to seat0
	se.input([]byte{inButton, 0, 0, 100, 0, 100, 0})
	se.input(append([]byte{inPaste}, "hi"...))
	if !reflect.DeepEqual(seats, []bool{true}) {
		t.Errorf("clipboard set on first=%v, want seat0's, where Chrome reads it", seats)
	}
	if got := first.calls[len(first.calls)-4:]; !reflect.DeepEqual(got, []string{"key 29 true", "key 47 true", "key 47 false", "key 29 false"}) {
		t.Errorf("first seat ended with %q, want Ctrl+V", got)
	}
	for _, c := range owner.calls {
		if strings.HasPrefix(c, "key") {
			t.Errorf("owner's seat got %q", c)
		}
	}
}
