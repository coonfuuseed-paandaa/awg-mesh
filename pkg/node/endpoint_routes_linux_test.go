//go:build linux

package node

import (
	"net"
	"reflect"
	"sort"
	"testing"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	"github.com/rs/zerolog"
)

func TestEndpointConfigureTransportInstallsRoutesFromAllowedIPs(t *testing.T) {
	originalAddInterfaceAddress := endpointAddInterfaceAddress
	originalRouteReplaceLink := endpointRouteReplaceLink
	t.Cleanup(func() {
		endpointAddInterfaceAddress = originalAddInterfaceAddress
		endpointRouteReplaceLink = originalRouteReplaceLink
	})

	calls := make([]string, 0)
	endpointAddInterfaceAddress = func(_ string, _ string) error { return nil }
	endpointRouteReplaceLink = func(dest *net.IPNet, _ string) error {
		calls = append(calls, dest.String())
		return nil
	}

	runner := &EndpointRunner{
		node: &Node{
			config: NodeConfig{OverlayIP: "10.44.0.9/32"},
			logger: zerolog.Nop(),
		},
	}

	err := runner.ConfigureTransport(
		"abc",
		"10.255.0.2",
		"10.255.0.1",
		[]string{
			"10.255.0.0/30", // transport subnet-like route (skip)
			"10.44.0.9/32",  // own overlay /32 (skip)
			"10.44.0.0/24",  // overlay range (install)
			"10.66.0.0/27",  // overlay range (install)
		},
		"",
	)
	if err != nil {
		t.Fatalf("ConfigureTransport returned error: %v", err)
	}

	want := []string{"10.44.0.0/24", "10.66.0.0/27"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("unexpected routes installed: want %v got %v", want, calls)
	}
}

func TestEndpointConfigureTransportSkipsOnlyOwnHostRoute(t *testing.T) {
	originalAddInterfaceAddress := endpointAddInterfaceAddress
	originalRouteReplaceLink := endpointRouteReplaceLink
	t.Cleanup(func() {
		endpointAddInterfaceAddress = originalAddInterfaceAddress
		endpointRouteReplaceLink = originalRouteReplaceLink
	})

	calls := make([]string, 0)
	endpointAddInterfaceAddress = func(_ string, _ string) error { return nil }
	endpointRouteReplaceLink = func(dest *net.IPNet, _ string) error {
		calls = append(calls, dest.String())
		return nil
	}

	runner := &EndpointRunner{
		node: &Node{
			config: NodeConfig{OverlayIP: "10.50.0.10"},
			logger: zerolog.Nop(),
		},
	}

	err := runner.ConfigureTransport(
		"abc",
		"10.255.0.6",
		"10.255.0.5",
		[]string{
			"10.50.0.10/32", // own host route (skip)
			"10.50.0.0/24",  // containing network (must install)
		},
		"",
	)
	if err != nil {
		t.Fatalf("ConfigureTransport returned error: %v", err)
	}

	want := []string{"10.50.0.0/24"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("unexpected routes installed: want %v got %v", want, calls)
	}
}

func TestEndpointConfigureTransportInstallsNonSelfHostRoute(t *testing.T) {
	originalAddInterfaceAddress := endpointAddInterfaceAddress
	originalRouteReplaceLink := endpointRouteReplaceLink
	t.Cleanup(func() {
		endpointAddInterfaceAddress = originalAddInterfaceAddress
		endpointRouteReplaceLink = originalRouteReplaceLink
	})

	calls := make([]string, 0)
	endpointAddInterfaceAddress = func(_ string, _ string) error { return nil }
	endpointRouteReplaceLink = func(dest *net.IPNet, _ string) error {
		calls = append(calls, dest.String())
		return nil
	}

	// overlayIP = 10.44.0.9 (the endpoint's own IP)
	// allowedIP = 10.44.0.1/32 (master's host route — a DIFFERENT /32)
	// The route MUST be installed; the skip filter must only skip the self-/32.
	runner := &EndpointRunner{
		node: &Node{
			config: NodeConfig{OverlayIP: "10.44.0.9"},
			logger: zerolog.Nop(),
		},
	}

	err := runner.ConfigureTransport(
		"abc",
		"10.255.0.2",
		"10.255.0.1",
		[]string{"10.44.0.1/32"},
		"",
	)
	if err != nil {
		t.Fatalf("ConfigureTransport returned error: %v", err)
	}

	if len(calls) != 1 || calls[0] != "10.44.0.1/32" {
		t.Fatalf("expected RouteReplaceLink called with 10.44.0.1/32, got %v", calls)
	}
}

// ---------------------------------------------------------------------------
// mockLinkRouter — inline LinkRouter mock used by overlay route tests.
// ---------------------------------------------------------------------------

type mockLinkRouter struct {
	// replaceCalls records (dest, dev) pairs from RouteReplaceLink.
	replaceCalls []mockRouteCall
	// deleteCalls records dest strings from RouteDelete.
	deleteCalls []string
	// replaceErr is returned by RouteReplaceLink when non-nil.
	replaceErr error
	// deleteErr is returned by RouteDelete when non-nil.
	deleteErr error
}

type mockRouteCall struct {
	dest string
	dev  string
}

func (m *mockLinkRouter) RouteReplaceLink(dest *net.IPNet, dev string) error {
	m.replaceCalls = append(m.replaceCalls, mockRouteCall{dest: dest.String(), dev: dev})
	return m.replaceErr
}

func (m *mockLinkRouter) RouteDelete(dest *net.IPNet) error {
	m.deleteCalls = append(m.deleteCalls, dest.String())
	return m.deleteErr
}

// ---------------------------------------------------------------------------
// TestEndpointInstallOverlayRoutes
// 2 endpoints (self=endpoint-a, peer=endpoint-b) bound to 2 masters.
// master-a: [endpoint-a, endpoint-b], master-b: [endpoint-a, endpoint-b]
// Chosen master (alphabetically first) for endpoint-a↔endpoint-b = master-a.
// Installing via master-a → 1 route on wg-master-a.
// Installing via master-b → 0 routes (master-a is chosen, not master-b).
// ---------------------------------------------------------------------------

func TestEndpointInstallOverlayRoutes(t *testing.T) {
	t.Parallel()

	topo := &topology.Topology{
		Masters: []topology.MasterNode{
			{Name: "master-a", Endpoints: []string{"endpoint-a", "endpoint-b"}},
			{Name: "master-b", Endpoints: []string{"endpoint-a", "endpoint-b"}},
		},
		Endpoints: []topology.EndpointNode{
			{Name: "endpoint-a", OverlayIP: "172.20.70.10"},
			{Name: "endpoint-b", OverlayIP: "172.20.70.11"},
		},
	}

	t.Run("via master-a installs route to endpoint-b", func(t *testing.T) {
		t.Parallel()
		mock := &mockLinkRouter{}
		err := installOverlayRoutesForMaster(topo, "endpoint-a", "master-a", "wg-master-a", mock, zerolog.Nop())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mock.replaceCalls) != 1 {
			t.Fatalf("expected 1 RouteReplaceLink call, got %d: %v", len(mock.replaceCalls), mock.replaceCalls)
		}
		if mock.replaceCalls[0].dest != "172.20.70.11/32" {
			t.Errorf("dest = %s, want 172.20.70.11/32", mock.replaceCalls[0].dest)
		}
		if mock.replaceCalls[0].dev != "wg-master-a" {
			t.Errorf("dev = %s, want wg-master-a", mock.replaceCalls[0].dev)
		}
	})

	t.Run("via master-b installs no routes (master-a is chosen)", func(t *testing.T) {
		t.Parallel()
		mock := &mockLinkRouter{}
		err := installOverlayRoutesForMaster(topo, "endpoint-a", "master-b", "wg-master-b", mock, zerolog.Nop())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mock.replaceCalls) != 0 {
			t.Errorf("expected 0 RouteReplaceLink calls via master-b, got %d: %v", len(mock.replaceCalls), mock.replaceCalls)
		}
	})
}

// ---------------------------------------------------------------------------
// TestEndpointInstallOverlayRoutes_NoOtherEndpoints
// Single endpoint in topology → no routes to install.
// ---------------------------------------------------------------------------

func TestEndpointInstallOverlayRoutes_NoOtherEndpoints(t *testing.T) {
	t.Parallel()

	topo := &topology.Topology{
		Masters: []topology.MasterNode{
			{Name: "master-a", Endpoints: []string{"endpoint-a"}},
		},
		Endpoints: []topology.EndpointNode{
			{Name: "endpoint-a", OverlayIP: "172.20.70.10"},
		},
	}

	mock := &mockLinkRouter{}
	err := installOverlayRoutesForMaster(topo, "endpoint-a", "master-a", "wg-master-a", mock, zerolog.Nop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.replaceCalls) != 0 {
		t.Errorf("expected 0 route installs for single-endpoint topology, got %d", len(mock.replaceCalls))
	}
}

// ---------------------------------------------------------------------------
// TestEndpointInstallOverlayRoutes_FirstMasterAlphabetically
// endpoint-b is bound to masters [master-a, master-b] together with endpoint-a.
// Only master-a (alphabetically first) should produce a route when installing
// via master-a. Installing via master-b must produce no routes.
// ---------------------------------------------------------------------------

func TestEndpointInstallOverlayRoutes_FirstMasterAlphabetically(t *testing.T) {
	t.Parallel()

	topo := &topology.Topology{
		Masters: []topology.MasterNode{
			{Name: "master-a", Endpoints: []string{"endpoint-a", "endpoint-b"}},
			{Name: "master-b", Endpoints: []string{"endpoint-a", "endpoint-b"}},
		},
		Endpoints: []topology.EndpointNode{
			{Name: "endpoint-a", OverlayIP: "172.20.70.10"},
			{Name: "endpoint-b", OverlayIP: "172.20.70.11"},
		},
	}

	// master-a: alphabetically first → should install route
	mockA := &mockLinkRouter{}
	if err := installOverlayRoutesForMaster(topo, "endpoint-a", "master-a", "wg-master-a", mockA, zerolog.Nop()); err != nil {
		t.Fatalf("master-a: unexpected error: %v", err)
	}
	if len(mockA.replaceCalls) != 1 || mockA.replaceCalls[0].dest != "172.20.70.11/32" {
		t.Errorf("master-a: expected route 172.20.70.11/32, got %v", mockA.replaceCalls)
	}

	// master-b: alphabetically second → master-a wins, no routes here
	mockB := &mockLinkRouter{}
	if err := installOverlayRoutesForMaster(topo, "endpoint-a", "master-b", "wg-master-b", mockB, zerolog.Nop()); err != nil {
		t.Fatalf("master-b: unexpected error: %v", err)
	}
	if len(mockB.replaceCalls) != 0 {
		t.Errorf("master-b: expected 0 routes (master-a wins), got %v", mockB.replaceCalls)
	}
}

// ---------------------------------------------------------------------------
// TestEndpointRemoveOverlayRoutes
// Removal mirrors installation: same chosen-master logic applies.
// ---------------------------------------------------------------------------

func TestEndpointRemoveOverlayRoutes(t *testing.T) {
	t.Parallel()

	topo := &topology.Topology{
		Masters: []topology.MasterNode{
			{Name: "master-a", Endpoints: []string{"endpoint-a", "endpoint-b"}},
			{Name: "master-b", Endpoints: []string{"endpoint-a", "endpoint-c"}},
		},
		Endpoints: []topology.EndpointNode{
			{Name: "endpoint-a", OverlayIP: "172.20.70.10"},
			{Name: "endpoint-b", OverlayIP: "172.20.70.11"},
			{Name: "endpoint-c", OverlayIP: "172.20.70.12"},
		},
	}

	t.Run("remove via master-a clears endpoint-b route", func(t *testing.T) {
		t.Parallel()
		mock := &mockLinkRouter{}
		err := removeOverlayRoutesForMaster(topo, "endpoint-a", "master-a", mock, zerolog.Nop())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mock.deleteCalls) != 1 || mock.deleteCalls[0] != "172.20.70.11/32" {
			t.Errorf("expected delete 172.20.70.11/32, got %v", mock.deleteCalls)
		}
	})

	t.Run("remove via master-b clears endpoint-c route", func(t *testing.T) {
		t.Parallel()
		mock := &mockLinkRouter{}
		err := removeOverlayRoutesForMaster(topo, "endpoint-a", "master-b", mock, zerolog.Nop())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mock.deleteCalls) != 1 || mock.deleteCalls[0] != "172.20.70.12/32" {
			t.Errorf("expected delete 172.20.70.12/32, got %v", mock.deleteCalls)
		}
	})
}

// ---------------------------------------------------------------------------
// TestEndpointRebuildAllOverlayRoutes
// 3 endpoints, 2 masters; verifies that rebuildAllOverlayRoutes installs the
// correct /32 routes by running the production function with mock ifaces and
// a stubbed-out router seam.
//
// Topology:
//   master-a: [endpoint-a (self), endpoint-b]   → route to endpoint-b via wg-master-a
//   master-b: [endpoint-a (self), endpoint-c]   → route to endpoint-c via wg-master-b
//
// rebuildAllOverlayRoutes iterates e.listIfaces() (sorted) and calls
// installOverlayRoutesForMaster for each. We exercise this by replacing the
// production NetlinkRouter with a package-level seam (overlayRouterFn), just
// as the existing tests replace endpointRouteReplaceLink.
// ---------------------------------------------------------------------------

func TestEndpointRebuildAllOverlayRoutes(t *testing.T) {
	t.Parallel()

	topo := &topology.Topology{
		Masters: []topology.MasterNode{
			{Name: "master-a", Endpoints: []string{"endpoint-a", "endpoint-b"}},
			{Name: "master-b", Endpoints: []string{"endpoint-a", "endpoint-c"}},
		},
		Endpoints: []topology.EndpointNode{
			{Name: "endpoint-a", OverlayIP: "172.20.70.10"},
			{Name: "endpoint-b", OverlayIP: "172.20.70.11"},
			{Name: "endpoint-c", OverlayIP: "172.20.70.12"},
		},
	}

	// Replace the overlay router factory used by rebuildAllOverlayRoutes.
	mock := &mockLinkRouter{}
	orig := overlayRouterFn
	overlayRouterFn = func() LinkRouter { return mock }
	t.Cleanup(func() { overlayRouterFn = orig })

	e := &EndpointRunner{
		node: &Node{
			config:   NodeConfig{Name: "endpoint-a"},
			topology: topo,
			logger:   zerolog.Nop(),
		},
	}
	// Populate platformState as if both master ifaces were created.
	e.setIface("master-a", nil)
	e.setIface("master-b", nil)

	if err := rebuildAllOverlayRoutes(e, topo); err != nil {
		t.Fatalf("rebuildAllOverlayRoutes returned error: %v", err)
	}

	// Collect installed routes and sort for deterministic comparison.
	got := make([]string, 0, len(mock.replaceCalls))
	for _, c := range mock.replaceCalls {
		got = append(got, c.dest+"@"+c.dev)
	}
	sort.Strings(got)

	want := []string{
		"172.20.70.11/32@wg-master-a", // endpoint-b via master-a
		"172.20.70.12/32@wg-master-b", // endpoint-c via master-b
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("route installs mismatch:\n got  %v\n want %v", got, want)
	}
}
