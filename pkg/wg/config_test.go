package wg

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGenerateConfigIncludesInterfaceAndPeers(t *testing.T) {
	t.Parallel()

	privateKey := mustNewKeyForConfig(t, repeatedBytes(0x10, 32))
	presharedKey := mustNewKeyForConfig(t, repeatedBytes(0x20, 32))
	listenPort := 51820
	jc := 4
	s1 := 21
	h1 := "12345"
	i1 := "<b deadbeef>"
	keepalive := 30 * time.Second

	cfg := Config{
		PrivateKey: &privateKey,
		ListenPort: &listenPort,
		Jc:         &jc,
		S1:         &s1,
		H1:         &h1,
		I1:         &i1,
	}

	peers := []PeerConfig{
		{
			PublicKey:                   mustNewKeyForConfig(t, repeatedBytes(0x30, 32)),
			PresharedKey:                &presharedKey,
			Endpoint:                    mustResolveUDPAddr(t, "127.0.0.1:51820"),
			PersistentKeepaliveInterval: &keepalive,
			AllowedIPs: []net.IPNet{
				mustParseCIDR(t, "10.0.0.2/32"),
				mustParseCIDR(t, "10.0.1.0/24"),
			},
		},
	}

	output := GenerateConfig(cfg, peers)

	checks := []string{
		"[Interface]",
		"PrivateKey = " + privateKey.String(),
		"ListenPort = 51820",
		"Jc = 4",
		"S1 = 21",
		"H1 = 12345",
		"I1 = <b deadbeef>",
		"[Peer]",
		"PublicKey = " + peers[0].PublicKey.String(),
		"PresharedKey = " + presharedKey.String(),
		"Endpoint = 127.0.0.1:51820",
		"PersistentKeepalive = 30",
		"AllowedIPs = 10.0.0.2/32, 10.0.1.0/24",
	}

	for _, line := range checks {
		if !strings.Contains(output, line) {
			t.Fatalf("expected config to contain %q, got:\n%s", line, output)
		}
	}
}

func TestParseConfigFileRoundTrip(t *testing.T) {
	t.Parallel()

	privateKey := mustNewKeyForConfig(t, repeatedBytes(0x41, 32))
	publicKey := mustNewKeyForConfig(t, repeatedBytes(0x42, 32))
	preshared := mustNewKeyForConfig(t, repeatedBytes(0x43, 32))
	listenPort := 51821
	jmin := 64
	jmax := 90
	h2 := "67890"
	i3 := "<t TLS_EXTENSIONS>"
	keepalive := 15 * time.Second

	cfg := Config{
		PrivateKey: &privateKey,
		ListenPort: &listenPort,
		Jmin:       &jmin,
		Jmax:       &jmax,
		H2:         &h2,
		I3:         &i3,
	}
	peers := []PeerConfig{
		{
			PublicKey:                   publicKey,
			PresharedKey:                &preshared,
			Endpoint:                    mustResolveUDPAddr(t, "10.1.1.1:6000"),
			PersistentKeepaliveInterval: &keepalive,
			AllowedIPs: []net.IPNet{
				mustParseCIDR(t, "10.2.0.0/16"),
			},
		},
	}

	content := GenerateConfig(cfg, peers)
	configPath := filepath.Join(t.TempDir(), "wg.conf")
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	parsedCfg, parsedPeers, err := ParseConfigFile(configPath)
	if err != nil {
		t.Fatalf("ParseConfigFile returned error: %v", err)
	}

	if parsedCfg.PrivateKey == nil || *parsedCfg.PrivateKey != privateKey {
		t.Fatalf("unexpected private key: %#v", parsedCfg.PrivateKey)
	}
	if parsedCfg.ListenPort == nil || *parsedCfg.ListenPort != listenPort {
		t.Fatalf("unexpected listen port: %#v", parsedCfg.ListenPort)
	}
	if parsedCfg.Jmin == nil || *parsedCfg.Jmin != jmin {
		t.Fatalf("unexpected jmin: %#v", parsedCfg.Jmin)
	}
	if parsedCfg.Jmax == nil || *parsedCfg.Jmax != jmax {
		t.Fatalf("unexpected jmax: %#v", parsedCfg.Jmax)
	}
	if parsedCfg.H2 == nil || *parsedCfg.H2 != h2 {
		t.Fatalf("unexpected h2: %#v", parsedCfg.H2)
	}
	if parsedCfg.I3 == nil || *parsedCfg.I3 != i3 {
		t.Fatalf("unexpected i3: %#v", parsedCfg.I3)
	}

	if len(parsedPeers) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(parsedPeers))
	}
	peer := parsedPeers[0]
	if peer.PublicKey != publicKey {
		t.Fatalf("unexpected peer public key")
	}
	if peer.PresharedKey == nil || *peer.PresharedKey != preshared {
		t.Fatalf("unexpected peer preshared key")
	}
	if peer.Endpoint == nil || peer.Endpoint.String() != "10.1.1.1:6000" {
		t.Fatalf("unexpected peer endpoint: %#v", peer.Endpoint)
	}
	if peer.PersistentKeepaliveInterval == nil || *peer.PersistentKeepaliveInterval != keepalive {
		t.Fatalf("unexpected keepalive: %#v", peer.PersistentKeepaliveInterval)
	}
	if len(peer.AllowedIPs) != 1 || peer.AllowedIPs[0].String() != "10.2.0.0/16" {
		t.Fatalf("unexpected allowed IPs: %#v", peer.AllowedIPs)
	}

	if len(parsedCfg.Peers) != 1 {
		t.Fatalf("expected cfg.Peers to be populated, got %d", len(parsedCfg.Peers))
	}
}

func TestParseConfigFileErrors(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	privateKey := mustNewKeyForConfig(t, repeatedBytes(0x55, 32)).String()

	tests := []struct {
		name        string
		path        string
		content     string
		expectError string
	}{
		{
			name:        "empty path",
			path:        "",
			expectError: "config path is required",
		},
		{
			name:        "missing file",
			path:        filepath.Join(tempDir, "missing.conf"),
			expectError: "open config file",
		},
		{
			name: "key outside section",
			path: filepath.Join(tempDir, "outside.conf"),
			content: strings.Join([]string{
				"PrivateKey = " + privateKey,
			}, "\n"),
			expectError: "outside known section",
		},
		{
			name: "invalid config line",
			path: filepath.Join(tempDir, "bad-line.conf"),
			content: strings.Join([]string{
				"[Interface]",
				"ListenPort 51820",
			}, "\n"),
			expectError: "invalid config line",
		},
		{
			name: "peer key outside peer section",
			path: filepath.Join(tempDir, "peer-outside.conf"),
			content: strings.Join([]string{
				"[Interface]",
				"PrivateKey = " + privateKey,
				"[Unknown]",
				"PublicKey = " + privateKey,
			}, "\n"),
			expectError: "outside known section",
		},
		{
			name: "invalid peer allowed ip",
			path: filepath.Join(tempDir, "bad-ip.conf"),
			content: strings.Join([]string{
				"[Interface]",
				"PrivateKey = " + privateKey,
				"[Peer]",
				"PublicKey = " + privateKey,
				"AllowedIPs = bad-cidr",
			}, "\n"),
			expectError: "parse Peer AllowedIPs",
		},
		{
			name: "invalid interface integer",
			path: filepath.Join(tempDir, "bad-int.conf"),
			content: strings.Join([]string{
				"[Interface]",
				"ListenPort = nope",
			}, "\n"),
			expectError: "parse Interface ListenPort",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.content != "" {
				if err := os.WriteFile(tt.path, []byte(tt.content), 0o600); err != nil {
					t.Fatalf("WriteFile returned error: %v", err)
				}
			}

			_, _, err := ParseConfigFile(tt.path)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.expectError) {
				t.Fatalf("expected error containing %q, got %v", tt.expectError, err)
			}
		})
	}
}

func TestParseConfigFileHandlesCommentsAndCaseInsensitiveSections(t *testing.T) {
	t.Parallel()

	privateKey := mustNewKeyForConfig(t, repeatedBytes(0x66, 32)).String()
	publicKey := mustNewKeyForConfig(t, repeatedBytes(0x67, 32)).String()

	content := strings.Join([]string{
		"; comment",
		"# comment",
		"[INTERFACE]",
		"PrivateKey = " + privateKey,
		"ListenPort = 51820",
		"",
		"[PEER]",
		"PublicKey = " + publicKey,
		"AllowedIPs = 10.20.0.0/16, 10.21.0.0/16",
	}, "\n")

	path := filepath.Join(t.TempDir(), "comments.conf")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg, peers, err := ParseConfigFile(path)
	if err != nil {
		t.Fatalf("ParseConfigFile returned error: %v", err)
	}

	if cfg.ListenPort == nil || *cfg.ListenPort != 51820 {
		t.Fatalf("unexpected listen port: %#v", cfg.ListenPort)
	}
	if len(peers) != 1 {
		t.Fatalf("expected one peer, got %d", len(peers))
	}
	if len(peers[0].AllowedIPs) != 2 {
		t.Fatalf("expected two allowed IPs, got %d", len(peers[0].AllowedIPs))
	}
}

func TestParseConfigFileAllInterfaceFields(t *testing.T) {
	t.Parallel()

	privateKey := mustNewKeyForConfig(t, repeatedBytes(0x68, 32)).String()
	content := strings.Join([]string{
		"[Interface]",
		"PrivateKey = " + privateKey,
		"ListenPort = 51830",
		"FwMark = 33",
		"Jc = 1",
		"Jmin = 2",
		"Jmax = 3",
		"S1 = 4",
		"S2 = 5",
		"S3 = 6",
		"S4 = 7",
		"H1 = 11",
		"H2 = 12",
		"H3 = 13",
		"H4 = 14",
		"I1 = i1",
		"I2 = i2",
		"I3 = i3",
		"I4 = i4",
		"I5 = i5",
	}, "\n")

	path := filepath.Join(t.TempDir(), "all-fields.conf")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg, _, err := ParseConfigFile(path)
	if err != nil {
		t.Fatalf("ParseConfigFile returned error: %v", err)
	}

	if cfg.FirewallMark == nil || *cfg.FirewallMark != 33 {
		t.Fatalf("unexpected firewall mark: %#v", cfg.FirewallMark)
	}
	if cfg.Jc == nil || *cfg.Jc != 1 {
		t.Fatalf("unexpected Jc: %#v", cfg.Jc)
	}
	if cfg.Jmin == nil || *cfg.Jmin != 2 {
		t.Fatalf("unexpected Jmin: %#v", cfg.Jmin)
	}
	if cfg.Jmax == nil || *cfg.Jmax != 3 {
		t.Fatalf("unexpected Jmax: %#v", cfg.Jmax)
	}
	if cfg.S1 == nil || *cfg.S1 != 4 {
		t.Fatalf("unexpected S1: %#v", cfg.S1)
	}
	if cfg.S2 == nil || *cfg.S2 != 5 {
		t.Fatalf("unexpected S2: %#v", cfg.S2)
	}
	if cfg.S3 == nil || *cfg.S3 != 6 {
		t.Fatalf("unexpected S3: %#v", cfg.S3)
	}
	if cfg.S4 == nil || *cfg.S4 != 7 {
		t.Fatalf("unexpected S4: %#v", cfg.S4)
	}
	if cfg.H1 == nil || *cfg.H1 != "11" {
		t.Fatalf("unexpected H1: %#v", cfg.H1)
	}
	if cfg.H2 == nil || *cfg.H2 != "12" {
		t.Fatalf("unexpected H2: %#v", cfg.H2)
	}
	if cfg.H3 == nil || *cfg.H3 != "13" {
		t.Fatalf("unexpected H3: %#v", cfg.H3)
	}
	if cfg.H4 == nil || *cfg.H4 != "14" {
		t.Fatalf("unexpected H4: %#v", cfg.H4)
	}
	if cfg.I1 == nil || *cfg.I1 != "i1" {
		t.Fatalf("unexpected I1: %#v", cfg.I1)
	}
	if cfg.I2 == nil || *cfg.I2 != "i2" {
		t.Fatalf("unexpected I2: %#v", cfg.I2)
	}
	if cfg.I3 == nil || *cfg.I3 != "i3" {
		t.Fatalf("unexpected I3: %#v", cfg.I3)
	}
	if cfg.I4 == nil || *cfg.I4 != "i4" {
		t.Fatalf("unexpected I4: %#v", cfg.I4)
	}
	if cfg.I5 == nil || *cfg.I5 != "i5" {
		t.Fatalf("unexpected I5: %#v", cfg.I5)
	}
}

func TestPointerHelpers(t *testing.T) {
	t.Parallel()

	intValue := IntPtr(7)
	if intValue == nil || *intValue != 7 {
		t.Fatalf("IntPtr returned unexpected value: %#v", intValue)
	}

	stringValue := StrPtr("hello")
	if stringValue == nil || *stringValue != "hello" {
		t.Fatalf("StrPtr returned unexpected value: %#v", stringValue)
	}
}

func mustResolveUDPAddr(t *testing.T, address string) *net.UDPAddr {
	t.Helper()

	udpAddr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		t.Fatalf("ResolveUDPAddr returned error: %v", err)
	}
	return udpAddr
}

func mustParseCIDR(t *testing.T, cidr string) net.IPNet {
	t.Helper()

	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("ParseCIDR returned error: %v", err)
	}
	return *network
}

func repeatedBytes(value byte, count int) []byte {
	result := make([]byte, count)
	for i := range result {
		result[i] = value
	}
	return result
}

func mustNewKeyForConfig(t *testing.T, input []byte) Key {
	t.Helper()

	key, err := NewKey(input)
	if err != nil {
		t.Fatalf("NewKey returned error: %v", err)
	}
	return key
}
