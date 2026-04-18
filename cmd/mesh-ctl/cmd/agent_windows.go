//go:build windows

package cmd

import (
	"net"
	"os"

	"github.com/Microsoft/go-winio"
)

// defaultWindowsSSHAgentPipe is the standard named-pipe path for the Windows
// OpenSSH agent service.
const defaultWindowsSSHAgentPipe = `\\.\pipe\openssh-ssh-agent`

// dialSSHAgent connects to the Windows OpenSSH agent via named pipe.
// Respects SSH_AUTH_SOCK if set (some tools set it to the pipe path);
// otherwise falls back to defaultWindowsSSHAgentPipe.
// Returns (nil, nil) only when neither SSH_AUTH_SOCK is set nor the default
// pipe was explicitly tried; callers interpret (nil, nil) as "no agent
// configured".
//
// Note: unlike the Unix variant, the Windows pipe always has a default
// location so (nil, nil) is never returned — we always attempt the dial.
func dialSSHAgent() (net.Conn, error) {
	pipe := os.Getenv("SSH_AUTH_SOCK")
	if pipe == "" {
		pipe = defaultWindowsSSHAgentPipe
	}
	return winio.DialPipe(pipe, nil)
}
