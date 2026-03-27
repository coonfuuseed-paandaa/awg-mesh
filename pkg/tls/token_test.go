package tls

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateToken(t *testing.T) {
	t.Parallel()

	token, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken returned error: %v", err)
	}
	if len(token) != 64 {
		t.Fatalf("expected 64-char token, got %d", len(token))
	}
	if _, err := hex.DecodeString(token); err != nil {
		t.Fatalf("token is not valid hex: %v", err)
	}
}

func TestHashAndVerifyToken(t *testing.T) {
	t.Parallel()

	hash, err := HashToken("secret-token")
	if err != nil {
		t.Fatalf("HashToken returned error: %v", err)
	}

	if !VerifyToken("secret-token", hash) {
		t.Fatalf("expected token verification to succeed")
	}
	if VerifyToken("wrong-token", hash) {
		t.Fatalf("expected token verification to fail for wrong token")
	}
}

func TestSaveLoadTokenHash(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	hash, err := HashToken("persisted-token")
	if err != nil {
		t.Fatalf("HashToken returned error: %v", err)
	}

	if err := SaveTokenHash(dir, hash); err != nil {
		t.Fatalf("SaveTokenHash returned error: %v", err)
	}

	loaded, err := LoadTokenHash(dir)
	if err != nil {
		t.Fatalf("LoadTokenHash returned error: %v", err)
	}
	if loaded != hash {
		t.Fatalf("loaded hash mismatch")
	}

	if err := os.WriteFile(filepath.Join(dir, "mesh.token"), []byte(hash+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	trimmed, err := LoadTokenHash(dir)
	if err != nil {
		t.Fatalf("LoadTokenHash returned error: %v", err)
	}
	if trimmed != hash {
		t.Fatalf("expected hash to be trimmed, got %q", trimmed)
	}
}

func TestLoadTokenHashMissingFile(t *testing.T) {
	t.Parallel()

	_, err := LoadTokenHash(t.TempDir())
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "read token hash") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSaveTokenHashError(t *testing.T) {
	t.Parallel()

	filePath := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	err := SaveTokenHash(filePath, "hash")
	if err == nil {
		t.Fatalf("expected SaveTokenHash error, got nil")
	}
	if !strings.Contains(err.Error(), "create token dir") {
		t.Fatalf("unexpected error: %v", err)
	}
}
