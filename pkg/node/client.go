package node

import (
	"context"
	"fmt"
	"time"

	grpcserver "github.com/coonfuuseed-paandaa/awg-mesh/pkg/grpc"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
)

// ClientRunner runs node logic for client mode.
type ClientRunner struct {
	node          *Node
	startTime     time.Time
	platformState clientPlatformState
	clientState   *ClientState
}

// NewClientRunner creates a client mode runner.
func NewClientRunner(node *Node) *ClientRunner {
	return &ClientRunner{
		node:          node,
		platformState: initClientPlatformState(),
	}
}

// GetPublicKey returns the client's WireGuard public key.
func (c *ClientRunner) GetPublicKey() (wg.Key, error) {
	_, pubKey, err := EnsureKeypair(c.node.config.ConfigDir)
	return pubKey, err
}

// GetListenPort is not applicable in client mode — clients connect to masters
// as peers rather than binding per-master listen ports. Returns 0, nil so that
// PeerManager callers treat this as a fallback case.
func (c *ClientRunner) GetListenPort(_ string) (int, error) {
	return 0, nil
}

// Run starts client mode and blocks until context cancellation.
func (c *ClientRunner) Run(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	if c == nil || c.node == nil {
		return fmt.Errorf("client runner node is required")
	}

	_, publicKey, err := EnsureKeypair(c.node.config.ConfigDir)
	if err != nil {
		return fmt.Errorf("ensure keypair: %w", err)
	}

	// FR-10.6: opt-in VRF overlay separation. Must run before interface creation
	// so EnslaveInterface calls in AddPeer find an initialised VRFManager.
	// Hard-fail when MESH_VRF=enabled and the kernel/module is unavailable (FR-10.2).
	if err := c.setupClientVRF(); err != nil {
		return fmt.Errorf("client VRF init: %w", err)
	}

	// FR-1.6: when VRF is active, the overlay IP is already assigned to the VRF
	// anchor dummy by VRFManager.Setup() — skip the lo assignment.
	if c.node.config.OverlayIP != "" && !c.isVRFActive() {
		if err := AssignOverlayIP(c.node.config.OverlayIP); err != nil {
			return fmt.Errorf("assign overlay IP: %w", err)
		}
	}
	if err := startGRPCServer(ctx, c.node.config.ConfigDir, c.node.logger, nil, nil, c, c, nil, c, nil); err != nil {
		return fmt.Errorf("start gRPC server: %w", err)
	}
	c.startTime = time.Now()

	if err := c.createInterfaces(ctx); err != nil {
		return fmt.Errorf("create client interfaces: %w", err)
	}
	defer func() {
		c.teardownDSCPRouting()
		if closeErr := c.closeInterfaces(); closeErr != nil {
			c.node.logger.Warn().Err(closeErr).Msg("failed to close client interfaces")
		}
	}()

	// Apply MSS clamping so TCP traffic through overlay tunnels does not stall
	// on fragmented packets. Idempotent and non-fatal. Bug 12 / F-002.
	c.setupClientFirewallRules()

	if err := c.reconcileFromTransportState(); err != nil {
		// Partial-mesh boot tolerance (FR-7): reconcile errors are non-fatal.
		// Some tunnels may have been set up successfully even if others failed.
		// Log the error and continue — healthcheck will converge state over time.
		c.node.logger.Warn().Err(err).Msg("reconcile client transport state failed (non-fatal, partial-mesh boot)")
	}

	// Without a mounted topology file, load persisted client metadata before the
	// first ECMP rebuild so overlay-space routes can still be programmed during
	// the initial init/startup cycle.
	if c.node.topology == nil {
		loaded, err := loadClientState(c.node.config.ConfigDir)
		if err != nil {
			c.node.logger.Warn().Err(err).Msg("load client state failed (non-fatal)")
		} else if loaded.OverlayIP != "" {
			c.clientState = &loaded
			c.node.logger.Info().
				Str("overlay_ip", loaded.OverlayIP).
				Str("overlay_space", loaded.OverlaySpace).
				Msg("client state loaded from disk")
		}
	}

	// Ensure ECMP is built from whatever links reconcile managed to bring up,
	// even when reconcile returned errors (FR-7 partial-mesh boot).
	if err := c.rebuildClientECMP("init"); err != nil {
		c.node.logger.Warn().Err(err).Msg("initial ECMP build failed (non-fatal)")
	}

	if err := c.setupDSCPRouting(); err != nil {
		c.node.logger.Warn().Err(err).Msg("setup DSCP policy routing failed (non-fatal)")
	}

	c.startDNSServer(ctx)

	c.node.logger.Info().
		Str("public_key", publicKey.String()).
		Msg("client runner started")
	c.node.logger.Info().Str("wan_interface", discoverWANInterface()).Msg("WAN interface discovered")

	c.startHealthCheck(ctx)

	<-ctx.Done()

	c.node.logger.Info().Msg("client runner stopping")
	return nil
}

func (c *ClientRunner) GetNodeState() grpcserver.NodeState {
	return grpcserver.NodeState{
		Name:      c.node.config.Name,
		Mode:      "client",
		OverlayIP: c.node.config.OverlayIP,
		StartTime: c.startTime,
	}
}
