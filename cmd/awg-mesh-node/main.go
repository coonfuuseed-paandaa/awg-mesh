// awg-mesh-node v2.0 — role-aware entrypoint.
//
// Implemented modes:
//
//	control-plane → CR-002 (mesh-ctl daemon, ledger, peer-list distribution)
//	master        → CR-004 (vanilla-WG + AmneziaWG dual listener)
//	clientd       → CR-003 (self-config agent on every non-Mikrotik node)
//	egress        → CR-005 (clientd + MASQUERADE on internet-bound iface)
//
// Remaining role implementations still print placeholders until their CRs land:
//
//	ingress       → CR-006 (SNI passthrough + TLS terminate + ACME + HTTP/3 + UDP)
//	balancer      → CR-007 (policy engine: dumb / labeled / smart-future)
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/awgmesh"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/clientd"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/control_plane"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/node"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/role"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
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
	auditCap := fs.Int("audit-cap", 8192, "control-plane: in-memory audit ring capacity")
	allowInsecurePublicBind := fs.Bool("allow-insecure-public-bind", false, "control-plane: allow binding insecure gRPC to non-loopback or wildcard addresses")
	controlPlaneAddr := fs.String("control-plane", "", "clientd: control-plane gRPC addr")
	nodeName := fs.String("name", "", "node name")
	overlayIP := fs.String("overlay-ip", "", "assigned overlay IP")
	clientdRegion := fs.String("region", "", "clientd: node region")
	clientdCert := fs.String("cert", "", "clientd: node certificate PEM path")
	clientdIface := fs.String("iface", "awg-mesh0", "clientd: WireGuard interface name")
	clientdProtocol := fs.String("protocol", string(wg.ProtocolAmneziaWG), "clientd: transport protocol: vanilla-wg or amneziawg")
	allowInsecureControlPlane := fs.Bool("allow-insecure-control-plane", false, "clientd: allow insecure control-plane gRPC to non-loopback targets")
	masterClientIface := fs.String("client-iface", wg.DefaultClientInterfaceName, "master: client-facing vanilla-WG interface name")
	masterMeshIface := fs.String("mesh-iface", wg.DefaultMeshInterfaceName, "master: mesh-internal AmneziaWG interface name")
	masterClientListenPort := fs.Int("client-listen-port", wg.DefaultClientListenPort, "master: client-facing vanilla-WG UDP listen port")
	masterMeshListenPort := fs.Int("mesh-listen-port", wg.DefaultMeshListenPort, "master: mesh-internal AmneziaWG UDP listen port")
	egressInternetIface := fs.String("internet-iface", "", "egress: internet-bound interface for MASQUERADE")
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
		return runControlPlane(*listenAddr, *stateDir, *auditCap, *allowInsecurePublicBind, stdout, stderr)
	case "master":
		return runMaster(context.Background(), node.MasterConfig{
			Name:      *nodeName,
			OverlayIP: *overlayIP,
			DualListener: wg.DualListenerConfig{
				ClientInterfaceName: *masterClientIface,
				MeshInterfaceName:   *masterMeshIface,
				ClientListenPort:    *masterClientListenPort,
				MeshListenPort:      *masterMeshListenPort,
			},
		}, *dryRun, stdout, stderr)
	case "endpoint", "egress":
		clientdCfg := clientd.CommandConfig{
			ControlPlane:              *controlPlaneAddr,
			Name:                      *nodeName,
			OverlayIP:                 *overlayIP,
			Region:                    *clientdRegion,
			CertPath:                  *clientdCert,
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
		return clientd.RunCommand(context.Background(), clientdArgs, stdout, stderr)
	default:
		writef(stdout, "awg-mesh-node %s — mode=%s — daemon implementation lands in %s\n",
			versionString(), *mode, implCR)
		writeLine(stderr, "CR-001 skeleton: this binary intentionally exits without doing any networking.")
		return 0
	}
}

func runControlPlane(listenAddr, stateDir string, auditCap int, allowInsecurePublicBind bool, stdout, stderr io.Writer) int {
	writef(stdout, "awg-mesh-node %s — mode=control-plane — listen=%s state=%s\n",
		versionString(), listenAddr, stateDir)
	d, err := control_plane.NewDaemon(control_plane.Config{
		ListenAddr:              listenAddr,
		StateDir:                stateDir,
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

func runMaster(ctx context.Context, cfg node.MasterConfig, dryRun bool, stdout, stderr io.Writer) int {
	master, err := node.NewMaster(cfg)
	if err != nil {
		writef(stderr, "master: %v\n", err)
		return 2
	}
	status := master.Status()
	if dryRun {
		writef(stdout, "master dry-run node=%s overlay=%s client=%s:%d/%s mesh=%s:%d/%s\n",
			status.Name,
			status.OverlayIP,
			status.Listeners.ClientInterfaceName,
			status.Listeners.ClientListenPort,
			status.Listeners.ClientProtocol,
			status.Listeners.MeshInterfaceName,
			status.Listeners.MeshListenPort,
			status.Listeners.MeshProtocol,
		)
		return 0
	}
	writef(stdout, "awg-mesh-node %s — mode=master — node=%s overlay=%s client=%s:%d mesh=%s:%d\n",
		versionString(),
		status.Name,
		status.OverlayIP,
		status.Listeners.ClientInterfaceName,
		status.Listeners.ClientListenPort,
		status.Listeners.MeshInterfaceName,
		status.Listeners.MeshListenPort,
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
