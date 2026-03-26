package node

import (
	"context"
	"os/exec"
	"strconv"
	"time"

	"github.com/rs/zerolog"
)

const (
	defaultHealthInterval         = 10 * time.Second
	defaultHealthTimeout          = 3 * time.Second
	defaultHealthFailureThreshold = 3
)

// HealthConfig holds configuration for the health checker.
type HealthConfig struct {
	Interval         time.Duration
	Timeout          time.Duration
	FailureThreshold int
}

// HealthChecker monitors tunnel liveness and fires callbacks on state transitions.
type HealthChecker struct {
	cfg    HealthConfig
	logger zerolog.Logger
}

// NewHealthChecker creates a new HealthChecker with the given configuration.
func NewHealthChecker(cfg HealthConfig, logger zerolog.Logger) *HealthChecker {
	return &HealthChecker{
		cfg:    cfg,
		logger: logger,
	}
}

// Run starts the healthcheck loop. It calls onDown when a tunnel fails consecutively
// cfg.FailureThreshold times, and onUp when it recovers. Blocks until ctx is cancelled.
func (h *HealthChecker) Run(
	ctx context.Context,
	tunnels func() []MasterTunnel,
	onDown func(name string),
	onUp func(name string),
) {
	failures := make(map[string]int)
	ticker := time.NewTicker(h.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, t := range tunnels() {
				if t.OverlayIP == "" {
					continue
				}

				alive := PingOverlay(t.OverlayIP, h.cfg.Timeout)

				if alive {
					if failures[t.Name] > 0 {
						h.logger.Info().
							Str("tunnel", t.Name).
							Str("overlay_ip", t.OverlayIP).
							Msg("tunnel recovered")
						onUp(t.Name)
					}
					failures[t.Name] = 0
				} else {
					failures[t.Name]++
					h.logger.Warn().
						Str("tunnel", t.Name).
						Str("overlay_ip", t.OverlayIP).
						Int("consecutive_failures", failures[t.Name]).
						Msg("tunnel ping failed")

					if failures[t.Name] >= h.cfg.FailureThreshold && t.Healthy {
						h.logger.Error().
							Str("tunnel", t.Name).
							Str("overlay_ip", t.OverlayIP).
							Msg("tunnel marked down")
						onDown(t.Name)
					}
				}
			}
		}
	}
}

// PingOverlay sends a single ICMP echo to ip and returns true if it succeeds.
// timeout controls how long to wait for a reply (minimum 1 second).
func PingOverlay(ip string, timeout time.Duration) bool {
	seconds := int(timeout.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	cmd := exec.Command("ping", "-c", "1", "-W", strconv.Itoa(seconds), ip)
	return cmd.Run() == nil
}
