package clientd

import (
	"context"
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
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
	pb "github.com/coonfuuseed-paandaa/awg-mesh/proto/control_plane"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// CommandConfig is parsed CLI configuration for standalone clientd and awg-mesh-node --mode clientd.
type CommandConfig struct {
	ControlPlane              string
	Name                      string
	OverlayIP                 string
	Region                    string
	CertPath                  string
	StateDir                  string
	InterfaceName             string
	Protocol                  wg.Protocol
	AllowInsecureControlPlane bool
}

// RunCommand parses args, builds the clientd agent, and blocks until the agent exits.
func RunCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cfg, err := ParseCommandConfig(args, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "clientd: %v\n", err)
		return 2
	}
	if err := RunWithConfig(ctx, cfg, stdout); err != nil {
		fmt.Fprintf(stderr, "clientd: %v\n", err)
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
	if cfg.Protocol != wg.ProtocolVanilla && cfg.Protocol != wg.ProtocolAmneziaWG {
		return CommandConfig{}, fmt.Errorf("invalid --protocol %q", cfg.Protocol)
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
	conn, err := grpc.NewClient(cfg.ControlPlane, grpc.WithTransportCredentials(insecure.NewCredentials()))
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
		Roles:         []role.Role{role.RoleClient},
		OverlayIP:     cfg.OverlayIP,
		Region:        cfg.Region,
		NodeCertPEM:   certPEM,
		Version:       awgmesh.Version,
		InterfaceName: cfg.InterfaceName,
		Protocol:      cfg.Protocol,
		StatePath:     filepath.Join(cfg.StateDir, "clientd-state.json"),
	}, pb.NewControlPlaneClient(conn), TransportConfigurator{Transport: transport})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "clientd node=%s control-plane=%s iface=%s protocol=%s\n", cfg.Name, cfg.ControlPlane, cfg.InterfaceName, cfg.Protocol)
	return agent.Run(ctx)
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
