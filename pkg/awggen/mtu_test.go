package awggen

import (
	"strings"
	"testing"
)

func TestValidateMTU(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		params      *Params
		physicalMTU int
		awgOverhead int
		expectError string
	}{
		{
			name:        "valid",
			params:      &Params{S3: 100, S4: 50},
			physicalMTU: 1500,
			awgOverhead: 80,
		},
		{
			name:        "nil params",
			params:      nil,
			physicalMTU: 1500,
			awgOverhead: 80,
			expectError: "params is required",
		},
		{
			name:        "invalid physical mtu",
			params:      &Params{S3: 10, S4: 10},
			physicalMTU: 0,
			awgOverhead: 80,
			expectError: "physicalMTU must be > 0",
		},
		{
			name:        "invalid overhead",
			params:      &Params{S3: 10, S4: 10},
			physicalMTU: 1500,
			awgOverhead: -1,
			expectError: "awgOverhead must be >= 0",
		},
		{
			name:        "negative s values",
			params:      &Params{S3: -1, S4: 10},
			physicalMTU: 1500,
			awgOverhead: 80,
			expectError: "S3 and S4 must be >= 0",
		},
		{
			name:        "overhead exceeds mtu",
			params:      &Params{S3: 800, S4: 700},
			physicalMTU: 1500,
			awgOverhead: 10,
			expectError: "obfuscation overhead exceeds MTU",
		},
		{
			name:        "cookie reply exceeds mtu",
			params:      &Params{S3: 900, S4: 10},
			physicalMTU: 950,
			awgOverhead: 10,
			expectError: "cookie reply exceeds MTU",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateMTU(tt.params, tt.physicalMTU, tt.awgOverhead)
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

func TestEffectiveMTU(t *testing.T) {
	t.Parallel()

	if got := EffectiveMTU(1500, 80); got != 1420 {
		t.Fatalf("unexpected EffectiveMTU: got %d want 1420", got)
	}
}

func TestMaxS4ForMTU(t *testing.T) {
	t.Parallel()

	if got := MaxS4ForMTU(1500, 80); got != 1356 {
		t.Fatalf("unexpected MaxS4ForMTU: got %d want 1356", got)
	}
}
