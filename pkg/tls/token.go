package tls

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	tokenFile    = "mesh.token"
	tokenByteLen = 32
)

// v2 token format constants — argon2id-based, all-ASCII-safe charset.
const (
	tokenFormatPrefix = "mesh1."
	argon2idAlgo      uint8  = 0x01
	tokenFormatV1     uint8  = 0x01
	argon2MemKB       uint16 = 4096
	argon2Iters       uint8  = 1
	argon2Parallel    uint8  = 1
	argon2KeyLen      uint32 = 32
	argon2SaltLen            = 16
	tokenBlobLen             = 54
)

// Sentinel errors for v2 token parsing and verification.
var (
	ErrUnknownTokenFormat = errors.New("unknown token format")
	ErrMalformedTokenBlob = errors.New("malformed token blob")
	ErrInvalidToken       = errors.New("invalid token")
)

// Token lifecycle (B5/B19 clarification):
//
//  1. GenerateToken()  — creates a random 64-char hex string (the raw secret).
//                        This is what the operator manually copies to the node
//                        (or stores locally in <nodeDir>/token for gRPC auth).
//
//  2. HashToken()      — argon2id-hashes the raw token (m=4096 KiB, t=1, p=1),
//                        delegating to HashTokenV2. The resulting v2-format hash
//                        "mesh1.<base64url>" is stored in two places:
//                          a. <nodeDir>/mesh.token   — via SaveTokenHash (admin-side)
//                          b. MESH_TOKEN_HASH env var — embedded in docker-compose
//                             (safe charset [A-Za-z0-9_-], no quoting required).
//
//  3. On node first boot — the node reads MESH_TOKEN_HASH, writes it to
//                          /config/mesh.token (inside the container), then ignores
//                          the env var on subsequent starts.
//
//  4. gRPC auth        — the node reads /config/mesh.token and compares each
//                        incoming RPC token via VerifyToken(). The raw token from
//                        <nodeDir>/token is passed by mesh-ctl in RPC calls.
//
// MESH_TOKEN_HASH is a v2-format hash (mesh1.<base64url>), not the raw token.
// Writing the raw token to MESH_TOKEN_HASH would cause every gRPC auth to fail.

// GenerateToken creates a cryptographically random 64-character hex token
// (32 random bytes, hex-encoded).
func GenerateToken() (string, error) {
	buf := make([]byte, tokenByteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// HashToken hashes token for secure storage. Delegates to HashTokenV2 (argon2id).
func HashToken(token string) (string, error) {
	return HashTokenV2(token)
}

// VerifyToken verifies rawToken against a v2 hash produced by HashTokenV2.
// Returns nil on a correct match, ErrInvalidToken on mismatch, or a sentinel
// error (ErrUnknownTokenFormat / ErrMalformedTokenBlob) on malformed input.
// The comparison is constant-time to prevent timing side-channels.
func VerifyToken(rawToken, hash string) error {
	_, _, mCostKB, tCost, parallelism, salt, key, err := ParseV2(hash)
	if err != nil {
		return err
	}
	computed := argon2.IDKey([]byte(rawToken), salt, uint32(tCost), uint32(mCostKB), parallelism, uint32(len(key)))
	if subtle.ConstantTimeCompare(computed, key) != 1 {
		return ErrInvalidToken
	}
	return nil
}

// SaveTokenHash writes the token hash to <dir>/mesh.token with 0600 permissions.
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

// LoadTokenHash reads the token hash from <dir>/mesh.token.
func LoadTokenHash(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, tokenFile))
	if err != nil {
		return "", fmt.Errorf("read token hash: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// HashTokenV2 generates an argon2id-based token hash with the safe charset
// [A-Za-z0-9_-] in the format "mesh1.<base64url(54-byte blob)>". Total: 78 chars.
//
// Wire format (54 bytes, BigEndian):
//
//	[0]    format_version  = 0x01
//	[1]    algo            = 0x01 (argon2id)
//	[2:4]  m_cost_kb       = 4096 (uint16 BE)
//	[4]    t_cost          = 1
//	[5]    parallelism     = 1
//	[6:22] salt            = 16-byte CSPRNG random
//	[22:54] hash           = 32-byte argon2id output
func HashTokenV2(rawToken string) (string, error) {
	salt := make([]byte, argon2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(rawToken), salt, uint32(argon2Iters), uint32(argon2MemKB), argon2Parallel, argon2KeyLen)

	blob := make([]byte, tokenBlobLen)
	blob[0] = tokenFormatV1
	blob[1] = argon2idAlgo
	binary.BigEndian.PutUint16(blob[2:4], argon2MemKB)
	blob[4] = argon2Iters
	blob[5] = argon2Parallel
	copy(blob[6:22], salt)
	copy(blob[22:54], key)

	return tokenFormatPrefix + base64.RawURLEncoding.EncodeToString(blob), nil
}

// ParseV2 parses a v2 token hash and returns its constituent fields.
// Returns ErrUnknownTokenFormat if the prefix is not "mesh1.",
// ErrMalformedTokenBlob on bad base64 or wrong blob length (not 54 bytes).
func ParseV2(hash string) (formatVersion, algo uint8, mCostKB uint16, tCost, parallelism uint8, salt, key []byte, err error) {
	if !strings.HasPrefix(hash, tokenFormatPrefix) {
		err = ErrUnknownTokenFormat
		return
	}
	body := hash[len(tokenFormatPrefix):]
	blob, decErr := base64.RawURLEncoding.DecodeString(body)
	if decErr != nil {
		err = ErrMalformedTokenBlob
		return
	}
	if len(blob) != tokenBlobLen {
		err = ErrMalformedTokenBlob
		return
	}
	formatVersion = blob[0]
	algo = blob[1]
	mCostKB = binary.BigEndian.Uint16(blob[2:4])
	tCost = blob[4]
	parallelism = blob[5]
	salt = make([]byte, argon2SaltLen)
	copy(salt, blob[6:22])
	key = make([]byte, argon2KeyLen)
	copy(key, blob[22:54])
	return
}
