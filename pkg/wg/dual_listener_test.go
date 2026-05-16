package wg

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/amnezia-vpn/amneziawg-go/device/awg"
)

type fakeDualTransport struct {
	name         string
	protocol     Protocol
	configureErr error
	closeErr     error
	configs      []Config
	closeCount   int
}

func (t *fakeDualTransport) Protocol() Protocol { return t.protocol }
func (t *fakeDualTransport) Name() string       { return t.name }
func (t *fakeDualTransport) Configure(cfg Config) error {
	t.configs = append(t.configs, cfg)
	return t.configureErr
}
func (t *fakeDualTransport) AddPeer(PeerConfig) error { return nil }
func (t *fakeDualTransport) RemovePeer(Key) error     { return nil }
func (t *fakeDualTransport) Stats() (*Device, error)  { return &Device{Name: t.name}, nil }
func (t *fakeDualTransport) Close() error {
	t.closeCount++
	return t.closeErr
}

func TestNewDualListenerValidation(t *testing.T) {
	t.Parallel()

	validFactory := func(protocol Protocol) TransportFactory {
		return func(name string) (Transport, error) {
			return &fakeDualTransport{name: name, protocol: protocol}, nil
		}
	}

	tests := []struct {
		name    string
		cfg     DualListenerConfig
		wantErr string
	}{
		{
			name:    "missing factories",
			cfg:     DualListenerConfig{},
			wantErr: "vanilla transport factory is required",
		},
		{
			name: "same interface",
			cfg: DualListenerConfig{
				ClientInterfaceName: "wg0",
				MeshInterfaceName:   "wg0",
				VanillaFactory:      validFactory(ProtocolVanilla),
				AWGFactory:          validFactory(ProtocolAmneziaWG),
			},
			wantErr: "must be distinct",
		},
		{
			name: "invalid port",
			cfg: DualListenerConfig{
				ClientListenPort: -1,
				VanillaFactory:   validFactory(ProtocolVanilla),
				AWGFactory:       validFactory(ProtocolAmneziaWG),
			},
			wantErr: "client listen port",
		},
		{
			name: "same port",
			cfg: DualListenerConfig{
				ClientListenPort: 10000,
				MeshListenPort:   10000,
				VanillaFactory:   validFactory(ProtocolVanilla),
				AWGFactory:       validFactory(ProtocolAmneziaWG),
			},
			wantErr: "listen ports must be distinct",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewDualListener(tt.cfg)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestDualListenerStartConfiguresProtocolSpecificListeners(t *testing.T) {
	t.Parallel()

	var clientTransports []*fakeDualTransport
	var meshTransports []*fakeDualTransport
	_, allowed, err := net.ParseCIDR("172.21.92.130/32")
	if err != nil {
		t.Fatalf("parse allowed CIDR: %v", err)
	}
	clientPeerKey := Key{1}
	listener, err := NewDualListener(DualListenerConfig{
		VanillaFactory: collectingFactory(ProtocolVanilla, &clientTransports, nil),
		AWGFactory:     collectingFactory(ProtocolAmneziaWG, &meshTransports, nil),
		ClientPeers: []PeerConfig{
			{
				PublicKey:         clientPeerKey,
				ReplaceAllowedIPs: true,
				AllowedIPs:        []net.IPNet{*allowed},
			},
		},
	})
	if err != nil {
		t.Fatalf("NewDualListener: %v", err)
	}

	if err := listener.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	if len(clientTransports) != 1 || len(meshTransports) != 1 {
		t.Fatalf("expected one client and one mesh transport, got %d/%d", len(clientTransports), len(meshTransports))
	}
	clientCfg := onlyConfig(t, clientTransports[0])
	meshCfg := onlyConfig(t, meshTransports[0])
	if clientCfg.ListenPort == nil || *clientCfg.ListenPort != DefaultClientListenPort {
		t.Fatalf("unexpected client listen port: %#v", clientCfg.ListenPort)
	}
	if meshCfg.ListenPort == nil || *meshCfg.ListenPort != DefaultMeshListenPort {
		t.Fatalf("unexpected mesh listen port: %#v", meshCfg.ListenPort)
	}
	if clientCfg.PrivateKey == nil || clientCfg.PrivateKey.IsZero() {
		t.Fatalf("client listener private key was not generated")
	}
	if meshCfg.PrivateKey == nil || meshCfg.PrivateKey.IsZero() {
		t.Fatalf("mesh listener private key was not generated")
	}
	if clientCfg.Jc != nil || clientCfg.S1 != nil || clientCfg.H1 != nil || clientCfg.I1 != nil {
		t.Fatalf("vanilla listener must not receive AmneziaWG params: %#v", clientCfg)
	}
	if len(clientCfg.Peers) != 1 || clientCfg.Peers[0].PublicKey != clientPeerKey {
		t.Fatalf("vanilla listener did not receive static client peer: %#v", clientCfg.Peers)
	}
	if len(clientCfg.Peers[0].AllowedIPs) != 1 || clientCfg.Peers[0].AllowedIPs[0].String() != "172.21.92.130/32" {
		t.Fatalf("vanilla listener static peer allowed IPs mismatch: %#v", clientCfg.Peers[0].AllowedIPs)
	}
	if meshCfg.Jc == nil || meshCfg.S1 == nil || meshCfg.H1 == nil || meshCfg.I1 == nil {
		t.Fatalf("mesh listener must receive bootstrap AmneziaWG params: %#v", meshCfg)
	}
	snapshot := listener.Snapshot()
	if !snapshot.Started || snapshot.ClientProtocol != ProtocolVanilla || snapshot.MeshProtocol != ProtocolAmneziaWG {
		t.Fatalf("unexpected snapshot: %#v", snapshot)
	}
}

func TestDualListenerStartUsesConfiguredPrivateKeys(t *testing.T) {
	t.Parallel()

	clientPrivateKey, err := GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey client: %v", err)
	}
	meshPrivateKey, err := GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey mesh: %v", err)
	}
	var clientTransports []*fakeDualTransport
	var meshTransports []*fakeDualTransport
	listener, err := NewDualListener(DualListenerConfig{
		ClientPrivateKey: &clientPrivateKey,
		MeshPrivateKey:   &meshPrivateKey,
		VanillaFactory:   collectingFactory(ProtocolVanilla, &clientTransports, nil),
		AWGFactory:       collectingFactory(ProtocolAmneziaWG, &meshTransports, nil),
	})
	if err != nil {
		t.Fatalf("NewDualListener: %v", err)
	}

	if err := listener.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	clientCfg := onlyConfig(t, clientTransports[0])
	meshCfg := onlyConfig(t, meshTransports[0])
	if clientCfg.PrivateKey == nil || *clientCfg.PrivateKey != clientPrivateKey {
		t.Fatalf("client listener private key = %#v, want configured key", clientCfg.PrivateKey)
	}
	if meshCfg.PrivateKey == nil || *meshCfg.PrivateKey != meshPrivateKey {
		t.Fatalf("mesh listener private key = %#v, want configured key", meshCfg.PrivateKey)
	}
}

func TestMeshBootstrapConfigUsesDriverAcceptedAWGParams(t *testing.T) {
	t.Parallel()

	for i := 0; i < 100; i++ {
		cfg, err := meshBootstrapConfig()
		if err != nil {
			t.Fatalf("meshBootstrapConfig: %v", err)
		}
		if cfg.Jc == nil || *cfg.Jc < 1 {
			t.Fatalf("invalid jc: %#v", cfg.Jc)
		}
		if cfg.Jmin == nil || cfg.Jmax == nil || *cfg.Jmin < 1 || *cfg.Jmax <= *cfg.Jmin {
			t.Fatalf("invalid junk packet range: jmin=%#v jmax=%#v", cfg.Jmin, cfg.Jmax)
		}
		if cfg.S1 == nil || cfg.S2 == nil {
			t.Fatalf("missing s1/s2: %#v", cfg)
		}
		if 148+*cfg.S1 == 92+*cfg.S2 {
			t.Fatalf("init and response packet sizes must differ: s1=%d s2=%d", *cfg.S1, *cfg.S2)
		}
		if cfg.S3 != nil || cfg.S4 != nil {
			t.Fatalf("s3/s4 must not be sent to amneziawg-go v1.0.4: s3=%#v s4=%#v", cfg.S3, cfg.S4)
		}
		headers := map[uint64]struct{}{}
		for _, header := range []*string{cfg.H1, cfg.H2, cfg.H3, cfg.H4} {
			if header == nil {
				t.Fatalf("missing magic header: %#v", cfg)
			}
			parsed, err := strconv.ParseUint(*header, 10, 32)
			if err != nil {
				t.Fatalf("parse header %q: %v", *header, err)
			}
			if parsed <= 4 {
				t.Fatalf("magic header must be >4, got %d", parsed)
			}
			headers[parsed] = struct{}{}
		}
		if len(headers) != 4 {
			t.Fatalf("magic headers must be distinct: %#v", headers)
		}
		if cfg.I1 == nil {
			t.Fatalf("missing i1 handshake template")
		}
		if _, err := awg.Parse("i1", *cfg.I1); err != nil {
			t.Fatalf("invalid i1 template %q: %v", *cfg.I1, err)
		}
	}
}

func TestDualListenerRejectsFactoryProtocolMismatch(t *testing.T) {
	t.Parallel()

	var clientTransports []*fakeDualTransport
	listener, err := NewDualListener(DualListenerConfig{
		VanillaFactory: collectingFactory(ProtocolAmneziaWG, &clientTransports, nil),
		AWGFactory:     func(name string) (Transport, error) { return nil, errors.New("must not be called") },
	})
	if err != nil {
		t.Fatalf("NewDualListener: %v", err)
	}

	err = listener.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "factory returned protocol") {
		t.Fatalf("expected protocol mismatch error, got %v", err)
	}
	if len(clientTransports) != 1 || clientTransports[0].closeCount != 1 {
		t.Fatalf("mismatched client transport was not closed: %#v", clientTransports)
	}
}

func TestDualListenerRejectsNilFactoryTransport(t *testing.T) {
	t.Parallel()

	listener, err := NewDualListener(DualListenerConfig{
		VanillaFactory: func(name string) (Transport, error) { return nil, nil },
		AWGFactory:     collectingFactory(ProtocolAmneziaWG, nil, nil),
	})
	if err != nil {
		t.Fatalf("NewDualListener: %v", err)
	}

	err = listener.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "factory returned nil transport") {
		t.Fatalf("expected nil transport error, got %v", err)
	}
}

func TestDualListenerRollsBackClientOnMeshCreateFailure(t *testing.T) {
	t.Parallel()

	var clientTransports []*fakeDualTransport
	listener, err := NewDualListener(DualListenerConfig{
		VanillaFactory: collectingFactory(ProtocolVanilla, &clientTransports, nil),
		AWGFactory:     func(name string) (Transport, error) { return nil, errors.New("mesh create failed") },
	})
	if err != nil {
		t.Fatalf("NewDualListener: %v", err)
	}

	err = listener.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "mesh create failed") {
		t.Fatalf("expected mesh create error, got %v", err)
	}
	if len(clientTransports) != 1 || clientTransports[0].closeCount != 1 {
		t.Fatalf("client transport was not rolled back: %#v", clientTransports)
	}
}

func TestDualListenerCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	var clientTransports []*fakeDualTransport
	var meshTransports []*fakeDualTransport
	listener, err := NewDualListener(DualListenerConfig{
		VanillaFactory: collectingFactory(ProtocolVanilla, &clientTransports, nil),
		AWGFactory:     collectingFactory(ProtocolAmneziaWG, &meshTransports, nil),
	})
	if err != nil {
		t.Fatalf("NewDualListener: %v", err)
	}
	if err := listener.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("Close first: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("Close second: %v", err)
	}
	if clientTransports[0].closeCount != 1 || meshTransports[0].closeCount != 1 {
		t.Fatalf("expected one close per transport, got client=%d mesh=%d", clientTransports[0].closeCount, meshTransports[0].closeCount)
	}
}

func collectingFactory(protocol Protocol, out *[]*fakeDualTransport, configureErr error) TransportFactory {
	return func(name string) (Transport, error) {
		t := &fakeDualTransport{name: name, protocol: protocol, configureErr: configureErr}
		if out != nil {
			*out = append(*out, t)
		}
		return t, nil
	}
}

func onlyConfig(t *testing.T, transport *fakeDualTransport) Config {
	t.Helper()
	if len(transport.configs) != 1 {
		t.Fatalf("expected one Configure call for %s, got %d", transport.name, len(transport.configs))
	}
	return transport.configs[0]
}
