package node

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/transport"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
)

func TestEndpointReconcilePeerKey(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	peerKeyHex := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	peerKeyBytes, err := hex.DecodeString(peerKeyHex)
	if err != nil {
		t.Fatalf("DecodeString returned error: %v", err)
	}

	state := transport.NodeTransportState{
		SchemaVersion: transport.CurrentSchemaVersion,
		Tunnels: []transport.TunnelTransport{
			{
				Name:                "endpoint-a",
				TransportIP:         "10.200.0.1",
				PeerTransportIP:     "10.200.0.2",
				PeerPublicKey:       peerKeyHex,
				PeerEndpoint:        "198.51.100.10:51820",
				AllowedIPs:          []string{"10.200.0.2/32"},
				PersistentKeepalive: 25,
			},
		},
	}
	if err := saveNodeTransportState(configDir, state); err != nil {
		t.Fatalf("saveNodeTransportState returned error: %v", err)
	}

	readBack, err := loadNodeTransportState(configDir)
	if err != nil {
		t.Fatalf("loadNodeTransportState returned error: %v", err)
	}
	if len(readBack.Tunnels) != 1 {
		t.Fatalf("expected 1 tunnel, got %d", len(readBack.Tunnels))
	}

	reconciledPeerBytes, err := hex.DecodeString(strings.TrimSpace(readBack.Tunnels[0].PeerPublicKey))
	if err != nil {
		t.Fatalf("DecodeString returned error: %v", err)
	}
	if len(reconciledPeerBytes) != 32 {
		t.Fatalf("expected 32 decoded bytes, got %d", len(reconciledPeerBytes))
	}
	if !bytes.Equal(reconciledPeerBytes, peerKeyBytes) {
		t.Fatalf("decoded bytes mismatch: got %x want %x", reconciledPeerBytes, peerKeyBytes)
	}

	reconciledPeerKey, err := wg.NewKey(reconciledPeerBytes)
	if err != nil {
		t.Fatalf("NewKey returned error: %v", err)
	}
	if !bytes.Equal(reconciledPeerKey[:], peerKeyBytes) {
		t.Fatalf("reconciled key mismatch: got %x want %x", reconciledPeerKey[:], peerKeyBytes)
	}

	// Regression guard for local tracker issue #94:
	// wg.ParseKey (base64 decoder) on the same string must NOT yield 32 bytes.
	// If a future refactor changes the storage encoding without updating readers,
	// this check will catch it.
	if _, err := wg.ParseKey(readBack.Tunnels[0].PeerPublicKey); err == nil {
		t.Fatalf("regression guard: hex-encoded key was unexpectedly parseable as base64 -> encoding contract silently changed")
	}
}
