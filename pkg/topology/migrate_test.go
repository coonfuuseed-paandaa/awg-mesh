package topology

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectSchemaVersion_V1Fixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "v1x-topology.yml"))
	if err != nil {
		t.Fatalf("read v1 fixture: %v", err)
	}
	got, err := DetectSchemaVersion(data)
	if err != nil {
		t.Fatalf("DetectSchemaVersion on v1 fixture: %v", err)
	}
	if got != SchemaV1 {
		t.Fatalf("expected SchemaV1 (1), got %d", got)
	}
}

func TestDetectSchemaVersion_V2Fixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "v2-topology.yml"))
	if err != nil {
		t.Fatalf("read v2 fixture: %v", err)
	}
	got, err := DetectSchemaVersion(data)
	if err != nil {
		t.Fatalf("DetectSchemaVersion on v2 fixture: %v", err)
	}
	if got != SchemaV2 {
		t.Fatalf("expected SchemaV2 (2), got %d", got)
	}
}

func TestDetectSchemaVersion_Empty(t *testing.T) {
	if _, err := DetectSchemaVersion(nil); err == nil {
		t.Fatalf("DetectSchemaVersion(nil) must error")
	}
	if _, err := DetectSchemaVersion([]byte{}); err == nil {
		t.Fatalf("DetectSchemaVersion(empty) must error")
	}
}

func TestDetectSchemaVersion_OnlyTransportPool(t *testing.T) {
	// transport: with pool is the v1.x marker even without masters/endpoints keys.
	data := []byte(`transport:
  pool: 10.255.0.0/16
  prefix_length: 30
`)
	got, err := DetectSchemaVersion(data)
	if err != nil {
		t.Fatalf("DetectSchemaVersion: %v", err)
	}
	if got != SchemaV1 {
		t.Fatalf("expected SchemaV1, got %d", got)
	}
}

func TestDetectSchemaVersion_UnsupportedSchema(t *testing.T) {
	data := []byte(`schema_version: 99
`)
	_, err := DetectSchemaVersion(data)
	if err == nil {
		t.Fatalf("schema_version=99 must error")
	}
	if !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("expected 'unsupported' in error, got: %v", err)
	}
}

func TestMigrateV1ToV2_ConvertsFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "v1x-topology.yml"))
	if err != nil {
		t.Fatalf("read v1 fixture: %v", err)
	}
	result, err := MigrateV1ToV2WithReport(data)
	if err != nil {
		t.Fatalf("MigrateV1ToV2WithReport: %v", err)
	}
	topo := result.Topology
	if topo.SchemaVersion != SchemaV2 {
		t.Fatalf("schema_version = %d, want 2", topo.SchemaVersion)
	}
	if topo.Mesh.OverlaySupernet != "172.21.92.0/24" {
		t.Fatalf("overlay_supernet = %q", topo.Mesh.OverlaySupernet)
	}
	if len(topo.Nodes) != 3 {
		t.Fatalf("nodes = %d, want 3: %#v", len(topo.Nodes), topo.Nodes)
	}

	assertNode(t, topo, "master-01", []string{"master", "balancer"}, "172.21.92.2", "")
	assertNode(t, topo, "endpoint-us-01", []string{"egress"}, "172.21.92.34", "")
	assertNode(t, topo, "home-01", []string{"client"}, "172.21.92.130", "master-01")
	if len(result.Warnings) != 3 {
		t.Fatalf("warnings = %d, want 3: %#v", len(result.Warnings), result.Warnings)
	}
}

func TestMigrateV1ToV2_RejectsAlreadyV2(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "v2-topology.yml"))
	if err != nil {
		t.Fatalf("read v2 fixture: %v", err)
	}
	_, err = MigrateV1ToV2(data)
	if err == nil {
		t.Fatalf("MigrateV1ToV2 must reject schema v2 input")
	}
	if !strings.Contains(err.Error(), "already schema_version 2") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMigrateV1ToV2_MapsMasterExitToMixedRoles(t *testing.T) {
	topo, err := MigrateV1ToV2([]byte(`
overlay:
  space: 172.21.92.0/24
masters:
  - name: master-exit
    overlay_ip: 172.21.92.2
    exit: true
endpoints: []
clients: []
`))
	if err != nil {
		t.Fatalf("MigrateV1ToV2: %v", err)
	}
	assertNode(t, topo, "master-exit", []string{"master", "balancer", "egress"}, "172.21.92.2", "")
}

func assertNode(t *testing.T, topo *TopologyV2, name string, roles []string, overlayIP string, preferredMaster string) {
	t.Helper()
	for _, node := range topo.Nodes {
		if node.Name != name {
			continue
		}
		if node.OverlayIP != overlayIP {
			t.Fatalf("%s overlay_ip = %q, want %q", name, node.OverlayIP, overlayIP)
		}
		if node.PreferredMaster != preferredMaster {
			t.Fatalf("%s preferred_master = %q, want %q", name, node.PreferredMaster, preferredMaster)
		}
		if len(node.Roles) != len(roles) {
			t.Fatalf("%s roles = %#v, want %#v", name, node.Roles, roles)
		}
		for i, want := range roles {
			if string(node.Roles[i]) != want {
				t.Fatalf("%s role[%d] = %q, want %q", name, i, node.Roles[i], want)
			}
		}
		return
	}
	t.Fatalf("node %q not found in %#v", name, topo.Nodes)
}
