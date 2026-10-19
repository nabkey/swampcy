// Package web serves swampcy's desktop to a browser: a page and a signalling
// WebSocket behind `tailscale serve`, the screen as a WebRTC video track (AV1, or
// H.264 where the browser cannot decode AV1), and the viewer's pointer and keys
// back on a data channel (docs/web.md).
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/webrtc/v4"

	"github.com/nabkey/swampcy/internal/desktop"
)

//go:embed static
var static embed.FS

// LoginHeader is set by `tailscale serve` from the WireGuard peer, after removing
// any copy the client sent.
const LoginHeader = "Tailscale-User-Login"

// replacedCode closes the WebSocket of a viewer a newer one replaced.
const replacedCode websocket.StatusCode = 4000

const csp = "default-src 'self'; connect-src 'self'; img-src 'self' data:; media-src 'self' blob:; " +
	"style-src 'self'; script-src 'self'; frame-ancestors 'none'"

// Options configures the server.
type Options struct {
	Login, Origin string
	// UDP is where WebRTC media and data are received: host:port, or :port for the
	// machine's Tailscale IPv4 address.
	UDP string
	// Codec is "auto" (AV1 when the browser offers it, else H.264), "av1" or "h264".
	Codec       string
	FPS         int
	StartKbps   int
	MaxKbps     int
	Quality     int // AV1's cq-level
	Threads     int
	H264Profile string // "high" or "baseline"
	// The agents' desktop is AgentOutput (empty for the top-left output), and the
	// owner's OwnOutput (empty for none), sized to each viewer that asks for it.
	// A viewer's input joins Seat (empty for the first) on either,
	// except input to the windows of apps SingleSeatApps matches (by app ID or X11
	// class), which joins the first seat, since they bind no other.
	AgentOutput    string
	OwnOutput      string
	Seat           string
	SingleSeatApps *regexp.Regexp
	Log            *slog.Logger
}

// Server is the web viewer.
type Server struct {
	opt Options

	// first holds, by output, the virtual pointer and keyboard on the first seat
	// that sessions' input to single-seat apps goes through.
	firstMu sync.Mutex
	first   map[string]inputs

	// clips holds the clipboard viewers paste into, on the owner's seat and on the
	// first (keyed by first), for the server's life: a Wayland selection lasts only
	// as long as the connection that set it.
	clipMu sync.Mutex
	clips  map[bool]*desktop.Clipboard
	live   *desktop.Live
	api    *webrtc.API
	udp    net.PacketConn

	// estimators hands each new peer connection's bandwidth estimator to the
	// session that created it; newPeer serialises creation so they pair up.
	estimators chan cc.BandwidthEstimator
	newPeer    sync.Mutex

	mu      sync.Mutex
	current *session
}

// tailnetV4 is Tailscale's CGNAT range, which every tailnet IPv4 address is in.
var tailnetV4 = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// TailnetIPv4 returns this machine's Tailscale IPv4 address.
func TailnetIPv4() (net.IP, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && tailnetV4.Contains(n.IP) {
			return n.IP, nil
		}
	}
	return nil, errors.New("no Tailscale IPv4 address on this machine")
}

// New binds the UDP port and builds the WebRTC stack.
func New(opt Options, live *desktop.Live) (*Server, error) {
	host, port, err := net.SplitHostPort(opt.UDP)
	if err != nil {
		return nil, fmt.Errorf("-udp: %w", err)
	}
	ip := net.ParseIP(host)
	if host == "" {
		if ip, err = TailnetIPv4(); err != nil {
			return nil, err
		}
	}
	if ip == nil || ip.To4() == nil || !tailnetV4.Contains(ip) {
		return nil, fmt.Errorf("-udp %q: WebRTC must listen on a Tailscale IPv4 address", opt.UDP)
	}
	udp, err := net.ListenPacket("udp4", net.JoinHostPort(ip.String(), port))
	if err != nil {
		return nil, err
	}

	s := &Server{opt: opt, live: live, udp: udp, estimators: make(chan cc.BandwidthEstimator, 1)}

	m := &webrtc.MediaEngine{}
	feedback := []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}, {Type: "ccm", Parameter: "fir"}, {Type: webrtc.TypeRTCPFBTransportCC}}
	codecs := []webrtc.RTPCodecCapability{{MimeType: webrtc.MimeTypeAV1, ClockRate: 90000, RTCPFeedback: feedback}}
	for _, id := range h264Profiles {
		codecs = append(codecs, webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: h264Fmtp(id), RTCPFeedback: feedback})
	}
	for i, c := range codecs {
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: c, PayloadType: webrtc.PayloadType(96 + i)}, webrtc.RTPCodecTypeVideo); err != nil {
			udp.Close()
			return nil, err
		}
	}

	ir := &interceptor.Registry{}
	bwe, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		return gcc.NewSendSideBWE(
			gcc.SendSideBWEInitialBitrate(opt.StartKbps*1000),
			gcc.SendSideBWEMinBitrate(150_000),
			gcc.SendSideBWEMaxBitrate(opt.MaxKbps*1000),
		)
	})
	if err != nil {
		udp.Close()
		return nil, err
	}
	bwe.OnNewPeerConnection(func(_ string, e cc.BandwidthEstimator) { s.estimators <- e })
	ir.Add(bwe)
	if err := webrtc.ConfigureTWCCHeaderExtensionSender(m, ir); err != nil {
		udp.Close()
		return nil, err
	}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		udp.Close()
		return nil, err
	}

	se := webrtc.SettingEngine{}
	se.SetICEUDPMux(webrtc.NewICEUDPMux(nil, udp))
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetLite(true) // the browser does the checks; we answer from one known address
	se.SetIncludeLoopbackCandidate(false)
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	se.SetIPFilter(func(ip net.IP) bool { return tailnetV4.Contains(ip) })

	s.api = webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se))
	return s, nil
}

// h264Profiles are the H.264 profile-level-ids offered, High before Constrained
// Baseline; pion matches on the first two bytes (profile and constraints), which
// is why Safari's (640c, 42e0) and Chrome's (6400, 4200) are both here.
var h264Profiles = []string{"640c1f", "64001f", "42e01f", "42001f"}

func h264Fmtp(profile string) string {
	return "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=" + profile
}

// Close ends the session and frees the port.
func (s *Server) Close() error {
	s.mu.Lock()
	cur := s.current
	s.mu.Unlock()
	if cur != nil {
		cur.end()
	}
	return s.udp.Close()
}

// CheckListen refuses any address but a loopback IP literal with a port: the
// server trusts LoginHeader, which only `tailscale serve` may set.
func CheckListen(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("listen address %q is not a loopback IP; swampcy web must sit behind tailscale serve", addr)
	}
	return nil
}

// Handler returns the routes, all behind the login check.
func (s *Server) Handler() http.Handler {
	sub, _ := fs.Sub(static, "static")
	files := http.FileServerFS(sub)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", s.signal)
	mux.Handle("GET /", files)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if login := r.Header.Get(LoginHeader); login != s.opt.Login {
			s.log().Warn("refused", "login", login, "path", r.URL.Path, "peer", r.RemoteAddr)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) log() *slog.Logger {
	if s.opt.Log != nil {
		return s.opt.Log
	}
	return slog.Default()
}

type message struct {
	Type    string `json:"type"`
	SDP     string `json:"sdp,omitempty"`
	Codec   string `json:"codec,omitempty"`
	Width   int    `json:"width,omitempty"`
	Height  int    `json:"height,omitempty"`
	Message string `json:"message,omitempty"`
	// Screen is the desktop: "agent" or "own". An offer asks for one (with the
	// viewer's CSS size in Width and Height and its pixel ratio in DPR, for "own");
	// the answer says which it got, and Screens which there are.
	Screen  string       `json:"screen,omitempty"`
	Screens []string     `json:"screens,omitempty"`
	DPR     float64      `json:"dpr,omitempty"`
	Windows []windowInfo `json:"windows,omitempty"`
	ID      int64        `json:"id,omitempty"` // move: the window
	To      string       `json:"to,omitempty"` // move: the desktop
	Apps    []appInfo    `json:"apps,omitempty"`
	App     string       `json:"app,omitempty"` // launch: the app's desktop file ID
	// The owner's desktop's zoom: asked for in an offer (0 for the viewer's own
	// pixel ratio), and given in the answer with the highest there could be.
	Zoom    float64 `json:"zoom,omitempty"`
	MaxZoom float64 `json:"maxZoom,omitempty"`
}

// windowInfo is one window, for moving between the desktops.
type windowInfo struct {
	ID     int64  `json:"id"`
	Title  string `json:"title"`
	App    string `json:"app"`
	Screen string `json:"screen"` // "agent", "own", or another output's name
}

// signal runs one viewer's session from its WebSocket: the offer in, the answer
// out, then keyframe requests until either side goes away.
func (s *Server) signal(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != s.opt.Origin {
		s.log().Warn("refused", "origin", origin, "path", r.URL.Path, "peer", r.RemoteAddr)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true}) // Origin checked above
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx := r.Context()
	send := func(m message) error {
		b, _ := json.Marshal(m)
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return c.Write(wctx, websocket.MessageText, b)
	}
	fail := func(err error) {
		s.log().Warn("session", "err", err)
		_ = send(message{Type: "error", Message: err.Error()})
		c.Close(websocket.StatusPolicyViolation, "")
	}

	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, data, err := c.Read(rctx)
	cancel()
	var offer message
	if err != nil || json.Unmarshal(data, &offer) != nil || offer.Type != "offer" || offer.SDP == "" {
		fail(errors.New("expected an offer"))
		return
	}
	sess, answer, err := s.start(offer)
	if err != nil {
		fail(err)
		return
	}
	defer sess.end()
	answer.Type = "answer"
	if err := send(answer); err != nil {
		return
	}
	s.log().Info("session", "codec", answer.Codec, "screen", answer.Screen, "size", fmt.Sprintf("%dx%d", answer.Width, answer.Height), "peer", r.RemoteAddr)

	go func() { // the WebSocket's end ends the session, and the session's end the socket
		<-sess.ctx.Done()
		if sess.replaced.Load() {
			c.Close(replacedCode, "another viewer took over") // the page stops reconnecting
			return
		}
		c.Close(websocket.StatusNormalClosure, "")
	}()
	for {
		_, data, err := c.Read(sess.ctx)
		if err != nil {
			return
		}
		var m message
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Type {
		case "keyframe":
			sess.requestKeyframe()
		case "windows":
			_ = send(message{Type: "windows", Windows: sess.windows()})
		case "move":
			if err := sess.move(m.ID, m.To); err != nil {
				_ = send(message{Type: "notice", Message: err.Error()})
			}
			_ = send(message{Type: "windows", Windows: sess.windows()})
		case "apps":
			_ = send(message{Type: "apps", Apps: scanApps(appDirs(os.Getenv))})
		case "launch":
			err := sess.launchApp(m.App, func(title string, err error) {
				if err != nil {
					s.log().Warn("launch", "app", m.App, "err", err)
					_ = send(message{Type: "notice", Message: err.Error()})
					return
				}
				_ = send(message{Type: "notice", Message: "opened " + title})
			})
			if err != nil {
				_ = send(message{Type: "notice", Message: err.Error()})
			}
		}
	}
}

// chooseCodec picks AV1 when the offer lists it (and the options allow), else
// H.264, preferring High over Constrained Baseline.
func chooseCodec(offer, want string) (webrtc.RTPCodecCapability, error) {
	sdp := strings.ToLower(offer)
	if want != "h264" && strings.Contains(sdp, " av1/90000") {
		return webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeAV1, ClockRate: 90000}, nil
	}
	if want != "av1" {
		for _, id := range h264Profiles {
			if strings.Contains(sdp, "profile-level-id="+id[:4]) {
				return webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: h264Fmtp(id)}, nil
			}
		}
	}
	return webrtc.RTPCodecCapability{}, fmt.Errorf("the browser offers neither AV1 nor H.264 (High or Constrained Baseline) to match -codec %s", want)
}
