package node

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	grpcserver "github.com/coonfuuseed-paandaa/awg-mesh/pkg/grpc"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
)

// EndpointRunner runs node logic for endpoint mode.
type EndpointRunner struct {
	node          *Node
	startTime     time.Time
	platformState endpointPlatformState
	// rotateMu serializes concurrent RotateKeypair RPC calls per NFR-5.
	// A single mutex is sufficient because each endpoint container serves one tunnel.
	rotateMu sync.Mutex
}

// NewEndpointRunner creates an endpoint mode runner.
func NewEndpointRunner(node *Node) *EndpointRunner {
	return &EndpointRunner{node: node}
}

// GetPublicKey returns the endpoint's WireGuard public key.
func (e *EndpointRunner) GetPublicKey() (wg.Key, error) {
	_, pubKey, err := EnsureKeypair(e.node.config.ConfigDir)
	return pubKey, err
}

// Run starts endpoint mode and blocks until context cancellation.
//
// Startup sequence (v1.12.2+):
//  1. EnsureKeypair + AssignOverlayIP
//  2. Per-master iface creation via createInterface() (multi-iface path when
//     topology + transport.yml are present; legacy wg0 fallback otherwise)
//  3. Stale iface cleanup: interfaces present from a previous run that are no
//     longer in transport.yml are closed before gRPC is exposed.
//  4. startGRPCServer — started AFTER ifaces are ready to prevent RPCs from
//     arriving before any interface exists.
func (e *EndpointRunner) Run(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	if e == nil || e.node == nil {
		return fmt.Errorf("endpoint runner node is required")
	}

	_, publicKey, err := EnsureKeypair(e.node.config.ConfigDir)
	if err != nil {
		return fmt.Errorf("ensure keypair: %w", err)
	}
	if e.node.config.OverlayIP != "" {
		if err := AssignOverlayIP(e.node.config.OverlayIP); err != nil {
			return fmt.Errorf("assign overlay IP: %w", err)
		}
	}

	// Phase 1: create per-master interfaces (or legacy wg0 fallback).
	// createInterface() handles both paths; see endpoint_linux.go for details.
	if err := e.createInterface(); err != nil {
		return fmt.Errorf("create endpoint interface: %w", err)
	}
	defer func() {
		if closeErr := e.closeAllIfaces(); closeErr != nil {
			e.node.logger.Warn().Err(closeErr).Msg("failed to close endpoint interfaces")
		}
	}()

	// Phase 2: stale iface cleanup.
	// Interfaces that are in platformState.ifaces but NOT in the current
	// transport.yml (e.g. a master was removed from topology between restarts)
	// are closed before we expose the gRPC port. Non-fatal per master.
	if state, err := loadNodeTransportState(e.node.config.ConfigDir); err == nil {
		activeTunnels := make(map[string]bool, len(state.Tunnels))
		for _, tt := range state.Tunnels {
			activeTunnels[tt.Name] = true
		}
		e.cleanupStaleIfaces(activeTunnels)
	}

	e.node.logger.Info().
		Str("overlay_ip", e.node.config.OverlayIP).
		Str("public_key", publicKey.String()).
		Msg("endpoint runner started")

	// Phase 3: expose gRPC — must come AFTER ifaces are ready.
	if err := startGRPCServer(ctx, e.node.config.ConfigDir, e.node.logger, nil, e, e, e, nil, e, e); err != nil {
		return fmt.Errorf("start gRPC server: %w", err)
	}
	e.startTime = time.Now()

	<-ctx.Done()

	e.node.logger.Info().Msg("endpoint runner stopping")
	return nil
}

// buildMasterIndex returns a map from master name to its sorted index in the
// masters slice. The index is used as the listen-port offset when calling
// createMasterInterface, matching the deterministic sort order produced by
// topology.MastersForEndpoint.
func buildMasterIndex(masters []topology.MasterNode) map[string]int {
	sorted := make([]topology.MasterNode, len(masters))
	copy(sorted, masters)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Name < sorted[j].Name
	})
	idx := make(map[string]int, len(sorted))
	for i, m := range sorted {
		idx[m.Name] = i
	}
	return idx
}

func (e *EndpointRunner) GetNodeState() grpcserver.NodeState {
	return grpcserver.NodeState{
		Name:      e.node.config.Name,
		Mode:      "endpoint",
		OverlayIP: e.node.config.OverlayIP,
		StartTime: e.startTime,
	}
}
