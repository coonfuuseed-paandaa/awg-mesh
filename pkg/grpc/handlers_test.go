package grpcserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/thebtf/awg-mesh/pkg/wg"
	proto "github.com/thebtf/awg-mesh/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type testTunnelManager struct {
	addTunnelCalls []addTunnelCall
	removeCalls    []string
	addErr         error
	removeErr      error
}

type addTunnelCall struct {
	name       string
	host       string
	overlayIP  string
	balancerIP string
	weight     int
	peerKey    wg.Key
}

func (m *testTunnelManager) AddTunnel(name, endpointHost, overlayIP, balancerIP string, weight int, peerPublicKey wg.Key) error {
	m.addTunnelCalls = append(m.addTunnelCalls, addTunnelCall{
		name:       name,
		host:       endpointHost,
		overlayIP:  overlayIP,
		balancerIP: balancerIP,
		weight:     weight,
		peerKey:    peerPublicKey,
	})
	return m.addErr
}

func (m *testTunnelManager) ListTunnels() []TunnelInfo {
	return nil
}

func (m *testTunnelManager) GetParams(tunnelName string) (wg.Config, error) {
	return wg.Config{}, nil
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
		handler := NewAgentHandlerFull(configDir, logger, tunnelMgr, paramApplier, nil, nil, nil)

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

	handler := NewAgentHandler(t.TempDir(), zerolog.Nop())
	resp, err := handler.Init(context.Background(), &proto.InitRequest{
		CaCert:   []byte("ca-cert"),
		NodeCert: []byte("node-cert"),
		NodeKey:  []byte("node-key"),
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
	assertFileContents(t, filepath.Join(configDir, "tls", "ca.crt"), "ca-cert")
	assertFileContents(t, filepath.Join(configDir, "tls", "node.crt"), "node-cert")
	assertFileContents(t, filepath.Join(configDir, "tls", "node.key"), "node-key")
	assertFileContents(t, filepath.Join(configDir, "node-config.json"), "{}")
}

func TestRotateParamsMapsProtoValuesToConfig(t *testing.T) {
	t.Parallel()

	paramApplier := &testParamApplier{}
	handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), nil, paramApplier, nil, nil, nil)
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
	handler := NewAgentHandlerFull(t.TempDir(), zerolog.Nop(), nil, paramApplier, nil, nil, nil)
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
