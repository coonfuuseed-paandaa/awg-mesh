package control_plane

import (
	"sort"
	"sync"
	"time"
)

// AuditEvent is a single record in the in-memory audit log. FR-20 query is
// served by AuditLog.Query; persistence to disk is the responsibility of the
// caller (Daemon writes audit entries to <state-dir>/audit.log via Append on
// every event).
type AuditEvent struct {
	Timestamp time.Time `json:"timestamp"`
	EventType string    `json:"event_type"` // "register" | "heartbeat" | "decommission" | "rotate" | "ownership-reassign" | "cert-issue"
	NodeName  string    `json:"node_name"`
	Detail    string    `json:"detail"`
	Actor     string    `json:"actor"` // "self" | "operator" | mTLS-CN
}

// AuditLog is a concurrent ring-buffer of AuditEvent values. The default
// capacity holds the most recent 8192 events; older events are dropped. CR-002
// keeps the log in memory only — durable persistence lands with the audit
// query subsystem in CR-020 / FR-20.
type AuditLog struct {
	mu     sync.RWMutex
	buf    []AuditEvent
	cap    int
	cursor int
	full   bool
}

// NewAuditLog constructs an in-memory audit log of the given capacity.
// capacity ≤ 0 falls back to 8192.
func NewAuditLog(capacity int) *AuditLog {
	if capacity <= 0 {
		capacity = 8192
	}
	return &AuditLog{
		buf: make([]AuditEvent, capacity),
		cap: capacity,
	}
}

// Append records a single audit event. Timestamp is set to time.Now().UTC() if
// the caller's value is zero.
func (a *AuditLog) Append(e AuditEvent) {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.buf[a.cursor] = e
	a.cursor = (a.cursor + 1) % a.cap
	if a.cursor == 0 {
		a.full = true
	}
}

// Query returns events matching the filter, sorted oldest→newest.
// since/until are inclusive bounds; zero values mean "unbounded". An empty
// eventType / nodeName means "match all". limit ≤ 0 returns all matches.
func (a *AuditLog) Query(since, until time.Time, eventType, nodeName string, limit int) []AuditEvent {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]AuditEvent, 0)
	end := a.cursor
	if a.full {
		// Iterate the whole ring starting at cursor (oldest).
		for i := range a.cap {
			idx := (a.cursor + i) % a.cap
			if a.buf[idx].Timestamp.IsZero() {
				continue
			}
			if matches(a.buf[idx], since, until, eventType, nodeName) {
				out = append(out, a.buf[idx])
			}
		}
	} else {
		for i := range end {
			if matches(a.buf[i], since, until, eventType, nodeName) {
				out = append(out, a.buf[i])
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Len returns the number of events currently stored.
func (a *AuditLog) Len() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.full {
		return a.cap
	}
	return a.cursor
}

func matches(e AuditEvent, since, until time.Time, eventType, nodeName string) bool {
	if !since.IsZero() && e.Timestamp.Before(since) {
		return false
	}
	if !until.IsZero() && e.Timestamp.After(until) {
		return false
	}
	if eventType != "" && e.EventType != eventType {
		return false
	}
	if nodeName != "" && e.NodeName != nodeName {
		return false
	}
	return true
}
