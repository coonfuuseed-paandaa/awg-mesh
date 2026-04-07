package grpcserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/pkg/tls"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
	proto "github.com/coonfuuseed-paandaa/awg-mesh/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type testTunnelManager struct {
	addTunnelCalls []addTunnelCall
	listTunnels    []TunnelInfo
	removeCalls    []string
	getParamsCalls []string
	getParamsCfg   wg.Config
	addErr         error
	removeErr      error
	getParamsErr   error
	listenPort     int
}

func (m *testTunnelManager) GetListenPort(_ string) (int, error) {
	return m.listenPort, nil
}

type addTunnelCall struct {
	name                string
	host                string
	overlayIP           string
	balancerIP          string
	transportSubnet     string
	masterTransportIP   string
	endpointTransportIP string
	weight              int
	peerKey             wg.Key
}

func (m *testTunnelManager) AddTunnel(
	name,
	endpointHost,
	overlayIP,
	balancerIP,
	transportSubnet,
	masterTransportIP,
	endpointTransportIP string,
	weight int,
	peerPublicKey wg.Key,
) error {
	m.addTunnelCalls = append(m.addTunnelCalls, addTunnelCall{
		name:                name,
		host:                endpointHost,
		overlayIP:           overlayIP,
		balancerIP:          balancerIP,
		transportSubnet:     transportSubnet,
		masterTransportIP:   masterTransportIP,
		endpointTransportIP: endpointTransportIP,
		weight:              weight,
		peerKey:             peerPublicKey,
	})
	return m.addErr
}

func (m *testTunnelManager) ListTunnels() []TunnelInfo {
	result := make([]TunnelInfo, 0, len(m.listTunnels))
	result = append(result, m.listTunnels...)
	return result
}

func (m *testTunnelManager) GetParams(tunnelName string) (wg.Config, error) {
	m.getParamsCalls = append(m.getParamsCalls, tunnelName)
	return m.getParamsCfg, m.getParamsErr
}

func (m *testTunnelManager) RemoveTunnel(name string) error {
	m.removeCalls = append(m.removeCalls, name)
	return m.removeErr
}

type testParamApplier struct {
	calls []applyCall
	err   error
}

type applyCall struct {
	tunnelName string
	cfg        wg.Config
}

func (m *testParamApplier) ApplyParams(tunnelName string, cfg wg.Config) error {
	m.calls = append(m.calls, applyCall{
		tunnelName: tunnelName,
		cfg:        cfg,
	})
	return m.err
}

type testPeerManager struct {
	listPeers   []PeerInfo
	addCalls    []addPeerCall
	removeCalls [][]byte
	addErr      error
	removeErr   error
}

type testTransportPeerManager struct {
	testPeerManager
	configDir      string
	configureCalls []configureTransportCall
	configureErr   error
	stateSeen      bool
}

type configureTransportCall struct {
	pubkeyHex string
	localIP   string
	peerIP    string
}

type addPeerCall struct {
	publicKey           []byte
	presharedKey        []byte
	allowedIPs          []string
	endpointHost        string
	persistentKeepalive int32
}

func (m *testPeerManager) ListPeers() []PeerInfo {
	result := make([]PeerInfo, 0, len(m.listPeers))
	for _, peer := range m.listPeers {
		result = append(result, PeerInfo{
			PublicKey:     append([]byte(nil), peer.PublicKey...),
			Endpoint:      peer.Endpoint,
			AllowedIPs:    append([]string(nil), peer.AllowedIPs...),
			LastHandshake: peer.LastHandshake,
			TxBytes:       peer.TxBytes,
			RxBytes:       peer.RxBytes,
		})
	}
	return result
}

func (m *testPeerManager) AddPeer(publicKey []byte, presharedKey []byte, allowedIPs []string, endpointHost string, persistentKeepalive int32) error {
	m.addCalls = append(m.addCalls, addPeerCall{
		publicKey:           append([]byte(nil), publicKey...),
		presharedKey:        append([]byte(nil), presharedKey...),
		allowedIPs:          append([]string(nil), allowedIPs...),
		endpointHost:        endpointHost,
		persistentKeepalive: persistentKeepalive,
	})
	return m.addErr
}

func (m *testPeerManager) RemovePeer(publicKey []byte) error {
	m.removeCalls = append(m.removeCalls, append([]byte(nil), publicKey...))
	return m.removeErr
}

func (m *testTransportPeerManager) ConfigureTransport(pubkeyHex, localIP, peerIP string) error {
	m.configureCalls = append(m.configureCalls, configureTransportCall{
		pubkeyHex: pubkeyHex,
		localIP:   localIP,
		peerIP:    peerIP,
	})

	if strings.TrimSpace(m.configDir) != "" {
		state, err := loadNodeTransportState(m.configDir)
		if err == nil && len(state.Tunnels) > 0 {
			m.stateSeen = true
		}
	}

	return m.configureErr
}

type testKeyProvider struct {
	key wg.Key
	err error
}

func (m *testKeyProvider) GetPublicKey() (wg.Key, error) {
	return m.key, m.err
}

type testNodeStateProvider struct {
	state NodeState
}

func (m *testNodeStateProvider) GetNodeState() NodeState {
	return m.state
}

func TestNewAgentHandlerConstructors(t *testing.T) {
	t.Parallel()

	logger := zerolog.Nop()
	configDir := t.TempDir()

	t.Run("default constructor sets unsupported mode handlers", func(t *testing.T) {
		t.Parallel()

		handler := NewAgentHandler(configDir, logger)
		if handler == nil {
			t.Fatal("expected handler instance, got nil")
		}

		_, err := handler.RotateParams(context.Background(), &proto.RotateParamsRequest{
			TunnelName: "tunnel-1",
			Tier:       1,
			NewParams:  &proto.AwgParams{Jc: 1},
		})
		if err == nil {
			t.Fatal("expected rotate params error")
		}
		if status.Code(err) != codes.Unimplemented {
			t.Fatalf("expected unimplemented code, got %v", status.Code(err))
		}
	})

	t.Run("full constructor injects dependencies", func(t *testing.T) {
		t.Parallel()

		tunnelMgr := &testTunnelManager{}
		paramApplier := &testParamApplier{}
		handler := NewAgentHandlerFull(configDir, logger, tunnelMgr, paramApplier, nil, nil, nil, nil, nil)

		peerKey := make([]byte, 32)
		for i := range peerKey {
			peerKey[i] = 1
		}

		addResp, err := handler.AddTunnel(context.Background(), &proto.AddTunnelRequest{
			Name:          "  test-tunnel ",
			EndpointHost:  "  host.example ",
			OverlayIp:     "  10.0.0.1/32 ",
			BalancerIp:    "  ",
			PeerPublicKey: peerKey,
			Weight:        0,
		})
		if err != nil {
			t.Fatalf("AddTunnel returned error: %v", err)
		}
		if len(addResp.InterfaceName) == 0 {
			t.Fatal("expected interface name in AddTunnel response")
		}
		if addResp.InterfaceName != "wg-test-tunnel" {
			t.Fatalf("unexpected interface name: %q", addResp.InterfaceName)
		}
		if len(tunnelMgr.addTunnelCalls) != 1 {
			t.Fatalf("expected one AddTunnel call, got %d", len(tunnelMgr.addTunnelCalls))
		}

		call := tunnelMgr.addTunnelCalls[0]
		if call.name != "test-tunnel" {
			t.Fatalf("expected trimmed tunnel name, got %q", call.name)
		}
		if call.host != "host.example" {
			t.Fatalf("expected trimmed host, got %q", call.host)
		}
		if call.overlayIP != "10.0.0.1/32" {
			t.Fatalf("expected trimmed overlay IP, got %q", call.overlayIP)
		}
		if call.balancerIP != "" {
			t.Fatalf("expected empty balancer IP, got %q", call.balancerIP)
		}
		if call.transportSubnet != "" {
			t.Fatalf("expected empty transport subnet, got %q", call.transportSubnet)
		}
		if call.masterTransportIP != "" {
			t.Fatalf("expected empty master transport IP, got %q", call.masterTransportIP)
		}
		if call.endpointTransportIP != "" {
			t.Fatalf("expected empty endpoint transport IP, got %q", call.endpointTransportIP)
		}
		if call.weight != 1 {
			t.Fatalf("expected default weight 1, got %d", call.weight)
		}

		_, err = handler.RotateParams(context.Background(), &proto.RotateParamsRequest{
			TunnelName: "  test-tunnel ",
			Tier:       1,
			NewParams:  &proto.AwgParams{Jc: 10, I1: "seed"},
		})
		if err != nil {
			t.Fatalf("RotateParams returned error: %v", err)
		}
		if len(paramApplier.calls) != 1 {
			t.Fatalf("expected one ApplyParams call, got %d", len(paramApplier.calls))
		}
		if paramApplier.calls[0].tunnelName != "test-tunnel" {
			t.Fatalf("expected trimmed tunnel name, got %q", paramApplier.calls[0].tunnelName)
		}
	})
}

func TestInitWritesInitArtifacts(t *testing.T) {
	t.Parallel()

	caCertPEM, nodeCertPEM, nodeKeyPEM := generateTestCerts(t)

	handler := NewAgentHandler(t.TempDir(), zerolog.Nop())
	resp, err := handler.Init(context.Background(), &proto.InitRequest{
		CaCert:   caCertPEM,
		NodeCert: nodeCertPEM,
		NodeKey:  nodeKeyPEM,
		Config:   &proto.NodeConfig{},
	})
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatal("expected successful Init response")
	}
	if !strings.Contains(resp.GetMessage(), "initialized") {
		t.Fatalf("unexpected response message: %q", resp.GetMessage())
	}

	configDir := handler.configDir
	assertFileContents(t, filepath.Join(configDir, "tls", "ca.crt"), string(caCertPEM))
	assertFileContents(t, filepath.Join(configDir, "tls", "node.crt"), string(nodeCertPEM))
	assertFileContents(t, filepath.Join(configDir, "tls", "node.key"), string(nodeKeyPEM))
	assertFileContents(t, filepath.Join(configDir, "node-config.json"), "{}")
}

func TestInitReturnsPublicKeyFromKeyProvider(t *testing.T) {
	t.Parallel()

	key, err := wg.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pubKey := key.PublicKey()

	caCertPEM, nodeCertPEM, nodeKeyPEM := generateTestCerts(t)

	kp := &testKeyProvider{key: pubKey}
	handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), nil, nil, nil, nil, nil, nil, kp)
	resp, initErr := handler.Init(context.Background(), &proto.InitRequest{
		CaCert:   caCertPEM,
		NodeCert: nodeCertPEM,
		NodeKey:  nodeKeyPEM,
	})
	if initErr != nil {
		t.Fatalf("Init returned error: %v", initErr)
	}
	if !resp.GetSuccess() {
		t.Fatal("expected successful Init")
	}
	if len(resp.GetNodePublicKey()) != 32 {
		t.Fatalf("expected 32-byte public key, got %d bytes", len(resp.GetNodePublicKey()))
	}
	var gotKey wg.Key
	copy(gotKey[:], resp.GetNodePublicKey())
	if gotKey != pubKey {
		t.Fatalf("public key mismatch: got %s, want %s", gotKey, pubKey)
	}
}

func TestAddTunnelReturnsMasterPublicKey(t *testing.T) {
	t.Parallel()

	key, err := wg.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pubKey := key.PublicKey()

	mgr := &testTunnelManager{}
	kp := &testKeyProvider{key: pubKey}
	handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), mgr, nil, nil, nil, nil, nil, kp)

	peerKey := make([]byte, 32)
	for i := range peerKey {
		peerKey[i] = 0xAA
	}

	resp, addErr := handler.AddTunnel(context.Background(), &proto.AddTunnelRequest{
		Name:          "test-tunnel",
		PeerPublicKey: peerKey,
		Weight:        1,
	})
	if addErr != nil {
		t.Fatalf("AddTunnel returned error: %v", addErr)
	}
	if !resp.GetSuccess() {
		t.Fatal("expected successful AddTunnel")
	}
	if len(resp.GetMasterPublicKey()) != 32 {
		t.Fatalf("expected 32-byte master public key, got %d bytes", len(resp.GetMasterPublicKey()))
	}
	var gotKey wg.Key
	copy(gotKey[:], resp.GetMasterPublicKey())
	if gotKey != pubKey {
		t.Fatalf("master public key mismatch: got %s, want %s", gotKey, pubKey)
	}
}

func TestRotateParamsMapsProtoValuesToConfig(t *testing.T) {
	t.Parallel()

	paramApplier := &testParamApplier{}
	handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), nil, paramApplier, nil, nil, nil, nil, nil)
	resp, err := handler.RotateParams(context.Background(), &proto.RotateParamsRequest{
		TunnelName: "rotate-tunnel",
		Tier:       1,
		NewParams: &proto.AwgParams{
			Jc:   11,
			Jmin: 7,
			Jmax: 9,
			S1:   19,
			H1:   101,
			I1:   "alpha",
			I3:   "gamma",
		},
	})
	if err != nil {
		t.Fatalf("RotateParams returned error: %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatal("expected successful rotate response")
	}
	if len(paramApplier.calls) != 1 {
		t.Fatalf("expected one ApplyParams call, got %d", len(paramApplier.calls))
	}

	cfg := paramApplier.calls[0].cfg
	requireIntField(t, cfg.Jc, 11)
	requireIntField(t, cfg.Jmin, 7)
	requireIntField(t, cfg.Jmax, 9)
	requireIntField(t, cfg.S1, 19)
	requireStringField(t, cfg.H1, "101")
	requireStringField(t, cfg.I1, "alpha")
	requireStringField(t, cfg.I3, "gamma")

	if cfg.S2 != nil || cfg.H2 != nil || cfg.I2 != nil || cfg.I4 != nil || cfg.I5 != nil {
		t.Fatal("unexpected zero-value params mapped into config")
	}
}

func TestRotateParamsRejectsEmptyNewParams(t *testing.T) {
	t.Parallel()

	paramApplier := &testParamApplier{}
	handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), nil, paramApplier, nil, nil, nil, nil, nil)
	_, err := handler.RotateParams(context.Background(), &proto.RotateParamsRequest{
		TunnelName: "rotate-tunnel",
		NewParams:  &proto.AwgParams{},
	})
	if err == nil {
		t.Fatal("expected error for empty params")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected invalid argument, got %v", status.Code(err))
	}
	if len(paramApplier.calls) != 0 {
		t.Fatal("should not call ApplyParams when params are empty")
	}
}

func TestRotateParamsAppliesNewPublicKey(t *testing.T) {
	t.Parallel()

	paramApplier := &testParamApplier{}
	handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), nil, paramApplier, nil, nil, nil, nil, nil)

	newKey := make([]byte, 32)
	for i := range newKey {
		newKey[i] = byte(i + 0xA0)
	}

	resp, err := handler.RotateParams(context.Background(), &proto.RotateParamsRequest{
		TunnelName:   "rekey-tunnel",
		Tier:         3,
		NewParams:    &proto.AwgParams{Jc: 5},
		NewPublicKey: newKey,
	})
	if err != nil {
		t.Fatalf("RotateParams returned error: %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatal("expected successful rotate response")
	}
	// Two ApplyParams calls: one for params, one for new public key
	if len(paramApplier.calls) != 2 {
		t.Fatalf("expected 2 ApplyParams calls (params + key), got %d", len(paramApplier.calls))
	}
	// First call: AWG params
	if paramApplier.calls[0].cfg.Jc == nil || *paramApplier.calls[0].cfg.Jc != 5 {
		t.Fatalf("first call should set Jc=5")
	}
	// Second call: peer config with new public key
	if len(paramApplier.calls[1].cfg.Peers) != 1 {
		t.Fatalf("second call should have 1 peer, got %d", len(paramApplier.calls[1].cfg.Peers))
	}
	gotKey := paramApplier.calls[1].cfg.Peers[0].PublicKey
	var expectedKey wg.Key
	copy(expectedKey[:], newKey)
	if gotKey != expectedKey {
		t.Fatalf("peer public key mismatch")
	}
}

func TestRemoveTunnel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		manager     *testTunnelManager
		req         *proto.RemoveTunnelRequest
		wantCode    codes.Code
		wantSuccess bool
		wantCalls   []string
	}{
		{
			name:     "returns unimplemented when manager missing",
			manager:  nil,
			req:      &proto.RemoveTunnelRequest{Name: "tunnel-a"},
			wantCode: codes.Unimplemented,
		},
		{
			name:     "validates non-empty name",
			manager:  &testTunnelManager{},
			req:      &proto.RemoveTunnelRequest{Name: "   "},
			wantCode: codes.InvalidArgument,
		},
		{
			name:        "removes tunnel with trimmed name",
			manager:     &testTunnelManager{},
			req:         &proto.RemoveTunnelRequest{Name: "  tunnel-a  "},
			wantCode:    codes.OK,
			wantCalls:   []string{"tunnel-a"},
			wantSuccess: true,
		},
		{
			name:      "propagates manager errors",
			manager:   &testTunnelManager{removeErr: errors.New("boom")},
			req:       &proto.RemoveTunnelRequest{Name: "tunnel-a"},
			wantCode:  codes.Internal,
			wantCalls: []string{"tunnel-a"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var tunnelMgr TunnelManager
			if tt.manager != nil {
				tunnelMgr = tt.manager
			}
			handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), tunnelMgr, nil, nil, nil, nil, nil, nil)
			resp, err := handler.RemoveTunnel(context.Background(), tt.req)
			if tt.wantCode == codes.OK {
				if err != nil {
					t.Fatalf("RemoveTunnel returned error: %v", err)
				}
				if resp == nil || resp.GetSuccess() != tt.wantSuccess {
					t.Fatalf("unexpected success response: %#v", resp)
				}
			} else {
				assertCode(t, err, tt.wantCode)
			}

			if tt.manager != nil && !reflect.DeepEqual(tt.manager.removeCalls, tt.wantCalls) {
				t.Fatalf("unexpected RemoveTunnel calls: want %#v got %#v", tt.wantCalls, tt.manager.removeCalls)
			}
		})
	}
}

func TestListTunnels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		manager       *testTunnelManager
		wantCode      codes.Code
		wantTunnelLen int
	}{
		{
			name:     "returns unimplemented when manager missing",
			manager:  nil,
			wantCode: codes.Unimplemented,
		},
		{
			name: "maps tunnel fields",
			manager: &testTunnelManager{
				listTunnels: []TunnelInfo{
					{Name: "a", OverlayIP: "10.0.0.2/32", Healthy: true},
					{Name: "b", OverlayIP: "10.0.0.3/32", Healthy: false},
				},
			},
			wantCode:      codes.OK,
			wantTunnelLen: 2,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var tunnelMgr TunnelManager
			if tt.manager != nil {
				tunnelMgr = tt.manager
			}
			handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), tunnelMgr, nil, nil, nil, nil, nil, nil)
			resp, err := handler.ListTunnels(context.Background(), &proto.Empty{})
			if tt.wantCode != codes.OK {
				assertCode(t, err, tt.wantCode)
				return
			}
			if err != nil {
				t.Fatalf("ListTunnels returned error: %v", err)
			}
			if len(resp.GetTunnels()) != tt.wantTunnelLen {
				t.Fatalf("expected %d tunnels, got %d", tt.wantTunnelLen, len(resp.GetTunnels()))
			}
			if resp.GetTunnels()[0].GetName() != "a" || resp.GetTunnels()[0].GetOverlayIp() != "10.0.0.2/32" || !resp.GetTunnels()[0].GetHealthy() {
				t.Fatalf("unexpected first tunnel mapping: %#v", resp.GetTunnels()[0])
			}
			if resp.GetTunnels()[1].GetName() != "b" || resp.GetTunnels()[1].GetOverlayIp() != "10.0.0.3/32" || resp.GetTunnels()[1].GetHealthy() {
				t.Fatalf("unexpected second tunnel mapping: %#v", resp.GetTunnels()[1])
			}
		})
	}
}

func TestGetStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		provider   NodeStateProvider
		verifyFunc func(t *testing.T, resp *proto.NodeStatus)
	}{
		{
			name:     "returns unknown without provider",
			provider: nil,
			verifyFunc: func(t *testing.T, resp *proto.NodeStatus) {
				t.Helper()
				if resp.GetName() != "unknown" || resp.GetMode() != "unknown" {
					t.Fatalf("unexpected default status: %#v", resp)
				}
			},
		},
		{
			name: "maps provider state",
			provider: &testNodeStateProvider{
				state: NodeState{
					Name:      "node-a",
					Mode:      "master",
					OverlayIP: "10.0.0.1",
					StartTime: time.Now().Add(-2 * time.Minute),
					Tunnels: []TunnelInfo{
						{Name: "tunnel-a", OverlayIP: "10.0.0.2/32", Healthy: true},
					},
				},
			},
			verifyFunc: func(t *testing.T, resp *proto.NodeStatus) {
				t.Helper()
				if resp.GetName() != "node-a" || resp.GetMode() != "master" || resp.GetOverlayIp() != "10.0.0.1" {
					t.Fatalf("unexpected provider status mapping: %#v", resp)
				}
				if len(resp.GetTunnels()) != 1 {
					t.Fatalf("expected one tunnel, got %d", len(resp.GetTunnels()))
				}
				if resp.GetTunnels()[0].GetName() != "tunnel-a" || !resp.GetTunnels()[0].GetHealthy() {
					t.Fatalf("unexpected tunnel mapping: %#v", resp.GetTunnels()[0])
				}
				if resp.GetUptime() == "" {
					t.Fatal("expected non-empty uptime")
				}
				if _, err := time.ParseDuration(resp.GetUptime()); err != nil {
					t.Fatalf("expected parseable uptime, got %q (%v)", resp.GetUptime(), err)
				}
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), nil, nil, nil, nil, tt.provider, nil, nil)
			resp, err := handler.GetStatus(context.Background(), &proto.Empty{})
			if err != nil {
				t.Fatalf("GetStatus returned error: %v", err)
			}
			tt.verifyFunc(t, resp)
		})
	}
}

func TestGetHealth(t *testing.T) {
	t.Parallel()

	handler := NewAgentHandler(t.TempDir(), zerolog.Nop())
	resp, err := handler.GetHealth(context.Background(), &proto.Empty{})
	if err != nil {
		t.Fatalf("GetHealth returned error: %v", err)
	}
	if !resp.GetHealthy() {
		t.Fatalf("expected healthy response, got %#v", resp)
	}
}

func TestRotateTokenWritesFile(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	// Use a valid bcrypt hash (cost=12, matches RotateToken validation).
	validHash := "$2a$12$LJ3m4sFQmP.YBpOuQ0v8ru8Fx0g9FPHEMdKEaFVaMEMaYgK0Nb3k."

	handler := NewAgentHandler(configDir, zerolog.Nop())
	resp, err := handler.RotateToken(context.Background(), &proto.RotateTokenRequest{
		NewTokenHash: validHash,
	})
	if err != nil {
		t.Fatalf("RotateToken returned error: %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("expected success response, got %#v", resp)
	}

	assertFileContents(t, filepath.Join(configDir, "mesh.token"), validHash)
}

func TestRotateTokenRejectsInvalidHash(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		hash string
	}{
		{name: "empty", hash: ""},
		{name: "plain text", hash: "not-a-bcrypt-hash"},
		{name: "too long", hash: strings.Repeat("a", 101)},
		{name: "sha256 hex", hash: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewAgentHandler(t.TempDir(), zerolog.Nop())
			_, err := handler.RotateToken(context.Background(), &proto.RotateTokenRequest{
				NewTokenHash: tt.hash,
			})
			assertCode(t, err, codes.InvalidArgument)
		})
	}
}

func TestInitRejectsInvalidCerts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		caCert   []byte
		nodeCert []byte
		nodeKey  []byte
	}{
		{name: "empty ca_cert", caCert: nil, nodeCert: []byte("cert"), nodeKey: []byte("key")},
		{name: "empty node_cert", caCert: []byte("ca"), nodeCert: nil, nodeKey: []byte("key")},
		{name: "empty node_key", caCert: []byte("ca"), nodeCert: []byte("cert"), nodeKey: nil},
		{name: "non-PEM ca_cert", caCert: []byte("not-pem"), nodeCert: []byte("cert"), nodeKey: []byte("key")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewAgentHandler(t.TempDir(), zerolog.Nop())
			_, err := handler.Init(context.Background(), &proto.InitRequest{
				CaCert:   tt.caCert,
				NodeCert: tt.nodeCert,
				NodeKey:  tt.nodeKey,
			})
			assertCode(t, err, codes.InvalidArgument)
		})
	}
}

func TestAddTunnelRejectsInvalidNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		tunnelName string
	}{
		{name: "too long", tunnelName: "this-is-too-long"},
		{name: "path traversal", tunnelName: "../../evil"},
		{name: "spaces", tunnelName: "has space"},
		{name: "dots", tunnelName: "has.dot"},
		{name: "slash", tunnelName: "has/slash"},
	}

	tm := &testTunnelManager{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), tm, nil, nil, nil, nil, nil, nil)
			_, err := handler.AddTunnel(context.Background(), &proto.AddTunnelRequest{
				Name:         tt.tunnelName,
				EndpointHost: "host:51820",
			})
			assertCode(t, err, codes.InvalidArgument)
		})
	}
}

func TestCaptureRefresh(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		req                *proto.CaptureRequest
		captureFunc        CaptureFunc
		wantCode           codes.Code
		wantCapturedCount  int32
		wantSanitized      []string
		wantCountPerDomain int
	}{
		{
			name:              "returns success when capture is not injected",
			req:               &proto.CaptureRequest{Domains: []string{"example.com"}, CountPerDomain: 2},
			captureFunc:       nil,
			wantCode:          codes.OK,
			wantCapturedCount: 0,
		},
		{
			name: "calls capture function with sanitized arguments",
			req: &proto.CaptureRequest{
				Domains:        []string{"  example.com ", "", "api.example.com", "   "},
				CountPerDomain: 4,
			},
			wantCode:           codes.OK,
			wantCapturedCount:  7,
			wantSanitized:      []string{"example.com", "api.example.com"},
			wantCountPerDomain: 4,
		},
		{
			name:     "validates request",
			req:      nil,
			wantCode: codes.InvalidArgument,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var gotDomains []string
			gotCountPerDomain := -1
			gotTimeout := time.Duration(0)
			captureFunc := tt.captureFunc
			if captureFunc == nil && tt.wantSanitized != nil {
				captureFunc = func(interfaceName string, domains []string, countPerDomain int, timeout time.Duration) (int, error) {
					gotDomains = append([]string(nil), domains...)
					gotCountPerDomain = countPerDomain
					gotTimeout = timeout
					return int(tt.wantCapturedCount), nil
				}
			}

			handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), nil, nil, captureFunc, nil, nil, nil, nil)
			resp, err := handler.CaptureRefresh(context.Background(), tt.req)
			if tt.wantCode != codes.OK {
				assertCode(t, err, tt.wantCode)
				return
			}
			if err != nil {
				t.Fatalf("CaptureRefresh returned error: %v", err)
			}
			if !resp.GetSuccess() {
				t.Fatalf("expected success response, got %#v", resp)
			}
			if resp.GetCapturedCount() != tt.wantCapturedCount {
				t.Fatalf("expected captured count %d, got %d", tt.wantCapturedCount, resp.GetCapturedCount())
			}
			if tt.wantSanitized != nil {
				if !reflect.DeepEqual(gotDomains, tt.wantSanitized) {
					t.Fatalf("unexpected sanitized domains: want %#v got %#v", tt.wantSanitized, gotDomains)
				}
				if gotCountPerDomain != tt.wantCountPerDomain {
					t.Fatalf("unexpected count per domain: want %d got %d", tt.wantCountPerDomain, gotCountPerDomain)
				}
				if gotTimeout != 15*time.Second {
					t.Fatalf("unexpected timeout: want %s got %s", 15*time.Second, gotTimeout)
				}
			}
		})
	}
}

func TestListPeers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		peerMgr   *testPeerManager
		wantCode  codes.Code
		wantCount int
	}{
		{
			name:     "returns unimplemented when manager missing",
			peerMgr:  nil,
			wantCode: codes.Unimplemented,
		},
		{
			name: "maps peer fields",
			peerMgr: &testPeerManager{
				listPeers: []PeerInfo{
					{
						PublicKey:     []byte{1, 2, 3},
						Endpoint:      "endpoint-a",
						AllowedIPs:    []string{"10.0.0.0/24"},
						LastHandshake: 101,
						TxBytes:       1111,
						RxBytes:       2222,
					},
				},
			},
			wantCode:  codes.OK,
			wantCount: 1,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var peerMgr PeerManager
			if tt.peerMgr != nil {
				peerMgr = tt.peerMgr
			}
			handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), nil, nil, nil, peerMgr, nil, nil, nil)
			resp, err := handler.ListPeers(context.Background(), &proto.Empty{})
			if tt.wantCode != codes.OK {
				assertCode(t, err, tt.wantCode)
				return
			}
			if err != nil {
				t.Fatalf("ListPeers returned error: %v", err)
			}
			if len(resp.GetPeers()) != tt.wantCount {
				t.Fatalf("expected %d peers, got %d", tt.wantCount, len(resp.GetPeers()))
			}
			peer := resp.GetPeers()[0]
			if !reflect.DeepEqual(peer.GetPublicKey(), []byte{1, 2, 3}) {
				t.Fatalf("unexpected public key: %#v", peer.GetPublicKey())
			}
			if peer.GetEndpoint() != "endpoint-a" || peer.GetLastHandshake() != 101 || peer.GetTxBytes() != 1111 || peer.GetRxBytes() != 2222 {
				t.Fatalf("unexpected peer mapping: %#v", peer)
			}
			if !reflect.DeepEqual(peer.GetAllowedIps(), []string{"10.0.0.0/24"}) {
				t.Fatalf("unexpected allowed IPs mapping: %#v", peer.GetAllowedIps())
			}
		})
	}
}

func TestAddPeer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		peerMgr   *testPeerManager
		req       *proto.AddPeerRequest
		wantCode  codes.Code
		wantCalls int
	}{
		{
			name:     "returns unimplemented when manager missing",
			peerMgr:  nil,
			req:      &proto.AddPeerRequest{PublicKey: []byte{1}},
			wantCode: codes.Unimplemented,
		},
		{
			name:     "validates public key",
			peerMgr:  &testPeerManager{},
			req:      &proto.AddPeerRequest{PublicKey: nil},
			wantCode: codes.InvalidArgument,
		},
		{
			name:    "adds peer with trimmed endpoint host",
			peerMgr: &testPeerManager{},
			req: &proto.AddPeerRequest{
				PublicKey:           []byte{1, 2, 3},
				PresharedKey:        []byte{9, 9, 9},
				AllowedIps:          []string{"10.0.0.0/24"},
				EndpointHost:        "  peer.example  ",
				PersistentKeepalive: 25,
			},
			wantCode:  codes.OK,
			wantCalls: 1,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var peerMgr PeerManager
			if tt.peerMgr != nil {
				peerMgr = tt.peerMgr
			}
			handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), nil, nil, nil, peerMgr, nil, nil, nil)
			resp, err := handler.AddPeer(context.Background(), tt.req)
			if tt.wantCode != codes.OK {
				assertCode(t, err, tt.wantCode)
				return
			}
			if err != nil {
				t.Fatalf("AddPeer returned error: %v", err)
			}
			if !resp.GetSuccess() {
				t.Fatalf("expected success response, got %#v", resp)
			}
			if len(tt.peerMgr.addCalls) != tt.wantCalls {
				t.Fatalf("expected %d AddPeer calls, got %d", tt.wantCalls, len(tt.peerMgr.addCalls))
			}
			call := tt.peerMgr.addCalls[0]
			if call.endpointHost != "peer.example" {
				t.Fatalf("expected trimmed endpoint host, got %q", call.endpointHost)
			}
			if !reflect.DeepEqual(call.publicKey, []byte{1, 2, 3}) {
				t.Fatalf("unexpected public key: %#v", call.publicKey)
			}
			if !reflect.DeepEqual(call.presharedKey, []byte{9, 9, 9}) {
				t.Fatalf("unexpected preshared key: %#v", call.presharedKey)
			}
			if !reflect.DeepEqual(call.allowedIPs, []string{"10.0.0.0/24"}) || call.persistentKeepalive != 25 {
				t.Fatalf("unexpected add peer call: %#v", call)
			}
		})
	}
}

func TestAddTunnelAllowsEmptyEndpointHost(t *testing.T) {
	t.Parallel()

	tunnelMgr := &testTunnelManager{}
	handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), tunnelMgr, nil, nil, nil, nil, nil, nil)

	peerKey := make([]byte, 32)
	for idx := range peerKey {
		peerKey[idx] = byte(idx + 1)
	}

	resp, err := handler.AddTunnel(context.Background(), &proto.AddTunnelRequest{
		Name:                "client-a",
		EndpointHost:        "",
		OverlayIp:           "10.77.0.10",
		PeerPublicKey:       peerKey,
		TransportSubnet:     "10.250.0.0/30",
		MasterTransportIp:   "10.250.0.1",
		EndpointTransportIp: "10.250.0.2",
		Weight:              1,
	})
	if err != nil {
		t.Fatalf("AddTunnel returned error: %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("expected success response, got %#v", resp)
	}
	if len(tunnelMgr.addTunnelCalls) != 1 {
		t.Fatalf("expected one AddTunnel call, got %d", len(tunnelMgr.addTunnelCalls))
	}
	if tunnelMgr.addTunnelCalls[0].host != "" {
		t.Fatalf("expected empty endpoint host, got %q", tunnelMgr.addTunnelCalls[0].host)
	}
}

func TestAddPeerConfiguresTransportAfterStatePersisted(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	peerMgr := &testTransportPeerManager{configDir: configDir}
	handler := NewAgentHandlerFull(configDir, zerolog.Nop(), nil, nil, nil, peerMgr, nil, nil, nil)

	pubkey := []byte{1, 2, 3, 4}
	resp, err := handler.AddPeer(context.Background(), &proto.AddPeerRequest{
		PublicKey:        pubkey,
		AllowedIps:       []string{"0.0.0.0/0"},
		EndpointHost:     "master-a.example:51820",
		TransportSubnet:  "10.250.0.0/30",
		LocalTransportIp: "10.250.0.2",
		PeerTransportIp:  "10.250.0.1",
	})
	if err != nil {
		t.Fatalf("AddPeer returned error: %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("expected success response, got %#v", resp)
	}

	if len(peerMgr.configureCalls) != 1 {
		t.Fatalf("expected one transport configure call, got %d", len(peerMgr.configureCalls))
	}
	call := peerMgr.configureCalls[0]
	if call.pubkeyHex != "01020304" {
		t.Fatalf("unexpected pubkey hex: %q", call.pubkeyHex)
	}
	if call.localIP != "10.250.0.2" || call.peerIP != "10.250.0.1" {
		t.Fatalf("unexpected transport call: %#v", call)
	}
	if !peerMgr.stateSeen {
		t.Fatal("expected transport state to be persisted before ConfigureTransport")
	}
}

func TestAddPeerSkipsTransportConfiguratorWhenIPsMissing(t *testing.T) {
	t.Parallel()

	peerMgr := &testTransportPeerManager{}
	handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), nil, nil, nil, peerMgr, nil, nil, nil)

	resp, err := handler.AddPeer(context.Background(), &proto.AddPeerRequest{
		PublicKey:       []byte{9, 8, 7, 6},
		AllowedIps:      []string{"0.0.0.0/0"},
		TransportSubnet: "10.250.0.4/30",
	})
	if err != nil {
		t.Fatalf("AddPeer returned error: %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("expected success response, got %#v", resp)
	}
	if len(peerMgr.configureCalls) != 0 {
		t.Fatalf("expected no transport configure calls, got %d", len(peerMgr.configureCalls))
	}
}

func TestAddPeerStoresMasterNameFromEndpointMetadata(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	peerMgr := &testPeerManager{}
	handler := NewAgentHandlerFull(configDir, zerolog.Nop(), nil, nil, nil, peerMgr, nil, nil, nil)

	resp, err := handler.AddPeer(context.Background(), &proto.AddPeerRequest{
		PublicKey:        []byte{1, 2, 3, 4},
		AllowedIps:       []string{"0.0.0.0/0"},
		EndpointHost:     "master-a|master-a.example:51820",
		TransportSubnet:  "10.250.0.0/30",
		LocalTransportIp: "10.250.0.2",
		PeerTransportIp:  "10.250.0.1",
	})
	if err != nil {
		t.Fatalf("AddPeer returned error: %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("expected success response, got %#v", resp)
	}

	state, err := loadNodeTransportState(configDir)
	if err != nil {
		t.Fatalf("loadNodeTransportState returned error: %v", err)
	}
	if len(state.Tunnels) != 1 {
		t.Fatalf("expected one tunnel entry, got %d", len(state.Tunnels))
	}
	entry := state.Tunnels[0]
	if entry.Name != "master-a" {
		t.Fatalf("expected tunnel name master-a, got %q", entry.Name)
	}
	if entry.PeerEndpoint != "master-a.example:51820" {
		t.Fatalf("expected peer endpoint master-a.example:51820, got %q", entry.PeerEndpoint)
	}
}

func TestRemovePeer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		peerMgr   *testPeerManager
		req       *proto.RemovePeerRequest
		wantCode  codes.Code
		wantCalls int
	}{
		{
			name:     "returns unimplemented when manager missing",
			peerMgr:  nil,
			req:      &proto.RemovePeerRequest{PublicKey: []byte{1}},
			wantCode: codes.Unimplemented,
		},
		{
			name:     "validates public key",
			peerMgr:  &testPeerManager{},
			req:      &proto.RemovePeerRequest{},
			wantCode: codes.InvalidArgument,
		},
		{
			name:      "removes peer",
			peerMgr:   &testPeerManager{},
			req:       &proto.RemovePeerRequest{PublicKey: []byte{9, 8, 7}},
			wantCode:  codes.OK,
			wantCalls: 1,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var peerMgr PeerManager
			if tt.peerMgr != nil {
				peerMgr = tt.peerMgr
			}
			handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), nil, nil, nil, peerMgr, nil, nil, nil)
			resp, err := handler.RemovePeer(context.Background(), tt.req)
			if tt.wantCode != codes.OK {
				assertCode(t, err, tt.wantCode)
				return
			}
			if err != nil {
				t.Fatalf("RemovePeer returned error: %v", err)
			}
			if !resp.GetSuccess() {
				t.Fatalf("expected success response, got %#v", resp)
			}
			if len(tt.peerMgr.removeCalls) != tt.wantCalls {
				t.Fatalf("expected %d RemovePeer calls, got %d", tt.wantCalls, len(tt.peerMgr.removeCalls))
			}
			if !reflect.DeepEqual(tt.peerMgr.removeCalls[0], []byte{9, 8, 7}) {
				t.Fatalf("unexpected RemovePeer key: %#v", tt.peerMgr.removeCalls[0])
			}
		})
	}
}

func TestGetParams(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		manager        *testTunnelManager
		req            *proto.GetParamsRequest
		wantCode       codes.Code
		wantTunnelName string
		verifyParams   func(t *testing.T, params *proto.AwgParams)
	}{
		{
			name:     "returns unimplemented when manager missing",
			manager:  nil,
			req:      &proto.GetParamsRequest{TunnelName: "tunnel-a"},
			wantCode: codes.Unimplemented,
		},
		{
			name:     "validates tunnel name",
			manager:  &testTunnelManager{},
			req:      &proto.GetParamsRequest{TunnelName: " "},
			wantCode: codes.InvalidArgument,
		},
		{
			name: "maps config into response params",
			manager: &testTunnelManager{
				getParamsCfg: wg.Config{
					Jc:   wg.IntPtr(11),
					Jmin: wg.IntPtr(7),
					H1:   wg.StrPtr("101"),
					H2:   wg.StrPtr("invalid"),
					I1:   wg.StrPtr("alpha"),
				},
			},
			req:            &proto.GetParamsRequest{TunnelName: "  tunnel-a  "},
			wantCode:       codes.OK,
			wantTunnelName: "tunnel-a",
			verifyParams: func(t *testing.T, params *proto.AwgParams) {
				t.Helper()
				if params.GetJc() != 11 || params.GetJmin() != 7 {
					t.Fatalf("unexpected integer params: %#v", params)
				}
				if params.GetH1() != 101 || params.GetH2() != 0 {
					t.Fatalf("unexpected H params mapping: %#v", params)
				}
				if params.GetI1() != "alpha" {
					t.Fatalf("unexpected string params mapping: %#v", params)
				}
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var tunnelMgr TunnelManager
			if tt.manager != nil {
				tunnelMgr = tt.manager
			}
			handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), tunnelMgr, nil, nil, nil, nil, nil, nil)
			resp, err := handler.GetParams(context.Background(), tt.req)
			if tt.wantCode != codes.OK {
				assertCode(t, err, tt.wantCode)
				return
			}
			if err != nil {
				t.Fatalf("GetParams returned error: %v", err)
			}
			if len(tt.manager.getParamsCalls) != 1 || tt.manager.getParamsCalls[0] != tt.wantTunnelName {
				t.Fatalf("unexpected GetParams calls: %#v", tt.manager.getParamsCalls)
			}
			tt.verifyParams(t, resp)
		})
	}
}

func TestGetRoutesNonLinux(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "linux" {
		t.Skip("this test validates non-Linux behavior")
	}

	handler := NewAgentHandler(t.TempDir(), zerolog.Nop())
	_, err := handler.GetRoutes(context.Background(), &proto.Empty{})
	assertCode(t, err, codes.Unimplemented)
}

// generateTestCerts creates a CA + signed node certificate for Init tests.
func generateTestCerts(t *testing.T) (caCertPEM, nodeCertPEM, nodeKeyPEM []byte) {
	t.Helper()
	caCert, caKey, err := pkgtls.GenerateCA("test-ca")
	if err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	caCertPEM = pkgtls.EncodeCertPEM(caCert)
	nodeCertPEM, nodeKeyPEM, err = pkgtls.IssueCert(caCert, caKey, "test-node", nil)
	if err != nil {
		t.Fatalf("issue cert: %v", err)
	}
	return caCertPEM, nodeCertPEM, nodeKeyPEM
}

func assertFileContents(t *testing.T, path string, expected string) {
	t.Helper()

	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read %s: %v", path, err)
	}
	if got := string(bytes); got != expected {
		t.Fatalf("unexpected file content for %s, want %q got %q", path, expected, got)
	}
}

func requireIntField(t *testing.T, actual *int, expected int) {
	t.Helper()
	if actual == nil || *actual != expected {
		t.Fatalf("expected int pointer %d, got %#v", expected, actual)
	}
}

func requireStringField(t *testing.T, actual *string, expected string) {
	t.Helper()
	if actual == nil || *actual != expected {
		t.Fatalf("expected string pointer %q, got %#v", expected, actual)
	}
}

func assertCode(t *testing.T, err error, expected codes.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected grpc error code %v, got nil", expected)
	}
	if code := status.Code(err); code != expected {
		t.Fatalf("expected grpc error code %v, got %v (err=%v)", expected, code, err)
	}
}
