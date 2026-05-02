package topology

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/role"
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
