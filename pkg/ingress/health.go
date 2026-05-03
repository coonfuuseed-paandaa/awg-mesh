package ingress

import (
	"context"
	"net"
	"sync"
	"time"
)

// ProbeFunc checks whether a target route is currently reachable.
type ProbeFunc func(ctx context.Context, route Route) error

type TargetHealth struct {
	Healthy   bool
	LastError string
	CheckedAt time.Time
}

// HealthTracker stores target health without mutating registry snapshots.
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

func (h *HealthTracker) IsHealthy(route Route) bool {
	if h == nil {
		return true
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	status, ok := h.status[route.Hostname]
	return !ok || status.Healthy
}

func (h *HealthTracker) Status(route Route) TargetHealth {
	if h == nil {
		return TargetHealth{Healthy: true}
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	status, ok := h.status[route.Hostname]
	if !ok {
		return TargetHealth{Healthy: true}
	}
	return status
}

func (h *HealthTracker) Set(route Route, healthy bool, checkedAt time.Time, err error) {
	if h == nil {
		return
	}
	lastErr := ""
	if err != nil {
		lastErr = err.Error()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	next := make(map[string]TargetHealth, len(h.status)+1)
	for key, value := range h.status {
		next[key] = value
	}
	next[route.Hostname] = TargetHealth{Healthy: healthy, LastError: lastErr, CheckedAt: checkedAt}
	h.status = next
}

func (h *HealthTracker) ProbeOnce(ctx context.Context, snapshot *Snapshot, now time.Time) {
	if h == nil || snapshot == nil {
		return
	}
	for _, route := range snapshot.Routes() {
		err := h.probe(ctx, route)
		h.Set(route, err == nil, now, err)
	}
}

func (h *HealthTracker) Run(ctx context.Context, registry *Registry, interval time.Duration) {
	if h == nil || registry == nil {
		return
	}
	if interval <= 0 {
		interval = DefaultHealthProbeInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	h.ProbeOnce(ctx, registry.Snapshot(), time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			h.ProbeOnce(ctx, registry.Snapshot(), now)
		}
	}
}

func tcpProbe(ctx context.Context, route Route) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", route.Target)
	if err != nil {
		return err
	}
	return conn.Close()
}
