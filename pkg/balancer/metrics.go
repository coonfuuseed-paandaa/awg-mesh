package balancer

import "github.com/prometheus/client_golang/prometheus"

type Metrics struct {
	registry   *prometheus.Registry
	decisions  *prometheus.CounterVec
	fallbacks  *prometheus.CounterVec
	selections *prometheus.CounterVec
	health     *prometheus.GaugeVec
	active     prometheus.Gauge
}

func NewMetrics(registry *prometheus.Registry) (*Metrics, error) {
	if registry == nil {
		registry = prometheus.NewRegistry()
	}
	m := &Metrics{
		registry: registry,
		decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "awg_mesh_balancer_decisions_total",
			Help: "Balancer decisions by mode, egress, and fallback state.",
		}, []string{"mode", "egress", "fallback"}),
		fallbacks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "awg_mesh_balancer_fallbacks_total",
			Help: "Balancer fallback decisions by mode, egress, and reason.",
		}, []string{"mode", "egress", "reason"}),
		selections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "awg_mesh_balancer_egress_selections_total",
			Help: "Selected egress distribution by policy mode.",
		}, []string{"mode", "egress"}),
		health: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "awg_mesh_balancer_egress_healthy",
			Help: "Egress health state, 1 for healthy and 0 for unhealthy.",
		}, []string{"egress", "target"}),
		active: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "awg_mesh_balancer_active_flows",
			Help: "Active sticky flow mappings.",
		}),
	}
	for _, collector := range []prometheus.Collector{
		m.decisions,
		m.fallbacks,
		m.selections,
		m.health,
		m.active,
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

func (m *Metrics) RecordDecision(mode Mode, egress EgressTarget, fallback bool, reason string) {
	if m == nil {
		return
	}
	fallbackValue := "false"
	if fallback {
		fallbackValue = "true"
	}
	m.decisions.WithLabelValues(string(mode), egress.ID, fallbackValue).Inc()
	m.selections.WithLabelValues(string(mode), egress.ID).Inc()
	if fallback {
		m.fallbacks.WithLabelValues(string(mode), egress.ID, reason).Inc()
	}
}

func (m *Metrics) SetTargetHealth(egress EgressTarget, healthy bool) {
	if m == nil {
		return
	}
	value := 0.0
	if healthy {
		value = 1
	}
	m.health.WithLabelValues(egress.ID, egress.Target).Set(value)
}

func (m *Metrics) SetActiveFlows(count int) {
	if m == nil {
		return
	}
	m.active.Set(float64(count))
}
