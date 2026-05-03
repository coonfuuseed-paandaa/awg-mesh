package balancer

import (
	"testing"
	"time"
)

func TestDumbModeUsesWeightedRoundRobin(t *testing.T) {
	t.Parallel()

	engine := newTestEngine(t, Config{
		Name:      "master-01",
		OverlayIP: "172.21.92.1",
		Mode:      ModeDumb,
		Egresses: []EgressTarget{
			{ID: "egress-ru", Target: "172.21.92.10:51821", Weight: 2},
			{ID: "egress-eu", Target: "172.21.92.11:51821", Weight: 1},
		},
	})
	var got []string
	for i := 0; i < 6; i++ {
		decision, err := engine.Select(DecisionRequest{FlowKey: FlowKey{Source: "client", Destination: "internet", Protocol: "tcp", SourcePort: 1000 + i}}, time.Unix(int64(i), 0))
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		got = append(got, decision.Egress.ID)
	}
	want := []string{"egress-ru", "egress-ru", "egress-eu", "egress-ru", "egress-ru", "egress-eu"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("weighted sequence mismatch: got=%v want=%v", got, want)
		}
	}
}

func TestLabeledModePinsHealthyTarget(t *testing.T) {
	t.Parallel()

	engine := newTestEngine(t, Config{
		Name:      "master-01",
		OverlayIP: "172.21.92.1",
		Mode:      ModeLabeled,
		Egresses: []EgressTarget{
			{ID: "egress-ru", Target: "172.21.92.10:51821", Weight: 1},
			{ID: "egress-eu", Target: "172.21.92.11:51821", Weight: 1},
		},
		Labels: []LabelMapping{{Type: LabelDSCP, Value: 10, EgressID: "egress-ru"}},
	})
	for i := 0; i < 10; i++ {
		decision, err := engine.Select(DecisionRequest{FlowKey: FlowKey{Source: "client", Destination: "ru", Protocol: "tcp", SourcePort: 2000 + i}, DSCP: 10}, time.Unix(int64(i), 0))
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if decision.Egress.ID != "egress-ru" || decision.FallbackReason != "" {
			t.Fatalf("labeled flow not pinned: %+v", decision)
		}
	}
}

func TestLabeledModeFallsBackWhenMappedTargetUnhealthy(t *testing.T) {
	t.Parallel()

	engine := newTestEngine(t, Config{
		Name:      "master-01",
		OverlayIP: "172.21.92.1",
		Mode:      ModeLabeled,
		Egresses: []EgressTarget{
			{ID: "egress-ru", Target: "172.21.92.10:51821", Weight: 1},
			{ID: "egress-eu", Target: "172.21.92.11:51821", Weight: 1},
		},
		Labels: []LabelMapping{{Type: LabelDSCP, Value: 10, EgressID: "egress-ru"}},
	})
	engine.Health().Set("egress-ru", false, time.Unix(1, 0), "probe failed")
	decision, err := engine.Select(DecisionRequest{FlowKey: FlowKey{Source: "client", Destination: "ru", Protocol: "tcp"}, DSCP: 10}, time.Unix(2, 0))
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if decision.Egress.ID != "egress-eu" || decision.FallbackReason != "labeled-target-unhealthy" {
		t.Fatalf("unexpected fallback decision: %+v", decision)
	}
}

func TestFlowStickinessExpiresAndTracksHealth(t *testing.T) {
	t.Parallel()

	engine := newTestEngine(t, Config{
		Name:            "master-01",
		OverlayIP:       "172.21.92.1",
		Mode:            ModeDumb,
		FlowIdleTimeout: 5 * time.Second,
		Egresses: []EgressTarget{
			{ID: "egress-ru", Target: "172.21.92.10:51821", Weight: 1},
			{ID: "egress-eu", Target: "172.21.92.11:51821", Weight: 1},
		},
	})
	flow := FlowKey{Source: "client", Destination: "internet", Protocol: "tcp", SourcePort: 1000, DestinationPort: 443}
	first, err := engine.Select(DecisionRequest{FlowKey: flow}, time.Unix(10, 0))
	if err != nil {
		t.Fatalf("first Select: %v", err)
	}
	second, err := engine.Select(DecisionRequest{FlowKey: flow}, time.Unix(12, 0))
	if err != nil {
		t.Fatalf("second Select: %v", err)
	}
	if second.Egress.ID != first.Egress.ID || !second.Sticky {
		t.Fatalf("flow did not stay sticky: first=%+v second=%+v", first, second)
	}
	engine.Health().Set(first.Egress.ID, false, time.Unix(13, 0), "probe failed")
	afterHealth, err := engine.Select(DecisionRequest{FlowKey: flow}, time.Unix(14, 0))
	if err != nil {
		t.Fatalf("after health Select: %v", err)
	}
	if afterHealth.Egress.ID == first.Egress.ID {
		t.Fatalf("unhealthy sticky egress reused: %+v", afterHealth)
	}
	afterExpiry, err := engine.Select(DecisionRequest{FlowKey: flow}, time.Unix(30, 0))
	if err != nil {
		t.Fatalf("after expiry Select: %v", err)
	}
	if !afterExpiry.ExpiresAt.After(time.Unix(30, 0)) {
		t.Fatalf("expiry not refreshed: %+v", afterExpiry)
	}
}

func TestEmptyFlowKeyDoesNotCreateGlobalStickiness(t *testing.T) {
	t.Parallel()

	engine := newTestEngine(t, Config{
		Name:      "master-01",
		OverlayIP: "172.21.92.1",
		Mode:      ModeDumb,
		Egresses: []EgressTarget{
			{ID: "egress-ru", Target: "172.21.92.10:51821", Weight: 1},
			{ID: "egress-eu", Target: "172.21.92.11:51821", Weight: 1},
		},
	})
	first, err := engine.Select(DecisionRequest{}, time.Unix(10, 0))
	if err != nil {
		t.Fatalf("first Select: %v", err)
	}
	second, err := engine.Select(DecisionRequest{}, time.Unix(11, 0))
	if err != nil {
		t.Fatalf("second Select: %v", err)
	}
	if first.Egress.ID == second.Egress.ID || second.Sticky {
		t.Fatalf("empty flow key created sticky reuse: first=%+v second=%+v", first, second)
	}
}

func newTestEngine(t *testing.T, cfg Config) *Engine {
	t.Helper()
	engine, err := NewEngine(cfg, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return engine
}
