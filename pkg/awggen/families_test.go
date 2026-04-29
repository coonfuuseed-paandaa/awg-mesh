package awggen

import (
	"strings"
	"testing"
)

func TestGetFamily(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		input       string
		expectError string
		wantName    string
	}{
		{name: "case insensitive", input: "tlsclienthello", wantName: "TLSClientHello"},
		{name: "trimmed", input: "  DNSQuery ", wantName: "DNSQuery"},
		{name: "empty", input: "", expectError: "family name is required"},
		{name: "unknown", input: "foo", expectError: "unknown protocol family"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			family, err := GetFamily(tt.input)
			if tt.expectError != "" {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.expectError) {
					t.Fatalf("expected error containing %q, got %v", tt.expectError, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("GetFamily returned error: %v", err)
			}
			if family.Name != tt.wantName {
				t.Fatalf("unexpected family: got %s want %s", family.Name, tt.wantName)
			}
		})
	}
}

func TestListFamiliesReturnsCopy(t *testing.T) {
	t.Parallel()

	first := ListFamilies()
	if len(first) == 0 {
		t.Fatalf("expected non-empty families list")
	}

	originalName := first[0].Name
	first[0].Name = "mutated"

	second := ListFamilies()
	if second[0].Name != originalName {
		t.Fatalf("ListFamilies should return copy, got %q want %q", second[0].Name, originalName)
	}
}

func TestRandomFamily(t *testing.T) {
	t.Parallel()

	known := make(map[string]struct{})
	for _, family := range ListFamilies() {
		known[family.Name] = struct{}{}
	}

	for attempt := 0; attempt < 20; attempt++ {
		selected := RandomFamily()
		if selected == nil { //nolint:staticcheck // SA5011: t.Fatalf exits — Name access below is safe
			t.Fatalf("RandomFamily returned nil")
		}
		if _, exists := known[selected.Name]; !exists {
			t.Fatalf("RandomFamily returned unknown family %q", selected.Name)
		}
	}
}
