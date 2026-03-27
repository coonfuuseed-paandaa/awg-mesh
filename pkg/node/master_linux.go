//go:build linux

package node

import (
	"fmt"
	"net"
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
	tunnel.lastParams = copyConfig(cfg)

	m.node.logger.Info().
		Str("tunnel", trimmedTunnelName).
		Msg("applied AWG parameters to tunnel")

	return nil
}

func (m *MasterRunner) GetParams(tunnelName string) (wg.Config, error) {
	if m == nil || m.node == nil {
		return wg.Config{}, fmt.Errorf("master runner node is required")
	}

	trimmedTunnelName := strings.TrimSpace(tunnelName)
	if trimmedTunnelName == "" {
		return wg.Config{}, fmt.Errorf("tunnel name is required")
	}

	m.mu.RLock()
	tunnel, exists := m.tunnels[trimmedTunnelName]
	m.mu.RUnlock()
	if !exists {
		return wg.Config{}, fmt.Errorf("tunnel %q not found", trimmedTunnelName)
	}
	if tunnel.platformState.iface == nil {
		return wg.Config{}, fmt.Errorf("tunnel %q interface is not initialized", trimmedTunnelName)
	}
	if _, err := tunnel.platformState.iface.GetDevice(); err != nil {
		return wg.Config{}, fmt.Errorf("get tunnel %q device: %w", trimmedTunnelName, err)
	}

	return copyConfig(tunnel.lastParams), nil
}

func copyConfig(source wg.Config) wg.Config {
	result := wg.Config{
		ListenPort:   copyIntPtr(source.ListenPort),
		FirewallMark: copyIntPtr(source.FirewallMark),
		ReplacePeers: source.ReplacePeers,
		Jc:           copyIntPtr(source.Jc),
		Jmin:         copyIntPtr(source.Jmin),
		Jmax:         copyIntPtr(source.Jmax),
		S1:           copyIntPtr(source.S1),
		S2:           copyIntPtr(source.S2),
		S3:           copyIntPtr(source.S3),
		S4:           copyIntPtr(source.S4),
		H1:           copyStringPtr(source.H1),
		H2:           copyStringPtr(source.H2),
		H3:           copyStringPtr(source.H3),
		H4:           copyStringPtr(source.H4),
		I1:           copyStringPtr(source.I1),
		I2:           copyStringPtr(source.I2),
		I3:           copyStringPtr(source.I3),
		I4:           copyStringPtr(source.I4),
		I5:           copyStringPtr(source.I5),
	}

	if len(source.Peers) > 0 {
		result.Peers = make([]wg.PeerConfig, 0, len(source.Peers))
		for _, peer := range source.Peers {
			result.Peers = append(result.Peers, copyPeerConfig(peer))
		}
	}

	return result
}

func copyIntPtr(value *int) *int {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func copyStringPtr(value *string) *string {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func copyPeerConfig(peer wg.PeerConfig) wg.PeerConfig {
	copied := wg.PeerConfig{
		PublicKey:                   peer.PublicKey,
		Remove:                      peer.Remove,
		UpdateOnly:                  peer.UpdateOnly,
		ReplaceAllowedIPs:           peer.ReplaceAllowedIPs,
		AllowedIPs:                  append([]net.IPNet(nil), peer.AllowedIPs...),
	}
	if peer.PresharedKey != nil {
		copiedKey := *peer.PresharedKey
		copied.PresharedKey = &copiedKey
	}
	if peer.Endpoint != nil {
		endpoint := *peer.Endpoint
		copied.Endpoint = &endpoint
	}
	if peer.PersistentKeepaliveInterval != nil {
		copiedInterval := *peer.PersistentKeepaliveInterval
		copied.PersistentKeepaliveInterval = &copiedInterval
	}
	return copied
}

func (m *MasterRunner) closeTunnelInterface(tunnel *MasterTunnel) error {
	if tunnel == nil || tunnel.platformState.iface == nil {
		return nil
	}

	iface := tunnel.platformState.iface
	tunnel.platformState.iface = nil
	return iface.Close()
}
