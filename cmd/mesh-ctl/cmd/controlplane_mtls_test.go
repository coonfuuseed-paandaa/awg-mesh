package cmd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/tls"
	controlpb "github.com/coonfuuseed-paandaa/awg-mesh/v2/proto/control_plane"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
)

func writeControlPlaneMTLSConfig(t *testing.T) string {
	t.Helper()
	configDir := filepath.Join(t.TempDir(), "config")
	if _, _, err := loadOrCreateMeshCA(configDir, "mtls-test"); err != nil {
		t.Fatalf("create mesh CA: %v", err)
	}
	return configDir
}

func startMTLSControlPlaneTestServer(t *testing.T, configDir string, server controlpb.ControlPlaneServer) (string, controlpb.ControlPlaneClient, func()) {
	t.Helper()
	caCert, caKey, err := pkgtls.LoadCA(configDir)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
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
	controlpb.RegisterControlPlaneServer(gs, server)
	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- gs.Serve(lis) }()

	addr := lis.Addr().String()
	conn, err := newControlPlaneAdminConn(addr, configDir)
	if err != nil {
		gs.Stop()
		t.Fatalf("new mTLS client: %v", err)
	}
	waitForMTLSControlPlaneTestServer(t, conn)
	client := controlpb.NewControlPlaneClient(conn)

	return addr, client, func() {
		if err := conn.Close(); err != nil {
			t.Errorf("conn.Close: %v", err)
		}
		gs.Stop()
		select {
		case err := <-serveErrCh:
			if err != nil && !errors.Is(err, grpc.ErrServerStopped) && !errors.Is(err, io.EOF) {
				t.Errorf("grpc Serve: %v", err)
			}
		case <-time.After(time.Second):
			t.Errorf("grpc Serve did not stop")
		}
	}
}

func waitForMTLSControlPlaneTestServer(t *testing.T, conn *grpc.ClientConn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn.Connect()
	for state := conn.GetState(); state != connectivity.Ready; state = conn.GetState() {
		if !conn.WaitForStateChange(ctx, state) {
			t.Fatalf("mTLS control-plane test server did not become ready: state=%s err=%v", state, ctx.Err())
		}
	}
}
