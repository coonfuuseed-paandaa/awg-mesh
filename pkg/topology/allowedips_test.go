package topology

import (
	"strings"
	"testing"
)

func makeTopo(ranges ...NamedRange) *Topology {
	return &Topology{
		Overlay: OverlayConfig{
			Ranges: ranges,
		},
	}
}

// TestBuildAllowedIPsForEndpoint_HappyPath verifies that transport /30 +
// master /32 + all overlay range CIDRs are returned in order.
func TestBuildAllowedIPsForEndpoint_HappyPath(t *testing.T) {
	t.Parallel()

	topo := makeTopo(
		NamedRange{Name: "masters", CIDR: "172.20.70.0/27"},
		NamedRange{Name: "endpoints", CIDR: "172.20.70.32/27"},
		NamedRange{Name: "clients", CIDR: "172.20.70.128/25"},
	)

	got, err := BuildAllowedIPsForEndpoint(topo, "172.20.70.2", "10.255.0.24/30")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{
		"10.255.0.24/30",
		"172.20.70.2/32",
		"172.20.70.0/27",
		"172.20.70.32/27",
		"172.20.70.128/25",
	}

	if len(got) != len(want) {
		t.Fatalf("len(got)=%d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

// TestBuildAllowedIPsForEndpoint_EmptyMasterOverlay verifies that an empty
// masterOverlayIP returns a descriptive error.
func TestBuildAllowedIPsForEndpoint_EmptyMasterOverlay(t *testing.T) {
	t.Parallel()

	topo := makeTopo()
	_, err := BuildAllowedIPsForEndpoint(topo, "", "10.255.0.24/30")
	if err == nil {
		t.Fatal("expected error for empty masterOverlayIP, got nil")
	}
	if !strings.Contains(err.Error(), "master overlay IP is required") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestBuildAllowedIPsForEndpoint_EmptyTransportSubnet verifies that an empty
// transportSubnet returns a descriptive error.
func TestBuildAllowedIPsForEndpoint_EmptyTransportSubnet(t *testing.T) {
	t.Parallel()

	topo := makeTopo()
	_, err := BuildAllowedIPsForEndpoint(topo, "172.20.70.2", "")
	if err == nil {
		t.Fatal("expected error for empty transportSubnet, got nil")
	}
	if !strings.Contains(err.Error(), "transport subnet is required") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestBuildAllowedIPsForEndpoint_DeduplicatesCIDRs verifies that the helper
// deduplicates entries when a CIDR appears more than once (e.g. master /32 equal
// to a range CIDR, or duplicate ranges in topology).
func TestBuildAllowedIPsForEndpoint_DeduplicatesCIDRs(t *testing.T) {
	t.Parallel()

	// Master overlay /32 coincides with first range (pathological but defensive).
	topo := makeTopo(
		NamedRange{Name: "dup1", CIDR: "172.20.70.2/32"},
		NamedRange{Name: "dup2", CIDR: "172.20.70.2/32"},
		NamedRange{Name: "other", CIDR: "172.20.70.32/27"},
	)

	got, err := BuildAllowedIPsForEndpoint(topo, "172.20.70.2", "10.255.0.0/30")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Expected unique entries: transport /30, master /32, other /27
	// The two duplicate range CIDRs collapse into one.
	want := []string{
		"10.255.0.0/30",
		"172.20.70.2/32",
		"172.20.70.32/27",
	}
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

// TestBuildAllowedIPsForEndpoint_EmptyRanges verifies that when the topology has
// no overlay ranges the helper returns exactly [transport /30, master /32].
func TestBuildAllowedIPsForEndpoint_EmptyRanges(t *testing.T) {
	t.Parallel()

	topo := makeTopo() // no ranges
	got, err := BuildAllowedIPsForEndpoint(topo, "172.20.70.2", "10.255.0.24/30")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{"10.255.0.24/30", "172.20.70.2/32"}
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d, want %d; got=%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

// TestBuildAllowedIPsForEndpoint_EmptyCIDRRangeSkipped verifies that range
// entries with an empty CIDR string are silently skipped.
func TestBuildAllowedIPsForEndpoint_EmptyCIDRRangeSkipped(t *testing.T) {
	t.Parallel()

	topo := makeTopo(
		NamedRange{Name: "empty", CIDR: ""},
		NamedRange{Name: "real", CIDR: "172.20.70.0/27"},
	)
	got, err := BuildAllowedIPsForEndpoint(topo, "172.20.70.2", "10.255.0.24/30")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{"10.255.0.24/30", "172.20.70.2/32", "172.20.70.0/27"}
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d, want %d; got=%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

// TestBuildAllowedIPsForEndpoint_InvalidMasterOverlayIP verifies that a
// malformed masterOverlayIP (non-IP string) returns an error.
func TestBuildAllowedIPsForEndpoint_InvalidMasterOverlayIP(t *testing.T) {
	t.Parallel()

	topo := makeTopo()
	_, err := BuildAllowedIPsForEndpoint(topo, "not-an-ip", "10.255.0.24/30")
	if err == nil {
		t.Fatal("expected error for malformed masterOverlayIP, got nil")
	}
	if !strings.Contains(err.Error(), "invalid master overlay IP") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestBuildAllowedIPsForEndpoint_InvalidTransportSubnet verifies that a
// malformed transportSubnet (non-CIDR string) returns an error.
func TestBuildAllowedIPsForEndpoint_InvalidTransportSubnet(t *testing.T) {
	t.Parallel()

	topo := makeTopo()
	_, err := BuildAllowedIPsForEndpoint(topo, "172.20.70.2", "not-a-cidr")
	if err == nil {
		t.Fatal("expected error for malformed transportSubnet, got nil")
	}
	if !strings.Contains(err.Error(), "invalid transport subnet") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestBuildAllowedIPsForEndpoint_InvalidOverlayRangeCIDR verifies that a
// malformed CIDR in an overlay range entry returns an error.
func TestBuildAllowedIPsForEndpoint_InvalidOverlayRangeCIDR(t *testing.T) {
	t.Parallel()

	topo := makeTopo(
		NamedRange{Name: "good", CIDR: "172.20.70.0/27"},
		NamedRange{Name: "bad", CIDR: "999.999.999.999/33"},
	)
	_, err := BuildAllowedIPsForEndpoint(topo, "172.20.70.2", "10.255.0.24/30")
	if err == nil {
		t.Fatal("expected error for malformed overlay range CIDR, got nil")
	}
	if !strings.Contains(err.Error(), "invalid overlay range CIDR") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// --- BuildMinimalAllowedIPsForEndpointPeer tests ---

// TestBuildMinimalAllowedIPsForEndpointPeer_Happy verifies that nil topology
// (back-compat) returns exactly [transport_subnet, master_overlay_ip/32].
func TestBuildMinimalAllowedIPsForEndpointPeer_Happy(t *testing.T) {
	t.Parallel()

	got, err := BuildMinimalAllowedIPsForEndpointPeer(nil, "172.20.70.1", "10.255.0.0/30")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{"10.255.0.0/30", "172.20.70.1/32"}
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

// TestBuildMinimalAllowedIPsForEndpointPeer_IncludesClientsRange verifies that
// when topology declares a "clients" overlay range, it is appended to the
// AllowedIPs so that endpoint→client return-path traffic is permitted by
// WireGuard's bidirectional filter. F-005 gap-2 regression gate (mirrors G10
// for the endpoints-range fix shipped in v1.12.7 / issue #147).
func TestBuildMinimalAllowedIPsForEndpointPeer_IncludesClientsRange(t *testing.T) {
	t.Parallel()

	topo := &Topology{
		Overlay: OverlayConfig{
			Ranges: []NamedRange{
				{Name: "endpoints", CIDR: "172.20.70.32/27"},
				{Name: "clients", CIDR: "172.20.70.128/25"},
			},
		},
	}

	got, err := BuildMinimalAllowedIPsForEndpointPeer(topo, "172.20.70.1", "10.255.0.0/30")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{"10.255.0.0/30", "172.20.70.1/32", "172.20.70.128/25"}
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

// TestBuildMinimalAllowedIPsForEndpointPeer_NoClientsRange verifies that
// topology without a "clients" range returns the legacy minimal pair —
// endpoints range alone must NOT be included on endpoint side (Pattern X
// dedup contract: only the master/clients range is appropriate here).
func TestBuildMinimalAllowedIPsForEndpointPeer_NoClientsRange(t *testing.T) {
	t.Parallel()

	topo := &Topology{
		Overlay: OverlayConfig{
			Ranges: []NamedRange{
				{Name: "masters", CIDR: "172.20.70.0/27"},
				{Name: "endpoints", CIDR: "172.20.70.32/27"},
			},
		},
	}

	got, err := BuildMinimalAllowedIPsForEndpointPeer(topo, "172.20.70.1", "10.255.0.0/30")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{"10.255.0.0/30", "172.20.70.1/32"}
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

// TestBuildMinimalAllowedIPsForEndpointPeer_ClientsRangeCaseInsensitive
// mirrors the BuildAllowedIPsForMasterPeer endpoints-range lookup which is
// case-insensitive (EqualFold), so "Clients" / "CLIENTS" are honoured.
func TestBuildMinimalAllowedIPsForEndpointPeer_ClientsRangeCaseInsensitive(t *testing.T) {
	t.Parallel()

	topo := &Topology{
		Overlay: OverlayConfig{
			Ranges: []NamedRange{
				{Name: "Clients", CIDR: "172.20.70.128/25"},
			},
		},
	}

	got, err := BuildMinimalAllowedIPsForEndpointPeer(topo, "172.20.70.1", "10.255.0.0/30")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(got) != 3 || got[2] != "172.20.70.128/25" {
		t.Errorf("expected clients range matched case-insensitively, got: %v", got)
	}
}

// TestBuildMinimalAllowedIPsForEndpointPeer_InvalidClientsRange verifies that
// a malformed clients-range CIDR produces a clear error (not silently dropped).
func TestBuildMinimalAllowedIPsForEndpointPeer_InvalidClientsRange(t *testing.T) {
	t.Parallel()

	topo := &Topology{
		Overlay: OverlayConfig{
			Ranges: []NamedRange{
				{Name: "clients", CIDR: "not-a-cidr"},
			},
		},
	}

	_, err := BuildMinimalAllowedIPsForEndpointPeer(topo, "172.20.70.1", "10.255.0.0/30")
	if err == nil {
		t.Fatal("expected error for malformed clients range, got nil")
	}
	if !strings.Contains(err.Error(), "invalid clients overlay range CIDR") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestBuildMinimalAllowedIPsForEndpointPeer_EmptyInputs verifies that empty
// masterOverlayIP or transportSubnet both return an error.
func TestBuildMinimalAllowedIPsForEndpointPeer_EmptyInputs(t *testing.T) {
	t.Parallel()

	t.Run("empty masterOverlayIP", func(t *testing.T) {
		t.Parallel()
		_, err := BuildMinimalAllowedIPsForEndpointPeer(nil, "", "10.255.0.0/30")
		if err == nil {
			t.Fatal("expected error for empty masterOverlayIP, got nil")
		}
		if !strings.Contains(err.Error(), "master overlay IP is required") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("empty transportSubnet", func(t *testing.T) {
		t.Parallel()
		_, err := BuildMinimalAllowedIPsForEndpointPeer(nil, "172.20.70.1", "")
		if err == nil {
			t.Fatal("expected error for empty transportSubnet, got nil")
		}
		if !strings.Contains(err.Error(), "transport subnet is required") {
			t.Errorf("unexpected error message: %v", err)
		}
	})
}

// TestBuildMinimalAllowedIPsForEndpointPeer_InvalidSubnet verifies that a
// malformed transportSubnet returns an error.
func TestBuildMinimalAllowedIPsForEndpointPeer_InvalidSubnet(t *testing.T) {
	t.Parallel()

	_, err := BuildMinimalAllowedIPsForEndpointPeer(nil, "172.20.70.1", "not-a-cidr")
	if err == nil {
		t.Fatal("expected error for malformed transportSubnet, got nil")
	}
	if !strings.Contains(err.Error(), "invalid transport subnet") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestBuildMinimalAllowedIPsForEndpointPeer_IPv6 documents that the function
// is IPv4-only (consistent with BuildAllowedIPsForEndpoint scope).
// IPv6 addresses are valid net.IP values so net.ParseIP accepts them, but
// the /32 suffix appended is semantically incorrect for IPv6 (/128 would be
// correct). This test documents the current behaviour: no error is returned
// for an IPv6 masterOverlayIP, but callers MUST NOT pass IPv6 addresses.
func TestBuildMinimalAllowedIPsForEndpointPeer_IPv6(t *testing.T) {
	t.Parallel()

	// net.ParseIP accepts IPv6 — the function does not reject it, but the
	// resulting /32 suffix is incorrect for IPv6. This is the documented
	// limitation: IPv4-only scope, callers are responsible for passing
	// IPv4 addresses.
	_, err := BuildMinimalAllowedIPsForEndpointPeer(nil, "fd00::1", "10.255.0.0/30")
	// No error expected — net.ParseIP accepts IPv6. Document this behaviour.
	if err != nil {
		t.Logf("note: IPv6 masterOverlayIP returned error (acceptable if validation tightened): %v", err)
	}
}
