package clientd

import (
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	pb "github.com/coonfuuseed-paandaa/awg-mesh/proto/control_plane"
)

// ApplyCertUpdate validates and persists replacement node credentials.
func ApplyCertUpdate(certPath, keyPath string, update *pb.CertUpdate) error {
	if update == nil {
		return errors.New("cert update is required")
	}
	if len(update.GetCertPem()) == 0 {
		return errors.New("cert update missing cert_pem")
	}
	if len(update.GetKeyPem()) == 0 {
		return errors.New("cert update missing key_pem")
	}
	if update.GetValidUntilUnix() != 0 && !time.Unix(update.GetValidUntilUnix(), 0).After(time.Now().UTC()) {
		return errors.New("cert update is already expired")
	}
	if _, err := tls.X509KeyPair(update.GetCertPem(), update.GetKeyPem()); err != nil {
		return fmt.Errorf("cert update pair validation failed: %w", err)
	}
	if err := writeCertKeyPairAtomic(certPath, keyPath, update.GetCertPem(), update.GetKeyPem()); err != nil {
		return err
	}
	return nil
}

func writeCertKeyPairAtomic(certPath, keyPath string, certPEM, keyPEM []byte) error {
	oldKey, oldKeyErr := os.ReadFile(keyPath)
	if err := writeFileAtomic(keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write cert update key: %w", err)
	}
	if err := writeFileAtomic(certPath, certPEM, 0o644); err != nil {
		if oldKeyErr == nil {
			_ = writeFileAtomic(keyPath, oldKey, 0o600)
		} else if errors.Is(oldKeyErr, os.ErrNotExist) {
			_ = os.Remove(keyPath)
		}
		return fmt.Errorf("write cert update cert: %w", err)
	}
	return nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if path == "" {
		return errors.New("path is required")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer func() { _ = os.Remove(tempPath) }()

	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}
