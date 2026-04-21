package mikrotik

import (
	"strings"
	"testing"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
)

func TestGenerateRoutingRSC(t *testing.T) {
	client := topology.ClientNode{
		Name:      "my-router",
		Type:      "mikrotik",
		OverlayIP: "172.20.70.131",
		Masters:   []string{"master-01"},
		RoutingPolicies: []topology.RoutingPolicy{
			{Name: "vpn-asia", DSCP: 10, Targets: []string{"node-asia-01"}},
			{Name: "vpn-americas", DSCP: 20, Targets: []string{"node-us-01"}},
		},
	}

	// Pass nil topo — targets unresolvable, falls back to fallbackGateway.
	script, err := GenerateRoutingRSC(client, "172.33.23.100", nil)
	if err != nil {
		t.Fatalf("GenerateRoutingRSC: %v", err)
	}

	if !strings.Contains(script, "new-dscp=10") {
		t.Error("script should contain new-dscp=10")
	}
	if !strings.Contains(script, "new-dscp=20") {
		t.Error("script should contain new-dscp=20")
	}
	if !strings.Contains(script, "vpn-asia-conn") {
		t.Error("script should contain connection mark vpn-asia-conn")
	}
	if !strings.Contains(script, "gateway=172.33.23.100") {
		t.Error("script should contain fallback gateway address when topo=nil")
	}
	if !strings.Contains(script, "my-router") {
		t.Error("script should contain client name")
	}

	// B11 fix: each policy now gets its own routing table (vpn-mesh-<dscp>).
	if !strings.Contains(script, "vpn-mesh-10") {
		t.Error("script should contain per-DSCP routing table vpn-mesh-10")
	}
	if !strings.Contains(script, "vpn-mesh-20") {
		t.Error("script should contain per-DSCP routing table vpn-mesh-20")
	}

	// B11 fix: /routing/rule entries must be present.
	if !strings.Contains(script, "/routing/rule") {
		t.Error("script should contain /routing/rule entries")
	}

	// B11 fix: old single shared table must NOT appear.
	if strings.Contains(script, "add name=vpn-mesh fib") {
		t.Error("script must not create a single shared vpn-mesh table (B11 regression)")
	}

	// Existing regression guards.
	if !strings.Contains(script, "Classifier rules required") {
		t.Error("script should surface the classifier-required banner")
	}
	if !strings.Contains(script, "CLASSIFIER TEMPLATE") {
		t.Error("script should include the commented classifier template block")
	}
	if !strings.Contains(script, "action=mark-connection") {
		t.Error("script should include mark-connection examples in the classifier template")
	}
	if !strings.Contains(script, "new-connection-mark=vpn-asia-conn") {
		t.Error("script should reference vpn-asia-conn in classifier template")
	}
	if !strings.Contains(script, "new-connection-mark=vpn-americas-conn") {
		t.Error("script should reference vpn-americas-conn in classifier template")
	}
}

// TestGenerateRoutingRSCPerDSCPTablesWithTopology verifies B11 fix:
// when a topology is provided, each policy routes to its target endpoint's
// overlay IP (not a single shared gateway and not the client self-IP).
func TestGenerateRoutingRSCPerDSCPTablesWithTopology(t *testing.T) {
	topo := &topology.Topology{
		Endpoints: []topology.EndpointNode{
			{Name: "ep-pl-01", OverlayIP: "172.20.70.34"},
			{Name: "ep-kz-01", OverlayIP: "172.20.70.36"},
		},
	}
	client := topology.ClientNode{
		Name:      "my-router",
		OverlayIP: "172.20.70.131",
		RoutingPolicies: []topology.RoutingPolicy{
			{Name: "vpn-pl", DSCP: 10, Targets: []string{"ep-pl-01"}},
			{Name: "vpn-kz", DSCP: 20, Targets: []string{"ep-kz-01"}},
		},
	}

	script, err := GenerateRoutingRSC(client, "172.0.0.1", topo)
	if err != nil {
		t.Fatalf("GenerateRoutingRSC: %v", err)
	}

	// Each policy must have its own routing table.
	if !strings.Contains(script, "vpn-mesh-10") {
		t.Error("expected routing table vpn-mesh-10 for DSCP 10")
	}
	if !strings.Contains(script, "vpn-mesh-20") {
		t.Error("expected routing table vpn-mesh-20 for DSCP 20")
	}

	// Each table must route to DIFFERENT gateways (the resolved targets).
	if !strings.Contains(script, "gateway=172.20.70.34") {
		t.Error("expected gateway 172.20.70.34 for ep-pl-01 (DSCP 10 policy)")
	}
	if !strings.Contains(script, "gateway=172.20.70.36") {
		t.Error("expected gateway 172.20.70.36 for ep-kz-01 (DSCP 20 policy)")
	}

	// Must NOT use the fallback gateway when targets are resolvable.
	if strings.Contains(script, "gateway=172.0.0.1") {
		t.Error("fallback gateway should not appear when targets are resolved from topology")
	}

	// Must NOT use the client self-IP as gateway (circular routing).
	if strings.Contains(script, "gateway=172.20.70.131") {
		t.Error("client self-IP must not be used as gateway (B11: circular routing)")
	}

	// Must have /routing/rule entries.
	if !strings.Contains(script, "/routing/rule") {
		t.Error("expected /routing/rule entries")
	}
}

func TestGenerateRoutingRSCErrors(t *testing.T) {
	tests := []struct {
		name    string
		client  topology.ClientNode
		gateway string
	}{
		{"empty gateway and no topology", topology.ClientNode{Name: "c1", RoutingPolicies: []topology.RoutingPolicy{{Name: "p", DSCP: 10}}}, ""},
		{"no policies", topology.ClientNode{Name: "c1"}, "1.2.3.4"},
		{"dscp zero", topology.ClientNode{Name: "c1", RoutingPolicies: []topology.RoutingPolicy{{Name: "p", DSCP: 0}}}, "1.2.3.4"},
		{"dscp 64", topology.ClientNode{Name: "c1", RoutingPolicies: []topology.RoutingPolicy{{Name: "p", DSCP: 64}}}, "1.2.3.4"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := GenerateRoutingRSC(tt.client, tt.gateway, nil)
			if err == nil {
				t.Error("expected error")
			}
		})
	}
}
