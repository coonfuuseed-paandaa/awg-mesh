// awg-mesh-node v2.0 — role-aware entrypoint.
//
// Implemented modes:
//
//	control-plane → CR-002 compatibility/deprecated wrapper around coordination primitives
//	master        → CR-004 (vanilla-WG + AmneziaWG dual listener)
//	clientd       → CR-003 (self-config agent on every non-Mikrotik node)
//	egress        → CR-005 (clientd + MASQUERADE on internet-bound iface)
//	ingress       → CR-006 (SNI passthrough + TLS terminate + ACME + HTTP/3 + UDP)
//	balancer      → CR-007 (policy engine: dumb / labeled / smart-future)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/awgmesh"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/balancer"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/clientd"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/control_plane"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/ingress"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/node"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/role"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/routing"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/wg"
	"github.com/rs/zerolog"
)

// supportedModes is the set of valid --mode values + the implementing CR.
var supportedModes = map[string]string{
	"control-plane": "CR-002",
	"master":        "CR-004",
	"endpoint":      "CR-005",
	"client":        "CR-003",
	"clientd":       "CR-003",
	"egress":        "CR-005",
	"ingress":       "CR-006",
	"balancer":      "CR-007",
}

// versionFromBuild is injected at build time via:
//
//	go build -ldflags "-X main.versionFromBuild=<ref>"
var versionFromBuild = ""

// versionString returns "<Version> (<commit>)" when build info is available,
// or just <Version> otherwise. Matches the v1.x pattern in cmd/mesh-ctl.
func versionString() string {
	if versionFromBuild != "" {
		return versionFromBuild
	}
	v := awgmesh.Version
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 8 {
				return fmt.Sprintf("%s (%s)", v, s.Value[:8])
			}
		}
	}
	return v
}

func roleForMode(mode string) role.Role {
	switch mode {
	case "client", "clientd":
		return role.RoleClient
	case "endpoint":
		return role.RoleEgress
	default:
		return role.Role(mode)
	}
}

func warnDeprecatedMode(mode string, stderr io.Writer) {
	switch mode {
	case "client":
		writeLine(stderr, "warning: --mode client is deprecated for v2.0; use --mode clientd")
	case "endpoint":
		writeLine(stderr, "warning: --mode endpoint is deprecated for v2.0; use --mode egress")
	case "control-plane":
		writeLine(stderr, "warning: --mode control-plane is retained for v2.0.1 compatibility and deprecated for the current happy path; use --mode master with master-owned coordination")
	}
}

func writeLine(w io.Writer, msg string) {
	_, _ = fmt.Fprintln(w, msg)
}

func writef(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func main() {
	os.Exit(runCommand(os.Args[1:], os.Stdout, os.Stderr))
}

func runCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("awg-mesh-node", flag.ContinueOnError)
	fs.SetOutput(stderr)

	mode := fs.String("mode", "", "node mode: control-plane | master | endpoint | client | clientd | egress | ingress | balancer")
	version := fs.Bool("version", false, "print version and exit")
	listenAddr := fs.String("listen", "127.0.0.1:51820", "control-plane: gRPC listen addr")
	stateDir := fs.String("state-dir", "/var/lib/awg-mesh", "control-plane/clientd: state directory")
	caDir := fs.String("ca-dir", "", "control-plane: CA material directory containing ca.crt and ca.key")
	controlPlaneCertHosts := fs.String("cert-hosts", "", "control-plane: comma-separated additional DNS names/IPs for generated server certificate")
	certRotationDays := fs.Int("cert-rotation-days", 90, "control-plane: mTLS cert rotation interval in days")
	auditCap := fs.Int("audit-cap", 8192, "control-plane: in-memory audit ring capacity")
	allowInsecurePublicBind := fs.Bool("allow-insecure-public-bind", false, "control-plane: allow binding insecure gRPC to non-loopback or wildcard addresses")
	masterCoordinationListen := fs.String("coordination-listen", "", "master: coordination gRPC listen addr; enables master-owned coordination")
	masterCoordinationStateDir := fs.String("coordination-state-dir", "", "master: coordination state directory")
	masterCoordinationCADir := fs.String("coordination-ca-dir", "", "master: CA material directory containing ca.crt and ca.key for coordination mTLS")
	masterCoordinationCertHosts := fs.String("coordination-cert-hosts", "", "master: comma-separated additional DNS names/IPs for generated coordination server certificate")
	masterCoordinationCertRotationDays := fs.Int("coordination-cert-rotation-days", 0, "master: coordination mTLS cert rotation interval in days")
	masterCoordinationAuditCap := fs.Int("coordination-audit-cap", 0, "master: coordination in-memory audit ring capacity")
	allowInsecureCoordinationBind := fs.Bool("allow-insecure-coordination-bind", false, "master: allow binding insecure coordination gRPC to non-loopback or wildcard addresses")
	controlPlaneAddr := fs.String("control-plane", "", "clientd: control-plane gRPC addr")
	nodeName := fs.String("name", "", "node name")
	overlayIP := fs.String("overlay-ip", "", "assigned overlay IP")
	clientdRegion := fs.String("region", "", "clientd: node region")
	clientdCert := fs.String("cert", "", "clientd: node certificate PEM path")
	clientdKey := fs.String("key", "", "clientd: node private key PEM path")
	clientdCACert := fs.String("ca-cert", "", "clientd: mesh CA certificate PEM path for mTLS control-plane connections")
	clientdWGPrivateKey := fs.String("wireguard-private-key", "", "clientd: base64 WireGuard private key path generated by mesh-ctl node prepare")
	clientdIface := fs.String("iface", "awg-mesh0", "clientd: WireGuard interface name")
	clientdProtocol := fs.String("protocol", string(wg.ProtocolAmneziaWG), "clientd: transport protocol: vanilla-wg or amneziawg")
	allowInsecureControlPlane := fs.Bool("allow-insecure-control-plane", false, "clientd: allow insecure control-plane gRPC to non-loopback targets")
	masterClientIface := fs.String("client-iface", wg.DefaultClientInterfaceName, "master: client-facing vanilla-WG interface name")
	masterMeshIface := fs.String("mesh-iface", wg.DefaultMeshInterfaceName, "master: mesh-internal AmneziaWG interface name")
	masterClientListenPort := fs.Int("client-listen-port", wg.DefaultClientListenPort, "master: client-facing vanilla-WG UDP listen port")
	masterMeshListenPort := fs.Int("mesh-listen-port", wg.DefaultMeshListenPort, "master: mesh-internal AmneziaWG UDP listen port")
	masterPublicIP := fs.String("public-ip", "", "master: public IP/DNS name advertised as the mesh endpoint host")
	masterMeshEndpoint := fs.String("mesh-endpoint", "", "master: explicit public mesh endpoint host:port; overrides --public-ip + --mesh-listen-port")
	masterClientPrivateKeyFile := fs.String("client-private-key-file", "", "master: base64 WireGuard private key file for the client-facing vanilla-WG listener")
	var masterClientPeers routeFlags
	fs.Var(&masterClientPeers, "client-peer", "master: static client-facing peer public_key=allowed_cidr[,allowed_cidr]; repeatable")
	egressInternetIface := fs.String("internet-iface", "", "egress: internet-bound interface for MASQUERADE")
	ingressPublicAddr := fs.String("ingress-public-addr", "", "ingress: public HTTP/TLS bind address")
	ingressTenant := fs.String("ingress-tenant", ingress.DefaultTenant, "ingress: tenant for routes declared with --ingress-route")
	ingressTLSMode := fs.String("ingress-tls-mode", string(ingress.TLSModeTLSTerminate), "ingress: route TLS mode: sni_passthrough | tls_terminate | tcp_forward | udp_forward")
	ingressProtocol := fs.String("ingress-protocol", string(ingress.ProtocolHTTP), "ingress: route protocol: http | websocket | tcp | udp")
	ingressHealthInterval := fs.Duration("ingress-health-interval", ingress.DefaultHealthProbeInterval, "ingress: health probe interval")
	ingressUDPIdleTimeout := fs.Duration("ingress-udp-idle-timeout", ingress.DefaultUDPIdleTimeout, "ingress: UDP flow idle timeout")
	ingressMetricsAddr := fs.String("ingress-metrics", "", "ingress: Prometheus metrics bind address")
	ingressACMECache := fs.String("ingress-acme-cache", "", "ingress: ACME certificate cache directory")
	ingressACMEEmail := fs.String("ingress-acme-email", "", "ingress: ACME account email")
	ingressHTTP3 := fs.Bool("ingress-http3", false, "ingress: enable HTTP/3 when TLS is configured")
	var ingressRoutes routeFlags
	fs.Var(&ingressRoutes, "ingress-route", "ingress: hostname=overlay_ip:port route; repeatable")
	balancerMode := fs.String("balancer-mode", string(balancer.ModeDumb), "balancer: policy mode: dumb | labeled")
	balancerHealthInterval := fs.Duration("balancer-health-interval", balancer.DefaultHealthProbeInterval, "balancer: health probe interval")
	balancerFlowIdleTimeout := fs.Duration("balancer-flow-idle-timeout", balancer.DefaultFlowIdleTimeout, "balancer: sticky flow idle timeout")
	balancerMetricsAddr := fs.String("balancer-metrics", "", "balancer: Prometheus metrics bind address")
	var balancerEgresses routeFlags
	var balancerDSCP routeFlags
	var balancerFWMark routeFlags
	fs.Var(&balancerEgresses, "balancer-egress", "balancer: id=overlay_ip:port[,weight=N] egress target; repeatable")
	fs.Var(&balancerDSCP, "balancer-dscp", "balancer: dscp=egress-id mapping, for example 10=egress-ru; repeatable")
	fs.Var(&balancerFWMark, "balancer-fwmark", "balancer: fwmark=egress-id mapping, for example 100=egress-eu; repeatable")
	dryRun := fs.Bool("dry-run", false, "validate selected mode and print runtime plan without opening network resources")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *version {
		writef(stdout, "awg-mesh-node %s\n", versionString())
		return 0
	}

	if *mode == "" {
		writeLine(stderr, "error: --mode is required")
		writef(stderr, "supported modes: %s\n", strings.Join(sortedKeys(supportedModes), ", "))
		return 2
	}

	implCR, ok := supportedModes[*mode]
	if !ok {
		writef(stderr, "error: unsupported mode %q\n", *mode)
		writef(stderr, "supported modes: %s\n", strings.Join(sortedKeys(supportedModes), ", "))
		return 2
	}
	warnDeprecatedMode(*mode, stderr)

	if *mode != "control-plane" {
		r := roleForMode(*mode)
		if err := role.ValidateComposability([]role.Role{r}); err != nil {
			writef(stderr, "error: role %q failed validation: %v\n", *mode, err)
			return 2
		}
	}

	switch *mode {
	case "control-plane":
		return runControlPlane(*listenAddr, *stateDir, *caDir, parseCSV(*controlPlaneCertHosts), *certRotationDays, *auditCap, *allowInsecurePublicBind, stdout, stderr)
	case "master":
		clientPrivateKey, err := loadOptionalWGPrivateKey(*masterClientPrivateKeyFile)
		if err != nil {
			writef(stderr, "master: %v\n", err)
			return 2
		}
		clientPeers, err := parseMasterClientPeers(masterClientPeers)
		if err != nil {
			writef(stderr, "master: %v\n", err)
			return 2
		}
		coordination, err := buildMasterCoordinationConfig(
			*masterCoordinationListen,
			*masterCoordinationStateDir,
			*masterCoordinationCADir,
			parseCSV(*masterCoordinationCertHosts),
			*masterCoordinationCertRotationDays,
			*masterCoordinationAuditCap,
			*allowInsecureCoordinationBind,
		)
		if err != nil {
			writef(stderr, "master: %v\n", err)
			return 2
		}
		meshEndpoint, err := advertisedMeshEndpoint(*masterMeshEndpoint, *masterPublicIP, *masterMeshListenPort)
		if err != nil {
			writef(stderr, "master: %v\n", err)
			return 2
		}
		return runMaster(context.Background(), node.MasterConfig{
			Name:             *nodeName,
			OverlayIP:        *overlayIP,
			MeshEndpointHost: meshEndpoint,
			DualListener: wg.DualListenerConfig{
				ClientInterfaceName: *masterClientIface,
				MeshInterfaceName:   *masterMeshIface,
				ClientListenPort:    *masterClientListenPort,
				MeshListenPort:      *masterMeshListenPort,
				ClientPrivateKey:    clientPrivateKey,
				ClientPeers:         clientPeers,
			},
			Coordination:     coordination,
			LinkConfigurator: routing.NewNetlinkRouter(),
		}, *dryRun, stdout, stderr)
	case "endpoint", "egress":
		clientdCfg := clientd.CommandConfig{
			ControlPlane:              *controlPlaneAddr,
			Name:                      *nodeName,
			OverlayIP:                 *overlayIP,
			Region:                    *clientdRegion,
			CertPath:                  *clientdCert,
			KeyPath:                   *clientdKey,
			CACertPath:                *clientdCACert,
			WireGuardPrivateKeyPath:   *clientdWGPrivateKey,
			StateDir:                  *stateDir,
			InterfaceName:             *clientdIface,
			Protocol:                  wg.Protocol(*clientdProtocol),
			Roles:                     []role.Role{role.RoleEgress},
			AllowInsecureControlPlane: *allowInsecureControlPlane,
		}
		return runEgress(context.Background(), node.EgressConfig{
			Name:              *nodeName,
			OverlayIP:         *overlayIP,
			InternetInterface: *egressInternetIface,
		}, clientdCfg, *dryRun, stdout, stderr)
	case "ingress":
		cfg, err := buildIngressConfig(*nodeName, *overlayIP, *ingressPublicAddr, *ingressTenant, *ingressTLSMode, *ingressProtocol, *ingressHealthInterval, *ingressUDPIdleTimeout, *ingressMetricsAddr, *ingressACMECache, *ingressACMEEmail, *ingressHTTP3, ingressRoutes)
		if err != nil {
			writef(stderr, "ingress: %v\n", err)
			return 2
		}
		return runIngress(context.Background(), cfg, *dryRun, stdout, stderr)
	case "balancer":
		cfg, err := buildBalancerConfig(*nodeName, *overlayIP, *balancerMode, *balancerHealthInterval, *balancerFlowIdleTimeout, *balancerMetricsAddr, balancerEgresses, balancerDSCP, balancerFWMark)
		if err != nil {
			writef(stderr, "balancer: %v\n", err)
			return 2
		}
		return runBalancer(context.Background(), cfg, *dryRun, stdout, stderr)
	case "client", "clientd":
		clientdArgs := []string{
			"--control-plane", *controlPlaneAddr,
			"--name", *nodeName,
			"--overlay-ip", *overlayIP,
			"--region", *clientdRegion,
			"--cert", *clientdCert,
			"--state-dir", *stateDir,
			"--iface", *clientdIface,
			"--protocol", *clientdProtocol,
		}
		if *allowInsecureControlPlane {
			clientdArgs = append(clientdArgs, "--allow-insecure-control-plane")
		}
		if *clientdKey != "" {
			clientdArgs = append(clientdArgs, "--key", *clientdKey)
		}
		if *clientdCACert != "" {
			clientdArgs = append(clientdArgs, "--ca-cert", *clientdCACert)
		}
		if *clientdWGPrivateKey != "" {
			clientdArgs = append(clientdArgs, "--wireguard-private-key", *clientdWGPrivateKey)
		}
		return clientd.RunCommand(context.Background(), clientdArgs, stdout, stderr)
	default:
		writef(stdout, "awg-mesh-node %s — mode=%s — daemon implementation lands in %s\n",
			versionString(), *mode, implCR)
		writeLine(stderr, "CR-001 skeleton: this binary intentionally exits without doing any networking.")
		return 0
	}
}

func loadOptionalWGPrivateKey(path string) (*wg.Key, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, nil
	}
	data, err := os.ReadFile(trimmed)
	if err != nil {
		return nil, fmt.Errorf("read client private key file %q: %w", trimmed, err)
	}
	key, err := wg.ParseKey(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("parse client private key file %q: %w", trimmed, err)
	}
	if key.IsZero() {
		return nil, fmt.Errorf("client private key file %q must not contain the zero key", trimmed)
	}
	return &key, nil
}

func parseMasterClientPeers(values routeFlags) ([]wg.PeerConfig, error) {
	peers := make([]wg.PeerConfig, 0, len(values))
	for _, value := range values {
		separator := strings.LastIndex(value, "=")
		if separator < 0 {
			return nil, fmt.Errorf("--client-peer %q must use public_key=allowed_cidr[,allowed_cidr]", value)
		}
		rawKey := value[:separator]
		rawAllowed := value[separator+1:]
		key, err := wg.ParseKey(strings.TrimSpace(rawKey))
		if err != nil {
			return nil, fmt.Errorf("--client-peer %q has invalid public key: %w", value, err)
		}
		allowed, err := parseAllowedCIDRs(rawAllowed)
		if err != nil {
			return nil, fmt.Errorf("--client-peer %q: %w", value, err)
		}
		peers = append(peers, wg.PeerConfig{
			PublicKey:         key,
			ReplaceAllowedIPs: true,
			AllowedIPs:        allowed,
		})
	}
	return peers, nil
}

func parseAllowedCIDRs(raw string) ([]net.IPNet, error) {
	parts := strings.Split(raw, ",")
	allowed := make([]net.IPNet, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		_, ipNet, err := net.ParseCIDR(trimmed)
		if err != nil {
			return nil, fmt.Errorf("invalid allowed CIDR %q: %w", trimmed, err)
		}
		allowed = append(allowed, *ipNet)
	}
	if len(allowed) == 0 {
		return nil, errors.New("at least one allowed CIDR is required")
	}
	return allowed, nil
}

func runControlPlane(listenAddr, stateDir, caDir string, certHosts []string, certRotationDays, auditCap int, allowInsecurePublicBind bool, stdout, stderr io.Writer) int {
	writef(stdout, "awg-mesh-node %s — mode=control-plane — listen=%s state=%s\n",
		versionString(), listenAddr, stateDir)
	d, err := control_plane.NewDaemon(control_plane.Config{
		ListenAddr:              listenAddr,
		StateDir:                stateDir,
		CADir:                   caDir,
		CertHosts:               certHosts,
		CertRotationDays:        certRotationDays,
		AuditCap:                auditCap,
		AllowInsecurePublicBind: allowInsecurePublicBind,
	})
	if err != nil {
		writef(stderr, "control-plane: %v\n", err)
		return 1
	}
	if err := d.Run(context.Background()); err != nil {
		writef(stderr, "control-plane: %v\n", err)
		return 1
	}
	return 0
}

func parseCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func buildMasterCoordinationConfig(listenAddr, stateDir, caDir string, certHosts []string, certRotationDays, auditCap int, allowInsecureBind bool) (*node.MasterCoordinationConfig, error) {
	listenAddr = strings.TrimSpace(listenAddr)
	if listenAddr == "" {
		if strings.TrimSpace(stateDir) != "" ||
			strings.TrimSpace(caDir) != "" ||
			len(certHosts) > 0 ||
			certRotationDays != 0 ||
			auditCap != 0 ||
			allowInsecureBind {
			return nil, errors.New("--coordination-listen is required when any other --coordination-* flag is set")
		}
		return nil, nil
	}
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		return nil, errors.New("--coordination-state-dir is required when --coordination-listen is set")
	}
	return &node.MasterCoordinationConfig{
		ListenAddr:              listenAddr,
		StateDir:                stateDir,
		CADir:                   strings.TrimSpace(caDir),
		CertHosts:               append([]string(nil), certHosts...),
		CertRotationDays:        certRotationDays,
		AuditCap:                auditCap,
		AllowInsecurePublicBind: allowInsecureBind,
	}, nil
}

func advertisedMeshEndpoint(explicitEndpoint, publicIP string, meshListenPort int) (string, error) {
	explicitEndpoint = strings.TrimSpace(explicitEndpoint)
	publicIP = strings.TrimSpace(publicIP)
	if explicitEndpoint != "" {
		return explicitEndpoint, validateEndpointAddress(explicitEndpoint, "--mesh-endpoint")
	}
	if publicIP == "" {
		return "", nil
	}
	if _, _, err := net.SplitHostPort(publicIP); err == nil {
		return "", errors.New("--public-ip must not include a port; use --mesh-endpoint for explicit host:port")
	}
	if strings.Trim(publicIP, "[]") == "" {
		return "", errors.New("--public-ip must not be empty")
	}
	endpoint := net.JoinHostPort(strings.Trim(publicIP, "[]"), strconv.Itoa(meshListenPort))
	return endpoint, validateEndpointAddress(endpoint, "--public-ip")
}

func validateEndpointAddress(endpoint, flagName string) error {
	host, portText, err := net.SplitHostPort(endpoint)
	if err != nil {
		return fmt.Errorf("%s must be host:port: %w", flagName, err)
	}
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("%s must include a non-empty host", flagName)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("%s has invalid port %q", flagName, portText)
	}
	return nil
}

func runMaster(ctx context.Context, cfg node.MasterConfig, dryRun bool, stdout, stderr io.Writer) int {
	master, err := node.NewMaster(cfg)
	if err != nil {
		writef(stderr, "master: %v\n", err)
		return 2
	}
	status := master.Status()
	coordination := " coordination=disabled"
	if status.Coordination.Enabled {
		coordination = fmt.Sprintf(" coordination=%s", status.Coordination.ListenAddr)
	}
	meshEndpoint := ""
	if status.MeshEndpointHost != "" {
		meshEndpoint = fmt.Sprintf(" endpoint=%s", status.MeshEndpointHost)
	}
	if dryRun {
		writef(stdout, "master dry-run node=%s overlay=%s client=%s:%d/%s mesh=%s:%d/%s%s%s\n",
			status.Name,
			status.OverlayIP,
			status.Listeners.ClientInterfaceName,
			status.Listeners.ClientListenPort,
			status.Listeners.ClientProtocol,
			status.Listeners.MeshInterfaceName,
			status.Listeners.MeshListenPort,
			status.Listeners.MeshProtocol,
			meshEndpoint,
			coordination,
		)
		return 0
	}
	writef(stdout, "awg-mesh-node %s — mode=master — node=%s overlay=%s client=%s:%d mesh=%s:%d%s%s\n",
		versionString(),
		status.Name,
		status.OverlayIP,
		status.Listeners.ClientInterfaceName,
		status.Listeners.ClientListenPort,
		status.Listeners.MeshInterfaceName,
		status.Listeners.MeshListenPort,
		meshEndpoint,
		coordination,
	)
	if err := master.Run(ctx); err != nil {
		writef(stderr, "master: %v\n", err)
		return 1
	}
	return 0
}

func runEgress(ctx context.Context, cfg node.EgressConfig, clientCfg clientd.CommandConfig, dryRun bool, stdout, stderr io.Writer) int {
	egress, err := node.NewEgress(cfg)
	if err != nil {
		writef(stderr, "egress: %v\n", err)
		return 2
	}
	status := egress.Status()
	if dryRun {
		writef(stdout, "egress dry-run node=%s overlay=%s internet=%s nat=%s:%s/%s\n",
			status.Name,
			status.OverlayIP,
			status.InternetInterface,
			status.Masquerade.Table,
			status.Masquerade.Chain,
			status.Masquerade.Operation,
		)
		return 0
	}
	validatedClientCfg, err := clientd.ValidateCommandConfig(clientCfg)
	if err != nil {
		writef(stderr, "egress: clientd: %v\n", err)
		return 2
	}
	cfg.AgentRunner = func(ctx context.Context) error {
		return clientd.RunWithConfig(ctx, validatedClientCfg, stdout)
	}
	egress, err = node.NewEgress(cfg)
	if err != nil {
		writef(stderr, "egress: %v\n", err)
		return 2
	}
	writef(stdout, "awg-mesh-node %s — mode=egress — node=%s overlay=%s internet=%s\n",
		versionString(),
		status.Name,
		status.OverlayIP,
		status.InternetInterface,
	)
	if err := egress.Run(ctx); err != nil {
		writef(stderr, "egress: %v\n", err)
		return 1
	}
	return 0
}

func runIngress(ctx context.Context, cfg ingress.Config, dryRun bool, stdout, stderr io.Writer) int {
	runtime, err := ingress.NewRuntime(cfg, zerolog.New(stderr).With().Timestamp().Str("component", "ingress").Logger())
	if err != nil {
		writef(stderr, "ingress: %v\n", err)
		return 2
	}
	plan := runtime.Plan()
	if dryRun {
		writef(stdout, "ingress dry-run node=%s overlay=%s public=%s routes=%d health=%s udp_idle=%s tls=%s http3=%t",
			plan.Name,
			plan.OverlayIP,
			plan.PublicAddress,
			plan.RouteCount,
			plan.HealthProbeInterval,
			plan.UDPIdleTimeout,
			tlsPlan(plan),
			plan.HTTP3Enabled,
		)
		if plan.MetricsAddress != "" {
			writef(stdout, " metrics=%s", plan.MetricsAddress)
		}
		for _, route := range plan.Routes {
			writef(stdout, " route=%s:%s->%s/%s", route.Tenant, route.Hostname, route.Target, route.Mode)
		}
		writeLine(stdout, "")
		return 0
	}
	writef(stdout, "awg-mesh-node %s — mode=ingress — node=%s overlay=%s public=%s routes=%d\n",
		versionString(), plan.Name, plan.OverlayIP, plan.PublicAddress, plan.RouteCount)
	if err := runtime.Run(ctx); err != nil {
		writef(stderr, "ingress: %v\n", err)
		return 1
	}
	return 0
}

func runBalancer(ctx context.Context, cfg balancer.Config, dryRun bool, stdout, stderr io.Writer) int {
	runtime, err := balancer.NewRuntime(cfg, zerolog.New(stderr).With().Timestamp().Str("component", "balancer").Logger())
	if err != nil {
		writef(stderr, "balancer: %v\n", err)
		return 2
	}
	plan := runtime.Plan()
	if dryRun {
		writef(stdout, "balancer dry-run node=%s overlay=%s mode=%s egresses=%d health=%s flow_idle=%s",
			plan.Name,
			plan.OverlayIP,
			plan.Mode,
			plan.EgressCount,
			plan.HealthProbeInterval,
			plan.FlowIdleTimeout,
		)
		if plan.MetricsAddress != "" {
			writef(stdout, " metrics=%s", plan.MetricsAddress)
		}
		for _, egress := range plan.Egresses {
			writef(stdout, " egress=%s->%s/weight=%d", egress.ID, egress.Target, egress.Weight)
		}
		for _, label := range plan.Labels {
			writef(stdout, " %s=%d->%s", label.Type, label.Value, label.EgressID)
		}
		writeLine(stdout, "")
		return 0
	}
	writef(stdout, "awg-mesh-node %s — mode=balancer — node=%s overlay=%s policy=%s egresses=%d\n",
		versionString(), plan.Name, plan.OverlayIP, plan.Mode, plan.EgressCount)
	if err := runtime.Run(ctx); err != nil {
		writef(stderr, "balancer: %v\n", err)
		return 1
	}
	return 0
}

func buildIngressConfig(name, overlayIP, publicAddr, tenant, tlsMode, protocol string, healthInterval, udpIdle time.Duration, metricsAddr, acmeCache, acmeEmail string, enableHTTP3 bool, routeValues routeFlags) (ingress.Config, error) {
	routes := make([]ingress.Route, 0, len(routeValues))
	for _, value := range routeValues {
		hostname, target, ok := strings.Cut(value, "=")
		if !ok {
			return ingress.Config{}, fmt.Errorf("--ingress-route %q must use hostname=overlay_ip:port", value)
		}
		routes = append(routes, ingress.Route{
			Tenant:   tenant,
			Hostname: hostname,
			Target:   target,
			Mode:     ingress.TLSMode(tlsMode),
			Protocol: ingress.Protocol(protocol),
			HTTP3:    enableHTTP3,
		})
	}
	return ingress.Config{
		Name:                name,
		OverlayIP:           overlayIP,
		PublicAddress:       publicAddr,
		Routes:              routes,
		HealthProbeInterval: healthInterval,
		UDPIdleTimeout:      udpIdle,
		MetricsAddress:      metricsAddr,
		ACMECacheDir:        acmeCache,
		ACMEEmail:           acmeEmail,
		EnableHTTP3:         enableHTTP3,
	}, nil
}

func tlsPlan(plan ingress.Plan) string {
	if plan.ACMEEnabled {
		return "acme"
	}
	return "plain"
}

func buildBalancerConfig(name, overlayIP, mode string, healthInterval, flowIdle time.Duration, metricsAddr string, egressValues, dscpValues, fwmarkValues routeFlags) (balancer.Config, error) {
	egresses := make([]balancer.EgressTarget, 0, len(egressValues))
	for _, value := range egressValues {
		egress, err := parseBalancerEgress(value)
		if err != nil {
			return balancer.Config{}, err
		}
		egresses = append(egresses, egress)
	}
	labels := make([]balancer.LabelMapping, 0, len(dscpValues)+len(fwmarkValues))
	for _, value := range dscpValues {
		label, err := parseBalancerLabel(balancer.LabelDSCP, value)
		if err != nil {
			return balancer.Config{}, err
		}
		labels = append(labels, label)
	}
	for _, value := range fwmarkValues {
		label, err := parseBalancerLabel(balancer.LabelFWMark, value)
		if err != nil {
			return balancer.Config{}, err
		}
		labels = append(labels, label)
	}
	return balancer.Config{
		Name:                name,
		OverlayIP:           overlayIP,
		Mode:                balancer.Mode(mode),
		Egresses:            egresses,
		Labels:              labels,
		HealthProbeInterval: healthInterval,
		FlowIdleTimeout:     flowIdle,
		MetricsAddress:      metricsAddr,
	}, nil
}

func parseBalancerEgress(value string) (balancer.EgressTarget, error) {
	id, rest, ok := strings.Cut(value, "=")
	if !ok {
		return balancer.EgressTarget{}, fmt.Errorf("--balancer-egress %q must use id=overlay_ip:port[,weight=N]", value)
	}
	parts := strings.Split(rest, ",")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		return balancer.EgressTarget{}, fmt.Errorf("--balancer-egress %q is missing target endpoint", value)
	}
	egress := balancer.EgressTarget{ID: id, Target: strings.TrimSpace(parts[0]), Weight: 1}
	for _, raw := range parts[1:] {
		key, val, found := strings.Cut(strings.TrimSpace(raw), "=")
		if !found {
			return balancer.EgressTarget{}, fmt.Errorf("--balancer-egress %q has invalid option %q", value, raw)
		}
		switch key {
		case "weight":
			weight, err := strconv.Atoi(val)
			if err != nil {
				return balancer.EgressTarget{}, fmt.Errorf("--balancer-egress %q has invalid weight %q", value, val)
			}
			egress.Weight = weight
		default:
			return balancer.EgressTarget{}, fmt.Errorf("--balancer-egress %q has unsupported option %q", value, key)
		}
	}
	return egress, nil
}

func parseBalancerLabel(labelType balancer.LabelType, value string) (balancer.LabelMapping, error) {
	rawNumber, egressID, ok := strings.Cut(value, "=")
	if !ok {
		return balancer.LabelMapping{}, fmt.Errorf("--balancer-%s %q must use value=egress-id", labelType, value)
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(rawNumber))
	if err != nil {
		return balancer.LabelMapping{}, fmt.Errorf("--balancer-%s %q has invalid numeric value %q", labelType, value, rawNumber)
	}
	return balancer.LabelMapping{Type: labelType, Value: parsed, EgressID: strings.TrimSpace(egressID)}, nil
}

type routeFlags []string

func (f *routeFlags) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(*f, ",")
}

func (f *routeFlags) Set(value string) error {
	*f = append(*f, value)
	return nil
}

// sortedKeys returns the keys of a map[string]string in lexicographic order.
// Inlined here to keep the skeleton free of new dependencies.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Tiny n; insertion sort keeps the binary lean and avoids importing "sort".
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
