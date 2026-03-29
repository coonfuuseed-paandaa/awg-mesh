package node

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestNewHealthChecker(t *testing.T) {
	t.Parallel()

	cfg := HealthConfig{
		Interval:         2 * time.Second,
		Timeout:          500 * time.Millisecond,
		FailureThreshold: 5,
	}

	checker := NewHealthChecker(cfg, zerolog.Nop(), nil)
	if checker == nil {
		t.Fatal("expected checker instance")
	}
	if checker.cfg != cfg {
		t.Fatalf("unexpected checker config: got %#v want %#v", checker.cfg, cfg)
	}
}

func TestPingOverlayInvalidIP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ip   string
	}{
		{name: "empty", ip: ""},
		{name: "hostname", ip: "example.com"},
		{name: "invalid octets", ip: "999.1.2.3"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if PingOverlay(tt.ip, 200*time.Millisecond) {
				t.Fatalf("expected PingOverlay(%q) to return false", tt.ip)
			}
		})
	}
}

func TestPingICMPInvalidIP(t *testing.T) {
	t.Parallel()

	_, err := PingICMP(context.Background(), "not-an-ip", time.Second)
	if err == nil {
		t.Fatal("expected error for invalid IP")
	}
}

func TestPingICMPLocalhost(t *testing.T) {
	t.Parallel()

	alive, err := PingICMP(context.Background(), "127.0.0.1", 2*time.Second)
	if err != nil {
		t.Skipf("ICMP not available (need CAP_NET_RAW): %v", err)
	}
	if !alive {
		t.Fatal("expected localhost ping to succeed")
	}
}

func TestPingICMPUnreachable(t *testing.T) {
	t.Parallel()

	// 192.0.2.1 is TEST-NET-1 (RFC 5737) — guaranteed non-routable
	alive, err := PingICMP(context.Background(), "192.0.2.1", 500*time.Millisecond)
	if err != nil {
		t.Skipf("ICMP not available: %v", err)
	}
	if alive {
		t.Fatal("expected unreachable host to return false")
	}
}

func TestPingICMPContextCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	alive, _ := PingICMP(ctx, "127.0.0.1", 5*time.Second)
	if alive {
		// May or may not succeed depending on timing — not asserting false
		// Just verify it doesn't hang
		t.Log("ping succeeded despite cancelled context (fast local response)")
	}
}

func TestPurgeStaleFailures(t *testing.T) {
	t.Parallel()

	failures := map[string]int{
		"tunnel-a": 5,
		"tunnel-b": 2,
		"tunnel-c": 10,
	}

	currentTargets := []HealthTarget{
		{Name: "tunnel-a", PingAddr: "10.0.0.1"},
		{Name: "tunnel-d", PingAddr: "10.0.0.4"},
	}

	purgeStaleFailures(currentTargets, failures)

	if _, ok := failures["tunnel-b"]; ok {
		t.Fatal("tunnel-b should have been purged")
	}
	if _, ok := failures["tunnel-c"]; ok {
		t.Fatal("tunnel-c should have been purged")
	}
	if failures["tunnel-a"] != 5 {
		t.Fatalf("tunnel-a should be preserved with count 5, got %d", failures["tunnel-a"])
	}
	if _, ok := failures["tunnel-d"]; ok {
		t.Fatal("tunnel-d should not be in failures map (never failed)")
	}
}

func TestPingAllParallelCompletesInBoundedTime(t *testing.T) {
	t.Parallel()

	hc := NewHealthChecker(HealthConfig{
		Timeout: 500 * time.Millisecond,
	}, zerolog.Nop(), nil)

	// 10 unreachable targets — if sequential, would take 5s; parallel should be ~500ms
	targets := make([]HealthTarget, 10)
	for i := range targets {
		targets[i] = HealthTarget{
			Name:     "target-" + string(rune('a'+i)),
			PingAddr: "192.0.2." + string(rune('1'+i)), // TEST-NET-1
		}
	}

	start := time.Now()
	results := hc.pingAllParallel(context.Background(), targets)
	elapsed := time.Since(start)

	// All should be false (unreachable) or error (no CAP_NET_RAW)
	for i, r := range results {
		if r {
			t.Logf("target %d unexpectedly alive (skipping timing check)", i)
			return
		}
	}

	// Parallel: should complete in ~1× timeout, not 10× timeout
	maxExpected := 3 * time.Second // generous bound (500ms × 1 + overhead)
	if elapsed > maxExpected {
		t.Fatalf("parallel ping took %v, expected < %v (sequential would be ~5s)", elapsed, maxExpected)
	}
	t.Logf("10 parallel pings completed in %v", elapsed)
}
