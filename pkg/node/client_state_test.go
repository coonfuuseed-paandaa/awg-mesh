package node

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestClientStateSaveLoad(t *testing.T) {
	dir := t.TempDir()

	state := ClientState{
		OverlayIP:    "172.20.70.131",
		OverlaySpace: "172.20.70.0/24",
		RoutingPolicies: []RoutingPolicyState{
			{Name: "vpn-asia", DSCP: 10, Targets: []string{"node-asia-01"}},
			{Name: "vpn-us", DSCP: 20, Targets: []string{"node-us-01"}},
		},
		DNS: &DNSState{
			Zone:     "mesh.zone",
			Listen:   "0.0.0.0:53",
			Upstream: "1.1.1.1",
		},
		Masters: []NodeRef{
			{Name: "master-01", OverlayIP: "172.20.70.10"},
		},
		Endpoints: []NodeRef{
			{Name: "node-asia-01", OverlayIP: "172.20.70.34"},
		},
	}

	if err := saveClientState(dir, state); err != nil {
		t.Fatalf("saveClientState: %v", err)
	}

	// Verify file exists
	path := filepath.Join(dir, clientStateFile)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("client-state.yml not created")
	}

	// Load and verify
	loaded, err := loadClientState(dir)
	if err != nil {
		t.Fatalf("loadClientState: %v", err)
	}

	if loaded.OverlayIP != "172.20.70.131" {
		t.Errorf("expected overlay IP 172.20.70.131, got %s", loaded.OverlayIP)
	}
	if loaded.OverlaySpace != "172.20.70.0/24" {
		t.Errorf("expected overlay space 172.20.70.0/24, got %s", loaded.OverlaySpace)
	}
	if len(loaded.RoutingPolicies) != 2 {
		t.Fatalf("expected 2 routing policies, got %d", len(loaded.RoutingPolicies))
	}
	if loaded.RoutingPolicies[0].Name != "vpn-asia" {
		t.Errorf("expected first policy vpn-asia, got %s", loaded.RoutingPolicies[0].Name)
	}
	if loaded.DNS == nil {
		t.Fatal("expected DNS config")
	}
	if loaded.DNS.Zone != "mesh.zone" {
		t.Errorf("expected DNS zone mesh.zone, got %s", loaded.DNS.Zone)
	}
	if len(loaded.Masters) != 1 || loaded.Masters[0].Name != "master-01" {
		t.Errorf("expected master master-01")
	}
}

func TestClientStateLoadMissing(t *testing.T) {
	state, err := loadClientState(t.TempDir())
	if err != nil {
		t.Fatalf("expected no error for missing file, got: %v", err)
	}
	if state.OverlayIP != "" {
		t.Error("expected empty state for missing file")
	}
}

func TestClientStateSaveErrors(t *testing.T) {
	if err := saveClientState("", ClientState{}); err == nil {
		t.Error("expected error for empty config dir")
	}
}

func TestLoadClientStateCorruptSentinel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content []byte
	}{
		{
			name:    "truncated YAML",
			content: []byte("overlay_ip: 10.0.0.1\nrouting_policies: [unclosed"),
		},
		{
			name:    "non-YAML bytes",
			content: []byte{0xff, 0x00, 0xde, 0xad},
		},
		{
			name:    "unknown field",
			content: []byte("overlay_ip: 10.0.0.1\nunknown_field: bad\n"),
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			path := filepath.Join(dir, clientStateFile)
			if err := os.WriteFile(path, tt.content, 0o600); err != nil {
				t.Fatalf("WriteFile returned error: %v", err)
			}

			_, err := loadClientState(dir)
			if err == nil {
				t.Fatal("expected error for corrupt YAML, got nil")
			}
			if !errors.Is(err, ErrCorruptClientState) {
				t.Fatalf("expected errors.Is(err, ErrCorruptClientState)=true, got err=%v", err)
			}
		})
	}
}
