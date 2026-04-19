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

// TestUAPI_RotatePrivateKey_PreservesPeers proves that sending a PrivateKey-only
// UAPI payload to an amneziawg-go device replaces the device private key while
// leaving the pre-existing peer table intact.
//
// This is the UAPI contract that RotateKeypair (pkg/grpc) relies on: issuing
// ApplyParams with only PrivateKey set must NOT remove any peers, because
// writeConfig only emits "private_key=<hex>" with no "replace_peers=true" flag
// when the Peers slice is empty (local tracker #125).
func TestUAPI_RotatePrivateKey_PreservesPeers(t *testing.T) {
	dev := newTestDevice(t)

	// --- Step 1: create initial keypair K1 and peer P1 ---
	k1 := randomPrivateKey(t)

	// Build a distinct peer key P1 (different byte pattern from k1).
	peerRaw := make([]byte, 32)
	for i := range peerRaw {
		peerRaw[i] = byte(i + 100)
	}
	peerRaw[0] &= 248
	peerRaw[31] &= 127
	peerRaw[31] |= 64
	p1, err := NewKey(peerRaw)
	if err != nil {
		t.Fatalf("NewKey peer: %v", err)
	}

	// Apply K1 + P1 to device.
	initialCfg := Config{
		PrivateKey: &k1,
		Peers: []PeerConfig{
			{PublicKey: p1},
		},
	}
	if err := ipcSetFromWriteConfig(dev, initialCfg); err != nil {
		t.Fatalf("initial IpcSet (K1+P1): %v", err)
	}

	// Verify device has K1 and P1 before rotation.
	state1, err := dev.IpcGet()
	if err != nil {
		t.Fatalf("IpcGet after initial config: %v", err)
	}
	k1Hex := hex.EncodeToString(k1[:])
	p1Hex := hex.EncodeToString(p1[:])
	if !strings.Contains(state1, "private_key="+k1Hex) {
		t.Fatalf("device state does not contain K1 private key after initial config:\n%s", state1)
	}
	if !strings.Contains(state1, "public_key="+p1Hex) {
		t.Fatalf("device state does not contain P1 public key after initial config:\n%s", state1)
	}

	// --- Step 2: rotate to K2 with no peer changes ---
	k2Raw := make([]byte, 32)
	for i := range k2Raw {
		k2Raw[i] = byte(i + 200)
	}
	k2Raw[0] &= 248
	k2Raw[31] &= 127
	k2Raw[31] |= 64
	k2, err := NewKey(k2Raw)
	if err != nil {
		t.Fatalf("NewKey K2: %v", err)
	}

	// PrivateKey-only config — no Peers field, no replace_peers in wire format.
	rotateCfg := Config{PrivateKey: &k2}
	if err := ipcSetFromWriteConfig(dev, rotateCfg); err != nil {
		t.Fatalf("rotation IpcSet (K2 only): %v", err)
	}

	// --- Step 3: verify K2 is active AND P1 is still present ---
	state2, err := dev.IpcGet()
	if err != nil {
		t.Fatalf("IpcGet after rotation: %v", err)
	}
	k2Hex := hex.EncodeToString(k2[:])

	if !strings.Contains(state2, "private_key="+k2Hex) {
		t.Errorf("after rotation: device private_key is not K2\nstate:\n%s", state2)
	}
	if strings.Contains(state2, "private_key="+k1Hex) {
		t.Errorf("after rotation: device still reports old K1 private_key\nstate:\n%s", state2)
	}
	// P1 must still be listed as a peer — rotation must NOT wipe peers.
	if !strings.Contains(state2, "public_key="+p1Hex) {
		t.Errorf("after rotation: peer P1 was dropped — PrivateKey-only UAPI set must not remove peers\nstate:\n%s", state2)
	}
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
