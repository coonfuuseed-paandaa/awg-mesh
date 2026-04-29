package tls

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
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

	if err := VerifyToken("secret-token", hash); err != nil {
		t.Fatalf("expected token verification to succeed, got: %v", err)
	}
	if err := VerifyToken("wrong-token", hash); err == nil {
		t.Fatalf("expected token verification to fail for wrong token")
	}
}

func TestVerifyTokenV2_RoundTrip(t *testing.T) {
	t.Parallel()

	const raw = "round-trip-secret-token"
	hash, err := HashTokenV2(raw)
	if err != nil {
		t.Fatalf("HashTokenV2 returned error: %v", err)
	}

	if err := VerifyToken(raw, hash); err != nil {
		t.Fatalf("VerifyToken returned non-nil on correct token: %v", err)
	}
}

func TestVerifyTokenV2_WrongToken(t *testing.T) {
	t.Parallel()

	hash, err := HashTokenV2("correct-token")
	if err != nil {
		t.Fatalf("HashTokenV2 returned error: %v", err)
	}

	err = VerifyToken("wrong-token", hash)
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken, got: %v", err)
	}
}

func TestArgon2idVerifyTime_AMD64(t *testing.T) {
	// Informational: record how long one argon2id verify round takes.
	// Flag if > 30 ms — that would indicate unexpected CPU contention or
	// misconfigured parameters in CI.
	hash, err := HashTokenV2("timing-test-token")
	if err != nil {
		t.Fatalf("HashTokenV2 returned error: %v", err)
	}

	start := time.Now()
	if err := VerifyToken("timing-test-token", hash); err != nil {
		t.Fatalf("VerifyToken returned error: %v", err)
	}
	elapsed := time.Since(start)

	t.Logf("argon2id verify time: %d ms", elapsed.Milliseconds())
	if elapsed.Milliseconds() > 30 {
		t.Errorf("argon2id verify took %d ms (> 30 ms threshold) — check CPU contention or params", elapsed.Milliseconds())
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

func TestHashTokenV2_Charset(t *testing.T) {
	t.Parallel()

	hash, err := HashTokenV2("my-raw-token")
	if err != nil {
		t.Fatalf("HashTokenV2 returned error: %v", err)
	}
	re := regexp.MustCompile(`^mesh1\.[A-Za-z0-9_-]{72}$`)
	if !re.MatchString(hash) {
		t.Fatalf("HashTokenV2 output %q does not match ^mesh1\\.[A-Za-z0-9_-]{72}$", hash)
	}
}

func TestHashTokenV2_Length(t *testing.T) {
	t.Parallel()

	hash, err := HashTokenV2("my-raw-token")
	if err != nil {
		t.Fatalf("HashTokenV2 returned error: %v", err)
	}
	if len(hash) != 78 {
		t.Fatalf("expected 78-char hash, got %d: %q", len(hash), hash)
	}
}

func TestParseV2_ValidBlob(t *testing.T) {
	t.Parallel()

	rawToken := "round-trip-test-token"
	hash, err := HashTokenV2(rawToken)
	if err != nil {
		t.Fatalf("HashTokenV2 returned error: %v", err)
	}

	fv, algo, mCost, tCost, par, salt, key, err := ParseV2(hash)
	if err != nil {
		t.Fatalf("ParseV2 returned error: %v", err)
	}

	if fv != tokenFormatV1 {
		t.Errorf("format_version: want %d, got %d", tokenFormatV1, fv)
	}
	if algo != argon2idAlgo {
		t.Errorf("algo: want %d, got %d", argon2idAlgo, algo)
	}
	if mCost != argon2MemKB {
		t.Errorf("m_cost_kb: want %d, got %d", argon2MemKB, mCost)
	}
	if tCost != argon2Iters {
		t.Errorf("t_cost: want %d, got %d", argon2Iters, tCost)
	}
	if par != argon2Parallel {
		t.Errorf("parallelism: want %d, got %d", argon2Parallel, par)
	}
	if len(salt) != argon2SaltLen {
		t.Errorf("salt length: want %d, got %d", argon2SaltLen, len(salt))
	}
	if len(key) != int(argon2KeyLen) {
		t.Errorf("key length: want %d, got %d", argon2KeyLen, len(key))
	}

	// ParseV2 returns copies of the blob fields; ensure they match what's in the blob.
	body := hash[len(tokenFormatPrefix):]
	blob, _ := base64.RawURLEncoding.DecodeString(body)
	for i, b := range salt {
		if b != blob[6+i] {
			t.Errorf("salt[%d]: want 0x%02x, got 0x%02x", i, blob[6+i], b)
		}
	}
	for i, b := range key {
		if b != blob[22+i] {
			t.Errorf("key[%d]: want 0x%02x, got 0x%02x", i, blob[22+i], b)
		}
	}
	// Verify BigEndian encoding of mCost in blob matches returned mCostKB.
	if binary.BigEndian.Uint16(blob[2:4]) != mCost {
		t.Errorf("BigEndian mCost mismatch in blob")
	}
}

func TestParseV2_InvalidPrefix(t *testing.T) {
	t.Parallel()

	_, _, _, _, _, _, _, err := ParseV2("v1$thisisnotav2hash")
	if !errors.Is(err, ErrUnknownTokenFormat) {
		t.Fatalf("expected ErrUnknownTokenFormat, got %v", err)
	}
}

func TestParseV2_MalformedBlob(t *testing.T) {
	t.Parallel()

	// "mesh1." prefix present but body is not valid base64url.
	_, _, _, _, _, _, _, err := ParseV2("mesh1.!!!not-base64!!!")
	if !errors.Is(err, ErrMalformedTokenBlob) {
		t.Fatalf("expected ErrMalformedTokenBlob, got %v", err)
	}
}

func TestParseV2_WrongBlobSize(t *testing.T) {
	t.Parallel()

	// Encode a 53-byte blob (one byte short) — valid base64 but wrong size.
	shortBlob := make([]byte, 53)
	encoded := base64.RawURLEncoding.EncodeToString(shortBlob)
	_, _, _, _, _, _, _, err := ParseV2(tokenFormatPrefix + encoded)
	if !errors.Is(err, ErrMalformedTokenBlob) {
		t.Fatalf("expected ErrMalformedTokenBlob for 53-byte blob, got %v", err)
	}

	// Also test 55-byte blob (one byte over).
	longBlob := make([]byte, 55)
	encoded2 := base64.RawURLEncoding.EncodeToString(longBlob)
	_, _, _, _, _, _, _, err2 := ParseV2(tokenFormatPrefix + encoded2)
	if !errors.Is(err2, ErrMalformedTokenBlob) {
		t.Fatalf("expected ErrMalformedTokenBlob for 55-byte blob, got %v", err2)
	}
}
