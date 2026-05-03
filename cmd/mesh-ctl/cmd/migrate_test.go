package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	"gopkg.in/yaml.v3"
)

func TestRunMigrateCommandWritesFileAndProtectsOverwrite(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "v2-topology.yml")

	var out bytes.Buffer
	err := runMigrateCommand(migrateOptions{
		fromPath: v1TopologyFixture,
		toPath:   outPath,
		output:   topologyOutputHuman,
		stdout:   &out,
	})
	if err != nil {
		t.Fatalf("runMigrateCommand: %v", err)
	}
	if !strings.Contains(out.String(), "migration written") || !strings.Contains(out.String(), "warnings=3") {
		t.Fatalf("unexpected output: %q", out.String())
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read migrated topology: %v", err)
	}
	var migrated topology.TopologyV2
	if err := yaml.Unmarshal(data, &migrated); err != nil {
		t.Fatalf("decode migrated topology: %v", err)
	}
	if err := topology.ValidateV2(&migrated); err != nil {
		t.Fatalf("ValidateV2 migrated output: %v\n%s", err, string(data))
	}
	if strings.Contains(string(data), "transport:") || strings.Contains(string(data), "masters:") {
		t.Fatalf("legacy keys leaked into migrated topology:\n%s", string(data))
	}

	err = runMigrateCommand(migrateOptions{
		fromPath: v1TopologyFixture,
		toPath:   outPath,
		output:   topologyOutputHuman,
		stdout:   &bytes.Buffer{},
	})
	if err == nil || !strings.Contains(err.Error(), "without --force") {
		t.Fatalf("expected overwrite guard, got %v", err)
	}
}

func TestRunMigrateCommandOutputsJSONWithoutWritingFile(t *testing.T) {
	var out bytes.Buffer
	err := runMigrateCommand(migrateOptions{
		fromPath: v1TopologyFixture,
		output:   topologyOutputJSON,
		stdout:   &out,
	})
	if err != nil {
		t.Fatalf("runMigrateCommand json: %v", err)
	}
	var got struct {
		SchemaVersion int `json:"schema_version"`
		Mesh          struct {
			OverlaySupernet string `json:"overlay_supernet"`
		} `json:"mesh"`
		Nodes []struct {
			Name  string   `json:"name"`
			Roles []string `json:"roles"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out.String())
	}
	if got.SchemaVersion != 2 || got.Mesh.OverlaySupernet != "172.21.92.0/24" || len(got.Nodes) != 3 {
		t.Fatalf("unexpected json topology: %+v", got)
	}
	if strings.Contains(out.String(), "migration written") {
		t.Fatalf("json output should be topology only, got %s", out.String())
	}
}

func TestRunMigrateCommandRejectsAlreadyV2(t *testing.T) {
	err := runMigrateCommand(migrateOptions{
		fromPath: v2TopologyFixture,
		output:   topologyOutputJSON,
		stdout:   &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("expected v2 input rejection")
	}
	if !strings.Contains(err.Error(), "already schema_version 2") {
		t.Fatalf("unexpected error: %v", err)
	}
}
