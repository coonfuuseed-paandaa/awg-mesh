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
