//go:build linux

package node

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/amnezia-vpn/amneziawg-go/device"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/transport"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
	"github.com/rs/zerolog"
)

// ---------------------------------------------------------------------------
// TestEndpointReconcilePeerKey
//
// Verifies that transport.yml round-trips peer keys as hex, and that the
// reconcile path (createInterface) creates exactly one per-master interface
// when transport.yml has one tunnel entry matching the topology.
// ---------------------------------------------------------------------------

func TestEndpointReconcilePeerKey(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	peerKeyHex := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	peerKeyBytes, err := hex.DecodeString(peerKeyHex)
	if err != nil {
		t.Fatalf("DecodeString returned error: %v", err)
	}

	state := transport.NodeTransportState{
		SchemaVersion: transport.CurrentSchemaVersion,
		Tunnels: []transport.TunnelTransport{
			{
				Name:                "master-a",
				TransportIP:         "10.200.0.1",
				PeerTransportIP:     "10.200.0.2",
				PeerPublicKey:       peerKeyHex,
				PeerEndpoint:        "198.51.100.10:51820",
				AllowedIPs:          []string{"10.200.0.2/32"},
				PersistentKeepalive: 25,
			},
		},
	}
	if err := saveNodeTransportState(configDir, state); err != nil {
		t.Fatalf("saveNodeTransportState returned error: %v", err)
	}

	readBack, err := loadNodeTransportState(configDir)
	if err != nil {
		t.Fatalf("loadNodeTransportState returned error: %v", err)
	}
	if len(readBack.Tunnels) != 1 {
		t.Fatalf("expected 1 tunnel, got %d", len(readBack.Tunnels))
	}

	reconciledPeerBytes, err := hex.DecodeString(strings.TrimSpace(readBack.Tunnels[0].PeerPublicKey))
	if err != nil {
		t.Fatalf("DecodeString returned error: %v", err)
	}
	if len(reconciledPeerBytes) != 32 {
		t.Fatalf("expected 32 decoded bytes, got %d", len(reconciledPeerBytes))
	}
	if !bytes.Equal(reconciledPeerBytes, peerKeyBytes) {
		t.Fatalf("decoded bytes mismatch: got %x want %x", reconciledPeerBytes, peerKeyBytes)
	}

	reconciledPeerKey, err := wg.NewKey(reconciledPeerBytes)
	if err != nil {
		t.Fatalf("NewKey returned error: %v", err)
	}
	if !bytes.Equal(reconciledPeerKey[:], peerKeyBytes) {
		t.Fatalf("reconciled key mismatch: got %x want %x", reconciledPeerKey[:], peerKeyBytes)
	}

	// Regression guard for local tracker issue #94:
	// wg.ParseKey (base64 decoder) on the same string must NOT yield 32 bytes.
	// If a future refactor changes the storage encoding without updating readers,
	// this check will catch it.
	if _, err := wg.ParseKey(readBack.Tunnels[0].PeerPublicKey); err == nil {
		t.Fatalf("regression guard: hex-encoded key was unexpectedly parseable as base64 -> encoding contract silently changed")
	}

	// Assert that createInterface creates exactly 1 per-master iface when
	// topology has 1 master bound to this endpoint and transport.yml has a
	// matching tunnel entry.
	// NOT parallel: modifies package-level seam vars.
	origCreate := endpointCreateIfaceFn
	origConfigure := endpointConfigureIfaceFn
	origSetUp := endpointSetIfaceUpFn
	origAddAddr := endpointAddInterfaceAddress
	t.Cleanup(func() {
		endpointCreateIfaceFn = origCreate
		endpointConfigureIfaceFn = origConfigure
		endpointSetIfaceUpFn = origSetUp
		endpointAddInterfaceAddress = origAddAddr
	})

	ifacesCreated := 0
	endpointCreateIfaceFn = func(_ string, _ int, _ *device.Logger) (*wg.Interface, error) {
		ifacesCreated++
		return &wg.Interface{}, nil
	}
	endpointConfigureIfaceFn = func(_ *wg.Interface, _ wg.Config) error { return nil }
	endpointSetIfaceUpFn = func(_ string) error { return nil }
	endpointAddInterfaceAddress = func(_ string, _ string) error { return nil }

	topo := &topology.Topology{
		Masters: []topology.MasterNode{
			{Name: "master-a", Host: "10.0.0.1", OverlayIP: "10.200.0.2", ListenPort: 51820, Endpoints: []string{"endpoint-a"}},
		},
		Endpoints: []topology.EndpointNode{{Name: "endpoint-a"}},
	}
	runner := &EndpointRunner{
		node: &Node{
			config:   NodeConfig{Name: "endpoint-a", ConfigDir: configDir, ListenPort: 51820},
			logger:   zerolog.Nop(),
			topology: topo,
		},
	}

	if err := runner.createInterface(); err != nil {
		t.Fatalf("createInterface returned error: %v", err)
	}
	if ifacesCreated != 1 {
		t.Errorf("ifaces created = %d, want 1", ifacesCreated)
	}
	if runner.getIface("master-a") == nil {
		t.Error("expected iface stored under 'master-a', got nil")
	}
}

// ---------------------------------------------------------------------------
// TestEndpointReconcileCreatesNIfaces
//
// 2 transport.yml entries + 2 topology masters → exactly 2 ifaces created.
// NOT parallel: modifies package-level seam vars.
// ---------------------------------------------------------------------------

func TestEndpointReconcileCreatesNIfaces(t *testing.T) {
	configDir := t.TempDir()

	peerKeyHex := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	state := transport.NodeTransportState{
		SchemaVersion: transport.CurrentSchemaVersion,
		Tunnels: []transport.TunnelTransport{
			{Name: "master-a", TransportIP: "10.255.0.1", PeerTransportIP: "10.255.0.2", PeerPublicKey: peerKeyHex},
			{Name: "master-b", TransportIP: "10.255.0.5", PeerTransportIP: "10.255.0.6", PeerPublicKey: peerKeyHex},
		},
	}
	if err := saveNodeTransportState(configDir, state); err != nil {
		t.Fatalf("saveNodeTransportState: %v", err)
	}

	origCreate := endpointCreateIfaceFn
	origConfigure := endpointConfigureIfaceFn
	origSetUp := endpointSetIfaceUpFn
	origAddAddr := endpointAddInterfaceAddress
	t.Cleanup(func() {
		endpointCreateIfaceFn = origCreate
		endpointConfigureIfaceFn = origConfigure
		endpointSetIfaceUpFn = origSetUp
		endpointAddInterfaceAddress = origAddAddr
	})

	ifacesCreated := 0
	endpointCreateIfaceFn = func(_ string, _ int, _ *device.Logger) (*wg.Interface, error) {
		ifacesCreated++
		return &wg.Interface{}, nil
	}
	endpointConfigureIfaceFn = func(_ *wg.Interface, _ wg.Config) error { return nil }
	endpointSetIfaceUpFn = func(_ string) error { return nil }
	endpointAddInterfaceAddress = func(_ string, _ string) error { return nil }

	topo := &topology.Topology{
		Masters: []topology.MasterNode{
			{Name: "master-a", Host: "10.0.0.1", OverlayIP: "10.255.0.2", ListenPort: 51820, Endpoints: []string{"endpoint-a"}},
			{Name: "master-b", Host: "10.0.0.2", OverlayIP: "10.255.0.6", ListenPort: 51821, Endpoints: []string{"endpoint-a"}},
		},
		Endpoints: []topology.EndpointNode{{Name: "endpoint-a"}},
	}
	runner := &EndpointRunner{
		node: &Node{
			config:   NodeConfig{Name: "endpoint-a", ConfigDir: configDir, ListenPort: 51820},
			logger:   zerolog.Nop(),
			topology: topo,
		},
	}

	if err := runner.createInterface(); err != nil {
		t.Fatalf("createInterface: %v", err)
	}

	if ifacesCreated != 2 {
		t.Errorf("ifaces created = %d, want 2", ifacesCreated)
	}
	if runner.getIface("master-a") == nil {
		t.Error("expected iface stored under 'master-a', got nil")
	}
	if runner.getIface("master-b") == nil {
		t.Error("expected iface stored under 'master-b', got nil")
	}
}

// ---------------------------------------------------------------------------
// TestEndpointReconcileIsIdempotent
//
// Calling createInterface twice must not produce duplicate ifaces:
// after the second call the map still has exactly N entries (not 2N).
// NOT parallel: modifies package-level seam vars.
// ---------------------------------------------------------------------------

func TestEndpointReconcileIsIdempotent(t *testing.T) {
	configDir := t.TempDir()

	peerKeyHex := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	state := transport.NodeTransportState{
		SchemaVersion: transport.CurrentSchemaVersion,
		Tunnels: []transport.TunnelTransport{
			{Name: "master-a", TransportIP: "10.255.0.1", PeerTransportIP: "10.255.0.2", PeerPublicKey: peerKeyHex},
			{Name: "master-b", TransportIP: "10.255.0.5", PeerTransportIP: "10.255.0.6", PeerPublicKey: peerKeyHex},
		},
	}
	if err := saveNodeTransportState(configDir, state); err != nil {
		t.Fatalf("saveNodeTransportState: %v", err)
	}

	origCreate := endpointCreateIfaceFn
	origConfigure := endpointConfigureIfaceFn
	origSetUp := endpointSetIfaceUpFn
	origAddAddr := endpointAddInterfaceAddress
	t.Cleanup(func() {
		endpointCreateIfaceFn = origCreate
		endpointConfigureIfaceFn = origConfigure
		endpointSetIfaceUpFn = origSetUp
		endpointAddInterfaceAddress = origAddAddr
	})

	totalIfacesCreated := 0
	endpointCreateIfaceFn = func(_ string, _ int, _ *device.Logger) (*wg.Interface, error) {
		totalIfacesCreated++
		return &wg.Interface{}, nil
	}
	endpointConfigureIfaceFn = func(_ *wg.Interface, _ wg.Config) error { return nil }
	endpointSetIfaceUpFn = func(_ string) error { return nil }
	endpointAddInterfaceAddress = func(_ string, _ string) error { return nil }

	topo := &topology.Topology{
		Masters: []topology.MasterNode{
			{Name: "master-a", Host: "10.0.0.1", OverlayIP: "10.255.0.2", ListenPort: 51820, Endpoints: []string{"endpoint-a"}},
			{Name: "master-b", Host: "10.0.0.2", OverlayIP: "10.255.0.6", ListenPort: 51821, Endpoints: []string{"endpoint-a"}},
		},
		Endpoints: []topology.EndpointNode{{Name: "endpoint-a"}},
	}
	runner := &EndpointRunner{
		node: &Node{
			config:   NodeConfig{Name: "endpoint-a", ConfigDir: configDir, ListenPort: 51820},
			logger:   zerolog.Nop(),
			topology: topo,
		},
	}

	// First reconcile.
	if err := runner.createInterface(); err != nil {
		t.Fatalf("createInterface (first): %v", err)
	}

	// Second reconcile on the same runner: the per-master path exits early
	// because created > 0 after the first call already populated platformState.
	// The map must still have exactly 2 entries — not 4.
	if err := runner.createInterface(); err != nil {
		t.Fatalf("createInterface (second): %v", err)
	}

	ifaces := runner.listIfaces()
	if len(ifaces) != 2 {
		t.Errorf("iface count after 2 reconciles = %d, want 2 (got: %v)", len(ifaces), ifaces)
	}
}

// ---------------------------------------------------------------------------
// TestEndpointReconcileSkipsStaleTunnel
//
// transport.yml has 2 tunnel entries; topology only has 1 matching master.
// createInterface must create 1 iface and skip the stale entry with a warn log.
// NOT parallel: modifies package-level seam vars.
// ---------------------------------------------------------------------------

func TestEndpointReconcileSkipsStaleTunnel(t *testing.T) {
	configDir := t.TempDir()

	peerKeyHex := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	state := transport.NodeTransportState{
		SchemaVersion: transport.CurrentSchemaVersion,
		Tunnels: []transport.TunnelTransport{
			// master-a is in topology; master-gone has been removed from topology.
			{Name: "master-a", TransportIP: "10.255.0.1", PeerTransportIP: "10.255.0.2", PeerPublicKey: peerKeyHex},
			{Name: "master-gone", TransportIP: "10.255.0.5", PeerTransportIP: "10.255.0.6", PeerPublicKey: peerKeyHex},
		},
	}
	if err := saveNodeTransportState(configDir, state); err != nil {
		t.Fatalf("saveNodeTransportState: %v", err)
	}

	origCreate := endpointCreateIfaceFn
	origConfigure := endpointConfigureIfaceFn
	origSetUp := endpointSetIfaceUpFn
	origAddAddr := endpointAddInterfaceAddress
	t.Cleanup(func() {
		endpointCreateIfaceFn = origCreate
		endpointConfigureIfaceFn = origConfigure
		endpointSetIfaceUpFn = origSetUp
		endpointAddInterfaceAddress = origAddAddr
	})

	ifacesCreated := 0
	endpointCreateIfaceFn = func(_ string, _ int, _ *device.Logger) (*wg.Interface, error) {
		ifacesCreated++
		return &wg.Interface{}, nil
	}
	endpointConfigureIfaceFn = func(_ *wg.Interface, _ wg.Config) error { return nil }
	endpointSetIfaceUpFn = func(_ string) error { return nil }
	endpointAddInterfaceAddress = func(_ string, _ string) error { return nil }

	// Topology only knows master-a; master-gone was removed.
	topo := &topology.Topology{
		Masters: []topology.MasterNode{
			{Name: "master-a", Host: "10.0.0.1", OverlayIP: "10.255.0.2", ListenPort: 51820, Endpoints: []string{"endpoint-a"}},
		},
		Endpoints: []topology.EndpointNode{{Name: "endpoint-a"}},
	}
	runner := &EndpointRunner{
		node: &Node{
			config:   NodeConfig{Name: "endpoint-a", ConfigDir: configDir, ListenPort: 51820},
			logger:   zerolog.Nop(),
			topology: topo,
		},
	}

	if err := runner.createInterface(); err != nil {
		t.Fatalf("createInterface: %v", err)
	}

	// Only master-a must be created; master-gone is not in topology so it is skipped.
	if ifacesCreated != 1 {
		t.Errorf("ifaces created = %d, want 1 (only master-a)", ifacesCreated)
	}
	if runner.getIface("master-a") == nil {
		t.Error("expected iface stored under 'master-a', got nil")
	}
	if runner.getIface("master-gone") != nil {
		t.Error("unexpected iface stored under 'master-gone': stale tunnel was not skipped")
	}
}
