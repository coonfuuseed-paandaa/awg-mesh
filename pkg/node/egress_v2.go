package node

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"

	meshnft "github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/nftables"
)

// EgressMasqueradeInstaller applies the egress boundary NAT rule.
type EgressMasqueradeInstaller interface {
	Apply(ctx context.Context, cfg meshnft.MasqueradeConfig) (meshnft.MasqueradePlan, error)
}

// EgressAgentRunner runs the mesh peer-management agent after NAT is ready.
type EgressAgentRunner func(ctx context.Context) error

// EgressConfig describes the v2.0 egress role runtime.
type EgressConfig struct {
	Name                string
	OverlayIP           string
	InternetInterface   string
	MasqueradeInstaller EgressMasqueradeInstaller
	AgentRunner         EgressAgentRunner
}

// EgressStatus is the observable runtime state of the egress role.
type EgressStatus struct {
	Name              string
	OverlayIP         string
	InternetInterface string
	Masquerade        meshnft.MasqueradePlan
	Started           bool
}

// Egress applies internet-bound NAT and then runs the non-master mesh agent.
type Egress struct {
	mu      sync.Mutex
	cfg     EgressConfig
	plan    meshnft.MasqueradePlan
	started bool
}

// NewEgress validates config and builds the egress runtime without touching kernel state.
func NewEgress(cfg EgressConfig) (*Egress, error) {
	if strings.TrimSpace(cfg.Name) == "" {
		return nil, errors.New("egress name is required")
	}
	if strings.TrimSpace(cfg.OverlayIP) == "" {
		return nil, errors.New("egress overlay IP is required")
	}
	if _, err := netip.ParseAddr(cfg.OverlayIP); err != nil {
		return nil, fmt.Errorf("parse egress overlay IP %q: %w", cfg.OverlayIP, err)
	}
	plan, err := meshnft.Plan(meshnft.MasqueradeConfig{InternetInterface: cfg.InternetInterface})
	if err != nil {
		return nil, err
	}
	cfg.InternetInterface = plan.InternetInterface
	return &Egress{cfg: cfg, plan: plan}, nil
}

// Run applies the egress NAT boundary and then blocks in the configured agent runner.
func (e *Egress) Run(ctx context.Context) error {
	if e == nil {
		return errors.New("egress is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	installer := e.cfg.MasqueradeInstaller
	if installer == nil {
		defaultInstaller, err := meshnft.NewKernelMasqueradeInstaller()
		if err != nil {
			return err
		}
		installer = defaultInstaller
	}
	plan, err := installer.Apply(ctx, meshnft.MasqueradeConfig{InternetInterface: e.cfg.InternetInterface})
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.plan = plan
	e.started = true
	e.mu.Unlock()

	if e.cfg.AgentRunner != nil {
		return e.cfg.AgentRunner(ctx)
	}
	<-ctx.Done()
	return nil
}

// Close is currently idempotent because egress NAT rules are retained across restarts.
func (e *Egress) Close() error {
	return nil
}

// Status returns the configured egress runtime state.
func (e *Egress) Status() EgressStatus {
	if e == nil {
		return EgressStatus{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return EgressStatus{
		Name:              e.cfg.Name,
		OverlayIP:         e.cfg.OverlayIP,
		InternetInterface: e.cfg.InternetInterface,
		Masquerade:        e.plan,
		Started:           e.started,
	}
}
