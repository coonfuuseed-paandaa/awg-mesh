package clientd

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/role"
	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/tls"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/wg"
	pb "github.com/coonfuuseed-paandaa/awg-mesh/v2/proto/control_plane"
)

func TestParseCommandConfigRequiredFlagsAndProtocol(t *testing.T) {
	_, err := ParseCommandConfig(nil, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "--control-plane") || !strings.Contains(err.Error(), "--cert") {
		t.Fatalf("expected missing required flags error, got %v", err)
	}

	args := validCommandArgs(t)
	args[len(args)-1] = "bad-protocol"
	_, err = ParseCommandConfig(args, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "invalid --protocol") {
		t.Fatalf("expected invalid protocol error, got %v", err)
	}

	args[len(args)-1] = string(wg.ProtocolVanilla)
	cfg, err := ParseCommandConfig(args, &strings.Builder{})
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if cfg.Protocol != wg.ProtocolVanilla || cfg.Name != "client-a" {
		t.Fatalf("unexpected parsed config: %#v", cfg)
	}
	if len(cfg.Roles) != 1 || cfg.Roles[0] != role.RoleClient {
		t.Fatalf("default roles = %#v, want client", cfg.Roles)
	}
	if cfg.KeyPath != filepath.Join(".", "node.key") {
		t.Fatalf("default key path = %q, want ./node.key", cfg.KeyPath)
	}
	if cfg.WireGuardPrivateKeyPath != "wireguard-private.key" {
		t.Fatalf("default wireguard private key path = %q, want wireguard-private.key", cfg.WireGuardPrivateKeyPath)
	}

	args = append(validCommandArgs(t), "--key", "custom.key")
	cfg, err = ParseCommandConfig(args, &strings.Builder{})
	if err != nil {
		t.Fatalf("config with explicit key path rejected: %v", err)
	}
	if cfg.KeyPath != "custom.key" {
		t.Fatalf("explicit key path = %q, want custom.key", cfg.KeyPath)
	}

	args = append(validCommandArgs(t), "--ca-cert", "mesh-ca.crt")
	cfg, err = ParseCommandConfig(args, &strings.Builder{})
	if err != nil {
		t.Fatalf("config with explicit CA path rejected: %v", err)
	}
	if cfg.CACertPath != "mesh-ca.crt" {
		t.Fatalf("explicit CA path = %q, want mesh-ca.crt", cfg.CACertPath)
	}

	args = append(validCommandArgs(t), "--wireguard-private-key", "custom-wg.key")
	cfg, err = ParseCommandConfig(args, &strings.Builder{})
	if err != nil {
		t.Fatalf("config with explicit WireGuard key path rejected: %v", err)
	}
	if cfg.WireGuardPrivateKeyPath != "custom-wg.key" {
		t.Fatalf("explicit WireGuard key path = %q, want custom-wg.key", cfg.WireGuardPrivateKeyPath)
	}
}

func TestValidateCommandConfigDefaultsCACertFromPreparedNodeLayout(t *testing.T) {
	configDir := t.TempDir()
	certPath := filepath.Join(configDir, "nodes", "client-a", "node.crt")
	if err := os.MkdirAll(filepath.Dir(certPath), 0o755); err != nil {
		t.Fatalf("mkdir node dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "ca.crt"), []byte("ca"), 0o644); err != nil {
		t.Fatalf("write ca.crt: %v", err)
	}
	cfg := validCommandConfig(t)
	cfg.CertPath = certPath
	cfg.KeyPath = ""
	cfg.CACertPath = ""
	cfg.WireGuardPrivateKeyPath = ""
	validated, err := ValidateCommandConfig(cfg)
	if err != nil {
		t.Fatalf("prepared layout config rejected: %v", err)
	}
	if validated.CACertPath != filepath.Join(configDir, "ca.crt") {
		t.Fatalf("default CA path = %q, want %q", validated.CACertPath, filepath.Join(configDir, "ca.crt"))
	}
	if validated.WireGuardPrivateKeyPath != filepath.Join(configDir, "nodes", "client-a", "wireguard-private.key") {
		t.Fatalf("default WireGuard key path = %q", validated.WireGuardPrivateKeyPath)
	}

	cwd := t.TempDir()
	t.Chdir(cwd)
	if err := os.WriteFile(filepath.Join(cwd, "ca.crt"), []byte("ca"), 0o644); err != nil {
		t.Fatalf("write cwd ca.crt: %v", err)
	}
	cfg = validCommandConfig(t)
	cfg.CertPath = "node.pem"
	cfg.KeyPath = ""
	cfg.CACertPath = ""
	validated, err = ValidateCommandConfig(cfg)
	if err != nil {
		t.Fatalf("arbitrary cert path config rejected: %v", err)
	}
	if validated.CACertPath != "" {
		t.Fatalf("arbitrary cert path auto-discovered CA %q, want empty", validated.CACertPath)
	}
}

func TestLoadWireGuardPrivateKeyAndOverlayAddress(t *testing.T) {
	privateKey, err := wg.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	keyPath := filepath.Join(t.TempDir(), "wireguard-private.key")
	if err := os.WriteFile(keyPath, []byte(privateKey.String()+"\n"), 0o600); err != nil {
		t.Fatalf("write private key: %v", err)
	}
	loaded, err := loadWireGuardPrivateKey(keyPath)
	if err != nil {
		t.Fatalf("load wireguard private key: %v", err)
	}
	if loaded != privateKey {
		t.Fatalf("loaded key mismatch")
	}
	if _, err := loadWireGuardPrivateKey(filepath.Join(t.TempDir(), "missing.key")); err == nil || !strings.Contains(err.Error(), "read wireguard private key") {
		t.Fatalf("expected missing key read error, got %v", err)
	}

	addr, err := parseOverlayAddress("172.21.92.130")
	if err != nil {
		t.Fatalf("parse IPv4 overlay address: %v", err)
	}
	if addr.String() != "172.21.92.130/32" {
		t.Fatalf("IPv4 overlay address = %s, want /32", addr)
	}
	addr, err = parseOverlayAddress("fd00::10/64")
	if err != nil {
		t.Fatalf("parse IPv6 CIDR overlay address: %v", err)
	}
	if addr.String() != "fd00::10/64" {
		t.Fatalf("IPv6 overlay address = %s, want fd00::10/64", addr)
	}
}

func TestValidateCommandConfigInsecureControlPlaneGate(t *testing.T) {
	for _, target := range []string{"localhost:51820", "127.0.0.1:51820", "127.42.0.1:51820", "[::1]:51820"} {
		t.Run("accept_loopback_"+target, func(t *testing.T) {
			cfg := validCommandConfig(t)
			cfg.ControlPlane = target
			if _, err := ValidateCommandConfig(cfg); err != nil {
				t.Fatalf("loopback target rejected: %v", err)
			}
		})
	}

	cfg := validCommandConfig(t)
	cfg.ControlPlane = "192.0.2.10:51820"
	if _, err := ValidateCommandConfig(cfg); err == nil || !strings.Contains(err.Error(), "--allow-insecure-control-plane") || !strings.Contains(err.Error(), "coordination target") {
		t.Fatalf("expected non-loopback rejection, got %v", err)
	}

	cfg.CACertPath = "mesh-ca.crt"
	if _, err := ValidateCommandConfig(cfg); err != nil {
		t.Fatalf("mTLS-protected non-loopback target should be allowed: %v", err)
	}
	cfg.CACertPath = ""

	cfg.AllowInsecureControlPlane = true
	if _, err := ValidateCommandConfig(cfg); err != nil {
		t.Fatalf("override should allow non-loopback target: %v", err)
	}

	cfg = validCommandConfig(t)
	cfg.ControlPlane = "master-01.example:9090"
	cfg.CACertPath = "mesh-ca.crt"
	validated, err := ValidateCommandConfig(cfg)
	if err != nil {
		t.Fatalf("responsible master coordination target rejected: %v", err)
	}
	if validated.ControlPlane != "master-01.example:9090" {
		t.Fatalf("coordination target not preserved: %#v", validated)
	}
}

func TestLoadClientCertificateFromFilesReadsCurrentFiles(t *testing.T) {
	caCert, caKey, err := pkgtls.GenerateCA("mesh-ca")
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	certA, keyA, err := pkgtls.IssueCert(caCert, caKey, "client-a", []string{"client-a"})
	if err != nil {
		t.Fatalf("IssueCert client-a: %v", err)
	}
	certB, keyB, err := pkgtls.IssueCert(caCert, caKey, "client-b", []string{"client-b"})
	if err != nil {
		t.Fatalf("IssueCert client-b: %v", err)
	}

	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	certPath := filepath.Join(dir, "node.crt")
	keyPath := filepath.Join(dir, "node.key")
	if err := os.WriteFile(caPath, pkgtls.EncodeCertPEM(caCert), 0o644); err != nil {
		t.Fatalf("write ca.crt: %v", err)
	}
	if err := os.WriteFile(certPath, certA, 0o644); err != nil {
		t.Fatalf("write initial cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyA, 0o600); err != nil {
		t.Fatalf("write initial key: %v", err)
	}

	loader := loadClientCertificateFromFiles(certPath, keyPath)
	first, err := loader(nil)
	if err != nil {
		t.Fatalf("load initial client cert: %v", err)
	}
	if got := clientCertCommonName(t, first); got != "client-a" {
		t.Fatalf("initial client CN = %q, want client-a", got)
	}
	if err := os.WriteFile(certPath, certB, 0o644); err != nil {
		t.Fatalf("write rotated cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyB, 0o600); err != nil {
		t.Fatalf("write rotated key: %v", err)
	}
	second, err := loader(nil)
	if err != nil {
		t.Fatalf("load rotated client cert: %v", err)
	}
	if got := clientCertCommonName(t, second); got != "client-b" {
		t.Fatalf("rotated client CN = %q, want client-b", got)
	}
}

func TestValidateCommandConfigRejectsInvalidInterfaceName(t *testing.T) {
	cfg := validCommandConfig(t)
	cfg.InterfaceName = "../bad"
	if _, err := ValidateCommandConfig(cfg); err == nil || !strings.Contains(err.Error(), "invalid --iface") {
		t.Fatalf("expected invalid interface name rejection, got %v", err)
	}
}

func TestValidateCommandConfigAllowsEgressRoleOverride(t *testing.T) {
	cfg := validCommandConfig(t)
	cfg.Roles = []role.Role{role.RoleEgress}
	validated, err := ValidateCommandConfig(cfg)
	if err != nil {
		t.Fatalf("egress role override rejected: %v", err)
	}
	if len(validated.Roles) != 1 || validated.Roles[0] != role.RoleEgress {
		t.Fatalf("validated roles = %#v, want egress", validated.Roles)
	}

	cfg.Roles = []role.Role{role.RoleClient, role.RoleEgress}
	if _, err := ValidateCommandConfig(cfg); err == nil || !strings.Contains(err.Error(), "client") {
		t.Fatalf("expected invalid role combination, got %v", err)
	}
}

func TestCertUpdatePathsRequireMTLS(t *testing.T) {
	cfg := validCommandConfig(t)
	cfg.CertPath = "/config/node.crt"
	cfg.KeyPath = "/config/node.key"
	cfg.CACertPath = ""
	certPath, keyPath := certUpdatePaths(cfg)
	if certPath != "" || keyPath != "" {
		t.Fatalf("insecure control-plane enabled cert updates: cert=%q key=%q", certPath, keyPath)
	}

	cfg.CACertPath = "/config/ca.crt"
	certPath, keyPath = certUpdatePaths(cfg)
	if certPath != cfg.CertPath || keyPath != cfg.KeyPath {
		t.Fatalf("mTLS control-plane disabled cert updates: cert=%q key=%q", certPath, keyPath)
	}
}

func TestLazyTransportConfiguratorCreatesTransportOnFirstApply(t *testing.T) {
	transport := &fakeTransport{protocol: wg.ProtocolAmneziaWG, name: "awg-test0"}
	created := 0
	configurator := &lazyTransportConfigurator{
		protocol:   wg.ProtocolAmneziaWG,
		name:       "awg-test0",
		localRoles: []role.Role{role.RoleClient},
		newTransport: func(protocol wg.Protocol, name string) (wg.Transport, error) {
			created++
			if protocol != wg.ProtocolAmneziaWG || name != "awg-test0" {
				t.Fatalf("unexpected transport request: protocol=%s name=%s", protocol, name)
			}
			return transport, nil
		},
	}

	if created != 0 {
		t.Fatalf("transport created before Apply")
	}
	state := State{Peers: []PeerEntry{{
		PeerName:   "master-a",
		PeerRole:   role.RoleMaster,
		PeerPubkey: bytesOf(0x42),
		AllowedIPs: []string{"10.0.0.1/32"},
	}}}
	if err := configurator.Apply(context.Background(), state); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := configurator.Apply(context.Background(), state); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if created != 1 {
		t.Fatalf("transport factory called %d times, want 1", created)
	}
	if got := len(transport.configsSnapshot()); got != 2 {
		t.Fatalf("transport configure calls = %d, want 2", got)
	}
	if err := configurator.Close(); err != nil {
		t.Fatalf("close lazy configurator: %v", err)
	}
}

func TestRunWithConfigRegistersWireGuardPublicKeyFromPreparedKey(t *testing.T) {
	privateKey, err := wg.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	preparedDir := t.TempDir()
	certPath := filepath.Join(preparedDir, "node.crt")
	wgKeyPath := filepath.Join(preparedDir, "wireguard-private.key")
	if err := os.WriteFile(certPath, []byte("cert-bytes"), 0o600); err != nil {
		t.Fatalf("write cert fixture: %v", err)
	}
	if err := os.WriteFile(wgKeyPath, []byte(privateKey.String()+"\n"), 0o600); err != nil {
		t.Fatalf("write wireguard key fixture: %v", err)
	}

	server := &streamingTestServer{registered: make(chan *pb.RegisterNodeRequest, 1)}
	addr, cleanup := startTestControlPlane(t, server)
	defer cleanup()

	transport := &fakeTransport{protocol: wg.ProtocolAmneziaWG, name: "awg-test0"}
	link := &fakeLinkConfigurator{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runWithConfig(ctx, CommandConfig{
			ControlPlane:              addr,
			Name:                      "egress-01",
			OverlayIP:                 "172.21.92.35",
			Region:                    "us",
			CertPath:                  certPath,
			WireGuardPrivateKeyPath:   wgKeyPath,
			StateDir:                  t.TempDir(),
			InterfaceName:             "awg-test0",
			Protocol:                  wg.ProtocolAmneziaWG,
			Roles:                     []role.Role{role.RoleEgress},
			AllowInsecureControlPlane: true,
		}, &strings.Builder{}, commandRuntime{
			link: link,
			newTransport: func(protocol wg.Protocol, name string) (wg.Transport, error) {
				if protocol != wg.ProtocolAmneziaWG || name != "awg-test0" {
					return nil, fmt.Errorf("unexpected transport request: protocol=%s name=%s", protocol, name)
				}
				return transport, nil
			},
		})
	}()

	var req *pb.RegisterNodeRequest
	select {
	case req = <-server.registered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for production config registration")
	}
	publicKey := privateKey.PublicKey()
	if !bytes.Equal(req.GetPubkey(), publicKey[:]) {
		t.Fatalf("registered pubkey = %x, want %x", req.GetPubkey(), publicKey[:])
	}
	if got := req.GetRoles(); len(got) != 1 || got[0] != string(role.RoleEgress) {
		t.Fatalf("registered roles = %v, want [egress]", got)
	}
	if req.GetProtocol() != string(wg.ProtocolAmneziaWG) {
		t.Fatalf("registered protocol = %q", req.GetProtocol())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("runWithConfig returned after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for runWithConfig shutdown")
	}
}

func validCommandArgs(t *testing.T) []string {
	t.Helper()
	return []string{
		"--control-plane", "127.0.0.1:51820",
		"--name", "client-a",
		"--overlay-ip", "10.10.0.10",
		"--region", "eu-test",
		"--cert", "node.pem",
		"--state-dir", t.TempDir(),
		"--iface", "awg-test0",
		"--protocol", string(wg.ProtocolAmneziaWG),
	}
}

func validCommandConfig(t *testing.T) CommandConfig {
	t.Helper()
	cfg, err := ParseCommandConfig(validCommandArgs(t), &strings.Builder{})
	if err != nil {
		t.Fatalf("valid command args rejected: %v", err)
	}
	return cfg
}

func clientCertCommonName(t *testing.T, cert *tls.Certificate) string {
	t.Helper()
	if cert == nil || len(cert.Certificate) == 0 {
		t.Fatal("client certificate chain is empty")
	}
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse client cert: %v", err)
	}
	return parsed.Subject.CommonName
}
