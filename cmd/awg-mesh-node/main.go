// awg-mesh-node v2.0 — role-aware entrypoint skeleton.
//
// CR-001 (F-009 foundation): this file is a skeleton entrypoint that parses
// `--mode` and `--version`, prints the chosen role, and exits cleanly. Daemon
// implementations land in subsequent CRs:
//
//	control-plane → CR-002 (mesh-ctl daemon, ledger, peer-list distribution)
//	master        → CR-004 (vanilla-WG + AmneziaWG dual listener)
//	clientd       → CR-003 (self-config agent on every non-Mikrotik node)
//	egress        → CR-005 (MASQUERADE on internet-bound iface)
//	ingress       → CR-006 (SNI passthrough + TLS terminate + ACME + HTTP/3 + UDP)
//	balancer      → CR-007 (policy engine: dumb / labeled / smart-future)
//
// At v2.0.0-alpha.1 every --mode prints a placeholder line and exits 0. This
// makes CR-001 atomic: the binary compiles and runs end-to-end without v1.x
// code, even though no role does any networking yet.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"strings"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/awgmesh"
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
		mode    = flag.String("mode", "", "node mode: control-plane | master | clientd | egress | ingress | balancer")
		version = flag.Bool("version", false, "print version and exit")
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

	// Validate role-tag composability for non-control-plane modes when --mode
	// represents a single role flag. control-plane is not a role; it's the
	// mesh-ctl daemon. Other modes map 1:1 to role.Role.
	if *mode != "control-plane" {
		r := role.Role(*mode)
		// "clientd" daemon serves the "client" role; map manually.
		if *mode == "clientd" {
			r = role.RoleClient
		}
		if err := role.ValidateComposability([]role.Role{r}); err != nil {
			fmt.Fprintf(os.Stderr, "error: role %q failed validation: %v\n", *mode, err)
			os.Exit(2)
		}
	}

	fmt.Printf("awg-mesh-node %s — mode=%s — daemon implementation lands in %s\n",
		versionString(), *mode, implCR)
	fmt.Fprintln(os.Stderr, "CR-001 skeleton: this binary intentionally exits without doing any networking.")
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
