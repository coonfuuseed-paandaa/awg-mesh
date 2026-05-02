//go:build linux

package wg

import (
	"net"
	"strings"
	"testing"
	"time"
)

func TestUAPIConnectionDeadline(t *testing.T) {
	t.Parallel()

	// Verify the constant is set correctly
	if uapiConnectionTimeout != 30*time.Second {
		t.Fatalf("expected uapiConnectionTimeout = 30s, got %v", uapiConnectionTimeout)
	}
}

func TestInterfaceConstructorsRejectInvalidNames(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"", "wg/name", `wg\\name`, "wg..name", "0123456789abcdef"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewInterface(name, 1420, nil); err == nil {
				t.Fatalf("NewInterface accepted invalid name %q", name)
			}
			_, err := OpenExistingInterface(name)
			if err == nil {
				t.Fatalf("OpenExistingInterface accepted invalid name %q", name)
			}
			if strings.Contains(err.Error(), "uapi socket") {
				t.Fatalf("OpenExistingInterface reached UAPI socket path for invalid name %q: %v", name, err)
			}
		})
	}
}

func TestSetDeadlineOnConnection(t *testing.T) {
	t.Parallel()

	// Create a pipe to simulate a connection
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

	// Verify SetDeadline doesn't error
	deadline := time.Now().Add(uapiConnectionTimeout)
	if err := server.SetDeadline(deadline); err != nil {
		t.Fatalf("SetDeadline failed: %v", err)
	}

	// After deadline, reads should fail
	shortDeadline := time.Now().Add(10 * time.Millisecond)
	if err := server.SetDeadline(shortDeadline); err != nil {
		t.Fatalf("SetDeadline (short) failed: %v", err)
	}

	time.Sleep(20 * time.Millisecond)
	buf := make([]byte, 1)
	_, err := server.Read(buf)
	if err == nil {
		t.Fatal("expected timeout error after deadline")
	}
}
