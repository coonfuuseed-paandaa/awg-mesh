//go:build !linux

package awggen

import (
	"errors"
	"time"
)

// CaptureConfig configures a TLS/QUIC packet capture session.
type CaptureConfig struct {
	Interface      string
	Domains        []string
	CountPerDomain int
	Timeout        time.Duration
}

// CaptureResult holds a single captured packet record.
type CaptureResult struct {
	Domain    string
	Protocol  string
	Data      []byte
	Timestamp time.Time
}

// Capture captures packets from system interface on Linux.
// On non-Linux platforms, this is unavailable.
func Capture(cfg CaptureConfig) ([]CaptureResult, error) {
	if cfg.CountPerDomain < 0 {
		return nil, errors.New("capture: count_per_domain must be non-negative")
	}

	return nil, errors.New("capture: not supported on this platform")
}
