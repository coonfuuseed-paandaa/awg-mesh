//go:build integration

// Package wg integration tests verify that writeConfig produces a UAPI payload
// that the real amneziawg-go userspace driver accepts without error.
// These tests exercise the actual IpcSet code path, not a mock, so they catch
// format mismatches that unit tests cannot see.
//
// Run with: go test -tags integration ./pkg/wg/...
//
// Regression test for local tracker issue #117:
//   writeConfig was emitting "s3" and "s4" keys which are not recognised by
//   amneziawg-go v1.0.4's handleDeviceLine switch.  The default branch returns
//   ipcErrorf(IpcErrorInvalid, "invalid UAPI device key: s3") → errno=-22 (EINVAL).
package wg

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/amnezia-vpn/amneziawg-go/conn/bindtest"
	"github.com/amnezia-vpn/amneziawg-go/device"
	"github.com/amnezia-vpn/amneziawg-go/tun/tuntest"
)

// newTestDevice creates an in-memory amneziawg-go Device backed by a ChannelTUN
// and a ChannelBind.  It requires no kernel support and runs on any OS.
func newTestDevice(t *testing.T) *device.Device {
	t.Helper()
	tun := tuntest.NewChannelTUN()
	binds := bindtest.NewChannelBinds()
	logger := device.NewLogger(device.LogLevelError, "test: ")
	dev := device.NewDevice(tun.TUN(), binds[0], logger)
	t.Cleanup(dev.Close)
	return dev
}

// ipcSetFromWriteConfig uses writeConfig to serialise cfg and feeds the result
// to dev.IpcSet.  This exercises the exact same bytes that ConfigureDevice
// would send over the Unix socket, but without needing root or a kernel module.
func ipcSetFromWriteConfig(dev *device.Device, cfg Config) error {
	var buf strings.Builder
	if err := writeConfig(&buf, cfg); err != nil {
		return fmt.Errorf("writeConfig: %w", err)
	}
	// IpcSet expects the payload without the leading "set=1\n" frame and
	// without the trailing blank line; the device adds its own framing.
	payload := buf.String()
	return dev.IpcSet(payload)
}

// randomPrivateKey generates a valid 32-byte WireGuard private key (clamped).
func randomPrivateKey(t *testing.T) Key {
	t.Helper()
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	// Clamp according to Curve25519 spec.
	raw[0] &= 248
	raw[31] &= 127
	raw[31] |= 64
	k, err := NewKey(raw)
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	return k
}

// TestIntegration_writeConfig_AWGParamsAccepted verifies that a typical
// AWG obfuscation parameter set (jc, jmin, jmax, s1, s2, h1-h4) produced by
// writeConfig is accepted by the in-process amneziawg-go driver without error.
//
// Before the fix for local tracker issue #117, writeConfig also emitted s3 and
// s4 which the driver rejects with errno=-22 (EINVAL), so this test would have
// failed on the un-patched code with:
//   IPC error -22: invalid UAPI device key: s3
func TestIntegration_writeConfig_AWGParamsAccepted(t *testing.T) {
	dev := newTestDevice(t)
	privKey := randomPrivateKey(t)

	jc := 5
	jmin := 50
	jmax := 1000
	s1 := 30
	s2 := 40
	// H values must be >4 and all distinct for amneziawg-go to accept them.
	h1 := "150000000"
	h2 := "300000000"
	h3 := "600000000"
	h4 := "1200000000"

	cfg := Config{
		PrivateKey: &privKey,
		Jc:         &jc,
		Jmin:       &jmin,
		Jmax:       &jmax,
		S1:         &s1,
		S2:         &s2,
		H1:         &h1,
		H2:         &h2,
		H3:         &h3,
		H4:         &h4,
	}

	if err := ipcSetFromWriteConfig(dev, cfg); err != nil {
		t.Fatalf("IpcSet rejected valid AWG params: %v", err)
	}
}

// TestIntegration_writeConfig_S3S4NeverSent confirms that writeConfig does NOT
// emit s3 or s4 keys, since amneziawg-go v1.0.4 treats unknown keys as EINVAL.
// This is the structural regression test: even if S3/S4 are populated in the
// Config struct, they must not appear in the UAPI wire format.
func TestIntegration_writeConfig_S3S4NeverSent(t *testing.T) {
	s1, s2, s3, s4 := 15, 20, 30, 5
	cfg := Config{
		S1: &s1,
		S2: &s2,
		S3: &s3,
		S4: &s4,
	}

	var buf strings.Builder
	if err := writeConfig(&buf, cfg); err != nil {
		t.Fatalf("writeConfig: %v", err)
	}
	output := buf.String()

	if strings.Contains(output, "s3=") {
		t.Errorf("writeConfig emitted 's3' — amneziawg-go would reject this with EINVAL\noutput:\n%s", output)
	}
	if strings.Contains(output, "s4=") {
		t.Errorf("writeConfig emitted 's4' — amneziawg-go would reject this with EINVAL\noutput:\n%s", output)
	}

	// Confirm s3/s4 also NOT accepted by real driver (drives the point home).
	dev := newTestDevice(t)
	privKey := randomPrivateKey(t)
	privHex := hex.EncodeToString(privKey[:])

	badPayload := strings.Join([]string{
		"private_key=" + privHex,
		"s3=30",
		"",
	}, "\n")
	if err := dev.IpcSet(badPayload); err == nil {
		t.Error("expected IpcSet to reject 's3' key, but it accepted it — driver may have been updated; review writeConfig suppression")
	}
}

// TestUAPI_RotatePrivateKey_PreservesPeers proves that rotating the device
// PrivateKey via IpcSet (i.e. wg.Config{PrivateKey: &newKey}) does NOT wipe
// the existing peer table. This is the fundamental correctness property required
// by tier-3 keypair rotation: the endpoint swaps its own private key while all
// masters' peer entries (identified by the old public key) remain intact on the
// endpoint side until the master updates them.
//
// This test is the proof-of-concept for the T003 handler flow:
//   cfg := wg.Config{PrivateKey: &newPrivKey}  // no peer changes
//   h.paramApplier.ApplyParams(tunnelName, cfg)
//
// If this property did not hold, ApplyParams would silently clear all peers,
// breaking every active tunnel session. See local tracker #125.
//
// Skipped on Windows (CGO/kernel-interface constraints) and in -short mode.
func TestUAPI_RotatePrivateKey_PreservesPeers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}

	// Step 1: Create a real in-process amneziawg-go device.
	dev := newTestDevice(t)

	// Step 2: Generate an initial private key and a peer keypair.
	initialPrivKey, err := GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey (initial): %v", err)
	}

	peerPrivKey, err := GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey (peer): %v", err)
	}
	peerPubKey := peerPrivKey.PublicKey()

	// Step 3: Apply initial config — private key + one peer with AllowedIPs.
	_, peerNet, err := net.ParseCIDR("10.99.99.0/24")
	if err != nil {
		t.Fatalf("ParseCIDR: %v", err)
	}
	endpoint, err := net.ResolveUDPAddr("udp", "127.0.0.1:51820")
	if err != nil {
		t.Fatalf("ResolveUDPAddr: %v", err)
	}

	initialCfg := Config{
		PrivateKey: &initialPrivKey,
		Peers: []PeerConfig{
			{
				PublicKey:  peerPubKey,
				AllowedIPs: []net.IPNet{*peerNet},
				Endpoint:   endpoint,
			},
		},
	}
	if err := ipcSetFromWriteConfig(dev, initialCfg); err != nil {
		t.Fatalf("IpcSet (initial config): %v", err)
	}

	// Confirm peer is visible after initial config.
	state0, err := ipcGetParsed(t, dev)
	if err != nil {
		t.Fatalf("IpcGet (post-initial): %v", err)
	}
	if len(state0.Peers) != 1 {
		t.Fatalf("post-initial: expected 1 peer, got %d", len(state0.Peers))
	}

	// Step 4: Rotate the device private key — only PrivateKey, no peer changes.
	newPrivKey, err := GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey (new): %v", err)
	}
	// Ensure the new key is genuinely different.
	if newPrivKey == initialPrivKey {
		t.Fatalf("new private key is identical to initial — test invalid")
	}

	rotateCfg := Config{
		PrivateKey: &newPrivKey,
	}
	if err := ipcSetFromWriteConfig(dev, rotateCfg); err != nil {
		t.Fatalf("IpcSet (rotate keypair): %v", err)
	}

	// Step 5: Read back device state and assert correctness.
	state1, err := ipcGetParsed(t, dev)
	if err != nil {
		t.Fatalf("IpcGet (post-rotation): %v", err)
	}

	// 5a: device private key must be the new one.
	if state1.PrivateKey != newPrivKey {
		t.Errorf("device PrivateKey: got %x, want %x", state1.PrivateKey, newPrivKey)
	}

	// 5b: device public key must be derived from the new private key.
	expectedPubKey := newPrivKey.PublicKey()
	if state1.PublicKey != expectedPubKey {
		t.Errorf("device PublicKey: got %x, want %x (derived from new PrivateKey)",
			state1.PublicKey, expectedPubKey)
	}

	// 5c: peer table must still contain exactly one peer.
	if len(state1.Peers) != 1 {
		t.Fatalf("peer count after rotation: got %d, want 1 — private key rotation must not wipe peers",
			len(state1.Peers))
	}

	// 5d: the peer's public key must match the original peerPubKey.
	if state1.Peers[0].PublicKey != peerPubKey {
		t.Errorf("peer[0] PublicKey: got %x, want %x", state1.Peers[0].PublicKey, peerPubKey)
	}

	// 5e: peer AllowedIPs must still contain 10.99.99.0/24.
	if len(state1.Peers[0].AllowedIPs) == 0 {
		t.Errorf("peer[0] AllowedIPs: empty after rotation, want 10.99.99.0/24")
	} else if state1.Peers[0].AllowedIPs[0].String() != "10.99.99.0/24" {
		t.Errorf("peer[0] AllowedIPs[0]: got %q, want 10.99.99.0/24",
			state1.Peers[0].AllowedIPs[0].String())
	}
}

// ipcGetParsed reads the current device state via IpcGet and parses it with
// parseDevice. IpcGet returns the raw UAPI fields without a terminating
// errno=0 line (that is added by IpcHandle for socket clients); parseDevice
// requires it, so we append it here.
func ipcGetParsed(t *testing.T, dev interface {
	IpcGet() (string, error)
}) (*Device, error) {
	t.Helper()
	raw, err := dev.IpcGet()
	if err != nil {
		return nil, fmt.Errorf("IpcGet: %w", err)
	}
	// Append the errno terminator that the UAPI socket framing normally adds.
	return parseDevice(strings.NewReader(raw + "errno=0\n"))
}

// TestIntegration_writeConfig_FullPeerRoundTrip verifies that a peer config
// with AWG params can be applied to an in-memory device end-to-end.
func TestIntegration_writeConfig_FullPeerRoundTrip(t *testing.T) {
	dev := newTestDevice(t)
	privKey := randomPrivateKey(t)

	peerRaw := make([]byte, 32)
	for i := range peerRaw {
		peerRaw[i] = byte(i + 50)
	}
	peerRaw[0] &= 248
	peerRaw[31] &= 127
	peerRaw[31] |= 64

	peerPub, err := NewKey(peerRaw)
	if err != nil {
		t.Fatalf("NewKey peer: %v", err)
	}

	jc := 3
	jmin := 40
	jmax := 150
	s1 := 25
	s2 := 35
	s3 := 10 // retained in Config, must NOT be sent via UAPI
	s4 := 5  // retained in Config, must NOT be sent via UAPI
	h1 := "200000000"
	h2 := "400000000"
	h3 := "800000000"
	h4 := "1600000000"

	endpoint, _ := net.ResolveUDPAddr("udp", "127.0.0.1:51820")
	cfg := Config{
		PrivateKey: &privKey,
		Jc:         &jc,
		Jmin:       &jmin,
		Jmax:       &jmax,
		S1:         &s1,
		S2:         &s2,
		S3:         &s3,
		S4:         &s4,
		H1:         &h1,
		H2:         &h2,
		H3:         &h3,
		H4:         &h4,
		Peers: []PeerConfig{
			{
				PublicKey: peerPub,
				Endpoint:  endpoint,
			},
		},
	}

	var wireBuf bytes.Buffer
	if err := writeConfig(&wireBuf, cfg); err != nil {
		t.Fatalf("writeConfig: %v", err)
	}

	// Confirm s3/s4 absent from wire format.
	wire := wireBuf.String()
	if strings.Contains(wire, "s3=") || strings.Contains(wire, "s4=") {
		t.Fatalf("writeConfig emitted s3 or s4 in wire format:\n%s", wire)
	}

	// Apply to real in-memory device.
	if err := dev.IpcSet(wire); err != nil {
		t.Fatalf("IpcSet with full peer config failed: %v\nwire payload:\n%s", err, wire)
	}
}
