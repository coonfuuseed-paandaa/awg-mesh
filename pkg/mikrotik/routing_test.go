package mikrotik

import (
	"strings"
	"testing"

	"github.com/thebtf/awg-mesh/pkg/topology"
)

func TestGenerateRoutingRSC(t *testing.T) {
	client := topology.ClientNode{
		Name:      "home-router",
		Type:      "mikrotik",
		OverlayIP: "172.20.70.131",
		Masters:   []string{"ru-01"},
		RoutingPolicies: []topology.RoutingPolicy{
			{Name: "vpn-kz", DSCP: 10, Targets: []string{"kz-01"}},
			{Name: "vpn-us", DSCP: 20, Targets: []string{"us-01"}},
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
	if !strings.Contains(script, "vpn-kz-conn") {
		t.Error("script should contain connection mark vpn-kz-conn")
	}
	if !strings.Contains(script, "gateway=172.33.23.100") {
		t.Error("script should contain gateway address")
	}
	if !strings.Contains(script, "home-router") {
		t.Error("script should contain client name")
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
