// awg-mesh-node v2.0 — role-aware entrypoint.
//
// CR-001/CR-002 (F-009 foundation): skeleton entrypoint with control-plane
// daemon wired. Other modes still print a placeholder line and exit 0:
//
//	control-plane → CR-002 (mesh-ctl daemon, ledger, peer-list distribution) — IMPLEMENTED
//	master        → CR-004 (vanilla-WG + AmneziaWG dual listener)
//	clientd       → CR-003 (self-config agent on every non-Mikrotik node)
//	egress        → CR-005 (MASQUERADE on internet-bound iface)
//	ingress       → CR-006 (SNI passthrough + TLS terminate + ACME + HTTP/3 + UDP)
//	balancer      → CR-007 (policy engine: dumb / labeled / smart-future)
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"strings"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/awgmesh"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/control_plane"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/role"
)

// supportedModes is the set of valid --mode values + the implementing CR.
// Daemon logic for each mode lands in the listed CR.
var supportedModes = map[string]string{
	"control-plane": "CR-002",
	"master":        "CR-004",
	"clientd":       "CR-003",
	"egress":        "CR-005",
	"ingress":       "CR-006",
	"balancer":      "CR-007",
}

// versionString returns "<Version> (<commit>)" when build info is available,
// or just <Version> otherwise. Matches the v1.x pattern in cmd/mesh-ctl.
func versionString() string {
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

func main() {
	var (
		mode                    = flag.String("mode", "", "node mode: control-plane | master | clientd | egress | ingress | balancer")
		version                 = flag.Bool("version", false, "print version and exit")
		listenAddr              = flag.String("listen", "127.0.0.1:51820", "control-plane: gRPC listen addr")
		stateDir                = flag.String("state-dir", "/var/lib/awg-mesh", "control-plane: state directory")
		auditCap                = flag.Int("audit-cap", 8192, "control-plane: in-memory audit ring capacity")
		allowInsecurePublicBind = flag.Bool("allow-insecure-public-bind", false, "control-plane: allow binding insecure gRPC to non-loopback or wildcard addresses")
	)
	flag.Parse()

	if *version {
		fmt.Printf("awg-mesh-node %s\n", versionString())
		os.Exit(0)
	}

	if *mode == "" {
		fmt.Fprintln(os.Stderr, "error: --mode is required")
		fmt.Fprintf(os.Stderr, "supported modes: %s\n", strings.Join(sortedKeys(supportedModes), ", "))
		os.Exit(2)
	}

	implCR, ok := supportedModes[*mode]
	if !ok {
		fmt.Fprintf(os.Stderr, "error: unsupported mode %q\n", *mode)
		fmt.Fprintf(os.Stderr, "supported modes: %s\n", strings.Join(sortedKeys(supportedModes), ", "))
		os.Exit(2)
	}

	if *mode != "control-plane" {
		r := role.Role(*mode)
		if *mode == "clientd" {
			r = role.RoleClient
		}
		if err := role.ValidateComposability([]role.Role{r}); err != nil {
			fmt.Fprintf(os.Stderr, "error: role %q failed validation: %v\n", *mode, err)
			os.Exit(2)
		}
	}

	if *mode == "control-plane" {
		runControlPlane(*listenAddr, *stateDir, *auditCap, *allowInsecurePublicBind)
		return
	}

	fmt.Printf("awg-mesh-node %s — mode=%s — daemon implementation lands in %s\n",
		versionString(), *mode, implCR)
	fmt.Fprintln(os.Stderr, "CR-001 skeleton: this binary intentionally exits without doing any networking.")
}

func runControlPlane(listenAddr, stateDir string, auditCap int, allowInsecurePublicBind bool) {
	fmt.Printf("awg-mesh-node %s — mode=control-plane — listen=%s state=%s\n",
		versionString(), listenAddr, stateDir)
	d, err := control_plane.NewDaemon(control_plane.Config{
		ListenAddr:              listenAddr,
		StateDir:                stateDir,
		AuditCap:                auditCap,
		AllowInsecurePublicBind: allowInsecurePublicBind,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "control-plane: %v\n", err)
		os.Exit(1)
	}
	if err := d.Run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "control-plane: %v\n", err)
		os.Exit(1)
	}
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
