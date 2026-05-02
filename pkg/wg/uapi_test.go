package wg

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteConfigSuccess(t *testing.T) {
	t.Parallel()

	privateKey := mustKeyForUAPI(t, filledBytesUAPI(0x01))
	listenPort := 51820
	fwmark := 42
	jc := 3
	h1 := "111"
	keepalive := 25 * time.Second
	preshared := mustKeyForUAPI(t, filledBytesUAPI(0x02))

	cfg := Config{
		PrivateKey:   &privateKey,
		ListenPort:   &listenPort,
		FirewallMark: &fwmark,
		ReplacePeers: true,
		Jc:           &jc,
		H1:           &h1,
		Peers: []PeerConfig{
			{
				PublicKey:                   mustKeyForUAPI(t, filledBytesUAPI(0x03)),
				Remove:                      true,
				UpdateOnly:                  true,
				PresharedKey:                &preshared,
				Endpoint:                    mustUDPForUAPI(t, "127.0.0.1:51821"),
				PersistentKeepaliveInterval: &keepalive,
				ReplaceAllowedIPs:           true,
				AllowedIPs: []net.IPNet{
					mustCIDRForUAPI(t, "10.0.0.2/32"),
				},
			},
		},
	}

	var buffer strings.Builder
	if err := writeConfig(&buffer, cfg); err != nil {
		t.Fatalf("writeConfig returned error: %v", err)
	}

	lines := nonEmptyLines(buffer.String())
	lineSet := make(map[string]int, len(lines))
	for _, line := range lines {
		lineSet[line]++
	}

	expectedLines := []string{
		"private_key=" + hexKey(privateKey),
		"listen_port=51820",
		"fwmark=42",
		"replace_peers=true",
		"jc=3",
		"h1=111",
		"public_key=" + hexKey(cfg.Peers[0].PublicKey),
		"remove=true",
		"update_only=true",
		"preshared_key=" + hexKey(preshared),
		"endpoint=127.0.0.1:51821",
		"persistent_keepalive_interval=25",
		"replace_allowed_ips=true",
		"allowed_ip=10.0.0.2/32",
	}

	for _, expected := range expectedLines {
		if lineSet[expected] == 0 {
			t.Fatalf("expected line %q, got output:\n%s", expected, buffer.String())
		}
	}
}

// TestWriteConfigS3S4NotEmitted asserts that S3 and S4 are silently dropped
// from the UAPI payload. amneziawg-go v1.0.4 only recognises s1/s2 in its
// handleDeviceLine switch; any unrecognised key returns errno=-22 (EINVAL).
// Regression test for local tracker issue #117.
func TestWriteConfigS3S4NotEmitted(t *testing.T) {
	t.Parallel()

	s1, s2, s3, s4 := 15, 20, 30, 5
	cfg := Config{
		S1: &s1,
		S2: &s2,
		S3: &s3,
		S4: &s4,
	}

	var buf strings.Builder
	if err := writeConfig(&buf, cfg); err != nil {
		t.Fatalf("writeConfig returned error: %v", err)
	}

	output := buf.String()
	if strings.Contains(output, "s3=") {
		t.Errorf("writeConfig emitted s3 but amneziawg-go UAPI does not accept it; would cause errno=-22\noutput:\n%s", output)
	}
	if strings.Contains(output, "s4=") {
		t.Errorf("writeConfig emitted s4 but amneziawg-go UAPI does not accept it; would cause errno=-22\noutput:\n%s", output)
	}
	if !strings.Contains(output, "s1=15\n") {
		t.Errorf("writeConfig missing s1; output:\n%s", output)
	}
	if !strings.Contains(output, "s2=20\n") {
		t.Errorf("writeConfig missing s2; output:\n%s", output)
	}
}

func TestWriteConfigRequiresPeerPublicKey(t *testing.T) {
	t.Parallel()

	err := writeConfig(&strings.Builder{}, Config{
		Peers: []PeerConfig{{}},
	})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "peer[0] public key is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestParseDeviceSuccess feeds parseDevice a UAPI response that mirrors what
// amneziawg-go::IpcGetOperation actually emits — NO device-side public_key
// line. Device.PublicKey is derived from Device.PrivateKey by the parser.
//
// Pre-fix bug: the parser had a `seenDevicePublicKey` flag that captured the
// FIRST public_key= line as Device.PublicKey, dropping the only peer when
// N=1. This test would have caught the bug if it had used a realistic
// response (the old version of this test added a fake device-side
// public_key= line that WG UAPI never sends, masking the bug).
func TestParseDeviceSuccess(t *testing.T) {
	t.Parallel()

	privateKey := mustKeyForUAPI(t, filledBytesUAPI(0x10))
	peerPublicKey := mustKeyForUAPI(t, filledBytesUAPI(0x12))
	peerPresharedKey := mustKeyForUAPI(t, filledBytesUAPI(0x13))

	response := strings.Join([]string{
		"private_key=" + hexKey(privateKey),
		"listen_port=51820",
		"fwmark=77",
		"s1=9",
		"public_key=" + hexKey(peerPublicKey),
		"preshared_key=" + hexKey(peerPresharedKey),
		"protocol_version=1",
		"endpoint=10.0.0.2:12345",
		"persistent_keepalive_interval=15",
		"last_handshake_time_sec=1700000000",
		"last_handshake_time_nsec=10",
		"rx_bytes=123",
		"tx_bytes=456",
		"allowed_ip=10.10.0.0/16",
		"errno=0",
	}, "\n") + "\n"

	device, err := parseDevice(strings.NewReader(response))
	if err != nil {
		t.Fatalf("parseDevice returned error: %v", err)
	}

	if device.PrivateKey != privateKey {
		t.Fatalf("unexpected private key")
	}
	// Device.PublicKey must be derived from PrivateKey via curve25519
	// scalar multiplication (WG UAPI does not emit it).
	expectedDevicePub := privateKey.PublicKey()
	if device.PublicKey != expectedDevicePub {
		t.Fatalf("device public key not derived from private key: got %x, want %x",
			device.PublicKey, expectedDevicePub)
	}
	if device.ListenPort != 51820 {
		t.Fatalf("unexpected listen port: %d", device.ListenPort)
	}
	if device.FirewallMark != 77 {
		t.Fatalf("unexpected fwmark: %d", device.FirewallMark)
	}
	if !device.IsAmnezia {
		t.Fatalf("expected IsAmnezia to be true")
	}
	if len(device.Peers) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(device.Peers))
	}

	peer := device.Peers[0]
	if peer.PublicKey != peerPublicKey {
		t.Fatalf("unexpected peer public key")
	}
	if peer.PresharedKey != peerPresharedKey {
		t.Fatalf("unexpected peer preshared key")
	}
	if peer.Endpoint == nil || peer.Endpoint.String() != "10.0.0.2:12345" {
		t.Fatalf("unexpected peer endpoint: %#v", peer.Endpoint)
	}
	if peer.PersistentKeepaliveInterval != 15*time.Second {
		t.Fatalf("unexpected keepalive: %s", peer.PersistentKeepaliveInterval)
	}
	if peer.ReceiveBytes != 123 || peer.TransmitBytes != 456 {
		t.Fatalf("unexpected traffic counters: rx=%d tx=%d", peer.ReceiveBytes, peer.TransmitBytes)
	}
	if len(peer.AllowedIPs) != 1 || peer.AllowedIPs[0].String() != "10.10.0.0/16" {
		t.Fatalf("unexpected allowed IPs: %#v", peer.AllowedIPs)
	}

	expectedHandshake := time.Unix(1700000000, 10).UTC()
	if !peer.LastHandshakeTime.Equal(expectedHandshake) {
		t.Fatalf("unexpected handshake time: got %s want %s", peer.LastHandshakeTime, expectedHandshake)
	}
}

// TestParseDevice_PeerCounts is the explicit regression test for engram #128
// (parseDevice silently dropped the first peer when N=1). It runs the parser
// against responses with 0, 1, 2, and 3 peers and asserts every peer survives.
//
// Pre-fix behavior:
//   - N=0: PASS (no peers to lose)
//   - N=1: FAIL (the only peer was misclassified as device pubkey)
//   - N=2: FAIL on PublicKey check (second peer in slot 0)
//   - N=3: FAIL on PublicKey check (third peer in slot 1)
//
// The bug masked itself in production because:
//   - mesh-ctl inspect uses tunnelMgr.ListTunnels() (in-memory, not UAPI)
//   - applyPeerKeyUpdate failed silently — operators saw "key mismatch" errors
//   - endpoint.ListPeers always returned 0 peers, but no test asserted otherwise
func TestParseDevice_PeerCounts(t *testing.T) {
	t.Parallel()

	devicePriv := mustKeyForUAPI(t, filledBytesUAPI(0x20))
	mkPeer := func(seed byte) Key {
		return mustKeyForUAPI(t, filledBytesUAPI(seed))
	}

	tests := []struct {
		name     string
		peerKeys []Key
	}{
		{name: "N=0 (no peers)", peerKeys: nil},
		{name: "N=1 (the regression case)", peerKeys: []Key{mkPeer(0x30)}},
		{name: "N=2", peerKeys: []Key{mkPeer(0x40), mkPeer(0x41)}},
		{name: "N=3", peerKeys: []Key{mkPeer(0x50), mkPeer(0x51), mkPeer(0x52)}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			lines := []string{
				"private_key=" + hexKey(devicePriv),
				"listen_port=51820",
			}
			for i, pk := range tc.peerKeys {
				lines = append(lines,
					"public_key="+hexKey(pk),
					"protocol_version=1",
					"allowed_ip=10."+itoa(i)+".0.0/24",
				)
			}
			lines = append(lines, "errno=0")
			response := strings.Join(lines, "\n") + "\n"

			device, err := parseDevice(strings.NewReader(response))
			if err != nil {
				t.Fatalf("parseDevice returned error: %v", err)
			}
			if len(device.Peers) != len(tc.peerKeys) {
				t.Fatalf("peer count: got %d, want %d", len(device.Peers), len(tc.peerKeys))
			}
			for i, want := range tc.peerKeys {
				if device.Peers[i].PublicKey != want {
					t.Fatalf("peer[%d] PublicKey: got %x, want %x",
						i, device.Peers[i].PublicKey, want)
				}
			}
			// Device pubkey is always derived, regardless of peer count.
			if device.PublicKey != devicePriv.PublicKey() {
				t.Fatalf("device PublicKey not derived from PrivateKey")
			}
		})
	}
}

// itoa is a tiny local helper (avoids strconv just for one digit in a test).
func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	// Test inputs never exceed 9 — keep it trivial.
	return "x"
}

func TestParseDeviceErrors(t *testing.T) {
	t.Parallel()

	validKey := hexKey(mustKeyForUAPI(t, filledBytesUAPI(0xAA)))

	tests := []struct {
		name        string
		response    string
		expectError string
	}{
		{
			name:        "invalid line",
			response:    "badline\nerrno=0\n",
			expectError: "invalid uapi line",
		},
		{
			name:        "invalid errno parse",
			response:    "errno=nope\n",
			expectError: "invalid errno value",
		},
		{
			name:        "non zero errno",
			response:    "errno=5\n",
			expectError: "uapi errno=5",
		},
		{
			name:        "invalid private key",
			response:    "private_key=zz\nerrno=0\n",
			expectError: "parse private_key",
		},
		{
			name:        "invalid endpoint",
			response:    "public_key=" + validKey + "\npublic_key=" + validKey + "\nendpoint=bad endpoint\nerrno=0\n",
			expectError: "parse endpoint",
		},
		{
			name:        "missing errno",
			response:    "public_key=" + validKey + "\n",
			expectError: "before errno",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := parseDevice(strings.NewReader(tt.response))
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.expectError) {
				t.Fatalf("expected error containing %q, got %v", tt.expectError, err)
			}
		})
	}
}

func TestParseHexKey(t *testing.T) {
	t.Parallel()

	original := mustKeyForUAPI(t, filledBytesUAPI(0x6A))
	parsed, err := parseHexKey(hexKey(original))
	if err != nil {
		t.Fatalf("parseHexKey returned error: %v", err)
	}
	if parsed != original {
		t.Fatalf("unexpected parsed key")
	}

	_, err = parseHexKey("not-hex")
	if err == nil {
		t.Fatalf("expected parseHexKey error, got nil")
	}
}

func TestReadErrno(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		input       string
		expectError string
	}{
		{name: "success", input: "errno=0\n", expectError: ""},
		{name: "non-zero", input: "errno=10\n", expectError: "uapi errno=10"},
		{name: "invalid", input: "errno=nan\n", expectError: "invalid errno value"},
		{name: "missing", input: "ok=1\n", expectError: "missing errno"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := readErrno(bytes.NewBufferString(tt.input))
			if tt.expectError == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}

			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.expectError) {
				t.Fatalf("expected error containing %q, got %v", tt.expectError, err)
			}
		})
	}
}

func TestUAPIClientOptions(t *testing.T) {
	t.Parallel()

	client := NewUAPIClient()
	if client.socketDir != defaultUAPISocketDir {
		t.Fatalf("unexpected default socket dir: %q", client.socketDir)
	}

	custom := NewUAPIClient(WithSocketDir("/tmp/custom"))
	if custom.socketDir != "/tmp/custom" {
		t.Fatalf("unexpected custom socket dir: %q", custom.socketDir)
	}
}

func TestConfigureDeviceAndDeviceRequireValidName(t *testing.T) {
	t.Parallel()

	client := NewUAPIClient()
	for _, name := range []string{"", "wg/name", `wg\\name`, "wg..name", "0123456789abcdef"} {
		if err := client.ConfigureDevice(name, Config{}); err == nil {
			t.Fatalf("expected ConfigureDevice name validation error for %q", name)
		}

		_, err := client.Device(name)
		if err == nil {
			t.Fatalf("expected Device name validation error for %q", name)
		}
	}
}

func TestConfigureDeviceAndDeviceOverUnixSocket(t *testing.T) {
	t.Parallel()

	socketDir := t.TempDir()
	deviceName := "wg-test"
	socketPath := filepath.Join(socketDir, deviceName+".sock")

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Skipf("unix sockets unavailable on this platform: %v", err)
	}
	defer func() { _ = listener.Close() }()

	done := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			done <- acceptErr
			return
		}
		defer func() { _ = conn.Close() }()

		reader := bufio.NewReader(conn)
		var requestBuilder strings.Builder
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				done <- readErr
				return
			}
			requestBuilder.WriteString(line)
			if line == "\n" {
				break
			}
		}
		requestText := requestBuilder.String()
		if !strings.Contains(requestText, "set=1\n") {
			done <- errors.New("missing set header in request")
			return
		}
		if !strings.Contains(requestText, "replace_peers=true\n") {
			done <- errors.New("missing replace_peers in request")
			return
		}

		if _, writeErr := conn.Write([]byte("errno=0\n")); writeErr != nil {
			done <- writeErr
			return
		}
		done <- nil
	}()

	client := NewUAPIClient(WithSocketDir(socketDir))
	if err := client.ConfigureDevice(deviceName, Config{ReplacePeers: true}); err != nil {
		t.Fatalf("ConfigureDevice returned error: %v", err)
	}
	if serverErr := <-done; serverErr != nil {
		t.Fatalf("uapi server returned error: %v", serverErr)
	}

	getDone := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			getDone <- acceptErr
			return
		}
		defer func() { _ = conn.Close() }()

		buffer := make([]byte, 128)
		n, readErr := conn.Read(buffer)
		if readErr != nil {
			getDone <- readErr
			return
		}
		if string(buffer[:n]) != "get=1\n\n" {
			getDone <- errors.New("unexpected get request header")
			return
		}

		privateKey := mustKeyForUAPI(t, filledBytesUAPI(0x90))
		// WireGuard UAPI never emits a device-side public_key= line.
		// The response contains private_key, listen_port, fwmark, AWG params,
		// and then peer entries (each starting with public_key=). Emitting a
		// device-level public_key= here would violate the xplatform spec and
		// teach readers incorrect behaviour — it was removed in T009.
		payload := strings.Join([]string{
			"private_key=" + hexKey(privateKey),
			"listen_port=51820",
			"errno=0",
			"",
		}, "\n")

		if _, writeErr := conn.Write([]byte(payload)); writeErr != nil {
			getDone <- writeErr
			return
		}
		getDone <- nil
	}()

	device, err := client.Device(deviceName)
	if err != nil {
		t.Fatalf("Device returned error: %v", err)
	}
	if device.Name != deviceName {
		t.Fatalf("unexpected device name: got %q want %q", device.Name, deviceName)
	}
	if device.ListenPort != 51820 {
		t.Fatalf("unexpected device listen port: %d", device.ListenPort)
	}
	if serverErr := <-getDone; serverErr != nil {
		t.Fatalf("uapi get server returned error: %v", serverErr)
	}
}

func TestWriteKVError(t *testing.T) {
	t.Parallel()

	err := writeKV(failingWriter{}, "key", "value")
	if err == nil {
		t.Fatalf("expected writeKV error, got nil")
	}
}

func nonEmptyLines(raw string) []string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return []string{}
	}
	return strings.Split(trimmed, "\n")
}

func filledBytesUAPI(value byte) []byte {
	result := make([]byte, 32)
	for index := range result {
		result[index] = value
	}
	return result
}

func mustKeyForUAPI(t *testing.T, input []byte) Key {
	t.Helper()

	key, err := NewKey(input)
	if err != nil {
		t.Fatalf("NewKey returned error: %v", err)
	}
	return key
}

func mustUDPForUAPI(t *testing.T, addr string) *net.UDPAddr {
	t.Helper()

	endpoint, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatalf("ResolveUDPAddr returned error: %v", err)
	}
	return endpoint
}

func mustCIDRForUAPI(t *testing.T, cidr string) net.IPNet {
	t.Helper()

	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("ParseCIDR returned error: %v", err)
	}
	return *network
}

type failingWriter struct{}

func (failingWriter) Write(_ []byte) (int, error) {
	return 0, errors.New("write failed")
}
