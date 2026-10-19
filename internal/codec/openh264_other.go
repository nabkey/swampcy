//go:build !linux && !darwin

package codec

import "fmt"

// NewH264 is unavailable where purego cannot load OpenH264.
func NewH264(cfg Config) (Encoder, error) {
	return nil, fmt.Errorf("openh264: %w on this platform", ErrUnavailable)
}
