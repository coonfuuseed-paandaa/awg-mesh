//go:build linux

package node

import (
	"fmt"

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

	m.node.logger.Warn().
		Str("tunnel", tunnel.Name).
		Str("endpoint_host", endpointHost).
		Msg("endpoint public key unavailable in topology; configuring empty peer placeholder")

	cfg := wg.Config{
		PrivateKey: &privateKey,
		Peers: []wg.PeerConfig{
			{},
		},
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

func (m *MasterRunner) closeTunnelInterface(tunnel *MasterTunnel) error {
	if tunnel == nil || tunnel.platformState.iface == nil {
		return nil
	}

	iface := tunnel.platformState.iface
	tunnel.platformState.iface = nil
	return iface.Close()
}
