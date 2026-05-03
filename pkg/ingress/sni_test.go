package ingress

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestClientHelloServerNameExtractsSNI(t *testing.T) {
	t.Parallel()

	record := captureClientHello(t, "Media.Example.Com")
	hostname, err := ClientHelloServerName(record)
	if err != nil {
		t.Fatalf("ClientHelloServerName: %v", err)
	}
	if hostname != "media.example.com" {
		t.Fatalf("hostname = %q, want media.example.com", hostname)
	}
}

func TestClassifierRejectsUnknownSNI(t *testing.T) {
	t.Parallel()

	registry, err := NewRegistry([]Route{{Hostname: "media.example.com", Target: "172.21.92.10:8096"}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	classifier := NewClassifier(registry)
	_, err = classifier.ClassifyClientHello(captureClientHello(t, "unknown.example.com"))
	if err == nil || !stringsContains(err.Error(), "not allowed") {
		t.Fatalf("expected unknown hostname rejection, got %v", err)
	}
}

func TestClientHelloServerNameRejectsMissingSNI(t *testing.T) {
	t.Parallel()

	_, err := ClientHelloServerName([]byte{22, 3, 1, 0, 1, 1})
	if !errors.Is(err, ErrClientHelloTooShort) {
		t.Fatalf("error = %v, want ErrClientHelloTooShort", err)
	}
}

func captureClientHello(t *testing.T, serverName string) []byte {
	t.Helper()

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	errCh := make(chan error, 1)
	go func() {
		tlsConn := tls.Client(clientConn, &tls.Config{
			ServerName:         serverName,
			InsecureSkipVerify: true, //nolint:gosec // test captures ClientHello only.
			MinVersion:         tls.VersionTLS12,
		})
		errCh <- tlsConn.Handshake()
	}()

	if err := serverConn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	header := make([]byte, 5)
	if _, err := io.ReadFull(serverConn, header); err != nil {
		t.Fatalf("read TLS record header: %v", err)
	}
	length := int(header[3])<<8 | int(header[4])
	body := make([]byte, length)
	if _, err := io.ReadFull(serverConn, body); err != nil {
		t.Fatalf("read TLS record body: %v", err)
	}
	_ = serverConn.Close()
	select {
	case <-errCh:
	case <-time.After(time.Second):
		t.Fatal("client handshake did not return after server close")
	}
	return append(header, body...)
}

func stringsContains(value, part string) bool {
	return len(part) == 0 || (len(value) >= len(part) && contains(value, part))
}

func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
