package transport

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
)

func TestIsLegacySchema_ZeroVersion(t *testing.T) {
	t.Parallel()

	state := NodeTransportState{SchemaVersion: 0}
	if !IsLegacySchema(state) {
		t.Fatal("expected IsLegacySchema to return true for SchemaVersion=0")
	}
}

func TestIsLegacySchema_CurrentVersion(t *testing.T) {
	t.Parallel()

	state := NodeTransportState{SchemaVersion: CurrentSchemaVersion}
	if IsLegacySchema(state) {
		t.Fatalf("expected IsLegacySchema to return false for SchemaVersion=%d", CurrentSchemaVersion)
	}
}

func TestApplyLegacyDefaults_EmptyAllowedIPs(t *testing.T) {
	t.Parallel()

	state := NodeTransportState{
		Tunnels: []TunnelTransport{
			{Name: "t1", AllowedIPs: nil},
		},
	}
	ApplyLegacyDefaults(&state, zerolog.Nop())

	if len(state.Tunnels[0].AllowedIPs) != 1 || state.Tunnels[0].AllowedIPs[0] != "0.0.0.0/0" {
		t.Fatalf("expected AllowedIPs=[\"0.0.0.0/0\"], got %v", state.Tunnels[0].AllowedIPs)
	}
}

func TestApplyLegacyDefaults_ZeroKeepalive(t *testing.T) {
	t.Parallel()

	state := NodeTransportState{
		Tunnels: []TunnelTransport{
			{Name: "t1", PersistentKeepalive: 0},
		},
	}
	ApplyLegacyDefaults(&state, zerolog.Nop())

	if state.Tunnels[0].PersistentKeepalive != 25 {
		t.Fatalf("expected PersistentKeepalive=25, got %d", state.Tunnels[0].PersistentKeepalive)
	}
}

func TestApplyLegacyDefaults_StampsSchemaVersion(t *testing.T) {
	t.Parallel()

	state := NodeTransportState{SchemaVersion: 0}
	ApplyLegacyDefaults(&state, zerolog.Nop())

	if state.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("expected SchemaVersion=%d after ApplyLegacyDefaults, got %d", CurrentSchemaVersion, state.SchemaVersion)
	}
}

func TestApplyLegacyDefaults_PreservesPopulated(t *testing.T) {
	t.Parallel()

	state := NodeTransportState{
		Tunnels: []TunnelTransport{
			{
				Name:                "t1",
				AllowedIPs:          []string{"10.0.0.0/8"},
				PersistentKeepalive: 60,
			},
		},
	}
	ApplyLegacyDefaults(&state, zerolog.Nop())

	got := state.Tunnels[0]
	if len(got.AllowedIPs) != 1 || got.AllowedIPs[0] != "10.0.0.0/8" {
		t.Fatalf("expected AllowedIPs preserved as [\"10.0.0.0/8\"], got %v", got.AllowedIPs)
	}
	if got.PersistentKeepalive != 60 {
		t.Fatalf("expected PersistentKeepalive preserved as 60, got %d", got.PersistentKeepalive)
	}
}

func TestLoadNodeTransportStateCorruptSentinel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content []byte
	}{
		{
			name:    "truncated YAML",
			content: []byte("overlay_ip: 10.0.0.1\ntunnels: [unclosed"),
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
			path := filepath.Join(dir, "transport.yml")
			if err := os.WriteFile(path, tt.content, 0o600); err != nil {
				t.Fatalf("WriteFile returned error: %v", err)
			}

			_, err := LoadNodeTransportState(dir)
			if err == nil {
				t.Fatal("expected error for corrupt YAML, got nil")
			}
			if !errors.Is(err, ErrCorruptTransportState) {
				t.Fatalf("expected errors.Is(err, ErrCorruptTransportState)=true, got err=%v", err)
			}
		})
	}
}
