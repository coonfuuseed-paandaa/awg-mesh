//go:build linux

package node

import (
	"sync"
	"testing"
)

func TestAddPeerConcurrentSamePubkey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, _, err := EnsureKeypair(dir)
	if err != nil {
		t.Fatalf("EnsureKeypair: %v", err)
	}

	runner := NewClientRunner(&Node{config: NodeConfig{ConfigDir: dir}})

	// Generate a fake peer public key (32 bytes)
	peerKey := make([]byte, 32)
	for i := range peerKey {
		peerKey[i] = byte(i + 1)
	}

	// First AddPeer creates the interface — may fail on non-privileged, skip in that case
	err = runner.AddPeer(peerKey, nil, []string{"0.0.0.0/0"}, "192.168.1.1:51820", 25)
	if err != nil {
		t.Skipf("AddPeer requires TUN device (privileged): %v", err)
	}

	// Now fire concurrent AddPeer calls for the SAME pubkey — exercises the
	// existing-link path where the race condition was fixed (mutex held across
	// configurePeerOnIface).
	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make([]error, goroutines)

	for i := range goroutines {
		go func(idx int) {
			defer wg.Done()
			errs[idx] = runner.AddPeer(peerKey, nil, []string{"0.0.0.0/0"}, "192.168.1.1:51820", 25)
		}(i)
	}
	wg.Wait()

	// All should succeed (reconfigure existing peer) — no panic, no race
	for i, e := range errs {
		if e != nil {
			t.Errorf("concurrent AddPeer[%d] failed: %v", i, e)
		}
	}
}

func TestListPeersConcurrentClose(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, _, err := EnsureKeypair(dir)
	if err != nil {
		t.Fatalf("EnsureKeypair: %v", err)
	}

	runner := NewClientRunner(&Node{config: NodeConfig{ConfigDir: dir}})

	// Generate peer key
	peerKey := make([]byte, 32)
	for i := range peerKey {
		peerKey[i] = byte(i + 1)
	}

	// Add a peer first
	err = runner.AddPeer(peerKey, nil, []string{"0.0.0.0/0"}, "192.168.1.1:51820", 25)
	if err != nil {
		t.Skipf("AddPeer requires TUN device (privileged): %v", err)
	}

	// ListPeers while removing the peer concurrently — must not panic
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		// ListPeers should handle concurrent interface close gracefully
		_ = runner.ListPeers()
	}()

	go func() {
		defer wg.Done()
		_ = runner.RemovePeer(peerKey)
	}()

	wg.Wait()

	// If we get here without panic, the test passes
}
