// Package codec encodes swampcy's screen for the web viewer: AV1 through libaom and
// H.264 through OpenH264, both loaded at run time with purego, so swampcy needs no
// cgo and runs (without the viewer) where neither library is installed.
package codec

import (
	"errors"
	"time"
)

// Config is what an encoder is opened with. Sizes are fixed for its life; the
// bitrate can change.
type Config struct {
	Width, Height int
	// FPS is the frame rate the rate control plans for. Frames arrive only when
	// the screen changes, at most this often, each with its own timestamp.
	FPS int
	// BitrateKbps caps the stream; the network's estimate moves it at run time.
	BitrateKbps int
	// Quality is a constant-quality target where the codec has one (AV1's cq-level,
	// 0 best to 63 worst). The bitrate cap still applies. 0 means the default (24).
	Quality int
	// Threads is how many threads the encoder may use; 0 means its default.
	Threads int
	// H264Profile is "high" (CABAC) or "baseline" (CAVLC, constrained baseline).
	// Ignored by AV1.
	H264Profile string
}

// Frame is one picture in I420: full-range BT.709 luma and 2x2-subsampled chroma.
type Frame struct {
	Y, U, V          []byte
	YStride, UStride int // UStride is also V's
	PTS              time.Duration
}

// Packet is one encoded frame: for AV1 the temporal unit's OBUs (low-overhead
// format, what pion's AV1 payloader takes); for H.264 the access unit's NAL units
// in Annex B (start codes), what pion's H.264 payloader takes.
type Packet struct {
	Data     []byte
	Keyframe bool
	PTS      time.Duration
}

// Encoder turns frames into packets, one per frame, with no lookahead.
type Encoder interface {
	// Encode encodes f, as a keyframe when forceKey is set. The encoder does not
	// keep f after returning.
	Encode(f *Frame, forceKey bool) (Packet, error)
	// SetBitrate changes the cap, in kbit/s, from the next frame on.
	SetBitrate(kbps int) error
	// Name is the codec's name as WebRTC knows it: "AV1" or "H264".
	Name() string
	Close() error
}

// ErrUnavailable means the codec's library could not be loaded.
var ErrUnavailable = errors.New("codec library not available")

// NewI420 allocates a frame of the given size.
func NewI420(width, height int) *Frame {
	cw, ch := (width+1)/2, (height+1)/2
	return &Frame{
		Y: make([]byte, width*height), U: make([]byte, cw*ch), V: make([]byte, cw*ch),
		YStride: width, UStride: cw,
	}
}
