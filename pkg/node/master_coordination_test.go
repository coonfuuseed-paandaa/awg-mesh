package node

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/wg"
	pb "github.com/coonfuuseed-paandaa/awg-mesh/v2/proto/control_plane"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestMasterCoordinationListenerExposesRegistryAndOwnership(t *testing.T) {
	t.Parallel()

	master, err := NewMaster(testMasterConfigWithCoordination(t, "127.0.0.1:0"))
	if err != nil {
		t.Fatalf("NewMaster: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- master.Run(ctx)
	}()

	waitFor(t, func() bool {
		status := master.Status().Coordination
		return status.Enabled && status.Started && status.BoundAddr != ""
	})
	addr := master.Status().Coordination.BoundAddr

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		cancel()
		t.Fatalf("dial coordination endpoint: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("conn.Close: %v", err)
		}
	}()
	client := pb.NewControlPlaneClient(conn)

	registerCtx, registerCancel := context.WithTimeout(context.Background(), 2*time.Second)
	resp, err := client.RegisterNode(registerCtx, &pb.RegisterNodeRequest{
		NodeName:    "egress-01",
		Roles:       []string{"egress"},
		NodeCertPem: []byte("test-cert"),
		OverlayIp:   "172.21.92.10",
		Region:      "eu",
	})
	registerCancel()
	if err != nil {
		cancel()
		t.Fatalf("RegisterNode through master coordination: %v", err)
	}
	if !resp.GetAccepted() {
		cancel()
		t.Fatalf("registration rejected by master coordination: %s", resp.GetRejectReason())
	}

	streamCtx, streamCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer streamCancel()
	stream, err := client.StreamOwnership(streamCtx, &pb.StreamOwnershipRequest{SubscriberNode: "egress-01"})
	if err != nil {
		cancel()
		t.Fatalf("StreamOwnership through master coordination: %v", err)
	}
	update, err := stream.Recv()
	if err != nil {
		cancel()
		t.Fatalf("receive ownership snapshot: %v", err)
	}
	if !update.GetFullSnapshot() {
		cancel()
		t.Fatalf("expected initial full ownership snapshot, got %#v", update)
	}
	if !ownershipContains(update.GetEntries(), "172.21.92.2", "master-01") {
		cancel()
		t.Fatalf("ownership snapshot missing self-registered master: %#v", update.GetEntries())
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned error after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("master Run did not return after cancellation")
	}
}

func TestMasterSelfRegistersAndStreamsPubkey(t *testing.T) {
	t.Parallel()

	master, err := NewMaster(testMasterConfigWithCoordination(t, "127.0.0.1:0"))
	if err != nil {
		t.Fatalf("NewMaster: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- master.Run(ctx) }()

	waitFor(t, func() bool {
		s := master.Status().Coordination
		return s.Enabled && s.Started && s.BoundAddr != ""
	})
	addr := master.Status().Coordination.BoundAddr

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	client := pb.NewControlPlaneClient(conn)

	streamCtx, streamCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer streamCancel()
	stream, err := client.StreamPeerList(streamCtx, &pb.StreamPeerListRequest{NodeName: "client-01"})
	if err != nil {
		t.Fatalf("StreamPeerList: %v", err)
	}
	initial, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv initial peer list: %v", err)
	}
	if len(initial.GetPeers()) != 1 {
		t.Fatalf("expected 1 peer (self-registered master), got %d", len(initial.GetPeers()))
	}
	peer := initial.GetPeers()[0]
	if peer.GetPeerName() != "master-01" {
		t.Fatalf("peer name = %q, want master-01", peer.GetPeerName())
	}
	if len(peer.GetPeerPubkey()) != 32 {
		t.Fatalf("REGRESSION: peer pubkey length = %d, want 32 — master did not self-register its WG pubkey", len(peer.GetPeerPubkey()))
	}
	if peer.GetPeerOverlayIp() != "172.21.92.2" {
		t.Fatalf("peer overlay = %q, want 172.21.92.2", peer.GetPeerOverlayIp())
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestMasterCoordinationBindFailureDoesNotCorruptDataPlaneConfig(t *testing.T) {
	t.Parallel()

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve listener: %v", err)
	}
	defer func() {
		if err := occupied.Close(); err != nil {
			t.Errorf("occupied.Close: %v", err)
		}
	}()

	var clientTransport *fakeMasterTransport
	var meshTransport *fakeMasterTransport
	master, err := NewMaster(MasterConfig{
		Name:      "master-01",
		OverlayIP: "172.21.92.2",
		DualListener: wg.DualListenerConfig{
			ClientInterfaceName: "wg-clientx",
			MeshInterfaceName:   "wg-meshx",
			VanillaFactory: func(name string) (wg.Transport, error) {
				clientTransport = &fakeMasterTransport{name: name, protocol: wg.ProtocolVanilla}
				return clientTransport, nil
			},
			AWGFactory: func(name string) (wg.Transport, error) {
				meshTransport = &fakeMasterTransport{name: name, protocol: wg.ProtocolAmneziaWG}
				return meshTransport, nil
			},
		},
		Coordination: &MasterCoordinationConfig{
			ListenAddr: occupied.Addr().String(),
			StateDir:   t.TempDir(),
		},
	})
	if err != nil {
		t.Fatalf("NewMaster: %v", err)
	}

	err = master.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "coordination") {
		t.Fatalf("expected coordination bind error, got %v", err)
	}
	status := master.Status()
	if status.Listeners.ClientInterfaceName != "wg-clientx" || status.Listeners.MeshInterfaceName != "wg-meshx" {
		t.Fatalf("data-plane listener config was corrupted after coordination failure: %#v", status.Listeners)
	}
	if status.Coordination.Started || status.Coordination.BoundAddr != "" {
		t.Fatalf("failed coordination listener must not report started, got %#v", status.Coordination)
	}
	if clientTransport == nil || meshTransport == nil {
		t.Fatalf("data-plane transports were not created before coordination failure")
	}
	if clientTransport.closeCount != 1 || meshTransport.closeCount != 1 {
		t.Fatalf("data-plane transports were not closed after coordination failure: client=%d mesh=%d", clientTransport.closeCount, meshTransport.closeCount)
	}
}

var testMasterPubkey = wg.Key{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32}

func testMasterConfigWithCoordination(t *testing.T, listenAddr string) MasterConfig {
	t.Helper()

	return MasterConfig{
		Name:      "master-01",
		OverlayIP: "172.21.92.2",
		DualListener: wg.DualListenerConfig{
			VanillaFactory: func(name string) (wg.Transport, error) {
				return &fakeMasterTransport{name: name, protocol: wg.ProtocolVanilla}, nil
			},
			AWGFactory: func(name string) (wg.Transport, error) {
				return &fakeMasterTransport{name: name, protocol: wg.ProtocolAmneziaWG, pubkey: testMasterPubkey}, nil
			},
		},
		Coordination: &MasterCoordinationConfig{
			ListenAddr: listenAddr,
			StateDir:   t.TempDir(),
		},
	}
}

func ownershipContains(entries []*pb.OwnershipEntry, overlayIP, owningMaster string) bool {
	for _, entry := range entries {
		if entry.GetOverlayIp() == overlayIP && entry.GetOwningMaster() == owningMaster {
			return true
		}
	}
	return false
}
