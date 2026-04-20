package cmd

import (
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestUpdateTunnelPeerFailureStatus verifies the pure helper that classifies
// UpdateTunnelPeer errors for T011 (pre-v1.10.0 master detection).
//
// Anti-stub guarantee: replacing the body with `return "FAILED", false` causes
// the codes.Unimplemented case to fail (isPreV110 == false instead of true)
// and the status line to omit "pre-v1.10.0".
func TestUpdateTunnelPeerFailureStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		err          error
		wantPreV110  bool
		wantContains string
	}{
		{
			name:         "Unimplemented maps to pre-v1.10.0",
			err:          status.Error(codes.Unimplemented, "method UpdateTunnelPeer not implemented"),
			wantPreV110:  true,
			wantContains: "pre-v1.10.0",
		},
		{
			name:         "Internal error is generic FAILED",
			err:          status.Error(codes.Internal, "something broke"),
			wantPreV110:  false,
			wantContains: "something broke",
		},
		{
			name:         "Unavailable error is generic FAILED",
			err:          status.Error(codes.Unavailable, "connection refused"),
			wantPreV110:  false,
			wantContains: "connection refused",
		},
		{
			name:         "DeadlineExceeded is generic FAILED",
			err:          status.Error(codes.DeadlineExceeded, "context deadline exceeded"),
			wantPreV110:  false,
			wantContains: "deadline exceeded",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			line, isPreV110 := updateTunnelPeerFailureStatus(tc.err)

			if isPreV110 != tc.wantPreV110 {
				t.Errorf("isPreV110 = %v, want %v", isPreV110, tc.wantPreV110)
			}
			if !strings.Contains(line, tc.wantContains) {
				t.Errorf("statusLine %q does not contain %q", line, tc.wantContains)
			}
			if !strings.HasPrefix(line, "FAILED:") {
				t.Errorf("statusLine %q does not start with 'FAILED:'", line)
			}
		})
	}
}

// TestUpdateTunnelPeerFailureStatus_NoStringMatch verifies that
// codes.Unimplemented is detected via typed code comparison, not string
// matching on the error message. An error with "Unimplemented" in the message
// but a non-Unimplemented gRPC code must NOT be classified as pre-v1.10.0.
func TestUpdateTunnelPeerFailureStatus_NoStringMatch(t *testing.T) {
	t.Parallel()

	// codes.Internal error whose message happens to contain "Unimplemented" —
	// must NOT trigger the pre-v1.10.0 path.
	err := status.Error(codes.Internal, "Unimplemented feature in handler")
	line, isPreV110 := updateTunnelPeerFailureStatus(err)

	if isPreV110 {
		t.Errorf("isPreV110 = true for codes.Internal error; typed-code check must be used, not string match")
	}
	if strings.Contains(line, "pre-v1.10.0") {
		t.Errorf("statusLine %q contains 'pre-v1.10.0' for codes.Internal error; should not", line)
	}
}

// TestEndpointPrepareImage asserts that the endpoint prepare path resolves the
// docker image reference through the three-level priority order defined by
// resolveImage: CLI flag > topology default > built-in fallback.
//
// Each case builds the same template data struct that newEndpointPrepareCommand
// builds at runtime, substituting only the Image field with the value that
// resolveImage would return, then renders the endpoint compose template and
// confirms the expected image line appears in the output.
func TestEndpointPrepareImage(t *testing.T) {
	t.Parallel()

	const fallback = "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:latest"

	cases := []struct {
		name      string
		cliFlag   string
		topoNode  string
		wantImage string
	}{
		{
			name:      "cli-flag wins",
			cliFlag:   "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.8.1",
			topoNode:  "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.7.0",
			wantImage: "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.8.1",
		},
		{
			name:      "topology-default used when no cli-flag",
			cliFlag:   "",
			topoNode:  "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.7.0",
			wantImage: "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.7.0",
		},
		{
			name:      "fallback used when neither cli-flag nor topology set",
			cliFlag:   "",
			topoNode:  "",
			wantImage: fallback,
		},
	}

	// Load the endpoint compose template once; all sub-tests share it.
	content, err := templateFS.ReadFile("templates/docker-compose.endpoint.yml.tmpl")
	if err != nil {
		t.Fatalf("read endpoint template: %v", err)
	}
	tmpl, err := template.New("docker-compose.endpoint.yml.tmpl").Parse(string(content))
	if err != nil {
		t.Fatalf("parse endpoint template: %v", err)
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			image := resolveImage(tc.cliFlag, tc.topoNode, fallback)
			if image != tc.wantImage {
				t.Fatalf("resolveImage(%q, %q, fallback) = %q, want %q",
					tc.cliFlag, tc.topoNode, image, tc.wantImage)
			}

			data := struct {
				Name       string
				Host       string
				OverlayIP  string
				Image      string
				ListenPort int
				TokenHash  string
			}{
				Name:       "ep-01",
				Host:       "192.0.2.20",
				OverlayIP:  "10.0.0.2",
				Image:      image,
				ListenPort: 51820,
				TokenHash:  "$$2a$$12$$testonly",
			}

			var buf strings.Builder
			if err := tmpl.Execute(&buf, data); err != nil {
				t.Fatalf("execute template: %v", err)
			}
			rendered := buf.String()

			wantLine := "image: " + tc.wantImage
			if !strings.Contains(rendered, wantLine) {
				t.Errorf("rendered compose does not contain %q\n---\n%s", wantLine, rendered)
			}
		})
	}
}

// TestEndpointPrepareImageFlagValidation verifies that newEndpointPrepareCommand
// rejects an invalid --image value before performing any topology or CA work.
func TestEndpointPrepareImageFlagValidation(t *testing.T) {
	// Do not run in parallel: NewRootCommand binds cobra persistent flags to
	// package-level globals (topologyPath/configDir), and concurrent flag
	// registration causes a data race under -race.

	invalidRefs := []string{
		"img; rm -rf /",
		"img`touch /pwned`",
		"img$(id)",
		"img|sh",
	}

	for _, ref := range invalidRefs {
		ref := ref
		t.Run(ref, func(t *testing.T) {
			root := NewRootCommand("test")
			root.SilenceUsage = true
			root.SilenceErrors = true
			root.SetArgs([]string{"endpoint", "prepare", "--image", ref, "ep-01"})

			err := root.Execute()
			if err == nil {
				t.Errorf("endpoint prepare --image %q: expected error for invalid image ref, got nil", ref)
				return
			}
			if !strings.Contains(err.Error(), "invalid --image") {
				t.Errorf("endpoint prepare --image %q: expected 'invalid --image' in error, got: %v", ref, err)
			}
		})
	}
}

func TestReadEndpointPublicKeyFormats(t *testing.T) {
	t.Parallel()

	testKey32 := [32]byte{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
		0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
		0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
	}
	testKeyHex := hex.EncodeToString(testKey32[:])
	testKeyB64 := base64.StdEncoding.EncodeToString(testKey32[:])

	cases := []struct {
		name    string
		input   []byte
		wantErr bool
	}{
		{name: "32b raw", input: testKey32[:], wantErr: false},
		{name: "64hex", input: []byte(testKeyHex), wantErr: false},
		{name: "64hex+LF", input: []byte(testKeyHex + "\n"), wantErr: false},
		{name: "64hex+CRLF", input: []byte(testKeyHex + "\r\n"), wantErr: false},
		{name: "base64 44c", input: []byte(testKeyB64), wantErr: true},
		{name: "empty", input: []byte{}, wantErr: true},
		{name: "65b raw", input: bytesOfLen(65), wantErr: true},
		{name: "non-hex64", input: []byte(strings.Repeat("z", 64)), wantErr: true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tmpDir := t.TempDir()
			pubkeyPath := filepath.Join(tmpDir, "pubkey")
			if err := os.WriteFile(pubkeyPath, tc.input, 0o600); err != nil {
				t.Fatalf("write pubkey: %v", err)
			}

			got, err := readEndpointPublicKey(pubkeyPath)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("readEndpointPublicKey = %v, want nil", err)
			}
			if hex.EncodeToString(got) != testKeyHex {
				t.Fatalf("hex.EncodeToString(got) = %q, want %q", hex.EncodeToString(got), testKeyHex)
			}
		})
	}
}
