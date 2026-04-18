//go:build linux

package node

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/device"
	grpcserver "github.com/coonfuuseed-paandaa/awg-mesh/pkg/grpc"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/routing"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
)

const endpointInterfaceName = "wg0"

var endpointAddInterfaceAddress = addInterfaceAddress
var endpointRouter = routing.NewNetlinkRouter()
var endpointRouteReplaceLink = func(dest *net.IPNet, dev string) error {
	return endpointRouter.RouteReplaceLink(dest, dev)
}

type endpointPlatformState struct {
	iface *wg.Interface
}

func (e *EndpointRunner) createInterface() error {
	if e == nil || e.node == nil {
		return fmt.Errorf("endpoint runner node is required")
	}

	privateKey, _, err := EnsureKeypair(e.node.config.ConfigDir)
	if err != nil {
		return fmt.Errorf("ensure keypair: %w", err)
	}

	mtu := calculateMTUFromTopology(e.node.topology, 1)
	iface, err := wg.NewInterface(
		endpointInterfaceName,
		mtu,
		device.NewLogger(device.LogLevelError, "[endpoint] "),
	)
	if err != nil {
		return fmt.Errorf("create interface %q: %w", endpointInterfaceName, err)
	}

	cfg := wg.Config{
		PrivateKey: &privateKey,
		ListenPort: wg.IntPtr(e.node.config.ListenPort),
	}
	if err := iface.Configure(cfg); err != nil {
		_ = iface.Close()
		return fmt.Errorf("configure interface %q: %w", endpointInterfaceName, err)
	}

	if err := setInterfaceUp(endpointInterfaceName); err != nil {
		_ = iface.Close()
		return fmt.Errorf("bring up interface %q: %w", endpointInterfaceName, err)
	}

	e.platformState.iface = iface
	e.node.logger.Info().
		Str("interface", iface.Name()).
		Int("mtu", mtu).
		Msg("endpoint interface created")
	sysctl := routing.NewProcSysctl()
	if err := sysctl.EnableForwarding(); err != nil {
		e.node.logger.Warn().Err(err).Msg("failed to enable IP forwarding")
	}
	fw, fwErr := routing.NewNftablesFirewall()
	if fwErr != nil {
		e.node.logger.Error().Err(fwErr).Msg("nftables unavailable — firewall rules not applied")
	} else {
		if err := fw.SetupNAT(discoverWANInterface()); err != nil {
			e.node.logger.Warn().Err(err).Msg("nftables: failed to enable masquerade")
		}
		if err := fw.ClampMSSToPMTU(); err != nil {
			e.node.logger.Warn().Err(err).Msg("nftables: failed to enable MSS clamping")
		}
	}

	return nil
}

// ConfigureTransport assigns the local transport IP to wg0 after a peer is added.
// Each master peer gets its own /30 subnet; the endpoint's IP is added to wg0.
func (e *EndpointRunner) ConfigureTransport(pubkeyHex, localIP, peerIP string, allowedIPs []string) error {
	if e == nil || e.node == nil {
		return fmt.Errorf("endpoint runner node is required")
	}

	trimmedLocalIP := strings.TrimSpace(localIP)
	if net.ParseIP(trimmedLocalIP) == nil {
		return fmt.Errorf("local transport IP %q is invalid", localIP)
	}

	if err := endpointAddInterfaceAddress(endpointInterfaceName, trimmedLocalIP); err != nil {
		e.node.logger.Warn().Err(err).
			Str("local_ip", trimmedLocalIP).
			Msg("transport IP may already be assigned")
	}

	overlayIP := parseOverlayIP(e.node.config.OverlayIP)
	for _, allowedCIDR := range allowedIPs {
		trimmedCIDR := strings.TrimSpace(allowedCIDR)
		if trimmedCIDR == "" {
			continue
		}

		_, cidrNet, parseErr := net.ParseCIDR(trimmedCIDR)
		if parseErr != nil {
			e.node.logger.Warn().Err(parseErr).Str("cidr", trimmedCIDR).Msg("skip invalid allowed_ip route")
			continue
		}

		if shouldSkipEndpointLinkRoute(cidrNet, overlayIP) {
			continue
		}

		if err := endpointRouteReplaceLink(cidrNet, endpointInterfaceName); err != nil {
			e.node.logger.Warn().Err(err).Str("cidr", cidrNet.String()).Msg("failed to install overlay route")
		}
	}

	e.node.logger.Info().
		Str("interface", endpointInterfaceName).
		Str("local_ip", trimmedLocalIP).
		Str("peer_ip", peerIP).
		Msg("endpoint transport configured")

	return nil
}

func parseOverlayIP(overlayIP string) net.IP {
	trimmed := strings.TrimSpace(overlayIP)
	if trimmed == "" {
		return nil
	}
	if strings.Contains(trimmed, "/") {
		ip, _, err := net.ParseCIDR(trimmed)
		if err != nil {
			return nil
		}
		return ip
	}
	return net.ParseIP(trimmed)
}

func shouldSkipEndpointLinkRoute(cidrNet *net.IPNet, overlayIP net.IP) bool {
	if cidrNet == nil {
		return true
	}

	ones, bits := cidrNet.Mask.Size()
	if bits > 0 && ones >= 30 {
		return true
	}

	if overlayIP == nil {
		return false
	}

	return ones == 32 && cidrNet.IP.Equal(overlayIP)
}

func (e *EndpointRunner) closeInterface() error {
	if e == nil || e.platformState.iface == nil {
		return nil
	}

	iface := e.platformState.iface
	e.platformState.iface = nil
	return iface.Close()
}

func (e *EndpointRunner) ApplyParams(tunnelName string, cfg wg.Config) error {
	if e == nil || e.node == nil {
		return fmt.Errorf("endpoint runner node is required")
	}
	if e.platformState.iface == nil {
		return fmt.Errorf("endpoint interface is not initialized")
	}

	if err := e.platformState.iface.Configure(cfg); err != nil {
		return fmt.Errorf("configure endpoint interface %q: %w", endpointInterfaceName, err)
	}

	e.node.logger.Info().
		Str("tunnel", tunnelName).
		Str("interface", endpointInterfaceName).
		Msg("applied AWG parameters to endpoint interface")

	return nil
}

// ListPeers returns all current peers on the endpoint interface.
func (e *EndpointRunner) ListPeers() []grpcserver.PeerInfo {
	if e == nil || e.platformState.iface == nil {
		return nil
	}
	dev, err := e.platformState.iface.GetDevice()
	if err != nil {
		return nil
	}
	result := make([]grpcserver.PeerInfo, 0, len(dev.Peers))
	for _, p := range dev.Peers {
		endpoint := ""
		if p.Endpoint != nil {
			endpoint = p.Endpoint.String()
		}
		allowedIPs := make([]string, 0, len(p.AllowedIPs))
		for _, aip := range p.AllowedIPs {
			allowedIPs = append(allowedIPs, aip.String())
		}
		result = append(result, grpcserver.PeerInfo{
			PublicKey:     append([]byte(nil), p.PublicKey[:]...),
			Endpoint:      endpoint,
			AllowedIPs:    allowedIPs,
			LastHandshake: p.LastHandshakeTime.Unix(),
			TxBytes:       p.TransmitBytes,
			RxBytes:       p.ReceiveBytes,
		})
	}
	return result
}

// AddPeer adds a peer to the endpoint interface.
func (e *EndpointRunner) AddPeer(publicKey []byte, presharedKey []byte, allowedIPs []string, endpointHost string, persistentKeepalive int32) error {
	if e == nil || e.platformState.iface == nil {
		return fmt.Errorf("endpoint interface is not initialized")
	}
	key, err := wg.NewKey(publicKey)
	if err != nil {
		return fmt.Errorf("parse peer public key: %w", err)
	}
	peerCfg := wg.PeerConfig{
		PublicKey:         key,
		ReplaceAllowedIPs: true,
	}
	if len(presharedKey) == 32 {
		psk, err := wg.NewKey(presharedKey)
		if err == nil {
			peerCfg.PresharedKey = &psk
		}
	}
	for _, cidr := range allowedIPs {
		_, ipNet, err := net.ParseCIDR(strings.TrimSpace(cidr))
		if err != nil {
			return fmt.Errorf("parse allowed IP %q: %w", cidr, err)
		}
		peerCfg.AllowedIPs = append(peerCfg.AllowedIPs, *ipNet)
	}
	if endpointHost != "" {
		addr, err := net.ResolveUDPAddr("udp", endpointHost)
		if err != nil {
			return fmt.Errorf("resolve endpoint %q: %w", endpointHost, err)
		}
		peerCfg.Endpoint = addr
	}
	if persistentKeepalive > 0 {
		interval := time.Duration(persistentKeepalive) * time.Second
		peerCfg.PersistentKeepaliveInterval = &interval
	}
	return e.platformState.iface.Configure(wg.Config{Peers: []wg.PeerConfig{peerCfg}})
}

// RemovePeer removes a peer from the endpoint interface by public key.
func (e *EndpointRunner) RemovePeer(publicKey []byte) error {
	if e == nil || e.platformState.iface == nil {
		return fmt.Errorf("endpoint interface is not initialized")
	}
	key, err := wg.NewKey(publicKey)
	if err != nil {
		return fmt.Errorf("parse peer public key: %w", err)
	}
	return e.platformState.iface.Configure(wg.Config{
		Peers: []wg.PeerConfig{{PublicKey: key, Remove: true}},
	})
}
