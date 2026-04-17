package cmd

import (
	"crypto"
	"crypto/x509"
	"embed"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/transport"
	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/pkg/tls"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

func loadTemplate(name string) (string, error) {
	content, err := templateFS.ReadFile("templates/" + name)
	if err != nil {
		return "", fmt.Errorf("read template %q: %w", name, err)
	}

	return string(content), nil
}

func ensureCA(configDir string) (*x509.Certificate, crypto.PrivateKey, error) {
	caCert, caKey, err := pkgtls.LoadCA(configDir)
	if err == nil {
		return caCert, caKey, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}

	fmt.Fprintf(os.Stderr, "First run: creating mesh CA in %s\n", configDir)

	caCert, caKey, err = pkgtls.GenerateCA("awg-mesh-ca")
	if err != nil {
		return nil, nil, fmt.Errorf("generate CA: %w", err)
	}

	if err := pkgtls.SaveCA(configDir, caCert, caKey); err != nil {
		return nil, nil, fmt.Errorf("save CA: %w", err)
	}

	fmt.Fprintf(os.Stderr, "CA created: %s/ca.crt, %s/ca.key\n", configDir, configDir)
	return caCert, caKey, nil
}

func saveToken(nodeDir, token string) error {
	if err := os.MkdirAll(nodeDir, 0700); err != nil {
		return fmt.Errorf("create node dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(nodeDir, "token"), []byte(token), 0600); err != nil {
		return fmt.Errorf("write token file: %w", err)
	}
	return nil
}

func loadToken(nodeDir string) (string, error) {
	rawToken, err := os.ReadFile(filepath.Join(nodeDir, "token"))
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	return strings.TrimSpace(string(rawToken)), nil
}

func loadOrCreateAllocator(configDir string, topo *topology.Topology) (*transport.Allocator, error) {
	cleanConfigDir := strings.TrimSpace(configDir)
	if cleanConfigDir == "" {
		return nil, fmt.Errorf("config directory is required")
	}
	if topo == nil {
		return nil, fmt.Errorf("topology is required")
	}

	parsedPrefix, err := netip.ParsePrefix(strings.TrimSpace(topo.Transport.Pool))
	if err != nil {
		return nil, fmt.Errorf("parse transport pool %q: %w", topo.Transport.Pool, err)
	}
	if topo.Transport.PrefixLength <= 0 {
		return nil, fmt.Errorf("transport prefix length must be greater than zero")
	}

	alloc := transport.NewAllocator(parsedPrefix, topo.Transport.PrefixLength)
	statePath := filepath.Join(cleanConfigDir, "transport.yml")
	if err := alloc.LoadState(statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	return alloc, nil
}

func saveTransportState(alloc *transport.Allocator, configDir string) error {
	cleanConfigDir := strings.TrimSpace(configDir)
	if cleanConfigDir == "" {
		return fmt.Errorf("config directory is required")
	}
	if alloc == nil {
		return fmt.Errorf("allocator is required")
	}

	return alloc.SaveState(filepath.Join(cleanConfigDir, "transport.yml"))
}

func containsName(list []string, needle string) bool {
	for _, value := range list {
		if value == needle {
			return true
		}
	}
	return false
}

func renderDockerCompose(tmplContent string, data any, outputPath string) error {
	tmpl, err := template.New("compose").Parse(tmplContent)
	if err != nil {
		return fmt.Errorf("parse compose template: %w", err)
	}

	var output strings.Builder
	if err := tmpl.Execute(&output, data); err != nil {
		return fmt.Errorf("render compose template: %w", err)
	}

	if err := os.WriteFile(outputPath, []byte(output.String()), 0600); err != nil {
		return fmt.Errorf("write compose file: %w", err)
	}
	return nil
}

func nodeDir(configDir, name string) string {
	return filepath.Join(configDir, "nodes", name)
}

// composeEscapeDollar doubles every `$` so Docker Compose does not treat it
// as variable interpolation when parsing an environment value. Bcrypt hashes
// always start with `$2a$12$...`, and without this escape Compose expands
// `$2a` and `$12` to empty strings — the node then rejects the mangled value
// via bcrypt.Cost(...) and refuses to bootstrap `/config/mesh.token`.
// Apply only for docker-compose output. RouterOS `.rsc` does not interpolate
// `$`, so mikrotik deploy scripts carry the raw hash via quoteRouterOSValue.
func composeEscapeDollar(s string) string {
	return strings.ReplaceAll(s, "$", "$$")
}

// resolveMasterAddresses maps master names to "host:listen_port" strings so a
// client can dial each master on the port actually configured in topology —
// anti-DPI deployments commonly run masters on 443/udp, not the default 51820,
// and the old hard-coded port emitted a compose that dialled the wrong port.
// A missing master is a hard error: silently dropping it would produce a
// client that connects to the wrong subset of the mesh with no diagnostic.
func resolveMasterAddresses(topo *topology.Topology, masterNames []string) ([]string, error) {
	if topo == nil {
		return nil, fmt.Errorf("topology is required")
	}
	addrs := make([]string, 0, len(masterNames))
	for _, masterName := range masterNames {
		m := topo.FindMaster(masterName)
		if m == nil {
			return nil, fmt.Errorf("master %q not found in topology", masterName)
		}
		host := strings.TrimSpace(m.Host)
		if host == "" {
			return nil, fmt.Errorf("master %q has empty host", masterName)
		}
		port := m.ListenPort
		if port <= 0 || port > 65535 {
			return nil, fmt.Errorf("master %q has invalid listen_port %d", masterName, port)
		}
		addrs = append(addrs, fmt.Sprintf("%s:%d", host, port))
	}
	return addrs, nil
}

// printNextSteps writes a precise, actionable deploy sequence to stdout.
// The compose file already carries MESH_TOKEN_HASH, so the operator never has
// to ship the token file by hand — the binary bootstraps it on first boot.
// The plaintext token is still printed because `mesh-ctl <role> init` uses
// it as the pre-Init bearer credential.
//
// useTraefik selects the state directory to match the generated compose: the
// default (host-network) templates bind /var/lib/awg-mesh/<name>, the Traefik
// variants bind /srv/awg-mesh/<name>. Getting this wrong leaves the operator
// with an empty mount and a confused node, so it's routed through explicitly.
func printNextSteps(role, name, token, outputPath string, useTraefik bool) {
	stateDir := "/var/lib/awg-mesh/" + name
	if useTraefik {
		stateDir = "/srv/awg-mesh/" + name
	}
	fmt.Printf(`%s %q prepared.

Bearer token (keep locally — needed for '%s init'):
  %s

Docker Compose written to: %s

Next steps on the target host:
  1. scp %s <target-host>:/etc/docker/compose/%s-docker-compose.yml   # or wherever you keep compose files
  2. ssh <target-host> 'docker compose -f /etc/docker/compose/%s-docker-compose.yml up -d'
     (Docker creates %s automatically when the parent path exists.)

Then, back on this workstation:
  3. mesh-ctl %s init %s

Notes:
  - The compose file embeds the bcrypt token hash as MESH_TOKEN_HASH. The
    node bootstraps /config/mesh.token from that env var on first boot and
    ignores it afterwards. The hash remains visible via 'docker inspect' —
    treat the compose file itself as a secret and keep it off public hosts.
  - %s is the host-side bind-mount that becomes /config inside the
    container. All node state (keys, transport allocations, client-state)
    lives there.
`,
		titleCase(role), name,
		role, token,
		outputPath,
		outputPath, name,
		name,
		stateDir,
		role, name,
		stateDir,
	)
}

// titleCase upper-cases the first rune of s. Replaces the deprecated strings.Title
// for our narrow use-case (single-word ASCII role names: master/endpoint/client).
func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func caPath(configDir string) string {
	return filepath.Join(configDir, "ca.crt")
}
