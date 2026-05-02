package node

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
)

// MasterConfig describes the v2.0 master role runtime.
type MasterConfig struct {
	Name         string
	OverlayIP    string
	DualListener wg.DualListenerConfig
}

// MasterStatus is the observable runtime state of the master protocol bridge.
type MasterStatus struct {
	Name      string
	OverlayIP string
	Listeners wg.DualListenerSnapshot
}

// Master runs the vanilla-WG client listener and AmneziaWG mesh listener.
type Master struct {
	cfg      MasterConfig
	listener *wg.DualListener
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
	cfg.DualListener = listenerCfg
	return &Master{cfg: cfg, listener: listener}, nil
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
	<-ctx.Done()
	if err := m.Close(); err != nil {
		return err
	}
	return nil
}

// Close tears down the master listeners.
func (m *Master) Close() error {
	if m == nil || m.listener == nil {
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
		Name:      m.cfg.Name,
		OverlayIP: m.cfg.OverlayIP,
		Listeners: m.listener.Snapshot(),
	}
}
