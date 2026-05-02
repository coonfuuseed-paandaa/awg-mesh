package control_plane

import (
	"sync"
)

// peerListSubscriber + ownershipSubscriber receive streamed updates. Each
// subscriber has a small buffered channel; if the consumer falls behind the
// channel, the subscription is dropped (slow-consumer policy: protect the
// daemon from blocking on a misbehaving client).

const subscriberBuffer = 8

// PeerListUpdateMsg is the typed payload pushed to peer-list subscribers.
type PeerListUpdateMsg struct {
	SubjectNode string
	Snapshot    []OwnershipEntry // we reuse the ledger snapshot — peer composition derives from this
	Version     int64
}

type peerListSubscriber struct {
	ch chan PeerListUpdateMsg
}

// PeerListBroadcaster fans out ledger mutations to peer-list subscribers.
// StreamPeerList builds the subject-specific peer-list payload for each
// subscriber at send time.
type PeerListBroadcaster struct {
	mu          sync.Mutex
	subscribers map[string]*peerListSubscriber // key: subscription ID
	nextID      int
}

// NewPeerListBroadcaster constructs an empty broadcaster.
func NewPeerListBroadcaster() *PeerListBroadcaster {
	return &PeerListBroadcaster{subscribers: make(map[string]*peerListSubscriber)}
}

// Subscribe registers a new consumer. Returns the receive channel and a
// cancel func that drops the subscription.
func (b *PeerListBroadcaster) Subscribe(subjectNode string) (<-chan PeerListUpdateMsg, func()) {
	b.mu.Lock()
	id := b.nextSubID()
	sub := &peerListSubscriber{ch: make(chan PeerListUpdateMsg, subscriberBuffer)}
	b.subscribers[id] = sub
	b.mu.Unlock()
	return sub.ch, func() {
		b.mu.Lock()
		if s, ok := b.subscribers[id]; ok {
			close(s.ch)
			delete(b.subscribers, id)
		}
		b.mu.Unlock()
	}
}

// Broadcast pushes a snapshot to every subscriber.
// Subscribers with full buffers are dropped (slow-consumer protection).
func (b *PeerListBroadcaster) Broadcast(msg PeerListUpdateMsg) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, sub := range b.subscribers {
		select {
		case sub.ch <- msg:
		default:
			// Slow consumer: drop.
			close(sub.ch)
			delete(b.subscribers, id)
		}
	}
}

// SubscriberCount returns the number of active subscribers (test hook).
func (b *PeerListBroadcaster) SubscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subscribers)
}

func (b *PeerListBroadcaster) nextSubID() string {
	b.nextID++
	return idString(b.nextID)
}

// OwnershipUpdateMsg is the typed payload pushed to ownership-ledger subscribers.
type OwnershipUpdateMsg struct {
	Entries      []OwnershipEntry
	Version      int64
	FullSnapshot bool
}

type ownershipSubscriber struct {
	ch chan OwnershipUpdateMsg
}

// OwnershipBroadcaster fans out ledger mutations to ownership-ledger subscribers.
// Every subscriber receives every update — there is no per-node filtering.
type OwnershipBroadcaster struct {
	mu          sync.Mutex
	subscribers map[string]*ownershipSubscriber
	nextID      int
}

// NewOwnershipBroadcaster constructs an empty broadcaster.
func NewOwnershipBroadcaster() *OwnershipBroadcaster {
	return &OwnershipBroadcaster{subscribers: make(map[string]*ownershipSubscriber)}
}

// Subscribe registers a new consumer.
func (b *OwnershipBroadcaster) Subscribe() (<-chan OwnershipUpdateMsg, func()) {
	b.mu.Lock()
	id := b.nextSubID()
	sub := &ownershipSubscriber{ch: make(chan OwnershipUpdateMsg, subscriberBuffer)}
	b.subscribers[id] = sub
	b.mu.Unlock()
	return sub.ch, func() {
		b.mu.Lock()
		if s, ok := b.subscribers[id]; ok {
			close(s.ch)
			delete(b.subscribers, id)
		}
		b.mu.Unlock()
	}
}

// Broadcast pushes a snapshot to every subscriber.
func (b *OwnershipBroadcaster) Broadcast(msg OwnershipUpdateMsg) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, sub := range b.subscribers {
		select {
		case sub.ch <- msg:
		default:
			close(sub.ch)
			delete(b.subscribers, id)
		}
	}
}

// SubscriberCount is a test hook.
func (b *OwnershipBroadcaster) SubscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subscribers)
}

func (b *OwnershipBroadcaster) nextSubID() string {
	b.nextID++
	return idString(b.nextID)
}

// Tiny helper — int → short ASCII id. Used only as a map key.
func idString(n int) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	if n == 0 {
		return "0"
	}
	out := make([]byte, 0, 8)
	for n > 0 {
		out = append(out, digits[n%36])
		n /= 36
	}
	// Reverse.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}
