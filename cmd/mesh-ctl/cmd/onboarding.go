package cmd

import (
	"crypto"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/pkg/tls"
)

// ensureCA loads the mesh CA from configDir, creating one on first run.
//
// CR-002 control plane will reuse this on operator-facing `mesh-ctl init`.
// In CR-001 the CA helpers stay shared between mesh-ctl subcommands and the
// future control-plane daemon (single source of truth for CA bootstrap).
func ensureCA(configDir string) (*x509.Certificate, crypto.PrivateKey, error) {
	caCert, caKey, err := pkgtls.LoadCA(configDir)
	if err == nil {
		return caCert, caKey, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}

	fmt.Fprintf(os.Stderr, "First run: creating mesh CA in %s\n", configDir)

	caCert, caKey, err = pkgtls.GenerateCA("awg-mesh-ca")
	if err != nil {
		return nil, nil, fmt.Errorf("generate CA: %w", err)
	}

	if err := pkgtls.SaveCA(configDir, caCert, caKey); err != nil {
		return nil, nil, fmt.Errorf("save CA: %w", err)
	}

	fmt.Fprintf(os.Stderr, "CA created: %s/ca.crt, %s/ca.key\n", configDir, configDir)
	return caCert, caKey, nil
}

// saveToken writes the bearer token under nodeDir.
func saveToken(nodeDir, token string) error {
	if err := os.MkdirAll(nodeDir, 0o700); err != nil {
		return fmt.Errorf("create node dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(nodeDir, "token"), []byte(token), 0o600); err != nil {
		return fmt.Errorf("write token file: %w", err)
	}
	return nil
}

// loadToken reads the bearer token from a node directory.
func loadToken(nd string) (string, error) {
	rawToken, err := os.ReadFile(filepath.Join(nd, "token"))
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	return strings.TrimSpace(string(rawToken)), nil
}

// nodeDir returns the per-node config subdirectory under configDir.
// Layout: <configDir>/nodes/<name>/ contains node.crt, node.key, token,
// transport.yml (until v2.0 control plane replaces transport.yml).
func nodeDir(configDir, name string) string {
	return filepath.Join(configDir, "nodes", name)
}

// caPath returns the path to the mesh CA certificate.
func caPath(configDir string) string {
	return filepath.Join(configDir, "ca.crt")
}

// containsName reports whether needle appears in list. Used by mesh-ctl
// subcommands that filter nodes by --node flag against topology lists.
func containsName(list []string, needle string) bool {
	for _, value := range list {
		if value == needle {
			return true
		}
	}
	return false
}

// F-009 CR-001: removed v1.x onboarding helpers:
//   - loadTemplate / renderDockerCompose / printNextSteps + embedded
//     templates/*.tmpl — v1.x master/endpoint/client docker-compose generators.
//     CR-010 mesh-ctl redesign reintroduces topology→compose generation
//     under role-agnostic v2.0 schema.
//   - loadOrCreateAllocator / saveTransportState — transport /30 pool
//     allocator. v2.0 has no transport layer; eliminated entirely.
