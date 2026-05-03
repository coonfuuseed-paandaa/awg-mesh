package clientd

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/awgmesh"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/role"
	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/pkg/tls"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
	pb "github.com/coonfuuseed-paandaa/awg-mesh/proto/control_plane"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// CommandConfig is parsed CLI configuration for standalone clientd and awg-mesh-node --mode clientd.
type CommandConfig struct {
	ControlPlane              string
	Name                      string
	OverlayIP                 string
	Region                    string
	CertPath                  string
	KeyPath                   string
	CACertPath                string
	StateDir                  string
	InterfaceName             string
	Protocol                  wg.Protocol
	Roles                     []role.Role
	AllowInsecureControlPlane bool
}

// RunCommand parses args, builds the clientd agent, and blocks until the agent exits.
func RunCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cfg, err := ParseCommandConfig(args, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "clientd: %v\n", err)
		return 2
	}
	if err := RunWithConfig(ctx, cfg, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "clientd: %v\n", err)
		return 1
	}
	return 0
}

// ParseCommandConfig parses common clientd flags.
func ParseCommandConfig(args []string, output io.Writer) (CommandConfig, error) {
	fs := flag.NewFlagSet("clientd", flag.ContinueOnError)
	fs.SetOutput(output)
	cfg := CommandConfig{}
	protocol := string(wg.ProtocolAmneziaWG)
	fs.StringVar(&cfg.ControlPlane, "control-plane", "", "control-plane gRPC address")
	fs.StringVar(&cfg.Name, "name", "", "node name")
	fs.StringVar(&cfg.OverlayIP, "overlay-ip", "", "assigned overlay IP")
	fs.StringVar(&cfg.Region, "region", "", "node region")
	fs.StringVar(&cfg.CertPath, "cert", "", "node certificate PEM path")
	fs.StringVar(&cfg.KeyPath, "key", "", "node private key PEM path")
	fs.StringVar(&cfg.CACertPath, "ca-cert", "", "mesh CA certificate PEM path for mTLS control-plane connections")
	fs.StringVar(&cfg.StateDir, "state-dir", "/var/lib/awg-mesh", "clientd state directory")
	fs.StringVar(&cfg.InterfaceName, "iface", "awg-mesh0", "WireGuard interface name")
	fs.StringVar(&protocol, "protocol", string(wg.ProtocolAmneziaWG), "transport protocol: vanilla-wg or amneziawg")
	fs.BoolVar(&cfg.AllowInsecureControlPlane, "allow-insecure-control-plane", false, "allow insecure control-plane gRPC to non-loopback targets")
	if err := fs.Parse(args); err != nil {
		return CommandConfig{}, err
	}
	cfg.Protocol = wg.Protocol(protocol)
	return ValidateCommandConfig(cfg)
}

// ValidateCommandConfig validates CLI config without opening network resources.
func ValidateCommandConfig(cfg CommandConfig) (CommandConfig, error) {
	missing := make([]string, 0)
	if strings.TrimSpace(cfg.ControlPlane) == "" {
		missing = append(missing, "--control-plane")
	}
	if strings.TrimSpace(cfg.Name) == "" {
		missing = append(missing, "--name")
	}
	if strings.TrimSpace(cfg.OverlayIP) == "" {
		missing = append(missing, "--overlay-ip")
	}
	if strings.TrimSpace(cfg.Region) == "" {
		missing = append(missing, "--region")
	}
	if strings.TrimSpace(cfg.CertPath) == "" {
		missing = append(missing, "--cert")
	}
	if strings.TrimSpace(cfg.StateDir) == "" {
		missing = append(missing, "--state-dir")
	}
	if strings.TrimSpace(cfg.InterfaceName) == "" {
		missing = append(missing, "--iface")
	}
	if len(missing) > 0 {
		return CommandConfig{}, fmt.Errorf("missing required flags: %s", strings.Join(missing, ", "))
	}
	if strings.TrimSpace(cfg.KeyPath) == "" {
		cfg.KeyPath = filepath.Join(filepath.Dir(cfg.CertPath), "node.key")
	}
	if strings.TrimSpace(cfg.CACertPath) == "" {
		if candidate := defaultCACertPath(cfg.CertPath); regularFile(candidate) {
			cfg.CACertPath = candidate
		}
	}
	if cfg.Protocol != wg.ProtocolVanilla && cfg.Protocol != wg.ProtocolAmneziaWG {
		return CommandConfig{}, fmt.Errorf("invalid --protocol %q", cfg.Protocol)
	}
	if len(cfg.Roles) == 0 {
		cfg.Roles = []role.Role{role.RoleClient}
	}
	if err := role.ValidateComposability(cfg.Roles); err != nil {
		return CommandConfig{}, fmt.Errorf("validate roles: %w", err)
	}
	if err := wg.ValidateInterfaceName(cfg.InterfaceName); err != nil {
		return CommandConfig{}, fmt.Errorf("invalid --iface: %w", err)
	}
	if !cfg.AllowInsecureControlPlane && !isLoopbackControlPlaneTarget(cfg.ControlPlane) {
		return CommandConfig{}, fmt.Errorf("insecure control-plane target %q must be loopback or require --allow-insecure-control-plane", cfg.ControlPlane)
	}
	return cfg, nil
}

func isLoopbackControlPlaneTarget(target string) bool {
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return addr.IsLoopback()
}

// RunWithConfig creates the gRPC client and transport configurator, then runs the agent.
func RunWithConfig(ctx context.Context, cfg CommandConfig, stdout io.Writer) error {
	validated, err := ValidateCommandConfig(cfg)
	if err != nil {
		return err
	}
	cfg = validated
	certPEM, err := os.ReadFile(cfg.CertPath)
	if err != nil {
		return fmt.Errorf("read cert: %w", err)
	}
	transportCredentials, err := controlPlaneTransportCredentials(cfg)
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(cfg.ControlPlane, grpc.WithTransportCredentials(transportCredentials))
	if err != nil {
		return fmt.Errorf("create control-plane client: %w", err)
	}
	defer func() { _ = conn.Close() }()

	transport, err := newTransport(cfg.Protocol, cfg.InterfaceName)
	if err != nil {
		return err
	}
	defer func() { _ = transport.Close() }()

	agent, err := NewAgent(Config{
		NodeName:      cfg.Name,
		Roles:         cfg.Roles,
		OverlayIP:     cfg.OverlayIP,
		Region:        cfg.Region,
		NodeCertPEM:   certPEM,
		CertPath:      cfg.CertPath,
		KeyPath:       cfg.KeyPath,
		Version:       awgmesh.Version,
		InterfaceName: cfg.InterfaceName,
		Protocol:      cfg.Protocol,
		StatePath:     filepath.Join(cfg.StateDir, "clientd-state.json"),
	}, pb.NewControlPlaneClient(conn), TransportConfigurator{Transport: transport, LocalRoles: cfg.Roles})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "clientd node=%s control-plane=%s iface=%s protocol=%s\n", cfg.Name, cfg.ControlPlane, cfg.InterfaceName, cfg.Protocol)
	return agent.Run(ctx)
}

func controlPlaneTransportCredentials(cfg CommandConfig) (credentials.TransportCredentials, error) {
	if strings.TrimSpace(cfg.CACertPath) == "" {
		return insecure.NewCredentials(), nil
	}
	rootCAs, err := pkgtls.LoadCACert(cfg.CACertPath)
	if err != nil {
		return nil, fmt.Errorf("load control-plane CA cert: %w", err)
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertPath, cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("load client cert/key: %w", err)
	}
	return credentials.NewTLS(&tls.Config{
		RootCAs:      rootCAs,
		Certificates: []tls.Certificate{cert},
		ServerName:   controlPlaneServerName(cfg.ControlPlane),
		MinVersion:   tls.VersionTLS13,
	}), nil
}

func controlPlaneServerName(target string) string {
	host, _, err := net.SplitHostPort(target)
	if err != nil || host == "" {
		return "localhost"
	}
	return strings.Trim(host, "[]")
}

func defaultCACertPath(certPath string) string {
	certDir := filepath.Dir(certPath)
	return filepath.Join(filepath.Dir(filepath.Dir(certDir)), "ca.crt")
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func newTransport(protocol wg.Protocol, name string) (wg.Transport, error) {
	switch protocol {
	case wg.ProtocolVanilla:
		return wg.NewVanillaTransport(name)
	case wg.ProtocolAmneziaWG:
		return wg.NewAWGTransport(name)
	default:
		return nil, errors.New("unsupported protocol")
	}
}
