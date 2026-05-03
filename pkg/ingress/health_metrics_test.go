package ingress

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestHealthTrackerProbeOnceUpdatesTargets(t *testing.T) {
	t.Parallel()

	route := Route{Hostname: "media.example.com", Target: "172.21.92.10:8096"}
	snapshot, err := NewSnapshot([]Route{route})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	probeErr := errors.New("dial failed")
	tracker := NewHealthTracker(func(context.Context, Route) error {
		return probeErr
	})
	checkedAt := time.Unix(10, 0)
	tracker.ProbeOnce(context.Background(), snapshot, checkedAt)
	if tracker.IsHealthy(route) {
		t.Fatal("route should be unhealthy after failed probe")
	}
	status := tracker.Status(route)
	if status.LastError != "dial failed" || !status.CheckedAt.Equal(checkedAt) {
		t.Fatalf("unexpected health status: %+v", status)
	}

	tracker = NewHealthTracker(func(context.Context, Route) error { return nil })
	tracker.ProbeOnce(context.Background(), snapshot, checkedAt)
	if !tracker.IsHealthy(route) {
		t.Fatal("route should be healthy after successful probe")
	}
}

func TestMetricsUseIsolatedRegistry(t *testing.T) {
	t.Parallel()

	registry := prometheus.NewRegistry()
	metrics, err := NewMetrics(registry)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	route := Route{Tenant: "tenant-a", Hostname: "media.example.com", Target: "172.21.92.10:8096", Protocol: ProtocolHTTP}
	metrics.RecordRequest(route)
	metrics.RecordRejection(ProtocolHTTP, "unknown.example.com", "unknown-host")
	metrics.RecordProxyError(route, "target-unhealthy")
	metrics.SetTargetHealth(route, false)
	metrics.SetActiveUDPFlows(route, 3)

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	got := map[string]bool{}
	for _, family := range families {
		got[family.GetName()] = true
	}
	for _, want := range []string{
		"awg_mesh_ingress_requests_total",
		"awg_mesh_ingress_rejections_total",
		"awg_mesh_ingress_proxy_errors_total",
		"awg_mesh_ingress_target_healthy",
		"awg_mesh_ingress_udp_active_flows",
	} {
		if !got[want] {
			t.Fatalf("metric family %q missing from %#v", want, got)
		}
	}
}
