package tls

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SaveCertKey writes node.crt (0644) and node.key (0600) to dir.
func SaveCertKey(dir string, certPEM, keyPEM []byte) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create cert dir: %w", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "node.crt"), certPEM, 0644); err != nil {
		return fmt.Errorf("write node.crt: %w", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "node.key"), keyPEM, 0600); err != nil {
		return fmt.Errorf("write node.key: %w", err)
	}

	return nil
}

// LoadCertKey loads node.crt and node.key from dir and returns a tls.Certificate
// suitable for use in a tls.Config.
func LoadCertKey(dir string) (tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(
		filepath.Join(dir, "node.crt"),
		filepath.Join(dir, "node.key"),
	)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("load node cert/key: %w", err)
	}
	return cert, nil
}

// LoadCACert reads the PEM-encoded CA certificate at path and adds it to a
// new x509.CertPool for use as the RootCAs or ClientCAs in a tls.Config.
func LoadCACert(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read CA cert: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("load CA cert: no valid PEM certificate found in %s", path)
	}

	return pool, nil
}

// ValidateCert verifies that certPEM was signed by caCert.
func ValidateCert(certPEM []byte, caCert *x509.Certificate) error {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return fmt.Errorf("validate cert: no PEM block found")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("validate cert: parse: %w", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	opts := x509.VerifyOptions{
		Roots: pool,
		// Skip hostname/EKU checks — we only verify the signature chain.
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}

	if _, err := cert.Verify(opts); err != nil {
		return fmt.Errorf("validate cert: verification failed: %w", err)
	}

	return nil
}

// CertInfo extracts the CommonName and expiry time from a PEM-encoded certificate.
func CertInfo(certPEM []byte) (commonName string, notAfter time.Time, err error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", time.Time{}, fmt.Errorf("cert info: no PEM block found")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("cert info: parse: %w", err)
	}

	return cert.Subject.CommonName, cert.NotAfter, nil
}
