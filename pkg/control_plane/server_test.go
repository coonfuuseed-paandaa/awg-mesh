package control_plane

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/role"
	meshrotation "github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/rotation"
	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/tls"
	pb "github.com/coonfuuseed-paandaa/awg-mesh/v2/proto/control_plane"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
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
	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- gs.Serve(lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		gs.Stop()
		t.Fatalf("dial: %v", err)
	}
	client := pb.NewControlPlaneClient(conn)
	teardown := func() {
		if err := conn.Close(); err != nil {
			t.Errorf("conn.Close: %v", err)
		}
		gs.Stop()
		select {
		case err := <-serveErrCh:
			if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
				t.Errorf("grpc Serve: %v", err)
			}
		case <-time.After(time.Second):
			t.Errorf("grpc Serve did not stop")
		}
	}
	return client, srv, teardown
}

func startTLSControlPlaneServer(t *testing.T, caCert *x509.Certificate, caKey crypto.PrivateKey, clientCertPEM, clientKeyPEM []byte) (pb.ControlPlaneClient, *Server, func()) {
	t.Helper()
	registry := NewRegistry()
	ledger := NewLedger()
	audit := NewAuditLog(64)
	srv := NewServer(registry, ledger, audit)

	serverCertPEM, serverKeyPEM, err := pkgtls.IssueCert(caCert, caKey, "awg-mesh-control-plane", []string{"127.0.0.1", "localhost"})
	if err != nil {
		t.Fatalf("IssueCert server: %v", err)
	}
	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair server: %v", err)
	}
	clientCAs := x509.NewCertPool()
	clientCAs.AddCert(caCert)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
		Certificates: []tls.Certificate{serverCert},
		MinVersion:   tls.VersionTLS13,
	})))
	pb.RegisterControlPlaneServer(gs, srv)
	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- gs.Serve(lis) }()

	clientCert, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair client: %v", err)
	}
	rootCAs := x509.NewCertPool()
	rootCAs.AddCert(caCert)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		RootCAs:      rootCAs,
		Certificates: []tls.Certificate{clientCert},
		ServerName:   "127.0.0.1",
		MinVersion:   tls.VersionTLS13,
	})))
	if err != nil {
		gs.Stop()
		t.Fatalf("dial TLS: %v", err)
	}
	teardown := func() {
		if err := conn.Close(); err != nil {
			t.Errorf("conn.Close: %v", err)
		}
		gs.Stop()
		select {
		case err := <-serveErrCh:
			if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
				t.Errorf("grpc Serve: %v", err)
			}
		case <-time.After(time.Second):
			t.Errorf("grpc Serve did not stop")
		}
	}
	return pb.NewControlPlaneClient(conn), srv, teardown
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
		if _, err := srv.ledger.Reassign("10.0.0.5", "master-01", "scheduled"); err != nil {
			t.Errorf("Reassign: %v", err)
		}
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

func TestServer_StreamPeerList_IncludesPubkey(t *testing.T) {
	client, _, teardown := startTestServer(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	masterPubkey := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32}
	resp, err := client.RegisterNode(ctx, &pb.RegisterNodeRequest{
		NodeName:     "master-01",
		Roles:        []string{"master"},
		NodeCertPem:  []byte("test-cert"),
		OverlayIp:    "10.0.0.5",
		Region:       "eu",
		Pubkey:       masterPubkey,
		EndpointHost: "203.0.113.10:51820",
		Protocol:     "amneziawg",
	})
	if err != nil || !resp.GetAccepted() {
		t.Fatalf("register: err=%v accepted=%v reason=%s", err, resp.GetAccepted(), resp.GetRejectReason())
	}

	stream, err := client.StreamPeerList(ctx, &pb.StreamPeerListRequest{NodeName: "client-01"})
	if err != nil {
		t.Fatal(err)
	}
	initial, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if len(initial.GetPeers()) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(initial.GetPeers()))
	}
	peer := initial.GetPeers()[0]
	if peer.GetPeerName() != "master-01" {
		t.Fatalf("peer name = %q, want master-01", peer.GetPeerName())
	}
	if len(peer.GetPeerPubkey()) != 32 {
		t.Fatalf("peer pubkey length = %d, want 32", len(peer.GetPeerPubkey()))
	}
	if peer.GetPeerEndpointHost() != "203.0.113.10:51820" {
		t.Fatalf("peer endpoint = %q, want 203.0.113.10:51820", peer.GetPeerEndpointHost())
	}
	if peer.GetProtocol() != "amneziawg" {
		t.Fatalf("peer protocol = %q, want amneziawg", peer.GetProtocol())
	}
}

func TestServer_StreamPeerList_DeduplicatesByMaster(t *testing.T) {
	client, srv, teardown := startTestServer(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := client.RegisterNode(ctx, &pb.RegisterNodeRequest{
		NodeName:    "master-01",
		Roles:       []string{"master"},
		NodeCertPem: []byte("test-cert"),
		OverlayIp:   "10.0.0.5",
		Region:      "eu",
		Pubkey:      []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.ledger.Reassign("10.0.0.6", "master-01", "scheduled"); err != nil {
		t.Fatal(err)
	}

	stream, err := client.StreamPeerList(ctx, &pb.StreamPeerListRequest{NodeName: "client-01"})
	if err != nil {
		t.Fatal(err)
	}
	upd, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if len(upd.GetPeers()) != 1 {
		t.Fatalf("expected 1 deduplicated peer, got %d", len(upd.GetPeers()))
	}
	peer := upd.GetPeers()[0]
	if len(peer.GetAllowedIps()) != 2 {
		t.Fatalf("expected 2 AllowedIPs (merged), got %d: %v", len(peer.GetAllowedIps()), peer.GetAllowedIps())
	}
}

func TestServer_HeartbeatRefreshesPubkey(t *testing.T) {
	client, srv, teardown := startTestServer(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := client.RegisterNode(ctx, &pb.RegisterNodeRequest{
		NodeName:    "master-01",
		Roles:       []string{"master"},
		NodeCertPem: []byte("test-cert"),
		OverlayIp:   "10.0.0.5",
		Region:      "eu",
	})
	if err != nil {
		t.Fatal(err)
	}

	node, ok := srv.registry.Lookup("master-01")
	if !ok {
		t.Fatal("master-01 not found in registry")
	}
	if len(node.Pubkey) != 0 {
		t.Fatalf("expected empty pubkey after registration without pubkey, got %d bytes", len(node.Pubkey))
	}

	newPubkey := []byte{42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42}
	hbStream, err := client.Heartbeat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := hbStream.Send(&pb.HeartbeatRequest{
		NodeName:     "master-01",
		SentAtUnix:   time.Now().Unix(),
		Pubkey:       newPubkey,
		EndpointHost: "198.51.100.1:51820",
		Protocol:     "vanilla-wg",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := hbStream.Recv(); err != nil {
		t.Fatal(err)
	}

	node, ok = srv.registry.Lookup("master-01")
	if !ok {
		t.Fatal("master-01 not found after heartbeat")
	}
	if len(node.Pubkey) != 32 {
		t.Fatalf("expected pubkey refreshed to 32 bytes, got %d", len(node.Pubkey))
	}
	if node.EndpointHost != "198.51.100.1:51820" {
		t.Fatalf("endpoint = %q, want 198.51.100.1:51820", node.EndpointHost)
	}
	if node.Protocol != "vanilla-wg" {
		t.Fatalf("protocol = %q, want vanilla-wg", node.Protocol)
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

func TestServer_DecommissionNode_AllowsZeroOwnedNodeWithoutSuccessor(t *testing.T) {
	client, srv, teardown := startTestServer(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := client.RegisterNode(ctx, &pb.RegisterNodeRequest{
		NodeName:    "egress-01",
		Roles:       []string{string(role.RoleEgress)},
		NodeCertPem: fakeCert,
		OverlayIp:   "10.0.0.50",
		Region:      "ru",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.GetAccepted() {
		t.Fatalf("expected accepted, got %q", resp.GetRejectReason())
	}

	decomm, err := client.DecommissionNode(ctx, &pb.DecommissionRequest{NodeName: "egress-01"})
	if err != nil {
		t.Fatal(err)
	}
	if !decomm.GetSuccess() || decomm.GetReassignedOverlayCount() != 0 {
		t.Fatalf("expected zero-owned decommission success, got %+v", decomm)
	}
	if _, ok := srv.registry.Lookup("egress-01"); ok {
		t.Fatal("egress-01 should be removed from registry")
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
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		got++
	}
	if got != 2 {
		t.Fatalf("got %d audit entries, want 2 heartbeats", got)
	}
}

func TestServer_StreamCertUpdateIssuesDueCertAndAllowsReregister(t *testing.T) {
	caCert, caKey, err := pkgtls.GenerateCA("mesh-ca")
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	oldCert, oldKey, err := pkgtls.IssueCertWithValidity(caCert, caKey, "client-01", []string{"client-01", "172.21.92.130", "client-01.mesh.example"}, 6*24*time.Hour)
	if err != nil {
		t.Fatalf("IssueCertWithValidity old: %v", err)
	}
	client, srv, teardown := startTLSControlPlaneServer(t, caCert, caKey, oldCert, oldKey)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	lifecycle, err := NewCertLifecycle(CAIssuer{CACert: caCert, CAKey: caKey}, CertLifecycleConfig{
		RotationDays: 90,
		RenewBefore:  7 * 24 * time.Hour,
		PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewCertLifecycle: %v", err)
	}
	srv.certLifecycle = lifecycle

	resp, err := client.RegisterNode(ctx, &pb.RegisterNodeRequest{
		NodeName:    "client-01",
		Roles:       []string{"client"},
		NodeCertPem: oldCert,
		OverlayIp:   "172.21.92.130",
		Region:      "home",
	})
	if err != nil {
		t.Fatalf("RegisterNode old cert: %v", err)
	}
	if !resp.GetAccepted() {
		t.Fatalf("old cert registration rejected: %s", resp.GetRejectReason())
	}

	stream, err := client.StreamCertUpdate(ctx, &pb.StreamCertRequest{NodeName: "client-01"})
	if err != nil {
		t.Fatalf("StreamCertUpdate: %v", err)
	}
	update, err := stream.Recv()
	if err != nil {
		t.Fatalf("StreamCertUpdate Recv: %v", err)
	}
	if len(update.GetCertPem()) == 0 || len(update.GetKeyPem()) == 0 {
		t.Fatalf("cert update missing cert/key bytes")
	}
	if err := pkgtls.ValidateCert(update.GetCertPem(), caCert); err != nil {
		t.Fatalf("updated cert does not validate against CA: %v", err)
	}
	cn, notAfter, err := pkgtls.CertInfo(update.GetCertPem())
	if err != nil {
		t.Fatalf("CertInfo updated cert: %v", err)
	}
	if cn != "client-01" {
		t.Fatalf("updated cert CN = %q, want client-01", cn)
	}
	parsedUpdate, err := pkgtls.ParseCertPEM(update.GetCertPem())
	if err != nil {
		t.Fatalf("ParseCertPEM updated cert: %v", err)
	}
	if !certHasDNSName(parsedUpdate, "client-01.mesh.example") {
		t.Fatalf("updated cert dropped existing DNS SANs: %#v", parsedUpdate.DNSNames)
	}
	if update.GetValidUntilUnix() != notAfter.Unix() || update.GetValidFromUnix() == 0 {
		t.Fatalf("unexpected validity window: %+v notAfter=%s", update, notAfter)
	}
	pendingNode, ok := srv.registry.Lookup("client-01")
	if !ok {
		t.Fatal("registered node missing after cert update")
	}
	_, _, due, err := lifecycle.NextDueUpdate(pendingNode)
	if err != nil {
		t.Fatalf("NextDueUpdate with pending cert: %v", err)
	}
	if due {
		t.Fatal("pending cert should suppress duplicate replacement issuance")
	}
	if delay := lifecycle.DelayUntilDue(pendingNode); delay != 10*time.Millisecond {
		t.Fatalf("pending cert delay = %s, want poll interval", delay)
	}

	resp, err = client.RegisterNode(ctx, &pb.RegisterNodeRequest{
		NodeName:    "client-01",
		Roles:       []string{"client"},
		NodeCertPem: update.GetCertPem(),
		OverlayIp:   "172.21.92.130",
		Region:      "home",
	})
	if err != nil {
		t.Fatalf("RegisterNode updated cert: %v", err)
	}
	if !resp.GetAccepted() {
		t.Fatalf("updated cert registration rejected: %s", resp.GetRejectReason())
	}

	events := srv.audit.Query(time.Time{}, time.Time{}, "cert-update-issued", "client-01", 0)
	if len(events) != 1 {
		t.Fatalf("cert-update-issued audit events = %d, want 1", len(events))
	}
}

func certHasDNSName(cert *x509.Certificate, want string) bool {
	for _, got := range cert.DNSNames {
		if got == want {
			return true
		}
	}
	return false
}

func TestServer_StreamCertUpdateRejectsUnknownNode(t *testing.T) {
	caCert, caKey, err := pkgtls.GenerateCA("mesh-ca")
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	clientCert, clientKey, err := pkgtls.IssueCert(caCert, caKey, "ghost", []string{"ghost"})
	if err != nil {
		t.Fatalf("IssueCert ghost: %v", err)
	}
	client, srv, teardown := startTLSControlPlaneServer(t, caCert, caKey, clientCert, clientKey)
	defer teardown()
	lifecycle, err := NewCertLifecycle(CAIssuer{CACert: caCert, CAKey: caKey}, CertLifecycleConfig{})
	if err != nil {
		t.Fatalf("NewCertLifecycle: %v", err)
	}
	srv.certLifecycle = lifecycle

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stream, err := client.StreamCertUpdate(ctx, &pb.StreamCertRequest{NodeName: "ghost"})
	if err != nil {
		t.Fatalf("StreamCertUpdate: %v", err)
	}
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("expected NotFound for unknown cert stream node")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestServer_StreamCertUpdateRejectsMismatchedClientIdentity(t *testing.T) {
	caCert, caKey, err := pkgtls.GenerateCA("mesh-ca")
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	clientCert, clientKey, err := pkgtls.IssueCert(caCert, caKey, "client-01", []string{"client-01"})
	if err != nil {
		t.Fatalf("IssueCert client: %v", err)
	}
	client, srv, teardown := startTLSControlPlaneServer(t, caCert, caKey, clientCert, clientKey)
	defer teardown()
	lifecycle, err := NewCertLifecycle(CAIssuer{CACert: caCert, CAKey: caKey}, CertLifecycleConfig{})
	if err != nil {
		t.Fatalf("NewCertLifecycle: %v", err)
	}
	srv.certLifecycle = lifecycle

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stream, err := client.StreamCertUpdate(ctx, &pb.StreamCertRequest{NodeName: "client-02"})
	if err != nil {
		t.Fatalf("StreamCertUpdate: %v", err)
	}
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("expected Unauthenticated for mismatched client identity")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", err)
	}
}

func TestServer_RotateAWGParamsMeshWide_StreamsMeshInternalResults(t *testing.T) {
	client, srv, teardown := startTestServer(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	registerRotationNode(t, client, ctx, "client-01", role.RoleClient, "172.21.92.130")
	registerRotationNode(t, client, ctx, "master-01", role.RoleMaster, "172.21.92.2")
	registerRotationNode(t, client, ctx, "egress-01", role.RoleEgress, "172.21.92.34")

	stream := sendRotateRequest(t, client, ctx, &pb.RotateRequest{
		Tier:              "1",
		NewParams:         testControlPlaneAWGParams(),
		ApplyAtUnixMicros: time.Now().Add(time.Minute).UnixMicro(),
		RotationId:        "rot-success",
	})
	results, err := recvRotateResponses(stream)
	if err != nil {
		t.Fatalf("RotateAWGParamsMeshWide: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 mesh-internal targets", len(results))
	}
	if results[0].GetNodeName() != "egress-01" || results[1].GetNodeName() != "master-01" {
		t.Fatalf("unexpected stable target order: %+v", results)
	}
	for _, result := range results {
		if !result.GetAck() || result.GetError() != "" || result.GetRotationId() != "rot-success" {
			t.Fatalf("unexpected rotate result: %+v", result)
		}
	}
	events := srv.audit.Query(time.Time{}, time.Time{}, "rotate-result", "", 0)
	if len(events) != 1 {
		t.Fatalf("rotate-result audit events = %d, want 1", len(events))
	}
}

func TestServer_RotateAWGParamsMeshWide_RejectsInvalidRequest(t *testing.T) {
	client, _, teardown := startTestServer(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	stream := sendRotateRequest(t, client, ctx, &pb.RotateRequest{Tier: "1"})
	_, err := recvRotateResponses(stream)
	if err == nil {
		t.Fatal("expected InvalidArgument for missing params")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

func TestServer_RotateAWGParamsMeshWide_RejectsEmptyMeshInternalTargets(t *testing.T) {
	client, _, teardown := startTestServer(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	registerRotationNode(t, client, ctx, "client-01", role.RoleClient, "172.21.92.130")

	stream := sendRotateRequest(t, client, ctx, &pb.RotateRequest{
		Tier:      "tier1",
		NewParams: testControlPlaneAWGParams(),
	})
	_, err := recvRotateResponses(stream)
	if err == nil {
		t.Fatal("expected FailedPrecondition for client-only registry")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
}

func TestServer_RotateAWGParamsMeshWide_ReportsPartialApply(t *testing.T) {
	client, srv, teardown := startTestServer(t)
	defer teardown()
	applier := meshrotation.NewMemoryApplier()
	applier.SetFailure("egress-01", errors.New("apply failed"))
	srv.rotation = meshrotation.NewOrchestrator(applier, meshrotation.OrchestratorConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	registerRotationNode(t, client, ctx, "master-01", role.RoleMaster, "172.21.92.2")
	registerRotationNode(t, client, ctx, "egress-01", role.RoleEgress, "172.21.92.34")

	stream := sendRotateRequest(t, client, ctx, &pb.RotateRequest{
		Tier:       "tier3",
		NewParams:  testControlPlaneAWGParams(),
		RotationId: "rot-partial",
	})
	results, err := recvRotateResponses(stream)
	if err == nil {
		t.Fatal("expected Aborted after partial apply responses")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.Aborted {
		t.Fatalf("expected Aborted, got %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	if results[0].GetNodeName() != "egress-01" || results[0].GetAck() || results[0].GetError() == "" {
		t.Fatalf("expected egress failure result first, got %+v", results)
	}
	if results[1].GetNodeName() != "master-01" || !results[1].GetAck() {
		t.Fatalf("expected master success result second, got %+v", results)
	}
}

func TestServer_StubsReturnUnimplemented(t *testing.T) {
	client, _, teardown := startTestServer(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// StreamServiceRegistry stub.
	stream, err := client.StreamServiceRegistry(ctx, &pb.StreamServiceRegistryRequest{IngressNode: "i1"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("StreamServiceRegistry expected Unimplemented, got nil")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.Unimplemented {
		t.Fatalf("StreamServiceRegistry expected Unimplemented, got %v", err)
	}
}

func registerRotationNode(t *testing.T, client pb.ControlPlaneClient, ctx context.Context, name string, nodeRole role.Role, overlayIP string) {
	t.Helper()
	resp, err := client.RegisterNode(ctx, &pb.RegisterNodeRequest{
		NodeName:    name,
		Roles:       []string{string(nodeRole)},
		NodeCertPem: fakeCert,
		OverlayIp:   overlayIP,
	})
	if err != nil {
		t.Fatalf("RegisterNode %s: %v", name, err)
	}
	if !resp.GetAccepted() {
		t.Fatalf("RegisterNode %s rejected: %s", name, resp.GetRejectReason())
	}
}

func sendRotateRequest(t *testing.T, client pb.ControlPlaneClient, ctx context.Context, req *pb.RotateRequest) pb.ControlPlane_RotateAWGParamsMeshWideClient {
	t.Helper()
	stream, err := client.RotateAWGParamsMeshWide(ctx)
	if err != nil {
		t.Fatalf("RotateAWGParamsMeshWide: %v", err)
	}
	if err := stream.Send(req); err != nil {
		t.Fatalf("RotateAWGParamsMeshWide Send: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("RotateAWGParamsMeshWide CloseSend: %v", err)
	}
	return stream
}

func recvRotateResponses(stream pb.ControlPlane_RotateAWGParamsMeshWideClient) ([]*pb.RotateResponse, error) {
	results := make([]*pb.RotateResponse, 0)
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return results, nil
		}
		if err != nil {
			return results, err
		}
		results = append(results, resp)
	}
}

func testControlPlaneAWGParams() *pb.AWGParamsV2 {
	return &pb.AWGParamsV2{
		Jc: 1, Jmin: 2, Jmax: 3,
		S1: 4, S2: 5,
		H1: 6, H2: 7, H3: 8, H4: 9,
		I1: []byte("i1"),
		I2: []byte("i2"),
		I3: []byte("i3"),
		I4: []byte("i4"),
		I5: []byte("i5"),
	}
}
