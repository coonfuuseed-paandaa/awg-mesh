// Package role defines the role taxonomy for awg-mesh v2.0 nodes.
//
// A node carries one or more roles declared in topology.yml. Role composability
// rules enforce the v2.0 architecture invariants per F-009:
//   - "client" is exclusive: a client node is a leaf and cannot serve as
//     master, balancer, egress, or ingress simultaneously.
//   - "master", "balancer", "egress", "ingress" are composable freely on the
//     same host.
package role

import (
	"errors"
	"fmt"
)

// Role is an enum of supported node roles.
type Role string

// Role constants. Use these in topology.yml::nodes[*].roles.
const (
	RoleClient   Role = "client"
	RoleMaster   Role = "master"
	RoleEgress   Role = "egress"
	RoleIngress  Role = "ingress"
	RoleBalancer Role = "balancer"
)

// ErrRoleClientExclusive reports that "client" was combined with another role.
// Per F-009 FR-11, client is exclusive: a node with the client role cannot
// also be a master, balancer, egress, or ingress.
var ErrRoleClientExclusive = errors.New("role: 'client' is exclusive and cannot be combined with other roles")

// ErrRoleEmpty reports that a node declared an empty role list.
var ErrRoleEmpty = errors.New("role: at least one role is required")

// ErrRoleUnknown reports a role string that does not match any defined Role.
type ErrRoleUnknown struct {
	Got string
}

func (e *ErrRoleUnknown) Error() string {
	return fmt.Sprintf("role: unknown role %q (allowed: client, master, egress, ingress, balancer)", e.Got)
}

// validRoles is the set of canonical Role values used by the validator.
var validRoles = map[Role]struct{}{
	RoleClient:   {},
	RoleMaster:   {},
	RoleEgress:   {},
	RoleIngress:  {},
	RoleBalancer: {},
}

// ValidateComposability reports whether the given role list is permitted.
//
// Rules:
//  1. roles must be non-empty (returns ErrRoleEmpty otherwise)
//  2. every entry must be a known Role (returns *ErrRoleUnknown otherwise)
//  3. if RoleClient is in the list, it MUST be the only entry
//     (returns ErrRoleClientExclusive otherwise)
//
// Duplicate roles in the input are tolerated (deduplicated implicitly).
func ValidateComposability(roles []Role) error {
	if len(roles) == 0 {
		return ErrRoleEmpty
	}

	seen := make(map[Role]struct{}, len(roles))
	hasClient := false
	for _, r := range roles {
		if _, ok := validRoles[r]; !ok {
			return &ErrRoleUnknown{Got: string(r)}
		}
		seen[r] = struct{}{}
		if r == RoleClient {
			hasClient = true
		}
	}

	if hasClient && len(seen) > 1 {
		return ErrRoleClientExclusive
	}

	return nil
}
