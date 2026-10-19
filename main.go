// Command swampcy is an MCP server, spoken over stdio, that gives a coding agent a
// desktop: a headless sway session it can see with screenshots, drive with the
// pointer and keyboard, and read and operate through AT-SPI.
//
// It finds the session from SWAYSOCK and WAYLAND_DISPLAY, or from the sockets in
// XDG_RUNTIME_DIR, and needs grim on PATH. It sees and points at one output, and
// its pointer and keyboard join one seat:
//
//	claude mcp add --scope user swampcy -- swampcy
//	swampcy -max-edge 1280 -jpeg     # smaller screenshots
//	swampcy -output HEADLESS-1 -seat seat0
//	swampcy -replay-dir ~/clips -replay-keep 72h   # where the replay tool's clips go
//
// `swampcy web` serves the same desktop to a browser on the tailnet instead:
// behind `tailscale serve`, letting in one Tailscale login, with the screen as
// AV1 or H.264 over WebRTC:
//
//	swampcy web -login you@example.com -origin https://box.example.ts.net:8443
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nabkey/swampcy/internal/desktop"
	"github.com/nabkey/swampcy/internal/web"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "web" {
		os.Exit(runWeb(os.Args[2:]))
	}
	maxEdge := flag.Int("max-edge", 1568, "screenshots' long edge in pixels; larger screens are scaled down and coordinates converted")
	jpeg := flag.Bool("jpeg", false, "send screenshots as JPEG instead of PNG")
	a11y := flag.Bool("a11y", true, "offer the AT-SPI tools and turn accessibility on for the session")
	typeDelay := flag.Duration("type-delay", 2*time.Millisecond, "pause between keystrokes when typing")
	appLog := flag.String("app-log", "", "file for launched apps' output (default $XDG_RUNTIME_DIR/swampcy/apps.log)")
	output := flag.String("output", "", "the output to see and point at (default the one at the layout's top left)")
	seat := flag.String("seat", "seat0", "the seat the virtual pointer and keyboard join")
	replay := flag.Bool("replay", true, "keep a small frame after each action, for the replay tool's clips")
	replayDir := flag.String("replay-dir", defaultReplayDir(), "directory for replay clips; made 0700, clips 0600")
	replayWidth := flag.Int("replay-width", 640, "replay frames' width in pixels")
	replayFrames := flag.Int("replay-frames", 40, "most frames a replay holds")
	replayKeep := flag.Duration("replay-keep", 7*24*time.Hour, "delete replay clips older than this")
	version := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *version {
		fmt.Println(revision())
		return
	}
	// stdout is the protocol; everything else goes to stderr, which Claude Code
	// keeps in its MCP logs.
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if _, err := exec.LookPath("grim"); err != nil {
		log.Warn("missing tool", "tool", "grim", "err", err)
	}

	live := &desktop.Live{Getenv: os.Getenv, JPEG: *jpeg, TypeDelay: *typeDelay, AppLog: *appLog, Log: log, Seat: *seat, Output: *output}
	if _, err := live.Session(); err != nil {
		log.Warn("no session yet; tools will look again when called", "err", err)
	} else if _, err := live.Input(); err != nil {
		// The seat has no pointer or keyboard until these exist.
		log.Warn("no virtual pointer and keyboard yet", "err", err)
	}
	d := &desktop.Desktop{Sway: live, Screen: live, Pointer: live, Keyboard: live, Launch: live.Launch, MaxEdge: *maxEdge, Output: *output, Log: log}

	if *replay {
		d.Replay = &desktop.Replay{Screen: live, Dir: *replayDir, Width: *replayWidth, Size: *replayFrames, Keep: *replayKeep,
			Settle: 300 * time.Millisecond, Encode: desktop.EncodeClip, Log: log}
		if n, err := d.Replay.Expire(); err != nil {
			log.Warn("replay: expire", "err", err)
		} else if n > 0 {
			log.Info("replay: expired clips", "count", n)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *a11y {
		a := &desktop.A11y{BusAddress: sessionBus()}
		if err := a.Enable(ctx); err != nil {
			log.Warn("could not turn accessibility on; apps may expose no tree", "err", err)
		}
		d.A11y = a
	}

	log.Info("serving", "revision", revision(), "max_edge", *maxEdge, "a11y", *a11y, "output", *output, "seat", *seat)
	if err := d.Server(revision()).Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}

// sessionBus is the D-Bus session bus's address: from the environment, or the
// socket systemd puts in the runtime dir, which is where an SSH login finds it.
func sessionBus() string {
	if a := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); a != "" {
		return a
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
	}
	return "unix:path=" + filepath.Join(dir, "bus")
}

// defaultReplayDir is $XDG_STATE_HOME/swampcy/replays, or ~/.local/state/swampcy/replays.
func defaultReplayDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(d) {
		return filepath.Join(d, "swampcy", "replays")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "swampcy-replays")
	}
	return filepath.Join(home, ".local", "state", "swampcy", "replays")
}

// revision is the commit the binary was built from, when Go recorded it.
func revision() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
		if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			return bi.Main.Version
		}
	}
	return "unknown"
}

// runWeb is `swampcy web`: the desktop for a browser, behind tailscale serve.
func runWeb(args []string) int {
	fs := flag.NewFlagSet("swampcy web", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:7682", "address to serve the page and signalling on; must be loopback")
	login := fs.String("login", "", "the one Tailscale login let in (required)")
	origin := fs.String("origin", "", "the page's origin, e.g. https://box.example.ts.net:8443 (required)")
	udp := fs.String("udp", ":7683", "WebRTC's UDP port, on this machine's Tailscale IPv4 address unless a host is given")
	codecName := fs.String("codec", "auto", "auto (AV1 when the browser offers it, else H.264), av1, or h264")
	fps := fs.Int("fps", 60, "most frames a second")
	startKbps := fs.Int("start-kbps", 2500, "bitrate to start at before the network has been measured")
	maxKbps := fs.Int("max-kbps", 20000, "bitrate cap")
	quality := fs.Int("quality", 24, "AV1 constant-quality level, 0 (best) to 63")
	threads := fs.Int("threads", 4, "encoder threads")
	h264Profile := fs.String("h264-profile", "high", "high (CABAC) or baseline, when the browser offers both")
	agentOutput := fs.String("agent-output", "", "the agents' desktop: the output agents see (default the one at the layout's top left)")
	ownOutput := fs.String("own-output", "", "the owner's desktop, sized to each viewer that asks for it (default none)")
	seat := fs.String("seat", "", "the seat viewers' input joins (default the first)")
	singleSeat := fs.String("single-seat-apps", web.DefaultSingleSeatApps, "app IDs or X11 classes (a regexp) whose windows get viewers' input on the first seat, since they bind no other; empty for none")
	_ = fs.Parse(args)

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := web.CheckListen(*listen); err != nil {
		log.Error("start", "err", err)
		return 2
	}
	if *login == "" || *origin == "" {
		log.Error("start", "err", "-login and -origin are required")
		return 2
	}
	var singleSeatApps *regexp.Regexp
	if *singleSeat != "" {
		re, err := regexp.Compile(*singleSeat)
		if err != nil {
			log.Error("start", "err", fmt.Errorf("-single-seat-apps: %w", err))
			return 2
		}
		singleSeatApps = re
	}
	live := &desktop.Live{Getenv: os.Getenv, Log: log}
	if _, err := live.Session(); err != nil {
		log.Warn("no session yet; viewers will be refused until sway is up", "err", err)
	}
	srv, err := web.New(web.Options{Login: *login, Origin: *origin, UDP: *udp, Codec: *codecName, FPS: *fps,
		StartKbps: *startKbps, MaxKbps: *maxKbps, Quality: *quality, Threads: *threads, H264Profile: *h264Profile,
		AgentOutput: *agentOutput, OwnOutput: *ownOutput, Seat: *seat, SingleSeatApps: singleSeatApps, Log: log}, live)
	if err != nil {
		log.Error("start", "err", err)
		return 1
	}
	defer srv.Close()
	hs := &http.Server{Addr: *listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdown)
	}()
	log.Info("serving", "listen", *listen, "udp", *udp, "login", *login, "origin", *origin, "codec", *codecName,
		"agent_output", *agentOutput, "own_output", *ownOutput, "seat", *seat, "revision", revision())
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		return 1
	}
	return 0
}
