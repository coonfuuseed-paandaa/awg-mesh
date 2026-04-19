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
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
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

// endpointCreateIfaceFn is the test seam for wg.NewInterface. Unit tests
// replace this with a factory that returns a mock without touching the kernel.
var endpointCreateIfaceFn = func(name string, mtu int, logger *device.Logger) (*wg.Interface, error) {
	return wg.NewInterface(name, mtu, logger)
}

// endpointConfigureIfaceFn is the test seam for (*wg.Interface).Configure.
// Unit tests replace this to capture the wg.Config without kernel access.
var endpointConfigureIfaceFn = func(iface *wg.Interface, cfg wg.Config) error {
	return iface.Configure(cfg)
}

// endpointSetIfaceUpFn is the test seam for setInterfaceUp.
// Unit tests replace this to skip the netlink call.
var endpointSetIfaceUpFn = func(name string) error {
	return setInterfaceUp(name)
}

// buildEndpointPeerAllowedIPs returns the minimal AllowedIPs for an endpoint-side
// master peer: [transport_subnet, master_overlay_ip/32].
//
// This is intentionally NOT using topology.BuildAllowedIPsForEndpoint — that function
// produces the full (overlapping) list used on the master side. The endpoint side uses
// only the minimal set to avoid the AllowedIPs dedup issue (#134): two master peers
// sharing overlapping overlay CIDRs would fight over the same kernel AllowedIPs entry,
// causing one peer's traffic to silently route to the wrong interface. The /30 transport
// subnet and the master's exact /32 overlay IP are sufficient for correct forwarding.
func buildEndpointPeerAllowedIPs(transportSubnet, masterOverlayIP string) ([]net.IPNet, error) {
	trimmedSubnet := strings.TrimSpace(transportSubnet)
	if trimmedSubnet == "" {
		return nil, fmt.Errorf("transport subnet is required")
	}
	_, subnetNet, err := net.ParseCIDR(trimmedSubnet)
	if err != nil {
		return nil, fmt.Errorf("parse transport subnet %q: %w", transportSubnet, err)
	}

	trimmedOverlay := strings.TrimSpace(masterOverlayIP)
	if trimmedOverlay == "" {
		return nil, fmt.Errorf("master overlay IP is required")
	}
	// Normalise to host/32: strip any prefix length the caller may have included.
	overlayHost := trimmedOverlay
	if strings.Contains(trimmedOverlay, "/") {
		h, _, parseErr := net.ParseCIDR(trimmedOverlay)
		if parseErr != nil {
			return nil, fmt.Errorf("parse master overlay IP %q: %w", masterOverlayIP, parseErr)
		}
		overlayHost = h.String()
	}
	_, overlayNet, err := net.ParseCIDR(overlayHost + "/32")
	if err != nil {
		return nil, fmt.Errorf("build overlay /32 for %q: %w", masterOverlayIP, err)
	}

	return []net.IPNet{*subnetNet, *overlayNet}, nil
}

// deriveTransportSubnet returns the network address of the subnet that contains ip,
// expressed as a CIDR string (e.g. "10.255.0.0/30").
// If ip is empty or unparseable, it returns "".
// prefixLen must be in the range [1, 32]; values outside that range use 30 as a safe default.
func deriveTransportSubnet(ip string, prefixLen int) string {
	trimmed := strings.TrimSpace(ip)
	if trimmed == "" {
		return ""
	}
	parsed := net.ParseIP(trimmed)
	if parsed == nil {
		return ""
	}
	if prefixLen < 1 || prefixLen > 32 {
		prefixLen = 30
	}
	_, network, err := net.ParseCIDR(trimmed + "/" + fmt.Sprintf("%d", prefixLen))
	if err != nil {
		return ""
	}
	return network.String()
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
// Also removes overlay routes installed by installOverlayRoutesForMaster.
func (e *EndpointRunner) closeIface(masterName string) error {
	e.platformState.mu.Lock()
	iface := e.platformState.ifaces[masterName]
	delete(e.platformState.ifaces, masterName)
	e.platformState.mu.Unlock()

	// Remove overlay routes before the interface disappears from the kernel.
	if e.node != nil && e.node.topology != nil {
		if routeErr := removeOverlayRoutesForMaster(
			e.node.topology,
			e.node.config.Name,
			masterName,
			routing.NewNetlinkRouter(),
			e.node.logger,
		); routeErr != nil {
			e.node.logger.Warn().
				Err(routeErr).
				Str("master", masterName).
				Msg("endpoint overlay routes: partial remove failure on iface close")
		}
	}

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

// createMasterInterface creates a dedicated AWG interface for one master peer.
// It mirrors master_linux.go::createTunnelInterface for the endpoint side.
//
// Parameters:
//   - master: the master node descriptor from topology
//   - portOffset: added to node.config.ListenPort; index 0 = base port (no shift)
//   - transportIP: endpoint's /30 transport IP for this subnet (may be empty on first-boot)
//   - peerTransportIP: master's /30 transport IP (informational; not used for AllowedIPs)
//   - transportSubnet: /30 CIDR (e.g. "10.255.0.0/30"); empty means peer is added without AllowedIPs
//   - masterPubkey: master's WG public key; zero value means no peer is configured yet
func (e *EndpointRunner) createMasterInterface(
	master topology.MasterNode,
	portOffset int,
	transportIP string,
	peerTransportIP string,
	transportSubnet string,
	masterPubkey wg.Key,
) error {
	if e == nil || e.node == nil {
		return fmt.Errorf("endpoint runner node is required")
	}

	privateKey, _, err := EnsureKeypair(e.node.config.ConfigDir)
	if err != nil {
		return fmt.Errorf("ensure keypair: %w", err)
	}

	// Interface name: "wg-" + master name, truncated so the full name fits the
	// kernel's IFNAMSIZ limit (15 chars + NUL). "wg-" is 3 chars, leaving 12 for the name.
	masterNamePart := master.Name
	if len(masterNamePart) > 12 {
		masterNamePart = masterNamePart[:12]
	}
	ifaceName := "wg-" + masterNamePart

	listenPort := e.node.config.ListenPort + portOffset
	mtu := calculateMTUFromTopology(e.node.topology, 1)

	iface, err := endpointCreateIfaceFn(
		ifaceName,
		mtu,
		device.NewLogger(device.LogLevelError, "[endpoint] "),
	)
	if err != nil {
		return fmt.Errorf("create interface %q for master %q: %w", ifaceName, master.Name, err)
	}

	// Build peer config. If masterPubkey is zero (master not yet known) we configure
	// the interface without a peer — AddPeer/UpdateTunnelPeer will add it later.
	peerConfigs := make([]wg.PeerConfig, 0, 1)
	if !masterPubkey.IsZero() {
		peerCfg := wg.PeerConfig{
			PublicKey:         masterPubkey,
			ReplaceAllowedIPs: true,
		}

		// Resolve master endpoint address for the initial handshake.
		peerAddr := master.PeerAddr()
		if peerAddr != ":" {
			addr, resolveErr := net.ResolveUDPAddr("udp", peerAddr)
			if resolveErr != nil {
				e.node.logger.Warn().
					Err(resolveErr).
					Str("master", master.Name).
					Str("peer_addr", peerAddr).
					Msg("failed to resolve master peer address; peer configured without endpoint")
			} else {
				peerCfg.Endpoint = addr
			}
		}

		if strings.TrimSpace(transportSubnet) != "" && strings.TrimSpace(master.OverlayIP) != "" {
			allowedIPs, aipErr := buildEndpointPeerAllowedIPs(transportSubnet, master.OverlayIP)
			if aipErr != nil {
				_ = iface.Close()
				return fmt.Errorf("build allowed IPs for master %q: %w", master.Name, aipErr)
			}
			peerCfg.AllowedIPs = allowedIPs
		}

		keepalive := 25 * time.Second
		peerCfg.PersistentKeepaliveInterval = &keepalive
		peerConfigs = append(peerConfigs, peerCfg)
	}

	cfg := wg.Config{
		PrivateKey: &privateKey,
		ListenPort: &listenPort,
		Peers:      peerConfigs,
	}
	if err := endpointConfigureIfaceFn(iface, cfg); err != nil {
		_ = iface.Close()
		return fmt.Errorf("configure interface %q: %w", ifaceName, err)
	}

	if err := endpointSetIfaceUpFn(ifaceName); err != nil {
		_ = iface.Close()
		return fmt.Errorf("bring up interface %q: %w", ifaceName, err)
	}

	if strings.TrimSpace(transportIP) != "" {
		if err := endpointAddInterfaceAddress(ifaceName, strings.TrimSpace(transportIP)); err != nil {
			_ = iface.Close()
			return fmt.Errorf("assign transport address %q to interface %q: %w", transportIP, ifaceName, err)
		}
	}

	e.setIface(master.Name, iface)
	e.node.logger.Info().
		Str("event", "endpoint_iface_created").
		Str("interface", ifaceName).
		Str("master", master.Name).
		Int("listen_port", listenPort).
		Msg("endpoint master interface created")

	// Install overlay routes so this endpoint can reach peers via this master iface.
	// Topology may be nil for first-boot scenarios; skip silently in that case.
	if e.node.topology != nil {
		if routeErr := installOverlayRoutesForMaster(
			e.node.topology,
			e.node.config.Name,
			master.Name,
			ifaceName,
			routing.NewNetlinkRouter(),
			e.node.logger,
		); routeErr != nil {
			e.node.logger.Warn().
				Err(routeErr).
				Str("master", master.Name).
				Msg("endpoint overlay routes: partial install failure")
		}
	}

	return nil
}

// createInterface creates AWG interfaces for this endpoint node.
//
// If the topology lists masters for this endpoint AND transport.yml has per-master
// transport metadata (i.e. we have been contacted by at least one master), we use the
// per-master iface path: one wg-<master> device per master.
//
// Otherwise (first-boot before any master has contacted us, or no topology loaded) we
// fall back to the legacy single-wg0 path. The ConfigureTransport RPC will add
// transport metadata on first contact; T005 will retrofit this into the per-master path.
func (e *EndpointRunner) createInterface() error {
	if e == nil || e.node == nil {
		return fmt.Errorf("endpoint runner node is required")
	}

	// Attempt per-master iface creation when topology and transport state are available.
	if e.node.topology != nil {
		masters := e.node.topology.MastersForEndpoint(e.node.config.Name)
		if len(masters) > 0 {
			// Load transport.yml to resolve per-master transport IPs.
			transportState, tsErr := loadNodeTransportState(e.node.config.ConfigDir)
			if tsErr != nil {
				e.node.logger.Warn().Err(tsErr).Msg("failed to load transport state; falling back to legacy interface")
			} else {
				// Build a lookup map: master name → tunnel transport entry.
				tunnelByName := make(map[string]TunnelTransport, len(transportState.Tunnels))
				for _, tt := range transportState.Tunnels {
					tunnelByName[tt.Name] = tt
				}

				created := 0
				for i, master := range masters {
					tt := tunnelByName[master.Name]
					var masterPubkey wg.Key // zero if not yet known
					if tt.PeerPublicKey != "" {
						parsed, parseErr := wg.ParseKey(tt.PeerPublicKey)
						if parseErr == nil {
							masterPubkey = parsed
						}
					}
					// Derive the /30 transport subnet from the endpoint's transport IP.
					// The prefix length comes from the topology transport config (default 30).
					prefixLen := 30
					if e.node.topology.Transport.PrefixLength > 0 {
						prefixLen = e.node.topology.Transport.PrefixLength
					}
					transportSubnet := deriveTransportSubnet(tt.TransportIP, prefixLen)
					if err := e.createMasterInterface(master, i, tt.TransportIP, tt.PeerTransportIP, transportSubnet, masterPubkey); err != nil {
						e.node.logger.Warn().
							Err(err).
							Str("master", master.Name).
							Msg("failed to create per-master interface; skipping")
						continue
					}
					created++
				}
				if created > 0 {
					e.setupForwarding()
					// Reconcile overlay routes for all successfully created per-master ifaces.
					// Non-fatal: a partial failure is logged but does not abort startup.
					if rebuildErr := rebuildAllOverlayRoutes(e, e.node.topology); rebuildErr != nil {
						e.node.logger.Warn().
							Err(rebuildErr).
							Msg("createInterface: rebuildAllOverlayRoutes had partial failures")
					}
					return nil
				}
				e.node.logger.Warn().Msg("no per-master interfaces created; falling back to legacy interface")
			}
		}
	}

	// Legacy path: single wg0 interface, no per-master segregation.
	return e.createLegacyInterface()
}

// createLegacyInterface is the pre-v1.12.2 single-wg0 creation path retained for
// first-boot scenarios where no transport state is yet available.
func (e *EndpointRunner) createLegacyInterface() error {
	privateKey, _, err := EnsureKeypair(e.node.config.ConfigDir)
	if err != nil {
		return fmt.Errorf("ensure keypair: %w", err)
	}

	mtu := calculateMTUFromTopology(e.node.topology, 1)
	iface, err := endpointCreateIfaceFn(
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
	if err := endpointConfigureIfaceFn(iface, cfg); err != nil {
		_ = iface.Close()
		return fmt.Errorf("configure interface %q: %w", endpointLegacyIfaceName, err)
	}

	if err := endpointSetIfaceUpFn(endpointLegacyIfaceName); err != nil {
		_ = iface.Close()
		return fmt.Errorf("bring up interface %q: %w", endpointLegacyIfaceName, err)
	}

	e.setIface(endpointLegacyIfaceName, iface)
	e.node.logger.Info().
		Str("interface", iface.Name()).
		Int("mtu", mtu).
		Msg("endpoint interface created (legacy)")
	e.setupForwarding()
	return nil
}

// setupForwarding enables IP forwarding and configures nftables NAT/MSS clamping.
// Called after all interfaces are brought up. Errors are non-fatal (logged as warn/error).
func (e *EndpointRunner) setupForwarding() {
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
