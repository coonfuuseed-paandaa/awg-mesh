package grpcserver

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/routing"
	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/pkg/tls"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/transport"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
	proto "github.com/coonfuuseed-paandaa/awg-mesh/proto"
	"github.com/rs/zerolog"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
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
	statePersister   NodeStatePersister // nil for master/client modes; set for endpoint mode
}

// NewAgentHandler creates an AgentHandler that stores received config under configDir.
func NewAgentHandler(configDir string, logger zerolog.Logger) *AgentHandler {
	return NewAgentHandlerFull(configDir, logger, nil, nil, nil, nil, nil, nil, nil, nil)
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
	statePersister NodeStatePersister,
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
		statePersister:   statePersister,
	}
}

// Init receives TLS credentials and initial node config from mesh-ctl, writes
// them to disk, and returns success. After Init completes, the node can
// transition from token-only auth to full mTLS.
func (h *AgentHandler) Init(_ context.Context, req *proto.InitRequest) (*proto.InitResponse, error) {
	// Validate TLS material before writing to disk.
	if len(req.CaCert) == 0 || len(req.NodeCert) == 0 || len(req.NodeKey) == 0 {
		return nil, status.Error(codes.InvalidArgument, "init: ca_cert, node_cert, and node_key are all required")
	}
	caCertParsed, err := pkgtls.ParseCertPEM(req.CaCert)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "init: invalid ca_cert PEM: %v", err)
	}
	if _, err := pkgtls.ParseCertPEM(req.NodeCert); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "init: invalid node_cert PEM: %v", err)
	}
	// Verify node cert is signed by the provided CA.
	if err := pkgtls.ValidateCert(req.NodeCert, caCertParsed); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "init: node_cert not signed by ca_cert: %v", err)
	}
	// Verify cert and key form a valid pair.
	if _, err := tls.X509KeyPair(req.NodeCert, req.NodeKey); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "init: node_cert and node_key do not match: %v", err)
	}

	tlsDir := filepath.Join(h.configDir, "tls")

	if err := os.MkdirAll(tlsDir, 0700); err != nil {
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

// validTunnelName limits tunnel names to 12 safe characters.
// The derived interface name ("wg-" + name) must fit within IFNAMSIZ (15 chars).
var validTunnelName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,12}$`)

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
	if !validTunnelName.MatchString(tunnelName) {
		return nil, status.Errorf(codes.InvalidArgument, "tunnel name %q must match [a-zA-Z0-9_-]{1,12}", tunnelName)
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

// UpdateTunnelPeer updates the peer public key on a named tunnel idempotently.
func (h *AgentHandler) UpdateTunnelPeer(_ context.Context, req *proto.UpdateTunnelPeerRequest) (*proto.UpdateTunnelPeerResponse, error) {
	if h.tunnelMgr == nil {
		return nil, status.Error(codes.Unimplemented, "tunnel management not available in this mode")
	}

	name := strings.TrimSpace(req.GetName())
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "tunnel name required")
	}
	rawKey := req.GetPeerPublicKey()
	if len(rawKey) != 32 {
		return nil, status.Error(codes.InvalidArgument, "peer_public_key must be exactly 32 bytes")
	}

	var newKey [32]byte
	copy(newKey[:], rawKey)

	// Validate allowed_ips at the RPC boundary so malformed input returns
	// InvalidArgument rather than bubbling up as Internal from the manager.
	normalizedAllowedIPs := make([]string, 0, len(req.GetAllowedIps()))
	for _, cidr := range req.GetAllowedIps() {
		trimmedCIDR := strings.TrimSpace(cidr)
		if trimmedCIDR == "" {
			return nil, status.Error(codes.InvalidArgument, "allowed_ips entries must be non-empty CIDRs")
		}
		if _, _, parseErr := net.ParseCIDR(trimmedCIDR); parseErr != nil {
			return nil, status.Errorf(codes.InvalidArgument, "allowed_ips contains invalid CIDR %q", cidr)
		}
		normalizedAllowedIPs = append(normalizedAllowedIPs, trimmedCIDR)
	}

	unchanged, err := h.tunnelMgr.UpdateTunnelPeer(name, newKey, strings.TrimSpace(req.GetBalancerIp()), normalizedAllowedIPs)
	if err != nil {
		if strings.Contains(err.Error(), "tunnel not found") {
			return nil, status.Errorf(codes.NotFound, "tunnel not found: %s", name)
		}
		h.logger.Error().Err(err).Str("tunnel", name).Msg("update tunnel peer failed")
		// FR-5: when the underlying error hints at a key mismatch (wgctrl peer-replace
		// failed because the old key is no longer present in the WG device), emit a
		// structured error with the recovery hint. The hint deliberately says
		// "master remove + master init" — NOT "master reload", which issues the same
		// UpdateTunnelPeer path and would fail identically.
		errMsg := err.Error()
		if strings.Contains(errMsg, "wgctrl peer-replace") || strings.Contains(errMsg, "peer not found") {
			newKeyHex := hex.EncodeToString(rawKey)
			return nil, status.Errorf(codes.Internal,
				"update tunnel peer %q: key mismatch — admin sent %s but the WireGuard device "+
					"does not have this peer. Admin state has drifted from the master. "+
					"Recovery: mesh-ctl master remove <master> && mesh-ctl master init <master>. "+
					"Underlying: %v",
				name, newKeyHex[:min8(len(newKeyHex))], err)
		}
		return nil, status.Errorf(codes.Internal, "update tunnel peer: %v", err)
	}

	resp := &proto.UpdateTunnelPeerResponse{
		Success:   true,
		Unchanged: unchanged,
	}
	if h.keyProvider != nil {
		pubKey, kErr := h.keyProvider.GetPublicKey()
		if kErr != nil {
			h.logger.Warn().Err(kErr).Msg("update tunnel peer: failed to read master public key")
		} else {
			resp.MasterPublicKey = pubKey[:]
		}
	}

	return resp, nil
}

// min8 returns the smaller of n and 8.  Used to safely take a hex-prefix for
// error messages without panicking on keys shorter than 8 chars.
func min8(n int) int {
	if n < 8 {
		return n
	}
	return 8
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
		strings.TrimSpace(req.GetPeerName()),
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
			if err := tc.ConfigureTransport(pubkeyHex, localIP, peerIP, req.GetAllowedIps(), strings.TrimSpace(req.GetPeerName()), req.GetExtraRoutes()); err != nil {
				h.logger.Warn().Err(err).Msg("configure transport after AddPeer failed")
			}
		}
	}

	if cs, ok := h.peerMgr.(ClientStateSaver); ok {
		if err := cs.SaveClientState(); err != nil {
			h.logger.Warn().Err(err).Msg("save client state failed")
		}
	}

	return &proto.AddPeerResponse{Success: true}, nil
}

// nodeTransportState and tunnelTransport are type aliases for the shared
// transport state types in pkg/transport, eliminating the previous duplication.
type nodeTransportState = transport.NodeTransportState
type tunnelTransport = transport.TunnelTransport

func (h *AgentHandler) saveNodeTransportStateAfterPeerAdded(req *proto.AddPeerRequest) error {
	if req == nil || strings.TrimSpace(req.GetTransportSubnet()) == "" {
		return nil
	}

	// Fail fast at the RPC boundary when AllowedIPs is empty. Persisting a v1.6.0
	// schema tunnel with no AllowedIPs would cause reconcile to hard-error on the
	// next restart (FR-4). Rejecting here surfaces the mesh-ctl bug immediately
	// rather than on the next node boot.
	if len(req.GetAllowedIps()) == 0 {
		return fmt.Errorf("AddPeer: allowed_ips must be non-empty for transport persistence (v1.6.0 schema)")
	}

	state, err := loadNodeTransportState(h.configDir)
	if err != nil {
		return err
	}

	peerPublicKey := hex.EncodeToString(req.GetPublicKey())
	tunnelName, peerEndpoint := splitEndpointMetadata(strings.TrimSpace(req.GetEndpointHost()))
	if tunnelName != "" && (strings.Contains(tunnelName, "..") || strings.ContainsAny(tunnelName, "/\\")) {
		return fmt.Errorf("derived tunnel name %q contains unsafe path characters", tunnelName)
	}
	entry := tunnelTransport{
		Name:                tunnelName,
		TransportIP:         req.GetLocalTransportIp(),
		PeerTransportIP:     req.GetPeerTransportIp(),
		PeerPublicKey:       peerPublicKey,
		PeerEndpoint:        peerEndpoint,
		BalancerIP:          strings.TrimSpace(req.GetBalancerIp()),
		AllowedIPs:          req.GetAllowedIps(),
		PersistentKeepalive: req.GetPersistentKeepalive(),
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

	// FR-3: populate overlay_ip from the running node state when the disk state
	// has it empty. Endpoint nodes write transport.yml before their overlay IP is
	// known to the disk-only LoadNodeTransportState call above; fall back to
	// stateProvider (which reads from node.yml / --overlay-ip flag) so the field
	// is always populated after AddPeer completes.
	overlayIP := strings.TrimSpace(state.OverlayIP)
	if overlayIP == "" && h.stateProvider != nil {
		if nodeState := h.stateProvider.GetNodeState(); nodeState.OverlayIP != "" {
			overlayIP = strings.TrimSpace(nodeState.OverlayIP)
		}
	}

	h.logger.Info().
		Str("overlay_ip", overlayIP).
		Strs("allowed_ips", req.GetAllowedIps()).
		Msg("AddPeer: persisting transport state with allowed_ips")

	return saveNodeTransportState(filepath.Join(h.configDir, "transport.yml"), nodeTransportState{
		SchemaVersion: transport.CurrentSchemaVersion,
		OverlayIP:     overlayIP,
		Tunnels:       nextTunnels,
	})
}

// splitEndpointMetadata extracts (tunnelName, endpoint) from the endpointHost field.
// Preferred format: "name|host:port" (explicit separator).
// Legacy format: "host:port" (name derived from host) or plain hostname (name = hostname).
// Callers should prefer the "|" format to avoid tunnel name collisions.
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
	return transport.LoadNodeTransportState(configDir)
}

func saveNodeTransportState(path string, state nodeTransportState) error {
	return transport.SaveNodeTransportState(path, state)
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
		Bool("has_jc", cfg.Jc != nil).
		Bool("has_s1", cfg.S1 != nil).
		Bool("has_s2", cfg.S2 != nil).
		Bool("has_h1", cfg.H1 != nil).
		Bool("has_i1", cfg.I1 != nil).
		Msg("rotate params: applying AWG config via UAPI")

	// Apply AWG obfuscation parameters via UAPI.
	if err := h.paramApplier.ApplyParams(tunnelName, cfg); err != nil {
		h.logger.Error().Err(err).Str("tunnel", tunnelName).Int32("tier", req.GetTier()).Msg("rotate params: UAPI apply failed")
		return nil, status.Errorf(codes.Internal, "apply params: configure tunnel %q: %v", tunnelName, err)
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

// GetTransportState returns a read-only dump of this node's in-memory peer state
// for mesh-ctl inspect (v1.10.1). No private keys or PSKs are included.
// Pre-v1.10.1 nodes return codes.Unimplemented via the embedded UnimplementedAwgAgentServer.
func (h *AgentHandler) GetTransportState(_ context.Context, _ *proto.Empty) (*proto.TransportStateResponse, error) {
	var nodeName, mode, overlayIP string
	if h.stateProvider != nil {
		state := h.stateProvider.GetNodeState()
		nodeName = state.Name
		mode = state.Mode
		overlayIP = state.OverlayIP
	}

	// Load disk state once — used for allowed_ips enrichment and name lookup.
	diskState, diskErr := loadNodeTransportState(h.configDir)
	if diskErr != nil {
		h.logger.Warn().Err(diskErr).Msg("GetTransportState: could not load disk transport state")
		// diskState will be zero-value; we continue with runtime-only info.
	}
	if overlayIP == "" {
		overlayIP = diskState.OverlayIP
	}

	// Build a lookup map: peerPublicKeyHex → TunnelTransport (from disk).
	diskByKey := make(map[string]tunnelTransport, len(diskState.Tunnels))
	for _, tt := range diskState.Tunnels {
		if tt.PeerPublicKey != "" {
			diskByKey[tt.PeerPublicKey] = tt
		}
	}

	var peers []*proto.TransportPeerState

	// Build a lookup map: name → TunnelTransport (from disk) for name-based lookups.
	diskByName := make(map[string]tunnelTransport, len(diskState.Tunnels))
	for _, tt := range diskState.Tunnels {
		if tt.Name != "" {
			diskByName[tt.Name] = tt
		}
	}

	switch {
	case h.tunnelMgr != nil:
		// Master mode: runtime key comes from the live tunnel peer key.
		// Disk key and allowed IPs come from transport.yml, looked up by tunnel name.
		tunnels := h.tunnelMgr.ListTunnels()
		peers = make([]*proto.TransportPeerState, 0, len(tunnels))
		for _, t := range tunnels {
			runtimeKeyHex := hex.EncodeToString(t.PeerPublicKey)
			peer := &proto.TransportPeerState{
				Name:              t.Name,
				PublicKeyHex:      runtimeKeyHex,
				LastHandshakeUnix: 0, // not surfaced through TunnelManager interface
			}
			// Populate disk fields. Prefer name-based lookup; fall back to runtime-key lookup.
			if dt, ok := diskByName[t.Name]; ok {
				peer.DiskPublicKeyHex = dt.PeerPublicKey
				peer.DiskAllowedIps = append([]string(nil), dt.AllowedIPs...)
				peer.AllowedIps = append([]string(nil), dt.AllowedIPs...) // runtime IPs same source for master
			} else if dt, ok := diskByKey[runtimeKeyHex]; ok {
				peer.DiskPublicKeyHex = dt.PeerPublicKey
				peer.DiskAllowedIps = append([]string(nil), dt.AllowedIPs...)
				peer.AllowedIps = append([]string(nil), dt.AllowedIPs...)
			}
			peers = append(peers, peer)
		}

	case h.peerMgr != nil:
		// Endpoint mode: runtime key and allowed IPs come from live peer manager.
		// Disk key and name come from transport.yml, looked up by runtime key first.
		peerInfos := h.peerMgr.ListPeers()
		peers = make([]*proto.TransportPeerState, 0, len(peerInfos))
		for _, p := range peerInfos {
			runtimeKeyHex := hex.EncodeToString(p.PublicKey)
			name := runtimeKeyHex[:8] // fallback: first 8 hex chars
			var diskKeyHex string
			var diskAllowedIPs []string
			if dt, ok := diskByKey[runtimeKeyHex]; ok {
				if dt.Name != "" {
					name = dt.Name
				}
				diskKeyHex = dt.PeerPublicKey
				diskAllowedIPs = append([]string(nil), dt.AllowedIPs...)
			}
			peers = append(peers, &proto.TransportPeerState{
				Name:              name,
				PublicKeyHex:      runtimeKeyHex,
				AllowedIps:        append([]string(nil), p.AllowedIPs...),
				LastHandshakeUnix: p.LastHandshake,
				DiskPublicKeyHex:  diskKeyHex,
				DiskAllowedIps:    diskAllowedIPs,
			})
		}

	default:
		// Neither manager available; return disk state only (runtime = disk).
		peers = make([]*proto.TransportPeerState, 0, len(diskState.Tunnels))
		for _, tt := range diskState.Tunnels {
			peers = append(peers, &proto.TransportPeerState{
				Name:             tt.Name,
				PublicKeyHex:     tt.PeerPublicKey,
				AllowedIps:       append([]string(nil), tt.AllowedIPs...),
				DiskPublicKeyHex: tt.PeerPublicKey,
				DiskAllowedIps:   append([]string(nil), tt.AllowedIPs...),
			})
		}
	}

	return &proto.TransportStateResponse{
		NodeName:  nodeName,
		Mode:      mode,
		OverlayIp: overlayIP,
		Peers:     peers,
	}, nil
}

// RotateToken updates the node's MESH_TOKEN hash atomically (write-to-temp + rename).
func (h *AgentHandler) RotateToken(_ context.Context, req *proto.RotateTokenRequest) (*proto.RotateTokenResponse, error) {
	newHash := req.NewTokenHash
	if len(newHash) == 0 || len(newHash) > 100 {
		return nil, status.Errorf(codes.InvalidArgument, "token hash must be 1-100 bytes")
	}
	if _, err := bcrypt.Cost([]byte(newHash)); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid bcrypt hash: %v", err)
	}

	tokenPath := filepath.Join(h.configDir, "mesh.token")
	tmpPath := tokenPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(newHash), 0600); err != nil {
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

// RotateKeypair atomically persists and applies a new WireGuard private key on
// endpoint-mode nodes. Master and client nodes return codes.Unimplemented
// because h.statePersister is nil for those modes.
//
// Security: private key bytes are NEVER written to any log at any level.
func (h *AgentHandler) RotateKeypair(ctx context.Context, req *proto.RotateKeypairRequest) (*proto.RotateKeypairResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}

	// Gate: only endpoint mode has a statePersister injected.
	if h.statePersister == nil {
		return nil, status.Error(codes.Unimplemented, "keypair rotation not available in this mode")
	}

	// Validate key length first so we return InvalidArgument before locking.
	if len(req.PrivateKey) != 32 {
		return nil, status.Errorf(codes.InvalidArgument, "private_key must be exactly 32 bytes, got %d", len(req.PrivateKey))
	}

	tunnelName := strings.TrimSpace(req.TunnelName)
	if tunnelName == "" {
		return nil, status.Error(codes.InvalidArgument, "tunnel_name is required")
	}
	if !validTunnelName.MatchString(tunnelName) {
		return nil, status.Errorf(codes.InvalidArgument, "tunnel_name %q must match [a-zA-Z0-9_-]{1,12}", tunnelName)
	}

	// Serialize concurrent rotate RPCs for this node via the persister lock.
	unlock, err := h.statePersister.LockRotation(tunnelName)
	if err != nil {
		h.logger.Error().Err(err).Str("tunnel", tunnelName).Msg("rotate keypair: lock failed")
		return nil, status.Errorf(codes.Internal, "acquire rotation lock: %v", err)
	}
	defer unlock()
	h.logger.Info().Str("tunnel", tunnelName).Msg("rotate keypair: lock acquired")

	// Snapshot the existing private key before any mutation so we can roll back
	// the persisted state if UAPI apply fails. An os.ErrNotExist here means
	// there was no prior state — nothing to roll back to, which is acceptable
	// on first-time rotation.
	oldPrivKey, loadErr := h.statePersister.LoadKeypair(tunnelName)
	if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
		h.logger.Error().Err(loadErr).Str("tunnel", tunnelName).
			Msg("rotate keypair: load existing keypair failed")
		return nil, status.Errorf(codes.Internal, "load existing keypair: %v", loadErr)
	}

	// Persist the new private key atomically before touching the data plane.
	if persistErr := h.statePersister.PersistKeypair(tunnelName, req.PrivateKey); persistErr != nil {
		h.logger.Error().Err(persistErr).Str("tunnel", tunnelName).
			Msg("rotate keypair: persist failed")
		return nil, status.Errorf(codes.Internal, "persist keypair: %v", persistErr)
	}
	h.logger.Info().Str("tunnel", tunnelName).Int("key_len", len(req.PrivateKey)).
		Msg("rotate keypair: persisted to disk")

	// Copy into wg.Key and derive the public key via curve25519 scalar mult.
	var newPrivKey wg.Key
	copy(newPrivKey[:], req.PrivateKey)
	newPubKey := newPrivKey.PublicKey()

	// Apply to the live WireGuard interface via UAPI.
	if h.paramApplier != nil {
		cfg := wg.Config{PrivateKey: &newPrivKey}
		if applyErr := h.paramApplier.ApplyParams(tunnelName, cfg); applyErr != nil {
			h.logger.Error().Err(applyErr).Str("tunnel", tunnelName).
				Msg("rotate keypair: uapi apply failed")
			// Best-effort rollback: restore the persisted state so disk does not
			// diverge from runtime (which is still on the old key after ApplyParams
			// failed). If there was no prior state (first-time rotation), skip.
			if len(oldPrivKey) == 32 {
				if rbErr := h.statePersister.PersistKeypair(tunnelName, oldPrivKey); rbErr != nil {
					h.logger.Error().Err(rbErr).Str("tunnel", tunnelName).
						Msg("rotate keypair: rollback of persisted key FAILED — disk/runtime divergence")
					return nil, status.Errorf(codes.Internal,
						"apply keypair to uapi: %v (rollback also failed: %v — run 'mesh-ctl reconcile')", applyErr, rbErr)
				}
				h.logger.Warn().Str("tunnel", tunnelName).
					Msg("rotate keypair: rolled back persisted key after uapi apply failure")
			}
			return nil, status.Errorf(codes.Internal, "apply keypair to uapi: %v", applyErr)
		}
		h.logger.Info().Str("tunnel", tunnelName).Msg("rotate keypair: uapi applied")
	}

	return &proto.RotateKeypairResponse{NewPublicKey: newPubKey[:]}, nil
}

// mapParamsToConfig converts proto AWG params to wg.Config.
// Note: zero-value fields (e.g. Jc=0) are indistinguishable from "not set" in proto3
// and will be skipped. To clear a parameter, use the rotation API which replaces all params.
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
