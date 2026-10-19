package web

import (
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/nabkey/swampcy/internal/codec"
	"github.com/nabkey/swampcy/internal/desktop"
)

// session is one viewer: its peer connection, the stream it is sent, and the
// virtual pointer and keyboard its input drives.
type session struct {
	s      *Server
	ctx    context.Context
	cancel context.CancelFunc
	pc     *webrtc.PeerConnection
	track  *webrtc.TrackLocalStaticSample
	codec  string
	width  int // the output in device pixels: the frames, and the viewer's input
	height int
	sess   desktop.Session
	screen string            // "agent" or "own"
	output string            // the output streamed and pointed at
	desks  map[string]string // the desktops there are: "agent", maybe "own", to their outputs

	keyframe chan struct{}
	target   atomic.Int64 // bits/s from the bandwidth estimator, 0 until it speaks
	replaced atomic.Bool  // ended because another viewer connected

	inMu      sync.Mutex
	in        inputs                              // on the owner's seat; the first seat's are the server's
	newInputs func(first bool) (inputs, error)    // desktop.OpenVirtualInput, or a fake in tests
	clipboard func(first bool, text string) error // Server.setClipboard, or a fake in tests

	// Which seat each input goes through, and what the first seat holds.
	routeMu      sync.Mutex
	route        *router // nil when there is one seat to use
	firstButtons map[byte]bool
	firstKeys    map[uint32]bool
	giveBack     *time.Timer
	settle       time.Duration // between moving and pressing

	endOnce sync.Once
}

// desktops maps the desktops there are to their outputs: the agents' always, and
// the owner's when OwnOutput names an active output other than the agents'.
func (s *Server) desktops(outs []desktop.Output) (map[string]string, error) {
	agent, err := desktop.PickOutput(outs, s.opt.AgentOutput)
	if err != nil {
		return nil, err
	}
	d := map[string]string{"agent": agent.Name}
	if s.opt.OwnOutput != "" && s.opt.OwnOutput != agent.Name {
		if own, err := desktop.PickOutput(outs, s.opt.OwnOutput); err == nil {
			d["own"] = own.Name
		} else {
			s.log().Warn("no owner's desktop", "err", err) // sway not yet restarted with it
		}
	}
	return d, nil
}

// The owner's output is sized to the viewer: its CSS size, at its pixel ratio (at
// most 3) so apps are drawn as sharp as its screen, within maxOwnPixels.
const (
	maxOwnScale  = 3.0
	maxOwnPixels = 2560 * 1600
)

// ownMode is the mode and scale for a viewer of cssW x cssH CSS pixels at dpr,
// zoomed to zoom (0 for none). The mode is the viewer's device pixels: its CSS
// size at its pixel ratio to the nearest quarter, at most maxOwnScale and lower
// while the mode would exceed maxOwnPixels (that ratio is maxScale), in even
// numbers of pixels, as encoders want. The scale is the zoom, to a quarter,
// between 1 and maxScale, and maxScale without one: apps drawn at the viewer's
// own size; a lower zoom gives them more room in the same pixels.
func ownMode(cssW, cssH int, dpr, zoom float64) (w, h int, scale, maxScale float64, err error) {
	if cssW < 200 || cssH < 200 || cssW > 4096 || cssH > 4096 {
		return 0, 0, 0, 0, fmt.Errorf("viewer size %dx%d is out of range", cssW, cssH)
	}
	maxScale = math.Round(min(max(dpr, 1), maxOwnScale)*4) / 4
	for maxScale > 1 && float64(cssW)*maxScale*float64(cssH)*maxScale > maxOwnPixels {
		maxScale -= 0.25
	}
	scale = maxScale
	if zoom > 0 {
		scale = min(max(math.Round(zoom*4)/4, 1), maxScale)
	}
	even := func(f float64) int { return int(math.Round(f/2)) * 2 }
	return even(float64(cssW) * maxScale), even(float64(cssH) * maxScale), scale, maxScale, nil
}

// fitOwn sizes the owner's output to a viewer, and says the scale and the
// highest the viewer could ask for.
func (s *Server) fitOwn(output string, cssW, cssH int, dpr, zoom float64) (scale, maxScale float64, err error) {
	w, h, scale, maxScale, err := ownMode(cssW, cssH, dpr, zoom)
	if err != nil {
		return 0, 0, err
	}
	return scale, maxScale, s.live.Command(fmt.Sprintf("output %s mode %dx%d scale %s", output, w, h, strconv.FormatFloat(scale, 'f', -1, 64)))
}

// start ends any current session and sets up a new one from an offer, returning
// it with the answer to send.
func (s *Server) start(offer message) (*session, message, error) {
	capability, err := chooseCodec(offer.SDP, s.opt.Codec)
	if err != nil {
		return nil, message{}, err
	}
	sess, err := s.live.Session()
	if err != nil {
		return nil, message{}, err
	}
	outs, err := s.live.Outputs()
	if err != nil {
		return nil, message{}, err
	}
	desks, err := s.desktops(outs)
	if err != nil {
		return nil, message{}, err
	}
	screen := offer.Screen
	if _, ok := desks[screen]; !ok {
		screen = "agent"
	}
	var zoom, maxZoom float64
	if screen == "own" {
		if zoom, maxZoom, err = s.fitOwn(desks["own"], offer.Width, offer.Height, offer.DPR, offer.Zoom); err != nil {
			s.log().Warn("sizing the owner's desktop", "err", err) // stream it as it is
		} else if outs, err = s.live.Outputs(); err != nil {
			return nil, message{}, err
		}
	}
	out, err := desktop.PickOutput(outs, desks[screen])
	if err != nil {
		return nil, message{}, err
	}

	s.mu.Lock()
	prev := s.current
	s.current = nil
	s.mu.Unlock()
	if prev != nil {
		prev.replaced.Store(true)
		prev.end() // one viewer at a time
	}

	ctx, cancel := context.WithCancel(context.Background())
	se := &session{s: s, ctx: ctx, cancel: cancel, width: out.Mode.Width, height: out.Mode.Height, sess: sess,
		screen: screen, output: out.Name, desks: desks, settle: pressSettle,
		keyframe: make(chan struct{}, 1), codec: strings.TrimPrefix(capability.MimeType, "video/")}
	if s.opt.Seat != "" && s.opt.Seat != firstSeat && s.opt.SingleSeatApps != nil {
		se.route = &router{apps: s.opt.SingleSeatApps, output: out.Name, agents: desks["agent"],
			origin: out.Rect, scale: out.Scale, tree: s.live.Tree, seats: s.live.Seats, command: s.live.Command}
	}

	// Creating the connection creates its bandwidth estimator, handed over on a
	// channel; one creation at a time keeps each estimator with its connection.
	s.newPeer.Lock()
	pc, err := s.api.NewPeerConnection(webrtc.Configuration{})
	var bwe interface{ OnTargetBitrateChange(func(int)) }
	if err == nil {
		select {
		case e := <-s.estimators:
			bwe = e
		case <-time.After(2 * time.Second):
		}
	}
	s.newPeer.Unlock()
	if err != nil {
		cancel()
		return nil, message{}, err
	}
	se.pc = pc
	if bwe != nil {
		bwe.OnTargetBitrateChange(func(bps int) { se.target.Store(int64(bps)) })
	}

	fail := func(err error) (*session, message, error) {
		se.end()
		return nil, message{}, err
	}
	if se.track, err = webrtc.NewTrackLocalStaticSample(capability, "screen", "swampcy"); err != nil {
		return fail(err)
	}
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		if st == webrtc.PeerConnectionStateFailed || st == webrtc.PeerConnectionStateClosed {
			se.end()
		}
	})
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != "input" {
			return
		}
		dc.OnMessage(func(m webrtc.DataChannelMessage) {
			if !m.IsString {
				se.input(m.Data)
			}
		})
		dc.OnClose(func() { se.releaseAll() })
	})
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.SDP}); err != nil {
		return fail(fmt.Errorf("offer: %w", err))
	}
	sender, err := pc.AddTrack(se.track)
	if err != nil {
		return fail(err)
	}
	go se.readRTCP(sender)
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return fail(err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		return fail(err)
	}
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
		return fail(errors.New("ICE gathering did not finish"))
	}

	s.mu.Lock()
	s.current = se
	s.mu.Unlock()
	go func() {
		if err := se.stream(); err != nil && ctx.Err() == nil {
			s.log().Warn("stream", "err", err)
			se.end()
		}
	}()
	return se, message{SDP: pc.LocalDescription().SDP, Codec: se.codec, Width: se.width, Height: se.height,
		Screen: screen, Screens: slices.Sorted(maps.Keys(desks)), Zoom: zoom, MaxZoom: maxZoom}, nil
}

// end stops the session: the stream, the peer connection, and every key and
// button it held.
func (se *session) end() {
	se.endOnce.Do(func() {
		se.cancel()
		se.releaseAll()
		se.routeMu.Lock()
		if se.giveBack != nil {
			se.giveBack.Stop()
		}
		if se.route != nil {
			if err := se.route.giveBack(); err != nil {
				se.s.log().Warn("giving seat0's focus back", "err", err)
			}
		}
		se.routeMu.Unlock()
		se.inMu.Lock()
		if se.in != nil {
			se.in.Close()
			se.in = nil
		}
		se.inMu.Unlock()
		if se.pc != nil {
			_ = se.pc.Close()
		}
		se.s.mu.Lock()
		if se.s.current == se {
			se.s.current = nil
		}
		se.s.mu.Unlock()
	})
}

func (se *session) requestKeyframe() {
	select {
	case se.keyframe <- struct{}{}:
	default:
	}
}

// readRTCP turns the viewer's picture-loss reports into keyframes. Reading also
// drives the interceptors (NACK, transport-wide congestion control).
func (se *session) readRTCP(sender *webrtc.RTPSender) {
	for {
		pkts, _, err := sender.ReadRTCP()
		if err != nil {
			return
		}
		for _, p := range pkts {
			switch p.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				se.requestKeyframe()
			}
		}
	}
}

// stream captures the screen whenever it changes, converts what changed, and
// encodes and sends it; a keyframe request re-encodes the current picture even
// when nothing has changed.
func (se *session) stream() error {
	log := se.s.log()
	began := time.Now()
	capt, err := desktop.OpenCapture(se.sess, se.output)
	if err != nil {
		return fmt.Errorf("capture: %w", err)
	}
	defer capt.Close()
	firstCtx, cancelFirst := context.WithTimeout(se.ctx, 5*time.Second)
	shot, err := capt.Next(firstCtx)
	cancelFirst()
	if err != nil {
		return fmt.Errorf("first capture: %w", err)
	}
	w, h := shot.Width, shot.Height
	frame := codec.NewI420(w, h)
	var frameMu sync.Mutex
	codec.ConvertXRGB(frame, shot.Pix, shot.Stride, w, h, shot.Damage, shot.YInvert)

	opt := se.s.opt
	cfg := codec.Config{Width: w, Height: h, FPS: opt.FPS, BitrateKbps: opt.StartKbps, Quality: opt.Quality,
		Threads: opt.Threads, H264Profile: "baseline"}
	var enc codec.Encoder
	if se.codec == "AV1" {
		enc, err = codec.NewAV1(cfg)
	} else {
		if strings.Contains(se.track.Codec().SDPFmtpLine, "profile-level-id=64") && opt.H264Profile != "baseline" {
			cfg.H264Profile = "high"
		}
		enc, err = codec.NewH264(cfg)
	}
	if err != nil {
		return fmt.Errorf("encoder: %w", err)
	}
	defer enc.Close()
	log.Info("streaming", "codec", enc.Name(), "size", fmt.Sprintf("%dx%d", w, h), "setup_ms", time.Since(began).Milliseconds())

	// Capture in its own goroutine, so the next copy is in flight while a frame
	// encodes. Each shot's damage is converted before asking for the next, since
	// the shot's pixels are reused.
	changed := make(chan struct{}, 1)
	captureErr := make(chan error, 1)
	go func() {
		for {
			shot, err := capt.Next(se.ctx)
			if err != nil {
				captureErr <- err
				return
			}
			frameMu.Lock()
			codec.ConvertXRGB(frame, shot.Pix, shot.Stride, w, h, shot.Damage, shot.YInvert)
			frameMu.Unlock()
			select {
			case changed <- struct{}{}:
			default: // already pending: the encoder will take the latest picture
			}
		}
	}()

	start := time.Now()
	last := start
	// Every 10 s with frames sent, one line of what was sent and what it cost.
	var st struct {
		frames, keys, bytes int
		encode, maxEncode   time.Duration
		since               time.Time
	}
	st.since = start
	minGap := time.Second / time.Duration(max(opt.FPS, 1))
	kbps := opt.StartKbps
	force := true
	for {
		if !force {
			select {
			case <-se.ctx.Done():
				return nil
			case err := <-captureErr:
				if se.ctx.Err() != nil {
					return nil
				}
				return err
			case <-se.keyframe:
				force = true
			case <-changed:
			}
		}
		select { // a keyframe request that came with a change
		case <-se.keyframe:
			force = true
		default:
		}
		if t := int(se.target.Load() / 1000); t > 0 {
			if t = min(t, opt.MaxKbps); t != kbps && (t > kbps*11/10 || t < kbps*9/10) {
				if err := enc.SetBitrate(t); err == nil {
					kbps = t
				}
			}
		}
		now := time.Now()
		frameMu.Lock()
		frame.PTS = now.Sub(start)
		pkt, err := enc.Encode(frame, force)
		frameMu.Unlock()
		if err != nil {
			return fmt.Errorf("encode: %w", err)
		}
		took := time.Since(now)
		st.frames++
		st.bytes += len(pkt.Data)
		st.encode += took
		st.maxEncode = max(st.maxEncode, took)
		if pkt.Keyframe {
			st.keys++
		}
		if el := time.Since(st.since); el >= 10*time.Second {
			log.Info("stream", "frames", st.frames, "keyframes", st.keys, "kbps", int(float64(st.bytes*8)/el.Seconds()/1000),
				"encode_ms_avg", float64(st.encode.Microseconds())/float64(st.frames)/1000, "encode_ms_max", float64(st.maxEncode.Microseconds())/1000,
				"cap_kbps", kbps)
			st.frames, st.keys, st.bytes, st.encode, st.maxEncode, st.since = 0, 0, 0, 0, 0, time.Now()
		}
		force = false
		if len(pkt.Data) > 0 {
			d := now.Sub(last)
			if d <= 0 {
				d = time.Millisecond
			}
			if err := se.track.WriteSample(media.Sample{Data: pkt.Data, Duration: d}); err != nil && !errors.Is(err, context.Canceled) {
				return fmt.Errorf("send: %w", err)
			}
			if last == start {
				log.Info("first frame sent", "bytes", len(pkt.Data), "keyframe", pkt.Keyframe, "since_start_ms", time.Since(began).Milliseconds())
			}
			last = now
		}
		// Never faster than the frame rate: the screen may change faster than is
		// worth sending, and the latest picture is what goes next.
		if wait := minGap - time.Since(now); wait > 0 {
			select {
			case <-se.ctx.Done():
				return nil
			case <-time.After(wait):
			}
		}
	}
}

// inputs is what a viewer's input acts through: a virtual pointer and keyboard on
// a seat, the pointer tied to the session's output.
type inputs interface {
	MoveTo(x, y, width, height int) error
	Press(button string) error
	Release(button string) error
	Scroll(direction string, clicks int) error
	Key(code uint32, down bool) error
	TypeText(s string, delay time.Duration) error
	ReleaseAll() error
	Err() error
	Close()
}

// device returns the session's pointer and keyboard on the owner's seat, or with
// first on the first seat for single-seat apps, created on first use
// and again if the compositor went away.
func (se *session) device(first bool) inputs {
	se.inMu.Lock()
	defer se.inMu.Unlock()
	cur := se.in
	if first {
		// One pair on the first seat per output for the server's life, never closed
		// with a session: the last keyboard to send something is the seat's, and
		// seat0 left with none crashes Chrome when it next takes focus.
		se.s.firstMu.Lock()
		defer se.s.firstMu.Unlock()
		cur = se.s.first[se.output]
	}
	if cur == nil || cur.Err() != nil {
		open := se.newInputs
		if open == nil {
			open = func(first bool) (inputs, error) {
				if first {
					return desktop.OpenVirtualInput(se.sess, desktop.InputPlace{Output: se.output})
				}
				in, err := desktop.OpenVirtualInput(se.sess, desktop.InputPlace{Seat: se.s.opt.Seat, Output: se.output})
				if err != nil && se.s.opt.Seat != "" {
					// sway not yet restarted with the owner's seat: share the first,
					// where every app has the input already.
					se.s.log().Warn("owner's seat", "err", err)
					se.route = nil
					in, err = desktop.OpenVirtualInput(se.sess, desktop.InputPlace{Output: se.output})
				}
				return in, err
			}
		}
		in, err := open(first)
		if err != nil {
			se.s.log().Warn("virtual input", "first_seat", first, "err", err)
			return nil
		}
		if cur = in; first {
			if se.s.first == nil {
				se.s.first = map[string]inputs{}
			}
			se.s.first[se.output] = in
		} else {
			se.in = in
		}
	}
	return cur
}

// setClipboard makes text the selection of the owner's seat, or with first of the
// first seat, opening the clipboard there on first use and again if the session
// went away. Without the owner's seat (sway not yet restarted with it), the first
// seat's is used, as for input.
func (s *Server) setClipboard(sess desktop.Session, first bool, text string) error {
	s.clipMu.Lock()
	defer s.clipMu.Unlock()
	c := s.clips[first]
	if c == nil || c.Err() != nil {
		if c != nil {
			c.Close()
		}
		seat := ""
		if !first {
			seat = s.opt.Seat
		}
		var err error
		c, err = desktop.OpenClipboard(sess, seat)
		if err != nil && seat != "" {
			c, err = desktop.OpenClipboard(sess, "")
		}
		if err != nil {
			return err
		}
		if s.clips == nil {
			s.clips = map[bool]*desktop.Clipboard{}
		}
		s.clips[first] = c
	}
	return c.SetText(text)
}

func (se *session) releaseAll() {
	se.inMu.Lock()
	ins := []inputs{se.in}
	se.inMu.Unlock()
	if se.route != nil {
		se.s.firstMu.Lock()
		if in := se.s.first[se.output]; in != nil {
			ins = append(ins, in)
		}
		se.s.firstMu.Unlock()
	}
	for _, in := range ins {
		if in != nil {
			_ = in.ReleaseAll()
		}
	}
}

// Input message kinds (docs/web.md).
const (
	inMove    = 0x01
	inButton  = 0x02
	inWheel   = 0x03
	inKey     = 0x04
	inText    = 0x05
	inRelease = 0x06
	inPaste   = 0x07
)

// evdev keycodes for the Ctrl+V a paste presses.
const (
	keyLeftCtrl = 29
	keyV        = 47
)

var buttonNames = []string{"left", "right", "middle"}

// input applies one message from the data channel. A malformed one is dropped.
// Each input goes through the owner's seat, or through the first seat when it is
// for a single-seat app's window; a release goes where its press went.
func (se *session) input(m []byte) {
	if len(m) == 0 {
		return
	}
	point := func(b []byte) (int, int, bool) {
		if len(b) < 4 {
			return 0, 0, false
		}
		x, y := int(binary.LittleEndian.Uint16(b)), int(binary.LittleEndian.Uint16(b[2:]))
		return x, y, x < se.width && y < se.height
	}
	se.routeMu.Lock()
	defer se.routeMu.Unlock()
	if se.firstButtons == nil {
		se.firstButtons, se.firstKeys = map[byte]bool{}, map[uint32]bool{}
	}
	pointer := func(x, y int) (bool, *desktop.Window) {
		if se.route == nil {
			return false, nil
		}
		return se.route.pointer(x, y)
	}
	keyboard := func() bool { return se.route != nil && se.route.keyboard() }
	var err error
	var in inputs
	switch body := m[1:]; m[0] {
	case inMove:
		if x, y, ok := point(body); ok {
			first, _ := pointer(x, y)
			if in = se.device(first); in != nil {
				err = in.MoveTo(x, y, se.width, se.height)
			}
		}
	case inButton:
		if len(body) < 6 || int(body[0]) >= len(buttonNames) {
			return
		}
		b, down := body[0], body[1] == 1
		x, y, ok := point(body[2:])
		if !ok {
			return
		}
		first, w := se.firstButtons[b], (*desktop.Window)(nil)
		if down {
			first, w = pointer(x, y)
		}
		if in = se.device(first); in == nil {
			return
		}
		if err = in.MoveTo(x, y, se.width, se.height); err != nil {
			break
		}
		if down {
			if se.settle > 0 {
				time.Sleep(se.settle)
			}
			if se.route != nil {
				se.route.pressed(first, w)
			}
			if first {
				se.firstButtons[b] = true
			}
			err = in.Press(buttonNames[b])
		} else {
			delete(se.firstButtons, b)
			err = in.Release(buttonNames[b])
		}
	case inWheel:
		if len(body) < 6 {
			return
		}
		dx, dy := int8(body[0]), int8(body[1])
		if x, y, ok := point(body[2:]); ok {
			first, _ := pointer(x, y)
			if in = se.device(first); in != nil {
				if err = in.MoveTo(x, y, se.width, se.height); err == nil {
					err = scroll(in, dx, dy)
				}
			}
		}
	case inKey:
		if len(body) < 3 {
			return
		}
		code, down := uint32(binary.LittleEndian.Uint16(body)), body[2] == 1
		if code == 0 || code > desktop.MaxKeycode {
			return
		}
		first := se.firstKeys[code]
		if down {
			first = keyboard()
		}
		if in = se.device(first); in == nil {
			return
		}
		if down && first {
			se.firstKeys[code] = true
		} else if !down {
			delete(se.firstKeys, code)
		}
		err = in.Key(code, down)
	case inText:
		if !utf8.Valid(body) || len(body) == 0 {
			return
		}
		if in = se.device(keyboard()); in != nil {
			err = in.TypeText(string(body), 0) // on this viewer's keyboard, whatever the characters
		}
	case inPaste:
		// The viewer's clipboard text: made the selection of the seat their keys
		// go to now, then pasted there with Ctrl+V, so the app pastes it as it
		// would any clipboard, newlines and all.
		if !utf8.Valid(body) || len(body) == 0 {
			return
		}
		first := keyboard()
		set := se.clipboard
		if set == nil {
			set = func(first bool, text string) error { return se.s.setClipboard(se.sess, first, text) }
		}
		if err = set(first, string(body)); err != nil {
			break
		}
		if in = se.device(first); in == nil {
			return
		}
		for _, k := range []struct {
			code uint32
			down bool
		}{{keyLeftCtrl, true}, {keyV, true}, {keyV, false}, {keyLeftCtrl, false}} {
			if err = in.Key(k.code, k.down); err != nil {
				break
			}
		}
	case inRelease:
		se.releaseAll()
		clear(se.firstButtons)
		clear(se.firstKeys)
	}
	if err != nil {
		se.s.log().Warn("input", "kind", m[0], "err", err)
	}
	if se.route != nil && se.route.lent != 0 {
		se.armGiveBack()
	}
}

// armGiveBack (re)starts the wait after which seat0's focus goes back to the
// agents. Called with routeMu held.
func (se *session) armGiveBack() {
	if se.giveBack == nil {
		se.giveBack = time.AfterFunc(lendIdle, se.tryGiveBack)
		return
	}
	se.giveBack.Reset(lendIdle)
}

// tryGiveBack gives seat0's focus back once the owner has left it unused for
// lendIdle with nothing held on the first seat.
func (se *session) tryGiveBack() {
	se.routeMu.Lock()
	defer se.routeMu.Unlock()
	r := se.route
	if r == nil || r.lent == 0 || se.ctx.Err() != nil {
		return
	}
	if len(se.firstButtons) > 0 || len(se.firstKeys) > 0 || !r.due() {
		se.giveBack.Reset(lendIdle)
		return
	}
	if err := r.giveBack(); err != nil {
		se.s.log().Warn("giving seat0's focus back", "err", err)
	}
}

func scroll(in inputs, dx, dy int8) error {
	steps := func(n int8, pos, neg string) error {
		dir := pos
		if n < 0 {
			dir, n = neg, -n
		}
		if n == 0 {
			return nil
		}
		return in.Scroll(dir, int(n))
	}
	if err := steps(dy, "down", "up"); err != nil {
		return err
	}
	return steps(dx, "right", "left")
}

// windows lists every window, and which desktop it is on.
func (se *session) windows() []windowInfo {
	tree, err := se.s.live.Tree()
	if err != nil {
		return nil
	}
	screens := map[string]string{}
	for d, o := range se.desks {
		screens[o] = d
	}
	var out []windowInfo
	for _, w := range tree.Windows() {
		out = append(out, windowInfo{ID: w.ID, Title: w.Name, App: w.App(), Screen: cmp.Or(screens[w.Output], w.Output)})
	}
	return out
}

// move moves a window to a desktop, keeping the agents' focus where it was:
// sway's commands act on the agents' seat, and its focus can follow the window.
func (se *session) move(id int64, to string) error {
	output, ok := se.desks[to]
	if !ok {
		return fmt.Errorf("no desktop %q", to)
	}
	tree, err := se.s.live.Tree()
	if err != nil {
		return err
	}
	w := tree.Find(id)
	if w == nil {
		return fmt.Errorf("no window %d", id)
	}
	if w.Output == output {
		return nil
	}
	cmd := fmt.Sprintf("[con_id=%d] move container to output %s", w.ID, output) + keepAgentsFocus(tree, w.ID, to == "agent", se.desks["agent"])
	return se.s.live.Command(cmd)
}

// keepAgentsFocus is what to add to a move of window moved so that seat0's focus
// stays with the agents: on the window it had, when that is another one on their
// output; and when the window goes away from them and they had no other, on their
// output itself, since sway's focus would follow the window (as it did when the
// agents had no window open and Steam was moved to the owner's desktop).
func keepAgentsFocus(tree *desktop.Node, moved int64, toAgents bool, agentOutput string) string {
	if f := tree.FocusedWindow(); f != nil && f.ID != moved && (toAgents || f.Output == agentOutput) {
		return fmt.Sprintf("; [con_id=%d] focus", f.ID)
	}
	if toAgents || agentOutput == "" {
		return ""
	}
	return "; focus output " + agentOutput
}
