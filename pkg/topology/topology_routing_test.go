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
    type: mikrotik
    overlay_ip: 172.20.70.131
    masters: [ru-01]
    routing_policies:
      - name: vpn-kz
        dscp: 10
        targets: [kz-01]
      - name: vpn-ru-direct
        dscp: 50
        targets: [ru-01]
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
	master := topo.FindMaster("ru-01")
	if master == nil {
		t.Fatal("master ru-01 not found")
	}
	if !master.Exit {
		t.Error("master ru-01 should have exit=true")
	}

	// Verify client routing policies
	client := topo.FindClient("home-router")
	if client == nil {
		t.Fatal("client home-router not found")
	}
	if len(client.RoutingPolicies) != 2 {
		t.Fatalf("expected 2 routing policies, got %d", len(client.RoutingPolicies))
	}

	policy := client.RoutingPolicies[0]
	if policy.Name != "vpn-kz" {
		t.Errorf("expected policy name vpn-kz, got %s", policy.Name)
	}
	if policy.DSCP != 10 {
		t.Errorf("expected DSCP 10, got %d", policy.DSCP)
	}
	if len(policy.Targets) != 1 || policy.Targets[0] != "kz-01" {
		t.Errorf("expected targets [kz-01], got %v", policy.Targets)
	}

	policy2 := client.RoutingPolicies[1]
	if policy2.Name != "vpn-ru-direct" {
		t.Errorf("expected policy name vpn-ru-direct, got %s", policy2.Name)
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
	path := filepath.Join(dir, "topology.yml")
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatalf("write topology file: %v", err)
	}

	topo, err := LoadTopology(path)
	if err != nil {
		t.Fatalf("LoadTopology: %v", err)
	}

	master := topo.FindMaster("ru-01")
	if master == nil {
		t.Fatal("master ru-01 not found")
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
  - name: ru-01
    host: 1.2.3.4
    overlay_ip: 172.20.70.10
    listen_port: 51820
    endpoints: []
clients:
  - name: basic-client
    type: generic
    overlay_ip: 172.20.70.131
    masters: [ru-01]
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
