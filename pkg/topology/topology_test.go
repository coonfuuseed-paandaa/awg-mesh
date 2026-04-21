package topology

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadTopology(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	validPath := filepath.Join(tempDir, "topology.yml")
	invalidPath := filepath.Join(tempDir, "invalid.yml")

	validContent := strings.Join([]string{
		"overlay:",
		"  space: 10.0.0.0/16",
		"  physical_mtu: 1500",
		"  awg_overhead: 80",
		"  ranges:",
		"    - name: core",
		"      cidr: 10.0.1.0/24",
		"masters:",
		"  - name: m1",
		"    host: m1.example",
		"    overlay_ip: 10.0.1.10",
		"    listen_port: 51820",
		"    endpoints: [e1]",
		"endpoints:",
		"  - name: e1",
		"    host: e1.example",
		"    overlay_ip: 10.0.1.20",
		"    listen_port: 51820",
		"    region: europe",
		"clients:",
		"  - name: c1",
		"    type: desktop",
		"    overlay_ip: 10.0.1.30",
		"    masters: [m1]",
	}, "\n")

	if err := os.WriteFile(validPath, []byte(validContent), 0o600); err != nil {
		t.Fatalf("WriteFile valid topology returned error: %v", err)
	}
	if err := os.WriteFile(invalidPath, []byte("overlay: ["), 0o600); err != nil {
		t.Fatalf("WriteFile invalid topology returned error: %v", err)
	}

	tests := []struct {
		name        string
		path        string
		expectError string
	}{
		{name: "empty path", path: "", expectError: "topology path is required"},
		{name: "missing file", path: filepath.Join(tempDir, "missing.yml"), expectError: "read topology file"},
		{name: "invalid yaml", path: invalidPath, expectError: "unmarshal topology yaml"},
		{name: "success", path: validPath, expectError: ""},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			top, err := LoadTopology(tt.path)
			if tt.expectError == "" {
				if err != nil {
					t.Fatalf("LoadTopology returned error: %v", err)
				}
				if top.Overlay.Space != "10.0.0.0/16" {
					t.Fatalf("unexpected overlay space: %s", top.Overlay.Space)
				}
				if len(top.Masters) != 1 || top.Masters[0].Name != "m1" {
					t.Fatalf("unexpected masters: %#v", top.Masters)
				}
				return
			}

			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.expectError) {
				t.Fatalf("expected error containing %q, got %v", tt.expectError, err)
			}
		})
	}
}

func TestFindNodeHelpers(t *testing.T) {
	t.Parallel()

	top := &Topology{
		Masters:   []MasterNode{{Name: "m1"}},
		Endpoints: []EndpointNode{{Name: "e1"}},
		Clients:   []ClientNode{{Name: "c1"}},
	}

	if got := top.FindMaster("m1"); got == nil || got.Name != "m1" {
		t.Fatalf("FindMaster failed: %#v", got)
	}
	if got := top.FindMaster("missing"); got != nil {
		t.Fatalf("FindMaster expected nil, got %#v", got)
	}

	if got := top.FindEndpoint("e1"); got == nil || got.Name != "e1" {
		t.Fatalf("FindEndpoint failed: %#v", got)
	}
	if got := top.FindEndpoint("missing"); got != nil {
		t.Fatalf("FindEndpoint expected nil, got %#v", got)
	}

	if got := top.FindClient("c1"); got == nil || got.Name != "c1" {
		t.Fatalf("FindClient failed: %#v", got)
	}
	if got := top.FindClient("missing"); got != nil {
		t.Fatalf("FindClient expected nil, got %#v", got)
	}
}

func TestSaveTopologyRoundTrip(t *testing.T) {
	t.Parallel()

	top := &Topology{
		Overlay: OverlayConfig{
			Space:       "10.10.0.0/16",
			PhysicalMTU: 1500,
			AWGOverhead: 80,
			Ranges:      []NamedRange{{Name: "core", CIDR: "10.10.1.0/24", BalancerIP: "10.10.1.1"}},
		},
		Masters:   []MasterNode{{Name: "m1", Host: "m1.local", OverlayIP: "10.10.1.10", ListenPort: 51820, Endpoints: []string{"e1"}}},
		Endpoints: []EndpointNode{{Name: "e1", Host: "e1.local", OverlayIP: "10.10.1.20", ListenPort: 51820, Region: "us"}},
		Clients:   []ClientNode{{Name: "c1", Type: "desktop", OverlayIP: "10.10.1.30", Masters: []string{"m1"}}},
	}

	path := filepath.Join(t.TempDir(), "topology.yml")
	if err := SaveTopology(path, top); err != nil {
		t.Fatalf("SaveTopology returned error: %v", err)
	}

	loaded, err := LoadTopology(path)
	if err != nil {
		t.Fatalf("LoadTopology returned error: %v", err)
	}

	if loaded.Overlay.Space != top.Overlay.Space {
		t.Fatalf("overlay space mismatch: got %s want %s", loaded.Overlay.Space, top.Overlay.Space)
	}
	if len(loaded.Masters) != 1 || loaded.Masters[0].Name != "m1" {
		t.Fatalf("unexpected masters after round-trip: %#v", loaded.Masters)
	}
	if len(loaded.Endpoints) != 1 || loaded.Endpoints[0].Name != "e1" {
		t.Fatalf("unexpected endpoints after round-trip: %#v", loaded.Endpoints)
	}
}

func TestSaveTopologyErrors(t *testing.T) {
	t.Parallel()

	err := SaveTopology("", &Topology{})
	if err == nil || !strings.Contains(err.Error(), "topology path is required") {
		t.Fatalf("expected path-required error, got %v", err)
	}

	err = SaveTopology(filepath.Join(t.TempDir(), "topology.yml"), nil)
	if err == nil || !strings.Contains(err.Error(), "topology value is required") {
		t.Fatalf("expected topology-required error, got %v", err)
	}
}

func TestMastersForEndpoint(t *testing.T) {
	t.Parallel()

	top := &Topology{
		Masters: []MasterNode{
			{Name: "zeta", Endpoints: []string{"ep-a", "ep-b"}},
			{Name: "alpha", Endpoints: []string{"ep-a"}},
			{Name: "beta", Endpoints: []string{"ep-b"}},
		},
		Endpoints: []EndpointNode{
			{Name: "ep-a"},
			{Name: "ep-b"},
		},
	}

	t.Run("two masters bind one endpoint — returned sorted", func(t *testing.T) {
		t.Parallel()

		got := top.MastersForEndpoint("ep-a")
		if len(got) != 2 {
			t.Fatalf("expected 2 masters, got %d: %#v", len(got), got)
		}
		if got[0].Name != "alpha" || got[1].Name != "zeta" {
			t.Fatalf("expected [alpha, zeta], got [%s, %s]", got[0].Name, got[1].Name)
		}
	})

	t.Run("two masters bind endpoint — two results", func(t *testing.T) {
		t.Parallel()

		got := top.MastersForEndpoint("ep-b")
		if len(got) != 2 {
			t.Fatalf("expected 2 masters, got %d: %#v", len(got), got)
		}
		if got[0].Name != "beta" || got[1].Name != "zeta" {
			t.Fatalf("expected [beta, zeta], got [%s, %s]", got[0].Name, got[1].Name)
		}
	})

	t.Run("no master binds endpoint — empty slice not nil", func(t *testing.T) {
		t.Parallel()

		got := top.MastersForEndpoint("ep-unknown")
		if got == nil {
			t.Fatal("expected empty slice, got nil")
		}
		if len(got) != 0 {
			t.Fatalf("expected empty slice, got %d elements: %#v", len(got), got)
		}
	})

	t.Run("invalid endpoint name — empty slice not nil", func(t *testing.T) {
		t.Parallel()

		got := top.MastersForEndpoint("")
		if got == nil {
			t.Fatal("expected empty slice, got nil")
		}
		if len(got) != 0 {
			t.Fatalf("expected empty slice, got %d elements: %#v", len(got), got)
		}
	})

	t.Run("topology with no masters — empty slice not nil", func(t *testing.T) {
		t.Parallel()

		emptyTop := &Topology{}
		got := emptyTop.MastersForEndpoint("ep-a")
		if got == nil {
			t.Fatal("expected empty slice, got nil")
		}
		if len(got) != 0 {
			t.Fatalf("expected empty slice, got %d elements: %#v", len(got), got)
		}
	})

	t.Run("result is a copy — mutations do not affect topology", func(t *testing.T) {
		t.Parallel()

		got := top.MastersForEndpoint("ep-a")
		if len(got) == 0 {
			t.Fatal("expected non-empty result")
		}
		got[0].Name = "mutated"
		// Verify the topology is unchanged.
		got2 := top.MastersForEndpoint("ep-a")
		for _, m := range got2 {
			if m.Name == "mutated" {
				t.Fatal("MastersForEndpoint result shares memory with topology — must return a copy")
			}
		}
	})
}

// TestImageDefaultsUnmarshal verifies that the optional defaults.image fields
// round-trip through YAML correctly and that existing topology files without
// the field still parse with a zero-value ImageDefaults.
func TestImageDefaultsUnmarshal(t *testing.T) {
	t.Parallel()

	t.Run("existing topology without defaults.image has zero-value ImageDefaults", func(t *testing.T) {
		t.Parallel()

		yaml := strings.Join([]string{
			"overlay:",
			"  space: 10.0.0.0/16",
			"masters:",
			"  - name: m1",
			"    host: m1.example",
			"    overlay_ip: 10.0.1.10",
			"    listen_port: 51820",
			"    endpoints: [e1]",
		}, "\n")

		path := filepath.Join(t.TempDir(), "topology.yml")
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		top, err := LoadTopology(path)
		if err != nil {
			t.Fatalf("LoadTopology returned error: %v", err)
		}
		if top.Defaults.Image != (ImageDefaults{}) {
			t.Fatalf("expected zero-value ImageDefaults, got %#v", top.Defaults.Image)
		}
	})

	t.Run("topology with defaults.image parses node and client", func(t *testing.T) {
		t.Parallel()

		yaml := strings.Join([]string{
			"defaults:",
			"  image:",
			"    node: foo",
			"    client: bar",
			"overlay:",
			"  space: 10.0.0.0/16",
		}, "\n")

		path := filepath.Join(t.TempDir(), "topology.yml")
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		top, err := LoadTopology(path)
		if err != nil {
			t.Fatalf("LoadTopology returned error: %v", err)
		}
		if top.Defaults.Image.Node != "foo" {
			t.Fatalf("expected Image.Node == %q, got %q", "foo", top.Defaults.Image.Node)
		}
		if top.Defaults.Image.Client != "bar" {
			t.Fatalf("expected Image.Client == %q, got %q", "bar", top.Defaults.Image.Client)
		}
	})
}
