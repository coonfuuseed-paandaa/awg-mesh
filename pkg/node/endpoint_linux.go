//go:build linux

package node

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/device"
	grpcserver "github.com/coonfuuseed-paandaa/awg-mesh/pkg/grpc"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/routing"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
)

// endpointLegacyIfaceName is the single-interface name used by pre-v1.12.2 endpoints.
// It is retained here only for ConfigureTransport (legacy code path) and will be
// removed when T003/T004 replaces ConfigureTransport with per-master routing.
const endpointLegacyIfaceName = "wg0"

var endpointAddInterfaceAddress = addInterfaceAddress
var endpointRouteReplaceLink = func(dest *net.IPNet, dev string) error {
	return routing.NewNetlinkRouter().RouteReplaceLink(dest, dev)
}

// endpointPlatformState holds all kernel-level WireGuard interfaces for an endpoint.
// Each interface is keyed by its master name (e.g., "master-a"), not the iface name
// ("wg-master-a"). All map accesses MUST hold mu (RLock for reads, Lock for writes).
type endpointPlatformState struct {
	ifaces map[string]*wg.Interface // keyed by master name
	mu     sync.RWMutex             // protects ifaces map
}

// setIface stores iface under masterName. Acquires write lock.
func (e *EndpointRunner) setIface(masterName string, iface *wg.Interface) {
	e.platformState.mu.Lock()
	defer e.platformState.mu.Unlock()
	if e.platformState.ifaces == nil {
		e.platformState.ifaces = make(map[string]*wg.Interface)
	}
	e.platformState.ifaces[masterName] = iface
}

// getIface returns the interface for masterName, or nil if not found. Acquires read lock.
func (e *EndpointRunner) getIface(masterName string) *wg.Interface {
	e.platformState.mu.RLock()
	defer e.platformState.mu.RUnlock()
	return e.platformState.ifaces[masterName]
}

// deleteIface removes the map entry for masterName without closing the interface.
// The caller is responsible for closing the interface before or after calling this.
// Acquires write lock.
func (e *EndpointRunner) deleteIface(masterName string) {
	e.platformState.mu.Lock()
	defer e.platformState.mu.Unlock()
	delete(e.platformState.ifaces, masterName)
}

// listIfaces returns the current master names in sorted order. Acquires read lock.
func (e *EndpointRunner) listIfaces() []string {
	e.platformState.mu.RLock()
	defer e.platformState.mu.RUnlock()
	names := make([]string, 0, len(e.platformState.ifaces))
	for name := range e.platformState.ifaces {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// closeIface closes and removes the interface for masterName. Acquires write lock.
func (e *EndpointRunner) closeIface(masterName string) error {
	e.platformState.mu.Lock()
	iface := e.platformState.ifaces[masterName]
	delete(e.platformState.ifaces, masterName)
	e.platformState.mu.Unlock()

	if iface == nil {
		return nil
	}
	return iface.Close()
}

// closeAllIfaces closes every interface in the map and clears it. Acquires write lock.
func (e *EndpointRunner) closeAllIfaces() error {
	e.platformState.mu.Lock()
	ifaces := e.platformState.ifaces
	e.platformState.ifaces = nil
	e.platformState.mu.Unlock()

	var lastErr error
	for _, iface := range ifaces {
		if err := iface.Close(); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

func (e *EndpointRunner) createInterface() error {
	if e == nil || e.node == nil {
		return fmt.Errorf("endpoint runner node is required")
	}

	privateKey, _, err := EnsureKeypair(e.node.config.ConfigDir)
	if err != nil {
		return fmt.Errorf("ensure keypair: %w", err)
	}

	// T002 placeholder: create the legacy single wg0 interface stored under key "wg0".
	// T003 will replace this with per-master createMasterInterface calls and remove
	// the reference to endpointLegacyIfaceName.
	mtu := calculateMTUFromTopology(e.node.topology, 1)
	iface, err := wg.NewInterface(
		endpointLegacyIfaceName,
		mtu,
		device.NewLogger(device.LogLevelError, "[endpoint] "),
	)
	if err != nil {
		return fmt.Errorf("create interface %q: %w", endpointLegacyIfaceName, err)
	}

	cfg := wg.Config{
		PrivateKey: &privateKey,
		ListenPort: wg.IntPtr(e.node.config.ListenPort),
	}
	if err := iface.Configure(cfg); err != nil {
		_ = iface.Close()
		return fmt.Errorf("configure interface %q: %w", endpointLegacyIfaceName, err)
	}

	if err := setInterfaceUp(endpointLegacyIfaceName); err != nil {
		_ = iface.Close()
		return fmt.Errorf("bring up interface %q: %w", endpointLegacyIfaceName, err)
	}

	e.setIface(endpointLegacyIfaceName, iface)
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

// ConfigureTransport assigns the local transport IP to the legacy wg0 interface after
// a peer is added. Each master peer gets its own /30 subnet; the endpoint's IP is added
// to wg0. This function will be replaced in T003/T004 with per-master iface routing.
func (e *EndpointRunner) ConfigureTransport(pubkeyHex, localIP, peerIP string, allowedIPs []string) error {
	if e == nil || e.node == nil {
		return fmt.Errorf("endpoint runner node is required")
	}

	trimmedLocalIP := strings.TrimSpace(localIP)
	if net.ParseIP(trimmedLocalIP) == nil {
		return fmt.Errorf("local transport IP %q is invalid", localIP)
	}

	if err := endpointAddInterfaceAddress(endpointLegacyIfaceName, trimmedLocalIP); err != nil {
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

		if err := endpointRouteReplaceLink(cidrNet, endpointLegacyIfaceName); err != nil {
			e.node.logger.Warn().Err(err).Str("cidr", cidrNet.String()).Msg("failed to install overlay route")
		}
	}

	e.node.logger.Info().
		Str("interface", endpointLegacyIfaceName).
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
	if bits > 0 && ones >= 30 && ones < 32 {
		return true
	}

	if overlayIP == nil {
		return false
	}

	return ones == 32 && cidrNet.IP.Equal(overlayIP)
}

func (e *EndpointRunner) closeInterface() error {
	if e == nil {
		return nil
	}
	return e.closeAllIfaces()
}

// ApplyParams applies AWG parameters to the interface for the given tunnel name.
// The tunnelName is used as the map key into platformState.ifaces (master name).
// Falls back to the first available interface for the legacy single-iface case (T002
// placeholder; T003 will route precisely by master name once per-master ifaces exist).
func (e *EndpointRunner) ApplyParams(tunnelName string, cfg wg.Config) error {
	if e == nil || e.node == nil {
		return fmt.Errorf("endpoint runner node is required")
	}

	iface := e.getIface(tunnelName)
	if iface == nil {
		// T002 fallback: try first available iface (legacy wg0 stored under "wg0").
		names := e.listIfaces()
		if len(names) == 0 {
			return fmt.Errorf("endpoint interface is not initialized")
		}
		iface = e.getIface(names[0])
	}
	if iface == nil {
		return fmt.Errorf("endpoint interface is not initialized")
	}

	ifaceName := iface.Name()
	if err := iface.Configure(cfg); err != nil {
		return fmt.Errorf("configure endpoint interface %q: %w", ifaceName, err)
	}

	e.node.logger.Info().
		Str("tunnel", tunnelName).
		Str("interface", ifaceName).
		Msg("applied AWG parameters to endpoint interface")

	return nil
}

// ListPeers returns all current peers across all endpoint interfaces.
func (e *EndpointRunner) ListPeers() []grpcserver.PeerInfo {
	if e == nil {
		return nil
	}
	names := e.listIfaces()
	if len(names) == 0 {
		return nil
	}

	var result []grpcserver.PeerInfo
	for _, name := range names {
		iface := e.getIface(name)
		if iface == nil {
			continue
		}
		dev, err := iface.GetDevice()
		if err != nil {
			continue
		}
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
	}
	return result
}

// firstIface returns the first available interface (sorted by master name) or nil.
// Used as a T002 fallback for AddPeer/RemovePeer until T003 introduces per-master routing.
func (e *EndpointRunner) firstIface() *wg.Interface {
	names := e.listIfaces()
	if len(names) == 0 {
		return nil
	}
	return e.getIface(names[0])
}

// AddPeer adds a peer to the endpoint interface.
// T002 placeholder: routes to the first available interface in the map.
// T003 will introduce per-master routing (AddPeer will accept a masterName parameter
// or derive it from the public key via the transport state).
func (e *EndpointRunner) AddPeer(publicKey []byte, presharedKey []byte, allowedIPs []string, endpointHost string, persistentKeepalive int32) error {
	iface := e.firstIface()
	if iface == nil {
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
	return iface.Configure(wg.Config{Peers: []wg.PeerConfig{peerCfg}})
}

// RemovePeer removes a peer from the endpoint interface by public key.
// T002 placeholder: routes to the first available interface in the map.
// T003 will introduce per-master routing.
func (e *EndpointRunner) RemovePeer(publicKey []byte) error {
	iface := e.firstIface()
	if iface == nil {
		return fmt.Errorf("endpoint interface is not initialized")
	}
	key, err := wg.NewKey(publicKey)
	if err != nil {
		return fmt.Errorf("parse peer public key: %w", err)
	}
	return iface.Configure(wg.Config{
		Peers: []wg.PeerConfig{{PublicKey: key, Remove: true}},
	})
}
