package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/control_plane"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/role"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/wg"
)

// MasterConfig describes the v2.0 master role runtime.
type MasterConfig struct {
	Name             string
	OverlayIP        string
	MeshEndpointHost string
	DualListener     wg.DualListenerConfig
	Coordination     *MasterCoordinationConfig
	LinkConfigurator MasterLinkConfigurator
}

// MasterLinkConfigurator applies OS-level state for master WireGuard devices.
type MasterLinkConfigurator interface {
	AddrAdd(ifaceName string, addr *net.IPNet) error
	LinkSetUp(ifaceName string) error
	RouteReplaceLinkWithSrc(dest *net.IPNet, ifaceName string, src net.IP) error
}

// MasterStatus is the observable runtime state of the master protocol bridge.
type MasterStatus struct {
	Name             string
	OverlayIP        string
	MeshEndpointHost string
	Listeners        wg.DualListenerSnapshot
	Coordination     MasterCoordinationSnapshot
}

// Master runs the vanilla-WG client listener and AmneziaWG mesh listener.
type Master struct {
	cfg            MasterConfig
	overlayAddress *net.IPNet
	listener       *wg.DualListener
	coordination   *MasterCoordination
}

// NewMaster validates config and builds the master runtime.
func NewMaster(cfg MasterConfig) (*Master, error) {
	if strings.TrimSpace(cfg.Name) == "" {
		return nil, errors.New("master name is required")
	}
	if strings.TrimSpace(cfg.OverlayIP) == "" {
		return nil, errors.New("master overlay IP is required")
	}
	overlayAddr, err := parseMasterOverlayAddress(cfg.OverlayIP)
	if err != nil {
		return nil, fmt.Errorf("parse master overlay IP %q: %w", cfg.OverlayIP, err)
	}
	cfg.OverlayIP = overlayAddr.IP.String()
	cfg.MeshEndpointHost = strings.TrimSpace(cfg.MeshEndpointHost)
	if cfg.Coordination != nil {
		if err := validateAdvertisedMeshEndpoint(cfg.MeshEndpointHost); err != nil {
			return nil, err
		}
	}
	if cfg.LinkConfigurator == nil {
		return nil, errors.New("master link configurator is required")
	}
	listenerCfg := cfg.DualListener
	defaults := wg.DefaultDualListenerConfig()
	if listenerCfg.VanillaFactory == nil {
		listenerCfg.VanillaFactory = defaults.VanillaFactory
	}
	if listenerCfg.AWGFactory == nil {
		listenerCfg.AWGFactory = defaults.AWGFactory
	}
	listener, err := wg.NewDualListener(listenerCfg)
	if err != nil {
		return nil, fmt.Errorf("build master dual listener: %w", err)
	}
	var coordination *MasterCoordination
	if cfg.Coordination != nil {
		coordinationCfg := cloneMasterCoordinationConfig(*cfg.Coordination)
		coordinationCfg.RegistrationObserver = func(node control_plane.RegisteredNode) error {
			return applyRegisteredMeshPeer(listener, cfg.Name, cfg.LinkConfigurator, overlayAddr, node)
		}
		coordination, err = NewMasterCoordination(coordinationCfg)
		if err != nil {
			return nil, fmt.Errorf("build master coordination: %w", err)
		}
		cfg.Coordination = &coordinationCfg
	}
	cfg.DualListener = listenerCfg
	return &Master{cfg: cfg, overlayAddress: overlayAddr, listener: listener, coordination: coordination}, nil
}

// Run starts both master listeners and blocks until context cancellation.
func (m *Master) Run(ctx context.Context) error {
	if m == nil {
		return errors.New("master is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := m.listener.Start(ctx); err != nil {
		return err
	}
	if err := m.configureLinks(); err != nil {
		closeErr := m.listener.Close()
		return errors.Join(fmt.Errorf("configure master links: %w", err), closeErr)
	}
	var coordinationDone <-chan error
	if m.coordination != nil {
		if err := m.selfRegisterCoordination(); err != nil {
			closeErr := m.listener.Close()
			return errors.Join(fmt.Errorf("self-register master in coordination: %w", err), closeErr)
		}
		if err := m.coordination.Start(ctx); err != nil {
			closeErr := m.listener.Close()
			return errors.Join(fmt.Errorf("start master coordination: %w", err), closeErr)
		}
		coordinationDone = m.coordination.Done()
	}
	select {
	case <-ctx.Done():
	case err := <-coordinationDone:
		if err == nil && ctx.Err() != nil {
			break
		}
		if err != nil {
			closeErr := m.Close()
			return errors.Join(fmt.Errorf("master coordination stopped: %w", err), closeErr)
		}
		closeErr := m.Close()
		return errors.Join(errors.New("master coordination stopped unexpectedly"), closeErr)
	}
	if err := m.Close(); err != nil {
		return err
	}
	return nil
}

func (m *Master) configureLinks() error {
	snapshot := m.listener.Snapshot()
	if err := m.cfg.LinkConfigurator.AddrAdd(snapshot.MeshInterfaceName, m.overlayAddress); err != nil {
		return fmt.Errorf("assign mesh overlay address: %w", err)
	}
	if err := m.cfg.LinkConfigurator.LinkSetUp(snapshot.MeshInterfaceName); err != nil {
		return fmt.Errorf("bring mesh interface up: %w", err)
	}
	if err := m.cfg.LinkConfigurator.LinkSetUp(snapshot.ClientInterfaceName); err != nil {
		return fmt.Errorf("bring client interface up: %w", err)
	}
	return nil
}

func applyRegisteredMeshPeer(listener *wg.DualListener, masterName string, link MasterLinkConfigurator, localOverlay *net.IPNet, node control_plane.RegisteredNode) error {
	if node.Name == "" || node.Name == masterName {
		return nil
	}
	if hasRole(node.Roles, role.RoleMaster) {
		return nil
	}
	if node.Protocol != "" && node.Protocol != string(wg.ProtocolAmneziaWG) {
		return nil
	}
	if len(node.Pubkey) == 0 {
		log.Printf("master %s: skipping registered peer %q: missing public key", masterName, node.Name)
		return nil
	}
	if len(node.Pubkey) != len(wg.Key{}) {
		return fmt.Errorf("registered peer %q public key length = %d, want 32", node.Name, len(node.Pubkey))
	}
	key := wg.Key{}
	copy(key[:], node.Pubkey)
	peerOverlay, err := parseMasterOverlayAddress(node.OverlayIP)
	if err != nil {
		return fmt.Errorf("parse registered peer %q overlay IP %q: %w", node.Name, node.OverlayIP, err)
	}
	snapshot := listener.Snapshot()
	peer := wg.PeerConfig{
		PublicKey:         key,
		ReplaceAllowedIPs: true,
		AllowedIPs:        []net.IPNet{*peerOverlay},
	}
	if err := listener.AddMeshPeer(peer); err != nil {
		return fmt.Errorf("add registered peer %q to mesh listener: %w", node.Name, err)
	}
	if err := link.RouteReplaceLinkWithSrc(peerOverlay, snapshot.MeshInterfaceName, localOverlay.IP); err != nil {
		return fmt.Errorf("route registered peer %q overlay: %w", node.Name, err)
	}
	return nil
}

func hasRole(roles []role.Role, want role.Role) bool {
	for _, got := range roles {
		if got == want {
			return true
		}
	}
	return false
}

func (m *Master) selfRegisterCoordination() error {
	meshPubkey, err := m.listener.MeshPublicKey()
	if err != nil {
		return fmt.Errorf("read mesh public key: %w", err)
	}
	if meshPubkey.IsZero() {
		return errors.New("mesh public key is zero")
	}
	selfNode := control_plane.RegisteredNode{
		Name:         m.cfg.Name,
		Roles:        []role.Role{role.RoleMaster},
		OverlayIP:    m.cfg.OverlayIP,
		Pubkey:       meshPubkey[:],
		EndpointHost: m.cfg.MeshEndpointHost,
		NodeCertPEM:  []byte("self-registered-master"),
		Protocol:     string(wg.ProtocolAmneziaWG),
	}
	if err := m.coordination.SelfRegister(selfNode); err != nil {
		return fmt.Errorf("self-register master: %w", err)
	}
	log.Printf("master %s: self-registered in coordination registry (pubkey %x...)", m.cfg.Name, meshPubkey[:4])
	return nil
}

func parseMasterOverlayAddress(value string) (*net.IPNet, error) {
	addr, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil {
		return nil, err
	}
	bits := 32
	if addr.Is6() {
		bits = 128
	}
	return &net.IPNet{
		IP:   append(net.IP(nil), addr.AsSlice()...),
		Mask: net.CIDRMask(bits, bits),
	}, nil
}

func validateAdvertisedMeshEndpoint(endpoint string) error {
	if endpoint == "" {
		return errors.New("master mesh endpoint is required when coordination is enabled")
	}
	host, portText, err := net.SplitHostPort(endpoint)
	if err != nil {
		return fmt.Errorf("master mesh endpoint %q must be host:port: %w", endpoint, err)
	}
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("master mesh endpoint %q has empty host", endpoint)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("master mesh endpoint %q has invalid port %q", endpoint, portText)
	}
	return nil
}

// Close tears down the master listeners.
func (m *Master) Close() error {
	if m == nil {
		return nil
	}
	if m.coordination != nil {
		m.coordination.Stop()
	}
	if m.listener == nil {
		return nil
	}
	return m.listener.Close()
}

// Status returns the configured master runtime state.
func (m *Master) Status() MasterStatus {
	if m == nil {
		return MasterStatus{}
	}
	return MasterStatus{
		Name:             m.cfg.Name,
		OverlayIP:        m.cfg.OverlayIP,
		MeshEndpointHost: m.cfg.MeshEndpointHost,
		Listeners:        m.listener.Snapshot(),
		Coordination:     m.coordination.Snapshot(),
	}
}
