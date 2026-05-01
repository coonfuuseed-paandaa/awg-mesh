//go:build linux

package node

import (
	"net"
	"strings"
	"testing"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	"github.com/rs/zerolog"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
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

	got, err := buildPeerAllowedIPs(&topology.Topology{
		Overlay: topology.OverlayConfig{
			Ranges: []topology.NamedRange{
				{Name: "endpoints", CIDR: "172.20.70.32/27"},
			},
		},
	}, "pl-01", "10.255.0.24/30", "172.20.70.34")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 CIDRs, got %d: %v", len(got), got)
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
	_, wantEndpointsRange, _ := net.ParseCIDR("172.20.70.32/27")
	if got[2].String() != wantEndpointsRange.String() {
		t.Errorf("got[2]=%s, want %s", got[2].String(), wantEndpointsRange.String())
	}
}

// TestBuildPeerAllowedIPs_EmptyOverlay verifies that an empty overlayIP
// returns an error (the master interface must know the peer overlay address).
func TestBuildPeerAllowedIPs_EmptyOverlay(t *testing.T) {
	t.Parallel()

	_, err := buildPeerAllowedIPs(nil, "pl-01", "10.255.0.24/30", "")
	if err == nil {
		t.Fatal("expected error for empty overlayIP, got nil")
	}
}

// TestBuildPeerAllowedIPs_InvalidTransport verifies that a malformed transport
// CIDR returns an error rather than silently producing a wrong route.
func TestBuildPeerAllowedIPs_InvalidTransport(t *testing.T) {
	t.Parallel()

	_, err := buildPeerAllowedIPs(nil, "pl-01", "not-a-cidr", "172.20.70.34")
	if err == nil {
		t.Fatal("expected error for invalid transport CIDR, got nil")
	}
}

// =============================================================================
// F-008 CR-004: setupMasterVRF tests (mock-mode, no privilege required)
// =============================================================================

// TestSetupMasterVRF_Disabled verifies that when MESH_VRF is unset (default),
// setupMasterVRF is a no-op: vrfManager stays nil (FR-10.6 fallback preserved).
func TestSetupMasterVRF_Disabled(t *testing.T) {
	// Not t.Parallel(): t.Setenv mutates process env.

	t.Setenv("MESH_VRF", "")

	m := &MasterRunner{
		node: &Node{
			config: NodeConfig{Name: "test-master", OverlayIP: "172.20.70.1"},
			logger: zerolog.Nop(),
		},
		tunnels: make(map[string]*MasterTunnel),
	}

	if err := m.setupMasterVRF(); err != nil {
		t.Fatalf("setupMasterVRF() returned error when MESH_VRF unset: %v", err)
	}
	if m.vrfManager != nil {
		t.Error("vrfManager != nil with MESH_VRF unset — should remain nil")
	}
}

// TestSetupMasterVRF_EnabledKernelUnsupported verifies that when MESH_VRF=enabled
// and the kernel does not support VRF (mocked via netlinkLinkAdd returning EOPNOTSUPP),
// setupMasterVRF returns a non-nil error containing "kernel_too_old" (FR-10.2 hard-fail).
// Uses the withMockNetlink helper from vrf_test.go (same package).
func TestSetupMasterVRF_EnabledKernelUnsupported(t *testing.T) {
	// Not t.Parallel(): mutates package-level netlinkLinkAdd test seam.

	t.Setenv("MESH_VRF", "enabled")

	withMockNetlink(t, func(_ netlink.Link) error {
		return unix.EOPNOTSUPP
	}, func() {
		m := &MasterRunner{
			node: &Node{
				config: NodeConfig{Name: "test-master", OverlayIP: "172.20.70.1"},
				logger: zerolog.Nop(),
			},
			tunnels: make(map[string]*MasterTunnel),
		}
		err := m.setupMasterVRF()
		if err == nil {
			t.Fatal("setupMasterVRF() returned nil, want error on EOPNOTSUPP")
		}
		if !strings.Contains(err.Error(), "kernel_too_old") {
			t.Errorf("setupMasterVRF() error = %q, want 'kernel_too_old'", err.Error())
		}
		if m.vrfManager != nil {
			t.Error("vrfManager set after VRF setup error — should remain nil")
		}
	})
}
