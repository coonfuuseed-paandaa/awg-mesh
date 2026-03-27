//go:build linux

package node

import (
	"fmt"
	"strings"

	"github.com/amnezia-vpn/amneziawg-go/device"
	"github.com/thebtf/awg-mesh/pkg/wg"
)

type masterTunnelPlatformState struct {
	iface *wg.Interface
}

func (m *MasterRunner) createTunnelInterface(tunnel *MasterTunnel, endpointHost string) error {
	if m == nil || m.node == nil {
		return fmt.Errorf("master runner node is required")
	}
	if tunnel == nil {
		return fmt.Errorf("master tunnel is required")
	}

	privateKey, _, err := EnsureKeypair(m.node.config.ConfigDir)
	if err != nil {
		return fmt.Errorf("ensure keypair: %w", err)
	}

	mtu := CalculateMTU(1420, 80, 1)
	iface, err := wg.NewInterface(
		tunnel.InterfaceName,
		mtu,
		device.NewLogger(device.LogLevelError, "[master] "),
	)
	if err != nil {
		return fmt.Errorf("create interface %q: %w", tunnel.InterfaceName, err)
	}

	peerConfigs := make([]wg.PeerConfig, 0, 1)
	if tunnel.PeerPublicKey.IsZero() {
		m.node.logger.Warn().
			Str("tunnel", tunnel.Name).
			Str("endpoint_host", endpointHost).
			Msg("peer public key is empty; configuring tunnel without peers")
	} else {
		peerConfigs = append(peerConfigs, wg.PeerConfig{
			PublicKey: tunnel.PeerPublicKey,
		})
	}

	cfg := wg.Config{
		PrivateKey: &privateKey,
		Peers:      peerConfigs,
	}
	if err := iface.Configure(cfg); err != nil {
		_ = iface.Close()
		return fmt.Errorf("configure interface %q: %w", tunnel.InterfaceName, err)
	}

	tunnel.platformState.iface = iface
	m.node.logger.Info().
		Str("tunnel", tunnel.Name).
		Str("interface", iface.Name()).
		Int("mtu", mtu).
		Msg("master tunnel interface created")

	return nil
}

func (m *MasterRunner) ApplyParams(tunnelName string, cfg wg.Config) error {
	if m == nil || m.node == nil {
		return fmt.Errorf("master runner node is required")
	}

	trimmedTunnelName := strings.TrimSpace(tunnelName)
	if trimmedTunnelName == "" {
		return fmt.Errorf("tunnel name is required")
	}

	m.mu.RLock()
	tunnel, exists := m.tunnels[trimmedTunnelName]
	m.mu.RUnlock()
	if !exists {
		return fmt.Errorf("tunnel %q not found", trimmedTunnelName)
	}
	if tunnel.platformState.iface == nil {
		return fmt.Errorf("tunnel %q interface is not initialized", trimmedTunnelName)
	}

	if err := tunnel.platformState.iface.Configure(cfg); err != nil {
		return fmt.Errorf("configure tunnel %q: %w", trimmedTunnelName, err)
	}

	m.node.logger.Info().
		Str("tunnel", trimmedTunnelName).
		Msg("applied AWG parameters to tunnel")

	return nil
}

func (m *MasterRunner) closeTunnelInterface(tunnel *MasterTunnel) error {
	if tunnel == nil || tunnel.platformState.iface == nil {
		return nil
	}

	iface := tunnel.platformState.iface
	tunnel.platformState.iface = nil
	return iface.Close()
}
