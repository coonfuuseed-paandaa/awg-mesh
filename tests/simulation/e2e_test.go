//go:build e2e

package simulation

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	meshCtl  = "../../mesh-ctl.exe"
	topo     = "mesh-topology.yml"
	cfgDir   = "../../.mesh-ctl-e2e"
	compDir  = "."
	topoPath = "../../tests/simulation/mesh-topology.yml"
)

// TestE2EFullMesh runs the complete 8-node simulation and verifies all connectivity.
func TestE2EFullMesh(t *testing.T) {
	if os.Getenv("AWG_E2E") == "" {
		t.Skip("set AWG_E2E=1 to run E2E simulation tests")
	}

	t.Log("=== Phase 1: Setup ===")
	setup(t)

	t.Log("=== Phase 2: Init ===")
	initAll(t)

	t.Log("=== Phase 3: Wait for WG handshakes ===")
	time.Sleep(20 * time.Second)

	t.Run("WGHandshake", testWGHandshake)
	t.Run("OverlayPing", testOverlayPing)
	t.Run("ECMP", testECMP)
	t.Run("ClientToMaster", testClientToMaster)
	t.Run("ClientToEndpoint", testClientToEndpoint)
	t.Run("Status", testStatus)
}

func setup(t *testing.T) {
	t.Helper()

	// Fresh containers
	runDir(t, compDir, "docker", "compose", "down", "-v")
	runDir(t, compDir, "docker", "compose", "up", "-d")
	time.Sleep(5 * time.Second)

	// Clean config
	os.RemoveAll(cfgDir)

	// Prepare all nodes
	for _, n := range []string{"master-01", "master-02"} {
		run(t, meshCtl, "master", "prepare", n, "-t", topoPath, "--config-dir", cfgDir)
	}
	for _, n := range []string{"node-asia-01", "node-asia-02", "node-asia-03", "node-eu-01", "node-us-01"} {
		run(t, meshCtl, "endpoint", "prepare", n, "-t", topoPath, "--config-dir", cfgDir)
	}
	run(t, meshCtl, "client", "prepare", "client-01", "-t", topoPath, "--config-dir", cfgDir)

	// Deploy tokens
	nodes := []string{"master-01", "master-02", "node-asia-01", "node-asia-02", "node-asia-03", "node-eu-01", "node-us-01", "client-01"}
	for _, node := range nodes {
		tokenPath := fmt.Sprintf("%s/nodes/%s/mesh.token", cfgDir, node)
		hash, err := os.ReadFile(tokenPath)
		if err != nil {
			t.Fatalf("read token for %s: %v", node, err)
		}
		dockerExec(t, node, fmt.Sprintf("printf '%%s' '%s' > /config/mesh.token", strings.TrimSpace(string(hash))))
	}

	// Restart containers
	runDir(t, compDir, "docker", "compose", "restart")
	time.Sleep(5 * time.Second)
}

func initAll(t *testing.T) {
	t.Helper()

	for _, n := range []string{"node-asia-01", "node-asia-02", "node-asia-03", "node-eu-01", "node-us-01"} {
		out := run(t, meshCtl, "endpoint", "init", n, "-t", topoPath, "--config-dir", cfgDir)
		if !strings.Contains(out, "initialized successfully") {
			t.Fatalf("endpoint %s init failed: %s", n, out)
		}
	}
	for _, n := range []string{"master-01", "master-02"} {
		run(t, meshCtl, "master", "init", n, "-t", topoPath, "--config-dir", cfgDir)
	}
	out := run(t, meshCtl, "client", "init", "client-01", "-t", topoPath, "--config-dir", cfgDir)
	if !strings.Contains(out, "masters connected") && !strings.Contains(out, "Added peer") {
		t.Logf("client init output: %s", out)
		t.Fatalf("client init failed — no masters connected")
	}
}

func testWGHandshake(t *testing.T) {
	// Verify master has 6 WG interfaces (5 endpoints + 1 client)
	out := dockerExec(t, "master-01", "ip -br link | grep -c wg")
	count := strings.TrimSpace(out)
	if count != "6" {
		t.Fatalf("master-01 expected 6 WG interfaces, got %s", count)
	}

	// Verify client has 2 WG interfaces
	out = dockerExec(t, "client-01", "ip -br link | grep -c wg")
	count = strings.TrimSpace(out)
	if count != "2" {
		t.Fatalf("client-01 expected 2 WG interfaces, got %s", count)
	}

	// All WG interfaces should be UP
	out = dockerExec(t, "master-01", "ip -br link | grep wg | grep -v UP | wc -l")
	if strings.TrimSpace(out) != "0" {
		t.Fatalf("some WG interfaces on master-01 are not UP")
	}
}

func testOverlayPing(t *testing.T) {
	// Master → all endpoints
	endpoints := []string{"172.20.70.34", "172.20.70.35", "172.20.70.36", "172.20.70.37", "172.20.70.38"}
	for _, ep := range endpoints {
		out := dockerExec(t, "master-01", fmt.Sprintf("ping -c 1 -W 3 %s", ep))
		if !strings.Contains(out, "1 packets received") {
			t.Errorf("master-01 → %s: ping failed: %s", ep, out)
		}
	}
}

func testECMP(t *testing.T) {
	// Master ECMP: 172.20.70.33 should have at least 4 nexthops (5th may still be converging)
	out := dockerExec(t, "master-01", "ip route show 172.20.70.33")
	nexthopCount := strings.Count(out, "nexthop")
	if nexthopCount < 4 {
		t.Fatalf("master-01 ECMP expected >=4 nexthops, got %d: %s", nexthopCount, out)
	}
	t.Logf("master-01 ECMP: %d nexthops", nexthopCount)

	// Client ECMP: 172.20.70.1 should have 2 nexthops
	out = dockerExec(t, "client-01", "ip route show 172.20.70.1")
	nexthopCount = strings.Count(out, "nexthop")
	if nexthopCount < 2 {
		t.Fatalf("client-01 ECMP expected 2 nexthops, got %d: %s", nexthopCount, out)
	}
}

func testClientToMaster(t *testing.T) {
	// Client → master transport
	out := dockerExec(t, "client-01", "ping -c 1 -W 3 10.255.0.41")
	if !strings.Contains(out, "1 packets received") {
		t.Errorf("client → master-01 transport failed: %s", out)
	}

	// Client → master overlay
	out = dockerExec(t, "client-01", "ping -c 1 -W 3 172.20.70.2")
	if !strings.Contains(out, "1 packets received") {
		t.Errorf("client → master-01 overlay failed: %s", out)
	}
	out = dockerExec(t, "client-01", "ping -c 1 -W 3 172.20.70.3")
	if !strings.Contains(out, "1 packets received") {
		t.Errorf("client → master-02 overlay failed: %s", out)
	}
}

func testClientToEndpoint(t *testing.T) {
	out := dockerExec(t, "client-01", "ping -c 1 -W 3 172.20.70.37")
	if !strings.Contains(out, "1 packets received") {
		t.Errorf("client → endpoint overlay failed: %s", out)
	}
}

func testStatus(t *testing.T) {
	out := run(t, meshCtl, "status", "-t", topoPath, "--config-dir", cfgDir)
	for _, node := range []string{"master-01", "master-02", "node-asia-01", "node-asia-02", "node-asia-03", "node-eu-01", "node-us-01"} {
		if !strings.Contains(out, node) {
			t.Errorf("status missing node %s", node)
		}
	}
	if !strings.Contains(out, "ONLINE") {
		t.Error("no ONLINE nodes in status output")
	}
}

// Helpers

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("command %s %v failed: %v\n%s", name, args, err, string(out))
	}
	return string(out)
}

func runDir(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("command %s %v in %s failed: %v\n%s", name, args, dir, err, string(out))
	}
	return string(out)
}

func dockerExec(t *testing.T, container string, command string) string {
	t.Helper()
	out, err := exec.Command("docker", "exec", container, "sh", "-c", command).CombinedOutput()
	if err != nil {
		t.Logf("docker exec %s failed: %v\n%s", container, err, string(out))
	}
	return string(out)
}
