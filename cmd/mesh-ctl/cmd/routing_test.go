package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thebtf/awg-mesh/pkg/topology"
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
  - name: ru-01
    host: 1.2.3.4
    overlay_ip: 172.20.70.10
    listen_port: 51820
    endpoints: [kz-01]
endpoints:
  - name: kz-01
    host: 5.6.7.8
    overlay_ip: 172.20.70.34
    listen_port: 51820
    region: kz
  - name: us-01
    host: 9.10.11.12
    overlay_ip: 172.20.70.38
    listen_port: 51820
    region: us
clients:
  - name: home-router
    type: mikrotik
    overlay_ip: 172.20.70.131
    masters: [ru-01]
    routing_policies:
      - name: vpn-kz
        dscp: 10
        targets: [kz-01]
      - name: vpn-us
        dscp: 20
        targets: [us-01]
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
				"--client", "home-router",
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
			{Name: "vpn-kz", DSCP: 10, Targets: []string{"kz-01"}},
			{Name: "vpn-us", DSCP: 20, Targets: []string{"us-01"}},
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
  - name: ru-01
    host: 1.2.3.4
    overlay_ip: 172.20.70.10
    listen_port: 51820
    endpoints: [kz-01]
    exit: true
endpoints:
  - name: kz-01
    host: 5.6.7.8
    overlay_ip: 172.20.70.34
    listen_port: 51820
    region: kz
clients:
  - name: home-router
    type: generic
    overlay_ip: 172.20.70.131
    masters: [ru-01]
    routing_policies:
      - name: vpn-kz
        dscp: 10
        targets: [kz-01]
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
		"--client", "home-router",
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
	if !strings.Contains(jsonOutput, "kz-01") {
		t.Error("generic JSON should reference endpoint kz-01")
	}
}

func TestRoutingGenerateNoClient(t *testing.T) {
	yaml := `
overlay:
  space: "172.20.0.0/16"
masters:
  - name: ru-01
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
