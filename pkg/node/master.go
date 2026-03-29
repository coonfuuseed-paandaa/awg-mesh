package node

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	grpcserver "github.com/thebtf/awg-mesh/pkg/grpc"
	"github.com/thebtf/awg-mesh/pkg/wg"
)

// MasterTunnel represents a single tunnel managed by master mode.
type MasterTunnel struct {
	Name                string
	InterfaceName       string
	EndpointHost        string
	OverlayIP           string
	BalancerIP          string
	TransportSubnet     string
	MasterTransportIP   string
	EndpointTransportIP string
	PeerPublicKey       wg.Key
	Healthy             bool
	Weight              int
	lastParams          wg.Config
	platformState       masterTunnelPlatformState
}

// MasterRunner runs node logic for master mode.
type MasterRunner struct {
	node      *Node
	tunnels   map[string]*MasterTunnel
	mu        sync.RWMutex
	startTime time.Time
}

// NewMasterRunner creates a master mode runner.
func NewMasterRunner(node *Node) *MasterRunner {
	return &MasterRunner{
		node:    node,
		tunnels: make(map[string]*MasterTunnel),
	}
}

// Run starts master mode and blocks until context cancellation.
func (m *MasterRunner) Run(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	if m == nil || m.node == nil {
		return fmt.Errorf("master runner node is required")
	}

	_, publicKey, err := EnsureKeypair(m.node.config.ConfigDir)
	if err != nil {
		return fmt.Errorf("ensure keypair: %w", err)
	}
	if m.node.config.OverlayIP != "" {
		if err := AssignOverlayIP(m.node.config.OverlayIP); err != nil {
			return fmt.Errorf("assign overlay IP: %w", err)
		}
	}
	if err := startGRPCServer(ctx, m.node.config.ConfigDir, m.node.logger, m, m, nil, m, newCaptureScheduler(m.node.logger, newCaptureFunc()), m); err != nil {
		return fmt.Errorf("start gRPC server: %w", err)
	}
	m.startTime = time.Now()

	defer func() {
		if closeErr := m.closeAllTunnelInterfaces(); closeErr != nil {
			m.node.logger.Warn().Err(closeErr).Msg("failed to close master tunnel interfaces")
		}
	}()

	m.node.logger.Info().
		Str("overlay_ip", m.node.config.OverlayIP).
		Str("public_key", publicKey.String()).
		Msg("master runner started")

	if state, err := loadNodeTransportState(m.node.config.ConfigDir); err == nil && len(state.Tunnels) > 0 {
		reconciled := 0
		for _, tt := range state.Tunnels {
			peerKey := wg.Key{}
			if tt.PeerPublicKey != "" {
				if parsedKey, parseErr := wg.ParseKey(tt.PeerPublicKey); parseErr == nil {
					peerKey = parsedKey
				}
			}

			if err := m.AddTunnel(tt.Name, tt.PeerEndpoint, tt.OverlayIP, tt.BalancerIP, "", tt.TransportIP, tt.PeerTransportIP, 1, peerKey); err != nil {
				if !strings.Contains(err.Error(), "already exists") {
					m.node.logger.Warn().
						Str("tunnel", tt.Name).
						Err(err).
						Msg("reconcile tunnel failed")
				}
			} else {
				reconciled++
			}
		}

		m.node.logger.Info().
			Int("tunnels", reconciled).
			Msg("reconciled tunnels from saved state")
	}

	if m.node.topology != nil {
		for _, ep := range m.node.topology.Endpoints {
			if addErr := m.AddTunnel(ep.Name, ep.Host, ep.OverlayIP, "", "", "", "", 1, wg.Key{}); addErr != nil {
				m.node.logger.Warn().
					Str("endpoint", ep.Name).
					Err(addErr).
					Msg("failed to add tunnel")
			}
		}
	}

	hcCfg := HealthConfig{
		Interval:         defaultHealthInterval,
		Timeout:          defaultHealthTimeout,
		FailureThreshold: defaultHealthFailureThreshold,
	}
	hcLogger := m.node.logger.With().Str("component", "healthcheck").Logger()
	hc := NewHealthChecker(hcCfg, hcLogger)

	go hc.Run(ctx, m.healthTargets,
		func(name string) {
			m.mu.Lock()
			t, ok := m.tunnels[name]
			if ok {
				t.Healthy = false
			}
			m.mu.Unlock()

			if ok && t != nil {
				m.removeOverlayRoute(t.OverlayIP)
				m.rebuildECMP(t.BalancerIP)
				m.node.logger.Info().
					Str("tunnel", t.Name).
					Str("overlay_ip", t.OverlayIP).
					Str("balancer_ip", t.BalancerIP).
					Msg("tunnel down, overlay route removed, ECMP rebuilt")
			}
		},
		func(name string) {
			m.mu.Lock()
			t, ok := m.tunnels[name]
			if ok {
				t.Healthy = true
			}
			m.mu.Unlock()

			if ok && t != nil {
				m.restoreOverlayRoute(t.OverlayIP, t.EndpointTransportIP, t.InterfaceName)
				m.rebuildECMP(t.BalancerIP)
				m.node.logger.Info().
					Str("tunnel", t.Name).
					Str("overlay_ip", t.OverlayIP).
					Str("balancer_ip", t.BalancerIP).
					Msg("tunnel recovered, overlay route restored, ECMP rebuilt")
			}
		},
	)

	<-ctx.Done()

	m.node.logger.Info().Msg("master runner stopping")
	return nil
}

// GetPublicKey returns the master's WireGuard public key.
func (m *MasterRunner) GetPublicKey() (wg.Key, error) {
	_, pubKey, err := EnsureKeypair(m.node.config.ConfigDir)
	return pubKey, err
}

// AddTunnel adds a new tunnel to the managed set.
func (m *MasterRunner) AddTunnel(name, endpointHost, overlayIP, balancerIP, transportSubnet, masterTransportIP, endpointTransportIP string, weight int, peerPublicKey wg.Key) error {
	if name == "" {
		return fmt.Errorf("tunnel name is required")
	}

	m.mu.Lock()
	if _, exists := m.tunnels[name]; exists {
		m.mu.Unlock()
		return fmt.Errorf("tunnel %q already exists", name)
	}

	t := &MasterTunnel{
		Name:                name,
		InterfaceName:       "wg-" + name,
		EndpointHost:        endpointHost,
		OverlayIP:           overlayIP,
		BalancerIP:          balancerIP,
		TransportSubnet:     strings.TrimSpace(transportSubnet),
		MasterTransportIP:   strings.TrimSpace(masterTransportIP),
		EndpointTransportIP: strings.TrimSpace(endpointTransportIP),
		PeerPublicKey:       peerPublicKey,
		Healthy:             false, // Set true only after interface creation succeeds (prevents ECMP race)
		Weight:              weight,
	}
	m.tunnels[name] = t
	m.mu.Unlock()

	if err := m.createTunnelInterface(t, endpointHost); err != nil {
		m.mu.Lock()
		currentTunnel, exists := m.tunnels[name]
		if exists && currentTunnel == t {
			delete(m.tunnels, name)
		}
		m.mu.Unlock()
		return fmt.Errorf("create tunnel interface for %q: %w", name, err)
	}

	// Interface created successfully — mark healthy so ECMP includes this tunnel.
	m.mu.Lock()
	if currentTunnel, exists := m.tunnels[name]; exists && currentTunnel == t {
		t.Healthy = true
	}
	m.mu.Unlock()

	if err := m.saveTransportState(t); err != nil {
		m.mu.Lock()
		currentTunnel, exists := m.tunnels[name]
		if exists && currentTunnel == t {
			delete(m.tunnels, name)
		}
		m.mu.Unlock()
		if closeErr := m.closeTunnelInterface(t); closeErr != nil {
			return fmt.Errorf("save transport state for %q: %w (also failed to close interface: %v)", name, err, closeErr)
		}
		return fmt.Errorf("save transport state for %q: %w", name, err)
	}

	m.node.logger.Info().
		Str("tunnel", name).
		Str("endpoint_host", endpointHost).
		Str("overlay_ip", overlayIP).
		Str("interface", t.InterfaceName).
		Msg("tunnel added")

	return nil
}

// RemoveTunnel removes a tunnel by name.
func (m *MasterRunner) RemoveTunnel(name string) error {
	m.mu.Lock()
	tunnel, exists := m.tunnels[name]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("tunnel %q not found", name)
	}
	delete(m.tunnels, name)
	m.mu.Unlock()

	// Clean up routing state before closing the interface.
	m.removeOverlayRoute(tunnel.OverlayIP)
	if tunnel.BalancerIP != "" {
		m.rebuildECMP(tunnel.BalancerIP)
	}

	if err := m.closeTunnelInterface(tunnel); err != nil {
		return fmt.Errorf("close tunnel interface for %q: %w", name, err)
	}

	m.node.logger.Info().
		Str("tunnel", name).
		Msg("tunnel removed")

	return nil
}

// ListTunnels returns a snapshot of all managed tunnels.
func (m *MasterRunner) listMasterTunnels() []MasterTunnel {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]MasterTunnel, 0, len(m.tunnels))
	for _, t := range m.tunnels {
		result = append(result, *t)
	}
	return result
}

func (m *MasterRunner) healthTargets() []HealthTarget {
	tunnels := m.listMasterTunnels()
	targets := make([]HealthTarget, 0, len(tunnels))
	for _, t := range tunnels {
		pingAddr := t.EndpointTransportIP
		if pingAddr == "" {
			pingAddr = t.OverlayIP
		}
		targets = append(targets, HealthTarget{
			Name:     t.Name,
			PingAddr: pingAddr,
			Healthy:  t.Healthy,
		})
	}
	return targets
}

func (m *MasterRunner) ListTunnels() []grpcserver.TunnelInfo {
	tunnels := m.listMasterTunnels()
	infos := make([]grpcserver.TunnelInfo, 0, len(tunnels))
	for _, tunnel := range tunnels {
		infos = append(infos, grpcserver.TunnelInfo{
			Name:          tunnel.Name,
			OverlayIP:     tunnel.OverlayIP,
			Healthy:       tunnel.Healthy,
			Weight:        tunnel.Weight,
			PeerPublicKey: append([]byte(nil), tunnel.PeerPublicKey[:]...),
		})
	}
	return infos
}

func (m *MasterRunner) GetNodeState() grpcserver.NodeState {
	tunnels := m.listMasterTunnels()
	infos := make([]grpcserver.TunnelInfo, 0, len(tunnels))
	for _, tunnel := range tunnels {
		infos = append(infos, grpcserver.TunnelInfo{
			Name:          tunnel.Name,
			OverlayIP:     tunnel.OverlayIP,
			Healthy:       tunnel.Healthy,
			Weight:        tunnel.Weight,
			PeerPublicKey: append([]byte(nil), tunnel.PeerPublicKey[:]...),
		})
	}
	return grpcserver.NodeState{
		Name:      m.node.config.Name,
		Mode:      "master",
		OverlayIP: m.node.config.OverlayIP,
		Tunnels:   infos,
		StartTime: m.startTime,
	}
}

func (m *MasterRunner) closeAllTunnelInterfaces() error {
	tunnels := m.listTunnelPointers()
	closeErrors := make([]string, 0, len(tunnels))
	for _, tunnel := range tunnels {
		if err := m.closeTunnelInterface(tunnel); err != nil {
			closeErrors = append(closeErrors, fmt.Sprintf("%s: %v", tunnel.Name, err))
		}
	}

	if len(closeErrors) == 0 {
		return nil
	}

	return fmt.Errorf("close tunnel interfaces: %s", strings.Join(closeErrors, "; "))
}

func (m *MasterRunner) listTunnelPointers() []*MasterTunnel {
	m.mu.RLock()
	defer m.mu.RUnlock()

	tunnels := make([]*MasterTunnel, 0, len(m.tunnels))
	for _, tunnel := range m.tunnels {
		tunnels = append(tunnels, tunnel)
	}
	return tunnels
}

func (m *MasterRunner) saveTransportState(tunnel *MasterTunnel) error {
	if m == nil || m.node == nil {
		return fmt.Errorf("master runner node is required")
	}
	if tunnel == nil {
		return fmt.Errorf("master tunnel is required")
	}

	state, err := loadNodeTransportState(m.node.config.ConfigDir)
	if err != nil {
		return fmt.Errorf("load node transport state: %w", err)
	}

	peerPublicKey := tunnel.PeerPublicKey.String()
	if tunnel.PeerPublicKey.IsZero() {
		peerPublicKey = ""
	}

	next := append(make([]TunnelTransport, 0, len(state.Tunnels)+1),
		state.Tunnels...)
	updated := false
	entry := TunnelTransport{
		Name:            tunnel.Name,
		OverlayIP:       tunnel.OverlayIP,
		TransportIP:     tunnel.MasterTransportIP,
		PeerTransportIP: tunnel.EndpointTransportIP,
		PeerPublicKey:   peerPublicKey,
		PeerEndpoint:    tunnel.EndpointHost,
		BalancerIP:      tunnel.BalancerIP,
	}
	for idx, existing := range next {
		if existing.Name == tunnel.Name {
			next[idx] = entry
			updated = true
			break
		}
	}
	if !updated {
		next = append(next, entry)
	}

	return saveNodeTransportState(m.node.config.ConfigDir, NodeTransportState{
		OverlayIP: strings.TrimSpace(m.node.config.OverlayIP),
		Tunnels:   next,
	})
}
