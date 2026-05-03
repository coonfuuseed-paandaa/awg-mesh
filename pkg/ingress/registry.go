package ingress

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// Snapshot is an immutable view of hostname-to-route mappings.
type Snapshot struct {
	routes []Route
	byHost map[string]Route
}

// NewSnapshot validates and copies routes into an immutable lookup table.
func NewSnapshot(routes []Route) (*Snapshot, error) {
	copied := make([]Route, 0, len(routes))
	byHost := make(map[string]Route, len(routes))
	for i, route := range routes {
		normalized, err := NormalizeRoute(route)
		if err != nil {
			return nil, fmt.Errorf("snapshot route %d: %w", i, err)
		}
		if previous, exists := byHost[normalized.Hostname]; exists {
			return nil, fmt.Errorf("hostname %q is already owned by tenant %q; duplicate tenant %q is ambiguous",
				normalized.Hostname, previous.Tenant, normalized.Tenant)
		}
		copied = append(copied, normalized)
		byHost[normalized.Hostname] = normalized
	}
	if len(copied) == 0 {
		return nil, fmt.Errorf("snapshot requires at least one route")
	}
	return &Snapshot{routes: copied, byHost: byHost}, nil
}

// Routes returns a copy of the routes in this snapshot.
func (s *Snapshot) Routes() []Route {
	if s == nil {
		return nil
	}
	return append([]Route(nil), s.routes...)
}

// Lookup resolves a hostname in this immutable snapshot.
func (s *Snapshot) Lookup(hostname string) (Route, bool) {
	if s == nil {
		return Route{}, false
	}
	normalized, err := normalizeHostname(stripPort(hostname))
	if err != nil {
		return Route{}, false
	}
	route, ok := s.byHost[normalized]
	return route, ok
}

// Registry publishes copy-on-write route snapshots.
type Registry struct {
	current atomic.Value // *Snapshot
}

// NewRegistry creates a registry with an initial immutable snapshot.
func NewRegistry(routes []Route) (*Registry, error) {
	snapshot, err := NewSnapshot(routes)
	if err != nil {
		return nil, err
	}
	registry := &Registry{}
	registry.current.Store(snapshot)
	return registry, nil
}

// Replace validates routes and atomically publishes a new immutable snapshot.
func (r *Registry) Replace(routes []Route) (*Snapshot, error) {
	if r == nil {
		return nil, fmt.Errorf("ingress registry is nil")
	}
	snapshot, err := NewSnapshot(routes)
	if err != nil {
		return nil, err
	}
	r.current.Store(snapshot)
	return snapshot, nil
}

// Snapshot returns the current immutable registry snapshot.
func (r *Registry) Snapshot() *Snapshot {
	if r == nil {
		return nil
	}
	value := r.current.Load()
	if value == nil {
		return nil
	}
	snapshot, _ := value.(*Snapshot)
	return snapshot
}

// Lookup resolves against the current snapshot.
func (r *Registry) Lookup(hostname string) (Route, bool) {
	return r.Snapshot().Lookup(hostname)
}

func stripPort(host string) string {
	value := strings.TrimSpace(host)
	if value == "" {
		return value
	}
	if strings.Count(value, ":") == 1 {
		before, _, found := strings.Cut(value, ":")
		if found {
			return before
		}
	}
	return value
}
