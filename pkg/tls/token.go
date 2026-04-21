package tls

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

const (
	tokenFile    = "mesh.token"
	tokenByteLen = 32
	bcryptCost   = 12
)

// Token lifecycle (B5/B19 clarification):
//
//  1. GenerateToken()  — creates a random 64-char hex string (the raw secret).
//                        This is what the operator manually copies to the node
//                        (or stores locally in <nodeDir>/token for gRPC auth).
//
//  2. HashToken()      — bcrypt-hashes the raw token (cost 12).
//                        The resulting "$2a$12$…" string is stored in two places:
//                          a. <nodeDir>/mesh.token   — via SaveTokenHash (admin-side)
//                          b. MESH_TOKEN_HASH env var — embedded in docker-compose
//                             (apply composeEscapeDollar before writing — `$` in
//                              bcrypt hashes would be interpolated by Docker Compose
//                              and silently truncated to empty strings).
//
//  3. On node first boot — the node reads MESH_TOKEN_HASH, writes it to
//                          /config/mesh.token (inside the container), then ignores
//                          the env var on subsequent starts.
//
//  4. gRPC auth        — the node reads /config/mesh.token and compares each
//                        incoming RPC token via VerifyToken(). The raw token from
//                        <nodeDir>/token is passed by mesh-ctl in RPC calls.
//
// MESH_TOKEN_HASH is a bcrypt HASH, not the raw token. Writing the raw token to
// MESH_TOKEN_HASH would cause every gRPC auth to fail with bcrypt cost mismatch.

// GenerateToken creates a cryptographically random 64-character hex token
// (32 random bytes, hex-encoded).
func GenerateToken() (string, error) {
	buf := make([]byte, tokenByteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// HashToken returns a bcrypt hash of token for secure storage.
func HashToken(token string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(token), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("hash token: %w", err)
	}
	return string(hash), nil
}

// VerifyToken reports whether token matches the stored bcrypt hash.
func VerifyToken(token string, hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(token)) == nil
}

// SaveTokenHash writes the bcrypt hash to <dir>/mesh.token with 0600 permissions.
// Uses atomic write (temp file + rename) to prevent partial reads.
func SaveTokenHash(dir string, hash string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create token dir: %w", err)
	}

	path := filepath.Join(dir, tokenFile)
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(hash), 0600); err != nil {
		return fmt.Errorf("write token hash: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace token hash: %w", err)
	}

	return nil
}

// LoadTokenHash reads the bcrypt hash from <dir>/mesh.token.
func LoadTokenHash(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, tokenFile))
	if err != nil {
		return "", fmt.Errorf("read token hash: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}
