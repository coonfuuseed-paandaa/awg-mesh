//go:build linux

package node

import (
	"net"
	"testing"
)

// mockRouteReplaceLink captures calls to RouteReplaceLink for testing.
// It is a package-level variable so tests can inject it as a seam.
// Production code uses routing.NewNetlinkRouter().RouteReplaceLink directly
// inside createTunnelInterface; tests use normalizeTransportOverlayRoute directly.

// TestNormalizeTransportOverlayRoute verifies that plain IPs get /32 suffix,
// already-CIDR values are normalised to host /32, and empty string is safe.
func TestNormalizeTransportOverlayRoute(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want string
	}{
		{"172.20.70.34", "172.20.70.34/32"},
		{"172.20.70.34/32", "172.20.70.34/32"},
		{"172.20.70.34/27", "172.20.70.34/32"}, // host part extracted, /32 appended
		{"", ""},
	}

	for _, tc := range cases {
		got := normalizeTransportOverlayRoute(tc.in)
		if got != tc.want {
			t.Errorf("normalizeTransportOverlayRoute(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestBuildPeerAllowedIPs_TransportAndOverlay verifies that buildPeerAllowedIPs
// returns [transport_subnet, overlay/32] when both inputs are valid.
func TestBuildPeerAllowedIPs_TransportAndOverlay(t *testing.T) {
	t.Parallel()

	got, err := buildPeerAllowedIPs("10.255.0.24/30", "172.20.70.34")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 CIDRs, got %d: %v", len(got), got)
	}

	// First entry must be the /30 transport subnet.
	_, wantTransport, _ := net.ParseCIDR("10.255.0.24/30")
	if got[0].String() != wantTransport.String() {
		t.Errorf("got[0]=%s, want %s", got[0].String(), wantTransport.String())
	}

	// Second entry must be the overlay host /32.
	_, wantOverlay, _ := net.ParseCIDR("172.20.70.34/32")
	if got[1].String() != wantOverlay.String() {
		t.Errorf("got[1]=%s, want %s", got[1].String(), wantOverlay.String())
	}
}

// TestBuildPeerAllowedIPs_EmptyOverlay verifies that an empty overlayIP
// returns an error (the master interface must know the peer overlay address).
func TestBuildPeerAllowedIPs_EmptyOverlay(t *testing.T) {
	t.Parallel()

	_, err := buildPeerAllowedIPs("10.255.0.24/30", "")
	if err == nil {
		t.Fatal("expected error for empty overlayIP, got nil")
	}
}

// TestBuildPeerAllowedIPs_InvalidTransport verifies that a malformed transport
// CIDR returns an error rather than silently producing a wrong route.
func TestBuildPeerAllowedIPs_InvalidTransport(t *testing.T) {
	t.Parallel()

	_, err := buildPeerAllowedIPs("not-a-cidr", "172.20.70.34")
	if err == nil {
		t.Fatal("expected error for invalid transport CIDR, got nil")
	}
}
