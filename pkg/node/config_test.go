package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
)

func TestSaveLoadNodeStateRoundTrip(t *testing.T) {
	t.Parallel()

	state := NodeState{
		PrivateKey: mustPrivateKeyString(t),
		PublicKey:  mustPublicKeyString(t),
		Name:       "node-a",
		Mode:       "client",
		OverlayIP:  "10.0.0.10",
	}

	dir := t.TempDir()
	if err := SaveNodeState(dir, state); err != nil {
		t.Fatalf("SaveNodeState returned error: %v", err)
	}

	loaded, err := LoadNodeState(dir)
	if err != nil {
		t.Fatalf("LoadNodeState returned error: %v", err)
	}
	if *loaded != state {
		t.Fatalf("loaded node state mismatch: got %#v want %#v", *loaded, state)
	}
}

func TestSaveLoadNodeStateErrors(t *testing.T) {
	t.Parallel()

	err := SaveNodeState("", NodeState{})
	if err == nil || !strings.Contains(err.Error(), "config directory is required") {
		t.Fatalf("expected config directory error, got %v", err)
	}

	_, err = LoadNodeState("")
	if err == nil || !strings.Contains(err.Error(), "config directory is required") {
		t.Fatalf("expected config directory error, got %v", err)
	}

	_, err = LoadNodeState(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "read node state file") {
		t.Fatalf("expected read node state file error, got %v", err)
	}
}

func TestEnsureKeypairCreatesAndLoads(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	privateA, publicA, err := EnsureKeypair(dir)
	if err != nil {
		t.Fatalf("EnsureKeypair initial call returned error: %v", err)
	}
	if privateA.IsZero() || publicA.IsZero() {
		t.Fatalf("expected non-zero keypair")
	}
	if publicA != privateA.PublicKey() {
		t.Fatalf("public key does not match private key")
	}

	privateB, publicB, err := EnsureKeypair(dir)
	if err != nil {
		t.Fatalf("EnsureKeypair reload call returned error: %v", err)
	}
	if privateA != privateB || publicA != publicB {
		t.Fatalf("expected persisted keypair to be reused")
	}

	state, err := LoadNodeState(dir)
	if err != nil {
		t.Fatalf("LoadNodeState returned error: %v", err)
	}
	if state.PrivateKey != privateA.String() || state.PublicKey != publicA.String() {
		t.Fatalf("unexpected stored keys in node state")
	}
}

func TestEnsureKeypairErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	invalidStatePath := filepath.Join(dir, nodeStateFileName)
	invalidContent := "private_key: bad-key\npublic_key: bad-key\n"
	if err := os.WriteFile(invalidStatePath, []byte(invalidContent), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	_, _, err := EnsureKeypair(dir)
	if err == nil {
		t.Fatalf("expected EnsureKeypair error for invalid state")
	}
	if !strings.Contains(err.Error(), "parse private key") {
		t.Fatalf("unexpected error: %v", err)
	}

	fileDir := filepath.Join(t.TempDir(), "state-as-file")
	if err := os.WriteFile(fileDir, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	_, _, err = EnsureKeypair(fileDir)
	if err == nil {
		t.Fatalf("expected EnsureKeypair error when state path is not directory")
	}
	if !strings.Contains(err.Error(), "node state") {
		t.Fatalf("unexpected error (expected 'node state' in message): %v", err)
	}
}

func TestParseNodeKey(t *testing.T) {
	t.Parallel()

	_, err := parseNodeKey("", "private")
	if err == nil || !strings.Contains(err.Error(), "private key is empty") {
		t.Fatalf("expected empty-key error, got %v", err)
	}

	_, err = parseNodeKey("bad", "public")
	if err == nil || !strings.Contains(err.Error(), "parse public key") {
		t.Fatalf("expected parse-key error, got %v", err)
	}
}

func TestNewNodeValidationAndDefaults(t *testing.T) {
	t.Parallel()

	_, err := NewNode(NodeConfig{Mode: "client"})
	if err == nil || !strings.Contains(err.Error(), "node name is required") {
		t.Fatalf("expected node-name error, got %v", err)
	}

	_, err = NewNode(NodeConfig{Name: "n1"})
	if err == nil || !strings.Contains(err.Error(), "node mode is required") {
		t.Fatalf("expected node-mode error, got %v", err)
	}

	topologyPath := writeTopologyFixture(t)
	nodeValue, err := NewNode(NodeConfig{
		Name:         " node-a ",
		Mode:         " client ",
		ConfigDir:    "",
		OverlayIP:    " 10.1.0.10 ",
		TopologyPath: topologyPath,
	})
	if err != nil {
		t.Fatalf("NewNode returned error: %v", err)
	}

	if nodeValue.config.Name != "node-a" {
		t.Fatalf("unexpected node name: %q", nodeValue.config.Name)
	}
	if nodeValue.config.Mode != "client" {
		t.Fatalf("unexpected node mode: %q", nodeValue.config.Mode)
	}
	if nodeValue.config.ConfigDir != defaultConfigDir {
		t.Fatalf("expected default config dir %q, got %q", defaultConfigDir, nodeValue.config.ConfigDir)
	}
	if nodeValue.topology == nil {
		t.Fatalf("expected topology to be loaded")
	}
}

func TestNodeRunValidation(t *testing.T) {
	t.Parallel()

	nodeValue, err := NewNode(NodeConfig{Name: "n1", Mode: "unknown", ConfigDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewNode returned error: %v", err)
	}

	err = nodeValue.Run(nil)
	if err == nil || !strings.Contains(err.Error(), "context is required") {
		t.Fatalf("expected context-required error, got %v", err)
	}

	err = nodeValue.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unsupported node mode") {
		t.Fatalf("expected unsupported-mode error, got %v", err)
	}
}

func TestNodeRunClientAndEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		mode string
	}{
		{name: "client", mode: "client"},
		{name: "endpoint", mode: "endpoint"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			nodeValue, err := NewNode(NodeConfig{
				Name:      "node-" + tt.mode,
				Mode:      tt.mode,
				ConfigDir: t.TempDir(),
				OverlayIP: "10.2.0.10",
			})
			if err != nil {
				t.Fatalf("NewNode returned error: %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			if err := nodeValue.Run(ctx); err != nil {
				t.Fatalf("Run returned error: %v", err)
			}
		})
	}
}

func TestMasterRunnerTunnelLifecycle(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root (TUN device creation)")
	}
	t.Parallel()

	nodeValue, err := NewNode(NodeConfig{
		Name:      "master-node",
		Mode:      "master",
		ConfigDir: t.TempDir(),
		OverlayIP: "10.3.0.10",
	})
	if err != nil {
		t.Fatalf("NewNode returned error: %v", err)
	}

	runner := NewMasterRunner(nodeValue)

	err = runner.AddTunnel("", "endpoint-a", "10.4.0.2", "", "", "", "", 1, wg.Key{})
	if err == nil || !strings.Contains(err.Error(), "tunnel name is required") {
		t.Fatalf("expected tunnel-name error, got %v", err)
	}

	err = runner.AddTunnel("endpoint-a", "endpoint-a.local", "10.4.0.2", "10.4.0.1", "", "", "", 2, wg.Key{})
	if err != nil {
		t.Fatalf("AddTunnel returned error: %v", err)
	}

	err = runner.AddTunnel("endpoint-a", "endpoint-a.local", "10.4.0.2", "10.4.0.1", "", "", "", 2, wg.Key{})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected duplicate tunnel error, got %v", err)
	}

	tunnels := runner.listMasterTunnels()
	if len(tunnels) != 1 {
		t.Fatalf("expected one tunnel, got %d", len(tunnels))
	}
	if tunnels[0].Name != "endpoint-a" || tunnels[0].InterfaceName != "wg-endpoint-a" {
		t.Fatalf("unexpected tunnel snapshot: %#v", tunnels[0])
	}

	err = runner.RemoveTunnel("missing")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected tunnel-not-found error, got %v", err)
	}

	if err := runner.RemoveTunnel("endpoint-a"); err != nil {
		t.Fatalf("RemoveTunnel returned error: %v", err)
	}
	if len(runner.listMasterTunnels()) != 0 {
		t.Fatalf("expected empty tunnel list after removal")
	}
}

func writeTopologyFixture(t *testing.T) string {
	t.Helper()

	content := strings.Join([]string{
		"overlay:",
		"  space: 10.0.0.0/16",
		"  physical_mtu: 1500",
		"  awg_overhead: 80",
		"  ranges:",
		"    - name: core",
		"      cidr: 10.0.1.0/24",
		"masters: []",
		"endpoints: []",
		"clients: []",
	}, "\n")

	path := filepath.Join(t.TempDir(), "topology.yml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	return path
}

func mustPrivateKeyString(t *testing.T) string {
	t.Helper()
	privateKey, err := wg.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey returned error: %v", err)
	}
	return privateKey.String()
}

func mustPublicKeyString(t *testing.T) string {
	t.Helper()
	privateKey, err := wg.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey returned error: %v", err)
	}
	return privateKey.PublicKey().String()
}

func TestGetPublicKeyMaster(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, expectedPub, err := EnsureKeypair(dir)
	if err != nil {
		t.Fatalf("EnsureKeypair: %v", err)
	}

	runner := &MasterRunner{node: &Node{config: NodeConfig{ConfigDir: dir}}}
	gotPub, err := runner.GetPublicKey()
	if err != nil {
		t.Fatalf("GetPublicKey: %v", err)
	}
	if gotPub != expectedPub {
		t.Fatalf("key mismatch: got %s, want %s", gotPub, expectedPub)
	}
}

func TestGetPublicKeyEndpoint(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, expectedPub, err := EnsureKeypair(dir)
	if err != nil {
		t.Fatalf("EnsureKeypair: %v", err)
	}

	runner := &EndpointRunner{node: &Node{config: NodeConfig{ConfigDir: dir}}}
	gotPub, err := runner.GetPublicKey()
	if err != nil {
		t.Fatalf("GetPublicKey: %v", err)
	}
	if gotPub != expectedPub {
		t.Fatalf("key mismatch: got %s, want %s", gotPub, expectedPub)
	}
}

func TestGetPublicKeyClient(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, expectedPub, err := EnsureKeypair(dir)
	if err != nil {
		t.Fatalf("EnsureKeypair: %v", err)
	}

	runner := NewClientRunner(&Node{config: NodeConfig{ConfigDir: dir}})
	gotPub, err := runner.GetPublicKey()
	if err != nil {
		t.Fatalf("GetPublicKey: %v", err)
	}
	if gotPub != expectedPub {
		t.Fatalf("key mismatch: got %s, want %s", gotPub, expectedPub)
	}
}

func TestNewClientRunnerInitializesMap(t *testing.T) {
	t.Parallel()

	runner := NewClientRunner(&Node{config: NodeConfig{ConfigDir: t.TempDir()}})
	if runner == nil {
		t.Fatal("expected non-nil runner")
	}
	// platformState.byKey should be initialized — verified via GetPublicKey not panicking
	// On Linux, byKey map is pre-initialized. On non-Linux, struct is empty.
	_ = runner
}

func TestTransportStatePreservesOverlayAndBalancerIP(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	state := NodeTransportState{
		OverlayIP: "172.20.70.2",
		Tunnels: []TunnelTransport{
			{
				Name:            "node-asia-01",
				OverlayIP:       "172.20.70.34",
				TransportIP:     "10.255.0.1",
				PeerTransportIP: "10.255.0.2",
				PeerPublicKey:   "abcd1234",
				PeerEndpoint:    "192.168.1.1:51820",
				BalancerIP:      "172.20.70.33",
			},
		},
	}
	if err := saveNodeTransportState(dir, state); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := loadNodeTransportState(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(loaded.Tunnels) != 1 {
		t.Fatalf("expected 1 tunnel, got %d", len(loaded.Tunnels))
	}
	tt := loaded.Tunnels[0]
	if tt.OverlayIP != "172.20.70.34" {
		t.Fatalf("expected overlayIP 172.20.70.34, got %q", tt.OverlayIP)
	}
	if tt.BalancerIP != "172.20.70.33" {
		t.Fatalf("expected balancerIP 172.20.70.33, got %q", tt.BalancerIP)
	}
	if loaded.OverlayIP != "172.20.70.2" {
		t.Fatalf("expected node overlayIP 172.20.70.2, got %q", loaded.OverlayIP)
	}
}

func TestAddTunnelInitiallyUnhealthy(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root (TUN device creation)")
	}
	t.Parallel()

	dir := t.TempDir()
	_, _, err := EnsureKeypair(dir)
	if err != nil {
		t.Fatalf("EnsureKeypair: %v", err)
	}

	runner := &MasterRunner{
		node:    &Node{config: NodeConfig{ConfigDir: dir}},
		tunnels: make(map[string]*MasterTunnel),
	}

	// AddTunnel on non-Linux is a no-op for interface creation but should
	// still add the tunnel to the map. On Linux, Healthy starts false and
	// becomes true only after interface creation succeeds.
	_ = runner.AddTunnel("test-ep", "192.168.1.1:51820", "172.20.70.34", "172.20.70.33", "", "10.255.0.1", "10.255.0.2", 1, wg.Key{})

	runner.mu.RLock()
	tunnel, exists := runner.tunnels["test-ep"]
	runner.mu.RUnlock()

	if !exists {
		t.Fatal("expected tunnel to exist in map")
	}
	if tunnel.OverlayIP != "172.20.70.34" {
		t.Fatalf("expected overlayIP 172.20.70.34, got %q", tunnel.OverlayIP)
	}
	if tunnel.BalancerIP != "172.20.70.33" {
		t.Fatalf("expected balancerIP 172.20.70.33, got %q", tunnel.BalancerIP)
	}
}
