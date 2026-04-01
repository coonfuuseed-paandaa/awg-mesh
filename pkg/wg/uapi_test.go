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

func TestParseDeviceSuccess(t *testing.T) {
	t.Parallel()

	privateKey := mustKeyForUAPI(t, filledBytesUAPI(0x10))
	devicePublicKey := mustKeyForUAPI(t, filledBytesUAPI(0x11))
	peerPublicKey := mustKeyForUAPI(t, filledBytesUAPI(0x12))
	peerPresharedKey := mustKeyForUAPI(t, filledBytesUAPI(0x13))

	response := strings.Join([]string{
		"private_key=" + hexKey(privateKey),
		"public_key=" + hexKey(devicePublicKey),
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
	if device.PublicKey != devicePublicKey {
		t.Fatalf("unexpected public key")
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
		tt := tt
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
		tt := tt
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

func TestConfigureDeviceAndDeviceRequireName(t *testing.T) {
	t.Parallel()

	client := NewUAPIClient()
	if err := client.ConfigureDevice("", Config{}); err == nil || !strings.Contains(err.Error(), "device name is required") {
		t.Fatalf("expected ConfigureDevice name validation error, got %v", err)
	}

	_, err := client.Device("")
	if err == nil || !strings.Contains(err.Error(), "device name is required") {
		t.Fatalf("expected Device name validation error, got %v", err)
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
		publicKey := mustKeyForUAPI(t, filledBytesUAPI(0x91))
		payload := strings.Join([]string{
			"private_key=" + hexKey(privateKey),
			"public_key=" + hexKey(publicKey),
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
