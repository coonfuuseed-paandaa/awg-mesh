package control_plane

import (
	"testing"
	"time"
)

func TestPeerListBroadcaster_FanOut(t *testing.T) {
	b := NewPeerListBroadcaster()
	chN1, cancelN1 := b.Subscribe("n1")
	defer cancelN1()
	chN2, cancelN2 := b.Subscribe("n2")
	defer cancelN2()

	b.Broadcast(PeerListUpdateMsg{SubjectNode: "n1", Version: 1})
	for _, ch := range []<-chan PeerListUpdateMsg{chN1, chN2} {
		select {
		case msg := <-ch:
			if msg.Version != 1 {
				t.Fatalf("version = %d, want 1", msg.Version)
			}
		case <-time.After(1 * time.Second):
			t.Fatal("subscriber timeout")
		}
	}
}

func TestPeerListBroadcaster_SlowConsumerDropped(t *testing.T) {
	b := NewPeerListBroadcaster()
	_, _ = b.Subscribe("slow")
	// Don't drain — fill buffer + 1.
	for i := range subscriberBuffer + 5 {
		b.Broadcast(PeerListUpdateMsg{SubjectNode: "slow", Version: int64(i)})
	}
	if b.SubscriberCount() != 0 {
		t.Fatalf("slow consumer should have been dropped, got %d subs", b.SubscriberCount())
	}
}

func TestOwnershipBroadcaster_FanOut(t *testing.T) {
	b := NewOwnershipBroadcaster()
	ch1, c1 := b.Subscribe()
	defer c1()
	ch2, c2 := b.Subscribe()
	defer c2()

	b.Broadcast(OwnershipUpdateMsg{Version: 42, FullSnapshot: false})
	for _, ch := range []<-chan OwnershipUpdateMsg{ch1, ch2} {
		select {
		case msg := <-ch:
			if msg.Version != 42 {
				t.Fatalf("version = %d, want 42", msg.Version)
			}
		case <-time.After(1 * time.Second):
			t.Fatal("subscriber timeout")
		}
	}
}

func TestIDStringMonotonic(t *testing.T) {
	a := idString(1)
	b := idString(36)
	c := idString(37)
	if a == b || b == c || a == c {
		t.Fatalf("idString collisions: %q %q %q", a, b, c)
	}
}
