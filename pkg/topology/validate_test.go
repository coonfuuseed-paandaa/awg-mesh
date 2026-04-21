package topology

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateTopologyValid(t *testing.T) {
	t.Parallel()

	top := validTopologyForValidation()
	errs := ValidateTopology(top)
	if len(errs) != 0 {
		t.Fatalf("expected no validation errors, got %#v", errs)
	}
}

func TestValidateTopologyFindings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		mutate          func(*Topology)
		wantFieldPart   string
		wantMessagePart string
		wantSeverity    string
	}{
		{
			name: "invalid overlay CIDR",
			mutate: func(t *Topology) {
				t.Overlay.Space = "bad-cidr"
			},
			wantFieldPart:   "overlay.space",
			wantMessagePart: "invalid overlay CIDR",
			wantSeverity:    "error",
		},
		{
			name: "duplicate master name",
			mutate: func(t *Topology) {
				t.Masters = append(t.Masters, MasterNode{Name: "master-a", Host: "x", OverlayIP: "10.0.1.99", ListenPort: 51820})
			},
			wantFieldPart:   "masters",
			wantMessagePart: "duplicate name",
			wantSeverity:    "error",
		},
		{
			name: "overlapping ranges",
			mutate: func(t *Topology) {
				t.Overlay.Ranges = []NamedRange{
					{Name: "r1", CIDR: "10.0.1.0/24"},
					{Name: "r2", CIDR: "10.0.1.128/25"},
				}
			},
			wantFieldPart:   "overlay.ranges",
			wantMessagePart: "overlap",
			wantSeverity:    "error",
		},
		{
			name: "overlay ip conflict",
			mutate: func(t *Topology) {
				t.Endpoints[0].OverlayIP = t.Masters[0].OverlayIP
			},
			wantFieldPart:   "endpoints[0].overlay_ip",
			wantMessagePart: "already used",
			wantSeverity:    "error",
		},
		{
			name: "bad references warning",
			mutate: func(t *Topology) {
				t.Masters[0].Endpoints = []string{"missing-endpoint"}
				t.Clients[0].Masters = []string{"missing-master"}
			},
			wantFieldPart:   "masters[0].endpoints[0]",
			wantMessagePart: "not found",
			wantSeverity:    "warning",
		},
		{
			name: "range outside overlay",
			mutate: func(t *Topology) {
				t.Overlay.Ranges = []NamedRange{{Name: "outside", CIDR: "10.200.0.0/24"}}
			},
			wantFieldPart:   "overlay.ranges[0].cidr",
			wantMessagePart: "not contained",
			wantSeverity:    "error",
		},
		{
			name: "master name exceeds 12 characters — warning emitted",
			mutate: func(t *Topology) {
				// 14 characters — one over the limit.
				t.Masters[0].Name = "thirteen-chars"
			},
			wantFieldPart:   "masters[0].name",
			wantMessagePart: "truncated",
			wantSeverity:    "warning",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			top := validTopologyForValidation()
			tt.mutate(top)

			errs := ValidateTopology(top)
			if len(errs) == 0 {
				t.Fatalf("expected validation findings, got none")
			}

			if !containsValidationError(errs, tt.wantFieldPart, tt.wantMessagePart, tt.wantSeverity) {
				t.Fatalf("expected finding field=%q message=%q severity=%q, got %#v", tt.wantFieldPart, tt.wantMessagePart, tt.wantSeverity, errs)
			}
		})
	}
}

func containsValidationError(errors []ValidationError, fieldPart, messagePart, severity string) bool {
	for _, current := range errors {
		if strings.Contains(current.Field, fieldPart) &&
			strings.Contains(current.Message, messagePart) &&
			current.Severity == severity {
			return true
		}
	}
	return false
}

func TestValidateDSCP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		dscp    int
		wantErr bool
	}{
		{dscp: -1, wantErr: true},
		{dscp: 0, wantErr: true},
		{dscp: 1, wantErr: false},
		{dscp: 32, wantErr: false},
		{dscp: 63, wantErr: false},
		{dscp: 64, wantErr: true},
		{dscp: 100, wantErr: true},
		{dscp: 153, wantErr: true},
		{dscp: 200, wantErr: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run("", func(t *testing.T) {
			t.Parallel()
			err := ValidateDSCP(tt.dscp)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ValidateDSCP(%d): expected error, got nil", tt.dscp)
				}
				if !errors.Is(err, ErrInvalidDSCP) {
					t.Fatalf("ValidateDSCP(%d): error not sentinel-matchable via ErrInvalidDSCP: %v", tt.dscp, err)
				}
			} else {
				if err != nil {
					t.Fatalf("ValidateDSCP(%d): expected nil, got %v", tt.dscp, err)
				}
			}
		})
	}
}

func TestValidateTopologyDSCPRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		dscp    int
		wantErr bool
	}{
		{dscp: -1, wantErr: true},
		{dscp: 0, wantErr: true},
		{dscp: 1, wantErr: false},
		{dscp: 32, wantErr: false},
		{dscp: 63, wantErr: false},
		{dscp: 64, wantErr: true},
		{dscp: 100, wantErr: true},
		{dscp: 153, wantErr: true},
		{dscp: 200, wantErr: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run("", func(t *testing.T) {
			t.Parallel()

			top := validTopologyForValidation()
			top.Clients[0].RoutingPolicies = []RoutingPolicy{
				{Name: "test-policy", DSCP: tt.dscp, Targets: []string{"endpoint-a"}},
			}

			errs := ValidateTopology(top)

			hasError := false
			for _, e := range errs {
				if strings.Contains(e.Field, "dscp") || strings.Contains(e.Message, "dscp") {
					hasError = true
					break
				}
			}

			if tt.wantErr && !hasError {
				t.Fatalf("ValidateTopology with dscp=%d: expected DSCP error, got %#v", tt.dscp, errs)
			}
			if !tt.wantErr && hasError {
				t.Fatalf("ValidateTopology with dscp=%d: expected no DSCP error, got %#v", tt.dscp, errs)
			}
		})
	}
}

func validTopologyForValidation() *Topology {
	return &Topology{
		Overlay: OverlayConfig{
			Space:       "10.0.0.0/16",
			PhysicalMTU: 1500,
			AWGOverhead: 80,
			Ranges: []NamedRange{
				{Name: "core-a", CIDR: "10.0.1.0/24", BalancerIP: "10.0.1.1"},
				{Name: "core-b", CIDR: "10.0.2.0/24", BalancerIP: "10.0.2.1"},
			},
		},
		Masters: []MasterNode{
			{Name: "master-a", Host: "m-a.local", OverlayIP: "10.0.1.10", ListenPort: 51820, Endpoints: []string{"endpoint-a"}},
		},
		Endpoints: []EndpointNode{
			{Name: "endpoint-a", Host: "e-a.local", OverlayIP: "10.0.2.10", ListenPort: 51820, Region: "us"},
		},
		Clients: []ClientNode{
			{Name: "client-a", Type: "desktop", OverlayIP: "10.0.3.10", Masters: []string{"master-a"}},
		},
	}
}
