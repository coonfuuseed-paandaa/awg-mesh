package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	proto "github.com/coonfuuseed-paandaa/awg-mesh/proto"
)

// TestStatusCommandRegistered verifies that 'mesh-ctl status' is registered
// and the --verify-data-plane flag is available.
//
// Anti-stub: if newStatusCommand() is not added to NewRootCommand, Execute()
// returns "unknown command" instead of a flag / topology error.
func TestStatusCommandRegistered(t *testing.T) {
	// No t.Parallel — cobra persistent flags bind to package-level globals.

	t.Run("no-topology returns topology error not unknown-command", func(t *testing.T) {
		root := NewRootCommand("test")
		root.SilenceUsage = true
		root.SilenceErrors = true
		root.SetArgs([]string{"status", "--topology", "/nonexistent/topology.yml"})

		err := root.Execute()
		if err == nil {
			t.Fatal("expected error for nonexistent topology, got nil")
		}
		if strings.Contains(err.Error(), "unknown command") {
			t.Errorf("got 'unknown command' — newStatusCommand not registered: %v", err)
		}
	})

	t.Run("--verify-data-plane flag accepted without error about unknown flag", func(t *testing.T) {
		root := NewRootCommand("test")
		root.SilenceUsage = true
		root.SilenceErrors = true
		root.SetArgs([]string{"status", "--verify-data-plane", "--topology", "/nonexistent/topology.yml"})

		err := root.Execute()
		if err == nil {
			t.Fatal("expected topology error, got nil")
		}
		if strings.Contains(err.Error(), "unknown flag") {
			t.Errorf("--verify-data-plane is not registered: %v", err)
		}
	})

	t.Run("--timeout flag accepted", func(t *testing.T) {
		root := NewRootCommand("test")
		root.SilenceUsage = true
		root.SilenceErrors = true
		root.SetArgs([]string{"status", "--timeout", "10s", "--topology", "/nonexistent/topology.yml"})

		err := root.Execute()
		if err == nil {
			t.Fatal("expected topology error, got nil")
		}
		if strings.Contains(err.Error(), "unknown flag") {
			t.Errorf("--timeout is not registered: %v", err)
		}
	})

	t.Run("--concurrency flag accepted", func(t *testing.T) {
		root := NewRootCommand("test")
		root.SilenceUsage = true
		root.SilenceErrors = true
		root.SetArgs([]string{"status", "--concurrency", "8", "--topology", "/nonexistent/topology.yml"})

		err := root.Execute()
		if err == nil {
			t.Fatal("expected topology error, got nil")
		}
		if strings.Contains(err.Error(), "unknown flag") {
			t.Errorf("--concurrency is not registered: %v", err)
		}
	})
}

// TestClassifyTunnelHealth verifies that classifyTunnelHealth returns the correct
// structured reason code for each combination of health state.
//
// Anti-stub: replacing classifyTunnelHealth with `return ""` makes every
// non-healthy case fail; replacing with `return "unreachable"` makes the
// healthy and missing cases fail.
func TestClassifyTunnelHealth(t *testing.T) {
	t.Parallel()

	now := time.Now()

	// A pubkey pair where master's live key differs from admin-stored key.
	adminKeyHex := strings.Repeat("aa", 32) // 64 hex chars = 32 bytes
	matchingKey := make([]byte, 32)
	for i := range matchingKey {
		matchingKey[i] = 0xAA
	}
	differentKey := make([]byte, 32)
	differentKey[0] = 0xBB

	// Stale last_check_ms: older than handshakeStaleThreshold.
	staleMs := now.Add(-(handshakeStaleThreshold + time.Second)).UnixMilli()

	// Recent last_check_ms: within threshold.
	recentMs := now.Add(-time.Second).UnixMilli()

	cases := []struct {
		name         string
		h            *proto.TunnelHealth
		masterPeerKey []byte
		adminKeyHex  string
		want         string
	}{
		{
			name: "nil TunnelHealth → missing_peer",
			h:    nil,
			want: "missing_peer",
		},
		{
			name: "healthy=true → empty (no drift)",
			h:    &proto.TunnelHealth{Name: "ep-1", Healthy: true},
			want: "",
		},
		{
			name: "unhealthy + matching keys → not key_mismatch (falls through to unreachable)",
			h:    &proto.TunnelHealth{Name: "ep-1", Healthy: false},
			masterPeerKey: matchingKey,
			adminKeyHex:  adminKeyHex,
			want: "unreachable",
		},
		{
			name: "unhealthy + key mismatch → key_mismatch",
			h:    &proto.TunnelHealth{Name: "ep-1", Healthy: false},
			masterPeerKey: differentKey,
			adminKeyHex:  adminKeyHex,
			want: "key_mismatch",
		},
		{
			name: "unhealthy + stale last_check_ms → handshake_timeout",
			h:    &proto.TunnelHealth{Name: "ep-1", Healthy: false, LastCheckMs: staleMs},
			want: "handshake_timeout",
		},
		{
			name: "unhealthy + recent last_check_ms → unreachable (not yet stale)",
			h:    &proto.TunnelHealth{Name: "ep-1", Healthy: false, LastCheckMs: recentMs},
			want: "unreachable",
		},
		{
			name: "unhealthy + consecutive_failures > 0 → handshake_timeout",
			h:    &proto.TunnelHealth{Name: "ep-1", Healthy: false, ConsecutiveFailures: 3},
			want: "handshake_timeout",
		},
		{
			name: "unhealthy + no extra info → unreachable",
			h:    &proto.TunnelHealth{Name: "ep-1", Healthy: false},
			want: "unreachable",
		},
		{
			name: "unhealthy + admin key empty → key check skipped",
			h:    &proto.TunnelHealth{Name: "ep-1", Healthy: false},
			masterPeerKey: differentKey,
			adminKeyHex:  "", // no admin key → skip key check
			want: "unreachable",
		},
		{
			name: "unhealthy + masterPeerKey wrong length → key check skipped",
			h:    &proto.TunnelHealth{Name: "ep-1", Healthy: false},
			masterPeerKey: differentKey[:16], // only 16 bytes → not a valid WG key
			adminKeyHex:  adminKeyHex,
			want: "unreachable",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyTunnelHealth(tc.h, tc.masterPeerKey, tc.adminKeyHex, now)
			if got != tc.want {
				t.Errorf("classifyTunnelHealth(%+v, ...) = %q, want %q", tc.h, got, tc.want)
			}
		})
	}
}

// TestRunDataPlaneProbesNoMasters verifies that runDataPlaneProbes returns an
// empty slice when the topology has no masters.
//
// Anti-stub: replacing runDataPlaneProbes with `return nil` makes a non-empty
// master topology test fail (not tested here, but the healthy case would).
// This test confirms the zero case at minimum.
func TestRunDataPlaneProbesNoMasters(t *testing.T) {
	t.Parallel()

	topo := &topology.Topology{}
	results := runDataPlaneProbes(topo, t.TempDir(), 2*time.Second, 2)
	if len(results) != 0 {
		t.Errorf("want 0 results for empty topology, got %d", len(results))
	}
}

// TestRunDataPlaneProbesMasterNoEndpoints verifies that masters with zero
// bound endpoints are skipped without producing results.
//
// Anti-stub: if the endpoint loop is removed, a master with no endpoints
// would still produce 0 results — so this is a structure test, not a behavior
// test. The real value is ensuring the semaphore path is exercised.
func TestRunDataPlaneProbesMasterNoEndpoints(t *testing.T) {
	t.Parallel()

	topo := &topology.Topology{
		Masters: []topology.MasterNode{
			{Name: "master-1", Host: "10.0.0.1", OverlayIP: "10.1.0.1", ListenPort: 51820, Endpoints: nil},
		},
	}
	results := runDataPlaneProbes(topo, t.TempDir(), 2*time.Second, 2)
	if len(results) != 0 {
		t.Errorf("want 0 results for master with no endpoints, got %d", len(results))
	}
}

// TestRunDataPlaneProbesMasterNoToken verifies that when a master has no token
// file, all its endpoint pairs are reported as "unreachable" (not silently dropped).
//
// Anti-stub: if probeNodePairs returns nil for missing token instead of
// len(master.Endpoints) "unreachable" entries, this test fails.
func TestRunDataPlaneProbesMasterNoToken(t *testing.T) {
	t.Parallel()

	topo := &topology.Topology{
		Masters: []topology.MasterNode{
			{
				Name:      "master-1",
				Host:      "10.0.0.1",
				OverlayIP: "10.1.0.1",
				ListenPort: 51820,
				Endpoints: []string{"ep-1", "ep-2"},
			},
		},
		Endpoints: []topology.EndpointNode{
			{Name: "ep-1", Host: "10.0.0.2", OverlayIP: "10.1.0.2", ListenPort: 51820},
			{Name: "ep-2", Host: "10.0.0.3", OverlayIP: "10.1.0.3", ListenPort: 51820},
		},
	}

	// cfgDir has no token files → every master pair should be "unreachable"
	cfgDir := t.TempDir()
	results := runDataPlaneProbes(topo, cfgDir, 2*time.Second, 2)

	if len(results) != 2 {
		t.Fatalf("want 2 results (one per endpoint), got %d", len(results))
	}
	for _, r := range results {
		if r.masterName != "master-1" {
			t.Errorf("want masterName=master-1, got %q", r.masterName)
		}
		if r.reason != "unreachable" {
			t.Errorf("want reason=unreachable for missing token, got %q", r.reason)
		}
	}
}

// TestRunDataPlaneProbesConcurrency verifies that runDataPlaneProbes handles
// maxConcurrency ≤ 0 gracefully (defaults to 4) and does not deadlock.
//
// Anti-stub: if sem channel is nil or unbuffered, goroutines deadlock →
// test times out rather than passing.
func TestRunDataPlaneProbesConcurrency(t *testing.T) {
	t.Parallel()

	topo := &topology.Topology{
		Masters: []topology.MasterNode{
			{Name: "m1", Host: "10.0.0.1", OverlayIP: "10.1.0.1", ListenPort: 51820, Endpoints: []string{"ep-1"}},
			{Name: "m2", Host: "10.0.0.2", OverlayIP: "10.1.0.2", ListenPort: 51820, Endpoints: []string{"ep-1"}},
		},
		Endpoints: []topology.EndpointNode{
			{Name: "ep-1", Host: "10.0.0.10", OverlayIP: "10.1.0.10", ListenPort: 51820},
		},
	}

	// maxConcurrency=0 should default to 4 and not deadlock.
	cfgDir := t.TempDir()
	results := runDataPlaneProbes(topo, cfgDir, 500*time.Millisecond, 0)

	// Both masters have no token → 2 "unreachable" results.
	if len(results) != 2 {
		t.Fatalf("want 2 results (one per master/ep pair), got %d", len(results))
	}
	for _, r := range results {
		if r.reason != "unreachable" {
			t.Errorf("want reason=unreachable, got %q", r.reason)
		}
	}
}
