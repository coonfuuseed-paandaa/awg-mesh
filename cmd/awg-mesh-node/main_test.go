package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"golang.org/x/crypto/bcrypt"
)

func TestEnvFallbackString(t *testing.T) {
	tests := []struct {
		name     string
		setFlags map[string]bool
		flagVal  string
		envKey   string
		envVal   string
		want     string
	}{
		{
			name:     "flag explicitly set beats env var",
			setFlags: map[string]bool{"mode": true},
			flagVal:  "client",
			envKey:   "TEST_MODE_EXPLICIT",
			envVal:   "master",
			want:     "client",
		},
		{
			name:     "flag default + env var present → env wins",
			setFlags: map[string]bool{},
			flagVal:  "master",
			envKey:   "TEST_MODE_ENV",
			envVal:   "endpoint",
			want:     "endpoint",
		},
		{
			name:     "flag default + no env → flag default used",
			setFlags: map[string]bool{},
			flagVal:  "master",
			envKey:   "TEST_MODE_NONE",
			envVal:   "",
			want:     "master",
		},
		{
			name:     "flag default + whitespace env → treated as empty",
			setFlags: map[string]bool{},
			flagVal:  "master",
			envKey:   "TEST_MODE_WS",
			envVal:   "   ",
			want:     "master",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			if tt.envVal != "" {
				t.Setenv(tt.envKey, tt.envVal)
			}
			got := envFallbackString(tt.setFlags, "mode", tt.flagVal, tt.envKey)
			if got != tt.want {
				t.Fatalf("envFallbackString: got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEnvFallbackInt(t *testing.T) {
	t.Run("explicit flag beats env", func(t *testing.T) {
		t.Setenv("TEST_PORT_A", "8080")
		got := envFallbackInt(map[string]bool{"listen-port": true}, "listen-port", 51820, "TEST_PORT_A")
		if got != 51820 {
			t.Fatalf("expected flag value 51820, got %d", got)
		}
	})

	t.Run("env overrides flag default", func(t *testing.T) {
		t.Setenv("TEST_PORT_B", "443")
		got := envFallbackInt(map[string]bool{}, "listen-port", 51820, "TEST_PORT_B")
		if got != 443 {
			t.Fatalf("expected env value 443, got %d", got)
		}
	})

	t.Run("invalid env falls back to flag default", func(t *testing.T) {
		t.Setenv("TEST_PORT_C", "not-an-int")
		got := envFallbackInt(map[string]bool{}, "listen-port", 51820, "TEST_PORT_C")
		if got != 51820 {
			t.Fatalf("expected flag default 51820, got %d", got)
		}
	})

	t.Run("no env → flag default", func(t *testing.T) {
		got := envFallbackInt(map[string]bool{}, "listen-port", 51820, "TEST_PORT_D_UNSET")
		if got != 51820 {
			t.Fatalf("expected flag default 51820, got %d", got)
		}
	})
}

func TestBootstrapTokenHash(t *testing.T) {
	validHash, err := bcrypt.GenerateFromPassword([]byte("test-token"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("generate test hash: %v", err)
	}

	t.Run("writes hash when token file missing", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("MESH_TOKEN_HASH", string(validHash))

		if err := bootstrapTokenHash(dir, zerolog.Nop()); err != nil {
			t.Fatalf("bootstrapTokenHash returned error: %v", err)
		}

		data, err := os.ReadFile(filepath.Join(dir, "mesh.token"))
		if err != nil {
			t.Fatalf("read token file: %v", err)
		}
		if string(data) != string(validHash) {
			t.Fatalf("token file contents mismatch:\n got  %q\n want %q", data, validHash)
		}
	})

	t.Run("preserves existing token file", func(t *testing.T) {
		dir := t.TempDir()
		tokenPath := filepath.Join(dir, "mesh.token")
		existing := []byte("existing-hash-on-disk")
		if err := os.WriteFile(tokenPath, existing, 0o600); err != nil {
			t.Fatalf("seed token file: %v", err)
		}
		t.Setenv("MESH_TOKEN_HASH", string(validHash))

		if err := bootstrapTokenHash(dir, zerolog.Nop()); err != nil {
			t.Fatalf("bootstrapTokenHash returned error: %v", err)
		}

		got, err := os.ReadFile(tokenPath)
		if err != nil {
			t.Fatalf("read token file: %v", err)
		}
		if string(got) != string(existing) {
			t.Fatalf("existing token overwritten:\n got  %q\n want %q", got, existing)
		}
	})

	t.Run("no env var is a no-op", func(t *testing.T) {
		dir := t.TempDir()
		if err := bootstrapTokenHash(dir, zerolog.Nop()); err != nil {
			t.Fatalf("bootstrapTokenHash returned error: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "mesh.token")); !os.IsNotExist(err) {
			t.Fatalf("expected no token file, got err=%v", err)
		}
	})

	t.Run("invalid bcrypt hash causes error", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("MESH_TOKEN_HASH", "not-a-bcrypt-hash")
		err := bootstrapTokenHash(dir, zerolog.Nop())
		if err == nil {
			t.Fatal("expected error for invalid hash, got nil")
		}
	})

	t.Run("plaintext value rejected", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("MESH_TOKEN_HASH", "abc123def456")
		if err := bootstrapTokenHash(dir, zerolog.Nop()); err == nil {
			t.Fatal("expected error for plaintext value, got nil")
		}
	})

	t.Run("empty config dir is an error", func(t *testing.T) {
		t.Setenv("MESH_TOKEN_HASH", string(validHash))
		if err := bootstrapTokenHash("", zerolog.Nop()); err == nil {
			t.Fatal("expected error for empty config dir")
		}
	})
}
