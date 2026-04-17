package transport

import (
	"bytes"
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
)

func TestNodeTransportStatePeerPublicKeyHexEncoding(t *testing.T) {
	t.Parallel()

	originalPeerKeyHex := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	originalPeerKeyBytes, err := hex.DecodeString(originalPeerKeyHex)
	if err != nil {
		t.Fatalf("DecodeString returned error: %v", err)
	}
	if len(originalPeerKeyBytes) != 32 {
		t.Fatalf("expected 32-byte test key, got %d", len(originalPeerKeyBytes))
	}

	state := NodeTransportState{
		SchemaVersion: CurrentSchemaVersion,
		OverlayIP:     "10.20.30.40",
		Tunnels: []TunnelTransport{
			{
				Name:                "endpoint-a",
				TransportIP:         "10.200.0.1",
				PeerTransportIP:     "10.200.0.2",
				PeerPublicKey:       originalPeerKeyHex,
				PeerEndpoint:        "198.51.100.10:51820",
				AllowedIPs:          []string{"10.200.0.2/32"},
				PersistentKeepalive: 25,
			},
		},
	}

	dir := t.TempDir()
	if err := SaveNodeTransportState(filepath.Join(dir, "transport.yml"), state); err != nil {
		t.Fatalf("SaveNodeTransportState returned error: %v", err)
	}

	readBack, err := LoadNodeTransportState(dir)
	if err != nil {
		t.Fatalf("LoadNodeTransportState returned error: %v", err)
	}
	if len(readBack.Tunnels) != 1 {
		t.Fatalf("expected 1 tunnel, got %d", len(readBack.Tunnels))
	}

	decodedPeerKey, err := hex.DecodeString(readBack.Tunnels[0].PeerPublicKey)
	if err != nil {
		t.Fatalf("DecodeString returned error: %v", err)
	}
	if len(decodedPeerKey) != 32 {
		t.Fatalf("expected 32 decoded bytes, got %d", len(decodedPeerKey))
	}
	if !bytes.Equal(decodedPeerKey, originalPeerKeyBytes) {
		t.Fatalf("decoded bytes mismatch: got %x want %x", decodedPeerKey, originalPeerKeyBytes)
	}

	// Regression guard for local tracker issue #94:
	// wg.ParseKey (base64 decoder) on the same string must NOT yield 32 bytes.
	// If a future refactor changes the storage encoding without updating readers,
	// this check will catch it.
	if _, err := wg.ParseKey(readBack.Tunnels[0].PeerPublicKey); err == nil {
		t.Fatalf("regression guard: hex-encoded key was unexpectedly parseable as base64 -> encoding contract silently changed")
	}
}
