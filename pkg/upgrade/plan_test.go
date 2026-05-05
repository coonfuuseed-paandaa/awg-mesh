package upgrade

import (
	"testing"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/topology"
)

// ─── helpers ────────────────────────────────────────────────────────────────

func makeTopo(masters []string, endpoints []topology.EndpointNode) *topology.Topology {
	ms := make([]topology.MasterNode, len(masters))
	for i, name := range masters {
		ms[i] = topology.MasterNode{Name: name, Host: name + ".example.com", GRPCPort: 9090}
	}
	return &topology.Topology{
		Masters:   ms,
		Endpoints: endpoints,
	}
}

func ep(name, region string) topology.EndpointNode {
	return topology.EndpointNode{Name: name, Region: region, Host: name + ".example.com", GRPCPort: 9090}
}

// ─── ComputeOrder ───────────────────────────────────────────────────────────

func TestComputeOrder_DefaultOrder(t *testing.T) {
	topo := makeTopo(
		[]string{"master-b", "master-a"},
		[]topology.EndpointNode{
			ep("ep-us-2", "us"),
			ep("ep-eu-1", "eu"),
			ep("ep-us-1", "us"),
		},
	)

	got, err := ComputeOrder(topo, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Expected: endpoints first by region then name (eu before us), masters alphabetical last.
	want := []string{"ep-eu-1", "ep-us-1", "ep-us-2", "master-a", "master-b"}
	if len(got) != len(want) {
		t.Fatalf("length mismatch: got %d want %d — %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("position %d: got %q want %q", i, got[i], w)
		}
	}
}

func TestComputeOrder_NoEndpoints(t *testing.T) {
	topo := makeTopo([]string{"m2", "m1"}, nil)
	got, err := ComputeOrder(topo, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"m1", "m2"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestComputeOrder_NoMasters(t *testing.T) {
	topo := makeTopo(nil, []topology.EndpointNode{ep("ep-b", "eu"), ep("ep-a", "eu")})
	got, err := ComputeOrder(topo, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"ep-a", "ep-b"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestComputeOrder_ManualOverride_Valid(t *testing.T) {
	topo := makeTopo([]string{"m1"}, []topology.EndpointNode{ep("ep-1", "us")})
	override := []string{"m1", "ep-1"}
	got, err := ComputeOrder(topo, override)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "m1" || got[1] != "ep-1" {
		t.Errorf("got %v", got)
	}
}

func TestComputeOrder_ManualOverride_UnknownNode(t *testing.T) {
	topo := makeTopo([]string{"m1"}, nil)
	_, err := ComputeOrder(topo, []string{"m1", "ghost"})
	if err == nil {
		t.Fatal("expected error for unknown node, got nil")
	}
}

func TestComputeOrder_ManualOverride_Duplicate(t *testing.T) {
	topo := makeTopo([]string{"m1"}, nil)
	_, err := ComputeOrder(topo, []string{"m1", "m1"})
	if err == nil {
		t.Fatal("expected error for duplicate node, got nil")
	}
}

func TestComputeOrder_NilTopology(t *testing.T) {
	_, err := ComputeOrder(nil, nil)
	if err == nil {
		t.Fatal("expected error for nil topology, got nil")
	}
}

// ─── ComputePlan ────────────────────────────────────────────────────────────

func TestComputePlan_Basic(t *testing.T) {
	topo := makeTopo(
		[]string{"m1"},
		[]topology.EndpointNode{ep("ep-1", "us")},
	)
	topo.Defaults.Image.Node = "ghcr.io/example/awg-mesh-node:v1.9.0"

	plan, err := ComputePlan(topo, "v1.10.2", PlanOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.Version != "v1.10.2" {
		t.Errorf("version: got %q", plan.Version)
	}
	if len(plan.Nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(plan.Nodes))
	}

	// First node should be the endpoint (endpoints-first order).
	if plan.Nodes[0].Name != "ep-1" {
		t.Errorf("first node: got %q want ep-1", plan.Nodes[0].Name)
	}
	if plan.Nodes[0].Role != "endpoint" {
		t.Errorf("ep role: got %q", plan.Nodes[0].Role)
	}
	if plan.Nodes[0].Region != "us" {
		t.Errorf("ep region: got %q", plan.Nodes[0].Region)
	}
	if plan.Nodes[0].Status != StatusPlanned {
		t.Errorf("ep status: got %q", plan.Nodes[0].Status)
	}

	// Second node should be the master.
	if plan.Nodes[1].Name != "m1" {
		t.Errorf("second node: got %q want m1", plan.Nodes[1].Name)
	}
	if plan.Nodes[1].Role != "master" {
		t.Errorf("master role: got %q", plan.Nodes[1].Role)
	}

	// NewImage must strip the existing tag and append the target version.
	wantImg := "ghcr.io/example/awg-mesh-node:v1.10.2"
	if plan.Nodes[0].NewImage != wantImg {
		t.Errorf("NewImage: got %q want %q", plan.Nodes[0].NewImage, wantImg)
	}
}

func TestComputePlan_FallbackImage(t *testing.T) {
	topo := makeTopo([]string{"m1"}, nil)
	// No Defaults.Image.Node set — should use defaultFallbackImage.
	plan, err := ComputePlan(topo, "v1.10.2", PlanOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantImg := defaultFallbackImage + ":v1.10.2"
	if plan.Nodes[0].NewImage != wantImg {
		t.Errorf("fallback image: got %q want %q", plan.Nodes[0].NewImage, wantImg)
	}
}

func TestComputePlan_OldImageAlwaysUnknown(t *testing.T) {
	topo := makeTopo([]string{"m1"}, nil)
	plan, err := ComputePlan(topo, "v1.10.2", PlanOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.Nodes[0].OldImage != "UNKNOWN" {
		t.Errorf("OldImage: got %q want UNKNOWN", plan.Nodes[0].OldImage)
	}
}

func TestComputePlan_NilTopology(t *testing.T) {
	_, err := ComputePlan(nil, "v1.10.2", PlanOptions{})
	if err == nil {
		t.Fatal("expected error for nil topology")
	}
}

func TestComputePlan_EmptyVersion(t *testing.T) {
	topo := makeTopo([]string{"m1"}, nil)
	_, err := ComputePlan(topo, "   ", PlanOptions{})
	if err == nil {
		t.Fatal("expected error for blank version")
	}
}

func TestComputePlan_ManualOrder(t *testing.T) {
	topo := makeTopo([]string{"m1"}, []topology.EndpointNode{ep("ep-1", "us")})
	plan, err := ComputePlan(topo, "v1.10.2", PlanOptions{ManualOrder: []string{"m1", "ep-1"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.Nodes[0].Name != "m1" {
		t.Errorf("manual order: first node should be m1, got %q", plan.Nodes[0].Name)
	}
}

// ─── stripImageTag ───────────────────────────────────────────────────────────

func TestStripImageTag(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"ghcr.io/org/img:v1.2.3", "ghcr.io/org/img"},
		{"ghcr.io/org/img", "ghcr.io/org/img"},
		{"ghcr.io/org/img@sha256:abc123", "ghcr.io/org/img"},
		{"img:latest", "img"},
		{"localhost:5000/img:tag", "localhost:5000/img"},
	}
	for _, tc := range cases {
		got := stripImageTag(tc.input)
		if got != tc.want {
			t.Errorf("stripImageTag(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
