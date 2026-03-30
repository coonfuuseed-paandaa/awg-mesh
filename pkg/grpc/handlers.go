package grpcserver

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/thebtf/awg-mesh/pkg/routing"
	"github.com/thebtf/awg-mesh/pkg/wg"
	proto "github.com/thebtf/awg-mesh/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/yaml.v3"
)

// AgentHandler implements proto.AwgAgentServer for the awg-mesh-node.
// It embeds UnimplementedAwgAgentServer and overrides onboarding, tunnel, and
// parameter-rotation RPCs backed by injected runtime managers.
type AgentHandler struct {
	proto.UnimplementedAwgAgentServer
	configDir        string
	logger           zerolog.Logger
	tunnelMgr        TunnelManager
	paramApplier     ParamApplier
	peerMgr          PeerManager
	stateProvider    NodeStateProvider
	captureFunc      CaptureFunc
	captureScheduler CaptureScheduler
	keyProvider      KeyProvider
}

// NewAgentHandler creates an AgentHandler that stores received config under configDir.
func NewAgentHandler(configDir string, logger zerolog.Logger) *AgentHandler {
	return NewAgentHandlerFull(configDir, logger, nil, nil, nil, nil, nil, nil, nil)
}

// NewAgentHandlerFull creates an AgentHandler with optional runtime managers.
func NewAgentHandlerFull(
	configDir string,
	logger zerolog.Logger,
	tunnelMgr TunnelManager,
	paramApplier ParamApplier,
	captureFunc CaptureFunc,
	peerMgr PeerManager,
	stateProvider NodeStateProvider,
	captureScheduler CaptureScheduler,
	keyProvider KeyProvider,
) *AgentHandler {
	return &AgentHandler{
		configDir:        configDir,
		logger:           logger,
		tunnelMgr:        tunnelMgr,
		paramApplier:     paramApplier,
		peerMgr:          peerMgr,
		stateProvider:    stateProvider,
		captureFunc:      captureFunc,
		captureScheduler: captureScheduler,
		keyProvider:      keyProvider,
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

	resp := &proto.InitResponse{
		Success: true,
		Message: fmt.Sprintf("initialized: config written to %s", h.configDir),
	}

	if h.keyProvider != nil {
		pubKey, err := h.keyProvider.GetPublicKey()
		if err != nil {
			h.logger.Warn().Err(err).Msg("init: failed to read public key")
		} else {
			resp.NodePublicKey = pubKey[:]
		}
	}

	return resp, nil
}

// CaptureRefresh triggers TLS/QUIC packet capture on the node.
// Current implementation only acknowledges the request.
func (h *AgentHandler) CaptureRefresh(_ context.Context, req *proto.CaptureRequest) (*proto.CaptureResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}

	if h.captureFunc == nil {
		h.logger.Info().
			Int("domains", len(req.GetDomains())).
			Int32("count_per_domain", req.GetCountPerDomain()).
			Msg("capture unavailable: capture function not injected")

		return &proto.CaptureResponse{
			Success:       true,
			CapturedCount: 0,
		}, nil
	}

	domains := make([]string, 0, len(req.GetDomains()))
	for _, domain := range req.GetDomains() {
		if trimmed := strings.TrimSpace(domain); trimmed != "" {
			domains = append(domains, trimmed)
		}
	}

	// Persist received domains to /config/domains.txt for future reference.
	if len(domains) > 0 {
		domainsPath := filepath.Join(h.configDir, "domains.txt")
		content := strings.Join(domains, "\n") + "\n"
		if writeErr := os.WriteFile(domainsPath, []byte(content), 0644); writeErr != nil {
			h.logger.Warn().Err(writeErr).Str("path", domainsPath).Msg("failed to persist domains file")
		} else {
			h.logger.Info().Int("count", len(domains)).Str("path", domainsPath).Msg("domains file updated")
		}
	}

	countPerDomain := int(req.GetCountPerDomain())
	if countPerDomain <= 0 {
		countPerDomain = 3
	}

	// Configure autonomous capture schedule if provided.
	if schedule := strings.TrimSpace(req.GetSchedule()); schedule != "" && h.captureScheduler != nil {
		if err := h.captureScheduler.SetSchedule(domains, countPerDomain, schedule, int(req.GetRetentionDays())); err != nil {
			h.logger.Warn().Err(err).Str("schedule", schedule).Msg("failed to set capture schedule")
		} else {
			h.logger.Info().Str("schedule", schedule).Int32("retention_days", req.GetRetentionDays()).Msg("capture schedule configured")
		}
	}

	capturedCount, err := h.captureFunc("", domains, countPerDomain, 15*time.Second)
	if err != nil {
		h.logger.Error().Err(err).Msg("capture failed")
		return nil, status.Errorf(codes.Internal, "capture failed: %v", err)
	}

	h.logger.Info().
		Int32("count_per_domain", req.CountPerDomain).
		Int("domains", len(req.Domains)).
		Int("captured", capturedCount).
		Msg("capture refresh completed")

	return &proto.CaptureResponse{
		Success:       true,
		CapturedCount: int32(capturedCount),
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
		strings.TrimSpace(req.GetTransportSubnet()),
		strings.TrimSpace(req.GetMasterTransportIp()),
		strings.TrimSpace(req.GetEndpointTransportIp()),
		weight,
		peerPublicKey,
	)
	if err != nil {
		h.logger.Error().Err(err).Str("tunnel", tunnelName).Msg("add tunnel failed")
		return nil, status.Errorf(codes.Internal, "add tunnel: %v", err)
	}

	resp := &proto.AddTunnelResponse{
		Success:       true,
		InterfaceName: "wg-" + tunnelName,
	}

	if h.keyProvider != nil {
		pubKey, err := h.keyProvider.GetPublicKey()
		if err != nil {
			h.logger.Warn().Err(err).Msg("add tunnel: failed to read master public key")
		} else {
			resp.MasterPublicKey = pubKey[:]
		}
	}

	if h.tunnelMgr != nil {
		if port, portErr := h.tunnelMgr.GetListenPort(tunnelName); portErr == nil {
			resp.ListenPort = int32(port)
		}
	}

	return resp, nil
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

func (h *AgentHandler) ListTunnels(_ context.Context, _ *proto.Empty) (*proto.TunnelList, error) {
	if h.tunnelMgr == nil {
		return nil, status.Error(codes.Unimplemented, "tunnel management not available in this mode")
	}

	tunnels := h.tunnelMgr.ListTunnels()
	protoTunnels := make([]*proto.TunnelStatus, 0, len(tunnels))
	for _, tunnel := range tunnels {
		protoTunnels = append(protoTunnels, &proto.TunnelStatus{
			Name:      tunnel.Name,
			OverlayIp: tunnel.OverlayIP,
			Healthy:   tunnel.Healthy,
		})
	}

	return &proto.TunnelList{Tunnels: protoTunnels}, nil
}

func (h *AgentHandler) ListPeers(_ context.Context, _ *proto.Empty) (*proto.PeerList, error) {
	if h.peerMgr == nil {
		return nil, status.Error(codes.Unimplemented, "peer management not available in this mode")
	}

	peers := h.peerMgr.ListPeers()
	protoPeers := make([]*proto.PeerStatus, 0, len(peers))
	for _, peer := range peers {
		protoPeers = append(protoPeers, &proto.PeerStatus{
			PublicKey:     peer.PublicKey,
			Endpoint:      peer.Endpoint,
			AllowedIps:    peer.AllowedIPs,
			LastHandshake: peer.LastHandshake,
			TxBytes:       peer.TxBytes,
			RxBytes:       peer.RxBytes,
		})
	}

	return &proto.PeerList{Peers: protoPeers}, nil
}

func (h *AgentHandler) AddPeer(_ context.Context, req *proto.AddPeerRequest) (*proto.AddPeerResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if h.peerMgr == nil {
		return nil, status.Error(codes.Unimplemented, "peer management not available in this mode")
	}
	if len(req.GetPublicKey()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "public_key is required")
	}

	if err := h.peerMgr.AddPeer(
		req.GetPublicKey(),
		req.GetPresharedKey(),
		req.GetAllowedIps(),
		strings.TrimSpace(req.GetEndpointHost()),
		req.GetPersistentKeepalive(),
	); err != nil {
		h.logger.Error().Err(err).Msg("add peer failed")
		return nil, status.Errorf(codes.Internal, "add peer: %v", err)
	}

	if err := h.saveNodeTransportStateAfterPeerAdded(req); err != nil {
		return nil, status.Errorf(codes.Internal, "save node transport state: %v", err)
	}

	if tc, ok := h.peerMgr.(TransportConfigurator); ok {
		localIP := strings.TrimSpace(req.GetLocalTransportIp())
		peerIP := strings.TrimSpace(req.GetPeerTransportIp())
		if localIP != "" && peerIP != "" {
			pubkeyHex := hex.EncodeToString(req.GetPublicKey())
			// Set balancer IP before ConfigureTransport so rebuildECMP can use it.
			if balancerIP := strings.TrimSpace(req.GetBalancerIp()); balancerIP != "" {
				if bs, bsOk := h.peerMgr.(BalancerIPSetter); bsOk {
					bs.SetBalancerIP(pubkeyHex, balancerIP)
				}
			}
			if err := tc.ConfigureTransport(pubkeyHex, localIP, peerIP); err != nil {
				h.logger.Warn().Err(err).Msg("configure transport after AddPeer failed")
			}
		}
	}

	return &proto.AddPeerResponse{Success: true}, nil
}

// nodeTransportState mirrors node.NodeTransportState for transport.yml
// serialization. Both structs must be kept in sync — they share the same file
// format. A refactor to a shared package (e.g. pkg/transport) would eliminate
// this duplication but requires breaking the pkg/node → pkg/grpc import cycle.
type nodeTransportState struct {
	OverlayIP string            `yaml:"overlay_ip"`
	Tunnels   []tunnelTransport `yaml:"tunnels"`
}

// tunnelTransport mirrors node.TunnelTransport. See nodeTransportState comment.
type tunnelTransport struct {
	Name            string `yaml:"name"`
	OverlayIP       string `yaml:"overlay_ip,omitempty"`
	TransportIP     string `yaml:"transport_ip"`
	PeerTransportIP string `yaml:"peer_transport_ip"`
	PeerPublicKey   string `yaml:"peer_public_key"`
	PeerEndpoint    string `yaml:"peer_endpoint"`
	BalancerIP      string `yaml:"balancer_ip,omitempty"`
}

func (h *AgentHandler) saveNodeTransportStateAfterPeerAdded(req *proto.AddPeerRequest) error {
	if req == nil || strings.TrimSpace(req.GetTransportSubnet()) == "" {
		return nil
	}

	state, err := loadNodeTransportState(h.configDir)
	if err != nil {
		return err
	}

	peerPublicKey := hex.EncodeToString(req.GetPublicKey())
	tunnelName, peerEndpoint := splitEndpointMetadata(strings.TrimSpace(req.GetEndpointHost()))
	entry := tunnelTransport{
		Name:            tunnelName,
		TransportIP:     req.GetLocalTransportIp(),
		PeerTransportIP: req.GetPeerTransportIp(),
		PeerPublicKey:   peerPublicKey,
		PeerEndpoint:    peerEndpoint,
		BalancerIP:      strings.TrimSpace(req.GetBalancerIp()),
	}

	nextTunnels := append([]tunnelTransport(nil), state.Tunnels...)
	found := false
	for idx, existing := range nextTunnels {
		if existing.PeerPublicKey == peerPublicKey {
			if strings.TrimSpace(existing.Name) != "" {
				entry.Name = existing.Name
			}
			nextTunnels[idx] = entry
			found = true
			break
		}
	}
	if !found {
		nextTunnels = append(nextTunnels, entry)
	}

	return saveNodeTransportState(filepath.Join(h.configDir, "transport.yml"), nodeTransportState{
		OverlayIP: strings.TrimSpace(state.OverlayIP),
		Tunnels:   nextTunnels,
	})
}

func splitEndpointMetadata(endpointHost string) (string, string) {
	trimmedEndpointHost := strings.TrimSpace(endpointHost)
	if trimmedEndpointHost == "" {
		return "", ""
	}

	if strings.Contains(trimmedEndpointHost, "|") {
		parts := strings.SplitN(trimmedEndpointHost, "|", 2)
		namePart := strings.TrimSpace(parts[0])
		if len(parts) < 2 {
			return namePart, ""
		}
		endpointPart := strings.TrimSpace(parts[1])
		if endpointPart == "" {
			return namePart, ""
		}
		return namePart, endpointPart
	}

	host, _, err := net.SplitHostPort(trimmedEndpointHost)
	if err == nil {
		return strings.TrimSpace(host), trimmedEndpointHost
	}

	return trimmedEndpointHost, trimmedEndpointHost
}

func loadNodeTransportState(configDir string) (nodeTransportState, error) {
	configDir = strings.TrimSpace(configDir)
	if configDir == "" {
		return nodeTransportState{}, fmt.Errorf("config directory is required")
	}

	path := filepath.Join(configDir, "transport.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nodeTransportState{}, nil
		}
		return nodeTransportState{}, fmt.Errorf("read node transport state %q: %w", path, err)
	}

	var state nodeTransportState
	if err := yaml.Unmarshal(data, &state); err != nil {
		return nodeTransportState{}, fmt.Errorf("unmarshal node transport state %q: %w", path, err)
	}
	return state, nil
}

func saveNodeTransportState(path string, state nodeTransportState) error {
	data, err := yaml.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal node transport state %q: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create transport state directory for %q: %w", path, err)
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return fmt.Errorf("write temporary node transport state %q: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace node transport state %q: %w", path, err)
	}
	return nil
}

func (h *AgentHandler) RemovePeer(_ context.Context, req *proto.RemovePeerRequest) (*proto.RemovePeerResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if h.peerMgr == nil {
		return nil, status.Error(codes.Unimplemented, "peer management not available in this mode")
	}
	if len(req.GetPublicKey()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "public_key is required")
	}

	if err := h.peerMgr.RemovePeer(req.GetPublicKey()); err != nil {
		h.logger.Error().Err(err).Msg("remove peer failed")
		return nil, status.Errorf(codes.Internal, "remove peer: %v", err)
	}
	return &proto.RemovePeerResponse{Success: true}, nil
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

	// Apply AWG obfuscation parameters via UAPI.
	if err := h.paramApplier.ApplyParams(tunnelName, cfg); err != nil {
		h.logger.Error().Err(err).Str("tunnel", tunnelName).Int32("tier", req.GetTier()).Msg("rotate params failed")
		return nil, status.Errorf(codes.Internal, "apply params: %v", err)
	}

	// Tier 3: apply new peer public key if provided.
	if rawKey := req.GetNewPublicKey(); len(rawKey) > 0 {
		newKey, keyErr := wg.NewKey(rawKey)
		if keyErr != nil {
			return nil, status.Errorf(codes.InvalidArgument, "new_public_key: %v", keyErr)
		}
		peerCfg := wg.Config{
			Peers: []wg.PeerConfig{{
				PublicKey:         newKey,
				ReplaceAllowedIPs: false,
				UpdateOnly:        false,
			}},
		}
		if err := h.paramApplier.ApplyParams(tunnelName, peerCfg); err != nil {
			h.logger.Error().Err(err).Str("tunnel", tunnelName).Msg("rotate: apply new public key failed")
			return nil, status.Errorf(codes.Internal, "apply new public key: %v", err)
		}
		h.logger.Info().Str("tunnel", tunnelName).Msg("tier 3: new peer public key applied")
	}

	return &proto.RotateParamsResponse{
		Success: true,
		Message: fmt.Sprintf("tier %d params applied to tunnel %s", req.GetTier(), tunnelName),
	}, nil
}

// GetStatus returns current node status.
func (h *AgentHandler) GetStatus(_ context.Context, _ *proto.Empty) (*proto.NodeStatus, error) {
	if h.stateProvider != nil {
		state := h.stateProvider.GetNodeState()
		tunnels := make([]*proto.TunnelStatus, 0, len(state.Tunnels))
		for _, tunnel := range state.Tunnels {
			tunnels = append(tunnels, &proto.TunnelStatus{
				Name:      tunnel.Name,
				OverlayIp: tunnel.OverlayIP,
				Healthy:   tunnel.Healthy,
			})
		}
		return &proto.NodeStatus{
			Name:      state.Name,
			Mode:      state.Mode,
			OverlayIp: state.OverlayIP,
			Tunnels:   tunnels,
			Uptime:    time.Since(state.StartTime).String(),
		}, nil
	}

	return &proto.NodeStatus{
		Name: "unknown",
		Mode: "unknown",
	}, nil
}

func (h *AgentHandler) GetParams(_ context.Context, req *proto.GetParamsRequest) (*proto.AwgParams, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if h.tunnelMgr == nil {
		return nil, status.Error(codes.Unimplemented, "param retrieval not available in this mode")
	}

	tunnelName := strings.TrimSpace(req.GetTunnelName())
	if tunnelName == "" {
		return nil, status.Error(codes.InvalidArgument, "tunnel_name is required")
	}

	cfg, err := h.tunnelMgr.GetParams(tunnelName)
	if err != nil {
		h.logger.Error().Err(err).Str("tunnel", tunnelName).Msg("get params failed")
		return nil, status.Errorf(codes.Internal, "get params: %v", err)
	}

	params := configToParams(cfg)
	return params, nil
}

func (h *AgentHandler) GetRoutes(_ context.Context, _ *proto.Empty) (*proto.RouteTable, error) {
	if runtime.GOOS != "linux" {
		return nil, status.Error(codes.Unimplemented, "routes are supported only on linux")
	}

	router := routing.NewNetlinkRouter()
	routes, err := router.ListRoutes()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list routes: %v", err)
	}

	protoRoutes := make([]*proto.RouteEntry, 0, len(routes))
	for _, route := range routes {
		protoRoutes = append(protoRoutes, &proto.RouteEntry{
			Destination:  route.Destination,
			ViaInterface: route.Device,
			ViaOverlayIp: route.Via,
			Active:       true,
		})
	}

	return &proto.RouteTable{Routes: protoRoutes}, nil
}

// GetHealth returns node health information.
func (h *AgentHandler) GetHealth(_ context.Context, _ *proto.Empty) (*proto.HealthResponse, error) {
	return &proto.HealthResponse{
		Healthy: true,
	}, nil
}

// RotateToken updates the node's MESH_TOKEN hash atomically (write-to-temp + rename).
func (h *AgentHandler) RotateToken(_ context.Context, req *proto.RotateTokenRequest) (*proto.RotateTokenResponse, error) {
	tokenPath := filepath.Join(h.configDir, "mesh.token")
	tmpPath := tokenPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(req.NewTokenHash), 0600); err != nil {
		h.logger.Error().Err(err).Msg("rotate token: write temp hash")
		return nil, status.Errorf(codes.Internal, "rotate token: %v", err)
	}
	if err := os.Rename(tmpPath, tokenPath); err != nil {
		_ = os.Remove(tmpPath)
		h.logger.Error().Err(err).Msg("rotate token: atomic rename")
		return nil, status.Errorf(codes.Internal, "rotate token rename: %v", err)
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

func configToParams(cfg wg.Config) *proto.AwgParams {
	params := &proto.AwgParams{}

	if cfg.Jc != nil {
		params.Jc = int32(*cfg.Jc)
	}
	if cfg.Jmin != nil {
		params.Jmin = int32(*cfg.Jmin)
	}
	if cfg.Jmax != nil {
		params.Jmax = int32(*cfg.Jmax)
	}
	if cfg.S1 != nil {
		params.S1 = int32(*cfg.S1)
	}
	if cfg.S2 != nil {
		params.S2 = int32(*cfg.S2)
	}
	if cfg.S3 != nil {
		params.S3 = int32(*cfg.S3)
	}
	if cfg.S4 != nil {
		params.S4 = int32(*cfg.S4)
	}

	if cfg.H1 != nil {
		if parsed, err := strconv.ParseInt(*cfg.H1, 10, 32); err == nil {
			params.H1 = int32(parsed)
		}
	}
	if cfg.H2 != nil {
		if parsed, err := strconv.ParseInt(*cfg.H2, 10, 32); err == nil {
			params.H2 = int32(parsed)
		}
	}
	if cfg.H3 != nil {
		if parsed, err := strconv.ParseInt(*cfg.H3, 10, 32); err == nil {
			params.H3 = int32(parsed)
		}
	}
	if cfg.H4 != nil {
		if parsed, err := strconv.ParseInt(*cfg.H4, 10, 32); err == nil {
			params.H4 = int32(parsed)
		}
	}

	if cfg.I1 != nil {
		params.I1 = *cfg.I1
	}
	if cfg.I2 != nil {
		params.I2 = *cfg.I2
	}
	if cfg.I3 != nil {
		params.I3 = *cfg.I3
	}
	if cfg.I4 != nil {
		params.I4 = *cfg.I4
	}
	if cfg.I5 != nil {
		params.I5 = *cfg.I5
	}

	return params
}
