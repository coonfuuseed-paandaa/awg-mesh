//go:build linux

package node

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/device"
	"github.com/thebtf/awg-mesh/pkg/routing"
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

	mtu := calculateMTUFromTopology(m.node.topology, 1)
	iface, err := wg.NewInterface(
		tunnel.InterfaceName,
		mtu,
		device.NewLogger(device.LogLevelError, "[master] "),
	)
	if err != nil {
		return fmt.Errorf("create interface %q: %w", tunnel.InterfaceName, err)
	}

	masterTransportIP := strings.TrimSpace(tunnel.MasterTransportIP)
	endpointTransportIP := strings.TrimSpace(tunnel.EndpointTransportIP)
	transportSubnet := strings.TrimSpace(tunnel.TransportSubnet)

	peerConfigs := make([]wg.PeerConfig, 0, 1)
	if tunnel.PeerPublicKey.IsZero() {
		m.node.logger.Warn().
			Str("tunnel", tunnel.Name).
			Str("endpoint_host", endpointHost).
			Msg("peer public key is empty; configuring tunnel without peers")
	} else {
		peerCfg := wg.PeerConfig{
			PublicKey: tunnel.PeerPublicKey,
		}
		if endpointHost != "" {
			addr, err := net.ResolveUDPAddr("udp", endpointHost)
			if err != nil {
				m.node.logger.Warn().Err(err).Str("endpoint_host", endpointHost).Msg("failed to resolve endpoint address")
			} else {
				peerCfg.Endpoint = addr
			}
		}
		if transportSubnet != "" && endpointTransportIP != "" {
			allowedIPs, err := buildPeerAllowedIPs(transportSubnet, tunnel.OverlayIP)
			if err != nil {
				_ = iface.Close()
				return err
			}
			peerCfg.AllowedIPs = allowedIPs
		}
		peerConfigs = append(peerConfigs, peerCfg)
	}

	cfg := wg.Config{
		PrivateKey: &privateKey,
		Peers:      peerConfigs,
	}
	if err := iface.Configure(cfg); err != nil {
		_ = iface.Close()
		return fmt.Errorf("configure interface %q: %w", tunnel.InterfaceName, err)
	}

	if err := setInterfaceUp(tunnel.InterfaceName); err != nil {
		_ = iface.Close()
		return fmt.Errorf("bring up interface %q: %w", tunnel.InterfaceName, err)
	}

	if masterTransportIP != "" {
		if err := addInterfaceAddress(tunnel.InterfaceName, masterTransportIP); err != nil {
			_ = iface.Close()
			return fmt.Errorf("assign transport address for tunnel %q: %w", tunnel.Name, err)
		}
	}

	tunnel.platformState.iface = iface
	m.node.logger.Info().
		Str("tunnel", tunnel.Name).
		Str("interface", iface.Name()).
		Int("mtu", mtu).
		Msg("master tunnel interface created")

	if endpointTransportIP != "" && strings.TrimSpace(tunnel.OverlayIP) != "" {
		overlayCIDR := normalizeTransportOverlayRoute(tunnel.OverlayIP)
		if err := routing.AddRoute(overlayCIDR, endpointTransportIP, tunnel.InterfaceName); err != nil {
			_ = iface.Close()
			return fmt.Errorf("add overlay route for tunnel %q: %w", tunnel.Name, err)
		}
	}

	if tunnel.BalancerIP != "" {
		m.rebuildECMP(tunnel.BalancerIP)
	}

	return nil
}

func (m *MasterRunner) rebuildECMP(balancerIP string) {
	if balancerIP == "" {
		return
	}

	cidr := balancerIP + "/32"
	m.mu.RLock()
	nexthops := make([]routing.NextHop, 0, len(m.tunnels))
	for _, t := range m.tunnels {
		if t.BalancerIP == balancerIP && t.Healthy {
			nexthops = append(nexthops, routing.NextHop{
				Via:    t.EndpointTransportIP,
				Dev:    "wg-" + t.Name,
				Weight: t.Weight,
			})
		}
	}
	m.mu.RUnlock()

	if len(nexthops) == 0 {
		if err := routing.RemoveECMPRoute(cidr); err != nil {
			m.node.logger.Warn().Str("balancer_ip", balancerIP).Err(err).Msg("failed to remove ECMP route")
			return
		}
		m.node.logger.Info().Str("balancer_ip", balancerIP).Msg("ECMP route removed")
		return
	}

	if err := routing.SetECMPRoute(cidr, nexthops); err != nil {
		m.node.logger.Warn().Str("balancer_ip", balancerIP).Err(err).Msg("failed to rebuild ECMP route")
		return
	}
	m.node.logger.Info().Str("balancer_ip", balancerIP).Int("nexthops", len(nexthops)).Msg("ECMP route updated")
}

func (m *MasterRunner) removeOverlayRoute(overlayIP string) {
	if overlayIP == "" {
		return
	}
	cidr := normalizeTransportOverlayRoute(overlayIP)
	if err := routing.DeleteRoute(cidr); err != nil {
		m.node.logger.Warn().Str("overlay_ip", overlayIP).Err(err).Msg("failed to remove overlay route")
	}
}

func (m *MasterRunner) restoreOverlayRoute(overlayIP, endpointTransportIP, interfaceName string) {
	if overlayIP == "" || endpointTransportIP == "" {
		return
	}
	cidr := normalizeTransportOverlayRoute(overlayIP)
	if err := routing.ReplaceRoute(cidr, endpointTransportIP, interfaceName); err != nil {
		m.node.logger.Warn().Str("overlay_ip", overlayIP).Err(err).Msg("failed to restore overlay route")
	}
}

func setInterfaceUp(interfaceName string) error {
	out, err := exec.Command("ip", "link", "set", interfaceName, "up").CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip link set %s up: %w: %s", interfaceName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func addInterfaceAddress(interfaceName, address string) error {
	ip := net.ParseIP(strings.TrimSpace(address))
	if ip == nil {
		return fmt.Errorf("invalid transport IP %q", address)
	}

	networkAddress := ip.String() + "/30"
	out, err := exec.Command("ip", "addr", "add", networkAddress, "dev", interfaceName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip addr add %s dev %s: %w: %s", networkAddress, interfaceName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func buildPeerAllowedIPs(transportSubnet, overlayIP string) ([]net.IPNet, error) {
	peerAllowed := make([]net.IPNet, 0, 2)
	_, transport, err := net.ParseCIDR(strings.TrimSpace(transportSubnet))
	if err != nil {
		return nil, fmt.Errorf("parse transport subnet %q: %w", transportSubnet, err)
	}
	peerAllowed = append(peerAllowed, *transport)

	if strings.TrimSpace(overlayIP) == "" {
		return nil, fmt.Errorf("overlay IP is required for peer allowed IPs")
	}
	normalizedOverlay := normalizeTransportOverlayRoute(overlayIP)
	_, parsedOverlay, err := net.ParseCIDR(normalizedOverlay)
	if err != nil {
		return nil, fmt.Errorf("parse overlay IP %q: %w", normalizedOverlay, err)
	}
	peerAllowed = append(peerAllowed, *parsedOverlay)
	return peerAllowed, nil
}

func normalizeTransportOverlayRoute(overlayIP string) string {
	trimmed := strings.TrimSpace(overlayIP)
	if trimmed == "" {
		return ""
	}
	if strings.Contains(trimmed, "/") {
		host, _, err := net.ParseCIDR(trimmed)
		if err == nil {
			return host.String() + "/32"
		}
	}
	return trimmed + "/32"
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
		PublicKey:         peer.PublicKey,
		Remove:            peer.Remove,
		UpdateOnly:        peer.UpdateOnly,
		ReplaceAllowedIPs: peer.ReplaceAllowedIPs,
		AllowedIPs:        append([]net.IPNet(nil), peer.AllowedIPs...),
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

// masterHandshakeChecker returns a HandshakeChecker that looks up the last WG
// handshake time for a peer by its public key across all master tunnel interfaces.
func (m *MasterRunner) masterHandshakeChecker() HandshakeChecker {
	return func(peerKey wg.Key) time.Time {
		m.mu.RLock()
		tunnels := make([]*MasterTunnel, 0, len(m.tunnels))
		for _, t := range m.tunnels {
			tunnels = append(tunnels, t)
		}
		m.mu.RUnlock()

		for _, t := range tunnels {
			if t.platformState.iface == nil {
				continue
			}
			dev, err := t.platformState.iface.GetDevice()
			if err != nil {
				continue
			}
			for _, peer := range dev.Peers {
				if peer.PublicKey == peerKey {
					return peer.LastHandshakeTime
				}
			}
		}
		return time.Time{}
	}
}

func (m *MasterRunner) closeTunnelInterface(tunnel *MasterTunnel) error {
	if tunnel == nil || tunnel.platformState.iface == nil {
		return nil
	}

	iface := tunnel.platformState.iface
	tunnel.platformState.iface = nil
	return iface.Close()
}
