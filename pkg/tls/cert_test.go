package tls

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveLoadCertKey(t *testing.T) {
	t.Parallel()

	caCert, caKey, err := GenerateCA("mesh-ca")
	if err != nil {
		t.Fatalf("GenerateCA returned error: %v", err)
	}
	certPEM, keyPEM, err := IssueCert(caCert, caKey, "node-a", []string{"node-a.local"})
	if err != nil {
		t.Fatalf("IssueCert returned error: %v", err)
	}

	dir := t.TempDir()
	if err := SaveCertKey(dir, certPEM, keyPEM); err != nil {
		t.Fatalf("SaveCertKey returned error: %v", err)
	}

	loaded, err := LoadCertKey(dir)
	if err != nil {
		t.Fatalf("LoadCertKey returned error: %v", err)
	}
	if len(loaded.Certificate) == 0 {
		t.Fatalf("expected loaded TLS certificate chain")
	}
}

func TestSaveCertKeyError(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	err := SaveCertKey(path, []byte("cert"), []byte("key"))
	if err == nil {
		t.Fatalf("expected SaveCertKey error, got nil")
	}
	if !strings.Contains(err.Error(), "create cert dir") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadCACert(t *testing.T) {
	t.Parallel()

	caCert, caKey, err := GenerateCA("mesh-ca")
	if err != nil {
		t.Fatalf("GenerateCA returned error: %v", err)
	}

	dir := t.TempDir()
	if err := SaveCA(dir, caCert, caKey); err != nil {
		t.Fatalf("SaveCA returned error: %v", err)
	}

	pool, err := LoadCACert(filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatalf("LoadCACert returned error: %v", err)
	}
	if pool == nil {
		t.Fatalf("expected non-nil cert pool")
	}

	_, err = LoadCACert(filepath.Join(dir, "missing.crt"))
	if err == nil || !strings.Contains(err.Error(), "read CA cert") {
		t.Fatalf("expected read error for missing CA cert, got %v", err)
	}

	invalidPEMPath := filepath.Join(dir, "invalid.crt")
	if err := os.WriteFile(invalidPEMPath, []byte("not-pem"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	_, err = LoadCACert(invalidPEMPath)
	if err == nil || !strings.Contains(err.Error(), "no valid PEM certificate") {
		t.Fatalf("expected invalid PEM error, got %v", err)
	}
}

func TestValidateCert(t *testing.T) {
	t.Parallel()

	caCert, caKey, err := GenerateCA("mesh-ca")
	if err != nil {
		t.Fatalf("GenerateCA returned error: %v", err)
	}
	certPEM, _, err := IssueCert(caCert, caKey, "node-a", []string{"node-a.local"})
	if err != nil {
		t.Fatalf("IssueCert returned error: %v", err)
	}

	if err := ValidateCert(certPEM, caCert); err != nil {
		t.Fatalf("ValidateCert returned error: %v", err)
	}

	wrongCACert, _, err := GenerateCA("other-ca")
	if err != nil {
		t.Fatalf("GenerateCA returned error: %v", err)
	}
	if err := ValidateCert(certPEM, wrongCACert); err == nil {
		t.Fatalf("expected validation failure with wrong CA")
	}

	if err := ValidateCert([]byte("not pem"), caCert); err == nil {
		t.Fatalf("expected validation failure for invalid PEM")
	}
}

func TestCertInfo(t *testing.T) {
	t.Parallel()

	caCert, caKey, err := GenerateCA("mesh-ca")
	if err != nil {
		t.Fatalf("GenerateCA returned error: %v", err)
	}
	certPEM, _, err := IssueCert(caCert, caKey, "node-b", []string{"node-b.local"})
	if err != nil {
		t.Fatalf("IssueCert returned error: %v", err)
	}

	cn, notAfter, err := CertInfo(certPEM)
	if err != nil {
		t.Fatalf("CertInfo returned error: %v", err)
	}
	if cn != "node-b" {
		t.Fatalf("unexpected common name: %s", cn)
	}
	if notAfter.Before(time.Now().UTC()) {
		t.Fatalf("certificate already expired: %s", notAfter)
	}

	_, _, err = CertInfo([]byte("not pem"))
	if err == nil {
		t.Fatalf("expected CertInfo error for invalid PEM")
	}
}

func TestLoadCertKeyError(t *testing.T) {
	t.Parallel()

	_, err := LoadCertKey(t.TempDir())
	if err == nil {
		t.Fatalf("expected LoadCertKey error, got nil")
	}
	if !strings.Contains(err.Error(), "load node cert/key") {
		t.Fatalf("unexpected error: %v", err)
	}
}
