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
	tokenFile      = "mesh.token"
	tokenByteLen   = 32
	bcryptCost     = 12
)

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
	if err := os.MkdirAll(dir, 0755); err != nil {
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
