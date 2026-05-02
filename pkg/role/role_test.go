package role

import (
	"errors"
	"testing"
)

func TestValidateComposability(t *testing.T) {
	cases := []struct {
		name    string
		roles   []Role
		wantErr error
	}{
		// Single-role acceptance
		{name: "single client", roles: []Role{RoleClient}, wantErr: nil},
		{name: "single master", roles: []Role{RoleMaster}, wantErr: nil},
		{name: "single egress", roles: []Role{RoleEgress}, wantErr: nil},
		{name: "single ingress", roles: []Role{RoleIngress}, wantErr: nil},
		{name: "single balancer", roles: []Role{RoleBalancer}, wantErr: nil},

		// Multi-role acceptance (no client)
		{name: "master+balancer", roles: []Role{RoleMaster, RoleBalancer}, wantErr: nil},
		{
			name:    "all-in-one",
			roles:   []Role{RoleMaster, RoleBalancer, RoleEgress, RoleIngress},
			wantErr: nil,
		},

		// Client exclusivity rejections
		{name: "client+master", roles: []Role{RoleClient, RoleMaster}, wantErr: ErrRoleClientExclusive},
		{name: "client+balancer", roles: []Role{RoleClient, RoleBalancer}, wantErr: ErrRoleClientExclusive},
		{name: "client+egress", roles: []Role{RoleClient, RoleEgress}, wantErr: ErrRoleClientExclusive},
		{name: "client+ingress", roles: []Role{RoleClient, RoleIngress}, wantErr: ErrRoleClientExclusive},
		{name: "client+all", roles: []Role{RoleClient, RoleMaster, RoleEgress, RoleIngress, RoleBalancer}, wantErr: ErrRoleClientExclusive},

		// Empty input
		{name: "empty", roles: []Role{}, wantErr: ErrRoleEmpty},
		{name: "nil", roles: nil, wantErr: ErrRoleEmpty},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateComposability(tc.roles)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateComposability(%v) error = %v, want %v", tc.roles, err, tc.wantErr)
			}
		})
	}
}

func TestValidateComposability_UnknownRole(t *testing.T) {
	err := ValidateComposability([]Role{Role("ghost")})
	if err == nil {
		t.Fatalf("expected error for unknown role, got nil")
	}
	var unk *ErrRoleUnknown
	if !errors.As(err, &unk) {
		t.Fatalf("expected *ErrRoleUnknown, got %T: %v", err, err)
	}
	if unk.Got != "ghost" {
		t.Fatalf("ErrRoleUnknown.Got = %q, want %q", unk.Got, "ghost")
	}
}

func TestValidateComposability_ClientDuplicate(t *testing.T) {
	// Duplicate client-only roles is acceptable.
	if err := ValidateComposability([]Role{RoleClient, RoleClient}); err != nil {
		t.Fatalf("duplicate client should be tolerated, got: %v", err)
	}
}
