package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// v151Compose is a minimal v1.5.1-schema docker-compose snippet for testing.
const v151Compose = `services:
  awg-mesh-node:
    image: ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.5.1
    network_mode: host
    restart: unless-stopped
    cap_add:
      - NET_ADMIN
      - NET_RAW
    devices:
      - /dev/net/tun:/dev/net/tun
    command:
      - awg-mesh-node
      - --mode=endpoint
      - --name=ep-01
      - --overlay-ip=10.10.0.2
      - --listen-port=51820
    environment:
      - MESH_TOKEN=plaintexttoken
    volumes:
      - /var/lib/awg/ep-01:/data
`

// currentCompose is a minimal current-schema docker-compose snippet.
const currentCompose = `services:
  awg-mesh-node:
    image: ghcr.io/coonfuuseed-paandaa/awg-mesh-node:latest
    network_mode: host
    restart: unless-stopped
    cap_add:
      - NET_ADMIN
      - NET_RAW
    devices:
      - /dev/net/tun:/dev/net/tun
    environment:
      - MESH_TOKEN_HASH=$2a$10$abcdefghijklmnopqrstuuVwxyz
      - MESH_MODE=endpoint
      - MESH_NAME=ep-02
      - MESH_OVERLAY_IP=10.10.0.3
      - MESH_LISTEN_PORT=51820
      - MESH_CONFIG_DIR=/config
    volumes:
      - /var/lib/awg-mesh/ep-02:/config
`

// TestUpgradeComposeStdout verifies that without --in-place the migrated
// content is written to stdout and the source file is not modified.
func TestUpgradeComposeStdout(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(src, []byte(v151Compose), 0600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	origContent, _ := os.ReadFile(src)

	// Capture stdout by redirecting the command's output.
	var buf bytes.Buffer
	cmd := newUpgradeComposeCommand()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{src})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("command failed: %v", err)
	}

	// Source file must be unchanged.
	afterContent, _ := os.ReadFile(src)
	if !bytes.Equal(origContent, afterContent) {
		t.Error("source file was modified without --in-place")
	}

	// Backup must not exist.
	if _, err := os.Stat(src + ".bak"); err == nil {
		t.Error("unexpected .bak file created in stdout mode")
	}
}

// TestUpgradeComposeInPlace verifies that --in-place rewrites the file and
// saves the original as <file>.bak.
func TestUpgradeComposeInPlace(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(src, []byte(v151Compose), 0600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cmd := newUpgradeComposeCommand()
	cmd.SetArgs([]string{"--in-place", src})
	var errBuf bytes.Buffer
	cmd.SetErr(&errBuf)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("command failed: %v", err)
	}

	// Backup must exist and contain the original content.
	bakContent, err := os.ReadFile(src + ".bak")
	if err != nil {
		t.Fatalf("backup not created: %v", err)
	}
	if !bytes.Equal(bakContent, []byte(v151Compose)) {
		t.Error("backup content does not match original")
	}

	// Migrated file must contain MESH_CONFIG_DIR (current schema marker).
	migratedContent, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read migrated file: %v", err)
	}
	if !strings.Contains(string(migratedContent), "MESH_CONFIG_DIR") {
		t.Error("migrated compose missing MESH_CONFIG_DIR — migration did not apply")
	}
	// Must NOT contain the old MESH_TOKEN plain key.
	if strings.Contains(string(migratedContent), "MESH_TOKEN=") {
		t.Error("migrated compose still contains plain MESH_TOKEN")
	}
}

// TestUpgradeComposeAlreadyCurrent verifies that a current-schema file causes
// an early exit with "already current schema" message and no .bak file.
func TestUpgradeComposeAlreadyCurrent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(src, []byte(currentCompose), 0600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	var errBuf bytes.Buffer
	cmd := newUpgradeComposeCommand()
	cmd.SetArgs([]string{src})
	cmd.SetErr(&errBuf)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("command failed for already-current schema: %v", err)
	}

	// Message must indicate nothing was done.
	if !strings.Contains(errBuf.String(), "already current schema") {
		t.Errorf("expected 'already current schema' in stderr, got: %q", errBuf.String())
	}

	// No backup must be created.
	if _, err := os.Stat(src + ".bak"); err == nil {
		t.Error("unexpected .bak file for already-current schema")
	}
}
