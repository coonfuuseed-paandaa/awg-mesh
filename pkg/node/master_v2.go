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
	cfg          MasterConfig
	listener     *wg.DualListener
	coordination *MasterCoordination
}

// NewMaster validates config and builds the master runtime.
func NewMaster(cfg MasterConfig) (*Master, error) {
	if strings.TrimSpace(cfg.Name) == "" {
		return nil, errors.New("master name is required")
	}
	if strings.TrimSpace(cfg.OverlayIP) == "" {
		return nil, errors.New("master overlay IP is required")
	}
	if _, err := netip.ParseAddr(cfg.OverlayIP); err != nil {
		return nil, fmt.Errorf("parse master overlay IP %q: %w", cfg.OverlayIP, err)
	}
	cfg.MeshEndpointHost = strings.TrimSpace(cfg.MeshEndpointHost)
	if cfg.Coordination != nil {
		if err := validateAdvertisedMeshEndpoint(cfg.MeshEndpointHost); err != nil {
			return nil, err
		}
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
		coordination, err = NewMasterCoordination(coordinationCfg)
		if err != nil {
			return nil, fmt.Errorf("build master coordination: %w", err)
		}
		cfg.Coordination = &coordinationCfg
	}
	cfg.DualListener = listenerCfg
	return &Master{cfg: cfg, listener: listener, coordination: coordination}, nil
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
