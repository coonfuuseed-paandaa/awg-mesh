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
