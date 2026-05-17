package control_plane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/role"
	meshrotation "github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/rotation"
	pb "github.com/coonfuuseed-paandaa/awg-mesh/v2/proto/control_plane"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Server implements pb.ControlPlaneServer. Wire it into a grpc.Server
// via pb.RegisterControlPlaneServer.
//
// CR-002 implements the minimum-viable subset:
//
//	RegisterNode, Heartbeat, StreamPeerList, StreamOwnership, DecommissionNode
//
// Streaming RPCs that depend on yet-unwritten subsystems return Unimplemented:
//
//	SignalExchange        → CR-007 NAT signal relay
//	StreamServiceRegistry → CR-006 ingress mode (ServiceRegistry)
type Server struct {
	pb.UnimplementedControlPlaneServer
	registry       *Registry
	ledger         *Ledger
	audit          *AuditLog
	peerListBcast  *PeerListBroadcaster
	ownershipBcast *OwnershipBroadcaster
	rotation       *meshrotation.Orchestrator
	certLifecycle  *CertLifecycle
	onRegister     func(RegisteredNode) error
}

// NewServer wires the provided dependencies into a Server. The ledger
// listener is set so every Reassign / Remove fans out via the broadcasters.
func NewServer(registry *Registry, ledger *Ledger, audit *AuditLog) *Server {
	s := &Server{
		registry:       registry,
		ledger:         ledger,
		audit:          audit,
		peerListBcast:  NewPeerListBroadcaster(),
		ownershipBcast: NewOwnershipBroadcaster(),
		rotation:       meshrotation.NewOrchestrator(meshrotation.NewMemoryApplier(), meshrotation.OrchestratorConfig{}),
	}
	ledger.SetListener(ledgerListenerFn(func(snap []OwnershipEntry, version int64) {
		s.ownershipBcast.Broadcast(OwnershipUpdateMsg{Entries: snap, Version: version})
		s.peerListBcast.Broadcast(PeerListUpdateMsg{Snapshot: snap, Version: version})
	}))
	return s
}

// SetRegistrationObserver installs a post-registration integration hook.
func (s *Server) SetRegistrationObserver(observer func(RegisteredNode) error) {
	if s == nil {
		return
	}
	s.onRegister = observer
}

func (s *Server) notifyRegistrationObserver(node RegisteredNode) error {
	if s.onRegister == nil {
		return nil
	}
	if err := s.onRegister(node); err != nil {
		return fmt.Errorf("registration observer: %w", err)
	}
	return nil
}

// RegisterNode handles initial registration. The node cert MUST be present
// (operators provision it during onboarding). Returns Accepted=false with a
// reject_reason on validation failure.
func (s *Server) RegisterNode(ctx context.Context, req *pb.RegisterNodeRequest) (*pb.RegisterNodeResponse, error) {
	if req.GetNodeName() == "" {
		return &pb.RegisterNodeResponse{Accepted: false, RejectReason: "node_name required"}, nil
	}
	roles := make([]role.Role, 0, len(req.GetRoles()))
	for _, r := range req.GetRoles() {
		roles = append(roles, role.Role(r))
	}
	node := RegisteredNode{
		Name:         req.GetNodeName(),
		Roles:        roles,
		OverlayIP:    req.GetOverlayIp(),
		Region:       req.GetRegion(),
		NodeCertPEM:  req.GetNodeCertPem(),
		NodeVersion:  req.GetNodeVersion(),
		Pubkey:       req.GetPubkey(),
		EndpointHost: req.GetEndpointHost(),
		Protocol:     req.GetProtocol(),
	}
	if err := s.registry.Register(node); err != nil {
		s.audit.Append(AuditEvent{
			EventType: "register-rejected",
			NodeName:  req.GetNodeName(),
			Detail:    err.Error(),
		})
		return &pb.RegisterNodeResponse{Accepted: false, RejectReason: err.Error()}, nil
	}
	if slices.Contains(roles, role.RoleMaster) {
		if _, err := s.ledger.Reassign(req.GetOverlayIp(), req.GetNodeName(), "register"); err != nil {
			s.audit.Append(AuditEvent{
				EventType: "register-rejected",
				NodeName:  req.GetNodeName(),
				Detail:    err.Error(),
			})
			if removeErr := s.registry.Remove(req.GetNodeName()); removeErr != nil {
				s.audit.Append(AuditEvent{
					EventType: "register-rollback-failed",
					NodeName:  req.GetNodeName(),
					Detail:    removeErr.Error(),
				})
			}
			return &pb.RegisterNodeResponse{Accepted: false, RejectReason: err.Error()}, nil
		}
	}
	saved, ok := s.registry.Lookup(req.GetNodeName())
	if !ok {
		s.audit.Append(AuditEvent{
			EventType: "register-rejected",
			NodeName:  req.GetNodeName(),
			Detail:    "registered node missing after registry write",
		})
		return &pb.RegisterNodeResponse{Accepted: false, RejectReason: "registered node missing after registry write"}, nil
	}
	if err := s.seedRegistrationOwnership(saved); err != nil {
		s.audit.Append(AuditEvent{
			EventType: "register-rejected",
			NodeName:  req.GetNodeName(),
			Detail:    err.Error(),
		})
		if removeErr := s.registry.Remove(req.GetNodeName()); removeErr != nil {
			s.audit.Append(AuditEvent{
				EventType: "register-rollback-failed",
				NodeName:  req.GetNodeName(),
				Detail:    removeErr.Error(),
			})
		}
		return &pb.RegisterNodeResponse{Accepted: false, RejectReason: err.Error()}, nil
	}
	if err := s.notifyRegistrationObserver(saved); err != nil {
		s.audit.Append(AuditEvent{
			EventType: "register-rejected",
			NodeName:  req.GetNodeName(),
			Detail:    err.Error(),
		})
		if removeErr := s.registry.Remove(req.GetNodeName()); removeErr != nil {
			s.audit.Append(AuditEvent{
				EventType: "register-rollback-failed",
				NodeName:  req.GetNodeName(),
				Detail:    removeErr.Error(),
			})
		}
		return &pb.RegisterNodeResponse{Accepted: false, RejectReason: err.Error()}, nil
	}
	s.audit.Append(AuditEvent{
		EventType: "register",
		NodeName:  req.GetNodeName(),
		Detail:    fmt.Sprintf("roles=%v overlay=%s region=%s", req.GetRoles(), req.GetOverlayIp(), req.GetRegion()),
	})
	return &pb.RegisterNodeResponse{
		Accepted:         true,
		RegisteredAtUnix: saved.RegisteredAt.Unix(),
	}, nil
}

// Heartbeat is a bidirectional stream. The node sends a HeartbeatRequest at
// configured cadence; the server responds with the current server time and a
// stale flag (set when the registry believes the node should re-register).
func (s *Server) Heartbeat(stream pb.ControlPlane_HeartbeatServer) error {
	for {
		req, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(stream.Context().Err(), context.Canceled) || errors.Is(stream.Context().Err(), context.DeadlineExceeded) {
				return nil
			}
			return err
		}
		hasPeerIdentity := len(req.GetPubkey()) > 0 || req.GetEndpointHost() != "" || req.GetProtocol() != ""
		if err := s.registry.Heartbeat(req.GetNodeName(), req.GetHealth(), req.GetPubkey(), req.GetEndpointHost(), req.GetProtocol()); err != nil {
			return status.Errorf(codes.NotFound, "node %q not registered", req.GetNodeName())
		}
		if hasPeerIdentity {
			saved, ok := s.registry.Lookup(req.GetNodeName())
			if !ok {
				return status.Errorf(codes.NotFound, "node %q not registered", req.GetNodeName())
			}
			if err := s.seedRegistrationOwnership(saved); err != nil {
				s.audit.Append(AuditEvent{
					EventType: "heartbeat-rejected",
					NodeName:  req.GetNodeName(),
					Detail:    err.Error(),
				})
				return status.Errorf(codes.Internal, "%v", err)
			}
			if err := s.notifyRegistrationObserver(saved); err != nil {
				s.audit.Append(AuditEvent{
					EventType: "heartbeat-rejected",
					NodeName:  req.GetNodeName(),
					Detail:    err.Error(),
				})
				return status.Errorf(codes.Internal, "%v", err)
			}
		}
		s.audit.Append(AuditEvent{
			EventType: "heartbeat",
			NodeName:  req.GetNodeName(),
		})
		resp := &pb.HeartbeatResponse{
			ServerAtUnix: time.Now().UTC().Unix(),
			Stale:        false,
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

func (s *Server) seedRegistrationOwnership(node RegisteredNode) error {
	if slices.Contains(node.Roles, role.RoleMaster) {
		return seedExistingRegisteredNodeOwnership(s.registry, s.ledger)
	}
	return seedRegisteredNodeOwnership(s.registry, s.ledger, node)
}

func seedExistingRegisteredNodeOwnership(registry *Registry, ledger *Ledger) error {
	for _, node := range registry.List() {
		if err := seedRegisteredNodeOwnership(registry, ledger, node); err != nil {
			return err
		}
	}
	return nil
}

func seedRegisteredNodeOwnership(registry *Registry, ledger *Ledger, node RegisteredNode) error {
	if slices.Contains(node.Roles, role.RoleMaster) || slices.Contains(node.Roles, role.RoleClient) {
		return nil
	}
	owner, ok := singleSelfRegisteredMaster(registry, ledger)
	if !ok {
		return nil
	}
	if _, ok := ledger.Lookup(node.OverlayIP); ok {
		return nil
	}
	if _, err := ledger.Reassign(node.OverlayIP, owner, "register"); err != nil {
		return fmt.Errorf("seed ownership for registered node %q: %w", node.Name, err)
	}
	return nil
}

func singleSelfRegisteredMaster(registry *Registry, ledger *Ledger) (string, bool) {
	snapshot, _ := ledger.Snapshot()
	owner := ""
	for _, entry := range snapshot {
		if entry.OwningMaster == "" {
			continue
		}
		node, ok := registry.Lookup(entry.OwningMaster)
		if !ok || !slices.Contains(node.Roles, role.RoleMaster) || node.OverlayIP != entry.OverlayIP {
			continue
		}
		if owner != "" && owner != entry.OwningMaster {
			return "", false
		}
		owner = entry.OwningMaster
	}
	return owner, owner != ""
}

// StreamPeerList: server-streaming. The first message is a full snapshot; we
// then push deltas as the ledger mutates.
func (s *Server) StreamPeerList(req *pb.StreamPeerListRequest, stream pb.ControlPlane_StreamPeerListServer) error {
	subject := req.GetNodeName()
	if subject == "" {
		return status.Error(codes.InvalidArgument, "node_name required")
	}
	// Initial snapshot.
	snapshot, version := s.ledger.Snapshot()
	if err := stream.Send(s.buildPeerListUpdate(subject, snapshot, version)); err != nil {
		return err
	}
	// Subscribe for deltas.
	ch, cancel := s.peerListBcast.Subscribe(subject)
	defer cancel()
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case msg, ok := <-ch:
			if !ok {
				return status.Error(codes.Aborted, "peer-list subscription dropped (slow consumer)")
			}
			if err := stream.Send(s.buildPeerListUpdate(subject, msg.Snapshot, msg.Version)); err != nil {
				return err
			}
		}
	}
}

// StreamOwnership: server-streaming. First message is a full snapshot with
// FullSnapshot=true; subsequent updates carry deltas.
func (s *Server) StreamOwnership(req *pb.StreamOwnershipRequest, stream pb.ControlPlane_StreamOwnershipServer) error {
	snapshot, version := s.ledger.Snapshot()
	first := &pb.OwnershipUpdate{
		Entries:      ownershipToProto(snapshot),
		Version:      version,
		FullSnapshot: true,
	}
	if err := stream.Send(first); err != nil {
		return err
	}
	ch, cancel := s.ownershipBcast.Subscribe()
	defer cancel()
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case msg, ok := <-ch:
			if !ok {
				return status.Error(codes.Aborted, "ownership subscription dropped (slow consumer)")
			}
			update := &pb.OwnershipUpdate{
				Entries:      ownershipToProto(msg.Entries),
				Version:      msg.Version,
				FullSnapshot: msg.FullSnapshot,
			}
			if err := stream.Send(update); err != nil {
				return err
			}
		}
	}
}

// DecommissionNode: drains all overlay /32s owned by the node, removes it from
// the registry, and emits an audit entry. Successor master is selected by
// region affinity (FR-15).
func (s *Server) DecommissionNode(ctx context.Context, req *pb.DecommissionRequest) (*pb.DecommissionResponse, error) {
	name := req.GetNodeName()
	if name == "" {
		return &pb.DecommissionResponse{Success: false, Error: "node_name required"}, nil
	}
	target, ok := s.registry.Lookup(name)
	if !ok {
		return &pb.DecommissionResponse{Success: false, Error: "node not in registry"}, nil
	}

	var chooser func(overlayIP string) string
	if len(s.ledger.OwnedBy(name)) > 0 {
		candidates := s.registry.MastersInRegion(target.Region)
		candidates = filterOut(candidates, name)
		if len(candidates) == 0 {
			// Region empty — fall back to any master mesh-wide.
			candidates = filterOut(s.registry.MastersInRegion(""), name)
		}
		if len(candidates) == 0 {
			return &pb.DecommissionResponse{Success: false, Error: "no surviving master available for reassignment"}, nil
		}
		chooser = roundRobinChooser(candidates)
	}

	count, err := s.ledger.Drain(name, "decommission", chooser)
	if err != nil {
		s.audit.Append(AuditEvent{
			EventType: "decommission-failed",
			NodeName:  name,
			Detail:    err.Error(),
		})
		return &pb.DecommissionResponse{Success: false, Error: err.Error(), ReassignedOverlayCount: int32(count)}, nil
	}
	if err := s.registry.Remove(name); err != nil {
		// Ledger is drained but registry remove failed — still report partial success.
		s.audit.Append(AuditEvent{
			EventType: "decommission-partial",
			NodeName:  name,
			Detail:    fmt.Sprintf("drained=%d registry_remove=%v", count, err),
		})
		return &pb.DecommissionResponse{Success: false, Error: err.Error(), ReassignedOverlayCount: int32(count)}, nil
	}
	s.audit.Append(AuditEvent{
		EventType: "decommission",
		NodeName:  name,
		Detail:    fmt.Sprintf("reassigned=%d", count),
	})
	return &pb.DecommissionResponse{Success: true, ReassignedOverlayCount: int32(count)}, nil
}

// QueryAudit: server-streaming the matching events.
func (s *Server) QueryAudit(req *pb.QueryAuditRequest, stream pb.ControlPlane_QueryAuditServer) error {
	since := unixOrZero(req.GetSinceUnix())
	until := unixOrZero(req.GetUntilUnix())
	limit := int(req.GetLimit())
	events := s.audit.Query(since, until, req.GetEventTypeFilter(), req.GetNodeFilter(), limit)
	for _, e := range events {
		out := &pb.AuditEntry{
			TsUnix:    e.Timestamp.Unix(),
			EventType: e.EventType,
			NodeName:  e.NodeName,
			Detail:    e.Detail,
			Actor:     e.Actor,
		}
		if err := stream.Send(out); err != nil {
			return err
		}
	}
	return nil
}

// RotateAWGParamsMeshWide executes one mesh-wide AWG rotation request and
// streams one ACK/ERROR result per mesh-internal target.
func (s *Server) RotateAWGParamsMeshWide(stream pb.ControlPlane_RotateAWGParamsMeshWideServer) error {
	req, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return status.Error(codes.InvalidArgument, "rotate request required")
		}
		return err
	}
	if s.rotation == nil {
		s.rotation = meshrotation.NewOrchestrator(meshrotation.NewMemoryApplier(), meshrotation.OrchestratorConfig{})
	}
	rotationReq := meshrotation.Request{
		Tier:       req.GetTier(),
		Params:     req.GetNewParams(),
		ApplyAt:    unixMicrosOrZero(req.GetApplyAtUnixMicros()),
		RotationID: req.GetRotationId(),
		Reason:     "control-plane",
		Targets:    rotationTargetsFromRegistry(s.registry.List()),
	}
	meshInternalTargetCount := len(meshrotation.MeshInternalTargets(rotationReq.Targets))
	s.audit.Append(AuditEvent{
		EventType: "rotate-start",
		Detail:    fmt.Sprintf("tier=%s rotation_id=%s targets=%d", req.GetTier(), req.GetRotationId(), meshInternalTargetCount),
		Actor:     "operator",
	})
	exec, execErr := s.rotation.Execute(stream.Context(), rotationReq)
	if len(exec.Results) > 0 {
		if err := streamRotationResults(stream, exec); err != nil {
			return err
		}
	}
	s.audit.Append(AuditEvent{
		EventType: "rotate-result",
		Detail:    fmt.Sprintf("rotation_id=%s status=%s results=%d", exec.Plan.RotationID, exec.Status, len(exec.Results)),
		Actor:     "operator",
	})
	if execErr == nil {
		return nil
	}
	if errors.Is(execErr, meshrotation.ErrNoRotationTargets) {
		return status.Error(codes.FailedPrecondition, execErr.Error())
	}
	if errors.Is(execErr, meshrotation.ErrPartialApply) {
		return status.Errorf(codes.Aborted, "rotation partial apply: %s", exec.Plan.RotationID)
	}
	return status.Error(codes.InvalidArgument, execErr.Error())
}

// --- Streams handled by later CRs. ---

func (s *Server) SignalExchange(stream pb.ControlPlane_SignalExchangeServer) error {
	return status.Error(codes.Unimplemented, "NAT signal relay lands in CR-007")
}

func (s *Server) StreamServiceRegistry(req *pb.StreamServiceRegistryRequest, stream pb.ControlPlane_StreamServiceRegistryServer) error {
	return status.Error(codes.Unimplemented, "service registry lands in CR-006")
}

func (s *Server) StreamCertUpdate(req *pb.StreamCertRequest, stream pb.ControlPlane_StreamCertUpdateServer) error {
	name := req.GetNodeName()
	if name == "" {
		return status.Error(codes.InvalidArgument, "node_name required")
	}
	authenticatedName, err := authenticatedStreamNodeName(stream.Context())
	if err != nil {
		return status.Error(codes.Unauthenticated, err.Error())
	}
	if authenticatedName != name {
		return status.Errorf(codes.Unauthenticated, "authenticated node %q cannot request cert update for %q", authenticatedName, name)
	}
	if s.certLifecycle == nil {
		return status.Error(codes.FailedPrecondition, "cert lifecycle is not configured")
	}
	for {
		node, ok := s.registry.Lookup(name)
		if !ok {
			return status.Errorf(codes.NotFound, "node %q not registered", name)
		}
		update, overlapUntil, due, err := s.certLifecycle.NextDueUpdate(node)
		if err != nil {
			return status.Error(codes.FailedPrecondition, err.Error())
		}
		if due {
			if err := s.registry.AllowCertRollover(name, update.GetCertPem(), overlapUntil); err != nil {
				if errors.Is(err, ErrRegistryNotFound) {
					return status.Errorf(codes.NotFound, "node %q not registered", name)
				}
				if errors.Is(err, ErrRegistryPendingCert) {
					return status.Error(codes.Aborted, err.Error())
				}
				return status.Error(codes.Internal, err.Error())
			}
			s.audit.Append(AuditEvent{
				EventType: "cert-update-issued",
				NodeName:  name,
				Detail:    fmt.Sprintf("valid_until=%d overlap_until=%d", update.GetValidUntilUnix(), overlapUntil.Unix()),
			})
			if err := stream.Send(update); err != nil {
				if rollbackErr := s.registry.ClearCertRollover(name, update.GetCertPem()); rollbackErr != nil && !errors.Is(rollbackErr, ErrRegistryNotFound) {
					return status.Errorf(codes.Internal, "rollback pending cert after send failure: %v", rollbackErr)
				}
				if stream.Context().Err() != nil {
					return nil
				}
				return err
			}
			continue
		}

		timer := time.NewTimer(s.certLifecycle.DelayUntilDue(node))
		select {
		case <-stream.Context().Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func authenticatedStreamNodeName(ctx context.Context) (string, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", errors.New("mTLS peer identity required")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return "", errors.New("mTLS peer identity required")
	}
	if len(tlsInfo.State.VerifiedChains) == 0 || len(tlsInfo.State.PeerCertificates) == 0 {
		return "", errors.New("verified client certificate required")
	}
	cert := tlsInfo.State.PeerCertificates[0]
	if cert.Subject.CommonName != "" {
		return cert.Subject.CommonName, nil
	}
	if len(cert.DNSNames) > 0 {
		return cert.DNSNames[0], nil
	}
	return "", errors.New("client certificate identity is empty")
}

// --- Helpers -----------------------------------------------------------------

func (s *Server) buildPeerListUpdate(subject string, snapshot []OwnershipEntry, version int64) *pb.PeerListUpdate {
	// Step 1: Group overlay IPs by owning master, preserving first-seen order.
	type masterAgg struct {
		allowedIPs []string
		overlayIP  string // first overlay IP (for PeerOverlayIp field)
	}
	byMaster := make(map[string]*masterAgg)
	var masterOrder []string
	for _, e := range snapshot {
		agg, ok := byMaster[e.OwningMaster]
		if !ok {
			agg = &masterAgg{overlayIP: e.OverlayIp()}
			byMaster[e.OwningMaster] = agg
			masterOrder = append(masterOrder, e.OwningMaster)
		}
		agg.allowedIPs = append(agg.allowedIPs, e.OverlayIp()+"/32")
	}

	// Step 2: Build one PeerEntry per unique master, enriched from the registry.
	peers := make([]*pb.PeerEntry, 0, len(byMaster))
	for _, masterName := range masterOrder {
		agg := byMaster[masterName]
		entry := &pb.PeerEntry{
			PeerName:      masterName,
			PeerOverlayIp: agg.overlayIP,
			AllowedIps:    agg.allowedIPs,
		}
		if node, ok := s.registry.Lookup(masterName); ok {
			entry.PeerPubkey = node.Pubkey
			entry.PeerEndpointHost = node.EndpointHost
			entry.Protocol = node.Protocol
		}
		peers = append(peers, entry)
	}
	return &pb.PeerListUpdate{SubjectNode: subject, Peers: peers, Version: version}
}

// OverlayIp is a small helper so we don't have to fmt-import in the server file.
func (e OwnershipEntry) OverlayIp() string { return e.OverlayIP }

func ownershipToProto(in []OwnershipEntry) []*pb.OwnershipEntry {
	out := make([]*pb.OwnershipEntry, 0, len(in))
	for _, e := range in {
		out = append(out, &pb.OwnershipEntry{
			OverlayIp:            e.OverlayIP,
			OwningMaster:         e.OwningMaster,
			LastReassignedAtUnix: e.LastReassignedAt.Unix(),
			Reason:               e.Reason,
		})
	}
	return out
}

func filterOut(in []string, exclude string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != exclude {
			out = append(out, s)
		}
	}
	return out
}

func roundRobinChooser(masters []string) func(string) string {
	i := 0
	return func(string) string {
		if len(masters) == 0 {
			return ""
		}
		m := masters[i%len(masters)]
		i++
		return m
	}
}

func unixOrZero(t int64) time.Time {
	if t == 0 {
		return time.Time{}
	}
	return time.Unix(t, 0).UTC()
}

func unixMicrosOrZero(t int64) time.Time {
	if t == 0 {
		return time.Time{}
	}
	return time.UnixMicro(t).UTC()
}

func rotationTargetsFromRegistry(nodes []RegisteredNode) []meshrotation.Target {
	targets := make([]meshrotation.Target, 0, len(nodes))
	for _, node := range nodes {
		targets = append(targets, meshrotation.Target{
			Name:      node.Name,
			Roles:     append([]role.Role(nil), node.Roles...),
			OverlayIP: node.OverlayIP,
		})
	}
	return targets
}

func streamRotationResults(stream pb.ControlPlane_RotateAWGParamsMeshWideServer, exec meshrotation.Execution) error {
	for _, result := range exec.Results {
		resp := &pb.RotateResponse{
			NodeName:   result.Target.Name,
			RotationId: exec.Plan.RotationID,
			Ack:        result.Ack,
			Error:      result.Error,
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
	return nil
}

// ledgerListenerFn lets us pass closure-style listeners to Ledger.SetListener.
type ledgerListenerFn func([]OwnershipEntry, int64)

func (f ledgerListenerFn) OnLedgerMutation(snap []OwnershipEntry, version int64) {
	f(snap, version)
}
