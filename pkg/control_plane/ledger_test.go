package control_plane

import (
	"errors"
	"testing"
)

func TestLedger_ReassignAndLookup(t *testing.T) {
	l := NewLedger()
	v, err := l.Reassign("172.21.92.34", "master-01", "scheduled")
	if err != nil {
		t.Fatalf("Reassign: %v", err)
	}
	if v != 1 {
		t.Fatalf("first Reassign expected version 1, got %d", v)
	}
	got, ok := l.Lookup("172.21.92.34")
	if !ok {
		t.Fatalf("Lookup missing")
	}
	if got.OwningMaster != "master-01" || got.Reason != "scheduled" {
		t.Fatalf("unexpected entry: %+v", got)
	}
	v2, _ := l.Reassign("172.21.92.34", "master-02", "failover")
	if v2 != 2 {
		t.Fatalf("second Reassign expected version 2, got %d", v2)
	}
	got, _ = l.Lookup("172.21.92.34")
	if got.PreviousOwner != "master-01" {
		t.Fatalf("PreviousOwner = %q, want master-01", got.PreviousOwner)
	}
}

func TestLedger_RejectsBadInput(t *testing.T) {
	l := NewLedger()
	if _, err := l.Reassign("not-an-ip", "master-01", "x"); !errors.Is(err, ErrLedgerOverlayInvalid) {
		t.Fatalf("expected ErrLedgerOverlayInvalid, got %v", err)
	}
	if _, err := l.Reassign("2001:db8::1", "master-01", "x"); !errors.Is(err, ErrLedgerOverlayInvalid) {
		t.Fatalf("expected ErrLedgerOverlayInvalid for IPv6, got %v", err)
	}
	if _, err := l.Reassign("172.21.92.34", "", "x"); !errors.Is(err, ErrLedgerEmptyMaster) {
		t.Fatalf("expected ErrLedgerEmptyMaster, got %v", err)
	}
}

func TestLedger_OwnedByAndDrain(t *testing.T) {
	l := NewLedger()
	mustReassign(t, l, "172.21.92.10", "master-A", "scheduled")
	mustReassign(t, l, "172.21.92.11", "master-A", "scheduled")
	mustReassign(t, l, "172.21.92.12", "master-B", "scheduled")

	owned := l.OwnedBy("master-A")
	if len(owned) != 2 {
		t.Fatalf("OwnedBy(master-A) = %d, want 2", len(owned))
	}
	if owned[0].OverlayIP != "172.21.92.10" || owned[1].OverlayIP != "172.21.92.11" {
		t.Fatalf("OwnedBy not sorted: %+v", owned)
	}

	count, err := l.Drain("master-A", "failover", func(string) string { return "master-B" })
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if count != 2 {
		t.Fatalf("Drain count = %d, want 2", count)
	}
	if len(l.OwnedBy("master-A")) != 0 {
		t.Fatalf("master-A still owns entries after drain")
	}
	if len(l.OwnedBy("master-B")) != 3 {
		t.Fatalf("master-B should own 3 after drain, got %d", len(l.OwnedBy("master-B")))
	}
}

func TestLedger_DrainSkipsOverlayMovedAfterSnapshot(t *testing.T) {
	l := NewLedger()
	mustReassign(t, l, "172.21.92.10", "master-A", "scheduled")

	count, err := l.Drain("master-A", "drain", func(overlayIP string) string {
		if _, err := l.Reassign(overlayIP, "master-C", "failover"); err != nil {
			t.Fatalf("Reassign during chooser: %v", err)
		}
		return "master-B"
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if count != 0 {
		t.Fatalf("Drain count = %d, want 0", count)
	}
	got, ok := l.Lookup("172.21.92.10")
	if !ok || got.OwningMaster != "master-C" {
		t.Fatalf("drain stole concurrent owner: %+v", got)
	}
}

func TestLedger_Remove(t *testing.T) {
	l := NewLedger()
	mustReassign(t, l, "172.21.92.34", "master-01", "scheduled")
	if err := l.Remove("172.21.92.34"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := l.Lookup("172.21.92.34"); ok {
		t.Fatalf("entry not removed")
	}
	if err := l.Remove("172.21.92.34"); !errors.Is(err, ErrLedgerNotFound) {
		t.Fatalf("expected ErrLedgerNotFound on second remove, got %v", err)
	}
}

func TestLedger_ListenerFires(t *testing.T) {
	l := NewLedger()
	calls := 0
	var lastVersion int64
	l.SetListener(listenerFn(func(snap []OwnershipEntry, version int64) {
		calls++
		lastVersion = version
		_ = snap
	}))
	mustReassign(t, l, "172.21.92.34", "master-01", "scheduled")
	mustReassign(t, l, "172.21.92.35", "master-01", "scheduled")
	if calls != 2 {
		t.Fatalf("listener calls = %d, want 2", calls)
	}
	if lastVersion != 2 {
		t.Fatalf("listener lastVersion = %d, want 2", lastVersion)
	}
}

type listenerFn func([]OwnershipEntry, int64)

func (f listenerFn) OnLedgerMutation(s []OwnershipEntry, v int64) { f(s, v) }

func mustReassign(t *testing.T, l *Ledger, ip, owner, reason string) {
	t.Helper()
	if _, err := l.Reassign(ip, owner, reason); err != nil {
		t.Fatalf("Reassign(%s, %s): %v", ip, owner, err)
	}
}
