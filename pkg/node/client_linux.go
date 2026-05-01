//go:build linux

package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/device"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/dns"
	grpcserver "github.com/coonfuuseed-paandaa/awg-mesh/pkg/grpc"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/routing"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/transport"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
	"github.com/vishvananda/netlink"
)

const clientInterfacePrefix = "wg-c"

type transportLink struct {
	mu               sync.Mutex // serializes concurrent reconfigures/teardowns for this peer
	iface            *wg.Interface
	pubkeyHex        string
	localTransportIP string
	peerTransportIP  string
	balancerIP       string
	healthy          bool
}

// configurePeerOnIfaceFunc is the signature of the wg-configure hook.
// Stored as a field on clientPlatformState so tests can override it per
// ClientRunner instance without mutating package-level state (which would
// race under t.Parallel and leak between tests).
type configurePeerOnIfaceFunc func(
	c *ClientRunner,
	iface *wg.Interface,
	publicKey []byte,
	presharedKey []byte,
	allowedIPs []string,
	endpointHost string,
	persistentKeepalive int32,
) error

// defaultConfigurePeerOnIfaceFn is the production implementation used when
// the clientPlatformState field is nil (e.g. from raw struct construction).
func defaultConfigurePeerOnIfaceFn(
	c *ClientRunner,
	iface *wg.Interface,
	publicKey []byte,
	presharedKey []byte,
	allowedIPs []string,
	endpointHost string,
	persistentKeepalive int32,
) error {
	return c.configurePeerOnIface(iface, publicKey, presharedKey, allowedIPs, endpointHost, persistentKeepalive)
}

// configurePeerHook returns the per-instance wg-configure hook, falling back to
// the production default if the field was never initialised (e.g. raw struct
// construction from tests). Keeping the lookup behind a method lets tests swap
// `c.platformState.configurePeerOnIfaceFn` directly without racing on a
// package-level variable.
func (c *ClientRunner) configurePeerHook() configurePeerOnIfaceFunc {
	if fn := c.platformState.configurePeerOnIfaceFn; fn != nil {
		return fn
	}
	return defaultConfigurePeerOnIfaceFn
}

// ifaceName returns the kernel interface name for this transport link.
func (l *transportLink) ifaceName() string {
	if l.iface == nil {
		return ""
	}
	return l.iface.Name()
}

type clientPlatformState struct {
	mu      sync.Mutex
	links   []*transportLink
	byKey   map[string]*transportLink
	pending map[string]bool // pubkey creation in progress

	// currentStickyCIDRs tracks which CIDRs currently have sticky ECMP rules
	// installed in the kernel. Used by rebuildClientECMP to diff per rebuild
	// and call DisableStickyECMP only for retired CIDRs (FR-6).
	currentStickyCIDRs map[string]bool

	// ecmpRouteInstalled tracks whether this process has successfully called
	// SetECMPRoute(0.0.0.0/0) at least once (legacy path only).
	// Initialized false (zero value). Flipped to true after the first successful
	// SetECMPRoute(defaultDest,...) call, and back to false after a successful
	// RemoveECMPRoute(defaultDest) so a later zero-link rebuild cannot delete a
	// default route this process did not install (e.g. a RouterOS-injected
	// default route via the 100.127.0.1 veth gateway after the original was
	// withdrawn). Bug 7 / REQ-9 / F-002.
	//
	// MUST be read and written only while holding c.platformState.mu —
	// rebuildClientECMP is invoked from multiple goroutines (healthcheck
	// onUp/onDown callbacks, gRPC AddPeer/RemovePeer handlers, the init path),
	// so unsynchronised access is a data race.
	ecmpRouteInstalled bool

	// Injectable for testing. nil = use production implementations.
	router   routing.Router
	firewall routing.Firewall
	sysctl   routing.Sysctl

	// configurePeerOnIfaceFn is the wg-configure hook. Per-instance rather
	// than package-level so parallel tests can swap independently without
	// data races on a shared global.
	configurePeerOnIfaceFn configurePeerOnIfaceFunc

	// beforeExistingLinkLockFn is a test-only seam invoked in AddPeer's
	// existing-link path immediately before acquiring existingLink.mu.
	// Production default is nil (no-op). Tests can set this to signal when
	// the AddPeer call has reached the per-peer-lock point, avoiding
	// time.Sleep-based race assertions.
	beforeExistingLinkLockFn func()

	// vrfManager is non-nil when MESH_VRF=enabled and VRF setup succeeded.
	// Set once during setupClientVRF (before any AddPeer call) and then read
	// concurrently by AddPeer and rebuildClientECMP. The field itself is not
	// mutated after init so no additional lock is needed for reads.
	vrfManager *VRFManager
}

func initClientPlatformState() clientPlatformState {
	return clientPlatformState{
		byKey:                  make(map[string]*transportLink),
		pending:                make(map[string]bool),
		currentStickyCIDRs:     make(map[string]bool),
		configurePeerOnIfaceFn: defaultConfigurePeerOnIfaceFn,
	}
}

// setupClientVRF initialises the VRFManager when MESH_VRF=enabled (FR-10.6).
// Must be called before any AddPeer invocation. When MESH_VRF is absent or
// "disabled", the function is a no-op and returns nil.
//
// On success with MESH_VRF=enabled: c.platformState.vrfManager is set to a
// live VRFManager and will be used by AddPeer + rebuildClientECMP.
// On kernel/module failure with MESH_VRF=enabled: returns error — caller
// MUST treat this as fatal (exit 1 per FR-10.2).
func (c *ClientRunner) setupClientVRF() error {
	if os.Getenv("MESH_VRF") != "enabled" {
		return nil
	}

	vrfName := "vrf_overlay"
	vrfTable := uint32(100)

	if c.node != nil && c.node.topology != nil {
		if n := c.node.topology.Overlay.VRFName; n != "" {
			vrfName = n
		}
		if t := c.node.topology.Overlay.VRFTable; t != 0 {
			vrfTable = t
		}
	}

	var overlayIP net.IP
	if c.node != nil && c.node.config.OverlayIP != "" {
		overlayIP = net.ParseIP(c.node.config.OverlayIP)
	}

	mgr := NewVRFManager(vrfName, vrfTable, overlayIP)
	if err := mgr.Setup(); err != nil {
		return fmt.Errorf("VRF setup (MESH_VRF=enabled): %w", err)
	}

	c.platformState.vrfManager = mgr

	// F-008 FR-4 + Variant D completion: install policy rule routing overlay
	// destinations through the VRF table. Without this, apps in the main netns
	// (not bound to VRF via SO_BINDTODEVICE) consult the main FIB which has no
	// overlay route, and packets fall through to the default gateway. The rule
	// `ip rule add to <overlay-space> lookup <vrf-table>` makes the overlay
	// range VRF-aware for ALL contexts (main netns + VRF-bound). Idempotent —
	// existing rule with same (dst, table) is reused.
	if c.node != nil && c.node.topology != nil {
		if overlaySpace := c.node.topology.Overlay.Space; overlaySpace != "" {
			if err := installVRFOverlayPolicyRule(overlaySpace, int(vrfTable)); err != nil {
				return fmt.Errorf("install VRF policy rule for %s table %d: %w", overlaySpace, vrfTable, err)
			}
			c.node.logger.Info().
				Str("event", "vrf_policy_rule_installed").
				Str("overlay_space", overlaySpace).
				Uint32("vrf_table", vrfTable).
				Msg("policy rule routes overlay traffic through VRF table")
		}
	}

	c.node.logger.Info().
		Str("vrf_name", vrfName).
		Uint32("vrf_table", vrfTable).
		Msg("VRF overlay separation active")
	return nil
}

// isVRFActive reports whether VRF overlay separation is active.
func (c *ClientRunner) isVRFActive() bool {
	return c.platformState.vrfManager != nil
}

// clientIfaceName returns a deterministic WireGuard interface name for a peer.
// "wg-c" + first 4 hex chars of SHA-256(pubkey) → 16-bit name space.
// Deterministic: same pubkey → same name across restarts.
func clientIfaceName(pk wg.Key) string {
	sum := sha256.Sum256(pk[:])
	return clientInterfacePrefix + hex.EncodeToString(sum[:2])
}

type transportInfo struct {
	gateway string
	device  string
}

type resolvedDSCPPolicy struct {
	policy     routing.DSCPPolicy
	name       string
	targets    []string
	unresolved []string
}

// uniqueClientIfaceName resolves name collisions by appending numeric suffixes.
// Must be called with c.platformState.mu held.
//
// Checks BOTH in-memory links AND kernel interfaces for conflicts. The kernel
// check closes the window where a prior crash left a stale wg-c<hash>
// interface on the host that is not yet tracked in c.platformState.links
// (reconcile has not yet cleaned it up). Without this, wg.NewInterface would
// fail with EEXIST on the second run after an ungraceful shutdown.
func (c *ClientRunner) uniqueClientIfaceName(pk wg.Key) string {
	name := clientIfaceName(pk)
	pkHex := hex.EncodeToString(pk[:])
	// Idempotent: same pubkey already exists → reuse its name
	if existing, ok := c.platformState.byKey[pkHex]; ok {
		return existing.ifaceName()
	}
	// Collision: different pubkey holds target name — append -1, -2, ...
	base := name
	for suffix := 1; ; suffix++ {
		if !c.clientIfaceNameConflicts(name) {
			return name
		}
		name = fmt.Sprintf("%s-%d", base, suffix)
	}
}

// clientIfaceNameConflicts returns true if name is already claimed by an
// in-memory link OR an existing kernel interface.
func (c *ClientRunner) clientIfaceNameConflicts(name string) bool {
	for _, link := range c.platformState.links {
		if link.ifaceName() == name {
			return true
		}
	}
	// Kernel check — closes the crash-recovery race window.
	if _, err := netlink.LinkByName(name); err == nil {
		return true
	}
	return false
}

// setupClientFirewallRules applies client-mode nftables rules on startup.
// Currently installs MSS clamping so TCP traffic through overlay tunnels does
// not stall on fragmented packets. Idempotent: safe to call multiple times.
// Non-fatal: logs warn on error and continues (nftables may be unavailable).
func (c *ClientRunner) setupClientFirewallRules() {
	fw := c.firewallDep()
	if fw == nil {
		c.node.logger.Warn().Msg("nftables unavailable — MSS clamping not applied in client mode")
		return
	}
	if err := fw.ClampMSSToPMTU(); err != nil {
		c.node.logger.Warn().Err(err).Msg("nftables: failed to clamp MSS to PMTU in client mode")
	}
}

// routerDep returns the configured router or the production netlink router.
func (c *ClientRunner) routerDep() routing.Router {
	if c.platformState.router != nil {
		return c.platformState.router
	}
	return routing.NewNetlinkRouter()
}

// firewallDep returns the configured firewall or a new nftables firewall.
// Returns nil if nftables is unavailable (non-fatal — caller must nil-check).
func (c *ClientRunner) firewallDep() routing.Firewall {
	if c.platformState.firewall != nil {
		return c.platformState.firewall
	}
	fw, err := routing.NewNftablesFirewall()
	if err != nil {
		return nil
	}
	return fw
}

// sysctlDep returns the configured sysctl or the production proc sysctl.
func (c *ClientRunner) sysctlDep() routing.Sysctl {
	if c.platformState.sysctl != nil {
		return c.platformState.sysctl
	}
	return routing.NewProcSysctl()
}

func (c *ClientRunner) createInterfaces(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	if c == nil || c.node == nil {
		return fmt.Errorf("client runner node is required")
	}

	c.node.logger.Info().Msg("client interfaces will be configured via mesh-ctl client init")
	return nil
}

// AddPeer implements grpcserver.PeerManager.
// It creates a per-peer WireGuard interface and configures the master peer on it.
// peerName is accepted for interface compatibility but ignored by client mode (client
// creates per-peer-key interfaces, not per-master-name interfaces).
func (c *ClientRunner) AddPeer(publicKey []byte, presharedKey []byte, allowedIPs []string, endpointHost string, persistentKeepalive int32, peerName string) error {
	if c == nil || c.node == nil {
		return fmt.Errorf("client runner node is required")
	}
	if len(publicKey) == 0 {
		return fmt.Errorf("public key is required")
	}

	pubkeyHex := hex.EncodeToString(publicKey)

	c.platformState.mu.Lock()
	existingLink, hasExistingLink := c.platformState.byKey[pubkeyHex]
	if hasExistingLink {
		configureIface := existingLink.iface
		beforeLinkLock := c.platformState.beforeExistingLinkLockFn
		c.platformState.mu.Unlock()
		if configureIface == nil {
			return fmt.Errorf("existing interface is nil for peer %q", pubkeyHex[:8])
		}
		if beforeLinkLock != nil {
			beforeLinkLock()
		}
		existingLink.mu.Lock()
		err := c.configurePeerHook()(c, configureIface, publicKey, presharedKey, allowedIPs, endpointHost, persistentKeepalive)
		existingLink.mu.Unlock()
		return err
	}

	// Mark this key as pending to prevent concurrent AddPeer from creating a duplicate.
	if c.platformState.pending[pubkeyHex] {
		c.platformState.mu.Unlock()
		return fmt.Errorf("AddPeer for %s already in progress", pubkeyHex[:8])
	}
	c.platformState.pending[pubkeyHex] = true

	peerKey, keyErr := wg.NewKey(publicKey)
	if keyErr != nil {
		delete(c.platformState.pending, pubkeyHex)
		c.platformState.mu.Unlock()
		return fmt.Errorf("parse peer public key for naming: %w", keyErr)
	}
	ifaceName := c.uniqueClientIfaceName(peerKey)
	c.platformState.mu.Unlock()

	// Ensure pending flag is cleared on any exit path.
	defer func() {
		c.platformState.mu.Lock()
		delete(c.platformState.pending, pubkeyHex)
		c.platformState.mu.Unlock()
	}()

	privateKey, _, err := EnsureKeypair(c.node.config.ConfigDir)
	if err != nil {
		return fmt.Errorf("ensure keypair: %w", err)
	}

	mtu := calculateMTUFromTopology(c.node.topology, 1)
	iface, err := wg.NewInterface(ifaceName, mtu, device.NewLogger(device.LogLevelError, "[client] "))
	if err != nil {
		return fmt.Errorf("create client interface %q: %w", ifaceName, err)
	}

	if err := iface.Configure(wg.Config{PrivateKey: &privateKey}); err != nil {
		_ = iface.Close()
		return fmt.Errorf("configure private key on %q: %w", ifaceName, err)
	}

	if err := setInterfaceUp(ifaceName); err != nil {
		_ = iface.Close()
		return fmt.Errorf("bring up interface %q: %w", ifaceName, err)
	}

	// Enslave to VRF before Configure so transport /30 connected routes land in
	// the VRF table immediately upon peer config, never leaking into main table
	// (FR-3.3, FR-3.4). vrfManager is set once at startup and read-only here.
	if mgr := c.platformState.vrfManager; mgr != nil {
		if err := mgr.EnslaveInterface(ifaceName); err != nil {
			_ = iface.Close()
			return fmt.Errorf("enslave %q to VRF %q: %w", ifaceName, mgr.Name(), err)
		}
	}

	if err := c.configurePeerHook()(c, iface, publicKey, presharedKey, allowedIPs, endpointHost, persistentKeepalive); err != nil {
		_ = iface.Close()
		return fmt.Errorf("configure peer on %q: %w", ifaceName, err)
	}

	newLink := &transportLink{
		iface:     iface,
		pubkeyHex: pubkeyHex,
		healthy:   false,
	}

	c.platformState.mu.Lock()
	// Re-check after re-acquiring mu: another goroutine may have inserted a link
	// for this pubkey between the `pending` clear (deferred) and this lock. If so,
	// our freshly created interface is redundant — close it and return idempotently
	// to avoid orphaning a kernel interface.
	if existing, alreadyExists := c.platformState.byKey[pubkeyHex]; alreadyExists && existing != nil {
		c.platformState.mu.Unlock()
		_ = iface.Close()
		return nil
	}
	nextLinks := append(append([]*transportLink(nil), c.platformState.links...), newLink)
	c.platformState.links = nextLinks
	c.platformState.byKey[pubkeyHex] = newLink
	c.platformState.mu.Unlock()

	// Apply per-interface MASQUERADE rule so return packets reach the client.
	// Non-fatal: log warn and continue — overlay routing still works; only NAT
	// for return traffic through this interface is affected.
	if fw := c.firewallDep(); fw != nil {
		if err := fw.SetupNAT(ifaceName); err != nil {
			c.node.logger.Warn().Err(err).Str("interface", ifaceName).Msg("nftables: SetupNAT failed (non-fatal)")
		}
	}

	peerLabel := pubkeyHex
	if len(peerLabel) > 8 {
		peerLabel = peerLabel[:8] + "..."
	}
	c.node.logger.Info().
		Str("interface", ifaceName).
		Str("peer", peerLabel).
		Int("mtu", mtu).
		Msg("client interface created")

	return nil
}

func (c *ClientRunner) configurePeerOnIface(iface *wg.Interface, publicKey []byte, presharedKey []byte, allowedIPs []string, endpointHost string, persistentKeepalive int32) error {
	if iface == nil {
		return fmt.Errorf("interface is required")
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

	resolvedEndpointHost := extractPeerEndpoint(endpointHost)
	if resolvedEndpointHost != "" {
		addr, err := net.ResolveUDPAddr("udp", resolvedEndpointHost)
		if err != nil {
			return fmt.Errorf("resolve endpoint %q: %w", resolvedEndpointHost, err)
		}
		peerCfg.Endpoint = addr
	}

	if persistentKeepalive > 0 {
		interval := time.Duration(persistentKeepalive) * time.Second
		peerCfg.PersistentKeepaliveInterval = &interval
	}

	return iface.Configure(wg.Config{Peers: []wg.PeerConfig{peerCfg}})
}

// ListPeers implements grpcserver.PeerManager.
func (c *ClientRunner) ListPeers() []grpcserver.PeerInfo {
	if c == nil {
		return nil
	}

	c.platformState.mu.Lock()
	linksSnapshot := append([]*transportLink(nil), c.platformState.links...)
	c.platformState.mu.Unlock()

	result := make([]grpcserver.PeerInfo, 0)
	for _, link := range linksSnapshot {
		if link == nil || link.iface == nil {
			continue
		}
		dev, err := link.iface.GetDevice()
		if err != nil {
			continue
		}
		for _, peer := range dev.Peers {
			endpoint := ""
			if peer.Endpoint != nil {
				endpoint = peer.Endpoint.String()
			}
			allowedIPs := make([]string, 0, len(peer.AllowedIPs))
			for _, allowedIP := range peer.AllowedIPs {
				allowedIPs = append(allowedIPs, allowedIP.String())
			}
			result = append(result, grpcserver.PeerInfo{
				PublicKey:     append([]byte(nil), peer.PublicKey[:]...),
				Endpoint:      endpoint,
				AllowedIPs:    allowedIPs,
				LastHandshake: peer.LastHandshakeTime.Unix(),
				TxBytes:       peer.TransmitBytes,
				RxBytes:       peer.ReceiveBytes,
			})
		}
	}

	return result
}

// RemovePeer implements grpcserver.PeerManager.
func (c *ClientRunner) RemovePeer(publicKey []byte) error {
	if c == nil {
		return nil
	}
	if len(publicKey) == 0 {
		return fmt.Errorf("public key is required")
	}

	pubkeyHex := hex.EncodeToString(publicKey)

	c.platformState.mu.Lock()
	link, exists := c.platformState.byKey[pubkeyHex]
	if !exists {
		c.platformState.mu.Unlock()
		return nil
	}

	nextLinks := make([]*transportLink, 0, len(c.platformState.links)-1)
	for _, currentLink := range c.platformState.links {
		if currentLink != link {
			nextLinks = append(nextLinks, currentLink)
		}
	}

	nextByKey := make(map[string]*transportLink, len(c.platformState.byKey)-1)
	for key, value := range c.platformState.byKey {
		if key != pubkeyHex {
			nextByKey[key] = value
		}
	}

	c.platformState.links = nextLinks
	c.platformState.byKey = nextByKey
	c.platformState.mu.Unlock()

	link.mu.Lock()
	if err := c.rebuildClientECMP("balancer_change"); err != nil {
		c.node.logger.Warn().Err(err).Msg("rebuildClientECMP after peer removal failed")
	}
	closeErr := link.iface.Close()
	link.mu.Unlock()

	return closeErr
}

// ConfigureTransport implements grpcserver.TransportConfigurator.
// allowedIPs is accepted for interface compliance but ignored in client mode:
// client overlay routing uses ECMP via rebuildClientECMP, not per-peer link routes.
// extraRoutes carries topology-derived overlay CIDRs when the client process does
// not have a mounted topology file; the first valid CIDR is persisted into
// clientState so the initial ECMP rebuild can still program the overlay route.
func (c *ClientRunner) ConfigureTransport(pubkeyHex, localIP, peerIP string, _ []string, _ string, extraRoutes []string) error {
	if c == nil || c.node == nil {
		return fmt.Errorf("client runner node is required")
	}

	trimmedPubkeyHex := strings.TrimSpace(pubkeyHex)
	if trimmedPubkeyHex == "" {
		return fmt.Errorf("pubkey hex is required")
	}

	trimmedLocalIP := strings.TrimSpace(localIP)
	if net.ParseIP(trimmedLocalIP) == nil {
		return fmt.Errorf("local transport IP %q is invalid", localIP)
	}

	trimmedPeerIP := strings.TrimSpace(peerIP)
	if net.ParseIP(trimmedPeerIP) == nil {
		return fmt.Errorf("peer transport IP %q is invalid", peerIP)
	}

	persistedOverlaySpace := ""
	for _, route := range extraRoutes {
		trimmedRoute := strings.TrimSpace(route)
		if trimmedRoute == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(trimmedRoute); err != nil {
			return fmt.Errorf("client extra route %q is invalid: %w", route, err)
		}
		persistedOverlaySpace = trimmedRoute
		break
	}

	c.platformState.mu.Lock()
	link, exists := c.platformState.byKey[trimmedPubkeyHex]
	if !exists {
		c.platformState.mu.Unlock()
		return fmt.Errorf("no interface found for peer %q", trimmedPubkeyHex)
	}

	updatedLink := &transportLink{
		iface:            link.iface,
		pubkeyHex:        link.pubkeyHex,
		localTransportIP: trimmedLocalIP,
		peerTransportIP:  trimmedPeerIP,
		balancerIP:       link.balancerIP,
		healthy:          link.healthy,
	}

	nextLinks := make([]*transportLink, 0, len(c.platformState.links))
	for _, currentLink := range c.platformState.links {
		if currentLink == link {
			nextLinks = append(nextLinks, updatedLink)
			continue
		}
		nextLinks = append(nextLinks, currentLink)
	}

	nextByKey := make(map[string]*transportLink, len(c.platformState.byKey))
	for key, value := range c.platformState.byKey {
		nextByKey[key] = value
	}
	nextByKey[trimmedPubkeyHex] = updatedLink

	c.platformState.links = nextLinks
	c.platformState.byKey = nextByKey
	if persistedOverlaySpace != "" {
		if c.clientState == nil {
			c.clientState = &ClientState{}
		}
		c.clientState.OverlaySpace = persistedOverlaySpace
	}
	c.platformState.mu.Unlock()

	if err := ensureInterfaceAddress(updatedLink.iface.Name(), trimmedLocalIP); err != nil {
		return fmt.Errorf("assign transport IP %s/30 on %s: %w", trimmedLocalIP, updatedLink.iface.Name(), err)
	}

	if err := c.rebuildClientECMP("balancer_change"); err != nil {
		return err
	}

	peerLabel := trimmedPubkeyHex
	if len(peerLabel) > 8 {
		peerLabel = peerLabel[:8] + "..."
	}
	c.node.logger.Info().
		Str("interface", updatedLink.iface.Name()).
		Str("peer", peerLabel).
		Str("local_ip", trimmedLocalIP).
		Str("peer_ip", trimmedPeerIP).
		Msg("configured client transport link")

	return nil
}

func (c *ClientRunner) reconcileFromTransportState() error {
	if c == nil || c.node == nil {
		return fmt.Errorf("client runner node is required")
	}

	state, err := loadNodeTransportState(c.node.config.ConfigDir)
	if err != nil {
		return fmt.Errorf("load node transport state: %w", err)
	}
	if len(state.Tunnels) == 0 {
		return nil
	}

	if transport.IsLegacySchema(state) {
		transport.ApplyLegacyDefaults(&state, c.node.logger)
		// Persist migrated state so the WARN fires only once per node lifetime.
		// If the write fails (read-only FS, disk full), continue in-memory — the
		// reconcile still succeeds and the next AddPeer will re-attempt persistence.
		if err := transport.SaveNodeTransportState(
			filepath.Join(c.node.config.ConfigDir, "transport.yml"),
			state,
		); err != nil {
			c.node.logger.Warn().
				Err(err).
				Msg("persist migrated transport.yml failed; continuing with in-memory state")
		}
	}

	reconciled := 0
	for _, tunnel := range state.Tunnels {
		if strings.TrimSpace(tunnel.PeerPublicKey) == "" || strings.TrimSpace(tunnel.TransportIP) == "" || strings.TrimSpace(tunnel.PeerTransportIP) == "" {
			continue
		}

		if !transport.IsLegacySchema(state) && len(tunnel.AllowedIPs) == 0 {
			return fmt.Errorf("reconcile: tunnel %q has no allowed_ips in v1.6.0 schema state", tunnel.Name)
		}

		peerPublicKey, err := hex.DecodeString(strings.TrimSpace(tunnel.PeerPublicKey))
		if err != nil {
			c.node.logger.Warn().
				Str("tunnel", tunnel.Name).
				Err(err).
				Msg("reconcile peer: decode key failed")
			continue
		}

		if err := c.AddPeer(peerPublicKey, nil, tunnel.AllowedIPs, strings.TrimSpace(tunnel.PeerEndpoint), tunnel.PersistentKeepalive, tunnel.Name); err != nil {
			c.node.logger.Warn().
				Str("tunnel", tunnel.Name).
				Err(err).
				Msg("reconcile peer failed")
			continue
		}

		pubkeyHex := hex.EncodeToString(peerPublicKey)
		if balancerIP := strings.TrimSpace(tunnel.BalancerIP); balancerIP != "" {
			c.SetBalancerIP(pubkeyHex, balancerIP)
		}
		if err := c.ConfigureTransport(pubkeyHex, strings.TrimSpace(tunnel.TransportIP), strings.TrimSpace(tunnel.PeerTransportIP), tunnel.AllowedIPs, tunnel.Name, nil); err != nil {
			c.node.logger.Warn().
				Str("tunnel", tunnel.Name).
				Err(err).
				Msg("reconcile transport failed")
			continue
		}

		reconciled++
	}

	c.node.logger.Info().
		Int("tunnels", reconciled).
		Msg("reconciled client transport from saved state")

	// Legacy wg-c* interfaces cleanup — removes names that don't match the
	// deterministic naming for any current peer. Safe no-op on fresh nodes.
	knownNames := make(map[string]bool)
	c.platformState.mu.Lock()
	for _, link := range c.platformState.links {
		knownNames[link.ifaceName()] = true
	}
	c.platformState.mu.Unlock()

	allLinks, listErr := netlink.LinkList()
	if listErr != nil {
		c.node.logger.Warn().Err(listErr).Msg("list links for legacy cleanup failed")
		return nil
	}
	for _, l := range allLinks {
		name := l.Attrs().Name
		if !strings.HasPrefix(name, clientInterfacePrefix) {
			continue
		}
		if knownNames[name] {
			continue
		}
		if err := netlink.LinkDel(l); err != nil {
			c.node.logger.Warn().Str("iface", name).Err(err).Msg("legacy iface cleanup failed")
			continue
		}
		c.node.logger.Info().Str("iface", name).Str("event", "legacy_iface_cleanup").Msg("removed stale client wg interface")
	}

	return nil
}

func (c *ClientRunner) closeInterfaces() error {
	if c == nil {
		return nil
	}

	c.platformState.mu.Lock()
	linksSnapshot := append([]*transportLink(nil), c.platformState.links...)
	c.platformState.links = nil
	c.platformState.byKey = nil
	c.platformState.mu.Unlock()

	closeErrors := make([]string, 0, len(linksSnapshot))
	for _, link := range linksSnapshot {
		if link == nil || link.iface == nil {
			continue
		}
		if err := link.iface.Close(); err != nil {
			closeErrors = append(closeErrors, err.Error())
		}
	}

	if len(closeErrors) == 0 {
		return nil
	}
	return fmt.Errorf("close client interfaces: %s", strings.Join(closeErrors, "; "))
}

func ensureInterfaceAddress(interfaceName, address string) error {
	trimmedAddress := strings.TrimSpace(address)
	parsedIP := net.ParseIP(trimmedAddress)
	if parsedIP == nil {
		return fmt.Errorf("invalid transport IP %q", address)
	}

	addr := &net.IPNet{IP: parsedIP, Mask: net.CIDRMask(30, 32)}
	router := routing.NewNetlinkRouter()
	exists, err := router.AddrExists(interfaceName, addr)
	if err == nil && exists {
		return nil
	}

	return router.AddrAdd(interfaceName, addr)
}

// rebuildClientECMP is the single unified ECMP rebuild path for client mode.
// It reads current link state under the lock, determines the routing mode
// (VIP path when all links share a non-empty balancerIP, legacy path when all
// links have empty balancerIP), and installs or withdraws routes using only
// healthy links as nexthops, with sticky sessions and L4 hash enabled.
//
// Mixed balancerIP presence across links is a FR-1 invariant violation;
// in that case an error is returned and no partial state is installed.
// Routing mode is determined from ALL links (not just healthy) so that a
// zero-healthy VIP topology still knows to withdraw balancerIP/32 (not 0.0.0.0/0).
//
// reason is a caller-supplied label (e.g. "init", "onUp", "onDown", "reconcile",
// "balancer_change") threaded into structured log events for observability (NFR-5).
func (c *ClientRunner) rebuildClientECMP(reason string) error {
	c.platformState.mu.Lock()
	linksSnapshot := append([]*transportLink(nil), c.platformState.links...)
	ecmpInstalledSnapshot := c.platformState.ecmpRouteInstalled
	c.platformState.mu.Unlock()

	// Determine routing mode from ALL configured links (includes unhealthy).
	// We need this to know which destination to withdraw even when no links are healthy.
	var commonBalancerIP string
	isVIP := false
	modeSet := false
	for _, link := range linksSnapshot {
		if link == nil || link.iface == nil {
			continue
		}
		if !modeSet {
			commonBalancerIP = link.balancerIP
			isVIP = link.balancerIP != ""
			modeSet = true
			continue
		}
		// Detect mixed state: error out immediately, install nothing.
		if (link.balancerIP != "") != isVIP || (isVIP && link.balancerIP != commonBalancerIP) {
			c.node.logger.Error().
				Str("event", "ecmp_mixed_balancer").
				Int("link_count", len(linksSnapshot)).
				Str("reason", reason).
				Msg("links have mixed balancerIP presence — FR-1 invariant violated, aborting ECMP rebuild")
			return fmt.Errorf("client ECMP: links have mixed balancerIP presence (FR-1 violation)")
		}
	}

	// Collect healthy links with resolved peer transport IP for nexthops.
	healthyLinks := make([]*transportLink, 0, len(linksSnapshot))
	for _, link := range linksSnapshot {
		if link == nil || link.iface == nil {
			continue
		}
		if !link.healthy || strings.TrimSpace(link.peerTransportIP) == "" {
			continue
		}
		healthyLinks = append(healthyLinks, link)
	}

	router := c.routerDep()

	if isVIP {
		// VIP path: primary dest = balancerIP/32, secondary dest = overlay.space.
		primaryCIDR := commonBalancerIP + "/32"
		_, primaryDest, _ := net.ParseCIDR(primaryCIDR)

		if len(healthyLinks) == 0 {
			c.node.logger.Info().
				Str("event", "ecmp_withdraw").
				Str("dest", primaryCIDR).
				Str("reason", "no_healthy_links").
				Msg("withdrawing client ECMP route")
			if primaryDest != nil {
				if err := router.RemoveECMPRoute(primaryDest); err != nil {
					return fmt.Errorf("remove client ECMP route %s: %w", primaryCIDR, err)
				}
			}
			return nil
		}

		nexthops := buildNexthopsFromLinks(healthyLinks)

		// When VRF is active, install all ECMP routes into the VRF routing table
		// (default table 100) so overlay traffic stays isolated (FR-4.3).
		vrfTable := 0
		if mgr := c.platformState.vrfManager; mgr != nil {
			vrfTable = int(mgr.Table())
		}

		if primaryDest != nil {
			if vrfTable != 0 {
				if err := router.SetECMPRouteInTable(primaryDest, nexthops, vrfTable); err != nil {
					return fmt.Errorf("set client ECMP route %s table %d: %w", primaryCIDR, vrfTable, err)
				}
			} else {
				if err := router.SetECMPRoute(primaryDest, nexthops); err != nil {
					return fmt.Errorf("set client ECMP route %s: %w", primaryCIDR, err)
				}
			}
		}

		// Also route the overlay space through the same nexthops when available.
		// Set src=overlayIP so overlay traffic uses the client's overlay IP as source,
		// not the transport IP (which isn't in endpoint AllowedIPs).
		overlaySpace := c.overlaySpaceCIDR()
		if overlaySpace != "" {
			if _, overlayDest, parseErr := net.ParseCIDR(overlaySpace); parseErr != nil {
				return fmt.Errorf("parse client overlay space %q: %w", overlaySpace, parseErr)
			} else {
				var srcIP net.IP
				if c.node != nil && c.node.config.OverlayIP != "" {
					srcIP = net.ParseIP(c.node.config.OverlayIP)
				}
				if vrfTable != 0 {
					if err := router.SetECMPRouteInTable(overlayDest, nexthops, vrfTable, srcIP); err != nil {
						return fmt.Errorf("set overlay space ECMP route %s via %d nexthops table %d: %w", overlaySpace, len(nexthops), vrfTable, err)
					}
				} else {
					if err := router.SetECMPRoute(overlayDest, nexthops, srcIP); err != nil {
						return fmt.Errorf("set overlay space ECMP route %s via %d nexthops: %w", overlaySpace, len(nexthops), err)
					}
				}
			}
		} else {
			c.node.logger.Debug().
				Str("event", "ecmp_install_no_overlay").
				Str("dest", primaryCIDR).
				Msg("topology overlay.space empty or nil, installing balancerIP/32 only")
		}

		stickyCIDR := primaryCIDR
		if err := c.applyStickyECMPDiff(stickyCIDR, reason); err != nil {
			return err
		}
		if err := c.sysctlDep().EnableL4Hash(); err != nil {
			c.node.logger.Warn().Err(err).Msg("EnableL4Hash failed (non-fatal, routes already installed)")
		}

		nexthopIPs := make([]string, 0, len(nexthops))
		for _, nh := range nexthops {
			nexthopIPs = append(nexthopIPs, nh.Via)
		}
		c.node.logger.Info().
			Str("event", "ecmp_install").
			Str("dest", primaryCIDR).
			Str("nexthops", strings.Join(nexthopIPs, ",")).
			Str("cidr", stickyCIDR).
			Str("reason", reason).
			Msg("client ECMP route installed (VIP path)")
		return nil
	}

	// Legacy path: dest = 0.0.0.0/0.
	defaultDest := &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}
	defaultCIDR := "0.0.0.0/0"

	if len(healthyLinks) == 0 {
		if !ecmpInstalledSnapshot {
			c.node.logger.Info().
				Str("event", "ecmp_skip_remove_never_installed").
				Str("dest", defaultCIDR).
				Str("reason", "no_healthy_links_and_never_installed").
				Msg("preserving pre-existing default route — we never installed our own")
			return nil
		}
		c.node.logger.Info().
			Str("event", "ecmp_withdraw").
			Str("dest", defaultCIDR).
			Str("reason", "no_healthy_links").
			Msg("withdrawing client ECMP route")
		if err := router.RemoveECMPRoute(defaultDest); err != nil {
			return fmt.Errorf("remove default ECMP route: %w", err)
		}
		// Reset the installed flag under the lock — without this, a later
		// zero-link rebuild can still delete a default route this process did
		// not install (e.g. a RouterOS-injected default route that replaced
		// ours after withdraw).
		c.platformState.mu.Lock()
		c.platformState.ecmpRouteInstalled = false
		c.platformState.mu.Unlock()
		return nil
	}

	nexthops := buildNexthopsFromLinks(healthyLinks)

	// F-008 CR-002: When VRF is active, install all ECMP routes into the VRF
	// routing table (default 100) so overlay traffic stays isolated (FR-4.3).
	// Legacy path (no balancerIP) installs default route + overlay.space route.
	vrfTable := 0
	if mgr := c.platformState.vrfManager; mgr != nil {
		vrfTable = int(mgr.Table())
	}

	if vrfTable != 0 {
		if err := router.SetECMPRouteInTable(defaultDest, nexthops, vrfTable); err != nil {
			return fmt.Errorf("set default ECMP route table %d: %w", vrfTable, err)
		}
	} else {
		if err := router.SetECMPRoute(defaultDest, nexthops); err != nil {
			return fmt.Errorf("set default ECMP route: %w", err)
		}
	}
	c.platformState.mu.Lock()
	c.platformState.ecmpRouteInstalled = true
	c.platformState.mu.Unlock()

	// Overlay space is the sticky CIDR for legacy path when available.
	// Also install overlay-space ECMP route with overlayIP src so VRF picks
	// the overlay IP for outbound packets (closes F-005 PARTIAL gap #2).
	stickyCIDR := defaultCIDR
	if overlaySpace := c.overlaySpaceCIDR(); overlaySpace != "" {
		stickyCIDR = overlaySpace
		if _, overlayDest, parseErr := net.ParseCIDR(overlaySpace); parseErr == nil && overlayDest != nil {
			var srcIP net.IP
			if c.node != nil && c.node.config.OverlayIP != "" {
				srcIP = net.ParseIP(c.node.config.OverlayIP)
			}
			if vrfTable != 0 {
				if err := router.SetECMPRouteInTable(overlayDest, nexthops, vrfTable, srcIP); err != nil {
					return fmt.Errorf("set overlay space ECMP route %s table %d: %w", overlaySpace, vrfTable, err)
				}
			} else {
				if err := router.SetECMPRoute(overlayDest, nexthops, srcIP); err != nil {
					return fmt.Errorf("set overlay space ECMP route %s: %w", overlaySpace, err)
				}
			}
		}
	}
	if err := c.applyStickyECMPDiff(stickyCIDR, reason); err != nil {
		return err
	}
	if err := c.sysctlDep().EnableL4Hash(); err != nil {
		c.node.logger.Warn().Err(err).Msg("EnableL4Hash failed (non-fatal, routes already installed)")
	}

	nexthopIPs := make([]string, 0, len(nexthops))
	for _, nh := range nexthops {
		nexthopIPs = append(nexthopIPs, nh.Via)
	}
	c.node.logger.Info().
		Str("event", "ecmp_install").
		Str("dest", defaultCIDR).
		Str("nexthops", strings.Join(nexthopIPs, ",")).
		Str("cidr", stickyCIDR).
		Str("reason", reason).
		Msg("client ECMP route installed (legacy path)")
	return nil
}

// applyStickyECMPDiff computes the diff between the currently installed sticky
// CIDRs and the desired newCIDR, then calls DisableStickyECMP for each retired
// CIDR and EnableStickyECMP for newCIDR if it is not already active.
// It updates currentStickyCIDRs to {newCIDR} on success.
// If fw is nil (nftables unavailable), it is a no-op.
// reason is threaded from the rebuildClientECMP caller for structured log events (NFR-5).
//
// Concurrency note: this function does NOT hold platformState.mu across the
// firewall syscalls (they are fast, but blocking them under the state mutex
// would serialise all peer operations). Two concurrent rebuild callers can
// therefore both read the same oldCIDRs snapshot and both invoke Enable/Disable
// for the same CIDR. Both are idempotent at the nftables level: EnableStickyECMP
// appends rules using ensureTable semantics that no-op when the target rule
// already exists, and DisableStickyECMP deletes-by-match. Last-writer-wins on
// the final currentStickyCIDRs assignment, and the kernel table always ends in
// a consistent state matching whichever newCIDR was written last. Explicitly
// documented here so future readers don't add a broader lock thinking this is
// a race.
func (c *ClientRunner) applyStickyECMPDiff(newCIDR, reason string) error {
	fw := c.firewallDep()
	if fw == nil {
		return nil
	}

	c.platformState.mu.Lock()
	// Lazily initialize if struct was constructed without initClientPlatformState.
	if c.platformState.currentStickyCIDRs == nil {
		c.platformState.currentStickyCIDRs = make(map[string]bool)
	}
	oldCIDRs := make(map[string]bool, len(c.platformState.currentStickyCIDRs))
	for k, v := range c.platformState.currentStickyCIDRs {
		oldCIDRs[k] = v
	}
	c.platformState.mu.Unlock()

	// Disable CIDRs that are no longer needed.
	for cidr := range oldCIDRs {
		if cidr == newCIDR {
			continue
		}
		if err := fw.DisableStickyECMP(cidr); err != nil {
			return fmt.Errorf("disable sticky ECMP for retired CIDR %s: %w", cidr, err)
		}
		c.node.logger.Info().
			Str("event", "sticky_disable").
			Str("cidr", cidr).
			Str("reason", reason).
			Msg("sticky ECMP disabled for retired CIDR")
	}

	// Enable the new CIDR only if not already active.
	if !oldCIDRs[newCIDR] {
		if err := fw.EnableStickyECMP(newCIDR); err != nil {
			return fmt.Errorf("enable sticky ECMP for %s: %w", newCIDR, err)
		}
		c.node.logger.Info().
			Str("event", "sticky_enable").
			Str("cidr", newCIDR).
			Str("reason", reason).
			Msg("sticky ECMP enabled")
	}

	// Commit new state.
	c.platformState.mu.Lock()
	c.platformState.currentStickyCIDRs = map[string]bool{newCIDR: true}
	c.platformState.mu.Unlock()

	return nil
}

// buildNexthopsFromLinks constructs ECMP nexthops from a pre-filtered link slice.
// All links are assumed healthy and to have non-empty peerTransportIP.
func buildNexthopsFromLinks(links []*transportLink) []routing.NextHop {
	nexthops := make([]routing.NextHop, 0, len(links))
	for _, link := range links {
		nexthops = append(nexthops, routing.NextHop{
			Via:    link.peerTransportIP,
			Dev:    link.iface.Name(),
			Weight: 1,
		})
	}
	return nexthops
}

// startHealthCheck launches the healthcheck goroutine for client transport links.
func (c *ClientRunner) startHealthCheck(ctx context.Context) {
	hcCfg := HealthConfig{
		Interval:         defaultHealthInterval,
		Timeout:          defaultHealthTimeout,
		FailureThreshold: defaultHealthFailureThreshold,
	}
	hcLogger := c.node.logger.With().Str("component", "healthcheck").Logger()
	hc := NewHealthChecker(hcCfg, hcLogger, nil)

	// F-008 FR-7: when VRF active, bind ICMP socket to the VRF master device
	// so probes for transport peer IPs (10.93.0.x in VRF table 100) resolve
	// via the correct routing context. Must be set before hc.Run → Start.
	if mgr := c.platformState.vrfManager; mgr != nil {
		hc.BindToVRF(mgr.Name())
	}

	go hc.Run(ctx, c.healthTargets,
		func(name string) {
			c.setPeerHealth(name, false)
			if err := c.rebuildClientECMP("onDown"); err != nil {
				c.node.logger.Warn().Err(err).Str("peer", name).Msg("rebuildClientECMP failed on link down")
			}
		},
		func(name string) {
			c.setPeerHealth(name, true)
			if err := c.rebuildClientECMP("onUp"); err != nil {
				c.node.logger.Warn().Err(err).Str("peer", name).Msg("rebuildClientECMP failed on link up")
			}
		},
	)
}

func (c *ClientRunner) setPeerHealth(peerHex string, healthy bool) {
	trimmed := strings.TrimSpace(peerHex)
	if trimmed == "" {
		return
	}

	c.platformState.mu.Lock()
	defer c.platformState.mu.Unlock()

	existing, exists := c.platformState.byKey[trimmed]
	if !exists || existing == nil {
		return
	}

	// Explicit field-by-field copy: transportLink contains a sync.Mutex which
	// cannot be copied by value. The mu on the new instance is fresh — correct
	// under CoW semantics since readers of the old instance (holding old.mu)
	// are unrelated to readers who will discover updatedPeer via byKey.
	updatedPeer := &transportLink{
		iface:            existing.iface,
		pubkeyHex:        existing.pubkeyHex,
		localTransportIP: existing.localTransportIP,
		peerTransportIP:  existing.peerTransportIP,
		balancerIP:       existing.balancerIP,
		healthy:          healthy,
	}

	nextLinks := make([]*transportLink, 0, len(c.platformState.links))
	for _, link := range c.platformState.links {
		if link == existing {
			nextLinks = append(nextLinks, updatedPeer)
			continue
		}
		nextLinks = append(nextLinks, link)
	}

	c.platformState.byKey[trimmed] = updatedPeer
	c.platformState.links = nextLinks
}

// SetBalancerIP sets the balancer IP for a specific peer link using copy-on-write.
func (c *ClientRunner) SetBalancerIP(pubkeyHex, balancerIP string) {
	key := strings.TrimSpace(pubkeyHex)
	newIP := strings.TrimSpace(balancerIP)

	c.platformState.mu.Lock()
	old, exists := c.platformState.byKey[key]
	if exists && old != nil {
		updated := &transportLink{
			iface:            old.iface,
			pubkeyHex:        old.pubkeyHex,
			localTransportIP: old.localTransportIP,
			peerTransportIP:  old.peerTransportIP,
			balancerIP:       newIP,
			healthy:          old.healthy,
		}
		c.platformState.byKey[key] = updated
		nextLinks := make([]*transportLink, len(c.platformState.links))
		for i, link := range c.platformState.links {
			if link == old {
				nextLinks[i] = updated
			} else {
				nextLinks[i] = link
			}
		}
		c.platformState.links = nextLinks
	}
	c.platformState.mu.Unlock()
}

// healthTargets returns health check targets from all transport links.
func (c *ClientRunner) healthTargets() []HealthTarget {
	c.platformState.mu.Lock()
	defer c.platformState.mu.Unlock()

	targets := make([]HealthTarget, 0, len(c.platformState.links))
	for _, link := range c.platformState.links {
		if link == nil || link.peerTransportIP == "" {
			continue
		}
		targets = append(targets, HealthTarget{
			Name:     link.pubkeyHex,
			PingAddr: link.peerTransportIP,
			Healthy:  link.healthy,
		})
	}
	return targets
}

func (c *ClientRunner) resolveDSCPPolicies(routingPolicies []RoutingPolicyState, transportMap map[string]transportInfo) []resolvedDSCPPolicy {
	resolved := make([]resolvedDSCPPolicy, 0, len(routingPolicies))
	for _, rp := range routingPolicies {
		entry := resolvedDSCPPolicy{
			policy: routing.DSCPPolicy{
				DSCP:    rp.DSCP,
				Fwmark:  rp.DSCP,
				TableID: 100 + rp.DSCP,
			},
			name:    rp.Name,
			targets: append([]string(nil), rp.Targets...),
		}
		for _, target := range rp.Targets {
			trimmedTarget := strings.TrimSpace(target)
			if trimmedTarget == "" {
				continue
			}
			if info, ok := transportMap[trimmedTarget]; ok {
				entry.policy.Gateway = info.gateway
				entry.policy.Device = info.device
				break
			}

			resolvedViaMaster := false
			if c.node.topology != nil {
				matchedMasters := make([]string, 0)
				for _, m := range c.node.topology.Masters {
					for _, ep := range m.Endpoints {
						if ep != trimmedTarget {
							continue
						}
						resolvedViaMaster = true
						matchedMasters = append(matchedMasters, m.Name)
						if info, ok := transportMap[m.Name]; ok {
							entry.policy.Gateway = info.gateway
							entry.policy.Device = info.device
							break
						}
						break
					}
					if entry.policy.Gateway != "" {
						break
					}
				}
				if entry.policy.Gateway == "" && len(matchedMasters) > 0 {
					for _, masterName := range matchedMasters {
						entry.unresolved = append(entry.unresolved, trimmedTarget+" (via master "+masterName+")")
					}
				}
			}
			if entry.policy.Gateway != "" {
				break
			}
			if !resolvedViaMaster {
				entry.unresolved = append(entry.unresolved, trimmedTarget)
			}
		}
		resolved = append(resolved, entry)
	}
	return resolved
}

// setupDSCPRouting reads routing policies from topology (or persisted client state on restart)
// and sets up DSCP->fwmark->table policy routing. It resolves per-policy gateway/device from
// transport state so that per-table default routes are created.
func (c *ClientRunner) setupDSCPRouting() error {
	// Determine routing policy source: topology takes precedence, clientState is fallback on restart.
	var routingPolicies []RoutingPolicyState

	if c.node.topology != nil {
		client := c.node.topology.FindClient(c.node.config.Name)
		if client == nil || len(client.RoutingPolicies) == 0 {
			return nil
		}
		routingPolicies = make([]RoutingPolicyState, 0, len(client.RoutingPolicies))
		for _, rp := range client.RoutingPolicies {
			routingPolicies = append(routingPolicies, RoutingPolicyState{
				Name:    rp.Name,
				DSCP:    rp.DSCP,
				Targets: append([]string(nil), rp.Targets...),
			})
		}
	} else if c.clientState != nil && len(c.clientState.RoutingPolicies) > 0 {
		routingPolicies = c.clientState.RoutingPolicies
	} else {
		return nil
	}

	// Build transport link map: tunnel name -> (peerTransportIP, interface name).
	transportMap := make(map[string]transportInfo)

	state, stateErr := loadNodeTransportState(c.node.config.ConfigDir)
	if stateErr == nil {
		for _, tunnel := range state.Tunnels {
			trimmedKey := strings.TrimSpace(tunnel.PeerPublicKey)
			if trimmedKey == "" || strings.TrimSpace(tunnel.PeerTransportIP) == "" {
				continue
			}
			c.platformState.mu.Lock()
			link, exists := c.platformState.byKey[trimmedKey]
			c.platformState.mu.Unlock()
			if exists && link != nil && link.iface != nil {
				transportMap[tunnel.Name] = transportInfo{
					gateway: tunnel.PeerTransportIP,
					device:  link.iface.Name(),
				}
			}
		}
	}

	resolvedPolicies := c.resolveDSCPPolicies(routingPolicies, transportMap)
	policies := make([]routing.DSCPPolicy, 0, len(resolvedPolicies))
	resolvedCount := 0
	for i, rp := range resolvedPolicies {
		logEvent := c.node.logger.Info()
		if rp.policy.Gateway == "" || rp.policy.Device == "" {
			logEvent = c.node.logger.Error()
		}
		logEvent.
			Str("policy", rp.name).
			Int("dscp", rp.policy.DSCP).
			Int("table", rp.policy.TableID).
			Str("gateway", rp.policy.Gateway).
			Str("device", rp.policy.Device).
			Strs("targets", rp.targets).
			Strs("unresolved_targets", rp.unresolved).
			Int("index", i).
			Msg("DSCP routing policy configured")
		if rp.policy.Gateway == "" || rp.policy.Device == "" {
			continue
		}
		policies = append(policies, rp.policy)
		resolvedCount++
	}
	if len(resolvedPolicies) > 0 && resolvedCount == 0 {
		return fmt.Errorf("DSCP routing: no configured policies resolved to transport targets")
	}

	return routing.SetupDSCPPolicyRouting(policies)
}

// SaveClientState persists current client configuration to disk for restart recovery.
// Implements grpcserver.ClientStateSaver. Safe to call when topology is nil (returns nil).
func (c *ClientRunner) overlaySpaceCIDR() string {
	if c == nil {
		return ""
	}
	if c.node != nil && c.node.topology != nil {
		if overlaySpace := strings.TrimSpace(c.node.topology.Overlay.Space); overlaySpace != "" {
			return overlaySpace
		}
	}
	if c.clientState != nil {
		return strings.TrimSpace(c.clientState.OverlaySpace)
	}
	return ""
}

func (c *ClientRunner) SaveClientState() error {
	if c == nil || c.node == nil {
		return nil
	}
	if c.node.topology == nil {
		return nil
	}

	client := c.node.topology.FindClient(c.node.config.Name)
	if client == nil {
		return nil
	}

	routingPolicies := make([]RoutingPolicyState, 0, len(client.RoutingPolicies))
	for _, rp := range client.RoutingPolicies {
		routingPolicies = append(routingPolicies, RoutingPolicyState{
			Name:    rp.Name,
			DSCP:    rp.DSCP,
			Targets: append([]string(nil), rp.Targets...),
		})
	}

	masters := make([]NodeRef, 0, len(c.node.topology.Masters))
	for _, m := range c.node.topology.Masters {
		masters = append(masters, NodeRef{Name: m.Name, OverlayIP: m.OverlayIP})
	}

	endpoints := make([]NodeRef, 0, len(c.node.topology.Endpoints))
	for _, ep := range c.node.topology.Endpoints {
		endpoints = append(endpoints, NodeRef{Name: ep.Name, OverlayIP: ep.OverlayIP})
	}

	var dnsState *DNSState
	if client.DNS != nil {
		dnsState = &DNSState{
			Zone:     client.DNS.Zone,
			Listen:   client.DNS.Listen,
			Upstream: client.DNS.Upstream,
		}
	}

	state := ClientState{
		OverlayIP:       c.node.config.OverlayIP,
		OverlaySpace:    c.overlaySpaceCIDR(),
		RoutingPolicies: routingPolicies,
		DNS:             dnsState,
		Masters:         masters,
		Endpoints:       endpoints,
	}

	if err := saveClientState(c.node.config.ConfigDir, state); err != nil {
		return fmt.Errorf("save client state: %w", err)
	}
	c.node.logger.Info().Str("overlay_ip", state.OverlayIP).Msg("client state saved")
	return nil
}

// startDNSServer starts the embedded DNS server if DNS is configured in the topology or
// persisted client state. No-op if no DNS configuration is available.
func (c *ClientRunner) startDNSServer(ctx context.Context) {
	var dnsZone, dnsListen, dnsUpstream string
	nodeMap := make(map[string]string) // name -> overlayIP

	if c.node.topology != nil {
		client := c.node.topology.FindClient(c.node.config.Name)
		if client != nil && client.DNS != nil {
			dnsZone = client.DNS.Zone
			dnsListen = client.DNS.Listen
			dnsUpstream = client.DNS.Upstream
		}
		if strings.TrimSpace(dnsZone) != "" {
			for _, m := range c.node.topology.Masters {
				if m.OverlayIP != "" {
					nodeMap[m.Name] = m.OverlayIP
				}
			}
			for _, ep := range c.node.topology.Endpoints {
				if ep.OverlayIP != "" {
					nodeMap[ep.Name] = ep.OverlayIP
				}
			}
		}
	} else if c.clientState != nil && c.clientState.DNS != nil {
		dnsZone = c.clientState.DNS.Zone
		dnsListen = c.clientState.DNS.Listen
		dnsUpstream = c.clientState.DNS.Upstream
		if strings.TrimSpace(dnsZone) != "" {
			for _, ref := range c.clientState.Masters {
				if ref.OverlayIP != "" {
					nodeMap[ref.Name] = ref.OverlayIP
				}
			}
			for _, ref := range c.clientState.Endpoints {
				if ref.OverlayIP != "" {
					nodeMap[ref.Name] = ref.OverlayIP
				}
			}
		}
	}

	if strings.TrimSpace(dnsZone) == "" {
		return
	}

	records := dns.BuildZoneRecords(dnsZone, nodeMap)
	server := dns.NewServer(dnsZone, dnsListen, dnsUpstream, records)
	if server == nil {
		return
	}

	c.node.logger.Info().Str("zone", dnsZone).Str("listen", dnsListen).Msg("starting DNS server")

	go func() {
		if err := server.Start(ctx); err != nil {
			c.node.logger.Warn().Err(err).Msg("DNS server stopped")
		}
	}()
}

// teardownDSCPRouting removes DSCP policy routing rules and nftables table on shutdown.
func (c *ClientRunner) teardownDSCPRouting() {
	if err := routing.TeardownDSCPPolicyRouting(); err != nil {
		c.node.logger.Warn().Err(err).Msg("teardown DSCP policy routing failed")
	}
}

func extractPeerEndpoint(endpointHost string) string {
	trimmedEndpointHost := strings.TrimSpace(endpointHost)
	if trimmedEndpointHost == "" {
		return ""
	}

	if strings.Contains(trimmedEndpointHost, "|") {
		parts := strings.SplitN(trimmedEndpointHost, "|", 2)
		if len(parts) == 2 {
			return strings.TrimSpace(parts[1])
		}
	}

	return trimmedEndpointHost
}
