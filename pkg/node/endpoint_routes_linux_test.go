//go:build linux

package node

import (
	"net"
	"reflect"
	"testing"

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
	)
	if err != nil {
		t.Fatalf("ConfigureTransport returned error: %v", err)
	}

	if len(calls) != 1 || calls[0] != "10.44.0.1/32" {
		t.Fatalf("expected RouteReplaceLink called with 10.44.0.1/32, got %v", calls)
	}
}
