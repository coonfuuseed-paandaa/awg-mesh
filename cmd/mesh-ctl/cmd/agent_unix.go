//go:build !windows

package cmd

import (
	"net"
	"os"
)

// dialSSHAgent connects to the SSH agent via the unix socket at SSH_AUTH_SOCK.
// Returns (nil, nil) if SSH_AUTH_SOCK is unset (no agent available).
func dialSSHAgent() (net.Conn, error) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, nil
	}
	return net.Dial("unix", sock)
}
