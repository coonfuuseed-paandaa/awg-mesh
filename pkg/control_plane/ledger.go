// Package control_plane is the F-009 v2.0 control-plane daemon backing
// the gRPC service defined in proto/control_plane.proto.
//
// The package exposes:
//   - Ledger: ownership-of-overlay-/32 mapping with version-vector conflict
//     resolution (FR-5 HA-2 partitioned ownership).
//   - Registry: identity registry of all nodes that have registered with the
//     control plane (FR-15 node lifecycle, FR-16 cert tracking).
//   - Server: gRPC handler for proto/control_plane.ControlPlane.
//   - Daemon: top-level orchestrator wiring the above into a runnable process.
//
// CR-002 ships the minimum viable subset: registry, ledger, gRPC server with
// RegisterNode + Heartbeat + DecommissionNode + StreamPeerList + StreamOwnership.
// Streaming RotateAWGParamsMeshWide, SignalExchange, StreamServiceRegistry,
// QueryAudit, StreamCertUpdate land alongside the consuming features in
// CR-008, CR-006, CR-020 (audit), CR-015 (cert).
package control_plane

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"
)

// OwnershipEntry describes which master currently owns an overlay /32.
//
// FR-5 HA-2 invariant: every overlay /32 in the mesh has exactly one owning
// master at any moment. Reassign() bumps Version monotonically — readers
// detect stale snapshots by comparing Version values.
type OwnershipEntry struct {
	OverlayIP        string    `json:"overlay_ip"`
	OwningMaster     string    `json:"owning_master"`
	LastReassignedAt time.Time `json:"last_reassigned_at"`
	PreviousOwner    string    `json:"previous_owner,omitempty"`
	Reason           string    `json:"reason,omitempty"` // "scheduled" | "failover" | "operator-pinned"
	Version          int64     `json:"version"`          // monotonic per-entry
}

// Ledger errors.
var (
	ErrLedgerOverlayInvalid = errors.New("ledger: overlay_ip is not a valid IP")
	ErrLedgerEmptyMaster    = errors.New("ledger: owning_master must be non-empty")
	ErrLedgerNotFound       = errors.New("ledger: overlay_ip not found")
	ErrLedgerOwnerChanged   = errors.New("ledger: overlay owner changed")
)

// Ledger is the mesh-wide ownership map. Implementations must be safe for
// concurrent calls from gRPC handlers + the rotation orchestrator.
type Ledger struct {
	mu       sync.RWMutex
	entries  map[string]*OwnershipEntry // key: overlay /32 string
	version  int64                      // bumped on every mutation; broadcast as snapshot version
	listener LedgerListener
}

// LedgerListener is invoked after every mutation. The control plane wires its
// streaming-RPC fan-out through this hook.
type LedgerListener interface {
	OnLedgerMutation(snapshot []OwnershipEntry, version int64)
}

// NewLedger constructs an empty ledger.
func NewLedger() *Ledger {
	return &Ledger{
		entries: make(map[string]*OwnershipEntry),
	}
}

// SetListener registers a fan-out listener. nil clears it.
func (l *Ledger) SetListener(listener LedgerListener) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listener = listener
}

// Reassign sets the owner of overlay_ip to new_owner with the supplied reason.
// Returns the new entry version. The first call for a given overlay_ip
// behaves as an insert; subsequent calls bump Version.
func (l *Ledger) Reassign(overlayIP, newOwner, reason string) (int64, error) {
	return l.reassign(overlayIP, "", newOwner, reason, false)
}

func (l *Ledger) reassign(overlayIP, expectedOwner, newOwner, reason string, checkOwner bool) (int64, error) {
	if newOwner == "" {
		return 0, ErrLedgerEmptyMaster
	}
	ip := net.ParseIP(overlayIP)
	if ip == nil || ip.To4() == nil {
		return 0, fmt.Errorf("%w: %q", ErrLedgerOverlayInvalid, overlayIP)
	}

	l.mu.Lock()
	previous := ""
	entry, ok := l.entries[overlayIP]
	if ok {
		if checkOwner && entry.OwningMaster != expectedOwner {
			l.mu.Unlock()
			return 0, fmt.Errorf("%w: overlay_ip %s owned by %s, expected %s", ErrLedgerOwnerChanged, overlayIP, entry.OwningMaster, expectedOwner)
		}
		previous = entry.OwningMaster
		entry.PreviousOwner = previous
	} else if checkOwner {
		l.mu.Unlock()
		return 0, ErrLedgerNotFound
	} else {
		entry = &OwnershipEntry{OverlayIP: overlayIP}
		l.entries[overlayIP] = entry
	}
	entry.OwningMaster = newOwner
	entry.Reason = reason
	entry.LastReassignedAt = time.Now().UTC()
	entry.Version++
	entryVersion := entry.Version
	l.version++
	snapshot := l.snapshotLocked()
	listener := l.listener
	version := l.version
	l.mu.Unlock()

	if listener != nil {
		listener.OnLedgerMutation(snapshot, version)
	}
	return entryVersion, nil
}

// Lookup returns the entry for overlay_ip if present.
func (l *Ledger) Lookup(overlayIP string) (OwnershipEntry, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	entry, ok := l.entries[overlayIP]
	if !ok {
		return OwnershipEntry{}, false
	}
	return *entry, true
}

// Remove deletes an overlay_ip from the ledger (used during node
// decommissioning when the address is no longer routable).
func (l *Ledger) Remove(overlayIP string) error {
	l.mu.Lock()
	if _, ok := l.entries[overlayIP]; !ok {
		l.mu.Unlock()
		return ErrLedgerNotFound
	}
	delete(l.entries, overlayIP)
	l.version++
	snapshot := l.snapshotLocked()
	listener := l.listener
	version := l.version
	l.mu.Unlock()

	if listener != nil {
		listener.OnLedgerMutation(snapshot, version)
	}
	return nil
}

// OwnedBy returns all overlay /32 entries currently owned by the given master.
// Sorted by overlay_ip for deterministic output.
func (l *Ledger) OwnedBy(master string) []OwnershipEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]OwnershipEntry, 0)
	for _, e := range l.entries {
		if e.OwningMaster == master {
			out = append(out, *e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OverlayIP < out[j].OverlayIP })
	return out
}

// Snapshot returns a stable copy of the entire ledger plus its current
// version. Callers may iterate without holding the lock.
func (l *Ledger) Snapshot() ([]OwnershipEntry, int64) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.snapshotLocked(), l.version
}

// snapshotLocked must be called with l.mu held (read or write).
func (l *Ledger) snapshotLocked() []OwnershipEntry {
	out := make([]OwnershipEntry, 0, len(l.entries))
	for _, e := range l.entries {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OverlayIP < out[j].OverlayIP })
	return out
}

// Version returns the current ledger snapshot version. Streaming RPC clients
// compare this against their last-seen value to detect updates.
func (l *Ledger) Version() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.version
}

// Drain reassigns every overlay /32 currently owned by sourceMaster to a
// surviving master selected by chooseMaster. Returns the number of entries
// reassigned. Used during graceful node decommissioning (FR-15).
//
// chooseMaster is called for each overlay /32 owned by sourceMaster and must
// return a non-empty master name. If chooseMaster returns "" for any entry,
// Drain aborts and returns the number of entries successfully reassigned so
// far. Caller is responsible for picking masters from the live registry.
func (l *Ledger) Drain(sourceMaster, reason string, chooseMaster func(overlayIP string) string) (int, error) {
	owned := l.OwnedBy(sourceMaster)
	reassigned := 0
	for _, entry := range owned {
		if chooseMaster == nil {
			return reassigned, fmt.Errorf("ledger: drain aborted at overlay_ip %s — chooseMaster is nil", entry.OverlayIP)
		}
		newOwner := chooseMaster(entry.OverlayIP)
		if newOwner == "" {
			return reassigned, fmt.Errorf("ledger: drain aborted at overlay_ip %s — chooseMaster returned empty", entry.OverlayIP)
		}
		if newOwner == sourceMaster {
			continue // chooseMaster returned the source master itself; skip.
		}
		if _, err := l.reassign(entry.OverlayIP, sourceMaster, newOwner, reason, true); err != nil {
			if errors.Is(err, ErrLedgerOwnerChanged) || errors.Is(err, ErrLedgerNotFound) {
				continue
			}
			return reassigned, fmt.Errorf("ledger: drain failed at overlay_ip %s: %w", entry.OverlayIP, err)
		}
		reassigned++
	}
	return reassigned, nil
}
