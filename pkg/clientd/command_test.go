package clientd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/role"
	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/tls"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/wg"
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
	validated, err := ValidateCommandConfig(cfg)
	if err != nil {
		t.Fatalf("prepared layout config rejected: %v", err)
	}
	if validated.CACertPath != filepath.Join(configDir, "ca.crt") {
		t.Fatalf("default CA path = %q, want %q", validated.CACertPath, filepath.Join(configDir, "ca.crt"))
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
	if _, err := ValidateCommandConfig(cfg); err == nil || !strings.Contains(err.Error(), "--allow-insecure-control-plane") {
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
