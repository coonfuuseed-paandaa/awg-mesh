package node

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	grpcserver "github.com/coonfuuseed-paandaa/awg-mesh/pkg/grpc"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
)

// EndpointRunner runs node logic for endpoint mode.
type EndpointRunner struct {
	node          *Node
	startTime     time.Time
	platformState endpointPlatformState
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
	if err := startGRPCServer(ctx, e.node.config.ConfigDir, e.node.logger, nil, e, e, e, nil, e); err != nil {
		return fmt.Errorf("start gRPC server: %w", err)
	}
	e.startTime = time.Now()

	if err := e.createInterface(); err != nil {
		return fmt.Errorf("create endpoint interface: %w", err)
	}
	defer func() {
		if closeErr := e.closeInterface(); closeErr != nil {
			e.node.logger.Warn().Err(closeErr).Msg("failed to close endpoint interface")
		}
	}()

	e.node.logger.Info().
		Str("overlay_ip", e.node.config.OverlayIP).
		Str("public_key", publicKey.String()).
		Msg("endpoint runner started")

	if state, err := loadNodeTransportState(e.node.config.ConfigDir); err == nil && len(state.Tunnels) > 0 {
		peersAdded := 0
		routesConfigured := 0
		for _, tt := range state.Tunnels {
			if tt.PeerPublicKey == "" {
				continue
			}

			// AddPeer below receives only the transport /32 as the WireGuard peer allowed IP.
			// Overlay CIDRs (tt.AllowedIPs) are installed as kernel link-scope routes by the
			// subsequent ConfigureTransport call, NOT as WireGuard allowed IPs (which would
			// conflict with the endpoint's own overlay subnet membership).
			allowedIPs := make([]string, 0, 1)
			if tt.PeerTransportIP != "" {
				allowedIPs = append(allowedIPs, tt.PeerTransportIP+"/32")
			}

			// Persisted transport.yml encodes peer_public_key as hex
			// (written by saveNodeTransportStateAfterPeerAdded in pkg/grpc/handlers.go).
			// Previously decoded as base64 → "invalid key length 48" → all peers
			// silently dropped on every restart (local tracker issue #94).
			peerBytes, err := hex.DecodeString(strings.TrimSpace(tt.PeerPublicKey))
			if err != nil {
				e.node.logger.Warn().
					Str("tunnel", tt.Name).
					Err(err).
					Msg("reconcile peer: decode hex key failed")
				continue
			}
			peerKey, err := wg.NewKey(peerBytes)
			if err != nil {
				e.node.logger.Warn().
					Str("tunnel", tt.Name).
					Err(err).
					Msg("reconcile peer: invalid key length")
				continue
			}

			if err := e.AddPeer(peerKey[:], nil, allowedIPs, tt.PeerEndpoint, 25); err != nil {
				e.node.logger.Warn().
					Str("tunnel", tt.Name).
					Err(err).
					Msg("reconcile peer failed")
			} else {
				peersAdded++
				if err := e.ConfigureTransport(
					hex.EncodeToString(peerBytes),
					tt.TransportIP,
					tt.PeerTransportIP,
					tt.AllowedIPs,
				); err != nil {
					e.node.logger.Warn().
						Err(err).
						Str("tunnel", tt.Name).
						Msg("reconcile: configure transport failed")
					continue
				}
				routesConfigured++
			}
		}

		e.node.logger.Info().
			Int("peers_added", peersAdded).
			Int("routes_configured", routesConfigured).
			Msg("reconciled peers from saved state")
	}

	<-ctx.Done()

	e.node.logger.Info().Msg("endpoint runner stopping")
	return nil
}

func (e *EndpointRunner) GetNodeState() grpcserver.NodeState {
	return grpcserver.NodeState{
		Name:      e.node.config.Name,
		Mode:      "endpoint",
		OverlayIP: e.node.config.OverlayIP,
		StartTime: e.startTime,
	}
}

// LoadKeypair reads the endpoint's current keypair from node.yml.
//
// Implements grpcserver.NodeStatePersister — paired with PersistKeypair for the
// read→write cycle used by grpcserver.RotateKeypair to capture the pre-rotation
// keypair for rollback.
//
// Returns error when node.yml is missing, malformed, or missing keypair fields.
func (e *EndpointRunner) LoadKeypair() (wg.Key, wg.Key, error) {
	if e == nil || e.node == nil {
		return wg.Key{}, wg.Key{}, fmt.Errorf("endpoint runner is not initialized")
	}
	dir := strings.TrimSpace(e.node.config.ConfigDir)
	if dir == "" {
		return wg.Key{}, wg.Key{}, fmt.Errorf("config dir is required")
	}
	state, err := LoadNodeState(dir)
	if err != nil {
		return wg.Key{}, wg.Key{}, fmt.Errorf("load node state: %w", err)
	}
	priv, err := wg.ParseKey(state.PrivateKey)
	if err != nil {
		return wg.Key{}, wg.Key{}, fmt.Errorf("parse private key: %w", err)
	}
	pub, err := wg.ParseKey(state.PublicKey)
	if err != nil {
		return wg.Key{}, wg.Key{}, fmt.Errorf("parse public key: %w", err)
	}
	return priv, pub, nil
}

// PersistKeypair atomically updates the endpoint's on-disk keypair.
//
// Implements grpcserver.NodeStatePersister — master and client modes do NOT
// implement this interface, which is how grpcserver.RotateKeypair gates
// endpoint-only operation via type assertion on stateProvider.
//
// Writes node.yml via SaveNodeState (which uses .tmp + rename, mode 0600).
// Preserves Name / Mode / OverlayIP from the current on-disk state, if any;
// when the file is missing (fresh node never initialized) falls back to
// config-driven defaults and the new keypair is written as the first entry.
//
// On error the on-disk file is unchanged (SaveNodeState is atomic).
//
// Used by tier-3 keypair rotation (engram #125).
func (e *EndpointRunner) PersistKeypair(priv wg.Key, pub wg.Key) error {
	if e == nil || e.node == nil {
		return fmt.Errorf("endpoint runner is not initialized")
	}
	dir := strings.TrimSpace(e.node.config.ConfigDir)
	if dir == "" {
		return fmt.Errorf("config dir is required")
	}

	// Load current state to preserve non-keypair fields. Missing file is OK —
	// we'll write a fresh NodeState with the new keypair as baseline.
	current, loadErr := LoadNodeState(dir)
	var next NodeState
	if loadErr == nil && current != nil {
		next = *current
	} else {
		next = NodeState{
			Name:      e.node.config.Name,
			Mode:      "endpoint",
			OverlayIP: e.node.config.OverlayIP,
		}
	}

	next.PrivateKey = priv.String()
	next.PublicKey = pub.String()

	if err := SaveNodeState(dir, next); err != nil {
		return fmt.Errorf("persist keypair: %w", err)
	}

	e.node.logger.Info().
		Str("new_pub_prefix", pub.String()[:8]).
		Msg("endpoint keypair persisted to node.yml")
	return nil
}
