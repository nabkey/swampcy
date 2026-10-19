//go:build !linux && !darwin

package codec

import "fmt"

// NewAV1 is unavailable where purego cannot load libaom.
func NewAV1(cfg Config) (Encoder, error) {
	return nil, fmt.Errorf("libaom: %w on this platform", ErrUnavailable)
}
