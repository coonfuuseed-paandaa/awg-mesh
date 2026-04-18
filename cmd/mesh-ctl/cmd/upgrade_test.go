package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
)

// ─── patchImageLine ────────────────────────────────────────────────────────────

// TestPatchImageLine verifies that patchImageLine replaces the image: line of
// the awg-mesh-node service only, leaving unrelated services untouched.
//
// This locks in the fix from PR #53 (CodeRabbit review): scope replacement to
// the awg-mesh-node service block so that sidecar images are not corrupted.
func TestPatchImageLine(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		compose   string
		newImage  string
		wantImage string // substring that MUST appear in result
		wantAbsent string // substring that MUST NOT appear in result (empty = skip)
	}{
		{
			name: "single awg-mesh-node service: image replaced",
			compose: `services:
  awg-mesh-node:
    image: ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.5.1
    network_mode: host
`,
			newImage:  "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.10.2",
			wantImage: "image: ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.10.2",
		},
		{
			name: "no awg-mesh-node service: nothing changes",
			compose: `services:
  some-sidecar:
    image: redis:7
`,
			newImage:   "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.10.2",
			wantImage:  "image: redis:7",
			wantAbsent: "v1.10.2",
		},
		{
			name: "multiple services: awg-mesh-node image updated",
			compose: `services:
  prometheus:
    image: prom/prometheus:v2.45.0
  awg-mesh-node:
    image: ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.9.0
    network_mode: host
`,
			newImage:   "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.10.2",
			wantImage:  "image: ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.10.2",
			wantAbsent: "awg-mesh-node:v1.9.0",
		},
		{
			name: "prometheus image preserved alongside patched awg-mesh-node image",
			compose: `services:
  prometheus:
    image: prom/prometheus:v2.45.0
  awg-mesh-node:
    image: ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.9.0
`,
			newImage:  "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.10.2",
			wantImage: "prom/prometheus:v2.45.0",
		},
		{
			name: "indented image line: indentation preserved",
			compose: `services:
  awg-mesh-node:
    image: ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.0.0
`,
			newImage:  "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.10.2",
			wantImage: "    image: ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.10.2",
		},
		{
			name:      "empty input: returns empty",
			compose:   "",
			newImage:  "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.10.2",
			wantImage: "",
		},
		{
			name: "latest tag replaced by versioned tag",
			compose: `services:
  awg-mesh-node:
    image: ghcr.io/coonfuuseed-paandaa/awg-mesh-node:latest
`,
			newImage:   "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.10.2",
			wantImage:  "image: ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.10.2",
			wantAbsent: ":latest",
		},
		{
			name: "service after awg-mesh-node: its image line is not touched",
			compose: `services:
  awg-mesh-node:
    image: ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.0.0
    network_mode: host
  exporter:
    image: prom/node-exporter:v1.8.0
`,
			newImage:  "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.10.2",
			wantImage: "prom/node-exporter:v1.8.0",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := patchImageLine(tc.compose, tc.newImage)

			if tc.compose == "" && got != "" {
				t.Fatalf("patchImageLine(empty) = %q, want empty string", got)
			}
			if tc.wantImage != "" && !strings.Contains(got, tc.wantImage) {
				t.Errorf("patchImageLine result missing %q\nInput:\n%s\nResult:\n%s",
					tc.wantImage, tc.compose, got)
			}
			if tc.wantAbsent != "" && strings.Contains(got, tc.wantAbsent) {
				t.Errorf("patchImageLine result contains unexpected %q\nInput:\n%s\nResult:\n%s",
					tc.wantAbsent, tc.compose, got)
			}
		})
	}
}

// TestPatchImageLineScopeGuard is an explicit regression test for the PR #53
// fix: the old code replaced every `image:` line in the file; the fixed code
// scopes replacement to the awg-mesh-node service block only.
func TestPatchImageLineScopeGuard(t *testing.T) {
	t.Parallel()

	// A compose file with two services, each with an image: line.
	// Only the awg-mesh-node image must be changed.
	compose := `services:
  sidecar:
    image: redis:7.2
    restart: unless-stopped
  awg-mesh-node:
    image: ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.9.0
    network_mode: host
`
	got := patchImageLine(compose, "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.10.2")

	if !strings.Contains(got, "image: redis:7.2") {
		t.Errorf("sidecar image was corrupted; result:\n%s", got)
	}
	if !strings.Contains(got, "image: ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.10.2") {
		t.Errorf("awg-mesh-node image was not updated; result:\n%s", got)
	}
	if strings.Contains(got, "awg-mesh-node:v1.9.0") {
		t.Errorf("old awg-mesh-node image tag still present; result:\n%s", got)
	}
}

// ─── parseHostPort ─────────────────────────────────────────────────────────────

// TestParseHostPort verifies the host:port splitter used by buildSSHDeployer.
func TestParseHostPort(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		addr     string
		wantHost string
		wantPort string
		wantErr  bool
	}{
		{
			name:     "standard host:port",
			addr:     "10.0.0.1:22",
			wantHost: "10.0.0.1",
			wantPort: "22",
		},
		{
			name:     "hostname with port",
			addr:     "node.example.com:2222",
			wantHost: "node.example.com",
			wantPort: "2222",
		},
		{
			name:     "no colon: returns addr as host, empty port",
			addr:     "10.0.0.1",
			wantHost: "10.0.0.1",
			wantPort: "",
		},
		{
			name:     "IPv6-style last colon wins",
			addr:     "2001:db8::1:22",
			wantHost: "2001:db8::1",
			wantPort: "22",
		},
		{
			name:     "empty string: empty host and port",
			addr:     "",
			wantHost: "",
			wantPort: "",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotHost, gotPort, err := parseHostPort(tc.addr)
			if tc.wantErr && err == nil {
				t.Errorf("parseHostPort(%q): expected error, got nil", tc.addr)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("parseHostPort(%q): unexpected error: %v", tc.addr, err)
			}
			if gotHost != tc.wantHost {
				t.Errorf("parseHostPort(%q) host = %q, want %q", tc.addr, gotHost, tc.wantHost)
			}
			if gotPort != tc.wantPort {
				t.Errorf("parseHostPort(%q) port = %q, want %q", tc.addr, gotPort, tc.wantPort)
			}
		})
	}
}

// ─── execTemplate ──────────────────────────────────────────────────────────────

// TestExecTemplate verifies the text/template executor used in compose rendering.
func TestExecTemplate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		tmplContent string
		data        map[string]interface{}
		wantContain string
		wantErr     bool
	}{
		{
			name:        "simple substitution",
			tmplContent: "image: {{.Image}}",
			data:        map[string]interface{}{"Image": "ghcr.io/org/awg-mesh-node:v1.10.2"},
			wantContain: "image: ghcr.io/org/awg-mesh-node:v1.10.2",
		},
		{
			name: "multiple fields",
			tmplContent: `name: {{.Name}}
mode: {{.Mode}}
image: {{.Image}}`,
			data:        map[string]interface{}{"Name": "ep-01", "Mode": "endpoint", "Image": "img:v1"},
			wantContain: "name: ep-01",
		},
		{
			name:        "invalid template syntax returns error",
			tmplContent: "{{.Unclosed",
			data:        map[string]interface{}{},
			wantErr:     true,
		},
		{
			name:        "empty template produces empty string",
			tmplContent: "",
			data:        map[string]interface{}{},
			wantContain: "",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := execTemplate(tc.tmplContent, tc.data)
			if tc.wantErr {
				if err == nil {
					t.Errorf("execTemplate(%q): expected error, got nil (output: %q)", tc.tmplContent, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("execTemplate(%q): unexpected error: %v", tc.tmplContent, err)
			}
			if tc.tmplContent == "" && got != "" {
				t.Fatalf("execTemplate(empty) = %q, want empty string", got)
			}
			if tc.wantContain != "" && !strings.Contains(got, tc.wantContain) {
				t.Errorf("execTemplate output missing %q\nGot: %q", tc.wantContain, got)
			}
		})
	}
}

// ─── buildComposeData ──────────────────────────────────────────────────────────

// TestBuildComposeData verifies that buildComposeData sets expected keys for
// master and endpoint roles.
func TestBuildComposeData(t *testing.T) {
	t.Parallel()

	topo := &topology.Topology{
		Masters: []topology.MasterNode{
			{Name: "master-01", OverlayIP: "10.10.0.1", ListenPort: 51820},
		},
		Endpoints: []topology.EndpointNode{
			{Name: "ep-01", OverlayIP: "10.10.0.2", ListenPort: 51821},
		},
	}

	t.Run("master role: populates master fields", func(t *testing.T) {
		t.Parallel()
		data, err := buildComposeData(topo, "master-01", "master", "img:v1", "token123", "/cfg")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		checks := map[string]interface{}{
			"Image":      "img:v1",
			"Token":      "token123",
			"ConfigDir":  "/cfg",
			"Name":       "master-01",
			"Mode":       "master",
			"OverlayIP":  "10.10.0.1",
			"ListenPort": 51820,
		}
		for key, want := range checks {
			got, ok := data[key]
			if !ok {
				t.Errorf("key %q missing from data", key)
				continue
			}
			if got != want {
				t.Errorf("data[%q] = %v, want %v", key, got, want)
			}
		}
	})

	t.Run("endpoint role: populates endpoint fields", func(t *testing.T) {
		t.Parallel()
		data, err := buildComposeData(topo, "ep-01", "endpoint", "img:v2", "tok456", "/cfg2")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		checks := map[string]interface{}{
			"Image":      "img:v2",
			"Token":      "tok456",
			"ConfigDir":  "/cfg2",
			"Name":       "ep-01",
			"Mode":       "endpoint",
			"OverlayIP":  "10.10.0.2",
			"ListenPort": 51821,
		}
		for key, want := range checks {
			got, ok := data[key]
			if !ok {
				t.Errorf("key %q missing from data", key)
				continue
			}
			if got != want {
				t.Errorf("data[%q] = %v, want %v", key, got, want)
			}
		}
	})

	t.Run("master not found in topology: returns error", func(t *testing.T) {
		t.Parallel()
		_, err := buildComposeData(topo, "nonexistent-master", "master", "img:v1", "tok", "/cfg")
		if err == nil {
			t.Error("expected error for unknown master, got nil")
		}
		if !strings.Contains(err.Error(), "nonexistent-master") {
			t.Errorf("error should mention node name, got: %v", err)
		}
	})

	t.Run("endpoint not found in topology: returns error", func(t *testing.T) {
		t.Parallel()
		_, err := buildComposeData(topo, "nonexistent-ep", "endpoint", "img:v1", "tok", "/cfg")
		if err == nil {
			t.Error("expected error for unknown endpoint, got nil")
		}
		if !strings.Contains(err.Error(), "nonexistent-ep") {
			t.Errorf("error should mention node name, got: %v", err)
		}
	})

	t.Run("unknown role: no role-specific keys, no error", func(t *testing.T) {
		t.Parallel()
		data, err := buildComposeData(topo, "anything", "client", "img:v3", "tok789", "/cfg3")
		if err != nil {
			t.Fatalf("unexpected error for unknown role: %v", err)
		}
		if data["Image"] != "img:v3" {
			t.Errorf("Image field missing for unknown role")
		}
		if _, ok := data["Name"]; ok {
			t.Error("Name field should not be set for unknown role")
		}
	})
}

// ─── cobra wiring smoke tests ──────────────────────────────────────────────────

// TestUpgradeHelpFlag verifies that `mesh-ctl upgrade --help` exits cleanly and
// describes the command.
func TestUpgradeHelpFlag(t *testing.T) {
	root := NewRootCommand("test")
	root.SilenceErrors = true

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"upgrade", "--help"})

	// --help exits with nil (cobra treats it as success).
	_ = root.Execute()

	helpText := buf.String()
	if !strings.Contains(helpText, "upgrade") {
		t.Errorf("upgrade --help output missing 'upgrade'\nGot: %s", helpText)
	}
	if !strings.Contains(helpText, "version") {
		t.Errorf("upgrade --help output missing 'version'\nGot: %s", helpText)
	}
}

// TestUpgradeRequiresVersion verifies that `mesh-ctl upgrade` with no positional
// argument returns a non-nil error (cobra ExactArgs(1) enforcement).
func TestUpgradeRequiresVersion(t *testing.T) {
	root := NewRootCommand("test")
	root.SilenceUsage = true
	root.SilenceErrors = true

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"upgrade"})

	err := root.Execute()
	if err == nil {
		t.Error("expected error when version argument is omitted, got nil")
	}
}

// TestUpgradeComposeRequiresFile verifies that `mesh-ctl upgrade compose` with no
// file argument returns a non-nil error.
func TestUpgradeComposeRequiresFile(t *testing.T) {
	root := NewRootCommand("test")
	root.SilenceUsage = true
	root.SilenceErrors = true

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"upgrade", "compose"})

	err := root.Execute()
	if err == nil {
		t.Error("expected error when compose file argument is omitted, got nil")
	}
}

// TestUpgradeStatusHelpFlag verifies that `mesh-ctl upgrade status --help`
// exits cleanly.
func TestUpgradeStatusHelpFlag(t *testing.T) {
	root := NewRootCommand("test")
	root.SilenceErrors = true

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"upgrade", "status", "--help"})

	_ = root.Execute()

	helpText := buf.String()
	if !strings.Contains(helpText, "status") {
		t.Errorf("upgrade status --help output missing 'status'\nGot: %s", helpText)
	}
}

// TestUpgradeCommandFlags verifies that all documented flags are registered on
// the upgrade command.
func TestUpgradeCommandFlags(t *testing.T) {
	root := NewRootCommand("test")
	root.SilenceErrors = true

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"upgrade", "--help"})
	_ = root.Execute()

	helpText := buf.String()

	requiredFlags := []string{
		"--dry-run",
		"--ssh",
		"--ssh-user",
		"--ssh-port",
		"--ssh-key",
		"--accept-new-host-key",
		"--downtime-budget",
		"--deploy-wait",
		"--order",
	}
	for _, flag := range requiredFlags {
		if !strings.Contains(helpText, flag) {
			t.Errorf("upgrade --help missing flag %q\nFull output:\n%s", flag, helpText)
		}
	}
}

// TestUpgradeCommandRegistered verifies that `mesh-ctl upgrade` is registered
// and that no-args returns an args-validation error (not "unknown command").
func TestUpgradeCommandRegistered(t *testing.T) {
	root := NewRootCommand("test")
	root.SilenceUsage = true
	root.SilenceErrors = true
	root.SetArgs([]string{"upgrade"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for missing <version> argument, got nil")
	}
	if strings.Contains(err.Error(), "unknown command") {
		t.Errorf("got 'unknown command' — newUpgradeCommand not registered in root: %v", err)
	}
}
