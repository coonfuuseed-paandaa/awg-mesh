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

	script, err := GenerateRoutingRSC(client, "172.33.23.100")
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
		t.Error("script should contain gateway address")
	}
	if !strings.Contains(script, "my-router") {
		t.Error("script should contain client name")
	}

	// Bug 11 regression: the script previously emitted only change-dscp
	// rules (which run on already-marked traffic), with nothing that
	// actually marks traffic in the first place. Result: DSCP routing was
	// a silent no-op on every deployment.
	//
	// The generator now emits a header banner explaining the requirement
	// and a commented classifier template block so the operator has a
	// ready-to-edit starting point.
	if !strings.Contains(script, "Classifier rules required") {
		t.Error("script should surface the classifier-required banner")
	}
	if !strings.Contains(script, "CLASSIFIER TEMPLATE") {
		t.Error("script should include the commented classifier template block")
	}
	if !strings.Contains(script, "action=mark-connection") {
		t.Error("script should include mark-connection examples in the classifier template")
	}
	// Each policy must get its own commented starter block so the operator
	// cannot confuse one policy for another.
	if !strings.Contains(script, "new-connection-mark=vpn-asia-conn") {
		t.Error("script should reference vpn-asia-conn in classifier template")
	}
	if !strings.Contains(script, "new-connection-mark=vpn-americas-conn") {
		t.Error("script should reference vpn-americas-conn in classifier template")
	}
}

func TestGenerateRoutingRSCErrors(t *testing.T) {
	tests := []struct {
		name    string
		client  topology.ClientNode
		gateway string
	}{
		{"empty gateway", topology.ClientNode{Name: "c1", RoutingPolicies: []topology.RoutingPolicy{{Name: "p", DSCP: 10}}}, ""},
		{"no policies", topology.ClientNode{Name: "c1"}, "1.2.3.4"},
		{"dscp zero", topology.ClientNode{Name: "c1", RoutingPolicies: []topology.RoutingPolicy{{Name: "p", DSCP: 0}}}, "1.2.3.4"},
		{"dscp 64", topology.ClientNode{Name: "c1", RoutingPolicies: []topology.RoutingPolicy{{Name: "p", DSCP: 64}}}, "1.2.3.4"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := GenerateRoutingRSC(tt.client, tt.gateway)
			if err == nil {
				t.Error("expected error")
			}
		})
	}
}
