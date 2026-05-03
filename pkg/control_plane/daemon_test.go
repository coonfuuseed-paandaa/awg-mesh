package control_plane

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/pkg/tls"
	pb "github.com/coonfuuseed-paandaa/awg-mesh/proto/control_plane"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestNewDaemon_RejectsInsecurePublicBind(t *testing.T) {
	_, err := NewDaemon(Config{ListenAddr: "0.0.0.0:51820", StateDir: t.TempDir()})
	if err == nil {
		t.Fatalf("expected insecure wildcard bind rejection")
	}
	_, err = NewDaemon(Config{ListenAddr: ":51820", StateDir: t.TempDir()})
	if err == nil {
		t.Fatalf("expected insecure empty-host bind rejection")
	}
	_, err = NewDaemon(Config{ListenAddr: "0.0.0.0:51820", StateDir: t.TempDir(), AllowInsecurePublicBind: true})
	if err != nil {
		t.Fatalf("explicit public bind opt-in should be accepted: %v", err)
	}
}

func TestNewDaemon_ConfiguresCertLifecycleFromCADir(t *testing.T) {
	caDir := t.TempDir()
	caCert, caKey, err := pkgtls.GenerateCA("mesh-ca")
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	if err := pkgtls.SaveCA(caDir, caCert, caKey); err != nil {
		t.Fatalf("SaveCA: %v", err)
	}

	d, err := NewDaemon(Config{
		ListenAddr:       "127.0.0.1:0",
		StateDir:         t.TempDir(),
		CADir:            caDir,
		CertRotationDays: 30,
	})
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	if d.server.certLifecycle == nil {
		t.Fatal("cert lifecycle was not configured from CA dir")
	}
}

func TestNewDaemon_RejectsExplicitBadCADir(t *testing.T) {
	_, err := NewDaemon(Config{
		ListenAddr: "127.0.0.1:0",
		StateDir:   t.TempDir(),
		CADir:      filepath.Join(t.TempDir(), "missing-ca"),
	})
	if err == nil {
		t.Fatal("expected explicit bad CA dir to fail")
	}
}

func TestNewDaemon_RejectsIncompleteDefaultCAMaterial(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "ca.crt"), []byte("not-a-complete-ca"), 0o600); err != nil {
		t.Fatalf("write partial CA material: %v", err)
	}
	_, err := NewDaemon(Config{
		ListenAddr: "127.0.0.1:0",
		StateDir:   stateDir,
	})
	if err == nil {
		t.Fatal("expected incomplete default CA material to fail")
	}
}

func TestDaemon_LifecycleAndAcceptsRegister(t *testing.T) {
	dir := t.TempDir()
	d, err := NewDaemon(Config{
		ListenAddr:   "127.0.0.1:0",
		StateDir:     dir,
		AuditCap:     16,
		StartupGrace: 0,
	})
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- d.Run(ctx)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for d.ListenerAddr() == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	addr := d.ListenerAddr()
	if addr == "" {
		t.Fatal("daemon never bound listener")
	}

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("conn.Close: %v", err)
		}
	}()
	client := pb.NewControlPlaneClient(conn)

	regCtx, regCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer regCancel()
	resp, err := client.RegisterNode(regCtx, &pb.RegisterNodeRequest{
		NodeName:    "n1",
		Roles:       []string{"master"},
		NodeCertPem: fakeCert,
		OverlayIp:   "10.0.0.1",
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	if !resp.GetAccepted() {
		t.Fatalf("registration rejected: %s", resp.GetRejectReason())
	}

	cancel()
	select {
	case err := <-doneCh:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not shut down within 5s")
	}

	auditPath := filepath.Join(dir, "audit.log")
	info, err := os.Stat(auditPath)
	if err != nil {
		t.Fatalf("audit log not written: %v", err)
	}
	if info.Size() == 0 {
		t.Fatalf("audit log empty")
	}
}
