package cmd

import (
	"strings"
	"testing"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
)

const masterFallback = "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:latest"

// TestMasterPrepareImage verifies the three-level image resolution that
// newMasterPrepareCommand applies when building data.Image:
//
//  1. CLI --image flag wins when non-empty.
//  2. Topology defaults.image.node wins when CLI flag is empty.
//  3. Built-in fallback is used when neither source is set.
//
// Anti-stub guarantee: if resolveImage is replaced with `return ""`, cases
// 1 and 2 MUST fail. Case 3 would also fail because the fallback is non-empty.
// (If the fallback itself were "", case 3 would pass incidentally — but the
// production fallback is always a non-empty string, so all three cases fail.)
func TestMasterPrepareImage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		imageFlag string
		topoImage string // topology Defaults.Image.Node
		want      string
	}{
		{
			name:      "cli-flag overrides topology and fallback",
			imageFlag: "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.9.0",
			topoImage: "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.8.0",
			want:      "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.9.0",
		},
		{
			name:      "topology-default used when no cli flag",
			imageFlag: "",
			topoImage: "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.8.0",
			want:      "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.8.0",
		},
		{
			name:      "baseline fallback used when neither flag nor topology set",
			imageFlag: "",
			topoImage: "",
			want:      masterFallback,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Build a topology that mirrors the state master.go reads.
			// Only Defaults.Image.Node matters for this resolution path.
			topo := topology.Topology{
				Defaults: topology.Defaults{
					Image: topology.ImageDefaults{
						Node: tc.topoImage,
					},
				},
			}

			// Replicate the exact resolveImage call from newMasterPrepareCommand.
			got := resolveImage(tc.imageFlag, topo.Defaults.Image.Node, masterFallback)
			if got != tc.want {
				t.Errorf("resolveImage(flag=%q, topo=%q, fallback=%q) = %q, want %q",
					tc.imageFlag, tc.topoImage, masterFallback, got, tc.want)
			}
		})
	}
}

// TestMasterPrepareImageFlagValidation verifies that newMasterPrepareCommand
// rejects an invalid --image value before performing any topology or CA work.
// This exercises the validateImageRef gate added to the prepare command's RunE.
func TestMasterPrepareImageFlagValidation(t *testing.T) {
	t.Parallel()

	invalidRefs := []string{
		"img; rm -rf /",
		"img`touch /pwned`",
		"img$(id)",
		"img|sh",
	}

	for _, ref := range invalidRefs {
		ref := ref
		t.Run(ref, func(t *testing.T) {
			t.Parallel()

			root := NewRootCommand("test")
			root.SilenceUsage = true
			root.SilenceErrors = true
			root.SetArgs([]string{"master", "prepare", "--image", ref, "master-01"})

			err := root.Execute()
			if err == nil {
				t.Errorf("master prepare --image %q: expected error for invalid image ref, got nil", ref)
				return
			}
			if !strings.Contains(err.Error(), "invalid --image") {
				t.Errorf("master prepare --image %q: expected 'invalid --image' in error, got: %v", ref, err)
			}
		})
	}
}
