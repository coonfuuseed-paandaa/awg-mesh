package cmd

import (
	"strings"
	"testing"
	"text/template"

	"gopkg.in/yaml.v3"
)

// These contract tests pin the structural invariants that a 2026-04-17 production
// deployment exposed as latent defects in the compose templates: sysctls blocks
// clashing with host networking, missing /dev/net/tun bind, plaintext tokens
// landing on nodes, and env-var config the binary never read. Every regression
// against any of these rules should fail the build, not production.

type templateFixture struct {
	Name     string
	fileName string
	data     any
}

var allTemplates = []templateFixture{
	{Name: "master", fileName: "docker-compose.master.yml.tmpl", data: masterData()},
	{Name: "endpoint", fileName: "docker-compose.endpoint.yml.tmpl", data: endpointData()},
	{Name: "client", fileName: "docker-compose.client.yml.tmpl", data: clientData()},
	{Name: "master-traefik", fileName: "docker-compose.master.traefik.yml.tmpl", data: masterData()},
	{Name: "endpoint-traefik", fileName: "docker-compose.endpoint.traefik.yml.tmpl", data: endpointData()},
	{Name: "client-traefik", fileName: "docker-compose.client.traefik.yml.tmpl", data: clientData()},
}

var hostNetworkTemplates = []string{
	"docker-compose.master.yml.tmpl",
	"docker-compose.endpoint.yml.tmpl",
	"docker-compose.client.yml.tmpl",
}

func masterData() any {
	return struct {
		Name       string
		Host       string
		OverlayIP  string
		Image      string
		ListenPort int
		TokenHash  string
	}{
		Name:       "master-01",
		Host:       "192.0.2.10",
		OverlayIP:  "10.0.0.1",
		Image:      "ghcr.io/example/awg-mesh-node:latest",
		ListenPort: 443,
		TokenHash:  "$2a$12$abcdefghijklmnopqrstuv",
	}
}

func endpointData() any {
	return struct {
		Name       string
		Host       string
		OverlayIP  string
		Image      string
		ListenPort int
		TokenHash  string
	}{
		Name:       "endpoint-01",
		Host:       "192.0.2.20",
		OverlayIP:  "10.0.0.2",
		Image:      "ghcr.io/example/awg-mesh-node:latest",
		ListenPort: 51820,
		TokenHash:  "$2a$12$abcdefghijklmnopqrstuv",
	}
}

func clientData() any {
	return struct {
		Name       string
		Host       string
		OverlayIP  string
		Image      string
		ListenPort int
		TokenHash  string
		Masters    string
	}{
		Name:       "client-01",
		Host:       "",
		OverlayIP:  "10.0.0.100",
		Image:      "ghcr.io/example/awg-mesh-client:latest",
		ListenPort: 51820,
		TokenHash:  "$2a$12$abcdefghijklmnopqrstuv",
		Masters:    "master-01,master-02",
	}
}

func renderTemplate(t *testing.T, fixture templateFixture) string {
	t.Helper()
	content, err := templateFS.ReadFile("templates/" + fixture.fileName)
	if err != nil {
		t.Fatalf("read template %q: %v", fixture.fileName, err)
	}
	tmpl, err := template.New(fixture.fileName).Parse(string(content))
	if err != nil {
		t.Fatalf("parse template %q: %v", fixture.fileName, err)
	}
	var buf strings.Builder
	if err := tmpl.Execute(&buf, fixture.data); err != nil {
		t.Fatalf("execute template %q: %v", fixture.fileName, err)
	}
	return buf.String()
}

// TestTemplatesAreValidYAML guards against syntax errors in any generated compose.
func TestTemplatesAreValidYAML(t *testing.T) {
	t.Parallel()
	for _, tt := range allTemplates {
		tt := tt
		t.Run(tt.Name, func(t *testing.T) {
			t.Parallel()
			rendered := renderTemplate(t, tt)
			var parsed map[string]any
			if err := yaml.Unmarshal([]byte(rendered), &parsed); err != nil {
				t.Fatalf("rendered template is not valid YAML: %v\n---\n%s", err, rendered)
			}
			if _, ok := parsed["services"]; !ok {
				t.Fatalf("rendered template has no `services` root:\n%s", rendered)
			}
		})
	}
}

// TestHostNetworkTemplatesDoNotSetSysctls mirrors Bug 1 from the field report:
// runc rejects container-scoped sysctls when the container shares the host
// network namespace. Non-host templates (traefik variants) are allowed and
// explicitly need ip_forward enabled.
func TestHostNetworkTemplatesDoNotSetSysctls(t *testing.T) {
	t.Parallel()
	for _, tt := range allTemplates {
		tt := tt
		if !isHostNetwork(tt.fileName) {
			continue
		}
		t.Run(tt.Name, func(t *testing.T) {
			t.Parallel()
			rendered := renderTemplate(t, tt)
			if strings.Contains(rendered, "sysctls:") {
				t.Fatalf("host-network template must not set container sysctls (runc rejects them):\n%s", rendered)
			}
		})
	}
}

// TestAllTemplatesMountTunDevice mirrors Bug 3 from the field report: without
// /dev/net/tun the binary fails at CreateTUN(). NET_ADMIN alone is not enough.
func TestAllTemplatesMountTunDevice(t *testing.T) {
	t.Parallel()
	for _, tt := range allTemplates {
		tt := tt
		t.Run(tt.Name, func(t *testing.T) {
			t.Parallel()
			rendered := renderTemplate(t, tt)
			if !strings.Contains(rendered, "/dev/net/tun:/dev/net/tun") {
				t.Fatalf("template must mount /dev/net/tun:\n%s", rendered)
			}
		})
	}
}

// TestAllTemplatesCarryMeshTokenHash mirrors Bug 4: the bootstrap path in the
// binary reads MESH_TOKEN_HASH. Templates must use that exact env name and
// must never carry plaintext tokens (MESH_TOKEN without _HASH suffix).
func TestAllTemplatesCarryMeshTokenHash(t *testing.T) {
	t.Parallel()
	for _, tt := range allTemplates {
		tt := tt
		t.Run(tt.Name, func(t *testing.T) {
			t.Parallel()
			rendered := renderTemplate(t, tt)
			if !strings.Contains(rendered, "MESH_TOKEN_HASH=") {
				t.Fatalf("template must embed MESH_TOKEN_HASH env var:\n%s", rendered)
			}
			// Plaintext MESH_TOKEN= (no _HASH) is a regression — tokens must
			// never land on the node filesystem, only bcrypt hashes.
			lines := strings.Split(rendered, "\n")
			for _, line := range lines {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "- MESH_TOKEN=") {
					t.Fatalf("plaintext MESH_TOKEN= leaked into template:\n%s", rendered)
				}
			}
		})
	}
}

// TestAllTemplatesCarryMeshName mirrors Bug 2: bare templates set MESH_MODE
// but forgot MESH_NAME, so the binary crashed with 'node name is required'.
func TestAllTemplatesCarryMeshName(t *testing.T) {
	t.Parallel()
	for _, tt := range allTemplates {
		tt := tt
		t.Run(tt.Name, func(t *testing.T) {
			t.Parallel()
			rendered := renderTemplate(t, tt)
			if !strings.Contains(rendered, "MESH_NAME=") {
				t.Fatalf("template must embed MESH_NAME env var:\n%s", rendered)
			}
		})
	}
}

// TestAllTemplatesMountConfigDir guards the volume mapping: the binary reads
// /config/mesh.token by default, so the host bind-mount has to land there.
func TestAllTemplatesMountConfigDir(t *testing.T) {
	t.Parallel()
	for _, tt := range allTemplates {
		tt := tt
		t.Run(tt.Name, func(t *testing.T) {
			t.Parallel()
			rendered := renderTemplate(t, tt)
			if !strings.Contains(rendered, ":/config") {
				t.Fatalf("template must bind a host directory to /config:\n%s", rendered)
			}
		})
	}
}

func isHostNetwork(fileName string) bool {
	for _, n := range hostNetworkTemplates {
		if n == fileName {
			return true
		}
	}
	return false
}
