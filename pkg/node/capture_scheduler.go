package node

import (
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// captureScheduler runs capture autonomously on a schedule.
// All settings come from gRPC CaptureRefresh — no hardcoded defaults.
type captureScheduler struct {
	mu     sync.Mutex
	logger zerolog.Logger
	stopCh chan struct{}

	// captureFunc is the actual capture implementation (injected, platform-specific).
	captureFunc func(iface string, domains []string, countPerDomain int, timeout time.Duration) (int, error)
}

func newCaptureScheduler(logger zerolog.Logger, captureFn func(string, []string, int, time.Duration) (int, error)) *captureScheduler {
	return &captureScheduler{
		logger:      logger,
		captureFunc: captureFn,
	}
}

// SetSchedule configures and starts (or restarts) the capture ticker.
// schedule is parsed as a Go duration (e.g. "24h", "6h", "30m").
// All parameters come from the gRPC request — nothing is hardcoded.
func (cs *captureScheduler) SetSchedule(domains []string, countPerDomain int, schedule string, retentionDays int) error {
	interval, err := time.ParseDuration(schedule)
	if err != nil {
		return err
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()

	// Stop existing schedule if any.
	if cs.stopCh != nil {
		close(cs.stopCh)
	}

	cs.stopCh = make(chan struct{})
	stopCh := cs.stopCh

	cs.logger.Info().
		Str("interval", interval.String()).
		Int("domains", len(domains)).
		Int("count_per_domain", countPerDomain).
		Int("retention_days", retentionDays).
		Msg("capture schedule started")

	go cs.run(stopCh, interval, domains, countPerDomain)

	return nil
}

// StopSchedule stops the running capture schedule.
func (cs *captureScheduler) StopSchedule() {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if cs.stopCh != nil {
		close(cs.stopCh)
		cs.stopCh = nil
		cs.logger.Info().Msg("capture schedule stopped")
	}
}

func (cs *captureScheduler) run(stopCh <-chan struct{}, interval time.Duration, domains []string, countPerDomain int) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			if cs.captureFunc == nil {
				cs.logger.Warn().Msg("scheduled capture skipped: no capture function")
				continue
			}

			cs.logger.Info().Int("domains", len(domains)).Msg("scheduled capture starting")

			count, err := cs.captureFunc("", domains, countPerDomain, 30*time.Second)
			if err != nil {
				cs.logger.Error().Err(err).Msg("scheduled capture failed")
				continue
			}

			cs.logger.Info().Int("captured", count).Msg("scheduled capture completed")
		}
	}
}
