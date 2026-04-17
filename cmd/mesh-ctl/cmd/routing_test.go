package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
)

func TestRoutingGenerateCommand(t *testing.T) {
	yaml := `
overlay:
  space: "172.20.0.0/16"
  physical_mtu: 1500
  awg_overhead: 96
  ranges:
    - name: default
      cidr: "172.20.70.0/24"
masters:
  - name: master-01
    host: 1.2.3.4
    overlay_ip: 172.20.70.10
    listen_port: 51820
    endpoints: [node-asia-01]
endpoints:
  - name: node-asia-01
    host: 5.6.7.8
    overlay_ip: 172.20.70.34
    listen_port: 51820
    region: asia
  - name: node-us-01
    host: 9.10.11.12
    overlay_ip: 172.20.70.38
    listen_port: 51820
    region: americas
clients:
  - name: my-router
    type: mikrotik
    overlay_ip: 172.20.70.131
    masters: [master-01]
    routing_policies:
      - name: vpn-asia
        dscp: 10
        targets: [node-asia-01]
      - name: vpn-americas
        dscp: 20
        targets: [node-us-01]
transport:
  pool: "10.200.0.0/16"
  prefix_length: 30
`

	dir := t.TempDir()
	topoPath := filepath.Join(dir, "topology.yml")
	if err := os.WriteFile(topoPath, []byte(yaml), 0644); err != nil {
		t.Fatalf("write topology: %v", err)
	}

	platforms := []string{"mikrotik", "linux", "generic"}
	for _, platform := range platforms {
		t.Run(platform, func(t *testing.T) {
			root := NewRootCommand("test")
			root.SetArgs([]string{"routing", "generate",
				"--platform", platform,
				"--client", "my-router",
				"-t", topoPath,
			})
			if err := root.Execute(); err != nil {
				t.Errorf("routing generate --%s failed: %v", platform, err)
			}
		})
	}
}

func TestLinuxRoutingGenerate(t *testing.T) {
	client := topology.ClientNode{
		Name:      "test-router",
		OverlayIP: "172.20.70.131",
		RoutingPolicies: []topology.RoutingPolicy{
			{Name: "vpn-asia", DSCP: 10, Targets: []string{"node-asia-01"}},
			{Name: "vpn-americas", DSCP: 20, Targets: []string{"node-us-01"}},
		},
	}

	var buf strings.Builder
	origStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := generateLinux(client, "172.33.23.100")
	w.Close()
	os.Stdout = origStdout

	if err != nil {
		t.Fatalf("generateLinux: %v", err)
	}

	output := make([]byte, 4096)
	n, _ := r.Read(output)
	buf.Write(output[:n])
	script := buf.String()

	if !strings.Contains(script, "iptables -t mangle") {
		t.Error("Linux script should contain iptables mangle rule")
	}
	if !strings.Contains(script, "ip rule add fwmark 10 lookup 110") {
		t.Error("Linux script should contain ip rule for fwmark 10 lookup table 110")
	}
	if !strings.Contains(script, "ip route replace default via 172.33.23.100 table 110") {
		t.Error("Linux script should contain route for numeric table 110 (100+DSCP)")
	}
	if !strings.Contains(script, "set -euo pipefail") {
		t.Error("Linux script should be idempotent with set -euo pipefail")
	}
}

func TestGenericRoutingFallback(t *testing.T) {
	yaml := `
overlay:
  space: "172.20.0.0/16"
masters:
  - name: master-01
    host: 1.2.3.4
    overlay_ip: 172.20.70.10
    listen_port: 51820
    endpoints: [node-asia-01]
    exit: true
endpoints:
  - name: node-asia-01
    host: 5.6.7.8
    overlay_ip: 172.20.70.34
    listen_port: 51820
    region: asia
clients:
  - name: my-router
    type: generic
    overlay_ip: 172.20.70.131
    masters: [master-01]
    routing_policies:
      - name: vpn-asia
        dscp: 10
        targets: [node-asia-01]
transport:
  pool: "10.200.0.0/16"
  prefix_length: 30
`
	dir := t.TempDir()
	topoPath := filepath.Join(dir, "topology.yml")
	if err := os.WriteFile(topoPath, []byte(yaml), 0644); err != nil {
		t.Fatalf("write topology: %v", err)
	}

	root := NewRootCommand("test")

	// Capture stdout
	origStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	root.SetArgs([]string{"routing", "generate",
		"--platform", "generic",
		"--client", "my-router",
		"-t", topoPath,
	})
	err := root.Execute()
	w.Close()
	os.Stdout = origStdout

	if err != nil {
		t.Fatalf("routing generate --generic failed: %v", err)
	}

	output := make([]byte, 8192)
	n, _ := r.Read(output)
	jsonOutput := string(output[:n])

	if !strings.Contains(jsonOutput, "fallback_routes") {
		t.Error("generic JSON should contain fallback_routes")
	}
	if !strings.Contains(jsonOutput, "172.20.70.34") {
		t.Error("generic JSON should contain endpoint overlay IP 172.20.70.34")
	}
	if !strings.Contains(jsonOutput, "172.20.70.10") {
		t.Error("generic JSON should contain exit master overlay IP 172.20.70.10")
	}
	if !strings.Contains(jsonOutput, "node-asia-01") {
		t.Error("generic JSON should reference endpoint node-asia-01")
	}
}

func TestLinuxRoutingGenerate_RejectsOutOfRangeDSCP(t *testing.T) {
	t.Parallel()

	client := topology.ClientNode{
		Name:      "bad-router",
		OverlayIP: "172.20.70.131",
		RoutingPolicies: []topology.RoutingPolicy{
			{Name: "bad-policy", DSCP: 153, Targets: []string{"node-asia-01"}},
		},
	}

	err := generateLinux(client, "172.33.23.100")
	if err == nil {
		t.Fatal("generateLinux with DSCP=153: expected non-nil error, got nil")
	}

	errText := strings.ToLower(err.Error())
	if !strings.Contains(errText, "dscp") {
		t.Errorf("error message should reference 'dscp', got: %v", err)
	}
	if !strings.Contains(err.Error(), "1..63") {
		t.Errorf("error message should reference '1..63', got: %v", err)
	}
}

func TestRoutingGenerateNoClient(t *testing.T) {
	yaml := `
overlay:
  space: "172.20.0.0/16"
masters:
  - name: master-01
    host: 1.2.3.4
    overlay_ip: 172.20.70.10
    listen_port: 51820
    endpoints: []
transport:
  pool: "10.200.0.0/16"
  prefix_length: 30
`
	dir := t.TempDir()
	topoPath := filepath.Join(dir, "topology.yml")
	if err := os.WriteFile(topoPath, []byte(yaml), 0644); err != nil {
		t.Fatalf("write topology: %v", err)
	}

	root := NewRootCommand("test")
	root.SetArgs([]string{"routing", "generate", "-t", topoPath})
	if err := root.Execute(); err == nil {
		t.Error("expected error for topology with no clients")
	}
}
