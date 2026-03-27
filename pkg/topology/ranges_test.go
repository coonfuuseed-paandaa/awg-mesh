package topology

import (
	"net/netip"
	"strings"
	"testing"
)

func TestParseRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		input        NamedRange
		expectError  string
		wantCIDR     string
		wantBalancer string
	}{
		{
			name:         "valid with balancer",
			input:        NamedRange{Name: "core", CIDR: "10.0.1.0/24", BalancerIP: "10.0.1.1"},
			wantCIDR:     "10.0.1.0/24",
			wantBalancer: "10.0.1.1",
		},
		{
			name:     "valid without balancer",
			input:    NamedRange{Name: "edge", CIDR: "10.0.2.0/24"},
			wantCIDR: "10.0.2.0/24",
		},
		{
			name:        "invalid cidr",
			input:       NamedRange{Name: "bad", CIDR: "invalid"},
			expectError: "parse CIDR",
		},
		{
			name:        "invalid balancer",
			input:       NamedRange{Name: "bad", CIDR: "10.0.3.0/24", BalancerIP: "not-an-ip"},
			expectError: "parse balancer IP",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			parsed, err := ParseRange(tt.input)
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
				t.Fatalf("ParseRange returned error: %v", err)
			}
			if parsed.Network.String() != tt.wantCIDR {
				t.Fatalf("unexpected network: got %s want %s", parsed.Network.String(), tt.wantCIDR)
			}
			if tt.wantBalancer == "" {
				if parsed.BalancerIP.IsValid() {
					t.Fatalf("expected no balancer IP, got %s", parsed.BalancerIP)
				}
				return
			}
			if parsed.BalancerIP.String() != tt.wantBalancer {
				t.Fatalf("unexpected balancer IP: got %s want %s", parsed.BalancerIP, tt.wantBalancer)
			}
		})
	}
}

func TestRangeContains(t *testing.T) {
	t.Parallel()

	rangeValue := mustRange(t, NamedRange{Name: "core", CIDR: "10.1.0.0/24"})
	if !rangeValue.Contains(netip.MustParseAddr("10.1.0.10")) {
		t.Fatalf("expected IP to be contained")
	}
	if rangeValue.Contains(netip.MustParseAddr("10.2.0.10")) {
		t.Fatalf("expected IP to be outside range")
	}
}

func TestRangeAvailableIPs(t *testing.T) {
	t.Parallel()

	r30 := mustRange(t, NamedRange{Name: "small", CIDR: "10.2.0.0/30"})
	available := r30.AvailableIPs()
	if len(available) != 2 {
		t.Fatalf("expected 2 available IPs in /30, got %d", len(available))
	}
	if available[0].String() != "10.2.0.1" || available[1].String() != "10.2.0.2" {
		t.Fatalf("unexpected available IPs: %#v", available)
	}

	r31 := mustRange(t, NamedRange{Name: "tiny", CIDR: "10.3.0.0/31"})
	if got := r31.AvailableIPs(); len(got) != 0 {
		t.Fatalf("expected no available IPs in /31, got %#v", got)
	}
}

func TestAllocateIP(t *testing.T) {
	t.Parallel()

	ranges := []Range{
		mustRange(t, NamedRange{Name: "a", CIDR: "10.10.0.0/30"}),
		mustRange(t, NamedRange{Name: "b", CIDR: "10.20.0.0/30"}),
	}

	existing := []netip.Addr{
		netip.MustParseAddr("10.10.0.1"),
	}

	allocated, err := AllocateIP(ranges, existing)
	if err != nil {
		t.Fatalf("AllocateIP returned error: %v", err)
	}
	if allocated.String() != "10.10.0.2" {
		t.Fatalf("unexpected allocated IP: %s", allocated)
	}

	existingAll := []netip.Addr{
		netip.MustParseAddr("10.10.0.1"),
		netip.MustParseAddr("10.10.0.2"),
		netip.MustParseAddr("10.20.0.1"),
		netip.MustParseAddr("10.20.0.2"),
	}
	_, err = AllocateIP(ranges, existingAll)
	if err == nil {
		t.Fatalf("expected no-available-IP error, got nil")
	}
	if !strings.Contains(err.Error(), "no available IP") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRangesOverlap(t *testing.T) {
	t.Parallel()

	overlapA := mustRange(t, NamedRange{Name: "a", CIDR: "10.30.0.0/24"})
	overlapB := mustRange(t, NamedRange{Name: "b", CIDR: "10.30.0.128/25"})
	nonOverlap := mustRange(t, NamedRange{Name: "c", CIDR: "10.31.0.0/24"})

	if !RangesOverlap(overlapA, overlapB) {
		t.Fatalf("expected ranges to overlap")
	}
	if RangesOverlap(overlapA, nonOverlap) {
		t.Fatalf("expected ranges not to overlap")
	}
}

func mustRange(t *testing.T, input NamedRange) Range {
	t.Helper()

	parsed, err := ParseRange(input)
	if err != nil {
		t.Fatalf("ParseRange returned error: %v", err)
	}
	return parsed
}
