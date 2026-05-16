package control_plane

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/role"
	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/tls"
	pb "github.com/coonfuuseed-paandaa/awg-mesh/v2/proto/control_plane"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Config drives the control-plane daemon.
type Config struct {
	ListenAddr              string        // e.g. "127.0.0.1:51820" — control-plane gRPC port
	StateDir                string        // dir for audit log + persisted ledger
	CADir                   string        // dir containing ca.crt + ca.key for cert lifecycle; defaults to StateDir when present
	CertHosts               []string      // additional DNS names/IPs for the generated control-plane server cert
	CertRotationDays        int           // FR-16 rotation interval; defaults to 90
	AuditCap                int           // ring-buffer capacity (default 8192)
	StartupGrace            time.Duration // delay before declaring readiness (test hook)
	AllowInsecurePublicBind bool          // explicit opt-in for binding insecure gRPC outside loopback
	RegistrationObserver    func(RegisteredNode) error
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
	securedPublicBind, err := hasCompleteCAMaterial(cfg)
	if err != nil {
		return nil, err
	}
	if err := validateListenAddr(cfg.ListenAddr, cfg.AllowInsecurePublicBind || securedPublicBind); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("daemon: mkdir state dir: %w", err)
	}
	registry := NewRegistry()
	ledger := NewLedger()
	audit := NewAuditLog(cfg.AuditCap)
	server := NewServer(registry, ledger, audit)
	server.SetRegistrationObserver(cfg.RegistrationObserver)
	serverOptions, err := configureDaemonCertLifecycle(cfg, server)
	if err != nil {
		return nil, err
	}
	gs := grpc.NewServer(serverOptions...)
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

func configureDaemonCertLifecycle(cfg Config, server *Server) ([]grpc.ServerOption, error) {
	caDir := cfg.CADir
	if caDir == "" {
		caDir = cfg.StateDir
		hasAnyCA, hasCompleteCA, err := caMaterialState(caDir)
		if err != nil {
			return nil, fmt.Errorf("daemon: inspect CA material for cert lifecycle: %w", err)
		}
		if !hasAnyCA {
			return nil, nil
		}
		if !hasCompleteCA {
			return nil, fmt.Errorf("daemon: incomplete CA material for cert lifecycle in %s: ca.crt and ca.key are required", caDir)
		}
	}
	caCert, caKey, err := pkgtls.LoadCA(caDir)
	if err != nil {
		return nil, fmt.Errorf("daemon: load CA for cert lifecycle: %w", err)
	}
	lifecycle, err := NewCertLifecycle(CAIssuer{CACert: caCert, CAKey: caKey}, CertLifecycleConfig{
		RotationDays: cfg.CertRotationDays,
	})
	if err != nil {
		return nil, fmt.Errorf("daemon: configure cert lifecycle: %w", err)
	}
	server.certLifecycle = lifecycle
	tlsConfig, err := controlPlaneTLSConfig(cfg, caCert, caKey)
	if err != nil {
		return nil, fmt.Errorf("daemon: configure mTLS for cert lifecycle: %w", err)
	}
	return []grpc.ServerOption{grpc.Creds(credentials.NewTLS(tlsConfig))}, nil
}

func controlPlaneTLSConfig(cfg Config, caCert *x509.Certificate, caKey crypto.PrivateKey) (*tls.Config, error) {
	certPEM, keyPEM, err := pkgtls.IssueCertWithValidity(caCert, caKey, "awg-mesh-control-plane", serverCertHosts(cfg.ListenAddr, cfg.CertHosts), time.Duration(defaultCertRotationDays)*24*time.Hour)
	if err != nil {
		return nil, err
	}
	serverCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	clientCAs := x509.NewCertPool()
	clientCAs.AddCert(caCert)
	return &tls.Config{
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
		Certificates: []tls.Certificate{serverCert},
		MinVersion:   tls.VersionTLS13,
	}, nil
}

func serverCertHosts(listenAddr string, extraHosts []string) []string {
	hosts := []string{"localhost", "127.0.0.1", "::1"}
	host, _, err := net.SplitHostPort(listenAddr)
	if err != nil || host == "" {
		return appendCertHosts(hosts, extraHosts)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return appendCertHosts(hosts, extraHosts)
	}
	return appendCertHosts(append(hosts, host), extraHosts)
}

func appendCertHosts(hosts []string, extraHosts []string) []string {
	seen := make(map[string]struct{}, len(hosts)+len(extraHosts))
	out := make([]string, 0, len(hosts)+len(extraHosts))
	for _, host := range append(hosts, extraHosts...) {
		normalized := normalizeCertHost(host)
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	return out
}

func normalizeCertHost(host string) string {
	trimmed := strings.TrimSpace(host)
	if trimmed == "" {
		return ""
	}
	if splitHost, _, err := net.SplitHostPort(trimmed); err == nil {
		trimmed = splitHost
	}
	return strings.Trim(trimmed, "[]")
}

func caMaterialState(dir string) (bool, bool, error) {
	certExists, err := regularFileExists(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return false, false, err
	}
	keyExists, err := regularFileExists(filepath.Join(dir, "ca.key"))
	if err != nil {
		return false, false, err
	}
	return certExists || keyExists, certExists && keyExists, nil
}

func hasCompleteCAMaterial(cfg Config) (bool, error) {
	caDir := cfg.CADir
	if caDir == "" {
		caDir = cfg.StateDir
	}
	_, hasCompleteCA, err := caMaterialState(caDir)
	if err != nil {
		return false, fmt.Errorf("daemon: inspect CA material for public bind: %w", err)
	}
	return hasCompleteCA, nil
}

func regularFileExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if err == nil {
		if info.IsDir() {
			return false, fmt.Errorf("%s is a directory", path)
		}
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
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
		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return fmt.Errorf("daemon: serve: %w", err)
		}
	}

	cancel()
	d.shutdown()
	d.flushAudit()
	return nil
}

// SelfRegister writes a node directly into the internal registry and ledger,
// bypassing gRPC. Used by the master to register itself without a network
// round-trip. NodeCertPEM must be non-empty; pass []byte("self-registered-master")
// when no real cert is available at startup.
func (d *Daemon) SelfRegister(node RegisteredNode) error {
	if err := d.registry.Register(node); err != nil {
		return err
	}
	for _, r := range node.Roles {
		if r == role.RoleMaster {
			if _, err := d.ledger.Reassign(node.OverlayIP, node.Name, "self-register"); err != nil {
				return fmt.Errorf("ledger reassign for self-register: %w", err)
			}
			break
		}
	}
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
	defer func() {
		if err := f.Close(); err != nil {
			log.Printf("control-plane: audit flush close failed: %v", err)
		}
	}()
	for _, e := range events {
		if _, err := fmt.Fprintf(f, "%s\t%s\t%s\t%s\n", e.Timestamp.UTC().Format(time.RFC3339), e.EventType, e.NodeName, e.Detail); err != nil {
			log.Printf("control-plane: audit flush write failed: %v", err)
			return
		}
	}
}
