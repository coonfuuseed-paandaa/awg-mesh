package cmd

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunBackupCommandCapturesLocalTopologyAndControlPlaneState(t *testing.T) {
	dir := t.TempDir()
	configDir, topologyPath, controlStateDir := writeBackupFixture(t, dir)
	archivePath := filepath.Join(dir, "mesh-backup.zip")

	var out bytes.Buffer
	if err := runBackupCommand(backupOptions{
		archivePath:          archivePath,
		topologyPath:         topologyPath,
		configDir:            configDir,
		controlPlaneStateDir: controlStateDir,
		stdout:               &out,
	}); err != nil {
		t.Fatalf("runBackupCommand: %v", err)
	}
	if !strings.Contains(out.String(), archivePath) {
		t.Fatalf("backup output did not include archive path: %q", out.String())
	}

	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		t.Fatalf("open backup archive: %v", err)
	}
	defer func() { _ = reader.Close() }()

	names := zipEntryNames(reader.File)
	for _, want := range []string{
		backupManifestName,
		backupLocalConfigPrefix + "/ca.crt",
		backupLocalConfigPrefix + "/nodes/master-01/token",
		backupTopologyPrefix + "/mesh-topology.yml",
		backupControlPlanePrefix + "/audit.log",
	} {
		if !containsString(names, want) {
			t.Fatalf("archive missing %q in %#v", want, names)
		}
	}

	manifest := decodeArchiveManifest(t, reader.File)
	if manifest.Format != backupArchiveFormat || manifest.Version != backupArchiveVersion {
		t.Fatalf("unexpected manifest identity: %+v", manifest)
	}
	if !manifest.Includes.LocalConfig || !manifest.Includes.Topology || !manifest.Includes.ControlPlaneState {
		t.Fatalf("manifest includes wrong state set: %+v", manifest.Includes)
	}
}

func TestRunRestoreCommandRequiresConfirmBeforeOverwrite(t *testing.T) {
	dir := t.TempDir()
	sourceConfigDir, sourceTopologyPath, _ := writeBackupFixture(t, filepath.Join(dir, "source"))
	archivePath := filepath.Join(dir, "mesh-backup.zip")
	if err := runBackupCommand(backupOptions{
		archivePath:  archivePath,
		topologyPath: sourceTopologyPath,
		configDir:    sourceConfigDir,
		stdout:       &bytes.Buffer{},
	}); err != nil {
		t.Fatalf("backup fixture: %v", err)
	}

	restoreConfigDir := filepath.Join(dir, "restore-config")
	restoreTopologyPath := filepath.Join(dir, "restore-topology.yml")
	if err := os.MkdirAll(restoreConfigDir, 0o700); err != nil {
		t.Fatalf("create restore config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(restoreConfigDir, "stale"), []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale config: %v", err)
	}
	if err := os.WriteFile(restoreTopologyPath, []byte("stale topology"), 0o600); err != nil {
		t.Fatalf("write stale topology: %v", err)
	}

	err := runRestoreCommand(restoreOptions{
		archivePath:  archivePath,
		topologyPath: restoreTopologyPath,
		configDir:    restoreConfigDir,
		stdout:       &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("expected restore without --confirm to fail")
	}
	if !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("restore error should mention --confirm, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(restoreConfigDir, "stale")); err != nil {
		t.Fatalf("restore without confirm modified config dir: %v", err)
	}
}

func TestRunRestoreCommandValidatesManifestBeforeOverwrite(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "bad.zip")
	writeInvalidManifestArchive(t, archivePath)
	restoreConfigDir := filepath.Join(dir, "restore-config")
	if err := os.MkdirAll(restoreConfigDir, 0o700); err != nil {
		t.Fatalf("create restore config: %v", err)
	}
	stalePath := filepath.Join(restoreConfigDir, "stale")
	if err := os.WriteFile(stalePath, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale config: %v", err)
	}

	err := runRestoreCommand(restoreOptions{
		archivePath:  archivePath,
		topologyPath: filepath.Join(dir, "restore-topology.yml"),
		configDir:    restoreConfigDir,
		confirm:      true,
		stdout:       &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("expected invalid manifest to fail")
	}
	if !strings.Contains(err.Error(), "unsupported backup format") {
		t.Fatalf("unexpected restore error: %v", err)
	}
	if got, err := os.ReadFile(stalePath); err != nil || string(got) != "stale" {
		t.Fatalf("restore overwrote state before manifest validation: got=%q err=%v", string(got), err)
	}
}

func TestRunRestoreCommandValidatesTargetsBeforeOverwrite(t *testing.T) {
	dir := t.TempDir()
	sourceConfigDir, sourceTopologyPath, _ := writeBackupFixture(t, filepath.Join(dir, "source"))
	archivePath := filepath.Join(dir, "mesh-backup.zip")
	if err := runBackupCommand(backupOptions{
		archivePath:  archivePath,
		topologyPath: sourceTopologyPath,
		configDir:    sourceConfigDir,
		stdout:       &bytes.Buffer{},
	}); err != nil {
		t.Fatalf("backup fixture: %v", err)
	}

	restoreConfigDir := filepath.Join(dir, "restore-config")
	stalePath := filepath.Join(restoreConfigDir, "stale")
	writeFixtureFile(t, stalePath, "stale")
	err := runRestoreCommand(restoreOptions{
		archivePath:  archivePath,
		topologyPath: "",
		configDir:    restoreConfigDir,
		confirm:      true,
		stdout:       &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("expected invalid topology target to fail")
	}
	if !strings.Contains(err.Error(), "restore file path is required") {
		t.Fatalf("unexpected restore error: %v", err)
	}
	assertFileContent(t, stalePath, "stale")
}

func TestRunRestoreCommandRestoresConfirmedArchive(t *testing.T) {
	dir := t.TempDir()
	sourceConfigDir, sourceTopologyPath, controlStateDir := writeBackupFixture(t, filepath.Join(dir, "source"))
	archivePath := filepath.Join(dir, "mesh-backup.zip")
	if err := runBackupCommand(backupOptions{
		archivePath:          archivePath,
		topologyPath:         sourceTopologyPath,
		configDir:            sourceConfigDir,
		controlPlaneStateDir: controlStateDir,
		stdout:               &bytes.Buffer{},
	}); err != nil {
		t.Fatalf("backup fixture: %v", err)
	}

	restoreConfigDir := filepath.Join(dir, "restore-config")
	restoreTopologyPath := filepath.Join(dir, "restore-topology.yml")
	restoreControlStateDir := filepath.Join(dir, "restore-control-state")
	writeFixtureFile(t, filepath.Join(restoreConfigDir, "stale"), "stale")
	writeFixtureFile(t, restoreTopologyPath, "stale topology")
	writeFixtureFile(t, filepath.Join(restoreControlStateDir, "stale"), "stale")
	var out bytes.Buffer
	if err := runRestoreCommand(restoreOptions{
		archivePath:          archivePath,
		topologyPath:         restoreTopologyPath,
		configDir:            restoreConfigDir,
		controlPlaneStateDir: restoreControlStateDir,
		confirm:              true,
		stdout:               &out,
	}); err != nil {
		t.Fatalf("runRestoreCommand: %v", err)
	}
	if !strings.Contains(out.String(), archivePath) {
		t.Fatalf("restore output did not include archive path: %q", out.String())
	}

	assertFileContent(t, filepath.Join(restoreConfigDir, "ca.crt"), "ca")
	assertFileContent(t, filepath.Join(restoreConfigDir, "nodes", "master-01", "token"), "raw-token")
	assertFileContent(t, restoreTopologyPath, "schema_version: 2\n")
	assertFileContent(t, filepath.Join(restoreControlStateDir, "audit.log"), "registered\n")
}

func writeBackupFixture(t *testing.T, dir string) (string, string, string) {
	t.Helper()
	configDir := filepath.Join(dir, "config")
	nodeDir := filepath.Join(configDir, "nodes", "master-01")
	controlStateDir := filepath.Join(dir, "control-state")
	for _, path := range []string{nodeDir, controlStateDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("create fixture dir %s: %v", path, err)
		}
	}
	writeFixtureFile(t, filepath.Join(configDir, "ca.crt"), "ca")
	writeFixtureFile(t, filepath.Join(configDir, "ca.key"), "key")
	writeFixtureFile(t, filepath.Join(nodeDir, "token"), "raw-token")
	writeFixtureFile(t, filepath.Join(nodeDir, "mesh.token"), "mesh1.hash")
	writeFixtureFile(t, filepath.Join(controlStateDir, "audit.log"), "registered\n")

	topologyPath := filepath.Join(dir, "mesh-topology.yml")
	writeFixtureFile(t, topologyPath, "schema_version: 2\n")
	return configDir, topologyPath, controlStateDir
}

func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create fixture parent %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture file %s: %v", path, err)
	}
}

func writeInvalidManifestArchive(t *testing.T, archivePath string) {
	t.Helper()
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("create invalid archive: %v", err)
	}
	writer := zip.NewWriter(file)
	entry, err := writer.Create(backupManifestName)
	if err != nil {
		t.Fatalf("create invalid manifest entry: %v", err)
	}
	if _, err := entry.Write([]byte(`{"format":"wrong","version":1}`)); err != nil {
		t.Fatalf("write invalid manifest: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close invalid archive writer: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close invalid archive: %v", err)
	}
}

func decodeArchiveManifest(t *testing.T, files []*zip.File) backupManifest {
	t.Helper()
	for _, file := range files {
		if file.Name != backupManifestName {
			continue
		}
		body, err := readZipFile(file)
		if err != nil {
			t.Fatalf("read manifest: %v", err)
		}
		var manifest backupManifest
		if err := json.Unmarshal(body, &manifest); err != nil {
			t.Fatalf("decode manifest: %v", err)
		}
		return manifest
	}
	t.Fatalf("manifest missing")
	return backupManifest{}
}

func zipEntryNames(files []*zip.File) []string {
	names := make([]string, 0, len(files))
	for _, file := range files {
		names = append(names, file.Name)
	}
	return names
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read restored file %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("restored file %s = %q, want %q", path, string(got), want)
	}
}
