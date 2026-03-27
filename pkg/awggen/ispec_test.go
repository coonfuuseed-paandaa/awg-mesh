package awggen

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseISpecBinaryTag(t *testing.T) {
	t.Parallel()

	payload, err := ParseISpec("<b deadbeef>")
	if err != nil {
		t.Fatalf("ParseISpec returned error: %v", err)
	}

	expected := []byte{0xde, 0xad, 0xbe, 0xef}
	if !bytes.Equal(payload, expected) {
		t.Fatalf("unexpected payload: got %x want %x", payload, expected)
	}
}

func TestParseISpecTagBehaviors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		spec         string
		minLen       int
		maxLen       int
		digitsOnly   bool
		nonEmpty     bool
		expectPrefix []byte
	}{
		{name: "random bytes", spec: "<r 4>", minLen: 4, maxLen: 4},
		{name: "crypto random bytes", spec: "<rc 8>", minLen: 8, maxLen: 8},
		{name: "random digits", spec: "<rd 4 8>", minLen: 4, maxLen: 8, digitsOnly: true},
		{name: "template expansion", spec: "<t TLS_EXTENSIONS>", nonEmpty: true},
		{name: "literal token", spec: "HELLO", expectPrefix: []byte("HELLO"), minLen: 5, maxLen: 5},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload, err := ParseISpec(tt.spec)
			if err != nil {
				t.Fatalf("ParseISpec returned error: %v", err)
			}
			if tt.nonEmpty && len(payload) == 0 {
				t.Fatalf("expected non-empty payload")
			}
			if tt.minLen > 0 && len(payload) < tt.minLen {
				t.Fatalf("payload too short: got %d want >= %d", len(payload), tt.minLen)
			}
			if tt.maxLen > 0 && len(payload) > tt.maxLen {
				t.Fatalf("payload too long: got %d want <= %d", len(payload), tt.maxLen)
			}
			if len(tt.expectPrefix) > 0 && !bytes.Equal(payload, tt.expectPrefix) {
				t.Fatalf("unexpected literal payload: got %q want %q", payload, tt.expectPrefix)
			}
			if tt.digitsOnly && !allDigits(payload) {
				t.Fatalf("expected digit-only payload, got %q", payload)
			}
		})
	}
}

func TestParseISpecErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		spec        string
		expectError string
	}{
		{name: "unsupported tag", spec: "<x 1>", expectError: "unsupported I-spec tag"},
		{name: "invalid hex", spec: "<b zz>", expectError: "invalid hex"},
		{name: "unterminated tag", spec: "<b deadbeef", expectError: "unterminated tag"},
		{name: "invalid rd range", spec: "<rd 8 4>", expectError: "max length must be >= min length"},
		{name: "unknown template", spec: "<t UNKNOWN_TEMPLATE>", expectError: "unknown protocol template"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := ParseISpec(tt.spec)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.expectError) {
				t.Fatalf("expected error containing %q, got %v", tt.expectError, err)
			}
		})
	}
}

func TestGenerateISpec(t *testing.T) {
	t.Parallel()

	_, err := GenerateISpec(nil)
	if err == nil || !strings.Contains(err.Error(), "protocol family is required") {
		t.Fatalf("expected protocol family required error, got %v", err)
	}

	family, err := GetFamily("TLSClientHello")
	if err != nil {
		t.Fatalf("GetFamily returned error: %v", err)
	}

	values, err := GenerateISpec(family)
	if err != nil {
		t.Fatalf("GenerateISpec returned error: %v", err)
	}
	if len(values) != 5 {
		t.Fatalf("expected 5 I-spec values, got %d", len(values))
	}
	for i, value := range values {
		if strings.TrimSpace(value) == "" {
			t.Fatalf("expected non-empty I%d", i+1)
		}
	}

	invalidFamily := &ProtocolFamily{Name: "Bad", I1: "<x 1>"}
	_, err = GenerateISpec(invalidFamily)
	if err == nil || !strings.Contains(err.Error(), "invalid I1 template") {
		t.Fatalf("expected invalid template error, got %v", err)
	}
}

func TestValidateISpec(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		spec        string
		expectError string
	}{
		{name: "valid", spec: "<b deadbeef> <r 2> <t TLS_EXTENSIONS>"},
		{name: "invalid", spec: "<rd -1 2>", expectError: "must be >= 0"},
		{name: "unknown template", spec: "<t MISSING>", expectError: "unknown protocol template"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateISpec(tt.spec)
			if tt.expectError == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}

			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.expectError) {
				t.Fatalf("expected error containing %q, got %v", tt.expectError, err)
			}
		})
	}
}

func allDigits(value []byte) bool {
	for _, b := range value {
		if b < '0' || b > '9' {
			return false
		}
	}
	return true
}
