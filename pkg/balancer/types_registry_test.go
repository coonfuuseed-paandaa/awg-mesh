package balancer

import (
	"strings"
	"testing"
	"time"
)

func TestNormalizeConfigRejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "missing node",
			cfg: Config{
				OverlayIP: "172.21.92.1",
				Egresses:  []EgressTarget{{ID: "egress-ru", Target: "172.21.92.10:51821", Weight: 1}},
			},
			want: "balancer name is required",
		},
		{
			name: "invalid overlay",
			cfg: Config{
				Name:      "master-01",
				OverlayIP: "not-ip",
				Egresses:  []EgressTarget{{ID: "egress-ru", Target: "172.21.92.10:51821", Weight: 1}},
			},
			want: "parse balancer overlay IP",
		},
		{
			name: "missing egresses",
			cfg: Config{
				Name:      "master-01",
				OverlayIP: "172.21.92.1",
			},
			want: "requires at least one egress target",
		},
		{
			name: "invalid target",
			cfg: Config{
				Name:      "master-01",
				OverlayIP: "172.21.92.1",
				Egresses:  []EgressTarget{{ID: "egress-ru", Target: "service.local:51821", Weight: 1}},
			},
			want: "must be an overlay IP",
		},
		{
			name: "non-positive weight",
			cfg: Config{
				Name:      "master-01",
				OverlayIP: "172.21.92.1",
				Egresses:  []EgressTarget{{ID: "egress-ru", Target: "172.21.92.10:51821", Weight: 0}},
			},
			want: "weight must be positive",
		},
		{
			name: "unknown label target",
			cfg: Config{
				Name:      "master-01",
				OverlayIP: "172.21.92.1",
				Mode:      ModeLabeled,
				Egresses:  []EgressTarget{{ID: "egress-ru", Target: "172.21.92.10:51821", Weight: 1}},
				Labels:    []LabelMapping{{Type: LabelDSCP, Value: 10, EgressID: "egress-eu"}},
			},
			want: "unknown egress",
		},
		{
			name: "zero label",
			cfg: Config{
				Name:      "master-01",
				OverlayIP: "172.21.92.1",
				Mode:      ModeLabeled,
				Egresses:  []EgressTarget{{ID: "egress-ru", Target: "172.21.92.10:51821", Weight: 1}},
				Labels:    []LabelMapping{{Type: LabelDSCP, Value: 0, EgressID: "egress-ru"}},
			},
			want: "value must be positive",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := NormalizeConfig(tt.cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("NormalizeConfig error = %v, want contains %q", err, tt.want)
			}
		})
	}
}

func TestPlanConfigUsesPolicyFields(t *testing.T) {
	t.Parallel()

	plan, err := PlanConfig(Config{
		Name:                "master-01",
		OverlayIP:           "172.21.92.1",
		Mode:                ModeLabeled,
		HealthProbeInterval: 2 * time.Second,
		FlowIdleTimeout:     9 * time.Second,
		MetricsAddress:      ":9093",
		Egresses: []EgressTarget{
			{ID: "egress-ru", Target: "172.21.92.10:51821", Weight: 2},
			{ID: "egress-eu", Target: "172.21.92.11:51821", Weight: 1},
		},
		Labels: []LabelMapping{{Type: LabelDSCP, Value: 10, EgressID: "egress-ru"}},
	})
	if err != nil {
		t.Fatalf("PlanConfig: %v", err)
	}
	if plan.Name != "master-01" || plan.OverlayIP != "172.21.92.1" || plan.Mode != ModeLabeled {
		t.Fatalf("identity/policy fields not preserved: %+v", plan)
	}
	if plan.EgressCount != 2 || plan.Egresses[0].ID != "egress-ru" || plan.Egresses[0].Weight != 2 {
		t.Fatalf("egress fields not used in plan: %+v", plan)
	}
	if plan.LabelCount != 1 || plan.Labels[0].Value != 10 || plan.Labels[0].EgressID != "egress-ru" {
		t.Fatalf("label mapping fields not used in plan: %+v", plan)
	}
	if plan.HealthProbeInterval != 2*time.Second || plan.FlowIdleTimeout != 9*time.Second || plan.MetricsAddress != ":9093" {
		t.Fatalf("runtime fields not used in plan: %+v", plan)
	}
}

func TestRegistryReplaceIsCopyOnWrite(t *testing.T) {
	t.Parallel()

	registry, err := NewRegistry([]EgressTarget{{ID: "egress-ru", Target: "172.21.92.10:51821", Weight: 1}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	oldSnapshot := registry.Snapshot()
	if _, err := registry.Replace([]EgressTarget{{ID: "egress-eu", Target: "172.21.92.11:51821", Weight: 1}}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if target, ok := oldSnapshot.Lookup("egress-ru"); !ok || target.Target != "172.21.92.10:51821" {
		t.Fatalf("old snapshot was mutated: target=%+v ok=%v", target, ok)
	}
	if _, ok := oldSnapshot.Lookup("egress-eu"); ok {
		t.Fatal("old snapshot sees replacement target")
	}
	if target, ok := registry.Lookup("egress-eu"); !ok || target.Target != "172.21.92.11:51821" {
		t.Fatalf("current registry did not publish replacement: target=%+v ok=%v", target, ok)
	}
}
