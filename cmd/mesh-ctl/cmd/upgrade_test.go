package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunUpgradeCommandDryRunV2Plan(t *testing.T) {
	dir := t.TempDir()
	topologyPath := writeUpgradeTopology(t, dir)

	var out bytes.Buffer
	err := runUpgradeCommand(upgradeOptions{
		version:      "v2.0.1",
		topologyPath: topologyPath,
		configDir:    filepath.Join(dir, "config"),
		dryRun:       true,
		stdout:       &out,
	})
	if err != nil {
		t.Fatalf("runUpgradeCommand dry-run: %v", err)
	}

	text := out.String()
	for _, want := range []string{
		"Dry run",
		"PHASE",
		"masters",
		"mesh-roles",
		"clients",
		"master-a",
		"egress-a",
		"ingress-b",
		"client-z",
		"v2.0.1",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("dry-run output missing %q:\n%s", want, text)
		}
	}
	assertTextOrder(t, text, "master-a", "egress-a", "ingress-b", "client-z")
}

func TestRunUpgradeCommandHonorsManualOrder(t *testing.T) {
	dir := t.TempDir()
	topologyPath := writeUpgradeTopology(t, dir)

	var out bytes.Buffer
	err := runUpgradeCommand(upgradeOptions{
		version:      "v2.0.1",
		topologyPath: topologyPath,
		configDir:    filepath.Join(dir, "config"),
		dryRun:       true,
		order:        "client-z,master-a",
		stdout:       &out,
	})
	if err != nil {
		t.Fatalf("runUpgradeCommand manual order: %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "manual") {
		t.Fatalf("manual plan did not label manual phase:\n%s", text)
	}
	assertTextOrder(t, text, "client-z", "master-a")
}

func TestRunUpgradeCommandNonDryRunFailsExplicitUnsupported(t *testing.T) {
	dir := t.TempDir()
	topologyPath := writeUpgradeTopology(t, dir)
	configDir := filepath.Join(dir, "config")

	err := runUpgradeCommand(upgradeOptions{
		version:      "v2.0.1",
		topologyPath: topologyPath,
		configDir:    configDir,
		stdout:       &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("expected unsupported execution error")
	}
	if !strings.Contains(err.Error(), "v2 upgrade execution is not supported") {
		t.Fatalf("unexpected error: %v", err)
	}

	state, err := loadUpgradeState(configDir)
	if err != nil {
		t.Fatalf("load upgrade state: %v", err)
	}
	if state.Status != upgradeStatusBlocked || state.Version != "v2.0.1" || len(state.Plan) == 0 {
		t.Fatalf("unexpected persisted blocked state: %+v", state)
	}
}

func TestRunUpgradePauseResumeStatusMutateState(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "config")
	initial := meshUpgradeState{
		Version: "v2.0.1",
		Status:  upgradeStatusRunning,
		Plan: []meshUpgradePlanEntry{{
			Phase:         1,
			PhaseName:     "masters",
			NodeName:      "master-a",
			Roles:         []string{"master"},
			TargetVersion: "v2.0.1",
			Status:        "planned",
		}},
	}
	if err := saveUpgradeState(configDir, initial); err != nil {
		t.Fatalf("save initial state: %v", err)
	}

	if err := runUpgradePauseCommand(upgradeStateOptions{configDir: configDir, stdout: &bytes.Buffer{}}); err != nil {
		t.Fatalf("pause upgrade: %v", err)
	}
	paused, err := loadUpgradeState(configDir)
	if err != nil {
		t.Fatalf("load paused state: %v", err)
	}
	if !paused.Paused || paused.Status != upgradeStatusPaused {
		t.Fatalf("pause did not persist paused state: %+v", paused)
	}

	var statusOut bytes.Buffer
	if err := runUpgradeStatusCommand(upgradeStateOptions{configDir: configDir, stdout: &statusOut}); err != nil {
		t.Fatalf("status upgrade: %v", err)
	}
	if !strings.Contains(statusOut.String(), "paused") || !strings.Contains(statusOut.String(), "master-a") {
		t.Fatalf("status did not read persisted state:\n%s", statusOut.String())
	}

	if err := runUpgradeResumeCommand(upgradeStateOptions{configDir: configDir, stdout: &bytes.Buffer{}}); err != nil {
		t.Fatalf("resume upgrade: %v", err)
	}
	resumed, err := loadUpgradeState(configDir)
	if err != nil {
		t.Fatalf("load resumed state: %v", err)
	}
	if resumed.Paused || resumed.Status != upgradeStatusRunning {
		t.Fatalf("resume did not persist running state: %+v", resumed)
	}
}

func TestRunUpgradeStatusNoState(t *testing.T) {
	var out bytes.Buffer
	err := runUpgradeStatusCommand(upgradeStateOptions{
		configDir: t.TempDir(),
		stdout:    &out,
	})
	if err != nil {
		t.Fatalf("status without state: %v", err)
	}
	if !strings.Contains(out.String(), "No upgrade state") {
		t.Fatalf("unexpected status output: %q", out.String())
	}
}

func writeUpgradeTopology(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "topology.yml")
	data := []byte(`
schema_version: 2
mesh:
  name: upgrade-test
  overlay_supernet: 172.21.92.0/24
nodes:
  - name: client-z
    roles: [client]
    overlay_ip: 172.21.92.130
  - name: ingress-b
    roles: [ingress]
    overlay_ip: 172.21.92.20
  - name: master-a
    roles: [master, balancer]
    overlay_ip: 172.21.92.2
  - name: egress-a
    roles: [egress]
    overlay_ip: 172.21.92.34
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write topology: %v", err)
	}
	return path
}

func assertTextOrder(t *testing.T, text string, ordered ...string) {
	t.Helper()
	last := -1
	for _, value := range ordered {
		idx := strings.Index(text, value)
		if idx < 0 {
			t.Fatalf("output missing %q:\n%s", value, text)
		}
		if idx <= last {
			t.Fatalf("%q appeared out of order in:\n%s", value, text)
		}
		last = idx
	}
}
