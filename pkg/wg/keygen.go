package wg

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"golang.org/x/crypto/curve25519"
)

// GeneratePrivateKey creates a new clamped Curve25519 private key.
func GeneratePrivateKey() (Key, error) {
	key, err := GenerateKey()
	if err != nil {
		return Key{}, fmt.Errorf("generate private key bytes: %w", err)
	}

	key[0] &= 248
	key[31] &= 127
	key[31] |= 64

	return key, nil
}

// GenerateKey creates a random 32-byte key.
func GenerateKey() (Key, error) {
	var key Key
	if _, err := rand.Read(key[:]); err != nil {
		return Key{}, fmt.Errorf("read random key bytes: %w", err)
	}
	return key, nil
}

// PublicKey returns the public key derived from k.
func (k Key) PublicKey() Key {
	var publicKey Key
	curve25519.ScalarBaseMult((*[32]byte)(&publicKey), (*[32]byte)(&k))
	return publicKey
}

// ParseKey decodes a standard base64 key.
func ParseKey(s string) (Key, error) {
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return Key{}, fmt.Errorf("decode base64 key: %w", err)
	}
	return NewKey(decoded)
}

// NewKey converts b into Key and validates size.
func NewKey(b []byte) (Key, error) {
	if len(b) != 32 {
		return Key{}, fmt.Errorf("invalid key length %d: expected 32", len(b))
	}

	var key Key
	copy(key[:], b)
	return key, nil
}

// String returns the standard base64 key form.
func (k Key) String() string {
	return base64.StdEncoding.EncodeToString(k[:])
}

// IsZero reports whether k is all-zero.
func (k Key) IsZero() bool {
	for _, b := range k {
		if b != 0 {
			return false
		}
	}
	return true
}

// MarshalText encodes k as base64 text.
func (k Key) MarshalText() ([]byte, error) {
	return []byte(k.String()), nil
}

// UnmarshalText decodes a base64 key into k.
func (k *Key) UnmarshalText(b []byte) error {
	parsed, err := ParseKey(string(b))
	if err != nil {
		return fmt.Errorf("parse key text: %w", err)
	}
	*k = parsed
	return nil
}
