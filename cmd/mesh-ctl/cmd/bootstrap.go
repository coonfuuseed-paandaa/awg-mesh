package cmd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// imageRefRE matches valid Docker image references:
//
//	[registry/]name[:tag][@digest]
//
// Allowed characters: alphanumerics, dots, dashes, underscores, forward
// slashes, colons, and the '@' separator before a digest. Anything else
// (semicolons, backticks, pipes, dollar signs, etc.) would be a shell
// metacharacter and is explicitly rejected.
var imageRefRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._\-/:@]*$`)

// validateImageRef returns an error when ref contains characters that are
// invalid in a Docker image reference. This prevents shell metacharacters
// from being injected into remote commands that concatenate the image name.
func validateImageRef(ref string) error {
	if ref == "" {
		return fmt.Errorf("image reference must not be empty")
	}
	if !imageRefRE.MatchString(ref) {
		return fmt.Errorf("image reference %q contains invalid characters (only alphanumerics, ., -, _, /, :, @ are allowed)", ref)
	}
	return nil
}

// shellQuote wraps s in single quotes for safe inclusion in a remote shell
// command. Single quotes are not valid in Docker image references so this
// function panics when s contains one — call validateImageRef first.
func shellQuote(s string) string {
	if strings.ContainsRune(s, '\'') {
		panic("shellQuote: value contains a single quote — validateImageRef must be called first")
	}
	return "'" + s + "'"
}

// bootstrapOpts holds all CLI options for the bootstrap command.
type bootstrapOpts struct {
	host             string
	user             string
	port             int
	sshKey           string
	sshPassphrase    string // --ssh-passphrase or MESH_SSH_KEY_PASSPHRASE env var
	image            string
	acceptNewHostKey bool
}

// newBootstrapCommand creates the cobra command for `mesh-ctl bootstrap`.
func newBootstrapCommand() *cobra.Command {
	var opts bootstrapOpts

	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Prepare a bare Linux host: install Docker and pull the awg-mesh-node image",
		Long: `bootstrap SSHes into a fresh Linux host, installs Docker if missing,
and pulls the awg-mesh-node image so the host is ready for 'mesh-ctl master prepare'
or 'mesh-ctl endpoint prepare'.

SSH host-key verification uses ~/.ssh/known_hosts by default.
Use --accept-new-host-key only for first-contact with a host you trust.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.host == "" {
				return fmt.Errorf("--host is required")
			}
			return runBootstrap(opts)
		},
	}

	cmd.Flags().StringVar(&opts.host, "host", "", "Target host IP or hostname (required)")
	cmd.Flags().StringVar(&opts.user, "user", "root", "SSH user")
	cmd.Flags().IntVar(&opts.port, "port", 22, "SSH port")
	cmd.Flags().StringVar(&opts.sshKey, "ssh-key", "", "Path to SSH private key (default: ~/.ssh/id_ed25519 then ~/.ssh/id_rsa)")
	cmd.Flags().StringVar(&opts.image, "image", "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:latest", "Docker image to pull")
	cmd.Flags().BoolVar(&opts.acceptNewHostKey, "accept-new-host-key", false,
		"Accept unknown host keys (INSECURE — use only for first contact with a trusted host)")

	return cmd
}

// runBootstrap orchestrates the bootstrap sequence on the remote host.
func runBootstrap(opts bootstrapOpts) error {
	// Validate image reference before opening any network connections — an
	// invalid value would later be concatenated into a remote shell command.
	if err := validateImageRef(opts.image); err != nil {
		return fmt.Errorf("invalid --image: %w", err)
	}

	logger := log.With().
		Str("host", opts.host).
		Str("user", opts.user).
		Int("port", opts.port).
		Logger()

	client, err := dialSSH(opts, logger)
	if err != nil {
		logger.Error().
			Str("event", "bootstrap_failed").
			Str("stage", "ssh_connect").
			Err(err).
			Msg("SSH connection failed")
		return fmt.Errorf("ssh connect to %s:%d: %w", opts.host, opts.port, err)
	}
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			logger.Warn().Err(closeErr).Msg("SSH close error")
		}
	}()

	logger.Info().Str("event", "bootstrap_ssh_connect").Msg("SSH connected")

	// Step 1: detect Docker
	logger.Info().Str("event", "bootstrap_docker_detect").Msg("Checking for Docker")
	dockerPresent, err := checkDockerPresent(client, logger)
	if err != nil {
		logger.Error().
			Str("event", "bootstrap_failed").
			Str("stage", "docker_detect").
			Err(err).
			Msg("Docker detection failed")
		return fmt.Errorf("detect docker: %w", err)
	}

	// Step 2: install Docker if absent
	if !dockerPresent {
		logger.Info().Str("event", "bootstrap_docker_install").Msg("Docker not found — installing via get.docker.com")
		if err := installDocker(client, logger); err != nil {
			logger.Error().
				Str("event", "bootstrap_failed").
				Str("stage", "docker_install").
				Err(err).
				Msg("Docker installation failed")
			return fmt.Errorf("install docker: %w", err)
		}
	} else {
		logger.Info().Str("event", "bootstrap_docker_detect").Msg("Docker already present")
	}

	// Step 3: verify Docker version
	dockerVersion, err := runRemoteOutput(client, "docker --version")
	if err != nil {
		logger.Error().
			Str("event", "bootstrap_failed").
			Str("stage", "docker_verify").
			Err(err).
			Msg("Docker version check failed")
		return fmt.Errorf("verify docker after install: %w", err)
	}
	dockerVersion = strings.TrimSpace(dockerVersion)

	// Step 4: pull image
	logger.Info().
		Str("event", "bootstrap_docker_pull").
		Str("image", opts.image).
		Msg("Pulling Docker image")
	if err := pullImage(client, opts.image, logger); err != nil {
		logger.Error().
			Str("event", "bootstrap_failed").
			Str("stage", "docker_pull").
			Err(err).
			Msg("Docker image pull failed")
		return fmt.Errorf("pull image %q: %w", opts.image, err)
	}

	// Step 5: get image digest
	digest, err := runRemoteOutput(client,
		fmt.Sprintf("docker inspect --format='{{index .RepoDigests 0}}' %s 2>/dev/null || echo unknown", shellQuote(opts.image)))
	if err != nil {
		digest = "unknown"
	}
	digest = strings.TrimSpace(digest)

	logger.Info().Str("event", "bootstrap_complete").Msg("Bootstrap completed successfully")

	fmt.Printf("\n[bootstrap] SUCCESS\n")
	fmt.Printf("  Host:          %s\n", opts.host)
	fmt.Printf("  Docker:        %s\n", dockerVersion)
	fmt.Printf("  Image:         %s\n", opts.image)
	fmt.Printf("  Image digest:  %s\n", digest)
	fmt.Printf("\nHost is ready. Run 'mesh-ctl master prepare' or 'mesh-ctl endpoint prepare' next.\n")

	return nil
}

// dialSSH establishes an SSH client connection using agent, key file, or both.
// The SSH agent socket connection (if used) is closed automatically after the
// handshake completes — callers do not need to manage its lifetime.
func dialSSH(opts bootstrapOpts, logger zerolog.Logger) (*ssh.Client, error) {
	authMethods, cleanup, err := buildAuthMethods(opts, logger)
	if err != nil {
		return nil, fmt.Errorf("build auth methods: %w", err)
	}
	// Close the agent socket connection once auth is complete (after ssh.Dial).
	defer cleanup()

	hostKeyCallback, err := buildHostKeyCallback(opts)
	if err != nil {
		return nil, fmt.Errorf("build host key callback: %w", err)
	}

	config := &ssh.ClientConfig{
		User:            opts.user,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         30 * time.Second,
	}

	addr := fmt.Sprintf("%s:%d", opts.host, opts.port)
	return ssh.Dial("tcp", addr, config)
}

// buildAuthMethods returns SSH auth methods (agent first, then key file), plus
// a cleanup function that closes the agent connection. The cleanup MUST be
// called after ssh.Dial returns — authentication is complete by then and the
// agent connection is no longer needed.
//
// Agent dial is handled by the platform-specific dialSSHAgent() helper
// (agent_unix.go / agent_windows.go).
func buildAuthMethods(opts bootstrapOpts, logger zerolog.Logger) ([]ssh.AuthMethod, func(), error) {
	var methods []ssh.AuthMethod
	cleanup := func() {} // no-op by default; replaced when agent conn is opened

	agentSock := os.Getenv("SSH_AUTH_SOCK")

	conn, agentErr := dialSSHAgent()
	if agentErr != nil {
		// Agent dial failed — emit FR-3 diagnostic when no key file is available.
		if opts.sshKey == "" {
			logger.Warn().Err(agentErr).Str("ssh_auth_sock", agentSock).
				Msg("SSH agent dial failed and no --ssh-key provided")
		} else {
			logger.Warn().Err(agentErr).Msg("SSH agent dial failed — falling back to key file")
		}
	} else if conn != nil {
		cleanup = func() { _ = conn.Close() }
		agentClient := agent.NewClient(conn)
		methods = append(methods, ssh.PublicKeysCallback(agentClient.Signers))
		logger.Debug().Msg("Using SSH agent for authentication")
	}
	// conn == nil && agentErr == nil means no agent configured (SSH_AUTH_SOCK unset on Unix).

	// Resolve key path: explicit flag → id_ed25519 → id_rsa.
	keyPath := opts.sshKey
	if keyPath == "" {
		keyPath = resolveDefaultSSHKey()
	}

	if keyPath != "" {
		signer, err := loadPrivateKey(keyPath, opts.sshPassphrase)
		if err != nil {
			if len(methods) == 0 {
				cleanup()
				return nil, nil, fmt.Errorf("load SSH private key %q: %w", keyPath, err)
			}
			logger.Warn().Err(err).Str("key_path", keyPath).Msg("Could not load key file — agent-only auth")
		} else {
			methods = append(methods, ssh.PublicKeys(signer))
			logger.Debug().Str("key_path", keyPath).Msg("Using key file for authentication")
		}
	}

	if len(methods) == 0 {
		cleanup()
		// Emit FR-3 diagnostic when agent dial failed and no key is available.
		if agentErr != nil && agentSock != "" {
			return nil, nil, fmt.Errorf(
				"SSH authentication unavailable: SSH_AUTH_SOCK is set to %q but the agent dial failed: %v. "+
					"On Windows, ensure the OpenSSH agent service is running (`Get-Service ssh-agent`) "+
					"and SSH_AUTH_SOCK points to `\\\\.\\pipe\\openssh-ssh-agent`. "+
					"Or pass --ssh-key <path> to load a key file directly",
				agentSock, agentErr)
		}
		return nil, nil, fmt.Errorf("no SSH auth method available: no agent and no private key found")
	}

	return methods, cleanup, nil
}

// resolveDefaultSSHKey returns the first existing default key path.
func resolveDefaultSSHKey() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	candidates := []string{
		filepath.Join(home, ".ssh", "id_ed25519"),
		filepath.Join(home, ".ssh", "id_rsa"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// loadPrivateKey reads and parses a PEM-encoded private key.
// passphrase is tried first from the argument; if empty, MESH_SSH_KEY_PASSPHRASE
// is consulted. If neither is set and the key is passphrase-protected, a
// user-friendly error matching FR-2 is returned.
func loadPrivateKey(path, passphrase string) (ssh.Signer, error) {
	keyBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read key %q: %w", path, err)
	}

	if passphrase == "" {
		passphrase = os.Getenv("MESH_SSH_KEY_PASSPHRASE")
	}

	if passphrase != "" {
		signer, err := ssh.ParsePrivateKeyWithPassphrase(keyBytes, []byte(passphrase))
		if err != nil {
			return nil, fmt.Errorf("parse passphrase-protected key %q: %w", path, err)
		}
		return signer, nil
	}

	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		// golang.org/x/crypto/ssh exposes *ssh.PassphraseMissingError starting v0.0.0
		// but as of v0.30+ the public API still returns a plain error from
		// ParsePrivateKey; errors.As against *ssh.PassphraseMissingError only works
		// when the caller invokes ParsePrivateKeyWithPassphrase. For keys without
		// a passphrase attempt, we must string-match. If a future upstream release
		// exports a typed error from ParsePrivateKey, swap this for errors.As.
		if strings.Contains(err.Error(), "passphrase protected") {
			return nil, fmt.Errorf(
				"private key at %q is passphrase-protected but no passphrase provided. "+
					"Set --ssh-passphrase or MESH_SSH_KEY_PASSPHRASE, or use an unprotected key, "+
					"or load the key into an SSH agent and leave --ssh-key unset",
				path)
		}
		return nil, fmt.Errorf("parse private key %q: %w", path, err)
	}
	return signer, nil
}

// buildHostKeyCallback returns a host-key verification callback.
// Default: strict verification via ~/.ssh/known_hosts.
// With --accept-new-host-key: insecure (no verification).
func buildHostKeyCallback(opts bootstrapOpts) (ssh.HostKeyCallback, error) {
	if opts.acceptNewHostKey {
		//nolint:gosec // intentionally insecure when flag is set by operator
		return ssh.InsecureIgnoreHostKey(), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("locate home directory for known_hosts: %w", err)
	}

	knownHostsPath := filepath.Join(home, ".ssh", "known_hosts")
	cb, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil, fmt.Errorf(
			"load known_hosts from %q: %w\n  Hint: add the host fingerprint first with 'ssh-keyscan %s >> ~/.ssh/known_hosts',\n  or run bootstrap with --accept-new-host-key if you trust the host",
			knownHostsPath, err, opts.host)
	}

	return cb, nil
}

// checkDockerPresent runs `command -v docker` on the remote host.
// Returns true if Docker is found in PATH, false if not.
func checkDockerPresent(client *ssh.Client, logger zerolog.Logger) (bool, error) {
	sess, err := client.NewSession()
	if err != nil {
		return false, fmt.Errorf("open session: %w", err)
	}
	defer func() { _ = sess.Close() }()

	err = sess.Run("command -v docker >/dev/null 2>&1")
	if err == nil {
		return true, nil
	}

	// Exit code 1 means command not found — that is the expected "absent" result.
	if exitErr, ok := err.(*ssh.ExitError); ok && exitErr.ExitStatus() == 1 {
		return false, nil
	}

	// Unexpected error (e.g. signal, connection drop).
	logger.Warn().Err(err).Msg("docker detection returned unexpected error")
	return false, nil
}

// installDocker runs the official get.docker.com install script on the remote host,
// streaming output to the console with a [bootstrap] prefix.
func installDocker(client *ssh.Client, logger zerolog.Logger) error {
	sess, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("open session: %w", err)
	}
	defer func() { _ = sess.Close() }()

	sess.Stdout = &prefixWriter{prefix: "[bootstrap] ", w: os.Stdout}
	sess.Stderr = &prefixWriter{prefix: "[bootstrap] ", w: os.Stderr}

	logger.Info().Msg("Running: curl -fsSL https://get.docker.com | sh")
	if err := sess.Run("curl -fsSL https://get.docker.com | sh"); err != nil {
		return fmt.Errorf("docker install script exited with error: %w", err)
	}

	return nil
}

// pullImage runs `docker pull <image>` on the remote host, streaming progress.
func pullImage(client *ssh.Client, image string, logger zerolog.Logger) error {
	sess, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("open session: %w", err)
	}
	defer func() { _ = sess.Close() }()

	sess.Stdout = &prefixWriter{prefix: "[bootstrap] ", w: os.Stdout}
	sess.Stderr = &prefixWriter{prefix: "[bootstrap] ", w: os.Stderr}

	logger.Info().Str("image", image).Msg("Running: docker pull")
	if err := sess.Run("docker pull " + shellQuote(image)); err != nil {
		return fmt.Errorf("docker pull %q: %w", image, err)
	}

	return nil
}

// runRemoteOutput runs a command on the remote host and returns its stdout as a string.
func runRemoteOutput(client *ssh.Client, command string) (string, error) {
	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("open session: %w", err)
	}
	defer func() { _ = sess.Close() }()

	out, err := sess.Output(command)
	if err != nil {
		return "", fmt.Errorf("run %q: %w", command, err)
	}

	return string(out), nil
}

// prefixWriter wraps an io.Writer, prepending a fixed prefix to every line written.
type prefixWriter struct {
	prefix string
	w      io.Writer
	buf    []byte
}

// Write implements io.Writer. It buffers incomplete lines and flushes full lines with the prefix.
func (p *prefixWriter) Write(b []byte) (int, error) {
	p.buf = append(p.buf, b...)

	for {
		idx := bytes.IndexByte(p.buf, '\n')
		if idx < 0 {
			break
		}
		line := p.buf[:idx+1]
		if _, err := fmt.Fprintf(p.w, "%s%s", p.prefix, line); err != nil {
			return 0, err
		}
		p.buf = append([]byte{}, p.buf[idx+1:]...)
	}

	return len(b), nil
}
