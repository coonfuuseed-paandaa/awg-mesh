package control_plane

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/role"
)

// RegisteredNode is one record in the identity registry.
type RegisteredNode struct {
	Name             string            `json:"name"`
	Roles            []role.Role       `json:"roles"`
	OverlayIP        string            `json:"overlay_ip"`
	Region           string            `json:"region,omitempty"`
	NodeCertPEM      []byte            `json:"node_cert_pem"`
	PendingCertPEM   []byte            `json:"pending_cert_pem,omitempty"`
	CertOverlapUntil time.Time         `json:"cert_overlap_until,omitzero"`
	NodeVersion      string            `json:"node_version,omitempty"`
	RegisteredAt     time.Time         `json:"registered_at"`
	LastHeartbeatAt  time.Time         `json:"last_heartbeat_at,omitzero"`
	HealthIndicators map[string]string `json:"health,omitempty"`
}

// Registry errors.
var (
	ErrRegistryEmptyName   = errors.New("registry: node name required")
	ErrRegistryEmptyRoles  = errors.New("registry: node roles required")
	ErrRegistryNoCert      = errors.New("registry: node cert required")
	ErrRegistryNotFound    = errors.New("registry: node not found")
	ErrRegistryOverlayDup  = errors.New("registry: overlay_ip already registered to another node")
	ErrRegistryNameDup     = errors.New("registry: node name already registered with different cert")
	ErrRegistryOverlayMove = errors.New("registry: overlay_ip change on re-register is not supported")
)

// Registry holds the authoritative list of nodes that have called RegisterNode.
type Registry struct {
	mu        sync.RWMutex
	byName    map[string]*RegisteredNode
	byOverlay map[string]string // overlay -> name
}

// NewRegistry constructs an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		byName:    make(map[string]*RegisteredNode),
		byOverlay: make(map[string]string),
	}
}

// Register inserts or updates a node. If the node name already exists, the
// caller's cert must match (or registration fails — preventing a hostile
// node from hijacking another's identity). Successful re-registration with
// matching cert refreshes RegisteredAt.
func (r *Registry) Register(node RegisteredNode) error {
	if node.Name == "" {
		return ErrRegistryEmptyName
	}
	if len(node.Roles) == 0 {
		return ErrRegistryEmptyRoles
	}
	if err := role.ValidateComposability(node.Roles); err != nil {
		return fmt.Errorf("registry: %w", err)
	}
	if len(node.NodeCertPEM) == 0 {
		return ErrRegistryNoCert
	}
	if node.OverlayIP == "" {
		return fmt.Errorf("registry: overlay_ip required for node %q", node.Name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Overlay collision check (only if the overlay is owned by a DIFFERENT
	// node — a node re-registering its own IP is allowed).
	if existing, ok := r.byOverlay[node.OverlayIP]; ok && existing != node.Name {
		return fmt.Errorf("%w: %q claimed by %q", ErrRegistryOverlayDup, node.OverlayIP, existing)
	}

	// Cert pinning on re-registration.
	if prev, ok := r.byName[node.Name]; ok {
		if certBytesEqual(prev.NodeCertPEM, node.NodeCertPEM) {
			node.PendingCertPEM = append([]byte(nil), prev.PendingCertPEM...)
			node.CertOverlapUntil = prev.CertOverlapUntil
		} else if canPromotePendingCert(prev, node.NodeCertPEM, time.Now().UTC()) {
			node.PendingCertPEM = nil
			node.CertOverlapUntil = time.Time{}
		} else {
			return ErrRegistryNameDup
		}
		if prev.OverlayIP != node.OverlayIP {
			return fmt.Errorf("%w: %q -> %q", ErrRegistryOverlayMove, prev.OverlayIP, node.OverlayIP)
		}
		// Re-register: refresh metadata, preserve RegisteredAt of original.
		node.RegisteredAt = prev.RegisteredAt
	} else {
		node.RegisteredAt = time.Now().UTC()
	}

	stored := cloneRegisteredNode(node)
	r.byName[node.Name] = &stored
	r.byOverlay[node.OverlayIP] = node.Name
	return nil
}

// AllowCertRollover pins a just-issued replacement certificate for one node.
// The next RegisterNode call may promote that cert until overlapUntil.
func (r *Registry) AllowCertRollover(name string, certPEM []byte, overlapUntil time.Time) error {
	if name == "" {
		return ErrRegistryEmptyName
	}
	if len(certPEM) == 0 {
		return ErrRegistryNoCert
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	node, ok := r.byName[name]
	if !ok {
		return ErrRegistryNotFound
	}
	node.PendingCertPEM = append([]byte(nil), certPEM...)
	node.CertOverlapUntil = overlapUntil.UTC()
	return nil
}

// Heartbeat updates last-seen and health indicators for a node.
func (r *Registry) Heartbeat(name string, health map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.byName[name]
	if !ok {
		return ErrRegistryNotFound
	}
	n.LastHeartbeatAt = time.Now().UTC()
	if health != nil {
		n.HealthIndicators = make(map[string]string, len(health))
		maps.Copy(n.HealthIndicators, health)
	}
	return nil
}

// Lookup returns a node by name.
func (r *Registry) Lookup(name string) (RegisteredNode, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n, ok := r.byName[name]
	if !ok {
		return RegisteredNode{}, false
	}
	return cloneRegisteredNode(*n), true
}

// List returns all registered nodes sorted by name.
func (r *Registry) List() []RegisteredNode {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]RegisteredNode, 0, len(r.byName))
	for _, n := range r.byName {
		out = append(out, cloneRegisteredNode(*n))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// MastersInRegion returns names of nodes that have the master role in the
// given region. Used by ledger reassignment to pick a successor.
func (r *Registry) MastersInRegion(region string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0)
	for _, n := range r.byName {
		if region != "" && n.Region != region {
			continue
		}
		if slices.Contains(n.Roles, role.RoleMaster) {
			out = append(out, n.Name)
		}
	}
	sort.Strings(out)
	return out
}

// Remove drops a node from the registry. Returns ErrRegistryNotFound if absent.
func (r *Registry) Remove(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.byName[name]
	if !ok {
		return ErrRegistryNotFound
	}
	delete(r.byOverlay, n.OverlayIP)
	delete(r.byName, name)
	return nil
}

func cloneRegisteredNode(in RegisteredNode) RegisteredNode {
	out := in
	out.Roles = append([]role.Role(nil), in.Roles...)
	out.NodeCertPEM = append([]byte(nil), in.NodeCertPEM...)
	out.PendingCertPEM = append([]byte(nil), in.PendingCertPEM...)
	if in.HealthIndicators != nil {
		out.HealthIndicators = make(map[string]string, len(in.HealthIndicators))
		maps.Copy(out.HealthIndicators, in.HealthIndicators)
	}
	return out
}

func canPromotePendingCert(prev *RegisteredNode, certPEM []byte, now time.Time) bool {
	if len(prev.PendingCertPEM) == 0 || !certBytesEqual(prev.PendingCertPEM, certPEM) {
		return false
	}
	if !prev.CertOverlapUntil.IsZero() && now.After(prev.CertOverlapUntil) {
		return false
	}
	return true
}

func certBytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
