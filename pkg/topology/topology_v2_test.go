package topology

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/role"
	"gopkg.in/yaml.v3"
)

func TestValidateV2_V2Fixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "v2-topology.yml"))
	if err != nil {
		t.Fatalf("read v2 fixture: %v", err)
	}
	var topo TopologyV2
	if err := yaml.Unmarshal(data, &topo); err != nil {
		t.Fatalf("unmarshal v2 fixture: %v", err)
	}
	if err := ValidateV2(&topo); err != nil {
		t.Fatalf("ValidateV2 on v2 fixture should PASS, got: %v", err)
	}
}

func TestValidateV2_NilTopology(t *testing.T) {
	if err := ValidateV2(nil); err == nil {
		t.Fatalf("ValidateV2(nil) should error")
	}
}

func TestValidateV2_WrongSchema(t *testing.T) {
	t.Run("schema_version=1", func(t *testing.T) {
		topo := &TopologyV2{
			SchemaVersion: SchemaV1,
			Mesh:          MeshConfig{Name: "x", OverlaySupernet: "10.0.0.0/24"},
			Nodes: []NodeV2{
				{Name: "n1", Roles: []role.Role{role.RoleMaster}, OverlayIP: "10.0.0.1"},
			},
		}
		if err := ValidateV2(topo); !errors.Is(err, ErrV2SchemaMissing) {
			t.Fatalf("expected ErrV2SchemaMissing, got: %v", err)
		}
	})
	t.Run("schema_version=0", func(t *testing.T) {
		topo := &TopologyV2{
			SchemaVersion: 0,
			Mesh:          MeshConfig{Name: "x", OverlaySupernet: "10.0.0.0/24"},
			Nodes: []NodeV2{
				{Name: "n1", Roles: []role.Role{role.RoleMaster}, OverlayIP: "10.0.0.1"},
			},
		}
		if err := ValidateV2(topo); !errors.Is(err, ErrV2SchemaMissing) {
			t.Fatalf("expected ErrV2SchemaMissing, got: %v", err)
		}
	})
}

func TestValidateV2_OverlaySupernet(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		topo := &TopologyV2{
			SchemaVersion: SchemaV2,
			Nodes: []NodeV2{
				{Name: "n1", Roles: []role.Role{role.RoleMaster}, OverlayIP: "10.0.0.1"},
			},
		}
		if err := ValidateV2(topo); !errors.Is(err, ErrV2OverlayMissing) {
			t.Fatalf("expected ErrV2OverlayMissing, got: %v", err)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		topo := &TopologyV2{
			SchemaVersion: SchemaV2,
			Mesh:          MeshConfig{OverlaySupernet: "not-a-cidr"},
			Nodes: []NodeV2{
				{Name: "n1", Roles: []role.Role{role.RoleMaster}, OverlayIP: "10.0.0.1"},
			},
		}
		if err := ValidateV2(topo); !errors.Is(err, ErrV2OverlayInvalid) {
			t.Fatalf("expected ErrV2OverlayInvalid, got: %v", err)
		}
	})
}

func TestValidateV2_NoNodes(t *testing.T) {
	topo := &TopologyV2{
		SchemaVersion: SchemaV2,
		Mesh:          MeshConfig{OverlaySupernet: "10.0.0.0/24"},
	}
	if err := ValidateV2(topo); !errors.Is(err, ErrV2NoNodes) {
		t.Fatalf("expected ErrV2NoNodes, got: %v", err)
	}
}

func TestValidateV2_DuplicateOverlay(t *testing.T) {
	topo := &TopologyV2{
		SchemaVersion: SchemaV2,
		Mesh:          MeshConfig{OverlaySupernet: "10.0.0.0/24"},
		Nodes: []NodeV2{
			{Name: "n1", Roles: []role.Role{role.RoleMaster}, OverlayIP: "10.0.0.1"},
			{Name: "n2", Roles: []role.Role{role.RoleEgress}, OverlayIP: "10.0.0.1"},
		},
	}
	if err := ValidateV2(topo); !errors.Is(err, ErrV2OverlayDuplicate) {
		t.Fatalf("expected ErrV2OverlayDuplicate, got: %v", err)
	}
}

func TestValidateV2_OverlayOutOfRange(t *testing.T) {
	topo := &TopologyV2{
		SchemaVersion: SchemaV2,
		Mesh:          MeshConfig{OverlaySupernet: "10.0.0.0/24"},
		Nodes: []NodeV2{
			{Name: "n1", Roles: []role.Role{role.RoleMaster}, OverlayIP: "172.16.0.1"},
		},
	}
	if err := ValidateV2(topo); !errors.Is(err, ErrV2OverlayOutOfRange) {
		t.Fatalf("expected ErrV2OverlayOutOfRange, got: %v", err)
	}
}

func TestValidateV2_RoleViolation(t *testing.T) {
	topo := &TopologyV2{
		SchemaVersion: SchemaV2,
		Mesh:          MeshConfig{OverlaySupernet: "10.0.0.0/24"},
		Nodes: []NodeV2{
			{Name: "bad", Roles: []role.Role{role.RoleClient, role.RoleMaster}, OverlayIP: "10.0.0.1"},
		},
	}
	err := ValidateV2(topo)
	if err == nil {
		t.Fatalf("expected role-composability error, got nil")
	}
	if !errors.Is(err, role.ErrRoleClientExclusive) {
		t.Fatalf("expected ErrRoleClientExclusive in error chain, got: %v", err)
	}
}

func TestValidateV2_MeshEndpointFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		node        NodeV2
		wantErrPart string
	}{
		{
			name: "valid explicit endpoint and port",
			node: NodeV2{
				Name:           "egress-a",
				Roles:          []role.Role{role.RoleEgress},
				OverlayIP:      "10.0.0.10",
				PublicIP:       "203.0.113.10",
				MeshListenPort: 443,
				MeshEndpoint:   "mesh.example.test:443",
			},
		},
		{
			name: "invalid listen port",
			node: NodeV2{
				Name:           "egress-a",
				Roles:          []role.Role{role.RoleEgress},
				OverlayIP:      "10.0.0.10",
				MeshListenPort: 70000,
			},
			wantErrPart: "mesh_listen_port",
		},
		{
			name: "invalid mesh endpoint",
			node: NodeV2{
				Name:         "egress-a",
				Roles:        []role.Role{role.RoleEgress},
				OverlayIP:    "10.0.0.10",
				MeshEndpoint: "mesh.example.test:0",
			},
			wantErrPart: "mesh_endpoint",
		},
		{
			name: "public ip must not carry port",
			node: NodeV2{
				Name:      "egress-a",
				Roles:     []role.Role{role.RoleEgress},
				OverlayIP: "10.0.0.10",
				PublicIP:  "203.0.113.10:443",
			},
			wantErrPart: "public_ip",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			topo := &TopologyV2{
				SchemaVersion: SchemaV2,
				Mesh:          MeshConfig{OverlaySupernet: "10.0.0.0/24"},
				Nodes:         []NodeV2{tt.node},
			}
			err := ValidateV2(topo)
			if tt.wantErrPart == "" {
				if err != nil {
					t.Fatalf("ValidateV2 returned error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q", tt.wantErrPart)
			}
			if !strings.Contains(err.Error(), tt.wantErrPart) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErrPart, err)
			}
		})
	}
}
