package ingress

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics owns CR-006 collectors in an isolated Prometheus registry.
type Metrics struct {
	registry     *prometheus.Registry
	requests     *prometheus.CounterVec
	rejections   *prometheus.CounterVec
	proxyErrors  *prometheus.CounterVec
	targetHealth *prometheus.GaugeVec
	activeFlows  *prometheus.GaugeVec
}

func NewMetrics(registry *prometheus.Registry) (*Metrics, error) {
	if registry == nil {
		registry = prometheus.NewRegistry()
	}
	m := &Metrics{
		registry: registry,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "awg_mesh_ingress_requests_total",
			Help: "Accepted ingress requests by protocol, tenant, and hostname.",
		}, []string{"protocol", "tenant", "hostname"}),
		rejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "awg_mesh_ingress_rejections_total",
			Help: "Rejected ingress attempts by protocol, hostname, and reason.",
		}, []string{"protocol", "hostname", "reason"}),
		proxyErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "awg_mesh_ingress_proxy_errors_total",
			Help: "Ingress proxy errors by protocol, tenant, hostname, and reason.",
		}, []string{"protocol", "tenant", "hostname", "reason"}),
		targetHealth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "awg_mesh_ingress_target_healthy",
			Help: "Target health state, 1 for healthy and 0 for unhealthy.",
		}, []string{"tenant", "hostname", "target"}),
		activeFlows: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "awg_mesh_ingress_udp_active_flows",
			Help: "Active UDP flow mappings by tenant and hostname.",
		}, []string{"tenant", "hostname"}),
	}
	for _, collector := range []prometheus.Collector{
		m.requests,
		m.rejections,
		m.proxyErrors,
		m.targetHealth,
		m.activeFlows,
	} {
		if err := registry.Register(collector); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (m *Metrics) Registry() *prometheus.Registry {
	if m == nil {
		return nil
	}
	return m.registry
}

func (m *Metrics) RecordRequest(route Route) {
	if m == nil {
		return
	}
	m.requests.WithLabelValues(string(route.Protocol), route.Tenant, route.Hostname).Inc()
}

func (m *Metrics) RecordRejection(protocol Protocol, hostname, reason string) {
	if m == nil {
		return
	}
	m.rejections.WithLabelValues(string(protocol), hostname, reason).Inc()
}

func (m *Metrics) RecordProxyError(route Route, reason string) {
	if m == nil {
		return
	}
	m.proxyErrors.WithLabelValues(string(route.Protocol), route.Tenant, route.Hostname, reason).Inc()
}

func (m *Metrics) SetTargetHealth(route Route, healthy bool) {
	if m == nil {
		return
	}
	value := 0.0
	if healthy {
		value = 1
	}
	m.targetHealth.WithLabelValues(route.Tenant, route.Hostname, route.Target).Set(value)
}

func (m *Metrics) SetActiveUDPFlows(route Route, count int) {
	if m == nil {
		return
	}
	m.activeFlows.WithLabelValues(route.Tenant, route.Hostname).Set(float64(count))
}
