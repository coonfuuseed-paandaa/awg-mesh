package grpcserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rs/zerolog"
	"github.com/thebtf/awg-mesh/pkg/wg"
	proto "github.com/thebtf/awg-mesh/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

// AgentHandler implements proto.AwgAgentServer for the awg-mesh-node.
// It embeds UnimplementedAwgAgentServer and overrides onboarding, tunnel, and
// parameter-rotation RPCs backed by injected runtime managers.
type AgentHandler struct {
	proto.UnimplementedAwgAgentServer
	configDir    string
	logger       zerolog.Logger
	tunnelMgr    TunnelManager
	paramApplier ParamApplier
}

// NewAgentHandler creates an AgentHandler that stores received config under configDir.
func NewAgentHandler(configDir string, logger zerolog.Logger) *AgentHandler {
	return NewAgentHandlerFull(configDir, logger, nil, nil)
}

// NewAgentHandlerFull creates an AgentHandler with optional runtime managers.
func NewAgentHandlerFull(
	configDir string,
	logger zerolog.Logger,
	tunnelMgr TunnelManager,
	paramApplier ParamApplier,
) *AgentHandler {
	return &AgentHandler{
		configDir:    configDir,
		logger:       logger,
		tunnelMgr:    tunnelMgr,
		paramApplier: paramApplier,
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
// Current implementation only acknowledges the request.
func (h *AgentHandler) CaptureRefresh(_ context.Context, req *proto.CaptureRequest) (*proto.CaptureResponse, error) {
	h.logger.Info().
		Int32("count_per_domain", req.CountPerDomain).
		Int("domains", len(req.Domains)).
		Msg("capture refresh requested")

	return &proto.CaptureResponse{
		Success:       true,
		CapturedCount: 0,
	}, nil
}

// AddTunnel creates a tunnel on master mode nodes.
func (h *AgentHandler) AddTunnel(_ context.Context, req *proto.AddTunnelRequest) (*proto.AddTunnelResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if h.tunnelMgr == nil {
		return nil, status.Error(codes.Unimplemented, "tunnel management not available in this mode")
	}

	tunnelName := strings.TrimSpace(req.GetName())
	if tunnelName == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}

	endpointHost := strings.TrimSpace(req.GetEndpointHost())
	if endpointHost == "" {
		return nil, status.Error(codes.InvalidArgument, "endpoint_host is required")
	}

	weight := int(req.GetWeight())
	if weight <= 0 {
		weight = 1
	}

	peerPublicKey := wg.Key{}
	rawPeerKey := req.GetPeerPublicKey()
	if len(rawPeerKey) > 0 {
		parsedKey, err := wg.NewKey(rawPeerKey)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "peer_public_key: %v", err)
		}
		peerPublicKey = parsedKey
	}

	err := h.tunnelMgr.AddTunnel(
		tunnelName,
		endpointHost,
		strings.TrimSpace(req.GetOverlayIp()),
		strings.TrimSpace(req.GetBalancerIp()),
		weight,
		peerPublicKey,
	)
	if err != nil {
		h.logger.Error().Err(err).Str("tunnel", tunnelName).Msg("add tunnel failed")
		return nil, status.Errorf(codes.Internal, "add tunnel: %v", err)
	}

	return &proto.AddTunnelResponse{
		Success:       true,
		InterfaceName: "wg-" + tunnelName,
	}, nil
}

// RemoveTunnel removes a tunnel from master mode nodes.
func (h *AgentHandler) RemoveTunnel(_ context.Context, req *proto.RemoveTunnelRequest) (*proto.RemoveTunnelResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if h.tunnelMgr == nil {
		return nil, status.Error(codes.Unimplemented, "tunnel management not available in this mode")
	}

	tunnelName := strings.TrimSpace(req.GetName())
	if tunnelName == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}

	if err := h.tunnelMgr.RemoveTunnel(tunnelName); err != nil {
		h.logger.Error().Err(err).Str("tunnel", tunnelName).Msg("remove tunnel failed")
		return nil, status.Errorf(codes.Internal, "remove tunnel: %v", err)
	}

	return &proto.RemoveTunnelResponse{Success: true}, nil
}

// RotateParams applies rotated AWG parameters to a tunnel.
func (h *AgentHandler) RotateParams(_ context.Context, req *proto.RotateParamsRequest) (*proto.RotateParamsResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if h.paramApplier == nil {
		return nil, status.Error(codes.Unimplemented, "param rotation not available in this mode")
	}

	tunnelName := strings.TrimSpace(req.GetTunnelName())
	if tunnelName == "" {
		return nil, status.Error(codes.InvalidArgument, "tunnel_name is required")
	}

	newParams := req.GetNewParams()
	if newParams == nil {
		return nil, status.Error(codes.InvalidArgument, "new_params is required")
	}

	cfg, hasParams := mapParamsToConfig(newParams)
	if !hasParams {
		return nil, status.Error(codes.InvalidArgument, "new_params has no values to apply")
	}

	h.logger.Info().
		Str("tunnel", tunnelName).
		Int32("tier", req.GetTier()).
		Msg("rotate params requested")

	if err := h.paramApplier.ApplyParams(tunnelName, cfg); err != nil {
		h.logger.Error().Err(err).Str("tunnel", tunnelName).Int32("tier", req.GetTier()).Msg("rotate params failed")
		return nil, status.Errorf(codes.Internal, "apply params: %v", err)
	}

	return &proto.RotateParamsResponse{
		Success: true,
		Message: fmt.Sprintf("tier %d params applied to tunnel %s", req.GetTier(), tunnelName),
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

func mapParamsToConfig(params *proto.AwgParams) (wg.Config, bool) {
	cfg := wg.Config{}
	hasParams := false

	if params.GetJc() != 0 {
		cfg.Jc = wg.IntPtr(int(params.GetJc()))
		hasParams = true
	}
	if params.GetJmin() != 0 {
		cfg.Jmin = wg.IntPtr(int(params.GetJmin()))
		hasParams = true
	}
	if params.GetJmax() != 0 {
		cfg.Jmax = wg.IntPtr(int(params.GetJmax()))
		hasParams = true
	}
	if params.GetS1() != 0 {
		cfg.S1 = wg.IntPtr(int(params.GetS1()))
		hasParams = true
	}
	if params.GetS2() != 0 {
		cfg.S2 = wg.IntPtr(int(params.GetS2()))
		hasParams = true
	}
	if params.GetS3() != 0 {
		cfg.S3 = wg.IntPtr(int(params.GetS3()))
		hasParams = true
	}
	if params.GetS4() != 0 {
		cfg.S4 = wg.IntPtr(int(params.GetS4()))
		hasParams = true
	}
	if params.GetH1() != 0 {
		cfg.H1 = wg.StrPtr(strconv.FormatInt(int64(params.GetH1()), 10))
		hasParams = true
	}
	if params.GetH2() != 0 {
		cfg.H2 = wg.StrPtr(strconv.FormatInt(int64(params.GetH2()), 10))
		hasParams = true
	}
	if params.GetH3() != 0 {
		cfg.H3 = wg.StrPtr(strconv.FormatInt(int64(params.GetH3()), 10))
		hasParams = true
	}
	if params.GetH4() != 0 {
		cfg.H4 = wg.StrPtr(strconv.FormatInt(int64(params.GetH4()), 10))
		hasParams = true
	}
	if params.GetI1() != "" {
		cfg.I1 = wg.StrPtr(params.GetI1())
		hasParams = true
	}
	if params.GetI2() != "" {
		cfg.I2 = wg.StrPtr(params.GetI2())
		hasParams = true
	}
	if params.GetI3() != "" {
		cfg.I3 = wg.StrPtr(params.GetI3())
		hasParams = true
	}
	if params.GetI4() != "" {
		cfg.I4 = wg.StrPtr(params.GetI4())
		hasParams = true
	}
	if params.GetI5() != "" {
		cfg.I5 = wg.StrPtr(params.GetI5())
		hasParams = true
	}

	return cfg, hasParams
}
