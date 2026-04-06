package node

import (
	"fmt"
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
	doneCh chan struct{}

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
	if interval < time.Minute {
		return fmt.Errorf("capture schedule interval must be at least 1 minute, got %v", interval)
	}
	if len(domains) > 100 {
		return fmt.Errorf("capture domain count must be at most 100, got %d", len(domains))
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()

	// Stop existing schedule if any, waiting for its goroutine to finish.
	// Nil out the fields first so any concurrent caller (StopSchedule or
	// another SetSchedule) sees a clean state and cannot double-close.
	if cs.stopCh != nil {
		stopCh := cs.stopCh
		doneCh := cs.doneCh
		cs.stopCh = nil
		cs.doneCh = nil
		close(stopCh)
		<-doneCh // safe to block here: run() never acquires cs.mu
	}

	cs.stopCh = make(chan struct{})
	cs.doneCh = make(chan struct{})
	stopCh := cs.stopCh
	doneCh := cs.doneCh

	cs.logger.Info().
		Str("interval", interval.String()).
		Int("domains", len(domains)).
		Int("count_per_domain", countPerDomain).
		Int("retention_days", retentionDays).
		Msg("capture schedule started")

	go cs.run(stopCh, doneCh, interval, domains, countPerDomain)

	return nil
}

// StopSchedule stops the running capture schedule and blocks until the
// scheduler's goroutine has fully exited. This ensures captureFunc cannot
// race into closed tunnel interfaces after StopSchedule returns.
func (cs *captureScheduler) StopSchedule() {
	cs.mu.Lock()
	stopCh := cs.stopCh
	doneCh := cs.doneCh
	cs.stopCh = nil
	cs.doneCh = nil
	cs.mu.Unlock()

	if stopCh != nil {
		close(stopCh)
		<-doneCh
		cs.logger.Info().Msg("capture schedule stopped")
	}
}

func (cs *captureScheduler) run(stopCh <-chan struct{}, doneCh chan<- struct{}, interval time.Duration, domains []string, countPerDomain int) {
	defer close(doneCh)
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
