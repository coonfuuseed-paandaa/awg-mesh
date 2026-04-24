package cmd

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"text/template"

	"github.com/coonfuuseed-paandaa/awg-mesh/proto"
)

// TestClientPrepareImage exercises the three precedence branches for image
// resolution in "mesh-ctl client prepare" (linux client type).
//
// Precedence: --image flag > topology defaults.image.client > built-in fallback.
//
// The test renders the real docker-compose.client.yml.tmpl template with a
// data struct whose Image field is produced by resolveImage — the same call
// path used in newClientPrepareCommand — and asserts that the rendered
// "image:" line reflects the expected image reference.
func TestClientPrepareImage(t *testing.T) {
	t.Parallel()

	const fallback = "ghcr.io/coonfuuseed-paandaa/awg-mesh-client:latest"

	cases := []struct {
		name      string
		cliFlag   string
		topoImage string
		want      string
	}{
		{
			name:      "cli-flag wins over topo and fallback",
			cliFlag:   "myregistry.io/awg-mesh-client:v2.0.0",
			topoImage: "topo-registry.io/awg-mesh-client:v1.5.0",
			want:      "myregistry.io/awg-mesh-client:v2.0.0",
		},
		{
			name:      "topo-default used when no cli-flag",
			cliFlag:   "",
			topoImage: "topo-registry.io/awg-mesh-client:v1.5.0",
			want:      "topo-registry.io/awg-mesh-client:v1.5.0",
		},
		{
			name:      "built-in fallback when neither cli-flag nor topo-default",
			cliFlag:   "",
			topoImage: "",
			want:      fallback,
		},
	}

	// Load the client compose template once — shared across subtests.
	tmplContent, err := templateFS.ReadFile("templates/docker-compose.client.yml.tmpl")
	if err != nil {
		t.Fatalf("read client template: %v", err)
	}
	tmpl, err := template.New("docker-compose.client.yml.tmpl").Parse(string(tmplContent))
	if err != nil {
		t.Fatalf("parse client template: %v", err)
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resolvedImage := resolveImage(tc.cliFlag, tc.topoImage, fallback, "defaults.image.client")

			data := struct {
				Name      string
				Host      string
				OverlayIP string
				Image     string
				TokenHash string
				Masters   string
			}{
				Name:      "client-01",
				Host:      "",
				OverlayIP: "10.0.0.100",
				Image:     resolvedImage,
				// Pre-escaped bcrypt hash to pass Docker Compose dollar-escape contract.
				TokenHash: "$$2a$$12$$abcdefghijklmnopqrstuv",
				Masters:   "master-01:51820",
			}

			var buf strings.Builder
			if err := tmpl.Execute(&buf, data); err != nil {
				t.Fatalf("execute template: %v", err)
			}
			rendered := buf.String()

			wantLine := "image: " + tc.want
			if !strings.Contains(rendered, wantLine) {
				t.Errorf("rendered compose does not contain %q\n--- rendered ---\n%s", wantLine, rendered)
			}

			// Guard: the wrong image reference must not appear when a specific
			// one is expected. This prevents the test from passing vacuously if
			// rendered output happens to contain another image reference.
			if tc.want != fallback && strings.Contains(rendered, "image: "+fallback) {
				t.Errorf("rendered compose contains fallback %q but expected %q\n--- rendered ---\n%s",
					fallback, tc.want, rendered)
			}
		})
	}
}

func TestMasterClientTunnelID(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		client    string
		wantExact string
		wantRegex string
	}{
		{name: "short safe name unchanged", client: "client-01", wantExact: "client-01"},
		{name: "12-char safe name unchanged", client: "client-12345", wantExact: "client-12345"},
		{name: "long name hashed", client: "mikrotik-home", wantRegex: `^cli-[0-9a-f]{8}$`},
		{name: "invalid chars hashed", client: "client.home", wantRegex: `^cli-[0-9a-f]{8}$`},
		{name: "blank fallback", client: "   ", wantExact: "cli-00000000"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := masterClientTunnelID(tc.client)
			if tc.wantExact != "" && got != tc.wantExact {
				t.Fatalf("masterClientTunnelID(%q) = %q, want %q", tc.client, got, tc.wantExact)
			}
			if tc.wantRegex != "" {
				re := regexp.MustCompile(tc.wantRegex)
				if !re.MatchString(got) {
					t.Fatalf("masterClientTunnelID(%q) = %q, want match %s", tc.client, got, tc.wantRegex)
				}
			}
		})
	}
}

func TestMasterClientTunnelID_Deterministic(t *testing.T) {
	t.Parallel()

	const clientName = "mikrotik-home"
	first := masterClientTunnelID(clientName)
	for i := 0; i < 10; i++ {
		if got := masterClientTunnelID(clientName); got != first {
			t.Fatalf("iteration %d: got %q, want %q", i, got, first)
		}
	}
}

func TestMasterClientPreferredTunnelID(t *testing.T) {
	t.Parallel()

	bounded := masterClientTunnelID("mikrotik-home")
	legacy := masterClientLegacyTunnelID("mikrotik-home")
	if bounded == legacy {
		t.Fatalf("test requires distinct bounded and legacy names, both were %q", bounded)
	}

	cases := []struct {
		name    string
		tunnels []*proto.TunnelStatus
		want    string
	}{
		{
			name:    "fresh install uses bounded id",
			tunnels: nil,
			want:    bounded,
		},
		{
			name:    "legacy tunnel is reused when present",
			tunnels: []*proto.TunnelStatus{{Name: legacy}},
			want:    legacy,
		},
		{
			name:    "bounded tunnel stays bounded when legacy absent",
			tunnels: []*proto.TunnelStatus{{Name: bounded}},
			want:    bounded,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := masterClientPreferredTunnelID("mikrotik-home", tc.tunnels); got != tc.want {
				t.Fatalf("masterClientPreferredTunnelID() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMasterClientRemovalTunnelIDs(t *testing.T) {
	t.Parallel()

	bounded := masterClientTunnelID("mikrotik-home")
	legacy := masterClientLegacyTunnelID("mikrotik-home")
	if bounded == legacy {
		t.Fatalf("test requires distinct bounded and legacy names, both were %q", bounded)
	}

	cases := []struct {
		name    string
		tunnels []*proto.TunnelStatus
		want    []string
	}{
		{
			name:    "no tunnel list falls back to both candidates",
			tunnels: nil,
			want:    []string{legacy, bounded},
		},
		{
			name:    "legacy and bounded are both removed when both exist",
			tunnels: []*proto.TunnelStatus{{Name: legacy}, {Name: bounded}},
			want:    []string{legacy, bounded},
		},
		{
			name:    "legacy-only install removes legacy only",
			tunnels: []*proto.TunnelStatus{{Name: legacy}},
			want:    []string{legacy},
		},
		{
			name:    "bounded-only install removes bounded only",
			tunnels: []*proto.TunnelStatus{{Name: bounded}},
			want:    []string{bounded},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := masterClientRemovalTunnelIDs("mikrotik-home", tc.tunnels)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("masterClientRemovalTunnelIDs() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestClientPrepareImageFlagValidation verifies that newClientPrepareCommand
// rejects an invalid --image value before performing any topology or CA work.
func TestClientPrepareImageFlagValidation(t *testing.T) {
	// Do not run in parallel: NewRootCommand binds cobra persistent flags to
	// package-level globals (topologyPath/configDir), and concurrent flag
	// registration causes a data race under -race.

	invalidRefs := []string{
		"img; rm -rf /",
		"img`touch /pwned`",
		"img$(id)",
		"img|sh",
	}

	for _, ref := range invalidRefs {
		ref := ref
		t.Run(ref, func(t *testing.T) {
			root := NewRootCommand("test")
			root.SilenceUsage = true
			root.SilenceErrors = true
			root.SetArgs([]string{"client", "prepare", "--image", ref, "client-01"})

			err := root.Execute()
			if err == nil {
				t.Errorf("client prepare --image %q: expected error for invalid image ref, got nil", ref)
				return
			}
			if !strings.Contains(err.Error(), "invalid --image") {
				t.Errorf("client prepare --image %q: expected 'invalid --image' in error, got: %v", ref, err)
			}
		})
	}
}

func TestClientInitMasterPubkeyReadsHexAdminFormat(t *testing.T) {
	t.Parallel()

	cfgDir := t.TempDir()
	masterName := "master-01"
	masterDir := nodeDir(cfgDir, masterName)
	if err := os.MkdirAll(masterDir, 0o755); err != nil {
		t.Fatalf("mkdir master dir: %v", err)
	}

	wantRaw := []byte{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
		0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
		0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
	}
	storedHex := hex.EncodeToString(wantRaw) + "\n"
	if err := os.WriteFile(filepath.Join(masterDir, "pubkey"), []byte(storedHex), 0o600); err != nil {
		t.Fatalf("write master pubkey: %v", err)
	}

	got, err := readAdminPubkeyRaw(cfgDir, masterName)
	if err != nil {
		t.Fatalf("readAdminPubkeyRaw = %v, want nil", err)
	}
	if len(got) != len(wantRaw) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(wantRaw))
	}
	if hex.EncodeToString(got) != hex.EncodeToString(wantRaw) {
		t.Fatalf("hex.EncodeToString(got) = %q, want %q", hex.EncodeToString(got), hex.EncodeToString(wantRaw))
	}
}
