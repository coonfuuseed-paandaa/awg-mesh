package control_plane

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/role"
	pb "github.com/coonfuuseed-paandaa/awg-mesh/proto/control_plane"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// startTestServer wires registry+ledger+audit+server into an in-process gRPC
// server and returns a connected client + teardown.
func startTestServer(t *testing.T) (pb.ControlPlaneClient, *Server, func()) {
	t.Helper()
	registry := NewRegistry()
	ledger := NewLedger()
	audit := NewAuditLog(64)
	srv := NewServer(registry, ledger, audit)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	gs := grpc.NewServer()
	pb.RegisterControlPlaneServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		gs.Stop()
		t.Fatalf("dial: %v", err)
	}
	client := pb.NewControlPlaneClient(conn)
	teardown := func() {
		_ = conn.Close()
		gs.Stop()
	}
	return client, srv, teardown
}

func TestServer_RegisterNode_Accept(t *testing.T) {
	client, _, teardown := startTestServer(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := client.RegisterNode(ctx, &pb.RegisterNodeRequest{
		NodeName:    "master-01",
		Roles:       []string{"master"},
		NodeCertPem: fakeCert,
		OverlayIp:   "10.0.0.1",
		Region:      "ru",
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	if !resp.GetAccepted() {
		t.Fatalf("expected accepted, got rejected: %s", resp.GetRejectReason())
	}
}

func TestServer_RegisterNode_Reject_RoleConflict(t *testing.T) {
	client, _, teardown := startTestServer(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := client.RegisterNode(ctx, &pb.RegisterNodeRequest{
		NodeName:    "x",
		Roles:       []string{"client", "master"},
		NodeCertPem: fakeCert,
		OverlayIp:   "10.0.0.1",
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	if resp.GetAccepted() {
		t.Fatalf("expected reject for client+master role")
	}
}

func TestServer_RegisterNode_MasterSeedsOwnershipLedger(t *testing.T) {
	client, srv, teardown := startTestServer(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := client.RegisterNode(ctx, &pb.RegisterNodeRequest{
		NodeName:    "master-seed",
		Roles:       []string{"master"},
		NodeCertPem: fakeCert,
		OverlayIp:   "10.0.0.10",
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	if !resp.GetAccepted() {
		t.Fatalf("expected accepted, got %q", resp.GetRejectReason())
	}
	entry, ok := srv.ledger.Lookup("10.0.0.10")
	if !ok {
		t.Fatalf("master overlay was not seeded into ledger")
	}
	if entry.OwningMaster != "master-seed" || entry.Reason != "register" {
		t.Fatalf("unexpected ownership entry: %+v", entry)
	}
}

func TestServer_RegisterNode_ClientDoesNotSeedOwnershipLedger(t *testing.T) {
	client, srv, teardown := startTestServer(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := client.RegisterNode(ctx, &pb.RegisterNodeRequest{
		NodeName:    "client-01",
		Roles:       []string{"client"},
		NodeCertPem: fakeCert,
		OverlayIp:   "10.0.0.20",
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	if !resp.GetAccepted() {
		t.Fatalf("expected accepted, got %q", resp.GetRejectReason())
	}
	if _, ok := srv.ledger.Lookup("10.0.0.20"); ok {
		t.Fatalf("client must not self-own its overlay in the ledger")
	}
}

func TestServer_Heartbeat_Roundtrip(t *testing.T) {
	client, srv, teardown := startTestServer(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := client.RegisterNode(ctx, &pb.RegisterNodeRequest{
		NodeName: "n1", Roles: []string{"master"}, NodeCertPem: fakeCert, OverlayIp: "10.0.0.1",
	}); err != nil {
		t.Fatal(err)
	}

	stream, err := client.Heartbeat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&pb.HeartbeatRequest{NodeName: "n1", SentAtUnix: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	resp, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetServerAtUnix() == 0 {
		t.Fatalf("server time empty")
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}

	// Verify registry now has heartbeat timestamp.
	got, ok := srv.registry.Lookup("n1")
	if !ok || got.LastHeartbeatAt.IsZero() {
		t.Fatalf("heartbeat did not update registry")
	}
}

func TestServer_Heartbeat_UnknownNodeError(t *testing.T) {
	client, _, teardown := startTestServer(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stream, err := client.Heartbeat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&pb.HeartbeatRequest{NodeName: "ghost"}); err != nil {
		t.Fatal(err)
	}
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("expected NotFound error for unknown node")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestServer_StreamOwnership_InitialSnapshot(t *testing.T) {
	client, srv, teardown := startTestServer(t)
	defer teardown()
	// Seed ledger.
	if _, err := srv.ledger.Reassign("172.21.92.10", "master-01", "scheduled"); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.ledger.Reassign("172.21.92.11", "master-01", "scheduled"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stream, err := client.StreamOwnership(ctx, &pb.StreamOwnershipRequest{SubscriberNode: "client-01"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if !first.GetFullSnapshot() {
		t.Fatal("first message must have full_snapshot=true")
	}
	if len(first.GetEntries()) != 2 {
		t.Fatalf("entries = %d, want 2", len(first.GetEntries()))
	}
}

func TestServer_StreamOwnership_LiveUpdate(t *testing.T) {
	client, srv, teardown := startTestServer(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := client.StreamOwnership(ctx, &pb.StreamOwnershipRequest{SubscriberNode: "obs"})
	if err != nil {
		t.Fatal(err)
	}
	// Initial empty snapshot.
	first, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if !first.GetFullSnapshot() {
		t.Fatal("expected full_snapshot=true on first")
	}
	// Drive a mutation.
	go func() {
		time.Sleep(50 * time.Millisecond)
		if _, err := srv.ledger.Reassign("10.0.0.5", "master-01", "scheduled"); err != nil {
			t.Errorf("Reassign: %v", err)
		}
	}()
	upd, err := stream.Recv()
	if err != nil {
		t.Fatalf("expected live update: %v", err)
	}
	if len(upd.GetEntries()) != 1 || upd.GetEntries()[0].GetOverlayIp() != "10.0.0.5" {
		t.Fatalf("update payload mismatch: %+v", upd.GetEntries())
	}
}

func TestServer_StreamPeerList_LiveUpdate(t *testing.T) {
	client, srv, teardown := startTestServer(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := client.StreamPeerList(ctx, &pb.StreamPeerListRequest{NodeName: "client-01"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if first.GetVersion() != 0 || len(first.GetPeers()) != 0 {
		t.Fatalf("initial peer list = %+v, want empty v0", first)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = srv.ledger.Reassign("10.0.0.5", "master-01", "scheduled")
	}()
	upd, err := stream.Recv()
	if err != nil {
		t.Fatalf("expected live peer-list update: %v", err)
	}
	if len(upd.GetPeers()) != 1 {
		t.Fatalf("peers = %d, want 1", len(upd.GetPeers()))
	}
	peer := upd.GetPeers()[0]
	if peer.GetPeerName() != "master-01" || peer.GetPeerOverlayIp() != "10.0.0.5" {
		t.Fatalf("unexpected peer update: %+v", peer)
	}
}

func TestServer_DecommissionNode(t *testing.T) {
	client, srv, teardown := startTestServer(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Register two masters in same region; ledger entries owned by master-A.
	for _, name := range []string{"master-A", "master-B"} {
		if _, err := client.RegisterNode(ctx, &pb.RegisterNodeRequest{
			NodeName:    name,
			Roles:       []string{string(role.RoleMaster)},
			NodeCertPem: fakeCert,
			OverlayIp:   "10.0.0." + map[string]string{"master-A": "1", "master-B": "2"}[name],
			Region:      "ru",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := srv.ledger.Reassign("172.21.92.10", "master-A", "scheduled"); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.ledger.Reassign("172.21.92.11", "master-A", "scheduled"); err != nil {
		t.Fatal(err)
	}

	resp, err := client.DecommissionNode(ctx, &pb.DecommissionRequest{NodeName: "master-A", DrainSeconds: 0})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("expected success, got %s (count=%d)", resp.GetError(), resp.GetReassignedOverlayCount())
	}
	if resp.GetReassignedOverlayCount() != 3 {
		t.Fatalf("reassigned = %d, want 3", resp.GetReassignedOverlayCount())
	}
	if _, ok := srv.registry.Lookup("master-A"); ok {
		t.Fatal("master-A should be removed from registry")
	}
	if got := srv.ledger.OwnedBy("master-B"); len(got) != 4 {
		t.Fatalf("master-B should now own 4 entries, got %d", len(got))
	}
}

func TestServer_QueryAudit_FiltersAndStreams(t *testing.T) {
	client, srv, teardown := startTestServer(t)
	defer teardown()
	srv.audit.Append(AuditEvent{EventType: "register", NodeName: "n1"})
	srv.audit.Append(AuditEvent{EventType: "heartbeat", NodeName: "n1"})
	srv.audit.Append(AuditEvent{EventType: "heartbeat", NodeName: "n2"})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stream, err := client.QueryAudit(ctx, &pb.QueryAuditRequest{EventTypeFilter: "heartbeat"})
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for {
		_, err := stream.Recv()
		if err != nil {
			break
		}
		got++
	}
	if got != 2 {
		t.Fatalf("got %d audit entries, want 2 heartbeats", got)
	}
}

func TestServer_StubsReturnUnimplemented(t *testing.T) {
	client, _, teardown := startTestServer(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// StreamServiceRegistry stub.
	stream, _ := client.StreamServiceRegistry(ctx, &pb.StreamServiceRegistryRequest{IngressNode: "i1"})
	_, err := stream.Recv()
	if st, _ := status.FromError(err); st.Code() != codes.Unimplemented {
		t.Fatalf("StreamServiceRegistry expected Unimplemented, got %v", err)
	}
}
