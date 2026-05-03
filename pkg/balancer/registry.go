package balancer

import (
	"fmt"
	"sync/atomic"
)

type Snapshot struct {
	egresses []EgressTarget
	byID     map[string]EgressTarget
	weighted []EgressTarget
}

func NewSnapshot(egresses []EgressTarget) (*Snapshot, error) {
	normalized, byID, err := normalizeEgresses(egresses)
	if err != nil {
		return nil, err
	}
	weighted := make([]EgressTarget, 0, len(normalized))
	for _, egress := range normalized {
		for i := 0; i < egress.Weight; i++ {
			weighted = append(weighted, egress)
		}
	}
	return &Snapshot{
		egresses: normalized,
		byID:     byID,
		weighted: weighted,
	}, nil
}

func (s *Snapshot) Egresses() []EgressTarget {
	if s == nil {
		return nil
	}
	return append([]EgressTarget(nil), s.egresses...)
}

func (s *Snapshot) Weighted() []EgressTarget {
	if s == nil {
		return nil
	}
	return append([]EgressTarget(nil), s.weighted...)
}

func (s *Snapshot) Lookup(id string) (EgressTarget, bool) {
	if s == nil {
		return EgressTarget{}, false
	}
	egress, ok := s.byID[id]
	return egress, ok
}

type Registry struct {
	current atomic.Value // *Snapshot
}

func NewRegistry(egresses []EgressTarget) (*Registry, error) {
	snapshot, err := NewSnapshot(egresses)
	if err != nil {
		return nil, err
	}
	registry := &Registry{}
	registry.current.Store(snapshot)
	return registry, nil
}

func (r *Registry) Replace(egresses []EgressTarget) (*Snapshot, error) {
	if r == nil {
		return nil, fmt.Errorf("balancer registry is nil")
	}
	snapshot, err := NewSnapshot(egresses)
	if err != nil {
		return nil, err
	}
	r.current.Store(snapshot)
	return snapshot, nil
}

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

func (r *Registry) Lookup(id string) (EgressTarget, bool) {
	return r.Snapshot().Lookup(id)
}
