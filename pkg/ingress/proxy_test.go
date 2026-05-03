package ingress

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rs/zerolog"
)

func TestHTTPProxyForwardsToOverlayTargetAndPreservesHeaders(t *testing.T) {
	t.Parallel()

	received := make(chan *http.Request, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Clone(r.Context())
		_, _ = w.Write([]byte("from-target"))
	}))
	defer target.Close()

	registry, err := NewRegistry([]Route{{
		Tenant:   "tenant-a",
		Hostname: "media.example.com",
		Target:   strings.TrimPrefix(target.URL, "http://"),
		Protocol: ProtocolHTTP,
	}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://media.example.com/library", nil)
	req.Host = "media.example.com"
	req.RemoteAddr = "198.51.100.7:53100"

	NewHTTPProxy(registry, nil, nil, zerolog.Nop()).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "from-target" {
		t.Fatalf("response = code %d body %q", rec.Code, rec.Body.String())
	}
	select {
	case got := <-received:
		if got.Host != "media.example.com" {
			t.Fatalf("target Host = %q", got.Host)
		}
		if got.Header.Get("X-Forwarded-Host") != "media.example.com" {
			t.Fatalf("X-Forwarded-Host = %q", got.Header.Get("X-Forwarded-Host"))
		}
		if got.Header.Get("X-Forwarded-Proto") != "http" {
			t.Fatalf("X-Forwarded-Proto = %q", got.Header.Get("X-Forwarded-Proto"))
		}
		if got.Header.Get("X-Forwarded-For") != "198.51.100.7" {
			t.Fatalf("X-Forwarded-For = %q", got.Header.Get("X-Forwarded-For"))
		}
	case <-time.After(time.Second):
		t.Fatal("target did not receive proxied request")
	}
}

func TestHTTPProxyFailsClosedWhenTargetUnhealthy(t *testing.T) {
	t.Parallel()

	route := Route{Tenant: "tenant-a", Hostname: "media.example.com", Target: "172.21.92.10:8096", Protocol: ProtocolHTTP}
	registry, err := NewRegistry([]Route{route})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	health := NewHealthTracker(func(context.Context, Route) error { return nil })
	health.Set(route, false, time.Now(), io.ErrClosedPipe)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://media.example.com/", nil)
	req.Host = "media.example.com"
	NewHTTPProxy(registry, health, nil, zerolog.Nop()).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502", rec.Code)
	}
}

func TestWebSocketProxyPreservesBidirectionalMessages(t *testing.T) {
	t.Parallel()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
		typ, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		_ = conn.Write(r.Context(), typ, append([]byte("echo:"), data...))
	}))
	defer target.Close()

	registry, err := NewRegistry([]Route{{
		Tenant:   "tenant-a",
		Hostname: "ws.example.com",
		Target:   strings.TrimPrefix(target.URL, "http://"),
		Protocol: ProtocolWebSocket,
	}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	proxy := httptest.NewServer(NewWebSocketProxy(registry, nil, nil, zerolog.Nop()))
	defer proxy.Close()

	conn, _, err := websocket.Dial(context.Background(), "ws://"+strings.TrimPrefix(proxy.URL, "http://")+"/socket", &websocket.DialOptions{Host: "ws.example.com"})
	if err != nil {
		t.Fatalf("Dial proxy: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	if err := conn.Write(context.Background(), websocket.MessageText, []byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	typ, data, err := conn.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if typ != websocket.MessageText || string(data) != "echo:hello" {
		t.Fatalf("message = type %v data %q", typ, data)
	}
}

func TestUDPForwarderMapsFlowsAndRejectsUnknownHost(t *testing.T) {
	t.Parallel()

	sender := &fakeUDPSender{}
	route := Route{Tenant: "tenant-a", Hostname: "dns.example.com", Target: "172.21.92.53:53", Protocol: ProtocolUDP}
	registry, err := NewRegistry([]Route{route})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	table := NewUDPFlowTable(time.Second)
	forwarder := NewUDPForwarder(registry, nil, table, sender, nil)
	client := &net.UDPAddr{IP: net.ParseIP("198.51.100.20"), Port: 53000}
	flow, err := forwarder.Forward(context.Background(), client, "dns.example.com", []byte("query"), time.Unix(10, 0))
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if flow.Client != client.String() || flow.Target != "172.21.92.53:53" || table.Count(route) != 1 {
		t.Fatalf("unexpected flow/table state: flow=%+v count=%d", flow, table.Count(route))
	}
	if len(sender.calls) != 1 || sender.calls[0].target != "172.21.92.53:53" || string(sender.calls[0].payload) != "query" {
		t.Fatalf("sender calls = %#v", sender.calls)
	}
	if _, err := forwarder.Forward(context.Background(), client, "unknown.example.com", []byte("query"), time.Unix(11, 0)); err == nil {
		t.Fatal("expected unknown host rejection")
	}
	if removed := table.Expire(time.Unix(12, 1)); removed != 1 {
		t.Fatalf("Expire removed %d flows, want 1", removed)
	}
}

type fakeUDPSender struct {
	calls []struct {
		target  string
		payload []byte
	}
}

func (s *fakeUDPSender) Send(_ context.Context, target string, payload []byte) error {
	s.calls = append(s.calls, struct {
		target  string
		payload []byte
	}{target: target, payload: append([]byte(nil), payload...)})
	return nil
}
