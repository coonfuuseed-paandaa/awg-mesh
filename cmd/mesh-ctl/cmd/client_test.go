package cmd

import (
	"strings"
	"testing"
	"text/template"
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

			resolvedImage := resolveImage(tc.cliFlag, tc.topoImage, fallback)

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
