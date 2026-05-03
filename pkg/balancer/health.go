package balancer

import (
	"context"
	"net"
	"sync"
	"time"
)

type ProbeFunc func(ctx context.Context, egress EgressTarget) error

type TargetHealth struct {
	Healthy   bool
	LastError string
	CheckedAt time.Time
}

type HealthTracker struct {
	mu     sync.RWMutex
	probe  ProbeFunc
	status map[string]TargetHealth
}

func NewHealthTracker(probe ProbeFunc) *HealthTracker {
	if probe == nil {
		probe = tcpProbe
	}
	return &HealthTracker{probe: probe, status: make(map[string]TargetHealth)}
}

func (h *HealthTracker) IsHealthy(egress EgressTarget) bool {
	if h == nil {
		return true
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	status, ok := h.status[egress.ID]
	return !ok || status.Healthy
}

func (h *HealthTracker) Status(egress EgressTarget) TargetHealth {
	if h == nil {
		return TargetHealth{Healthy: true}
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	status, ok := h.status[egress.ID]
	if !ok {
		return TargetHealth{Healthy: true}
	}
	return status
}

func (h *HealthTracker) Set(egressID string, healthy bool, checkedAt time.Time, lastError string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	next := make(map[string]TargetHealth, len(h.status)+1)
	for key, value := range h.status {
		next[key] = value
	}
	next[egressID] = TargetHealth{Healthy: healthy, LastError: lastError, CheckedAt: checkedAt}
	h.status = next
}

func (h *HealthTracker) ProbeOnce(ctx context.Context, snapshot *Snapshot, metrics *Metrics, now time.Time) {
	if h == nil || snapshot == nil {
		return
	}
	for _, egress := range snapshot.Egresses() {
		err := h.probe(ctx, egress)
		lastErr := ""
		if err != nil {
			lastErr = err.Error()
		}
		healthy := err == nil
		h.Set(egress.ID, healthy, now, lastErr)
		if metrics != nil {
			metrics.SetTargetHealth(egress, healthy)
		}
	}
}

func (h *HealthTracker) Run(ctx context.Context, registry *Registry, metrics *Metrics, interval time.Duration) {
	if h == nil || registry == nil {
		return
	}
	if interval <= 0 {
		interval = DefaultHealthProbeInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	h.ProbeOnce(ctx, registry.Snapshot(), metrics, time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			h.ProbeOnce(ctx, registry.Snapshot(), metrics, now)
		}
	}
}

func tcpProbe(ctx context.Context, egress EgressTarget) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", egress.Target)
	if err != nil {
		return err
	}
	return conn.Close()
}
