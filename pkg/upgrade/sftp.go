package upgrade

import (
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// DefaultRemoteComposeDir is the remote directory for uploaded compose files.
const DefaultRemoteComposeDir = "/etc/docker/compose"

// UploadComposeFile uploads adminPath to remotePath over SFTP using an existing ssh.Client.
// It creates the remote parent directory via SSH exec first, then uploads and verifies size.
// Exported so cmd/mesh-ctl/cmd/upgrade.go can call it from buildSSHUploader.
func UploadComposeFile(sshClient *ssh.Client, adminPath, remotePath string) error {
	// 1. mkdir -p remote parent dir via SSH exec (FR-2)
	parent := path.Dir(remotePath)
	if err := execRemote(sshClient, "mkdir -p "+shellQuote(parent)); err != nil {
		return fmt.Errorf("mkdir remote dir %q: %w", parent, err)
	}

	// 2. open SFTP subchannel (FR-1)
	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		return fmt.Errorf("open sftp subsystem on %q: %w", remotePath, err)
	}
	defer func() { _ = sftpClient.Close() }()

	// 3. open local source
	localFile, err := os.Open(adminPath)
	if err != nil {
		return fmt.Errorf("open admin compose %q: %w", adminPath, err)
	}
	defer localFile.Close()

	localStat, err := localFile.Stat()
	if err != nil {
		return fmt.Errorf("stat admin compose %q: %w", adminPath, err)
	}

	// 4. create/truncate remote file — idempotent overwrite (FR-7)
	remoteFile, err := sftpClient.Create(remotePath)
	if err != nil {
		return fmt.Errorf("create remote compose %q: %w", remotePath, err)
	}

	// 5. copy
	if _, err := io.Copy(remoteFile, localFile); err != nil {
		_ = remoteFile.Close()
		return fmt.Errorf("copy compose to %q: %w", remotePath, err)
	}

	// 6. close before stat to flush buffers
	if err := remoteFile.Close(); err != nil {
		return fmt.Errorf("close remote compose %q: %w", remotePath, err)
	}

	// 7. verify size after upload (FR-3)
	remoteStat, err := sftpClient.Stat(remotePath)
	if err != nil {
		return fmt.Errorf("stat remote compose %q after upload: %w", remotePath, err)
	}
	if remoteStat.Size() != localStat.Size() {
		return fmt.Errorf("size mismatch on %q after upload: local=%d remote=%d",
			remotePath, localStat.Size(), remoteStat.Size())
	}
	return nil
}

// execRemote runs a single remote command via SSH session, collecting stderr on failure.
func execRemote(sshClient *ssh.Client, cmd string) error {
	sess, err := sshClient.NewSession()
	if err != nil {
		return fmt.Errorf("open ssh session: %w", err)
	}
	defer func() { _ = sess.Close() }()
	var errBuf strings.Builder
	sess.Stderr = &errBuf
	if err := sess.Run(cmd); err != nil {
		return fmt.Errorf("remote cmd %q failed: %w (stderr: %s)", cmd, err, errBuf.String())
	}
	return nil
}

// remoteComposePath returns the remote absolute path for a node's compose file.
// Uses path.Join (POSIX) not filepath.Join for cross-platform admin support (NFR-4).
func remoteComposePath(remoteDir, name string) string {
	dir := remoteDir
	if dir == "" {
		dir = DefaultRemoteComposeDir
	}
	return path.Join(dir, name+"-docker-compose.yml")
}

// remoteBackupComposePath returns the remote path for the rollback backup compose.
func remoteBackupComposePath(remoteDir, name string) string {
	dir := remoteDir
	if dir == "" {
		dir = DefaultRemoteComposeDir
	}
	return path.Join(dir, name+"-docker-compose.yml.bak")
}
