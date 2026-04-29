package grpcserver

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/pkg/tls"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
	proto "github.com/coonfuuseed-paandaa/awg-mesh/proto"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type testTunnelManager struct {
	addTunnelCalls      []addTunnelCall
	listTunnels         []TunnelInfo
	removeCalls         []string
	getParamsCalls      []string
	getParamsCfg        wg.Config
	updatePeerCalls     []updateTunnelPeerCall
	addErr              error
	removeErr           error
	getParamsErr        error
	updatePeerErr       error
	updatePeerUnchanged bool
	listenPort          int
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
	allowedIPs          []string
}

type updateTunnelPeerCall struct {
	name       string
	newPubkey  [32]byte
	balancerIP string
	allowedIPs []string
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
	allowedIPs []string,
) error {
	// Defensive copy so assertions are not sensitive to post-call mutations
	// of the caller's slice (CodeRabbit nitpick PR #79).
	allowedIPsCopy := append([]string(nil), allowedIPs...)
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
		allowedIPs:          allowedIPsCopy,
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

func (m *testTunnelManager) UpdateTunnelPeer(name string, newPubkey [32]byte, balancerIP string, allowedIPs []string) (unchanged bool, err error) {
	m.updatePeerCalls = append(m.updatePeerCalls, updateTunnelPeerCall{
		name:       name,
		newPubkey:  newPubkey,
		balancerIP: balancerIP,
		allowedIPs: append([]string(nil), allowedIPs...),
	})
	return m.updatePeerUnchanged, m.updatePeerErr
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
	listPeers     []PeerInfo
	addCalls      []addPeerCall
	removeCalls   [][]byte
	addErr        error
	removeErr     error
	listenPort    int
	listenPortErr error
}

type testTransportPeerManager struct {
	testPeerManager
	configDir      string
	configureCalls []configureTransportCall
	configureErr   error
	stateSeen      bool
}

type configureTransportCall struct {
	pubkeyHex  string
	localIP    string
	peerIP     string
	allowedIPs []string
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

func (m *testPeerManager) AddPeer(publicKey []byte, presharedKey []byte, allowedIPs []string, endpointHost string, persistentKeepalive int32, peerName string) error {
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

func (m *testPeerManager) GetListenPort(_ string) (int, error) {
	return m.listenPort, m.listenPortErr
}

func (m *testTransportPeerManager) ConfigureTransport(pubkeyHex, localIP, peerIP string, allowedIPs []string, peerName string, extraRoutes []string) error {
	m.configureCalls = append(m.configureCalls, configureTransportCall{
		pubkeyHex:  pubkeyHex,
		localIP:    localIP,
		peerIP:     peerIP,
		allowedIPs: append([]string(nil), allowedIPs...),
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
		handler := NewAgentHandlerFull(configDir, logger, tunnelMgr, paramApplier, nil, nil, nil, nil, nil, nil)

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
	handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), nil, nil, nil, nil, nil, nil, kp, nil)
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
	handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), mgr, nil, nil, nil, nil, nil, kp, nil)

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

func TestAgentHandler_UpdateTunnelPeer_Validation(t *testing.T) {
	logger := zerolog.Nop()
	h := &AgentHandler{
		logger:    logger,
		tunnelMgr: &testTunnelManager{},
	}

	t.Run("empty name returns InvalidArgument", func(t *testing.T) {
		_, err := h.UpdateTunnelPeer(context.Background(), &proto.UpdateTunnelPeerRequest{
			Name:          "",
			PeerPublicKey: make([]byte, 32),
		})
		if err == nil {
			t.Fatal("expected error")
		}
		st, _ := status.FromError(err)
		if st.Code() != codes.InvalidArgument {
			t.Errorf("expected InvalidArgument, got %v", st.Code())
		}
	})

	t.Run("wrong pubkey length returns InvalidArgument", func(t *testing.T) {
		_, err := h.UpdateTunnelPeer(context.Background(), &proto.UpdateTunnelPeerRequest{
			Name:          "tun1",
			PeerPublicKey: make([]byte, 31),
		})
		if err == nil {
			t.Fatal("expected error")
		}
		st, _ := status.FromError(err)
		if st.Code() != codes.InvalidArgument {
			t.Errorf("expected InvalidArgument, got %v", st.Code())
		}
	})

	t.Run("invalid allowed_ips returns InvalidArgument", func(t *testing.T) {
		_, err := h.UpdateTunnelPeer(context.Background(), &proto.UpdateTunnelPeerRequest{
			Name:          "tun1",
			PeerPublicKey: make([]byte, 32),
			AllowedIps:    []string{"not-a-cidr"},
		})
		if err == nil {
			t.Fatal("expected error")
		}
		st, _ := status.FromError(err)
		if st.Code() != codes.InvalidArgument {
			t.Errorf("expected InvalidArgument, got %v", st.Code())
		}
	})

	t.Run("nil tunnelMgr returns Unimplemented", func(t *testing.T) {
		h2 := &AgentHandler{logger: logger}
		_, err := h2.UpdateTunnelPeer(context.Background(), &proto.UpdateTunnelPeerRequest{
			Name:          "tun1",
			PeerPublicKey: make([]byte, 32),
		})
		if err == nil {
			t.Fatal("expected error")
		}
		st, _ := status.FromError(err)
		if st.Code() != codes.Unimplemented {
			t.Errorf("expected Unimplemented, got %v", st.Code())
		}
	})

	t.Run("tunnel not found returns NotFound", func(t *testing.T) {
		mgr := &testTunnelManager{updatePeerErr: errors.New("tunnel not found")}
		h3 := &AgentHandler{logger: logger, tunnelMgr: mgr}
		_, err := h3.UpdateTunnelPeer(context.Background(), &proto.UpdateTunnelPeerRequest{
			Name:          "tun1",
			PeerPublicKey: make([]byte, 32),
		})
		if err == nil {
			t.Fatal("expected error")
		}
		st, _ := status.FromError(err)
		if st.Code() != codes.NotFound {
			t.Errorf("expected NotFound, got %v", st.Code())
		}
	})

	t.Run("unchanged=true returns success with Unchanged", func(t *testing.T) {
		mgr := &testTunnelManager{updatePeerUnchanged: true}
		h4 := &AgentHandler{logger: logger, tunnelMgr: mgr}
		resp, err := h4.UpdateTunnelPeer(context.Background(), &proto.UpdateTunnelPeerRequest{
			Name:          "tun1",
			PeerPublicKey: make([]byte, 32),
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !resp.Unchanged {
			t.Error("expected Unchanged=true")
		}
	})
}

func TestRotateParamsMapsProtoValuesToConfig(t *testing.T) {
	t.Parallel()

	paramApplier := &testParamApplier{}
	handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), nil, paramApplier, nil, nil, nil, nil, nil, nil)
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
	handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), nil, paramApplier, nil, nil, nil, nil, nil, nil)
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
	handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), nil, paramApplier, nil, nil, nil, nil, nil, nil)

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
			handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), tunnelMgr, nil, nil, nil, nil, nil, nil, nil)
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
			handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), tunnelMgr, nil, nil, nil, nil, nil, nil, nil)
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

			handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), nil, nil, nil, nil, tt.provider, nil, nil, nil)
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
			handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), tm, nil, nil, nil, nil, nil, nil, nil)
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

			handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), nil, nil, captureFunc, nil, nil, nil, nil, nil)
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
			handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), nil, nil, nil, peerMgr, nil, nil, nil, nil)
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
			handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), nil, nil, nil, peerMgr, nil, nil, nil, nil)
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

// TestAddPeerReturnsListenPort verifies that the AddPeer handler populates
// EndpointListenPort from the peer manager's GetListenPort result.
func TestAddPeerReturnsListenPort(t *testing.T) {
	t.Parallel()

	t.Run("port returned from peer manager", func(t *testing.T) {
		t.Parallel()

		peerMgr := &testPeerManager{listenPort: 51821}
		handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), nil, nil, nil, peerMgr, nil, nil, nil, nil)
		resp, err := handler.AddPeer(context.Background(), &proto.AddPeerRequest{
			PublicKey: []byte{1, 2, 3},
			PeerName:  "master-a",
		})
		if err != nil {
			t.Fatalf("AddPeer returned error: %v", err)
		}
		if resp.GetEndpointListenPort() != 51821 {
			t.Errorf("EndpointListenPort = %d, want 51821", resp.GetEndpointListenPort())
		}
	})

	t.Run("fallback to 0 when GetListenPort errors", func(t *testing.T) {
		t.Parallel()

		peerMgr := &testPeerManager{listenPortErr: fmt.Errorf("iface not ready")}
		handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), nil, nil, nil, peerMgr, nil, nil, nil, nil)
		resp, err := handler.AddPeer(context.Background(), &proto.AddPeerRequest{
			PublicKey: []byte{1, 2, 3},
			PeerName:  "master-b",
		})
		if err != nil {
			t.Fatalf("AddPeer must not fail on port retrieval error, got: %v", err)
		}
		if resp.GetEndpointListenPort() != 0 {
			t.Errorf("EndpointListenPort = %d, want 0 (fallback)", resp.GetEndpointListenPort())
		}
	})

	t.Run("zero port when peerName empty", func(t *testing.T) {
		t.Parallel()

		peerMgr := &testPeerManager{listenPort: 51822}
		handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), nil, nil, nil, peerMgr, nil, nil, nil, nil)
		resp, err := handler.AddPeer(context.Background(), &proto.AddPeerRequest{
			PublicKey: []byte{1, 2, 3},
			PeerName:  "",
		})
		if err != nil {
			t.Fatalf("AddPeer returned error: %v", err)
		}
		// No peerName → GetListenPort is not called → 0 returned.
		if resp.GetEndpointListenPort() != 0 {
			t.Errorf("EndpointListenPort = %d, want 0 when peerName is empty", resp.GetEndpointListenPort())
		}
	})
}

func TestAddTunnelAllowsEmptyEndpointHost(t *testing.T) {
	t.Parallel()

	tunnelMgr := &testTunnelManager{}
	handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), tunnelMgr, nil, nil, nil, nil, nil, nil, nil)

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

// TestAddTunnelPassesThroughAllowedIPs verifies that AddTunnel forwards
// req.AllowedIps verbatim to the TunnelManager. This pins the admin-source-of-truth
// contract introduced in v1.12.8 (issue #147 layer 3): the handler must not
// silently drop or recompute the AllowedIPs field.
func TestAddTunnelPassesThroughAllowedIPs(t *testing.T) {
	t.Parallel()

	tunnelMgr := &testTunnelManager{}
	handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), tunnelMgr, nil, nil, nil, nil, nil, nil, nil)

	peerKey := make([]byte, 32)
	for idx := range peerKey {
		peerKey[idx] = byte(idx + 1)
	}
	wantAllowedIPs := []string{"10.255.0.24/30", "172.20.70.34/32", "172.20.70.32/27"}

	resp, err := handler.AddTunnel(context.Background(), &proto.AddTunnelRequest{
		Name:          "ep-pl-01",
		PeerPublicKey: peerKey,
		Weight:        1,
		AllowedIps:    wantAllowedIPs,
	})
	if err != nil {
		t.Fatalf("AddTunnel returned error: %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("expected success, got %#v", resp)
	}
	if len(tunnelMgr.addTunnelCalls) != 1 {
		t.Fatalf("expected one AddTunnel call, got %d", len(tunnelMgr.addTunnelCalls))
	}
	got := tunnelMgr.addTunnelCalls[0].allowedIPs
	if len(got) != len(wantAllowedIPs) {
		t.Fatalf("allowedIPs length = %d, want %d; got %v", len(got), len(wantAllowedIPs), got)
	}
	for i, cidr := range wantAllowedIPs {
		if got[i] != cidr {
			t.Errorf("allowedIPs[%d] = %q, want %q", i, got[i], cidr)
		}
	}
}

func TestAddPeerConfiguresTransportAfterStatePersisted(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	peerMgr := &testTransportPeerManager{configDir: configDir}
	handler := NewAgentHandlerFull(configDir, zerolog.Nop(), nil, nil, nil, peerMgr, nil, nil, nil, nil)

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
	if !reflect.DeepEqual(call.allowedIPs, []string{"0.0.0.0/0"}) {
		t.Fatalf("unexpected transport allowed IPs: %#v", call.allowedIPs)
	}
	if !peerMgr.stateSeen {
		t.Fatal("expected transport state to be persisted before ConfigureTransport")
	}
}

func TestAddPeerSkipsTransportConfiguratorWhenIPsMissing(t *testing.T) {
	t.Parallel()

	peerMgr := &testTransportPeerManager{}
	handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), nil, nil, nil, peerMgr, nil, nil, nil, nil)

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
	handler := NewAgentHandlerFull(configDir, zerolog.Nop(), nil, nil, nil, peerMgr, nil, nil, nil, nil)

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
			handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), nil, nil, nil, peerMgr, nil, nil, nil, nil)
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
			handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), tunnelMgr, nil, nil, nil, nil, nil, nil, nil)
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

// TestAgentHandler_UpdateTunnelPeer_Unauthenticated verifies that the token-auth
// interceptor (makeUnaryAuthInterceptor) rejects UpdateTunnelPeer calls that
// carry an empty or wrong bearer token with codes.Unauthenticated, and allows
// calls carrying the correct token to pass auth (the call may then fail with a
// different code due to missing tunnel — that is acceptable per T016 AC).
//
// Design: spins up an in-process gRPC server (no TLS transport, just the auth
// interceptor) and connects with grpc.WithTransportCredentials(insecure).
// This exercises the real makeUnaryAuthInterceptor code path without needing
// TLS certificate fixtures.
func TestAgentHandler_UpdateTunnelPeer_Unauthenticated(t *testing.T) {
	t.Parallel()

	// Generate a real token and its bcrypt hash so the interceptor has a
	// verifiable credential on disk. Use a temp dir as the token hash dir.
	token, err := pkgtls.GenerateToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	hash, err := pkgtls.HashToken(token)
	if err != nil {
		t.Fatalf("hash token: %v", err)
	}
	tokenDir := t.TempDir()
	if err := pkgtls.SaveTokenHash(tokenDir, hash); err != nil {
		t.Fatalf("save token hash: %v", err)
	}

	// Build a handler backed by a testTunnelManager so valid-auth calls reach
	// the RPC body (tunnel will not be found — NotFound, not Unauthenticated).
	mgr := &testTunnelManager{updatePeerErr: errors.New("tunnel not found")}
	handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), mgr, nil, nil, nil, nil, nil, nil, nil)

	// Spin up in-process gRPC server with only the token-auth interceptor.
	// No TLS transport: the interceptor's mTLS branch is skipped (no
	// credentials.TLSInfo in peer context) so it falls through to bearer-token.
	gs := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			makeUnaryAuthInterceptor(newTokenHashProvider(tokenDir), zerolog.Nop()),
		),
	)
	proto.RegisterAwgAgentServer(gs, handler)

	ln, listenErr := net.Listen("tcp", "127.0.0.1:0")
	if listenErr != nil {
		t.Fatalf("listen: %v", listenErr)
	}
	go func() { _ = gs.Serve(ln) }()
	t.Cleanup(func() { gs.GracefulStop() })

	conn, dialErr := grpc.NewClient(
		ln.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if dialErr != nil {
		t.Fatalf("dial: %v", dialErr)
	}
	t.Cleanup(func() { _ = conn.Close() })

	client := proto.NewAwgAgentClient(conn)

	validKey := make([]byte, 32)
	for i := range validKey {
		validKey[i] = byte(i + 1)
	}
	req := &proto.UpdateTunnelPeerRequest{
		Name:          "tun1",
		PeerPublicKey: validKey,
	}

	t.Run("no token returns Unauthenticated", func(t *testing.T) {
		t.Parallel()

		_, callErr := client.UpdateTunnelPeer(context.Background(), req)
		if callErr == nil {
			t.Fatal("expected error, got nil")
		}
		if code := status.Code(callErr); code != codes.Unauthenticated {
			t.Fatalf("expected Unauthenticated, got %v (err=%v)", code, callErr)
		}
	})

	t.Run("wrong token returns Unauthenticated", func(t *testing.T) {
		t.Parallel()

		ctx := metadata.AppendToOutgoingContext(
			context.Background(),
			"authorization", "Bearer wrong-token-value",
		)
		_, callErr := client.UpdateTunnelPeer(ctx, req)
		if callErr == nil {
			t.Fatal("expected error, got nil")
		}
		if code := status.Code(callErr); code != codes.Unauthenticated {
			t.Fatalf("expected Unauthenticated, got %v (err=%v)", code, callErr)
		}
	})

	t.Run("valid token passes auth (may fail with NotFound for missing tunnel)", func(t *testing.T) {
		t.Parallel()

		ctx := metadata.AppendToOutgoingContext(
			context.Background(),
			"authorization", "Bearer "+token,
		)
		_, callErr := client.UpdateTunnelPeer(ctx, req)
		// Auth passed — the RPC reached the handler and returned NotFound for the
		// missing tunnel. Any code other than Unauthenticated confirms auth was accepted.
		if callErr != nil && status.Code(callErr) == codes.Unauthenticated {
			t.Fatalf("valid token was rejected with Unauthenticated — auth interceptor is broken")
		}
	})
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

// writeDiskTransportState is a test helper that writes transport.yml to a temp dir.
func writeDiskTransportState(t *testing.T, configDir string, state nodeTransportState) {
	t.Helper()
	path := filepath.Join(configDir, "transport.yml")
	if err := saveNodeTransportState(path, state); err != nil {
		t.Fatalf("writeDiskTransportState: %v", err)
	}
}

func TestGetTransportState(t *testing.T) {
	t.Parallel()

	pubKey1 := make([]byte, 32)
	pubKey1[0] = 0xAB
	pubKey1[31] = 0xCD
	pubKey1Hex := "ab" + strings.Repeat("00", 30) + "cd"

	pubKey2 := make([]byte, 32)
	pubKey2[0] = 0x12
	pubKey2[31] = 0x34
	pubKey2Hex := "12" + strings.Repeat("00", 30) + "34"

	tests := []struct {
		name         string
		setupHandler func(t *testing.T, configDir string) *AgentHandler
		wantMode     string
		wantOverlay  string
		wantPeerLen  int
		verifyPeers  func(t *testing.T, peers []*proto.TransportPeerState)
	}{
		{
			name: "master mode: returns tunnels from TunnelManager enriched with disk AllowedIPs",
			setupHandler: func(t *testing.T, configDir string) *AgentHandler {
				t.Helper()
				writeDiskTransportState(t, configDir, nodeTransportState{
					OverlayIP: "10.0.0.1/32",
					Tunnels: []tunnelTransport{
						{
							Name:          "us-01",
							PeerPublicKey: pubKey1Hex,
							AllowedIPs:    []string{"10.0.1.0/24", "192.168.1.0/24"},
						},
					},
				})
				mgr := &testTunnelManager{
					listTunnels: []TunnelInfo{
						{Name: "us-01", OverlayIP: "10.0.1.1/32", Healthy: true, PeerPublicKey: pubKey1},
					},
				}
				sp := &testNodeStateProvider{state: NodeState{
					Name: "master-1", Mode: "master", OverlayIP: "10.0.0.1/32",
				}}
				return NewAgentHandlerFull(configDir, zerolog.Nop(), mgr, nil, nil, nil, sp, nil, nil, nil)
			},
			wantMode:    "master",
			wantOverlay: "10.0.0.1/32",
			wantPeerLen: 1,
			verifyPeers: func(t *testing.T, peers []*proto.TransportPeerState) {
				t.Helper()
				p := peers[0]
				if p.Name != "us-01" {
					t.Errorf("peer name: want us-01, got %q", p.Name)
				}
				if p.PublicKeyHex != pubKey1Hex {
					t.Errorf("public_key_hex: want %q, got %q", pubKey1Hex, p.PublicKeyHex)
				}
				wantIPs := []string{"10.0.1.0/24", "192.168.1.0/24"}
				if !reflect.DeepEqual(p.AllowedIps, wantIPs) {
					t.Errorf("allowed_ips: want %v, got %v", wantIPs, p.AllowedIps)
				}
				if p.LastHandshakeUnix != 0 {
					t.Errorf("last_handshake_unix: want 0 for master mode, got %d", p.LastHandshakeUnix)
				}
			},
		},
		{
			name: "endpoint mode: returns peers from PeerManager with name from disk",
			setupHandler: func(t *testing.T, configDir string) *AgentHandler {
				t.Helper()
				const ts = int64(1700000000)
				writeDiskTransportState(t, configDir, nodeTransportState{
					OverlayIP: "10.0.0.2/32",
					Tunnels: []tunnelTransport{
						{Name: "master-1", PeerPublicKey: pubKey2Hex, AllowedIPs: []string{"10.0.0.0/8"}},
					},
				})
				pm := &testPeerManager{
					listPeers: []PeerInfo{
						{PublicKey: pubKey2, AllowedIPs: []string{"10.0.0.0/8"}, LastHandshake: ts},
					},
				}
				sp := &testNodeStateProvider{state: NodeState{
					Name: "ep-1", Mode: "endpoint", OverlayIP: "10.0.0.2/32",
				}}
				return NewAgentHandlerFull(configDir, zerolog.Nop(), nil, nil, nil, pm, sp, nil, nil, nil)
			},
			wantMode:    "endpoint",
			wantOverlay: "10.0.0.2/32",
			wantPeerLen: 1,
			verifyPeers: func(t *testing.T, peers []*proto.TransportPeerState) {
				t.Helper()
				p := peers[0]
				if p.Name != "master-1" {
					t.Errorf("peer name: want master-1 (from disk), got %q", p.Name)
				}
				if p.PublicKeyHex != pubKey2Hex {
					t.Errorf("public_key_hex: want %q, got %q", pubKey2Hex, p.PublicKeyHex)
				}
				const wantTS = int64(1700000000)
				if p.LastHandshakeUnix != wantTS {
					t.Errorf("last_handshake_unix: want %d, got %d", wantTS, p.LastHandshakeUnix)
				}
			},
		},
		{
			name: "no managers: falls back to disk state only",
			setupHandler: func(t *testing.T, configDir string) *AgentHandler {
				t.Helper()
				writeDiskTransportState(t, configDir, nodeTransportState{
					OverlayIP: "10.0.0.3/32",
					Tunnels: []tunnelTransport{
						{Name: "fallback-peer", PeerPublicKey: pubKey1Hex, AllowedIPs: []string{"0.0.0.0/0"}},
					},
				})
				return NewAgentHandlerFull(configDir, zerolog.Nop(), nil, nil, nil, nil, nil, nil, nil, nil)
			},
			wantMode:    "",
			wantOverlay: "10.0.0.3/32",
			wantPeerLen: 1,
			verifyPeers: func(t *testing.T, peers []*proto.TransportPeerState) {
				t.Helper()
				p := peers[0]
				if p.Name != "fallback-peer" {
					t.Errorf("peer name: want fallback-peer, got %q", p.Name)
				}
				if p.PublicKeyHex != pubKey1Hex {
					t.Errorf("public_key_hex: want %q, got %q", pubKey1Hex, p.PublicKeyHex)
				}
				if !reflect.DeepEqual(p.AllowedIps, []string{"0.0.0.0/0"}) {
					t.Errorf("allowed_ips: want [0.0.0.0/0], got %v", p.AllowedIps)
				}
			},
		},
		{
			name: "no disk state and no managers: returns empty peer list",
			setupHandler: func(t *testing.T, configDir string) *AgentHandler {
				t.Helper()
				// No transport.yml written; no managers injected.
				return NewAgentHandlerFull(configDir, zerolog.Nop(), nil, nil, nil, nil, nil, nil, nil, nil)
			},
			wantMode:    "",
			wantOverlay: "",
			wantPeerLen: 0,
			verifyPeers: func(t *testing.T, peers []*proto.TransportPeerState) {},
		},
		{
			name: "endpoint mode: peer without disk name uses pubkey prefix fallback",
			setupHandler: func(t *testing.T, configDir string) *AgentHandler {
				t.Helper()
				// No disk state — peer name must fall back to first 8 hex chars.
				pm := &testPeerManager{
					listPeers: []PeerInfo{
						{PublicKey: pubKey1, AllowedIPs: []string{"10.0.0.0/8"}, LastHandshake: 0},
					},
				}
				return NewAgentHandlerFull(configDir, zerolog.Nop(), nil, nil, nil, pm, nil, nil, nil, nil)
			},
			wantMode:    "",
			wantOverlay: "",
			wantPeerLen: 1,
			verifyPeers: func(t *testing.T, peers []*proto.TransportPeerState) {
				t.Helper()
				// Fallback: first 8 hex chars of pubKey1Hex = "ab000000"
				wantPrefix := pubKey1Hex[:8]
				if peers[0].Name != wantPrefix {
					t.Errorf("fallback peer name: want %q, got %q", wantPrefix, peers[0].Name)
				}
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			configDir := t.TempDir()
			handler := tt.setupHandler(t, configDir)

			resp, err := handler.GetTransportState(context.Background(), &proto.Empty{})
			if err != nil {
				t.Fatalf("GetTransportState returned unexpected error: %v", err)
			}
			if resp == nil {
				t.Fatal("GetTransportState returned nil response")
			}
			if resp.Mode != tt.wantMode {
				t.Errorf("mode: want %q, got %q", tt.wantMode, resp.Mode)
			}
			if resp.OverlayIp != tt.wantOverlay {
				t.Errorf("overlay_ip: want %q, got %q", tt.wantOverlay, resp.OverlayIp)
			}
			if len(resp.Peers) != tt.wantPeerLen {
				t.Fatalf("peers len: want %d, got %d (peers=%v)", tt.wantPeerLen, len(resp.Peers), resp.Peers)
			}
			tt.verifyPeers(t, resp.Peers)

			// Anti-stub check: if GetTransportState returned a nil peers slice from a
			// non-empty source, the body is likely stubbed.
			if tt.wantPeerLen > 0 && resp.Peers == nil {
				t.Error("anti-stub: wantPeerLen > 0 but resp.Peers is nil — body may be a stub")
			}
		})
	}
}

// TestUpdateTunnelPeer_Idempotent_AlreadyApplied verifies NFR-3 crash-safety
// idempotency: when the tunnel manager's same-key check fires (newKey == current
// in-memory key after a previous successful call or post-crash recovery),
// UpdateTunnelPeer returns success with unchanged=true and no error.
func TestUpdateTunnelPeer_Idempotent_AlreadyApplied(t *testing.T) {
	t.Parallel()

	key, err := wg.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pubKey := key.PublicKey()

	// Stub manager that reports unchanged=true (same-key path).
	mgr := &testTunnelManager{updatePeerUnchanged: true}
	h := &AgentHandler{
		logger:    zerolog.Nop(),
		tunnelMgr: mgr,
	}

	resp, rpcErr := h.UpdateTunnelPeer(context.Background(), &proto.UpdateTunnelPeerRequest{
		Name:          "ep-01",
		PeerPublicKey: pubKey[:],
	})
	if rpcErr != nil {
		t.Fatalf("UpdateTunnelPeer (idempotent): unexpected error: %v", rpcErr)
	}
	if resp == nil {
		t.Fatal("unexpected nil response")
	}
	if !resp.GetUnchanged() {
		t.Error("expected Unchanged=true when same key re-applied, got false")
	}
	if !resp.GetSuccess() {
		t.Error("expected Success=true on idempotent update")
	}
}

// --- T004: RotateKeypair handler unit tests (9 cases) ---

// testNodeStatePersister is a test-double for NodeStatePersister.
// It records calls and allows fault injection per test case.
type testNodeStatePersister struct {
	mu sync.Mutex // guards fields below

	loadCalls  []string // tunnelNames passed to LoadKeypair
	loadResult []byte
	loadErr    error

	persistCalls []persistKeypairCall
	persistErr   error

	lockCalls []string // tunnelNames passed to LockRotation
	lockErr   error    // if non-nil, LockRotation returns this error

	// realMu is used by TestRotateKeypair_ConcurrentLock to exercise real serialization.
	realMu    sync.Mutex
	useRealMu bool
}

type persistKeypairCall struct {
	tunnelName string
	privateKey []byte
}

func (m *testNodeStatePersister) LoadKeypair(tunnelName string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadCalls = append(m.loadCalls, tunnelName)
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	if m.loadResult == nil {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), m.loadResult...), nil
}

func (m *testNodeStatePersister) PersistKeypair(tunnelName string, privateKey []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.persistCalls = append(m.persistCalls, persistKeypairCall{
		tunnelName: tunnelName,
		privateKey: append([]byte(nil), privateKey...),
	})
	return m.persistErr
}

func (m *testNodeStatePersister) LockRotation(tunnelName string) (func(), error) {
	m.mu.Lock()
	m.lockCalls = append(m.lockCalls, tunnelName)
	lockErr := m.lockErr
	useReal := m.useRealMu
	m.mu.Unlock()

	if lockErr != nil {
		return nil, lockErr
	}
	if useReal {
		m.realMu.Lock()
		return m.realMu.Unlock, nil
	}
	return func() {}, nil
}

// makeRotateHandler is a helper that builds a handler with the given persister and param applier.
func makeRotateHandler(persister NodeStatePersister, applier ParamApplier) *AgentHandler {
	return &AgentHandler{
		logger:         zerolog.New(nil), // discard all output
		statePersister: persister,
		paramApplier:   applier,
	}
}

// TestRotateKeypair_Happy: valid 32-byte key, valid tunnel name, persister OK,
// UAPI OK — response carries the curve25519 public key derived from the request's
// private key.
func TestRotateKeypair_Happy(t *testing.T) {
	t.Parallel()

	privKey, err := wg.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	expectedPubKey := privKey.PublicKey()

	persister := &testNodeStatePersister{}
	applier := &testParamApplier{}
	h := makeRotateHandler(persister, applier)

	resp, rpcErr := h.RotateKeypair(context.Background(), &proto.RotateKeypairRequest{
		PrivateKey: privKey[:],
		TunnelName: "ep-01",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %v", rpcErr)
	}
	if resp == nil {
		t.Fatal("nil response")
	}
	if !bytes.Equal(resp.NewPublicKey, expectedPubKey[:]) { //nolint:staticcheck // SA5011: t.Fatal above guards resp
		t.Errorf("public key mismatch:\n  want %s\n  got  %s",
			hex.EncodeToString(expectedPubKey[:]),
			hex.EncodeToString(resp.NewPublicKey))
	}
	if len(persister.persistCalls) != 1 {
		t.Errorf("expected 1 PersistKeypair call, got %d", len(persister.persistCalls))
	}
	if len(applier.calls) != 1 {
		t.Errorf("expected 1 ApplyParams call, got %d", len(applier.calls))
	}
}

// TestRotateKeypair_NilPersister: when statePersister is nil (master/client mode)
// the handler must return codes.Unimplemented without touching anything else.
func TestRotateKeypair_NilPersister(t *testing.T) {
	t.Parallel()

	h := makeRotateHandler(nil, nil)

	privKey, err := wg.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	_, rpcErr := h.RotateKeypair(context.Background(), &proto.RotateKeypairRequest{
		PrivateKey: privKey[:],
		TunnelName: "ep-01",
	})
	if rpcErr == nil {
		t.Fatal("expected Unimplemented error, got nil")
	}
	if st, ok := status.FromError(rpcErr); !ok || st.Code() != codes.Unimplemented {
		t.Errorf("expected Unimplemented, got %v", rpcErr)
	}
}

// TestRotateKeypair_InvalidKeyLength: key shorter than 32 bytes → InvalidArgument.
func TestRotateKeypair_InvalidKeyLength(t *testing.T) {
	t.Parallel()

	persister := &testNodeStatePersister{}
	h := makeRotateHandler(persister, nil)

	_, rpcErr := h.RotateKeypair(context.Background(), &proto.RotateKeypairRequest{
		PrivateKey: make([]byte, 31), // 31 bytes — one short
		TunnelName: "ep-01",
	})
	if rpcErr == nil {
		t.Fatal("expected InvalidArgument, got nil")
	}
	if st, ok := status.FromError(rpcErr); !ok || st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", rpcErr)
	}
	// No persist or lock must have been acquired.
	if len(persister.persistCalls) != 0 {
		t.Errorf("PersistKeypair must not be called on invalid key, got %d calls", len(persister.persistCalls))
	}
}

// TestRotateKeypair_EmptyTunnelName: empty tunnel_name → InvalidArgument.
func TestRotateKeypair_EmptyTunnelName(t *testing.T) {
	t.Parallel()

	privKey, err := wg.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	persister := &testNodeStatePersister{}
	h := makeRotateHandler(persister, nil)

	_, rpcErr := h.RotateKeypair(context.Background(), &proto.RotateKeypairRequest{
		PrivateKey: privKey[:],
		TunnelName: "",
	})
	if rpcErr == nil {
		t.Fatal("expected InvalidArgument, got nil")
	}
	if st, ok := status.FromError(rpcErr); !ok || st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", rpcErr)
	}
}

// TestRotateKeypair_InvalidTunnelName: tunnel name with illegal chars → InvalidArgument.
func TestRotateKeypair_InvalidTunnelName(t *testing.T) {
	t.Parallel()

	privKey, err := wg.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	persister := &testNodeStatePersister{}
	h := makeRotateHandler(persister, nil)

	_, rpcErr := h.RotateKeypair(context.Background(), &proto.RotateKeypairRequest{
		PrivateKey: privKey[:],
		TunnelName: "bad name!", // space + exclamation — fails validTunnelName
	})
	if rpcErr == nil {
		t.Fatal("expected InvalidArgument, got nil")
	}
	if st, ok := status.FromError(rpcErr); !ok || st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", rpcErr)
	}
}

// TestRotateKeypair_PersistError: PersistKeypair returns an error →
// handler returns Internal and does NOT call ApplyParams.
func TestRotateKeypair_PersistError(t *testing.T) {
	t.Parallel()

	privKey, err := wg.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	persister := &testNodeStatePersister{persistErr: errors.New("disk full")}
	applier := &testParamApplier{}
	h := makeRotateHandler(persister, applier)

	_, rpcErr := h.RotateKeypair(context.Background(), &proto.RotateKeypairRequest{
		PrivateKey: privKey[:],
		TunnelName: "ep-01",
	})
	if rpcErr == nil {
		t.Fatal("expected Internal error, got nil")
	}
	if st, ok := status.FromError(rpcErr); !ok || st.Code() != codes.Internal {
		t.Errorf("expected Internal, got %v", rpcErr)
	}
	// ApplyParams must NOT have been called.
	if len(applier.calls) != 0 {
		t.Errorf("ApplyParams must not be called when persist fails, got %d calls", len(applier.calls))
	}
}

// TestRotateKeypair_UAPIError: PersistKeypair succeeds but ApplyParams fails →
// handler returns Internal.
func TestRotateKeypair_UAPIError(t *testing.T) {
	t.Parallel()

	privKey, err := wg.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	persister := &testNodeStatePersister{}
	applier := &testParamApplier{err: errors.New("uapi: device busy")}
	h := makeRotateHandler(persister, applier)

	_, rpcErr := h.RotateKeypair(context.Background(), &proto.RotateKeypairRequest{
		PrivateKey: privKey[:],
		TunnelName: "ep-01",
	})
	if rpcErr == nil {
		t.Fatal("expected Internal error from UAPI failure, got nil")
	}
	if st, ok := status.FromError(rpcErr); !ok || st.Code() != codes.Internal {
		t.Errorf("expected Internal, got %v", rpcErr)
	}
	// Persist was called once (before the UAPI attempt).
	if len(persister.persistCalls) != 1 {
		t.Errorf("expected 1 PersistKeypair call before UAPI, got %d", len(persister.persistCalls))
	}
}

// TestRotateKeypair_ConcurrentLock: two goroutines call RotateKeypair concurrently.
// The mock uses a real mutex so the second call blocks until the first releases the lock.
// Both must complete without data races (run with -race).
func TestRotateKeypair_ConcurrentLock(t *testing.T) {
	t.Parallel()

	privKey, err := wg.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	persister := &testNodeStatePersister{useRealMu: true}
	h := makeRotateHandler(persister, &testParamApplier{})

	req := &proto.RotateKeypairRequest{
		PrivateKey: privKey[:],
		TunnelName: "ep-01",
	}

	var wg2 sync.WaitGroup
	errs := make([]error, 2)
	wg2.Add(2)
	for i := 0; i < 2; i++ {
		i := i
		go func() {
			defer wg2.Done()
			_, errs[i] = h.RotateKeypair(context.Background(), req)
		}()
	}
	wg2.Wait()

	for i, e := range errs {
		if e != nil {
			t.Errorf("goroutine %d: unexpected error: %v", i, e)
		}
	}
	// Both goroutines completed one persist call each → 2 total.
	if len(persister.persistCalls) != 2 {
		t.Errorf("expected 2 PersistKeypair calls (one per goroutine), got %d", len(persister.persistCalls))
	}
}

// TestRotateKeypair_LogHygiene: capture zerolog output into a bytes.Buffer and
// verify that no 4+ consecutive hex bytes from the private key appear in the logs.
// This is the NFR-1 CI assertion: private key bytes must never appear in logs.
func TestRotateKeypair_LogHygiene(t *testing.T) {
	t.Parallel()

	privKey, err := wg.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	persister := &testNodeStatePersister{}
	applier := &testParamApplier{}
	h := &AgentHandler{
		logger:         logger,
		statePersister: persister,
		paramApplier:   applier,
	}

	_, rpcErr := h.RotateKeypair(context.Background(), &proto.RotateKeypairRequest{
		PrivateKey: privKey[:],
		TunnelName: "ep-01",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %v", rpcErr)
	}

	logOutput := buf.String()

	// Check that no 4-byte (8 hex char) run from the private key appears in the log.
	// Four consecutive bytes is long enough to be a leak but short enough to avoid
	// false positives from tunnel names or other incidental hex strings.
	keyHex := hex.EncodeToString(privKey[:])
	for i := 0; i+8 <= len(keyHex); i += 2 {
		chunk := keyHex[i : i+8]
		if strings.Contains(logOutput, chunk) {
			t.Errorf("NFR-1 violation: log output contains private key bytes %q at offset %d.\nLog: %s",
				chunk, i, logOutput)
		}
	}
}

// TestUpdateTunnelPeer_FR5_ErrorMessage verifies FR-5: the structured error
// message for key-mismatch / drift scenarios must:
//   - contain "drifted" (admin state has drifted)
//   - contain "master remove" and "master init" (correct recovery steps)
//   - NOT contain "master reload" (the wrong recovery hint)
func TestUpdateTunnelPeer_FR5_ErrorMessage(t *testing.T) {
	t.Parallel()

	key, err := wg.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pubKey := key.PublicKey()

	// Stub manager that returns a wgctrl peer-replace error, triggering FR-5 path.
	mgr := &testTunnelManager{
		updatePeerErr: fmt.Errorf("wgctrl peer-replace failed: device ep-01: operation not permitted"),
	}
	h := &AgentHandler{
		logger:    zerolog.Nop(),
		tunnelMgr: mgr,
	}

	_, rpcErr := h.UpdateTunnelPeer(context.Background(), &proto.UpdateTunnelPeerRequest{
		Name:          "ep-01",
		PeerPublicKey: pubKey[:],
	})
	if rpcErr == nil {
		t.Fatal("expected error for drift scenario, got nil")
	}

	msg := rpcErr.Error()
	checks := []struct {
		desc    string
		present bool
		needle  string
	}{
		{"drift language", true, "drifted"},
		{"recovery: master remove", true, "master remove"},
		{"recovery: master init", true, "master init"},
		{"wrong hint absent: master reload", false, "master reload"},
	}
	for _, c := range checks {
		contains := strings.Contains(msg, c.needle)
		if c.present && !contains {
			t.Errorf("FR-5: error should contain %q but does not. Full error: %v", c.needle, msg)
		}
		if !c.present && contains {
			t.Errorf("FR-5: error must NOT contain %q but does. Full error: %v", c.needle, msg)
		}
	}
}
