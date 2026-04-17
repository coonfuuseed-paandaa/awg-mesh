package cmd

import (
	"strings"
	"testing"
	"text/template"
)

// TestEndpointPrepareImage asserts that the endpoint prepare path resolves the
// docker image reference through the three-level priority order defined by
// resolveImage: CLI flag > topology default > built-in fallback.
//
// Each case builds the same template data struct that newEndpointPrepareCommand
// builds at runtime, substituting only the Image field with the value that
// resolveImage would return, then renders the endpoint compose template and
// confirms the expected image line appears in the output.
func TestEndpointPrepareImage(t *testing.T) {
	t.Parallel()

	const fallback = "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:latest"

	cases := []struct {
		name      string
		cliFlag   string
		topoNode  string
		wantImage string
	}{
		{
			name:      "cli-flag wins",
			cliFlag:   "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.8.1",
			topoNode:  "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.7.0",
			wantImage: "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.8.1",
		},
		{
			name:      "topology-default used when no cli-flag",
			cliFlag:   "",
			topoNode:  "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.7.0",
			wantImage: "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.7.0",
		},
		{
			name:      "fallback used when neither cli-flag nor topology set",
			cliFlag:   "",
			topoNode:  "",
			wantImage: fallback,
		},
	}

	// Load the endpoint compose template once; all sub-tests share it.
	content, err := templateFS.ReadFile("templates/docker-compose.endpoint.yml.tmpl")
	if err != nil {
		t.Fatalf("read endpoint template: %v", err)
	}
	tmpl, err := template.New("docker-compose.endpoint.yml.tmpl").Parse(string(content))
	if err != nil {
		t.Fatalf("parse endpoint template: %v", err)
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			image := resolveImage(tc.cliFlag, tc.topoNode, fallback)
			if image != tc.wantImage {
				t.Fatalf("resolveImage(%q, %q, fallback) = %q, want %q",
					tc.cliFlag, tc.topoNode, image, tc.wantImage)
			}

			data := struct {
				Name       string
				Host       string
				OverlayIP  string
				Image      string
				ListenPort int
				TokenHash  string
			}{
				Name:       "ep-01",
				Host:       "192.0.2.20",
				OverlayIP:  "10.0.0.2",
				Image:      image,
				ListenPort: 51820,
				TokenHash:  "$$2a$$12$$testonly",
			}

			var buf strings.Builder
			if err := tmpl.Execute(&buf, data); err != nil {
				t.Fatalf("execute template: %v", err)
			}
			rendered := buf.String()

			wantLine := "image: " + tc.wantImage
			if !strings.Contains(rendered, wantLine) {
				t.Errorf("rendered compose does not contain %q\n---\n%s", wantLine, rendered)
			}
		})
	}
}
