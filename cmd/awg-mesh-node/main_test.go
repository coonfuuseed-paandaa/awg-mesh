package main

import (
	"os"
	"path/filepath"
	"testing"

	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/pkg/tls"
	"github.com/rs/zerolog"
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
	validHash, err := pkgtls.HashTokenV2("test-token")
	if err != nil {
		t.Fatalf("generate test v2 hash: %v", err)
	}

	t.Run("writes hash when token file missing", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("MESH_TOKEN_HASH", validHash)

		if err := bootstrapTokenHash(dir, zerolog.Nop()); err != nil {
			t.Fatalf("bootstrapTokenHash returned error: %v", err)
		}

		data, err := os.ReadFile(filepath.Join(dir, "mesh.token"))
		if err != nil {
			t.Fatalf("read token file: %v", err)
		}
		if string(data) != validHash {
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
		t.Setenv("MESH_TOKEN_HASH", validHash)

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
		// Ensure the env var is absent for this sub-test; restore after.
		prev, wasPrev := os.LookupEnv("MESH_TOKEN_HASH")
		os.Unsetenv("MESH_TOKEN_HASH")
		t.Cleanup(func() {
			if wasPrev {
				os.Setenv("MESH_TOKEN_HASH", prev)
			}
		})
		if err := bootstrapTokenHash(dir, zerolog.Nop()); err != nil {
			t.Fatalf("bootstrapTokenHash returned error: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "mesh.token")); !os.IsNotExist(err) {
			t.Fatalf("expected no token file, got err=%v", err)
		}
	})

	t.Run("invalid hash causes error", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("MESH_TOKEN_HASH", "not-a-valid-hash")
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
		t.Setenv("MESH_TOKEN_HASH", validHash)
		if err := bootstrapTokenHash("", zerolog.Nop()); err == nil {
			t.Fatal("expected error for empty config dir")
		}
	})
}

// TestBootstrapTokenHash_V2 verifies that a valid v2 argon2id hash is accepted
// and written to the token file without error.
func TestBootstrapTokenHash_V2(t *testing.T) {
	token, err := pkgtls.GenerateToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	hash, err := pkgtls.HashTokenV2(token)
	if err != nil {
		t.Fatalf("hash token v2: %v", err)
	}

	dir := t.TempDir()
	t.Setenv("MESH_TOKEN_HASH", hash)

	if err := bootstrapTokenHash(dir, zerolog.Nop()); err != nil {
		t.Fatalf("expected no error for valid v2 hash, got: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "mesh.token"))
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if string(data) != hash {
		t.Fatalf("token file contents mismatch: got %q, want %q", data, hash)
	}
}

// TestBootstrapTokenHash_RejectBcryptLegacy verifies that a bcrypt-format hash
// ($2a$...) is rejected because it lacks the "mesh1." v2 prefix.
func TestBootstrapTokenHash_RejectBcryptLegacy(t *testing.T) {
	// A syntactically valid-looking bcrypt hash that does NOT start with "mesh1."
	bcryptStyleHash := "$2a$12$abcdefghijklmnopqrstuuABCDEFGHIJKLMNOPQRSTUVWXYZ012345"
	dir := t.TempDir()
	t.Setenv("MESH_TOKEN_HASH", bcryptStyleHash)

	err := bootstrapTokenHash(dir, zerolog.Nop())
	if err == nil {
		t.Fatal("expected error for bcrypt-format hash, got nil")
	}
}

// TestBootstrapTokenHash_RejectEmpty verifies that MESH_TOKEN_HASH set to an
// empty (or whitespace-only) value is rejected with an error.
func TestBootstrapTokenHash_RejectEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MESH_TOKEN_HASH", "   ") // set but whitespace-only → trimmed to ""

	err := bootstrapTokenHash(dir, zerolog.Nop())
	if err == nil {
		t.Fatal("expected error for empty MESH_TOKEN_HASH, got nil")
	}
}

// TestClientModeLogRotation verifies that newClientLogRotator returns a
// lumberjack.Logger with the correct rotation parameters for client mode.
func TestClientModeLogRotation(t *testing.T) {
	configDir := "/config"
	rotator := newClientLogRotator(configDir)

	if rotator == nil {
		t.Fatal("expected non-nil lumberjack.Logger from newClientLogRotator")
	}

	wantFilename := "/config/awg-mesh-client.log"
	if rotator.Filename != wantFilename {
		t.Errorf("Filename: got %q, want %q", rotator.Filename, wantFilename)
	}
	if rotator.MaxSize != 10 {
		t.Errorf("MaxSize: got %d, want 10 (MB)", rotator.MaxSize)
	}
	if rotator.MaxBackups != 3 {
		t.Errorf("MaxBackups: got %d, want 3", rotator.MaxBackups)
	}
	if rotator.MaxAge != 0 {
		t.Errorf("MaxAge: got %d, want 0 (no age limit)", rotator.MaxAge)
	}
	if !rotator.LocalTime {
		t.Error("LocalTime: got false, want true")
	}
	if rotator.Compress {
		t.Error("Compress: got true, want false")
	}
}

// TestClientModeLogRotation_CustomDir verifies that the log file path respects
// a non-default configDir (e.g. when MESH_CONFIG_DIR is customised).
func TestClientModeLogRotation_CustomDir(t *testing.T) {
	dir := t.TempDir()
	rotator := newClientLogRotator(dir)

	wantFilename := dir + "/awg-mesh-client.log"
	if rotator.Filename != wantFilename {
		t.Errorf("Filename: got %q, want %q", rotator.Filename, wantFilename)
	}
}
