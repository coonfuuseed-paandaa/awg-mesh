package control_plane

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	pb "github.com/coonfuuseed-paandaa/awg-mesh/proto/control_plane"
	"google.golang.org/grpc"
)

// Config drives the control-plane daemon. CR-002 keeps it minimal — TLS bootstrap,
// state persistence, and a listen address. mTLS material lands in CR-016 (cert
// lifecycle); for CR-002 the listener uses insecure transport (intra-mesh
// only — never bind to a public interface).
type Config struct {
	ListenAddr              string        // e.g. "127.0.0.1:51820" — control-plane gRPC port
	StateDir                string        // dir for audit log + persisted ledger
	AuditCap                int           // ring-buffer capacity (default 8192)
	StartupGrace            time.Duration // delay before declaring readiness (test hook)
	AllowInsecurePublicBind bool          // explicit opt-in for binding insecure gRPC outside loopback
}

// Daemon is the long-running control-plane process. It owns the registry,
// ledger, audit log, gRPC server, and listener. Run blocks until the context
// is cancelled or a fatal error fires.
type Daemon struct {
	cfg      Config
	registry *Registry
	ledger   *Ledger
	audit    *AuditLog
	server   *Server
	grpc     *grpc.Server

	mu       sync.Mutex
	listener net.Listener
}

// NewDaemon builds a daemon from Config. State directory is created if absent.
func NewDaemon(cfg Config) (*Daemon, error) {
	if cfg.ListenAddr == "" {
		return nil, errors.New("daemon: ListenAddr required")
	}
	if cfg.StateDir == "" {
		return nil, errors.New("daemon: StateDir required")
	}
	if err := validateListenAddr(cfg.ListenAddr, cfg.AllowInsecurePublicBind); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("daemon: mkdir state dir: %w", err)
	}
	registry := NewRegistry()
	ledger := NewLedger()
	audit := NewAuditLog(cfg.AuditCap)
	server := NewServer(registry, ledger, audit)
	gs := grpc.NewServer()
	pb.RegisterControlPlaneServer(gs, server)
	return &Daemon{
		cfg:      cfg,
		registry: registry,
		ledger:   ledger,
		audit:    audit,
		server:   server,
		grpc:     gs,
	}, nil
}

func validateListenAddr(listenAddr string, allowInsecurePublicBind bool) error {
	if allowInsecurePublicBind {
		return nil
	}
	host, _, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return fmt.Errorf("daemon: invalid ListenAddr %q: %w", listenAddr, err)
	}
	if host == "" {
		return fmt.Errorf("daemon: insecure ListenAddr %q requires AllowInsecurePublicBind=true", listenAddr)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip != nil {
		if ip.IsLoopback() && !ip.IsUnspecified() {
			return nil
		}
		return fmt.Errorf("daemon: insecure ListenAddr %q requires AllowInsecurePublicBind=true", listenAddr)
	}
	resolved, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("daemon: resolve ListenAddr host %q: %w", host, err)
	}
	if len(resolved) == 0 {
		return fmt.Errorf("daemon: resolve ListenAddr host %q: no addresses", host)
	}
	for _, resolvedIP := range resolved {
		if !resolvedIP.IsLoopback() || resolvedIP.IsUnspecified() {
			return fmt.Errorf("daemon: insecure ListenAddr %q requires AllowInsecurePublicBind=true", listenAddr)
		}
	}
	return nil
}

// Run binds the listener, serves gRPC, and blocks until ctx is cancelled or a
// SIGINT/SIGTERM is received. On shutdown the gRPC server is stopped gracefully
// and any in-flight streams complete. Audit log is flushed to disk best-effort.
func (d *Daemon) Run(ctx context.Context) error {
	lis, err := net.Listen("tcp", d.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("daemon: listen %s: %w", d.cfg.ListenAddr, err)
	}
	d.mu.Lock()
	d.listener = lis
	d.mu.Unlock()
	log.Printf("control-plane: listening on %s (state=%s)", d.cfg.ListenAddr, d.cfg.StateDir)

	if d.cfg.StartupGrace > 0 {
		time.Sleep(d.cfg.StartupGrace)
	}

	// Trap signals → cancel ctx.
	ctx, cancel := context.WithCancel(ctx)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	errCh := make(chan error, 1)
	go func() {
		errCh <- d.grpc.Serve(lis)
	}()

	select {
	case <-ctx.Done():
		log.Printf("control-plane: context cancelled, shutting down")
	case sig := <-sigCh:
		log.Printf("control-plane: received signal %s, shutting down", sig)
	case err := <-errCh:
		cancel()
		if err != nil {
			return fmt.Errorf("daemon: serve: %w", err)
		}
		return nil
	}

	cancel()
	d.shutdown()
	d.flushAudit()
	return nil
}

// ListenerAddr returns the bound address (test hook).
func (d *Daemon) ListenerAddr() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.listener == nil {
		return ""
	}
	return d.listener.Addr().String()
}

// Stop forces graceful shutdown (test hook + ctx-cancel path).
func (d *Daemon) Stop() {
	d.shutdown()
}

func (d *Daemon) shutdown() {
	stopped := make(chan struct{})
	go func() {
		d.grpc.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		log.Printf("control-plane: graceful stop timed out, forcing")
		d.grpc.Stop()
	}
}

// flushAudit best-effort writes the in-memory audit log to <state-dir>/audit.log
// (NDJSON, one event per line, oldest first). On error logs and proceeds.
func (d *Daemon) flushAudit() {
	if d.audit == nil {
		return
	}
	events := d.audit.Query(time.Time{}, time.Time{}, "", "", 0)
	if len(events) == 0 {
		return
	}
	path := filepath.Join(d.cfg.StateDir, "audit.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf("control-plane: audit flush open failed: %v", err)
		return
	}
	defer f.Close()
	for _, e := range events {
		fmt.Fprintf(f, "%s\t%s\t%s\t%s\n", e.Timestamp.UTC().Format(time.RFC3339), e.EventType, e.NodeName, e.Detail)
	}
}
