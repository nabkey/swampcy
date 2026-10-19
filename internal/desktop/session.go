package desktop

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Session names the sway swampcy drives: its IPC socket, and the Wayland display
// that grim, wtype and launched apps connect to.
type Session struct {
	RuntimeDir     string
	SwaySock       string
	WaylandDisplay string
}

// Discover finds the session from SWAYSOCK and WAYLAND_DISPLAY when they are set,
// and otherwise from the sockets in XDG_RUNTIME_DIR. An MCP server started by a
// coding agent in an SSH session has neither variable, so discovery is the normal
// case; it refuses to guess when there is more than one candidate.
func Discover(getenv func(string) string) (Session, error) {
	s := Session{RuntimeDir: getenv("XDG_RUNTIME_DIR")}
	if s.RuntimeDir == "" {
		s.RuntimeDir = filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
	}

	s.SwaySock = getenv("SWAYSOCK")
	if s.SwaySock == "" {
		pattern := filepath.Join(s.RuntimeDir, fmt.Sprintf("sway-ipc.%d.*.sock", os.Getuid()))
		socks, _ := filepath.Glob(pattern)
		var live []string
		for _, p := range socks {
			if _, err := (&Sway{Socket: p}).Version(); err == nil {
				live = append(live, p)
			}
		}
		switch len(live) {
		case 0:
			return s, fmt.Errorf("no sway answers at %s; start the headless session (systemctl --user start swampcy-sway) or set SWAYSOCK", pattern)
		case 1:
			s.SwaySock = live[0]
		default:
			return s, fmt.Errorf("more than one sway is running (%s); set SWAYSOCK to choose", strings.Join(live, ", "))
		}
	}

	s.WaylandDisplay = getenv("WAYLAND_DISPLAY")
	if s.WaylandDisplay == "" {
		displays, err := waylandDisplays(s.RuntimeDir)
		if err != nil {
			return s, err
		}
		switch len(displays) {
		case 0:
			return s, fmt.Errorf("no Wayland display in %s; set WAYLAND_DISPLAY", s.RuntimeDir)
		case 1:
			s.WaylandDisplay = displays[0]
		default:
			return s, fmt.Errorf("more than one Wayland display in %s (%s); set WAYLAND_DISPLAY to choose", s.RuntimeDir, strings.Join(displays, ", "))
		}
	}
	return s, nil
}

// waylandDisplays lists the Wayland sockets in dir: wayland-N, each with a
// wayland-N.lock beside it.
func waylandDisplays(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "wayland-") || strings.HasSuffix(name, ".lock") {
			continue
		}
		if e.Type()&os.ModeSocket == 0 {
			continue
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out, nil
}

// a11yEnv turns accessibility on in toolkits that otherwise wait to be asked:
// Qt, and Chromium and Electron, which build their tree only when they believe
// an assistive technology is listening.
var a11yEnv = []string{
	"QT_LINUX_ACCESSIBILITY_ALWAYS_ON=1",
	"ACCESSIBILITY_ENABLED=1",
	"NO_AT_BRIDGE=0",
}

// Env is the environment for programs that act on the session: grim, wtype, and
// the apps swampcy launches. It is swampcy's own, pointed at sway's display, with
// DISPLAY removed so nothing reaches for an X server instead.
func (s Session) Env() []string {
	drop := []string{"WAYLAND_DISPLAY=", "SWAYSOCK=", "DISPLAY=", "XDG_SESSION_TYPE=", "XDG_RUNTIME_DIR="}
	var env []string
	hasBus := false
	for _, kv := range os.Environ() {
		if slices.ContainsFunc(drop, func(p string) bool { return strings.HasPrefix(kv, p) }) {
			continue
		}
		if strings.HasPrefix(kv, "DBUS_SESSION_BUS_ADDRESS=") {
			hasBus = true
		}
		env = append(env, kv)
	}
	env = append(env,
		"XDG_RUNTIME_DIR="+s.RuntimeDir,
		"WAYLAND_DISPLAY="+s.WaylandDisplay,
		"SWAYSOCK="+s.SwaySock,
		"XDG_SESSION_TYPE=wayland",
	)
	if !hasBus {
		env = append(env, "DBUS_SESSION_BUS_ADDRESS=unix:path="+filepath.Join(s.RuntimeDir, "bus"))
	}
	return append(env, a11yEnv...)
}
