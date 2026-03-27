//go:build !linux

package node

import "fmt"

type masterTunnelPlatformState struct{}

func (m *MasterRunner) createTunnelInterface(tunnel *MasterTunnel, endpointHost string) error {
	if m == nil || m.node == nil {
		return fmt.Errorf("master runner node is required")
	}
	if tunnel == nil {
		return fmt.Errorf("master tunnel is required")
	}

	m.node.logger.Warn().
		Str("tunnel", tunnel.Name).
		Str("endpoint_host", endpointHost).
		Msg("AWG tunnel interface not available on this platform")
	return nil
}

func (m *MasterRunner) closeTunnelInterface(tunnel *MasterTunnel) error {
	return nil
}
