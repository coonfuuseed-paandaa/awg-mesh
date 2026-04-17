package node

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
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

// captureScheduleMinInterval is the floor for both duration and cron-derived
// intervals. A cron expression like "* * * * *" (every minute) is permitted
// but "@every 1ns" or a sub-minute Duration is not.
const captureScheduleMinInterval = time.Minute

// cronParser recognises the standard 5-field cron form ("0 3 * * *") as well
// as robfig/cron's "@every <duration>" and "@hourly/@daily/..." descriptors.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// SetSchedule configures and starts (or restarts) the capture ticker.
// schedule accepts either a Go duration ("24h", "6h", "30m") or a standard
// 5-field cron expression ("0 3 * * *"). The example topology and README both
// document the cron form, so we accept both to match docs and stay
// backwards-compatible with any operator pinned on Duration strings.
//
// All parameters come from the gRPC request — nothing is hardcoded.
func (cs *captureScheduler) SetSchedule(domains []string, countPerDomain int, schedule string, retentionDays int) error {
	schedule = strings.TrimSpace(schedule)
	if schedule == "" {
		return fmt.Errorf("capture schedule is empty")
	}

	ticker, scheduleKind, err := buildCaptureTicker(schedule)
	if err != nil {
		return err
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
		Str("schedule", schedule).
		Str("schedule_kind", scheduleKind).
		Int("domains", len(domains)).
		Int("count_per_domain", countPerDomain).
		Int("retention_days", retentionDays).
		Msg("capture schedule started")

	go cs.run(stopCh, doneCh, ticker, domains, countPerDomain)

	return nil
}

// captureTicker abstracts over time.Ticker (for Duration schedules) and a
// cron-driven delayed channel (for cron schedules) so run() does not care
// which style the operator chose.
type captureTicker interface {
	// next returns a channel that fires once at the next scheduled tick.
	// Callers must call next() again after each tick to arm the next one.
	next() <-chan time.Time
	stop()
}

type durationTicker struct {
	t *time.Ticker
}

func (d *durationTicker) next() <-chan time.Time { return d.t.C }
func (d *durationTicker) stop()                  { d.t.Stop() }

type cronTicker struct {
	schedule cron.Schedule
	timer    *time.Timer
}

func (c *cronTicker) next() <-chan time.Time {
	now := time.Now()
	next := c.schedule.Next(now)
	if c.timer == nil {
		c.timer = time.NewTimer(next.Sub(now))
	} else {
		if !c.timer.Stop() {
			select {
			case <-c.timer.C:
			default:
			}
		}
		c.timer.Reset(next.Sub(now))
	}
	return c.timer.C
}

func (c *cronTicker) stop() {
	if c.timer != nil {
		c.timer.Stop()
	}
}

// buildCaptureTicker picks the right ticker implementation for the schedule
// string and enforces the 1-minute floor regardless of syntax.
func buildCaptureTicker(schedule string) (captureTicker, string, error) {
	if interval, err := time.ParseDuration(schedule); err == nil {
		if interval < captureScheduleMinInterval {
			return nil, "", fmt.Errorf("capture schedule interval must be at least %v, got %v", captureScheduleMinInterval, interval)
		}
		return &durationTicker{t: time.NewTicker(interval)}, "duration", nil
	}

	sched, err := cronParser.Parse(schedule)
	if err != nil {
		return nil, "", fmt.Errorf("capture schedule %q is neither a Go duration nor a cron expression: %w", schedule, err)
	}

	// Reject cron expressions whose inter-tick interval is strictly less
	// than one minute. "* * * * *" (every minute) fires exactly 60s apart
	// and is accepted; sub-minute descriptors like "@every 30s" (if ever
	// enabled by a future parser flag) would be rejected here.
	now := time.Now()
	first := sched.Next(now)
	second := sched.Next(first)
	if second.Sub(first) < captureScheduleMinInterval {
		return nil, "", fmt.Errorf("capture schedule %q fires faster than %v between ticks", schedule, captureScheduleMinInterval)
	}

	return &cronTicker{schedule: sched}, "cron", nil
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

func (cs *captureScheduler) run(stopCh <-chan struct{}, doneCh chan<- struct{}, ticker captureTicker, domains []string, countPerDomain int) {
	defer close(doneCh)
	defer ticker.stop()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.next():
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
