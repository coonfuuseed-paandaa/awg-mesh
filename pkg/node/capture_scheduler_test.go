package node

import (
	"strings"
	"testing"
	"time"
)

func TestBuildCaptureTicker_Duration(t *testing.T) {
	t.Parallel()
	ticker, kind, err := buildCaptureTicker("24h")
	if err != nil {
		t.Fatalf("24h should parse as Duration: %v", err)
	}
	if kind != "duration" {
		t.Fatalf("expected kind=duration, got %q", kind)
	}
	defer ticker.stop()
}

func TestBuildCaptureTicker_Cron(t *testing.T) {
	t.Parallel()
	// This is exactly the schedule in the example topology and README.
	ticker, kind, err := buildCaptureTicker("0 3 * * *")
	if err != nil {
		t.Fatalf("standard cron form should parse: %v", err)
	}
	if kind != "cron" {
		t.Fatalf("expected kind=cron, got %q", kind)
	}
	defer ticker.stop()

	// next() must arm and tick within a reasonable window. Don't wait for 3
	// AM — just check the channel is non-nil and stays unarmed for the
	// immediate future.
	ch := ticker.next()
	if ch == nil {
		t.Fatal("next() returned nil channel")
	}
	select {
	case <-ch:
		t.Fatal("cron 0 3 * * * should not fire immediately")
	case <-time.After(50 * time.Millisecond):
		// expected: timer is armed, not yet due
	}
}

func TestBuildCaptureTicker_CronDescriptor(t *testing.T) {
	t.Parallel()
	ticker, kind, err := buildCaptureTicker("@daily")
	if err != nil {
		t.Fatalf("@daily should parse: %v", err)
	}
	if kind != "cron" {
		t.Fatalf("expected kind=cron for @daily, got %q", kind)
	}
	defer ticker.stop()
}

func TestBuildCaptureTicker_EveryDescriptor(t *testing.T) {
	t.Parallel()
	ticker, kind, err := buildCaptureTicker("@every 5m")
	if err != nil {
		t.Fatalf("@every 5m should parse: %v", err)
	}
	if kind != "cron" {
		t.Fatalf("expected kind=cron for @every, got %q", kind)
	}
	defer ticker.stop()
}

func TestBuildCaptureTicker_Garbage(t *testing.T) {
	t.Parallel()
	_, _, err := buildCaptureTicker("not a schedule")
	if err == nil {
		t.Fatal("expected error for garbage schedule")
	}
	if !strings.Contains(err.Error(), "neither a Go duration nor a cron expression") {
		t.Fatalf("error message should explain both forms: %v", err)
	}
}

func TestBuildCaptureTicker_SubMinuteDurationRejected(t *testing.T) {
	t.Parallel()
	_, _, err := buildCaptureTicker("30s")
	if err == nil {
		t.Fatal("expected sub-minute Duration to be rejected")
	}
}

func TestBuildCaptureTicker_EmptyRejected(t *testing.T) {
	t.Parallel()
	// Empty is rejected at SetSchedule level, but buildCaptureTicker must
	// also reject it so direct callers can't bypass the guard.
	_, _, err := buildCaptureTicker("")
	if err == nil {
		t.Fatal("expected empty schedule to be rejected")
	}
}

func TestBuildCaptureTicker_CronFiresEvery60s(t *testing.T) {
	t.Parallel()
	// "* * * * *" is the tightest legal 5-field cron — it ticks every minute.
	// It must pass the 1-minute floor because consecutive ticks are exactly
	// 60 seconds apart, which equals — not exceeds — the floor.
	ticker, _, err := buildCaptureTicker("* * * * *")
	if err != nil {
		t.Fatalf("every-minute cron should be accepted: %v", err)
	}
	ticker.stop()
}
