package node

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/control_plane"
)

const masterCoordinationStartupTimeout = 3 * time.Second

// MasterCoordinationConfig enables the master-owned coordination endpoint.
type MasterCoordinationConfig struct {
	ListenAddr              string
	StateDir                string
	CADir                   string
	CertHosts               []string
	CertRotationDays        int
	AuditCap                int
	AllowInsecurePublicBind bool
}

// MasterCoordinationSnapshot is the observable state of the coordination child.
type MasterCoordinationSnapshot struct {
	Enabled    bool
	ListenAddr string
	BoundAddr  string
	Started    bool
}

// MasterCoordination wraps the existing control-plane daemon for master-owned use.
type MasterCoordination struct {
	cfg    MasterCoordinationConfig
	daemon *control_plane.Daemon

	mu      sync.RWMutex
	done    chan error
	started bool
}

// NewMasterCoordination builds an unstarted master-owned coordination adapter.
func NewMasterCoordination(cfg MasterCoordinationConfig) (*MasterCoordination, error) {
	daemon, err := control_plane.NewDaemon(control_plane.Config{
		ListenAddr:              cfg.ListenAddr,
		StateDir:                cfg.StateDir,
		CADir:                   cfg.CADir,
		CertHosts:               append([]string(nil), cfg.CertHosts...),
		CertRotationDays:        cfg.CertRotationDays,
		AuditCap:                cfg.AuditCap,
		AllowInsecurePublicBind: cfg.AllowInsecurePublicBind,
	})
	if err != nil {
		return nil, err
	}
	return &MasterCoordination{cfg: cloneMasterCoordinationConfig(cfg), daemon: daemon}, nil
}

// Start starts the coordination listener and returns after the socket is bound.
func (c *MasterCoordination) Start(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	done := c.begin(ctx)
	if done == nil {
		return nil
	}
	return c.waitUntilReady(ctx, done)
}

func (c *MasterCoordination) begin(ctx context.Context) chan error {
	c.mu.Lock()
	if c.done != nil {
		c.mu.Unlock()
		return nil
	}
	done := make(chan error, 1)
	c.done = done
	c.mu.Unlock()

	go func() {
		err := c.daemon.Run(ctx)
		c.setStarted(false)
		done <- err
	}()
	return done
}

func (c *MasterCoordination) waitUntilReady(ctx context.Context, done chan error) error {
	timer := time.NewTimer(masterCoordinationStartupTimeout)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		if c.daemon.ListenerAddr() != "" {
			c.setStarted(true)
			return nil
		}
		select {
		case err := <-done:
			c.clearDone()
			if err != nil {
				return err
			}
			return errors.New("coordination listener stopped before it was ready")
		case <-ctx.Done():
			c.Stop()
			return ctx.Err()
		case <-timer.C:
			c.Stop()
			return c.startupTimeoutError()
		case <-ticker.C:
		}
	}
}

func (c *MasterCoordination) startupTimeoutError() error {
	return fmt.Errorf("coordination listener %q did not start within %s", c.cfg.ListenAddr, masterCoordinationStartupTimeout)
}

// Done returns the adapter completion channel after Start has been called.
func (c *MasterCoordination) Done() <-chan error {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.done
}

// Stop requests graceful shutdown of the coordination server.
func (c *MasterCoordination) Stop() {
	if c == nil || c.daemon == nil {
		return
	}
	c.daemon.Stop()
	c.setStarted(false)
}

// Snapshot returns the current adapter state without exposing control-plane internals.
func (c *MasterCoordination) Snapshot() MasterCoordinationSnapshot {
	if c == nil {
		return MasterCoordinationSnapshot{}
	}
	c.mu.RLock()
	started := c.started
	c.mu.RUnlock()
	boundAddr := ""
	if started {
		boundAddr = c.daemon.ListenerAddr()
	}
	return MasterCoordinationSnapshot{
		Enabled:    true,
		ListenAddr: c.cfg.ListenAddr,
		BoundAddr:  boundAddr,
		Started:    started,
	}
}

func (c *MasterCoordination) setStarted(started bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.started = started
}

func (c *MasterCoordination) clearDone() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.done = nil
}

func cloneMasterCoordinationConfig(cfg MasterCoordinationConfig) MasterCoordinationConfig {
	cfg.CertHosts = append([]string(nil), cfg.CertHosts...)
	return cfg
}
