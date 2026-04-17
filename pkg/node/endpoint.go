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
		reconciled := 0
		for _, tt := range state.Tunnels {
			if tt.PeerPublicKey == "" {
				continue
			}

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
				}
				reconciled++
			}
		}

		e.node.logger.Info().
			Int("peers", reconciled).
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
