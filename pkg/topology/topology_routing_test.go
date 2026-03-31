package topology

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTopologyRoutingPoliciesParsing(t *testing.T) {
	yaml := `
overlay:
  space: "172.20.0.0/16"
  physical_mtu: 1500
  awg_overhead: 96
  ranges:
    - name: default
      cidr: "172.20.70.0/24"
      balancer_ip: "172.20.70.1"

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
    type: mikrotik
    overlay_ip: 172.20.70.131
    masters: [master-01]
    routing_policies:
      - name: vpn-asia
        dscp: 10
        targets: [node-asia-01]
      - name: vpn-direct
        dscp: 50
        targets: [master-01]
    dns:
      zone: mesh.zone
      listen: "0.0.0.0:53"
      upstream: "1.1.1.1"

transport:
  pool: "10.200.0.0/16"
  prefix_length: 30
`

	dir := t.TempDir()
	path := filepath.Join(dir, "topology.yml")
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatalf("write topology file: %v", err)
	}

	topo, err := LoadTopology(path)
	if err != nil {
		t.Fatalf("LoadTopology: %v", err)
	}

	// Verify master exit field
	master := topo.FindMaster("master-01")
	if master == nil {
		t.Fatal("master master-01 not found")
	}
	if !master.Exit {
		t.Error("master master-01 should have exit=true")
	}

	// Verify client routing policies
	client := topo.FindClient("my-router")
	if client == nil {
		t.Fatal("client my-router not found")
	}
	if len(client.RoutingPolicies) != 2 {
		t.Fatalf("expected 2 routing policies, got %d", len(client.RoutingPolicies))
	}

	policy := client.RoutingPolicies[0]
	if policy.Name != "vpn-asia" {
		t.Errorf("expected policy name vpn-asia, got %s", policy.Name)
	}
	if policy.DSCP != 10 {
		t.Errorf("expected DSCP 10, got %d", policy.DSCP)
	}
	if len(policy.Targets) != 1 || policy.Targets[0] != "node-asia-01" {
		t.Errorf("expected targets [node-asia-01], got %v", policy.Targets)
	}

	policy2 := client.RoutingPolicies[1]
	if policy2.Name != "vpn-direct" {
		t.Errorf("expected policy name vpn-direct, got %s", policy2.Name)
	}
	if policy2.DSCP != 50 {
		t.Errorf("expected DSCP 50, got %d", policy2.DSCP)
	}

	// Verify DNS config
	if client.DNS == nil {
		t.Fatal("client DNS config is nil")
	}
	if client.DNS.Zone != "mesh.zone" {
		t.Errorf("expected DNS zone mesh.zone, got %s", client.DNS.Zone)
	}
	if client.DNS.Listen != "0.0.0.0:53" {
		t.Errorf("expected DNS listen 0.0.0.0:53, got %s", client.DNS.Listen)
	}
	if client.DNS.Upstream != "1.1.1.1" {
		t.Errorf("expected DNS upstream 1.1.1.1, got %s", client.DNS.Upstream)
	}
}

func TestTopologyMasterExitDefault(t *testing.T) {
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
	path := filepath.Join(dir, "topology.yml")
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatalf("write topology file: %v", err)
	}

	topo, err := LoadTopology(path)
	if err != nil {
		t.Fatalf("LoadTopology: %v", err)
	}

	master := topo.FindMaster("master-01")
	if master == nil {
		t.Fatal("master master-01 not found")
	}
	if master.Exit {
		t.Error("master exit should default to false")
	}
}

func TestTopologyClientWithoutRoutingPolicies(t *testing.T) {
	yaml := `
overlay:
  space: "172.20.0.0/16"
masters:
  - name: master-01
    host: 1.2.3.4
    overlay_ip: 172.20.70.10
    listen_port: 51820
    endpoints: []
clients:
  - name: basic-client
    type: generic
    overlay_ip: 172.20.70.131
    masters: [master-01]
transport:
  pool: "10.200.0.0/16"
  prefix_length: 30
`
	dir := t.TempDir()
	path := filepath.Join(dir, "topology.yml")
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatalf("write topology file: %v", err)
	}

	topo, err := LoadTopology(path)
	if err != nil {
		t.Fatalf("LoadTopology: %v", err)
	}

	client := topo.FindClient("basic-client")
	if client == nil {
		t.Fatal("client basic-client not found")
	}
	if len(client.RoutingPolicies) != 0 {
		t.Errorf("expected 0 routing policies, got %d", len(client.RoutingPolicies))
	}
	if client.DNS != nil {
		t.Error("expected nil DNS config for basic client")
	}
}
