//go:build !linux

package node

import (
	"fmt"

	"github.com/thebtf/awg-mesh/pkg/wg"
)

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

func (m *MasterRunner) ApplyParams(tunnelName string, cfg wg.Config) error {
	return fmt.Errorf("UAPI not supported on this platform")
}

func (m *MasterRunner) GetParams(tunnelName string) (wg.Config, error) {
	return wg.Config{}, fmt.Errorf("UAPI not supported on this platform")
}

// masterHandshakeChecker returns nil on non-Linux platforms — WG UAPI is unavailable.
func (m *MasterRunner) masterHandshakeChecker() HandshakeChecker {
	return nil
}

func (m *MasterRunner) rebuildECMP(balancerIP string) {}

func (m *MasterRunner) removeOverlayRoute(overlayIP string) {}

func (m *MasterRunner) restoreOverlayRoute(overlayIP, endpointTransportIP, interfaceName string) {}
