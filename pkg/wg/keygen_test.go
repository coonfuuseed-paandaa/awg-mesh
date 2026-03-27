package wg

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestGeneratePrivateKey(t *testing.T) {
	t.Parallel()

	key, err := GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey returned error: %v", err)
	}

	if key[0]&0x07 != 0 {
		t.Fatalf("private key is not clamped at low bits: got %#02x", key[0])
	}
	if key[31]&0x80 != 0 {
		t.Fatalf("private key highest bit must be cleared: got %#02x", key[31])
	}
	if key[31]&0x40 == 0 {
		t.Fatalf("private key second highest bit must be set: got %#02x", key[31])
	}
}

func TestGenerateKey(t *testing.T) {
	t.Parallel()

	key, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey returned error: %v", err)
	}

	if len(key) != 32 {
		t.Fatalf("expected key length 32, got %d", len(key))
	}
	if key.IsZero() {
		t.Fatalf("generated key should not be all zeros")
	}
}

func TestPublicKeyDeterministic(t *testing.T) {
	t.Parallel()

	privateKey := mustNewKey(t, bytes.Repeat([]byte{0x11}, 32))
	publicKeyA := privateKey.PublicKey()
	publicKeyB := privateKey.PublicKey()

	if publicKeyA != publicKeyB {
		t.Fatalf("public key derivation must be deterministic")
	}
	if publicKeyA.IsZero() {
		t.Fatalf("public key should not be zero")
	}
}

func TestNewKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     []byte
		wantError bool
	}{
		{name: "valid", input: bytes.Repeat([]byte{0x22}, 32), wantError: false},
		{name: "too short", input: bytes.Repeat([]byte{0x22}, 31), wantError: true},
		{name: "too long", input: bytes.Repeat([]byte{0x22}, 33), wantError: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			key, err := NewKey(tt.input)
			if tt.wantError {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("NewKey returned error: %v", err)
			}
			if !bytes.Equal(key[:], tt.input) {
				t.Fatalf("unexpected key bytes")
			}
		})
	}
}

func TestParseKeyAndStringRoundTrip(t *testing.T) {
	t.Parallel()

	original := mustNewKey(t, bytes.Repeat([]byte{0x33}, 32))
	encoded := original.String()

	parsed, err := ParseKey(encoded)
	if err != nil {
		t.Fatalf("ParseKey returned error: %v", err)
	}
	if parsed != original {
		t.Fatalf("round-trip mismatch")
	}
}

func TestParseKeyErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{name: "invalid base64", input: "%%%"},
		{name: "valid base64 wrong len", input: base64.StdEncoding.EncodeToString([]byte{1, 2, 3})},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := ParseKey(tt.input)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

func TestKeyIsZero(t *testing.T) {
	t.Parallel()

	var zero Key
	if !zero.IsZero() {
		t.Fatalf("zero key should be reported as zero")
	}

	nonZero := zero
	nonZero[5] = 1
	if nonZero.IsZero() {
		t.Fatalf("non-zero key should not be reported as zero")
	}
}

func TestMarshalUnmarshalText(t *testing.T) {
	t.Parallel()

	original := mustNewKey(t, bytes.Repeat([]byte{0x44}, 32))
	marshaled, err := original.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText returned error: %v", err)
	}

	var parsed Key
	if err := parsed.UnmarshalText(marshaled); err != nil {
		t.Fatalf("UnmarshalText returned error: %v", err)
	}
	if parsed != original {
		t.Fatalf("marshal/unmarshal mismatch")
	}

	err = parsed.UnmarshalText([]byte("bad"))
	if err == nil {
		t.Fatalf("expected unmarshal error, got nil")
	}
	if !strings.Contains(err.Error(), "parse key text") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func mustNewKey(t *testing.T, input []byte) Key {
	t.Helper()

	key, err := NewKey(input)
	if err != nil {
		t.Fatalf("NewKey failed: %v", err)
	}
	return key
}
