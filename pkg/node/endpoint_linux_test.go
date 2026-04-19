//go:build linux

package node

import (
	"net"
	"testing"

	"github.com/amnezia-vpn/amneziawg-go/device"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
	"github.com/rs/zerolog"
)

// ---------------------------------------------------------------------------
// TestEndpointMinimalAllowedIPs
// ---------------------------------------------------------------------------

// TestEndpointMinimalAllowedIPs verifies that buildEndpointPeerAllowedIPs returns
// exactly [transport_subnet, master_overlay_ip/32] and nothing else.
// This is the primary regression guard for the AllowedIPs dedup issue (#134).
func TestEndpointMinimalAllowedIPs(t *testing.T) {
	t.Parallel()

	got, err := buildEndpointPeerAllowedIPs("10.255.0.0/30", "172.20.70.1")
	if err != nil {
		t.Fatalf("buildEndpointPeerAllowedIPs returned error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 AllowedIPs entries, got %d: %v", len(got), got)
	}

	_, wantSubnet, _ := net.ParseCIDR("10.255.0.0/30")
	if got[0].String() != wantSubnet.String() {
		t.Errorf("got[0] = %s, want %s (transport subnet)", got[0].String(), wantSubnet.String())
	}

	_, wantOverlay, _ := net.ParseCIDR("172.20.70.1/32")
	if got[1].String() != wantOverlay.String() {
		t.Errorf("got[1] = %s, want %s (master overlay /32)", got[1].String(), wantOverlay.String())
	}
}

// TestEndpointMinimalAllowedIPs_OverlayWithPrefix verifies that a master overlay IP
// supplied with an existing prefix (e.g. "172.20.70.1/27") is still normalised to /32.
func TestEndpointMinimalAllowedIPs_OverlayWithPrefix(t *testing.T) {
	t.Parallel()

	got, err := buildEndpointPeerAllowedIPs("10.255.0.4/30", "172.20.70.1/27")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	_, want32, _ := net.ParseCIDR("172.20.70.1/32")
	if got[1].String() != want32.String() {
		t.Errorf("overlay entry = %s, want /32 = %s", got[1].String(), want32.String())
	}
}

// TestEndpointMinimalAllowedIPs_InvalidSubnet verifies that a malformed transport
// subnet CIDR returns an error instead of silently producing a wrong route.
func TestEndpointMinimalAllowedIPs_InvalidSubnet(t *testing.T) {
	t.Parallel()

	_, err := buildEndpointPeerAllowedIPs("not-a-cidr", "172.20.70.1")
	if err == nil {
		t.Fatal("expected error for invalid transport subnet, got nil")
	}
}

// TestEndpointMinimalAllowedIPs_EmptySubnet verifies that an empty transport subnet
// is rejected with an error.
func TestEndpointMinimalAllowedIPs_EmptySubnet(t *testing.T) {
	t.Parallel()

	_, err := buildEndpointPeerAllowedIPs("", "172.20.70.1")
	if err == nil {
		t.Fatal("expected error for empty transport subnet, got nil")
	}
}

// TestEndpointMinimalAllowedIPs_EmptyOverlay verifies that an empty master overlay IP
// is rejected with an error.
func TestEndpointMinimalAllowedIPs_EmptyOverlay(t *testing.T) {
	t.Parallel()

	_, err := buildEndpointPeerAllowedIPs("10.255.0.0/30", "")
	if err == nil {
		t.Fatal("expected error for empty master overlay IP, got nil")
	}
}

// TestEndpointMinimalAllowedIPs_InvalidOverlay verifies that an unparseable overlay IP
// returns an error.
func TestEndpointMinimalAllowedIPs_InvalidOverlay(t *testing.T) {
	t.Parallel()

	_, err := buildEndpointPeerAllowedIPs("10.255.0.0/30", "not-an-ip")
	if err == nil {
		t.Fatal("expected error for invalid overlay IP, got nil")
	}
}

// ---------------------------------------------------------------------------
// TestEndpointCreateMasterInterface
// ---------------------------------------------------------------------------

// testIfaceRecord is captured by the endpointCreateIfaceFn mock.
type testIfaceRecord struct {
	name string
	mtu  int
}

// testConfigRecord is captured by the endpointConfigureIfaceFn mock.
type testConfigRecord struct {
	cfg wg.Config
}

// setupEndpointSeams installs mock implementations for all kernel-touching seams used
// by createMasterInterface and restores the originals via t.Cleanup.
//
// Returns pointers to the captured call records so the caller can assert on them.
func setupEndpointSeams(t *testing.T) (ifaceRec *testIfaceRecord, cfgRec *testConfigRecord, addedAddrs *[]string) {
	t.Helper()

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

	ifaceRec = &testIfaceRecord{}
	cfgRec = &testConfigRecord{}
	addrs := make([]string, 0)
	addedAddrs = &addrs

	// endpointCreateIfaceFn: record name+mtu, return a non-nil zero-value Interface
	// pointer so that subsequent nil-checks on the returned iface pass.
	// The zero-value Interface has name="" and nil dev/tun/uapi — Configure and Close
	// are intercepted by subsequent seams before any field is dereferenced.
	endpointCreateIfaceFn = func(name string, mtu int, _ *device.Logger) (*wg.Interface, error) {
		ifaceRec.name = name
		ifaceRec.mtu = mtu
		// Return a non-nil pointer to a zero Interface. The struct has unexported fields;
		// the zero value is safe here because Configure and Close are seamed out below.
		iface := &wg.Interface{}
		return iface, nil
	}

	endpointConfigureIfaceFn = func(_ *wg.Interface, cfg wg.Config) error {
		cfgRec.cfg = cfg
		return nil
	}

	endpointSetIfaceUpFn = func(_ string) error { return nil }

	endpointAddInterfaceAddress = func(_ string, addr string) error {
		*addedAddrs = append(*addedAddrs, addr)
		return nil
	}

	return ifaceRec, cfgRec, addedAddrs
}

// TestEndpointCreateMasterInterface_NoPeer verifies that createMasterInterface
// creates an interface named "wg-<master>" on the correct listen port, with zero
// peers when masterPubkey is zero (first-boot / not yet known).
// NOT parallel: modifies package-level seam vars shared across tests.
func TestEndpointCreateMasterInterface_NoPeer(t *testing.T) {
	ifaceRec, cfgRec, _ := setupEndpointSeams(t)

	master := topology.MasterNode{
		Name:      "master-a",
		Host:      "10.0.0.1",
		OverlayIP: "172.20.70.1",
	}

	runner := &EndpointRunner{
		node: &Node{
			config: NodeConfig{
				Name:       "endpoint-a",
				ConfigDir:  t.TempDir(),
				ListenPort: 51820,
			},
			logger: zerolog.Nop(),
		},
	}
	// EnsureKeypair needs a valid dir; it will generate a key on first call.

	var zeroKey wg.Key
	err := runner.createMasterInterface(master, 0, "", "", "", zeroKey)
	if err != nil {
		t.Fatalf("createMasterInterface returned error: %v", err)
	}

	// Interface name must be "wg-master-a".
	if ifaceRec.name != "wg-master-a" {
		t.Errorf("interface name = %q, want %q", ifaceRec.name, "wg-master-a")
	}

	// Listen port must be ListenPort + portOffset (0).
	if cfgRec.cfg.ListenPort == nil || *cfgRec.cfg.ListenPort != 51820 {
		t.Errorf("listen port = %v, want 51820", cfgRec.cfg.ListenPort)
	}

	// No peers — pubkey is zero.
	if len(cfgRec.cfg.Peers) != 0 {
		t.Errorf("peer count = %d, want 0", len(cfgRec.cfg.Peers))
	}

	// Interface must be stored in the map under master.Name.
	if runner.getIface("master-a") == nil {
		t.Error("expected iface stored under 'master-a', got nil")
	}
}

// TestEndpointCreateMasterInterface verifies the full happy path: interface name,
// listen port offset, peer count=1, and minimal AllowedIPs when masterPubkey is set.
// NOT parallel: modifies package-level seam vars shared across tests.
func TestEndpointCreateMasterInterface(t *testing.T) {
	ifaceRec, cfgRec, addedAddrs := setupEndpointSeams(t)

	master := topology.MasterNode{
		Name:      "master-a",
		Host:      "10.0.0.1",
		OverlayIP: "172.20.70.1",
	}

	// Use a non-zero public key (any 32-byte non-zero value).
	var masterPubkey wg.Key
	masterPubkey[0] = 0x42

	runner := &EndpointRunner{
		node: &Node{
			config: NodeConfig{
				Name:       "endpoint-a",
				ConfigDir:  t.TempDir(),
				ListenPort: 51820,
			},
			logger: zerolog.Nop(),
		},
	}

	err := runner.createMasterInterface(master, 2, "10.255.0.2", "10.255.0.1", "10.255.0.0/30", masterPubkey)
	if err != nil {
		t.Fatalf("createMasterInterface returned error: %v", err)
	}

	// Interface name must be "wg-master-a".
	if ifaceRec.name != "wg-master-a" {
		t.Errorf("interface name = %q, want %q", ifaceRec.name, "wg-master-a")
	}

	// Listen port = base + portOffset (51820 + 2 = 51822).
	if cfgRec.cfg.ListenPort == nil || *cfgRec.cfg.ListenPort != 51822 {
		t.Errorf("listen port = %v, want 51822", cfgRec.cfg.ListenPort)
	}

	// Exactly one peer configured.
	if len(cfgRec.cfg.Peers) != 1 {
		t.Fatalf("peer count = %d, want 1", len(cfgRec.cfg.Peers))
	}

	// Peer AllowedIPs must be minimal: [10.255.0.0/30, 172.20.70.1/32].
	peer := cfgRec.cfg.Peers[0]
	if len(peer.AllowedIPs) != 2 {
		t.Fatalf("AllowedIPs count = %d, want 2: %v", len(peer.AllowedIPs), peer.AllowedIPs)
	}

	_, wantSubnet, _ := net.ParseCIDR("10.255.0.0/30")
	if peer.AllowedIPs[0].String() != wantSubnet.String() {
		t.Errorf("AllowedIPs[0] = %s, want %s", peer.AllowedIPs[0].String(), wantSubnet.String())
	}

	_, wantOverlay, _ := net.ParseCIDR("172.20.70.1/32")
	if peer.AllowedIPs[1].String() != wantOverlay.String() {
		t.Errorf("AllowedIPs[1] = %s, want %s", peer.AllowedIPs[1].String(), wantOverlay.String())
	}

	// Keepalive must be 25 s.
	if peer.PersistentKeepaliveInterval == nil {
		t.Error("PersistentKeepaliveInterval is nil, want 25s")
	} else if peer.PersistentKeepaliveInterval.Seconds() != 25 {
		t.Errorf("keepalive = %v, want 25s", *peer.PersistentKeepaliveInterval)
	}

	// Transport IP must have been added to the interface.
	if len(*addedAddrs) != 1 || (*addedAddrs)[0] != "10.255.0.2" {
		t.Errorf("added addresses = %v, want [10.255.0.2]", *addedAddrs)
	}

	// Interface stored under master name.
	if runner.getIface("master-a") == nil {
		t.Error("expected iface stored under 'master-a', got nil")
	}
}

// TestEndpointCreateMasterInterface_NameTruncation verifies that a master name longer
// than 12 characters is truncated so the iface name stays within IFNAMSIZ (15 chars).
// NOT parallel: modifies package-level seam vars shared across tests.
func TestEndpointCreateMasterInterface_NameTruncation(t *testing.T) {
	ifaceRec, _, _ := setupEndpointSeams(t)

	master := topology.MasterNode{
		Name:      "very-long-master-name", // 21 chars — must be truncated to 12
		Host:      "10.0.0.1",
		OverlayIP: "172.20.70.1",
	}

	runner := &EndpointRunner{
		node: &Node{
			config: NodeConfig{
				Name:       "endpoint-a",
				ConfigDir:  t.TempDir(),
				ListenPort: 51820,
			},
			logger: zerolog.Nop(),
		},
	}

	var zeroKey wg.Key
	if err := runner.createMasterInterface(master, 0, "", "", "", zeroKey); err != nil {
		t.Fatalf("createMasterInterface returned error: %v", err)
	}

	// "wg-" (3) + 12 chars = 15 chars total — exactly IFNAMSIZ-1.
	wantName := "wg-very-long-ma"
	if ifaceRec.name != wantName {
		t.Errorf("interface name = %q, want %q", ifaceRec.name, wantName)
	}
}

// TestEndpointCreateMasterInterface_PortOffset verifies that portOffset is correctly
// added to the base listen port.
// NOT parallel: modifies package-level seam vars shared across tests.
func TestEndpointCreateMasterInterface_PortOffset(t *testing.T) {
	_, cfgRec, _ := setupEndpointSeams(t)

	master := topology.MasterNode{
		Name:      "m",
		Host:      "10.0.0.1",
		OverlayIP: "172.20.70.1",
	}
	runner := &EndpointRunner{
		node: &Node{
			config: NodeConfig{
				Name:       "endpoint-a",
				ConfigDir:  t.TempDir(),
				ListenPort: 51820,
			},
			logger: zerolog.Nop(),
		},
	}

	var zeroKey wg.Key
	if err := runner.createMasterInterface(master, 3, "", "", "", zeroKey); err != nil {
		t.Fatalf("createMasterInterface returned error: %v", err)
	}

	if cfgRec.cfg.ListenPort == nil || *cfgRec.cfg.ListenPort != 51823 {
		t.Errorf("listen port = %v, want 51823", cfgRec.cfg.ListenPort)
	}
}

// ---------------------------------------------------------------------------
// TestDeriveTransportSubnet
// ---------------------------------------------------------------------------

func TestDeriveTransportSubnet(t *testing.T) {
	t.Parallel()

	cases := []struct {
		ip        string
		prefixLen int
		want      string
	}{
		{"10.255.0.2", 30, "10.255.0.0/30"},
		{"10.255.0.1", 30, "10.255.0.0/30"},
		{"10.255.0.6", 30, "10.255.0.4/30"},
		{"192.168.1.100", 24, "192.168.1.0/24"},
		{"10.0.0.1", 0, "10.0.0.0/30"},  // invalid prefixLen → default 30
		{"10.0.0.1", 33, "10.0.0.0/30"}, // invalid prefixLen → default 30
		{"", 30, ""},
		{"not-an-ip", 30, ""},
	}

	for _, tc := range cases {
		got := deriveTransportSubnet(tc.ip, tc.prefixLen)
		if got != tc.want {
			t.Errorf("deriveTransportSubnet(%q, %d) = %q, want %q", tc.ip, tc.prefixLen, got, tc.want)
		}
	}
}
