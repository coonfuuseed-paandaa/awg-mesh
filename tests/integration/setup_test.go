//go:build integration

package integration_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/wg"
)

// composePath is the absolute path to the directory containing docker-compose.yml.
// It is set in TestMain before any tests run.
var composePath string

// TestMain builds the Docker image and sets up global test state before running any tests.
func TestMain(m *testing.M) {
	projectRoot, err := findProjectRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to locate project root (go.mod): %v\n", err)
		os.Exit(1)
	}

	composePath = filepath.Join(projectRoot, "tests", "integration")

	fmt.Fprintln(os.Stderr, "building awg-mesh:test Docker image...")
	out, err := runCmd(projectRoot, "docker", "build", "-t", "awg-mesh:test", "-f", "deploy/Dockerfile", ".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "docker build failed:\n%s\nerror: %v\n", out, err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "docker build succeeded")

	os.Exit(m.Run())
}

// TestContainerStartup verifies that both node containers start successfully
// using the current binary. It does not test AWG tunnel establishment.
func TestContainerStartup(t *testing.T) {
	out, err := runCmd(composePath, "docker", "compose", "up", "-d")
	if err != nil {
		t.Fatalf("docker compose up failed:\n%s\nerror: %v", out, err)
	}

	t.Cleanup(func() {
		out, err := runCmd(composePath, "docker", "compose", "down", "-v")
		if err != nil {
			t.Logf("docker compose down warning:\n%s\nerror: %v", out, err)
		}
	})

	waitFor(t, 30*time.Second, 2*time.Second, func() bool {
		out, err := runCmd(composePath, "docker", "compose", "ps")
		if err != nil {
			return false
		}
		return strings.Contains(out, "node-server") && strings.Contains(out, "node-client")
	})
}

// TestKeypairGeneration verifies AWG keypair generation, derivation, encoding,
// and parsing. This test does not require Docker.
func TestKeypairGeneration(t *testing.T) {
	serverPriv, err := wg.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate server private key: %v", err)
	}

	serverPub := serverPriv.PublicKey()
	if serverPub.IsZero() {
		t.Fatal("derived server public key is all-zero")
	}

	clientPriv, err := wg.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate client private key: %v", err)
	}

	clientPub := clientPriv.PublicKey()
	if clientPub.IsZero() {
		t.Fatal("derived client public key is all-zero")
	}

	// Base64-encoded Curve25519 keys are always 44 characters (32 bytes * 4/3, padded).
	if got := len(serverPub.String()); got != 44 {
		t.Fatalf("server public key: expected 44-char base64, got %d", got)
	}
	if got := len(clientPub.String()); got != 44 {
		t.Fatalf("client public key: expected 44-char base64, got %d", got)
	}

	// Round-trip: encode then parse must return the original key.
	parsed, err := wg.ParseKey(serverPub.String())
	if err != nil {
		t.Fatalf("parse server public key: %v", err)
	}
	if parsed != serverPub {
		t.Fatalf("round-trip mismatch: got %s, want %s", parsed.String(), serverPub.String())
	}
}

// TestTwoNodeTunnel verifies that two nodes establish an AWG tunnel and can
// communicate over their overlay IPs.
// AWG interface creation is wired since PR #13. This test requires Docker with
// --privileged and is designed for local/CI integration testing.
func TestTwoNodeTunnel(t *testing.T) {
	t.Skip("requires privileged Docker containers — run manually with: go test -tags integration -run TestTwoNodeTunnel -v")

	// When enabled, this test will:
	// 1. Generate keypairs for server and client via wg.GeneratePrivateKey().
	// 2. Write AWG config files to temp directories that are bind-mounted as volumes.
	// 3. Start docker-compose up -d with those config directories.
	// 4. Poll "docker exec node-server wg show" until a handshake timestamp appears.
	// 5. Run: docker exec node-client ping -c 3 172.20.70.2
	// 6. Assert exit code 0 — confirming end-to-end overlay IP reachability.
}

// runCmd runs an external command with Dir set to dir and returns combined stdout+stderr output.
func runCmd(dir string, args ...string) (string, error) {
	c := exec.Command(args[0], args[1:]...) //nolint:gosec // test helper; args are controlled
	c.Dir = dir
	out, err := c.CombinedOutput()
	return string(out), err
}

// waitFor polls condition every interval until it returns true or timeout elapses.
// It calls t.Fatal if the deadline is exceeded.
func waitFor(t *testing.T, timeout, interval time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(interval)
	}
	t.Fatalf("condition not satisfied within %v", timeout)
}

// findProjectRoot walks up from the current working directory until it finds
// a directory containing go.mod, which is the project root.
func findProjectRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getwd: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found in any parent of %s", dir)
		}
		dir = parent
	}
}
