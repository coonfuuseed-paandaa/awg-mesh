package clientd

import (
	"context"
	"strings"
	"testing"
	"time"

	pb "github.com/coonfuuseed-paandaa/awg-mesh/proto/control_plane"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestControlPlaneNATTraverserDisabledOnUnimplemented(t *testing.T) {
	addr, cleanup := startTestControlPlane(t, &pb.UnimplementedControlPlaneServer{})
	defer cleanup()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer func() { _ = conn.Close() }()

	result, err := (ControlPlaneNATTraverser{Client: pb.NewControlPlaneClient(conn)}).Probe(context.Background())
	if err != nil {
		t.Fatalf("probe returned fatal error: %v", err)
	}
	if result.Status != NATStatusDisabled {
		t.Fatalf("expected disabled NAT result, got %#v", result)
	}
}

func TestControlPlaneNATTraverserAddsDefaultTimeout(t *testing.T) {
	conn, cleanup := connectHangingSignalExchange(t)
	defer cleanup()

	start := time.Now()
	_, err := (ControlPlaneNATTraverser{Client: pb.NewControlPlaneClient(conn)}).Probe(context.Background())
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "signal exchange") {
		t.Fatalf("expected bounded signal exchange error, got %v", err)
	}
	if elapsed > 3*defaultNATProbeTimeout {
		t.Fatalf("probe did not honor default timeout: elapsed=%s", elapsed)
	}
}

func TestControlPlaneNATTraverserKeepsCallerDeadline(t *testing.T) {
	conn, cleanup := connectHangingSignalExchange(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := (ControlPlaneNATTraverser{Client: pb.NewControlPlaneClient(conn)}).Probe(ctx)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "signal exchange") {
		t.Fatalf("expected caller deadline error, got %v", err)
	}
	if elapsed >= defaultNATProbeTimeout {
		t.Fatalf("probe ignored caller deadline: elapsed=%s", elapsed)
	}
}

func connectHangingSignalExchange(t *testing.T) (*grpc.ClientConn, func()) {
	t.Helper()
	addr, cleanupServer := startTestControlPlane(t, hangingSignalExchangeServer{})
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		cleanupServer()
		t.Fatalf("new client: %v", err)
	}
	return conn, func() {
		_ = conn.Close()
		cleanupServer()
	}
}

type hangingSignalExchangeServer struct {
	pb.UnimplementedControlPlaneServer
}

func (hangingSignalExchangeServer) SignalExchange(stream pb.ControlPlane_SignalExchangeServer) error {
	<-stream.Context().Done()
	return stream.Context().Err()
}
