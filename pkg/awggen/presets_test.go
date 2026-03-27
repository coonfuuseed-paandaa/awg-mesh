package awggen

import (
	"strings"
	"testing"
)

func TestGetPreset(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		input       string
		expectError string
		wantName    string
	}{
		{name: "case insensitive", input: "bAlAnCeD", wantName: "Balanced"},
		{name: "trimmed", input: "  minimal  ", wantName: "Minimal"},
		{name: "empty", input: "", expectError: "preset name is required"},
		{name: "unknown", input: "unknown", expectError: "unknown preset"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			preset, err := GetPreset(tt.input)
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
				t.Fatalf("GetPreset returned error: %v", err)
			}
			if preset.Name != tt.wantName {
				t.Fatalf("unexpected preset: got %s want %s", preset.Name, tt.wantName)
			}
		})
	}
}

func TestListPresetsReturnsCopy(t *testing.T) {
	t.Parallel()

	first := ListPresets()
	if len(first) == 0 {
		t.Fatalf("expected non-empty presets list")
	}

	originalName := first[0].Name
	first[0].Name = "mutated"

	second := ListPresets()
	if second[0].Name != originalName {
		t.Fatalf("ListPresets should return copy, got %q want %q", second[0].Name, originalName)
	}
}
