package wg

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
)

const (
	DefaultClientInterfaceName = "wg-clients"
	DefaultMeshInterfaceName   = "wg-mesh"
	DefaultClientListenPort    = 51820
	DefaultMeshListenPort      = 51821
)

// TransportFactory creates one protocol-specific Transport for an interface name.
type TransportFactory func(name string) (Transport, error)

// DualListenerConfig describes the master's two transport listeners.
type DualListenerConfig struct {
	ClientInterfaceName string
	MeshInterfaceName   string
	ClientListenPort    int
	MeshListenPort      int
	ClientPrivateKey    *Key
	MeshPrivateKey      *Key
	VanillaFactory      TransportFactory
	AWGFactory          TransportFactory
}

// DualListenerSnapshot is a stable view of the configured master listeners.
type DualListenerSnapshot struct {
	ClientInterfaceName string
	MeshInterfaceName   string
	ClientListenPort    int
	MeshListenPort      int
	ClientProtocol      Protocol
	MeshProtocol        Protocol
	Started             bool
}

// DualListener owns the master's client-facing vanilla-WG listener and
// mesh-internal AmneziaWG listener.
type DualListener struct {
	cfg     DualListenerConfig
	mu      sync.Mutex
	client  Transport
	mesh    Transport
	started bool
}

// DefaultDualListenerConfig returns production defaults with real transport factories.
func DefaultDualListenerConfig() DualListenerConfig {
	return DualListenerConfig{
		ClientInterfaceName: DefaultClientInterfaceName,
		MeshInterfaceName:   DefaultMeshInterfaceName,
		ClientListenPort:    DefaultClientListenPort,
		MeshListenPort:      DefaultMeshListenPort,
		VanillaFactory:      NewVanillaTransport,
		AWGFactory:          NewAWGTransport,
	}
}

// NewDualListener validates config and returns an unstarted dual listener.
func NewDualListener(cfg DualListenerConfig) (*DualListener, error) {
	normalized := normalizeDualListenerConfig(cfg)
	if err := validateDualListenerConfig(normalized); err != nil {
		return nil, err
	}
	return &DualListener{cfg: normalized}, nil
}

// Start creates and configures both protocol listeners. If the second listener
// fails, the first one is closed before returning the error.
func (l *DualListener) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.started {
		return nil
	}

	client, err := l.cfg.VanillaFactory(l.cfg.ClientInterfaceName)
	if err != nil {
		return fmt.Errorf("create client listener %q: %w", l.cfg.ClientInterfaceName, err)
	}
	if client == nil {
		return fmt.Errorf("create client listener %q: factory returned nil transport", l.cfg.ClientInterfaceName)
	}
	if client.Protocol() != ProtocolVanilla {
		_ = client.Close()
		return fmt.Errorf("client listener %q factory returned protocol %q, want %q", l.cfg.ClientInterfaceName, client.Protocol(), ProtocolVanilla)
	}
	clientCfg, err := clientListenerConfig(l.cfg)
	if err != nil {
		_ = client.Close()
		return err
	}
	if err := client.Configure(clientCfg); err != nil {
		_ = client.Close()
		return fmt.Errorf("configure client listener %q: %w", l.cfg.ClientInterfaceName, err)
	}

	mesh, err := l.cfg.AWGFactory(l.cfg.MeshInterfaceName)
	if err != nil {
		_ = client.Close()
		return fmt.Errorf("create mesh listener %q: %w", l.cfg.MeshInterfaceName, err)
	}
	if mesh == nil {
		_ = client.Close()
		return fmt.Errorf("create mesh listener %q: factory returned nil transport", l.cfg.MeshInterfaceName)
	}
	if mesh.Protocol() != ProtocolAmneziaWG {
		_ = mesh.Close()
		_ = client.Close()
		return fmt.Errorf("mesh listener %q factory returned protocol %q, want %q", l.cfg.MeshInterfaceName, mesh.Protocol(), ProtocolAmneziaWG)
	}
	meshCfg, err := meshListenerConfig(l.cfg)
	if err != nil {
		_ = mesh.Close()
		_ = client.Close()
		return err
	}
	if err := mesh.Configure(meshCfg); err != nil {
		_ = mesh.Close()
		_ = client.Close()
		return fmt.Errorf("configure mesh listener %q: %w", l.cfg.MeshInterfaceName, err)
	}

	l.client = client
	l.mesh = mesh
	l.started = true
	return nil
}

// Close tears down both listeners. It is safe to call more than once.
func (l *DualListener) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	var errs []error
	if l.mesh != nil {
		if err := l.mesh.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close mesh listener %q: %w", l.cfg.MeshInterfaceName, err))
		}
	}
	if l.client != nil {
		if err := l.client.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close client listener %q: %w", l.cfg.ClientInterfaceName, err))
		}
	}
	l.mesh = nil
	l.client = nil
	l.started = false
	return errors.Join(errs...)
}

// Snapshot returns the configured listener state without exposing transports.
func (l *DualListener) Snapshot() DualListenerSnapshot {
	if l == nil {
		return DualListenerSnapshot{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return DualListenerSnapshot{
		ClientInterfaceName: l.cfg.ClientInterfaceName,
		MeshInterfaceName:   l.cfg.MeshInterfaceName,
		ClientListenPort:    l.cfg.ClientListenPort,
		MeshListenPort:      l.cfg.MeshListenPort,
		ClientProtocol:      ProtocolVanilla,
		MeshProtocol:        ProtocolAmneziaWG,
		Started:             l.started,
	}
}

func normalizeDualListenerConfig(cfg DualListenerConfig) DualListenerConfig {
	if cfg.ClientInterfaceName == "" {
		cfg.ClientInterfaceName = DefaultClientInterfaceName
	}
	if cfg.MeshInterfaceName == "" {
		cfg.MeshInterfaceName = DefaultMeshInterfaceName
	}
	if cfg.ClientListenPort == 0 {
		cfg.ClientListenPort = DefaultClientListenPort
	}
	if cfg.MeshListenPort == 0 {
		cfg.MeshListenPort = DefaultMeshListenPort
	}
	return cfg
}

func validateDualListenerConfig(cfg DualListenerConfig) error {
	if cfg.VanillaFactory == nil {
		return errors.New("vanilla transport factory is required")
	}
	if cfg.AWGFactory == nil {
		return errors.New("amneziawg transport factory is required")
	}
	if err := ValidateInterfaceName(cfg.ClientInterfaceName); err != nil {
		return fmt.Errorf("invalid client interface: %w", err)
	}
	if err := ValidateInterfaceName(cfg.MeshInterfaceName); err != nil {
		return fmt.Errorf("invalid mesh interface: %w", err)
	}
	if cfg.ClientInterfaceName == cfg.MeshInterfaceName {
		return fmt.Errorf("client and mesh interfaces must be distinct: %q", cfg.ClientInterfaceName)
	}
	if err := validateListenPort("client listen port", cfg.ClientListenPort); err != nil {
		return err
	}
	if err := validateListenPort("mesh listen port", cfg.MeshListenPort); err != nil {
		return err
	}
	if cfg.ClientListenPort == cfg.MeshListenPort {
		return fmt.Errorf("client and mesh listen ports must be distinct: %d", cfg.ClientListenPort)
	}
	return nil
}

func validateListenPort(name string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%s must be in range 1..65535, got %d", name, port)
	}
	return nil
}

func clientListenerConfig(cfg DualListenerConfig) (Config, error) {
	privateKey, err := listenerPrivateKey(cfg.ClientPrivateKey)
	if err != nil {
		return Config{}, fmt.Errorf("client listener private key: %w", err)
	}
	return Config{
		PrivateKey:   &privateKey,
		ListenPort:   IntPtr(cfg.ClientListenPort),
		ReplacePeers: true,
	}, nil
}

func meshListenerConfig(cfg DualListenerConfig) (Config, error) {
	privateKey, err := listenerPrivateKey(cfg.MeshPrivateKey)
	if err != nil {
		return Config{}, fmt.Errorf("mesh listener private key: %w", err)
	}
	out, err := meshBootstrapConfig()
	if err != nil {
		return Config{}, fmt.Errorf("mesh bootstrap params: %w", err)
	}
	out.PrivateKey = &privateKey
	out.ListenPort = IntPtr(cfg.MeshListenPort)
	out.ReplacePeers = true
	return out, nil
}

func listenerPrivateKey(key *Key) (Key, error) {
	if key != nil {
		if key.IsZero() {
			return Key{}, errors.New("key must not be zero")
		}
		return *key, nil
	}
	return GeneratePrivateKey()
}

func meshBootstrapConfig() (Config, error) {
	seed, err := GenerateKey()
	if err != nil {
		return Config{}, err
	}
	jmin := 64 + int(seed[0]%32)
	jmax := jmin + 32 + int(seed[1]%32)
	return Config{
		Jc:   IntPtr(1 + int(seed[2]%8)),
		Jmin: IntPtr(jmin),
		Jmax: IntPtr(jmax),
		S1:   IntPtr(seedInt(seed, 0)),
		S2:   IntPtr(seedInt(seed, 4)),
		S3:   IntPtr(seedInt(seed, 8)),
		S4:   IntPtr(seedInt(seed, 12)),
		H1:   StrPtr(fmt.Sprintf("%d", seedInt(seed, 16))),
		H2:   StrPtr(fmt.Sprintf("%d", seedInt(seed, 18))),
		H3:   StrPtr(fmt.Sprintf("%d", seedInt(seed, 20))),
		H4:   StrPtr(fmt.Sprintf("%d", seedInt(seed, 22))),
		I1:   StrPtr(fmt.Sprintf("<b %x>", seed[0:6])),
		I2:   StrPtr(fmt.Sprintf("<t %x>", seed[6:12])),
		I3:   StrPtr(fmt.Sprintf("<a %x>", seed[12:18])),
		I4:   StrPtr(fmt.Sprintf("<m %x>", seed[18:24])),
		I5:   StrPtr(fmt.Sprintf("<e %x>", seed[24:30])),
	}, nil
}

func seedInt(seed Key, offset int) int {
	return int(binary.BigEndian.Uint32(seed[offset:offset+4])&0x7fffffff) + 1
}
