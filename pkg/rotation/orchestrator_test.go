package rotation

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/role"
	controlpb "github.com/coonfuuseed-paandaa/awg-mesh/proto/control_plane"
)

func TestOrchestratorPlanFiltersMeshInternalTargets(t *testing.T) {
	t.Parallel()

	orchestrator := NewOrchestrator(nil, OrchestratorConfig{Clock: fixedClock})
	plan, err := orchestrator.Plan(Request{
		Tier:   "1",
		Params: testParams(),
		Targets: []Target{
			{Name: "client-01", Roles: []role.Role{role.RoleClient}, OverlayIP: "172.21.92.130"},
			{Name: "egress-01", Roles: []role.Role{role.RoleEgress}, OverlayIP: "172.21.92.34"},
			{Name: "master-01", Roles: []role.Role{role.RoleMaster, role.RoleBalancer}, OverlayIP: "172.21.92.2"},
			{Name: "ingress-01", Roles: []role.Role{role.RoleIngress}, OverlayIP: "172.21.92.20"},
		},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	got := targetNames(plan.Targets)
	want := []string{"egress-01", "ingress-01", "master-01"}
	if !slices.Equal(got, want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}
	if plan.Tier != Tier1 || plan.RotationID != "tier1-1704067200000000" {
		t.Fatalf("unexpected plan identity: %+v", plan)
	}
	if !plan.ApplyAt.Equal(fixedClock().Add(DefaultApplyLeadTime)) {
		t.Fatalf("default apply_at = %s", plan.ApplyAt)
	}
}

func TestOrchestratorRejectsInvalidPlan(t *testing.T) {
	t.Parallel()

	orchestrator := NewOrchestrator(nil, OrchestratorConfig{Clock: fixedClock})
	tests := []struct {
		name string
		req  Request
		want string
	}{
		{name: "bad tier", req: Request{Tier: "bad", Params: testParams(), Targets: testTargets()}, want: "unsupported tier"},
		{name: "missing params", req: Request{Tier: Tier1, Targets: testTargets()}, want: "params are required"},
		{name: "only clients", req: Request{Tier: Tier1, Params: testParams(), Targets: []Target{{Name: "client", Roles: []role.Role{role.RoleClient}}}}, want: ErrNoRotationTargets.Error()},
		{name: "past apply", req: Request{Tier: Tier1, Params: testParams(), ApplyAt: fixedClock().Add(-time.Second), Targets: testTargets()}, want: "in the past"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := orchestrator.Plan(tt.req)
			if err == nil {
				t.Fatal("expected error")
			}
			if !contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want contains %q", err, tt.want)
			}
		})
	}
}

func TestOrchestratorExecuteRecordsSuccessAndImmutableHistory(t *testing.T) {
	t.Parallel()

	applier := NewMemoryApplier()
	orchestrator := NewOrchestrator(applier, OrchestratorConfig{Clock: fixedClock})
	exec, err := orchestrator.Execute(context.Background(), Request{
		Tier:       Tier2,
		Params:     testParams(),
		RotationID: "rot-1",
		Targets:    testTargets(),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if exec.Status != RotationStatusSucceeded || len(exec.Results) != 2 {
		t.Fatalf("unexpected execution: %+v", exec)
	}
	for _, result := range exec.Results {
		if !result.Ack || result.Error != "" {
			t.Fatalf("unexpected result: %+v", result)
		}
	}
	if got := applier.Snapshot(); len(got) != 2 || got["master-01"].RotationID != "rot-1" || got["egress-01"].Tier != Tier2 {
		t.Fatalf("applier snapshot mismatch: %+v", got)
	}

	records := orchestrator.History().Records()
	if len(records) != 1 {
		t.Fatalf("history len = %d, want 1", len(records))
	}
	records[0].Plan.Targets[0].Name = "mutated"
	records[0].Plan.Params.I1[0] = 'x'
	again := orchestrator.History().Records()
	if again[0].Plan.Targets[0].Name == "mutated" || string(again[0].Plan.Params.I1) == "x1" {
		t.Fatalf("history returned mutable internals: %+v", again[0])
	}
}

func TestOrchestratorExecuteReportsPartialFailure(t *testing.T) {
	t.Parallel()

	applier := NewMemoryApplier()
	applier.SetFailure("egress-01", errors.New("apply failed"))
	orchestrator := NewOrchestrator(applier, OrchestratorConfig{Clock: fixedClock})
	exec, err := orchestrator.Execute(context.Background(), Request{
		Tier:    Tier3,
		Params:  testParams(),
		Targets: testTargets(),
	})
	if err == nil || !errors.Is(err, ErrPartialApply) {
		t.Fatalf("expected ErrPartialApply, got %v", err)
	}
	if exec.Status != RotationStatusPartialFailed || len(exec.Results) != 2 {
		t.Fatalf("unexpected partial execution: %+v", exec)
	}
	if exec.Results[0].Target.Name != "egress-01" || exec.Results[0].Ack || exec.Results[0].Error == "" {
		t.Fatalf("expected egress failure first by stable ordering, got %+v", exec.Results)
	}
	if !exec.Results[1].Ack {
		t.Fatalf("expected later target to still apply, got %+v", exec.Results[1])
	}
	if records := orchestrator.History().Records(); len(records) != 1 || records[0].Status != RotationStatusPartialFailed {
		t.Fatalf("partial history missing: %+v", records)
	}
}

func TestAdaptiveDetectorTriggersOnMetricAnomalies(t *testing.T) {
	t.Parallel()

	detector := NewAdaptiveDetector(AdaptiveConfig{})
	throughput := detector.Evaluate([]MetricSample{{
		NodeName:               "master-01",
		Window:                 30 * time.Second,
		BaselineThroughputMbps: 100,
		CurrentThroughputMbps:  69,
	}})
	if !throughput.Trigger || throughput.Tier != Tier1 || !contains(throughput.Reason, "throughput-drop") {
		t.Fatalf("expected throughput trigger, got %+v", throughput)
	}

	retry := detector.Evaluate([]MetricSample{{NodeName: "master-02", Window: time.Minute, HandshakeRetries: 10}})
	if !retry.Trigger || !contains(retry.Reason, "handshake-retry-storm") {
		t.Fatalf("expected retry trigger, got %+v", retry)
	}

	rtt := detector.Evaluate([]MetricSample{{NodeName: "master-03", Window: time.Minute, BaselineRTT: 50 * time.Millisecond, CurrentRTT: 100 * time.Millisecond}})
	if !rtt.Trigger || !contains(rtt.Reason, "rtt-spike") {
		t.Fatalf("expected RTT trigger, got %+v", rtt)
	}
}

func TestAdaptiveDetectorIgnoresShortOrHealthySamples(t *testing.T) {
	t.Parallel()

	detector := NewAdaptiveDetector(AdaptiveConfig{})
	short := detector.Evaluate([]MetricSample{{
		NodeName:               "master-01",
		Window:                 29 * time.Second,
		BaselineThroughputMbps: 100,
		CurrentThroughputMbps:  1,
	}})
	if short.Trigger {
		t.Fatalf("short window should not trigger: %+v", short)
	}
	healthy := detector.Evaluate([]MetricSample{{
		NodeName:               "master-01",
		Window:                 30 * time.Second,
		BaselineThroughputMbps: 100,
		CurrentThroughputMbps:  80,
		HandshakeRetries:       1,
		BaselineRTT:            50 * time.Millisecond,
		CurrentRTT:             60 * time.Millisecond,
	}})
	if healthy.Trigger {
		t.Fatalf("healthy sample should not trigger: %+v", healthy)
	}
}

func fixedClock() time.Time {
	return time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
}

func testParams() *controlpb.AWGParamsV2 {
	return &controlpb.AWGParamsV2{
		Jc: 1, Jmin: 2, Jmax: 3,
		S1: 4, S2: 5,
		H1: 6, H2: 7, H3: 8, H4: 9,
		I1: []byte("i1"),
		I2: []byte("i2"),
		I3: []byte("i3"),
		I4: []byte("i4"),
		I5: []byte("i5"),
	}
}

func testTargets() []Target {
	return []Target{
		{Name: "master-01", Roles: []role.Role{role.RoleMaster}, OverlayIP: "172.21.92.2"},
		{Name: "egress-01", Roles: []role.Role{role.RoleEgress}, OverlayIP: "172.21.92.34"},
	}
}

func targetNames(targets []Target) []string {
	names := make([]string, 0, len(targets))
	for _, target := range targets {
		names = append(names, target.Name)
	}
	return names
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
