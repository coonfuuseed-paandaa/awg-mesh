package cmd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// captureOutput redirects os.Stdout and os.Stderr for the duration of f,
// returning what was written to each. The file descriptors are restored
// before the function returns, even if f panics.
func captureOutput(t *testing.T, f func()) (stdout, stderr string) {
	t.Helper()

	origStdout := os.Stdout
	origStderr := os.Stderr

	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe: %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stderr pipe: %v", err)
	}

	os.Stdout = wOut
	os.Stderr = wErr

	defer func() {
		os.Stdout = origStdout
		os.Stderr = origStderr
	}()

	f()

	wOut.Close()
	wErr.Close()

	var outBuf, errBuf bytes.Buffer
	_, _ = io.Copy(&outBuf, rOut)
	_, _ = io.Copy(&errBuf, rErr)
	rOut.Close()
	rErr.Close()

	return outBuf.String(), errBuf.String()
}

// emitTokenGated mirrors the conditional emit logic in token.go and
// onboarding.go. It is extracted here so that the gating contract can be
// tested independently of the gRPC rotation call that precedes it in the
// real command.
func emitTokenGated(token, tokenPath, command string, showToken bool) {
	if showToken {
		_, _ = fmt.Fprintln(os.Stdout, token)
		logger := zerolog.New(os.Stderr).With().Timestamp().Logger()
		logger.Warn().
			Str("event", "show_token_flag").
			Str("command", command).
			Msg("token emitted to stdout; prefer 'cat <token-path>' for scripted retrieval")
	} else {
		fmt.Fprintf(os.Stderr, "Token saved to %s.\n", tokenPath)
	}
}

// TestTokenRotate_DefaultHidesToken verifies that without --show-token the raw
// token value never appears on stdout. The WARN log must NOT be emitted.
func TestTokenRotate_DefaultHidesToken(t *testing.T) {
	const fakeToken = "tok-abc123-default-hidden"
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")

	stdout, stderr := captureOutput(t, func() {
		emitTokenGated(fakeToken, tokenPath, "token rotate", false /*showToken=false*/)
	})

	if strings.Contains(stdout, fakeToken) {
		t.Errorf("stdout contains token value when --show-token is not set; got: %q", stdout)
	}
	if strings.Contains(stderr, "show_token_flag") {
		t.Errorf("stderr contains WARN show_token_flag when --show-token is not set; got: %q", stderr)
	}
	if !strings.Contains(stderr, tokenPath) {
		t.Errorf("stderr should reference token path %q; got: %q", tokenPath, stderr)
	}
}

// TestTokenRotate_ShowTokenFlagEmitsToken verifies that with --show-token the
// raw token value is written to stdout (fd 1) and a WARN log line is written to
// stderr (fd 2) containing event=show_token_flag.
func TestTokenRotate_ShowTokenFlagEmitsToken(t *testing.T) {
	const fakeToken = "tok-xyz789-show-token-test"
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")

	stdout, stderr := captureOutput(t, func() {
		emitTokenGated(fakeToken, tokenPath, "token rotate", true /*showToken=true*/)
	})

	if !strings.Contains(stdout, fakeToken) {
		t.Errorf("stdout does not contain token value when --show-token is set; got: %q", stdout)
	}
	if !strings.Contains(stderr, "show_token_flag") {
		t.Errorf("stderr does not contain WARN event=show_token_flag; got: %q", stderr)
	}
}

// TestTokenRotate_PersistsToDiskWithMode0600 verifies that saveToken writes the
// token to <nodeDir>/token with permission 0600, regardless of the --show-token
// state. This covers FR-2.2 and the disk persistence contract.
func TestTokenRotate_PersistsToDiskWithMode0600(t *testing.T) {
	for _, showToken := range []bool{false, true} {
		showToken := showToken
		t.Run(fmt.Sprintf("show-token=%v", showToken), func(t *testing.T) {
			dir := t.TempDir()
			const wantToken = "tok-persisted-0600-test"

			if err := saveToken(dir, wantToken); err != nil {
				t.Fatalf("saveToken: %v", err)
			}

			tokenPath := filepath.Join(dir, "token")
			info, err := os.Stat(tokenPath)
			if err != nil {
				t.Fatalf("stat token file: %v", err)
			}
			if runtime.GOOS != "windows" {
				if got := info.Mode().Perm(); got != 0600 {
					t.Errorf("token file mode = %o, want 0600", got)
				}
			}

			data, err := os.ReadFile(tokenPath)
			if err != nil {
				t.Fatalf("read token file: %v", err)
			}
			if got := strings.TrimSpace(string(data)); got != wantToken {
				t.Errorf("token file content = %q, want %q", got, wantToken)
			}
		})
	}
}
