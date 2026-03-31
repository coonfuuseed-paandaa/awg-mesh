package dns

import (
	"context"
	"net"
	"testing"
	"time"

	mdns "github.com/miekg/dns"
)

func startTestServer(t *testing.T, records []Record) (string, func()) {
	t.Helper()

	// Find a free port
	ln, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	addr := ln.LocalAddr().String()
	ln.Close()

	srv := NewServer("mesh.zone", addr, "1.1.1.1:53", records)
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		srv.Start(ctx)
	}()

	// Wait for server to be ready
	time.Sleep(100 * time.Millisecond)

	return addr, cancel
}

func TestDNSServerARecord(t *testing.T) {
	records := BuildZoneRecords("mesh.zone", map[string]string{
		"node-asia-01": "172.20.70.34",
	})

	addr, cancel := startTestServer(t, records)
	defer cancel()

	client := &mdns.Client{Net: "udp"}
	msg := new(mdns.Msg)
	msg.SetQuestion("node-asia-01.mesh.zone.", mdns.TypeA)

	resp, _, err := client.Exchange(msg, addr)
	if err != nil {
		t.Fatalf("DNS query failed: %v", err)
	}

	if len(resp.Answer) != 1 {
		t.Fatalf("expected 1 answer, got %d", len(resp.Answer))
	}

	a, ok := resp.Answer[0].(*mdns.A)
	if !ok {
		t.Fatal("expected A record")
	}
	if a.A.String() != "172.20.70.34" {
		t.Errorf("expected 172.20.70.34, got %s", a.A.String())
	}
}

func TestDNSServerPTR(t *testing.T) {
	records := BuildZoneRecords("mesh.zone", map[string]string{
		"node-asia-01": "172.20.70.34",
	})

	addr, cancel := startTestServer(t, records)
	defer cancel()

	client := &mdns.Client{Net: "udp"}
	msg := new(mdns.Msg)
	msg.SetQuestion("34.70.20.172.in-addr.arpa.", mdns.TypePTR)

	resp, _, err := client.Exchange(msg, addr)
	if err != nil {
		t.Fatalf("DNS query failed: %v", err)
	}

	if len(resp.Answer) != 1 {
		t.Fatalf("expected 1 answer, got %d", len(resp.Answer))
	}

	ptr, ok := resp.Answer[0].(*mdns.PTR)
	if !ok {
		t.Fatal("expected PTR record")
	}
	if ptr.Ptr != "node-asia-01.mesh.zone." {
		t.Errorf("expected node-asia-01.mesh.zone., got %s", ptr.Ptr)
	}
}

func TestDNSServerUpdateRecords(t *testing.T) {
	records := BuildZoneRecords("mesh.zone", map[string]string{
		"node-asia-01": "172.20.70.34",
	})

	srv := NewServer("mesh.zone", "127.0.0.1:0", "1.1.1.1:53", records)

	// Find a free port
	ln, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	addr := ln.LocalAddr().String()
	ln.Close()

	srv.listen = addr
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go srv.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	// Update records
	newRecords := BuildZoneRecords("mesh.zone", map[string]string{
		"node-asia-01": "172.20.70.34",
		"node-us-01": "172.20.70.38",
	})
	srv.UpdateRecords(newRecords)

	client := &mdns.Client{Net: "udp"}
	msg := new(mdns.Msg)
	msg.SetQuestion("node-us-01.mesh.zone.", mdns.TypeA)

	resp, _, err := client.Exchange(msg, addr)
	if err != nil {
		t.Fatalf("DNS query failed: %v", err)
	}

	if len(resp.Answer) != 1 {
		t.Fatalf("expected 1 answer after update, got %d", len(resp.Answer))
	}

	a, ok := resp.Answer[0].(*mdns.A)
	if !ok {
		t.Fatal("expected A record")
	}
	if a.A.String() != "172.20.70.38" {
		t.Errorf("expected 172.20.70.38, got %s", a.A.String())
	}
}

func TestDNSServerNonexistentRecord(t *testing.T) {
	records := BuildZoneRecords("mesh.zone", map[string]string{
		"node-asia-01": "172.20.70.34",
	})

	addr, cancel := startTestServer(t, records)
	defer cancel()

	client := &mdns.Client{Net: "udp"}
	msg := new(mdns.Msg)
	msg.SetQuestion("nonexistent.mesh.zone.", mdns.TypeA)

	resp, _, err := client.Exchange(msg, addr)
	if err != nil {
		t.Fatalf("DNS query failed: %v", err)
	}

	if len(resp.Answer) != 0 {
		t.Errorf("expected 0 answers for nonexistent record, got %d", len(resp.Answer))
	}
}
