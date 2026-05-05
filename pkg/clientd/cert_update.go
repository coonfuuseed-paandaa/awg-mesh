package clientd

import (
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	pb "github.com/coonfuuseed-paandaa/awg-mesh/v2/proto/control_plane"
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
	if certPath == "" || keyPath == "" {
		return errors.New("cert and key paths are required")
	}
	if filepath.Clean(certPath) == filepath.Clean(keyPath) {
		return errors.New("cert and key paths must be different")
	}

	keyTemp, err := writeTempFile(keyPath, keyPEM, 0o600)
	if err != nil {
		return fmt.Errorf("write cert update key temp: %w", err)
	}
	defer removeIfSet(&keyTemp)

	certTemp, err := writeTempFile(certPath, certPEM, 0o644)
	if err != nil {
		return fmt.Errorf("write cert update cert temp: %w", err)
	}
	defer removeIfSet(&certTemp)

	keyBackup, hadKey, err := backupExistingFile(keyPath)
	if err != nil {
		return fmt.Errorf("backup existing cert update key: %w", err)
	}
	certBackup, hadCert, err := backupExistingFile(certPath)
	if err != nil {
		if restoreErr := restoreCertKeyPair(certPath, keyPath, certBackup, keyBackup, hadCert, hadKey, false, false); restoreErr != nil {
			return fmt.Errorf("backup existing cert update cert: %w; rollback failed: %v", err, restoreErr)
		}
		return fmt.Errorf("backup existing cert update cert: %w", err)
	}

	if err := os.Rename(keyTemp, keyPath); err != nil {
		restoreErr := restoreCertKeyPair(certPath, keyPath, certBackup, keyBackup, hadCert, hadKey, false, false)
		if restoreErr != nil {
			return fmt.Errorf("replace cert update key: %w; rollback failed: %v", err, restoreErr)
		}
		return fmt.Errorf("replace cert update key: %w", err)
	}
	keyTemp = ""
	if err := os.Rename(certTemp, certPath); err != nil {
		restoreErr := restoreCertKeyPair(certPath, keyPath, certBackup, keyBackup, hadCert, hadKey, false, true)
		if restoreErr != nil {
			return fmt.Errorf("replace cert update cert: %w; rollback failed: %v", err, restoreErr)
		}
		return fmt.Errorf("replace cert update cert: %w", err)
	}
	certTemp = ""

	removeBackup(certBackup, hadCert)
	removeBackup(keyBackup, hadKey)
	return nil
}

func writeTempFile(path string, data []byte, mode os.FileMode) (string, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", err
	}
	tempPath := file.Name()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(tempPath)
		return "", err
	}
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		_ = os.Remove(tempPath)
		return "", err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tempPath)
		return "", err
	}
	return tempPath, nil
}

func backupExistingFile(path string) (string, bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if info.IsDir() {
		return "", false, fmt.Errorf("%s is a directory", path)
	}
	backup := path + ".bak"
	_ = os.Remove(backup)
	if err := os.Rename(path, backup); err != nil {
		return "", false, err
	}
	return backup, true, nil
}

func restoreCertKeyPair(certPath, keyPath, certBackup, keyBackup string, hadCert, hadKey, certInstalled, keyInstalled bool) error {
	var restoreErr error
	if err := restoreFile(certPath, certBackup, hadCert, certInstalled); err != nil {
		restoreErr = errors.Join(restoreErr, fmt.Errorf("restore cert: %w", err))
	}
	if err := restoreFile(keyPath, keyBackup, hadKey, keyInstalled); err != nil {
		restoreErr = errors.Join(restoreErr, fmt.Errorf("restore key: %w", err))
	}
	return restoreErr
}

func restoreFile(path, backup string, hadBackup, installed bool) error {
	if installed {
		_ = os.Remove(path)
	}
	if !hadBackup {
		return nil
	}
	return os.Rename(backup, path)
}

func removeBackup(path string, hadBackup bool) {
	if hadBackup {
		_ = os.Remove(path)
	}
}

func removeIfSet(path *string) {
	if *path != "" {
		_ = os.Remove(*path)
	}
}
