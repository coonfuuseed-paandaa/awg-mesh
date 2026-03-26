package node

import (
	"context"
	"fmt"
	"sync"
)

// MasterTunnel represents a single tunnel managed by master mode.
type MasterTunnel struct {
	Name          string
	InterfaceName string
	OverlayIP     string
	BalancerIP    string
	Healthy       bool
	Weight        int
}

// MasterRunner runs node logic for master mode.
type MasterRunner struct {
	node    *Node
	tunnels map[string]*MasterTunnel
	mu      sync.RWMutex
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

	m.node.logger.Info().
		Str("overlay_ip", m.node.config.OverlayIP).
		Str("public_key", publicKey.String()).
		Msg("master runner started")

	if m.node.topology != nil {
		for _, ep := range m.node.topology.Endpoints {
			if addErr := m.AddTunnel(ep.Name, ep.Host, ep.OverlayIP, "", 1); addErr != nil {
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

	go hc.Run(ctx, m.ListTunnels,
		func(name string) {
			m.mu.Lock()
			if t, ok := m.tunnels[name]; ok {
				t.Healthy = false
			}
			m.mu.Unlock()
		},
		func(name string) {
			m.mu.Lock()
			if t, ok := m.tunnels[name]; ok {
				t.Healthy = true
			}
			m.mu.Unlock()
		},
	)

	<-ctx.Done()

	m.node.logger.Info().Msg("master runner stopping")
	return nil
}

// AddTunnel adds a new tunnel to the managed set.
func (m *MasterRunner) AddTunnel(name, endpointHost, overlayIP, balancerIP string, weight int) error {
	if name == "" {
		return fmt.Errorf("tunnel name is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.tunnels[name]; exists {
		return fmt.Errorf("tunnel %q already exists", name)
	}

	t := &MasterTunnel{
		Name:          name,
		InterfaceName: "awg-" + name,
		OverlayIP:     overlayIP,
		BalancerIP:    balancerIP,
		Healthy:       true,
		Weight:        weight,
	}
	m.tunnels[name] = t

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
	defer m.mu.Unlock()

	if _, exists := m.tunnels[name]; !exists {
		return fmt.Errorf("tunnel %q not found", name)
	}

	delete(m.tunnels, name)

	m.node.logger.Info().
		Str("tunnel", name).
		Msg("tunnel removed")

	return nil
}

// ListTunnels returns a snapshot of all managed tunnels.
func (m *MasterRunner) ListTunnels() []MasterTunnel {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]MasterTunnel, 0, len(m.tunnels))
	for _, t := range m.tunnels {
		result = append(result, *t)
	}
	return result
}
