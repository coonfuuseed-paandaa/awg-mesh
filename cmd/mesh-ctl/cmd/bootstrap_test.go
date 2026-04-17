package cmd

import (
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// TestValidateImageRef verifies that validateImageRef accepts valid Docker image
// references and rejects values containing shell metacharacters.
func TestValidateImageRef(t *testing.T) {
	valid := []string{
		"ubuntu",
		"ubuntu:22.04",
		"ghcr.io/org/repo:latest",
		"ghcr.io/org/repo:v1.2.3",
		"registry.example.com:5000/name@sha256:abc123",
		"a/b/c:tag",
	}
	for _, ref := range valid {
		if err := validateImageRef(ref); err != nil {
			t.Errorf("validateImageRef(%q) returned unexpected error: %v", ref, err)
		}
	}

	invalid := []string{
		"",
		"img; rm -rf /",
		"img`touch /pwned`",
		"img$(id)",
		"img|sh",
		"img\necho",
		"img && bad",
	}
	for _, ref := range invalid {
		if err := validateImageRef(ref); err == nil {
			t.Errorf("validateImageRef(%q) expected error, got nil", ref)
		}
	}
}

// TestShellQuote verifies that shellQuote wraps values in single quotes.
func TestShellQuote(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"ubuntu", "'ubuntu'"},
		{"ghcr.io/org/repo:latest", "'ghcr.io/org/repo:latest'"},
		{"registry:5000/name@sha256:abc", "'registry:5000/name@sha256:abc'"},
	}
	for _, c := range cases {
		got := shellQuote(c.input)
		if got != c.want {
			t.Errorf("shellQuote(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

// TestBootstrapHelpFlags verifies that the bootstrap subcommand exposes all
// required flags in its help output. This does not establish a real SSH
// connection — it only exercises the cobra command setup.
func TestBootstrapHelpFlags(t *testing.T) {
	root := NewRootCommand("test")

	// Capture output by redirecting stdout via pipe.
	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w

	root.SetArgs([]string{"bootstrap", "--help"})
	_ = root.Execute()

	w.Close()
	os.Stdout = origStdout

	out, _ := io.ReadAll(r)
	helpText := string(out)

	requiredFlags := []string{
		"--host",
		"--user",
		"--port",
		"--ssh-key",
		"--image",
		"--accept-new-host-key",
	}
	for _, flag := range requiredFlags {
		if !strings.Contains(helpText, flag) {
			t.Errorf("bootstrap --help missing flag %q\nFull output:\n%s", flag, helpText)
		}
	}
}

// TestBootstrapHostRequired verifies that omitting --host returns a non-nil error.
func TestBootstrapHostRequired(t *testing.T) {
	root := NewRootCommand("test")
	root.SetArgs([]string{"bootstrap"})

	// Suppress usage output for this test.
	root.SilenceUsage = true
	root.SilenceErrors = true

	err := root.Execute()
	if err == nil {
		t.Error("expected error when --host is omitted, got nil")
	}
}

// TestResolveDefaultSSHKey verifies the key resolution: ed25519 wins over rsa.
func TestResolveDefaultSSHKey(t *testing.T) {
	dir := t.TempDir()

	// Redirect both HOME (Linux) and USERPROFILE (Windows) so that
	// os.UserHomeDir() returns our temp directory on all platforms.
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)

	// No key files yet — expect empty string.
	got := resolveDefaultSSHKey()
	if got != "" {
		t.Errorf("expected empty key path when no keys exist, got %q", got)
	}

	// Create only id_rsa — should return it.
	sshDir := filepath.Join(dir, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rsaPath := filepath.Join(sshDir, "id_rsa")
	if err := os.WriteFile(rsaPath, []byte("dummy"), 0600); err != nil {
		t.Fatalf("write id_rsa: %v", err)
	}

	got = resolveDefaultSSHKey()
	if got != rsaPath {
		t.Errorf("expected %q, got %q", rsaPath, got)
	}

	// Add id_ed25519 — it should now win.
	ed25519Path := filepath.Join(sshDir, "id_ed25519")
	if err := os.WriteFile(ed25519Path, []byte("dummy"), 0600); err != nil {
		t.Fatalf("write id_ed25519: %v", err)
	}

	got = resolveDefaultSSHKey()
	if got != ed25519Path {
		t.Errorf("ed25519 should win over rsa: expected %q, got %q", ed25519Path, got)
	}
}

// TestPrefixWriter verifies that prefixWriter prepends the prefix to each line.
func TestPrefixWriter(t *testing.T) {
	var out strings.Builder
	pw := &prefixWriter{prefix: "[bootstrap] ", w: &out}

	input := "line one\nline two\n"
	n, err := pw.Write([]byte(input))
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if n != len(input) {
		t.Errorf("Write returned %d, want %d", n, len(input))
	}

	got := out.String()
	if !strings.Contains(got, "[bootstrap] line one\n") {
		t.Errorf("expected '[bootstrap] line one\\n' in output, got:\n%s", got)
	}
	if !strings.Contains(got, "[bootstrap] line two\n") {
		t.Errorf("expected '[bootstrap] line two\\n' in output, got:\n%s", got)
	}
}

// TestPrefixWriterNoTrailingNewline verifies that incomplete lines are buffered.
func TestPrefixWriterNoTrailingNewline(t *testing.T) {
	var out strings.Builder
	pw := &prefixWriter{prefix: "[bootstrap] ", w: &out}

	// Write without trailing newline — should buffer, not flush.
	_, _ = pw.Write([]byte("partial"))
	if out.Len() != 0 {
		t.Errorf("expected empty output for partial line, got: %q", out.String())
	}

	// Now flush with a newline.
	_, _ = pw.Write([]byte(" line\n"))
	got := out.String()
	if got != "[bootstrap] partial line\n" {
		t.Errorf("expected '[bootstrap] partial line\\n', got: %q", got)
	}
}

// TestBuildHostKeyCallbackInsecure verifies that --accept-new-host-key yields
// a non-nil callback (InsecureIgnoreHostKey).
func TestBuildHostKeyCallbackInsecure(t *testing.T) {
	opts := bootstrapOpts{acceptNewHostKey: true}
	cb, err := buildHostKeyCallback(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cb == nil {
		t.Error("expected non-nil callback")
	}
}

// TestMockSSHSession exercises the SSH session wrapper logic using a real
// in-process SSH server via golang.org/x/crypto/ssh. This validates the
// session plumbing without requiring an external host.
//
// The test spins up a minimal SSH server that accepts one channel, runs one
// command ("echo hello"), and closes. It verifies that runRemoteOutput returns
// the expected output.
func TestMockSSHSession(t *testing.T) {
	// Generate a host key for the mock server.
	hostKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatalf("signer from host key: %v", err)
	}

	// Generate a client key for authentication.
	clientKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	clientPub, err := ssh.NewPublicKey(&clientKey.PublicKey)
	if err != nil {
		t.Fatalf("client public key: %v", err)
	}

	// Server config: accept the client's public key.
	serverCfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if ssh.FingerprintSHA256(key) == ssh.FingerprintSHA256(clientPub) {
				return &ssh.Permissions{}, nil
			}
			return nil, fmt.Errorf("unauthorized key")
		},
	}
	serverCfg.AddHostKey(hostSigner)

	// Start listener on a random port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	// Serve one connection in a goroutine.
	errCh := make(chan error, 1)
	go func() {
		conn, listenErr := ln.Accept()
		if listenErr != nil {
			errCh <- listenErr
			return
		}
		sconn, chans, reqs, sshErr := ssh.NewServerConn(conn, serverCfg)
		if sshErr != nil {
			errCh <- sshErr
			return
		}
		defer func() { _ = sconn.Close() }()
		go ssh.DiscardRequests(reqs)

		for newChan := range chans {
			if newChan.ChannelType() != "session" {
				_ = newChan.Reject(ssh.UnknownChannelType, "only session supported")
				continue
			}
			ch, chanReqs, acceptErr := newChan.Accept()
			if acceptErr != nil {
				errCh <- acceptErr
				return
			}
			for req := range chanReqs {
				if req.Type == "exec" {
					// Parse the command (4-byte length prefix + command bytes per SSH protocol).
					if len(req.Payload) < 4 {
						_ = req.Reply(false, nil)
						continue
					}
					cmdLen := int(req.Payload[0])<<24 | int(req.Payload[1])<<16 |
						int(req.Payload[2])<<8 | int(req.Payload[3])
					if len(req.Payload) < 4+cmdLen {
						_ = req.Reply(false, nil)
						continue
					}
					_ = req.Reply(true, nil)
					_, _ = fmt.Fprint(ch, "hello\n")
					_, _ = ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
					_ = ch.Close()
				}
			}
		}
		errCh <- nil
	}()

	// Build client connection using the client private key.
	clientSigner, err := ssh.NewSignerFromKey(clientKey)
	if err != nil {
		t.Fatalf("client signer: %v", err)
	}

	hostKeyCallback := ssh.FixedHostKey(hostSigner.PublicKey())
	clientCfg := &ssh.ClientConfig{
		User:            "test",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(clientSigner)},
		HostKeyCallback: hostKeyCallback,
	}

	addr := ln.Addr().String()
	sshClient, err := ssh.Dial("tcp", addr, clientCfg)
	if err != nil {
		t.Fatalf("dial mock server: %v", err)
	}
	defer func() { _ = sshClient.Close() }()

	out, err := runRemoteOutput(sshClient, "echo hello")
	if err != nil {
		t.Fatalf("runRemoteOutput: %v", err)
	}

	if strings.TrimSpace(out) != "hello" {
		t.Errorf("expected 'hello', got %q", out)
	}

	// Close the client so the server goroutine can finish and write to errCh.
	_ = sshClient.Close()

	// Drain the server goroutine result to catch any server-side errors.
	if serverErr := <-errCh; serverErr != nil {
		t.Errorf("mock SSH server error: %v", serverErr)
	}
}
