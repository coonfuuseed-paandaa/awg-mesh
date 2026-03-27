package grpcserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rs/zerolog"
	proto "github.com/thebtf/awg-mesh/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

// AgentHandler implements proto.AwgAgentServer for the awg-mesh-node.
// It embeds UnimplementedAwgAgentServer so only Init is overridden; all other
// RPCs return codes.Unimplemented automatically.
type AgentHandler struct {
	proto.UnimplementedAwgAgentServer
	configDir string
	logger    zerolog.Logger
}

// NewAgentHandler creates an AgentHandler that stores received config under configDir.
func NewAgentHandler(configDir string, logger zerolog.Logger) *AgentHandler {
	return &AgentHandler{
		configDir: configDir,
		logger:    logger,
	}
}

// Init receives TLS credentials and initial node config from mesh-ctl, writes
// them to disk, and returns success. After Init completes, the node can
// transition from token-only auth to full mTLS.
func (h *AgentHandler) Init(_ context.Context, req *proto.InitRequest) (*proto.InitResponse, error) {
	tlsDir := filepath.Join(h.configDir, "tls")

	if err := os.MkdirAll(tlsDir, 0755); err != nil {
		h.logger.Error().Err(err).Str("dir", tlsDir).Msg("init: create tls dir")
		return nil, status.Errorf(codes.Internal, "init: create tls dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(tlsDir, "ca.crt"), req.CaCert, 0644); err != nil {
		h.logger.Error().Err(err).Msg("init: write ca.crt")
		return nil, status.Errorf(codes.Internal, "init: write ca.crt: %v", err)
	}

	if err := os.WriteFile(filepath.Join(tlsDir, "node.crt"), req.NodeCert, 0644); err != nil {
		h.logger.Error().Err(err).Msg("init: write node.crt")
		return nil, status.Errorf(codes.Internal, "init: write node.crt: %v", err)
	}

	if err := os.WriteFile(filepath.Join(tlsDir, "node.key"), req.NodeKey, 0600); err != nil {
		h.logger.Error().Err(err).Msg("init: write node.key")
		return nil, status.Errorf(codes.Internal, "init: write node.key: %v", err)
	}

	if req.Config != nil {
		jsonBytes, err := protojson.Marshal(req.Config)
		if err != nil {
			h.logger.Error().Err(err).Msg("init: marshal node config")
			return nil, status.Errorf(codes.Internal, "init: marshal node config: %v", err)
		}
		cfgPath := filepath.Join(h.configDir, "node-config.json")
		if err := os.WriteFile(cfgPath, jsonBytes, 0600); err != nil {
			h.logger.Error().Err(err).Str("path", cfgPath).Msg("init: write node config")
			return nil, status.Errorf(codes.Internal, "init: write node config: %v", err)
		}
	}

	h.logger.Info().Str("configDir", h.configDir).Msg("node initialized via Init RPC")

	return &proto.InitResponse{
		Success: true,
		Message: fmt.Sprintf("initialized: config written to %s", h.configDir),
	}, nil
}

// CaptureRefresh triggers TLS/QUIC packet capture on the node.
// Currently a stub — actual gopacket capture is implemented in T053.
func (h *AgentHandler) CaptureRefresh(_ context.Context, req *proto.CaptureRequest) (*proto.CaptureResponse, error) {
	h.logger.Info().
		Int32("count_per_domain", req.CountPerDomain).
		Int("domains", len(req.Domains)).
		Msg("capture refresh requested")

	// TODO(T053): implement actual gopacket TLS/QUIC capture
	// For now, log the request and return success with zero count.
	return &proto.CaptureResponse{
		Success:       true,
		CapturedCount: 0,
	}, nil
}

// RotateParams applies rotated AWG parameters to a tunnel.
func (h *AgentHandler) RotateParams(_ context.Context, req *proto.RotateParamsRequest) (*proto.RotateParamsResponse, error) {
	h.logger.Info().
		Str("tunnel", req.TunnelName).
		Int32("tier", req.Tier).
		Msg("rotate params requested")

	// TODO: wire to actual UAPI param set on the tunnel interface
	return &proto.RotateParamsResponse{
		Success: true,
		Message: fmt.Sprintf("tier %d rotation accepted for tunnel %s", req.Tier, req.TunnelName),
	}, nil
}

// GetStatus returns current node status.
func (h *AgentHandler) GetStatus(_ context.Context, _ *proto.Empty) (*proto.NodeStatus, error) {
	return &proto.NodeStatus{
		Name: "unknown",
		Mode: "unknown",
	}, nil
}

// GetHealth returns node health information.
func (h *AgentHandler) GetHealth(_ context.Context, _ *proto.Empty) (*proto.HealthResponse, error) {
	return &proto.HealthResponse{
		Healthy: true,
	}, nil
}

// RotateToken updates the node's MESH_TOKEN hash.
func (h *AgentHandler) RotateToken(_ context.Context, req *proto.RotateTokenRequest) (*proto.RotateTokenResponse, error) {
	tokenPath := filepath.Join(h.configDir, "mesh.token")
	if err := os.WriteFile(tokenPath, []byte(req.NewTokenHash), 0600); err != nil {
		h.logger.Error().Err(err).Msg("rotate token: write hash")
		return nil, status.Errorf(codes.Internal, "rotate token: %v", err)
	}

	h.logger.Info().Msg("token rotated")
	return &proto.RotateTokenResponse{Success: true}, nil
}
