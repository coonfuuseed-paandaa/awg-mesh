package node

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	grpcserver "github.com/coonfuuseed-paandaa/awg-mesh/pkg/grpc"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/routing"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/transport"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
	"github.com/rs/zerolog"
)

// ErrTunnelNotFound is returned by UpdateTunnelPeer when the named tunnel
// does not exist in the master's live tunnel map.
var ErrTunnelNotFound = errors.New("tunnel not found")

// MasterTunnel represents a single tunnel managed by master mode.
type MasterTunnel struct {
	Name                string
	InterfaceName       string
	EndpointHost        string
	OverlayIP           string
	BalancerIP          string
	TransportSubnet     string
	MasterTransportIP   string
	EndpointTransportIP string
	PeerPublicKey       wg.Key
	Healthy             bool
	Weight              int
	AllowedIPs          []string // admin source of truth; set by AddTunnel/UpdateTunnelPeer, persisted verbatim
	lastParams          wg.Config
	platformState       masterTunnelPlatformState
}

// PacketForwarder abstracts eBPF-based inter-interface forwarding.
// Implemented by pkg/ebpf.Forwarder on Linux, nil on other platforms.
type PacketForwarder interface {
	SetRoute(overlayIP net.IP, ifindex int) error
	DeleteRoute(overlayIP net.IP) error
	Attach(ifaceName string) error
	Detach(ifaceName string) error
	Close() error
}

// MasterRunner runs node logic for master mode.
type MasterRunner struct {
	node      *Node
	tunnels   map[string]*MasterTunnel
	mu        sync.RWMutex
	startTime time.Time
	forwarder PacketForwarder // nil if eBPF unavailable (graceful degradation)

	// stateMu serializes saveTransportState calls so that concurrent
	// AddTunnel / UpdateTunnelPeer invocations do not race the
	// load+modify+atomic-rename sequence on transport.yml. Without this,
	// two parallel AddTunnel calls (e.g. `mesh-ctl client init` for two
	// clients in parallel) can ENOENT on os.Rename(.tmp -> .yml) because
	// one goroutine deletes the temp file the other just wrote. Separate
	// from m.mu (which guards the in-memory tunnels map) so saveTransportState
	// does not block UpdateTunnelPeer's full-RPC critical section. local
	// tracker issue: F-005 dpext gap-1 (concurrent saveTransportState race).
	stateMu sync.Mutex

	// applyPeerKeyUpdateFn is a test seam: when non-nil it replaces the
	// platform-specific applyPeerKeyUpdate method. Tests inject a stub here to
	// exercise the DifferentKey and ApplyFails paths without wgctrl.
	// Production code never sets this field.
	applyPeerKeyUpdateFn func(tunnel *MasterTunnel, newPubkey wg.Key, allowedIPs []string) error
}

// NewMasterRunner creates a master mode runner.
func NewMasterRunner(node *Node) *MasterRunner {
	return &MasterRunner{
		node:    node,
		tunnels: make(map[string]*MasterTunnel),
	}
}

// Run starts master mode and blocks until context cancellation.
func (m *MasterRunner) Run(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	if m == nil || m.node == nil {
		return fmt.Errorf("master runner node is required")
	}

	_, publicKey, err := EnsureKeypair(m.node.config.ConfigDir)
	if err != nil {
		return fmt.Errorf("ensure keypair: %w", err)
	}
	if m.node.config.OverlayIP != "" {
		if err := AssignOverlayIP(m.node.config.OverlayIP); err != nil {
			return fmt.Errorf("assign overlay IP: %w", err)
		}
	}
	// Self-heal: migrate legacy transport.yml entries that are missing AllowedIPs.
	// Must run before startGRPCServer so the repaired state is visible to the
	// tunnel-restore loop that follows immediately after.
	if migrateErr := migrateLegacyTransportState(m.node.config.ConfigDir, m.node.topology, m.node.logger); migrateErr != nil {
		m.node.logger.Error().Err(migrateErr).Msg("transport state migration failed; continuing with existing state")
	}

	scheduler := newCaptureScheduler(m.node.logger, newCaptureFunc())
	if err := startGRPCServer(ctx, m.node.config.ConfigDir, m.node.logger, m, m, nil, m, scheduler, m, nil); err != nil {
		return fmt.Errorf("start gRPC server: %w", err)
	}
	m.startTime = time.Now()

	defer func() {
		scheduler.StopSchedule()
		if closeErr := m.closeAllTunnelInterfaces(); closeErr != nil {
			m.node.logger.Warn().Err(closeErr).Msg("failed to close master tunnel interfaces")
		}
	}()

	m.node.logger.Info().
		Str("overlay_ip", m.node.config.OverlayIP).
		Str("public_key", publicKey.String()).
		Msg("master runner started")
	m.node.logger.Info().Str("wan_interface", discoverWANInterface()).Msg("WAN interface discovered")

	if state, err := loadNodeTransportState(m.node.config.ConfigDir); err == nil && len(state.Tunnels) > 0 {
		reconciled := 0
		for _, tt := range state.Tunnels {
			peerKey := wg.Key{}
			if tt.PeerPublicKey != "" {
				if parsedKey, parseErr := wg.ParseKey(tt.PeerPublicKey); parseErr == nil {
					peerKey = parsedKey
				}
			}

			// Derive transport subnet from the stored transport IP so saveTransportState
			// preserves AllowedIPs correctly (including entries migrated by
			// migrateLegacyTransportState). Passing "" here caused the just-migrated
			// AllowedIPs to be overwritten with nil on the same boot (CRIT fix).
			restoredSubnet := computeTransportSubnetFromIP(tt.TransportIP)
			if err := m.AddTunnel(tt.Name, tt.PeerEndpoint, tt.OverlayIP, tt.BalancerIP, restoredSubnet, tt.TransportIP, tt.PeerTransportIP, 1, peerKey, tt.AllowedIPs); err != nil {
				if !strings.Contains(err.Error(), "already exists") {
					m.node.logger.Warn().
						Str("tunnel", tt.Name).
						Err(err).
						Msg("reconcile tunnel failed")
				}
			} else {
				reconciled++
			}
		}

		m.node.logger.Info().
			Int("tunnels", reconciled).
			Msg("reconciled tunnels from saved state")
	}

	if m.node.topology != nil {
		for _, ep := range m.node.topology.Endpoints {
			if addErr := m.AddTunnel(ep.Name, ep.Host, ep.OverlayIP, "", "", "", "", 1, wg.Key{}, nil); addErr != nil {
				m.node.logger.Warn().
					Str("endpoint", ep.Name).
					Err(addErr).
					Msg("failed to add tunnel")
			}
		}
	}

	if err := m.setupDSCPRouting(); err != nil {
		m.node.logger.Warn().Err(err).Msg("setup master DSCP policy routing failed (non-fatal)")
	}
	if err := m.setupExitMode(); err != nil {
		m.node.logger.Warn().Err(err).Msg("setup master exit mode failed (non-fatal)")
	}
	if err := m.enableWGCrossTunnelForward(); err != nil {
		m.node.logger.Warn().Err(err).Msg("master: failed to enable wg-+ cross-tunnel FORWARD ACCEPT (non-fatal); endpoint↔endpoint overlay may be dropped on hosts with DROP FORWARD policy")
	}

	hcCfg := HealthConfig{
		Interval:         defaultHealthInterval,
		Timeout:          defaultHealthTimeout,
		FailureThreshold: defaultHealthFailureThreshold,
	}
	hcLogger := m.node.logger.With().Str("component", "healthcheck").Logger()
	hc := NewHealthChecker(hcCfg, hcLogger, m.masterHandshakeChecker())

	go hc.Run(ctx, m.healthTargets,
		func(name string) {
			m.mu.Lock()
			t, ok := m.tunnels[name]
			var overlayIP, balancerIP, tunnelName string
			if ok {
				t.Healthy = false
				overlayIP = t.OverlayIP
				balancerIP = t.BalancerIP
				tunnelName = t.Name
			}
			m.mu.Unlock()

			if ok {
				m.removeOverlayRoute(overlayIP)
				m.rebuildECMP(balancerIP)
				m.node.logger.Info().
					Str("tunnel", tunnelName).
					Str("overlay_ip", overlayIP).
					Str("balancer_ip", balancerIP).
					Msg("tunnel down, overlay route removed, ECMP rebuilt")
			}
		},
		func(name string) {
			m.mu.Lock()
			t, ok := m.tunnels[name]
			var overlayIP, balancerIP, tunnelName, epTransportIP, ifaceName string
			if ok {
				t.Healthy = true
				overlayIP = t.OverlayIP
				balancerIP = t.BalancerIP
				tunnelName = t.Name
				epTransportIP = t.EndpointTransportIP
				ifaceName = t.InterfaceName
			}
			m.mu.Unlock()

			if ok {
				m.restoreOverlayRoute(overlayIP, epTransportIP, ifaceName)
				m.rebuildECMP(balancerIP)
				m.node.logger.Info().
					Str("tunnel", tunnelName).
					Str("overlay_ip", overlayIP).
					Str("balancer_ip", balancerIP).
					Msg("tunnel recovered, overlay route restored, ECMP rebuilt")
			}
		},
	)

	<-ctx.Done()

	m.node.logger.Info().Msg("master runner stopping")
	return nil
}

// GetPublicKey returns the master's WireGuard public key.
func (m *MasterRunner) GetPublicKey() (wg.Key, error) {
	_, pubKey, err := EnsureKeypair(m.node.config.ConfigDir)
	return pubKey, err
}

// AddTunnel adds a new tunnel to the managed set.
func (m *MasterRunner) AddTunnel(name, endpointHost, overlayIP, balancerIP, transportSubnet, masterTransportIP, endpointTransportIP string, weight int, peerPublicKey wg.Key, allowedIPs []string) error {
	if name == "" {
		return fmt.Errorf("tunnel name is required")
	}

	m.mu.Lock()
	if _, exists := m.tunnels[name]; exists {
		m.mu.Unlock()
		return fmt.Errorf("tunnel %q already exists", name)
	}

	t := &MasterTunnel{
		Name:                name,
		InterfaceName:       "wg-" + name,
		EndpointHost:        endpointHost,
		OverlayIP:           overlayIP,
		BalancerIP:          balancerIP,
		TransportSubnet:     strings.TrimSpace(transportSubnet),
		MasterTransportIP:   strings.TrimSpace(masterTransportIP),
		EndpointTransportIP: strings.TrimSpace(endpointTransportIP),
		PeerPublicKey:       peerPublicKey,
		Healthy:             false, // Set true only after interface creation succeeds (prevents ECMP race)
		Weight:              weight,
		AllowedIPs:          allowedIPs,
	}
	m.tunnels[name] = t
	m.mu.Unlock()

	if err := m.createTunnelInterface(t, endpointHost); err != nil {
		m.mu.Lock()
		currentTunnel, exists := m.tunnels[name]
		if exists && currentTunnel == t {
			delete(m.tunnels, name)
		}
		m.mu.Unlock()
		return fmt.Errorf("create tunnel interface for %q: %w", name, err)
	}

	// Interface created successfully - mark healthy so ECMP includes this tunnel.
	m.mu.Lock()
	if currentTunnel, exists := m.tunnels[name]; exists && currentTunnel == t {
		t.Healthy = true
	}
	m.mu.Unlock()

	// Wire eBPF forwarding: add overlay IP -> interface index mapping.
	if m.forwarder != nil && overlayIP != "" {
		if overlayAddr := net.ParseIP(overlayIP); overlayAddr != nil {
			ifindex, _ := routing.NewNetlinkRouter().LinkGetIndex(t.InterfaceName)
			if ifindex > 0 {
				if fwdErr := m.forwarder.SetRoute(overlayAddr, ifindex); fwdErr != nil {
					m.node.logger.Warn().Err(fwdErr).Str("tunnel", name).Msg("ebpf: failed to set forwarding route")
				} else {
					m.node.logger.Debug().Str("tunnel", name).Str("overlay_ip", overlayIP).Int("ifindex", ifindex).Msg("ebpf: forwarding route set")
				}
			}
		}
		// Attach TC program to the tunnel interface.
		if attachErr := m.forwarder.Attach(t.InterfaceName); attachErr != nil {
			m.node.logger.Warn().Err(attachErr).Str("tunnel", name).Msg("ebpf: failed to attach TC")
		}
	}

	if err := m.saveTransportState(t); err != nil {
		m.mu.Lock()
		currentTunnel, exists := m.tunnels[name]
		if exists && currentTunnel == t {
			delete(m.tunnels, name)
		}
		m.mu.Unlock()
		if closeErr := m.closeTunnelInterface(t); closeErr != nil {
			return fmt.Errorf("save transport state for %q: %w (also failed to close interface: %v)", name, err, closeErr)
		}
		return fmt.Errorf("save transport state for %q: %w", name, err)
	}

	m.node.logger.Info().
		Str("tunnel", name).
		Str("endpoint_host", endpointHost).
		Str("overlay_ip", overlayIP).
		Str("interface", t.InterfaceName).
		Msg("tunnel added")

	return nil
}

// UpdateTunnelPeer replaces the peer public key on a named tunnel. Implements TunnelManager.
// C3 strict rollback ordering (NON-NEGOTIABLE):
// 1. Lookup tunnel - not found -> (false, ErrTunnelNotFound)
// 2. Same-key check -> (true, nil) no UAPI call, no persist
// 3. Capture old key for rollback
// 4. Apply new key via UAPI (applyPeerKeyUpdate - platform-specific)
// 5. UAPI fail -> restore old key in-memory, no persist, return error
// 6. UAPI success -> update in-memory state
// 7. Persist - fail -> log Warn with attempt count, return (false, nil) (UAPI authoritative)
func (m *MasterRunner) UpdateTunnelPeer(name string, newPubkeyBytes [32]byte, balancerIP string, allowedIPs []string) (unchanged bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Step 1: lookup tunnel by name
	tunnel, exists := m.tunnels[name]
	if !exists {
		return false, ErrTunnelNotFound
	}

	var newPubkey wg.Key
	copy(newPubkey[:], newPubkeyBytes[:])

	// Step 2: same-key idempotency check - only skip when there is no other
	// state to refresh. Reload/reconcile may send AllowedIPs and balancer state
	// for an already-matching key.
	if tunnel.PeerPublicKey == newPubkey && len(allowedIPs) == 0 && strings.TrimSpace(balancerIP) == "" {
		return true, nil
	}

	// Step 3: capture old key for rollback
	oldPubkey := tunnel.PeerPublicKey

	// Step 4: apply new key via UAPI (platform-specific implementation in master_linux.go).
	// applyPeerKeyUpdateFn is a test seam: when non-nil it overrides the method.
	applyFn := m.applyPeerKeyUpdate
	if m.applyPeerKeyUpdateFn != nil {
		applyFn = m.applyPeerKeyUpdateFn
	}
	if applyErr := applyFn(tunnel, newPubkey, allowedIPs); applyErr != nil {
		// Step 5: UAPI error -> restore in-memory old key, NO disk write
		tunnel.PeerPublicKey = oldPubkey
		return false, fmt.Errorf("wgctrl peer-replace: %w", applyErr)
	}

	// Step 6: UAPI success -> update in-memory state
	tunnel.PeerPublicKey = newPubkey
	if balancerIP != "" {
		tunnel.BalancerIP = balancerIP
	}
	if len(allowedIPs) > 0 {
		tunnel.AllowedIPs = allowedIPs // refresh admin intent on every reload/reconcile call
	}

	// Step 7: persist - failure is non-fatal (UAPI is authoritative)
	if persistErr := m.saveTransportState(tunnel); persistErr != nil {
		m.node.logger.Warn().
			Err(persistErr).
			Str("tunnel", name).
			Int("attempt", 1).
			Msg("UpdateTunnelPeer: persist transport state failed (UAPI applied, data plane is correct)")
	}

	return false, nil
}

// RemoveTunnel removes a tunnel by name.
func (m *MasterRunner) RemoveTunnel(name string) error {
	m.mu.Lock()
	tunnel, exists := m.tunnels[name]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("tunnel %q not found", name)
	}
	delete(m.tunnels, name)
	m.mu.Unlock()

	// Clean up eBPF forwarding before removing routes.
	if m.forwarder != nil && tunnel.OverlayIP != "" {
		if overlayAddr := net.ParseIP(tunnel.OverlayIP); overlayAddr != nil {
			_ = m.forwarder.DeleteRoute(overlayAddr)
		}
		_ = m.forwarder.Detach(tunnel.InterfaceName)
	}

	// Clean up routing state before closing the interface.
	m.removeOverlayRoute(tunnel.OverlayIP)
	if tunnel.BalancerIP != "" {
		m.rebuildECMP(tunnel.BalancerIP)
	}

	if err := m.closeTunnelInterface(tunnel); err != nil {
		return fmt.Errorf("close tunnel interface for %q: %w", name, err)
	}

	m.node.logger.Info().
		Str("tunnel", name).
		Msg("tunnel removed")

	return nil
}

// ListTunnels returns a snapshot of all managed tunnels.
func (m *MasterRunner) listMasterTunnels() []MasterTunnel {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]MasterTunnel, 0, len(m.tunnels))
	for _, t := range m.tunnels {
		result = append(result, *t)
	}
	return result
}

func (m *MasterRunner) healthTargets() []HealthTarget {
	tunnels := m.listMasterTunnels()
	UpdateTunnelMetrics(tunnels)
	targets := make([]HealthTarget, 0, len(tunnels))
	for _, t := range tunnels {
		pingAddr := t.EndpointTransportIP
		if pingAddr == "" {
			pingAddr = t.OverlayIP
		}
		targets = append(targets, HealthTarget{
			Name:          t.Name,
			PingAddr:      pingAddr,
			Healthy:       t.Healthy,
			PeerPublicKey: t.PeerPublicKey,
		})
	}
	return targets
}

func (m *MasterRunner) ListTunnels() []grpcserver.TunnelInfo {
	tunnels := m.listMasterTunnels()
	infos := make([]grpcserver.TunnelInfo, 0, len(tunnels))
	for _, tunnel := range tunnels {
		infos = append(infos, grpcserver.TunnelInfo{
			Name:          tunnel.Name,
			OverlayIP:     tunnel.OverlayIP,
			Healthy:       tunnel.Healthy,
			Weight:        tunnel.Weight,
			PeerPublicKey: append([]byte(nil), tunnel.PeerPublicKey[:]...),
		})
	}
	return infos
}

func (m *MasterRunner) GetNodeState() grpcserver.NodeState {
	tunnels := m.listMasterTunnels()
	infos := make([]grpcserver.TunnelInfo, 0, len(tunnels))
	for _, tunnel := range tunnels {
		infos = append(infos, grpcserver.TunnelInfo{
			Name:          tunnel.Name,
			OverlayIP:     tunnel.OverlayIP,
			Healthy:       tunnel.Healthy,
			Weight:        tunnel.Weight,
			PeerPublicKey: append([]byte(nil), tunnel.PeerPublicKey[:]...),
		})
	}
	return grpcserver.NodeState{
		Name:      m.node.config.Name,
		Mode:      "master",
		OverlayIP: m.node.config.OverlayIP,
		Tunnels:   infos,
		StartTime: m.startTime,
	}
}

func (m *MasterRunner) closeAllTunnelInterfaces() error {
	tunnels := m.listTunnelPointers()
	closeErrors := make([]string, 0, len(tunnels))
	for _, tunnel := range tunnels {
		if err := m.closeTunnelInterface(tunnel); err != nil {
			closeErrors = append(closeErrors, fmt.Sprintf("%s: %v", tunnel.Name, err))
		}
	}

	if len(closeErrors) == 0 {
		return nil
	}

	return fmt.Errorf("close tunnel interfaces: %s", strings.Join(closeErrors, "; "))
}

func (m *MasterRunner) listTunnelPointers() []*MasterTunnel {
	m.mu.RLock()
	defer m.mu.RUnlock()

	tunnels := make([]*MasterTunnel, 0, len(m.tunnels))
	for _, tunnel := range m.tunnels {
		tunnels = append(tunnels, tunnel)
	}
	return tunnels
}

func (m *MasterRunner) saveTransportState(tunnel *MasterTunnel) error {
	if m == nil || m.node == nil {
		return fmt.Errorf("master runner node is required")
	}
	if tunnel == nil {
		return fmt.Errorf("master tunnel is required")
	}

	// Serialize the load+modify+atomic-rename sequence so concurrent
	// AddTunnel/UpdateTunnelPeer do not race os.Rename on transport.yml.tmp.
	m.stateMu.Lock()
	defer m.stateMu.Unlock()

	state, err := loadNodeTransportState(m.node.config.ConfigDir)
	if err != nil {
		return fmt.Errorf("load node transport state: %w", err)
	}

	peerPublicKey := hex.EncodeToString(tunnel.PeerPublicKey[:])
	if tunnel.PeerPublicKey.IsZero() {
		peerPublicKey = ""
	}

	// FR-4 (issue #147 layer 3): persist AllowedIPs verbatim from admin intent when set.
	// tunnel.AllowedIPs is populated by AddTunnel (from gRPC req.AllowedIps) and
	// UpdateTunnelPeer (from reload/reconcile). On production masters that start without
	// --topology, m.node.topology is nil, so computeMasterPeerAllowedIPs returns minimal
	// [transport /32] — overwriting the admin-provided /27. Verbatim path eliminates that.
	// Fallback to local recompute only for legacy first-boot/migration (empty field).
	var allowedIPs []string
	if len(tunnel.AllowedIPs) > 0 {
		allowedIPs = tunnel.AllowedIPs // admin intent — persisted verbatim
	} else {
		// fallback: legacy first-boot or migration path where admin intent is unknown
		allowedIPs = computeMasterPeerAllowedIPs(m.node.topology, tunnel.Name, tunnel.TransportSubnet, tunnel.OverlayIP)
	}

	next := append(make([]TunnelTransport, 0, len(state.Tunnels)+1),
		state.Tunnels...)
	updated := false
	entry := TunnelTransport{
		Name:            tunnel.Name,
		OverlayIP:       tunnel.OverlayIP,
		TransportIP:     tunnel.MasterTransportIP,
		PeerTransportIP: tunnel.EndpointTransportIP,
		PeerPublicKey:   peerPublicKey,
		PeerEndpoint:    tunnel.EndpointHost,
		BalancerIP:      tunnel.BalancerIP,
		AllowedIPs:      allowedIPs,
	}
	for idx, existing := range next {
		if existing.Name == tunnel.Name {
			next[idx] = entry
			updated = true
			break
		}
	}
	if !updated {
		next = append(next, entry)
	}

	return saveNodeTransportState(m.node.config.ConfigDir, NodeTransportState{
		SchemaVersion: transport.CurrentSchemaVersion,
		OverlayIP:     strings.TrimSpace(m.node.config.OverlayIP),
		Tunnels:       next,
	})
}

// computeMasterPeerAllowedIPs returns the AllowedIPs a master persists for one
// endpoint peer — mirrors the platform-specific buildPeerAllowedIPs used for
// live UAPI peer configuration: [transport_subnet, overlay_ip/32]. Returns nil
// when either input is empty so we do not produce a partial entry that would
// mask a real misconfiguration.
func computeMasterPeerAllowedIPs(topo *topology.Topology, endpointName, transportSubnet, overlayIP string) []string {
	ts := strings.TrimSpace(transportSubnet)
	oi := strings.TrimSpace(overlayIP)
	if ts == "" || oi == "" {
		return nil
	}

	allowedIPs, err := topology.BuildAllowedIPsForMasterPeer(topo, endpointName, oi, ts)
	if err != nil {
		return nil
	}
	return allowedIPs
}

// computeTransportSubnetFromIP derives the /30 subnet CIDR from a transport IP.
// For example, "10.255.0.1" → "10.255.0.0/30", "10.255.0.2" → "10.255.0.0/30".
// Returns an empty string when ip is empty, not parseable, or not IPv4.
func computeTransportSubnetFromIP(ip string) string {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return ""
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ""
	}
	// Transport subnets are IPv4-only /30s; reject IPv6 to avoid the
	// "<nil>" string net.IPNet produces when Mask length disagrees with
	// the parsed IP size.
	ipv4 := parsed.To4()
	if ipv4 == nil {
		return ""
	}
	// Mask to /30 (standard transport subnet size for awg-mesh point-to-point links).
	mask := net.CIDRMask(30, 32)
	network := &net.IPNet{IP: ipv4.Mask(mask), Mask: mask}
	return network.String()
}

// migrateLegacyTransportState inspects transport.yml and back-fills AllowedIPs
// for tunnels that were persisted before v1.12.3 without that field. This covers
// the Pattern X regression: master transport.yml entries written by v1.12.2 have
// empty AllowedIPs because saveTransportState only populated the field starting
// from when AddTunnel was called with a non-empty TransportSubnet — but the
// tunnel-restore path on restart did not re-derive it.
//
// Migration is idempotent: if all tunnels already have AllowedIPs, the function
// returns nil without writing the file.
func migrateLegacyTransportState(configDir string, topo *topology.Topology, logger zerolog.Logger) error {
	state, err := loadNodeTransportState(configDir)
	if err != nil {
		return fmt.Errorf("load transport state: %w", err)
	}
	if len(state.Tunnels) == 0 {
		return nil
	}

	modified := false
	migratedCount := 0
	for i := range state.Tunnels {
		tt := &state.Tunnels[i]
		if len(tt.AllowedIPs) > 0 {
			// Already populated — skip.
			continue
		}
		if strings.TrimSpace(tt.PeerTransportIP) == "" {
			// Insufficient context to recompute: no peer transport IP means we
			// cannot derive the transport subnet reliably.
			continue
		}
		// Derive transport subnet from the master's own transport IP (TransportIP).
		transportSubnet := computeTransportSubnetFromIP(tt.TransportIP)
		if transportSubnet == "" {
			// TransportIP unparseable — skip without modifying.
			continue
		}
		// OverlayIP on the tunnel entry is the endpoint's overlay IP.
		computed := computeMasterPeerAllowedIPs(topo, tt.Name, transportSubnet, tt.OverlayIP)
		if len(computed) == 0 {
			continue
		}
		tt.AllowedIPs = computed
		modified = true
		migratedCount++
	}

	if !modified {
		return nil
	}

	if err := saveNodeTransportState(configDir, state); err != nil {
		return fmt.Errorf("save migrated transport state: %w", err)
	}

	logger.Info().
		Int("tunnel_count", migratedCount).
		Str("event", "transport_state_migrated").
		Msg("legacy transport.yml entries back-filled with AllowedIPs")

	return nil
}
