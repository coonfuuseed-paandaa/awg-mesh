package mikrotik

// PR review process: when this test fails because of intentional generator changes,
// run `go test ./pkg/mikrotik/... -run TestDeployRSCGolden -update` to re-seed the
// golden file, then commit the diff with explicit reviewer approval. The golden
// represents the byte-stable contract between awg-mesh-ctl and RouterOS — any
// drift must be intentional and reviewed.

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var updateGolden = flag.Bool("update", false, "regenerate golden file (TestDeployRSCGolden)")

func TestDeployRSCGolden(t *testing.T) {
	t.Parallel()

	fixture := DeployScript{
		TopologyName:  "mikrotik-home",
		ContainerName: "AWG_MESH_HOME",
		BridgeName:    "BR_AWG_MESH",
		Image:         "ghcr.io/coonfuuseed-paandaa/awg-mesh-client:v1.14.0",
		Veth:          "AWG_MESH_HOME",
		VethGateway:   "100.127.0.1",
		OverlayIP:     "10.10.0.10",
		OverlayNet:    "10.10.0.0/16",
		TokenHash:     "mesh1.AAEBABACAQEABAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHyAhIiMkJSYnKCkqKyws",
		DNS:           []string{"1.1.1.1", "8.8.8.8"},
		GRPCPort:      9090,
		StorageRoot:   "disk1",
	}

	got, err := GenerateDeployRSC(fixture)
	if err != nil {
		t.Fatalf("GenerateDeployRSC: %v", err)
	}

	goldenPath := filepath.Join("testdata", "deploy-golden.rsc")

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("golden file regenerated: %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update to seed): %v", err)
	}

	if got != string(want) {
		t.Errorf("generator output drifted from golden\n--- want (%d bytes) ---\n%s\n--- got (%d bytes) ---\n%s",
			len(want), string(want), len(got), got)
	}
}
