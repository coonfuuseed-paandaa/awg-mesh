package tls

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateCA(t *testing.T) {
	t.Parallel()

	cert, key, err := GenerateCA("awg-mesh-ca")
	if err != nil {
		t.Fatalf("GenerateCA returned error: %v", err)
	}
	if cert.Subject.CommonName != "awg-mesh-ca" {
		t.Fatalf("unexpected common name: %s", cert.Subject.CommonName)
	}
	if !cert.IsCA {
		t.Fatalf("expected IsCA=true")
	}
	if cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Fatalf("expected cert-sign key usage")
	}
	if _, ok := key.(*ecdsa.PrivateKey); !ok {
		t.Fatalf("expected ECDSA private key, got %T", key)
	}
}

func TestIssueCert(t *testing.T) {
	t.Parallel()

	caCert, caKey, err := GenerateCA("mesh-ca")
	if err != nil {
		t.Fatalf("GenerateCA returned error: %v", err)
	}

	certPEM, keyPEM, err := IssueCert(caCert, caKey, "node-1", []string{"node-1.local", "10.0.0.10"})
	if err != nil {
		t.Fatalf("IssueCert returned error: %v", err)
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		t.Fatalf("expected non-empty cert and key PEM")
	}

	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		t.Fatalf("failed to decode issued certificate PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate returned error: %v", err)
	}
	if cert.Subject.CommonName != "node-1" {
		t.Fatalf("unexpected cert CN: %s", cert.Subject.CommonName)
	}
	if !containsString(cert.DNSNames, "node-1.local") {
		t.Fatalf("missing DNS SAN in certificate: %#v", cert.DNSNames)
	}
	if !containsIP(cert.IPAddresses, net.ParseIP("10.0.0.10")) {
		t.Fatalf("missing IP SAN in certificate: %#v", cert.IPAddresses)
	}

	if err := ValidateCert(certPEM, caCert); err != nil {
		t.Fatalf("ValidateCert returned error: %v", err)
	}
}

func TestSaveLoadCA(t *testing.T) {
	t.Parallel()

	caCert, caKey, err := GenerateCA("mesh-ca")
	if err != nil {
		t.Fatalf("GenerateCA returned error: %v", err)
	}

	dir := t.TempDir()
	if err := SaveCA(dir, caCert, caKey); err != nil {
		t.Fatalf("SaveCA returned error: %v", err)
	}

	loadedCert, loadedKey, err := LoadCA(dir)
	if err != nil {
		t.Fatalf("LoadCA returned error: %v", err)
	}
	if loadedCert.Subject.CommonName != caCert.Subject.CommonName {
		t.Fatalf("loaded certificate CN mismatch: got %s want %s", loadedCert.Subject.CommonName, caCert.Subject.CommonName)
	}
	if _, ok := loadedKey.(*ecdsa.PrivateKey); !ok {
		t.Fatalf("expected loaded ECDSA key, got %T", loadedKey)
	}
}

func TestSaveCAErrors(t *testing.T) {
	t.Parallel()

	caCert, caKey, err := GenerateCA("mesh-ca")
	if err != nil {
		t.Fatalf("GenerateCA returned error: %v", err)
	}

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey returned error: %v", err)
	}

	err = SaveCA(t.TempDir(), caCert, rsaKey)
	if err == nil {
		t.Fatalf("expected unsupported key type error, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported key type") {
		t.Fatalf("unexpected error: %v", err)
	}

	filePath := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	err = SaveCA(filePath, caCert, caKey)
	if err == nil || !strings.Contains(err.Error(), "create CA dir") {
		t.Fatalf("expected create-dir error, got %v", err)
	}
}

func TestLoadCAErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	_, _, err := LoadCA(dir)
	if err == nil {
		t.Fatalf("expected missing CA file error, got nil")
	}
	if !strings.Contains(err.Error(), "read ca.crt") {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("not-pem"), 0o600); err != nil {
		t.Fatalf("WriteFile ca.crt returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.key"), []byte("not-pem"), 0o600); err != nil {
		t.Fatalf("WriteFile ca.key returned error: %v", err)
	}

	_, _, err = LoadCA(dir)
	if err == nil {
		t.Fatalf("expected invalid PEM error, got nil")
	}
	if !strings.Contains(err.Error(), "contains no PEM block") {
		t.Fatalf("unexpected error: %v", err)
	}

	validCert, validKey, err := GenerateCA("mesh-ca")
	if err != nil {
		t.Fatalf("GenerateCA returned error: %v", err)
	}
	if err := SaveCA(dir, validCert, validKey); err != nil {
		t.Fatalf("SaveCA returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.key"), []byte("-----BEGIN EC PRIVATE KEY-----\nabc\n-----END EC PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatalf("WriteFile ca.key returned error: %v", err)
	}
	_, _, err = LoadCA(dir)
	if err == nil || (!strings.Contains(err.Error(), "parse ca.key") && !strings.Contains(err.Error(), "contains no PEM block")) {
		t.Fatalf("expected parse ca.key error, got %v", err)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containsIP(values []net.IP, target net.IP) bool {
	for _, value := range values {
		if value.Equal(target) {
			return true
		}
	}
	return false
}
