package balancer

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
)

func TestMetricsUseIsolatedRegistry(t *testing.T) {
	t.Parallel()

	registry := prometheus.NewRegistry()
	metrics, err := NewMetrics(registry)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	target := EgressTarget{ID: "egress-ru", Target: "172.21.92.10:51821"}
	metrics.RecordDecision(ModeDumb, target, false, "")
	metrics.RecordDecision(ModeLabeled, target, true, "labeled-target-unhealthy")
	metrics.SetTargetHealth(target, false)
	metrics.SetActiveFlows(3)

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	got := map[string]bool{}
	for _, family := range families {
		got[family.GetName()] = true
	}
	for _, want := range []string{
		"awg_mesh_balancer_decisions_total",
		"awg_mesh_balancer_fallbacks_total",
		"awg_mesh_balancer_egress_selections_total",
		"awg_mesh_balancer_egress_healthy",
		"awg_mesh_balancer_active_flows",
	} {
		if !got[want] {
			t.Fatalf("metric family %q missing from %#v", want, got)
		}
	}
}

func TestRuntimeStartsMetricsAndStopsWithContext(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("Close listener: %v", err)
	}
	runtime, err := NewRuntime(Config{
		Name:           "master-01",
		OverlayIP:      "172.21.92.1",
		MetricsAddress: addr,
		Egresses:       []EgressTarget{{ID: "egress-ru", Target: "172.21.92.10:51821", Weight: 1}},
	}, zerolog.Nop())
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runtime did not stop after context cancellation")
	}
}
