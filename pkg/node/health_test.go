package node

import (
	"bytes"
	"context"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// syncBuffer is a bytes.Buffer protected by a mutex for safe concurrent use.
// zerolog writes log lines from the demuxLoop goroutine; the test reads them.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// newTestChecker creates a HealthChecker with a nop logger for test use.
func newTestChecker(t *testing.T) *HealthChecker {
	t.Helper()
	return NewHealthChecker(HealthConfig{
		Interval:         2 * time.Second,
		Timeout:          500 * time.Millisecond,
		FailureThreshold: 5,
	}, zerolog.Nop(), nil)
}

// startTestChecker opens the shared ICMP socket and registers a cleanup that
// calls Close. Skips the test if CAP_NET_RAW is not available.
func startTestChecker(t *testing.T, hc *HealthChecker) {
	t.Helper()
	if err := hc.Start(); err != nil {
		t.Skipf("requires CAP_NET_RAW: %v", err)
	}
	t.Cleanup(func() {
		_ = hc.Close()
	})
}

func TestNewHealthChecker(t *testing.T) {
	t.Parallel()

	cfg := HealthConfig{
		Interval:         2 * time.Second,
		Timeout:          500 * time.Millisecond,
		FailureThreshold: 5,
	}

	checker := NewHealthChecker(cfg, zerolog.Nop(), nil)
	if checker == nil { //nolint:staticcheck // SA5011: t.Fatal exits — cfg access below safe
		t.Fatal("expected checker instance")
	}
	if checker.cfg != cfg {
		t.Fatalf("unexpected checker config: got %#v want %#v", checker.cfg, cfg)
	}
}

func TestPingICMPInvalidIP(t *testing.T) {
	t.Parallel()

	hc := newTestChecker(t)
	startTestChecker(t, hc)

	_, err := hc.PingICMP(context.Background(), "not-an-ip", time.Second)
	if err == nil {
		t.Fatal("expected error for invalid IP")
	}
}

func TestPingICMPLocalhost(t *testing.T) {
	t.Parallel()

	hc := newTestChecker(t)
	startTestChecker(t, hc)

	alive, err := hc.PingICMP(context.Background(), "127.0.0.1", 2*time.Second)
	if err != nil {
		t.Fatalf("unexpected ping error: %v", err)
	}
	if !alive {
		t.Fatal("expected localhost ping to succeed")
	}
}

func TestPingICMPUnreachable(t *testing.T) {
	t.Parallel()

	hc := newTestChecker(t)
	startTestChecker(t, hc)

	// 192.0.2.1 is TEST-NET-1 (RFC 5737) — guaranteed non-routable.
	alive, err := hc.PingICMP(context.Background(), "192.0.2.1", 500*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected structural error: %v", err)
	}
	if alive {
		t.Fatal("expected unreachable host to return false")
	}
}

func TestPingICMPContextCancelled(t *testing.T) {
	t.Parallel()

	hc := newTestChecker(t)
	startTestChecker(t, hc)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	alive, _ := hc.PingICMP(ctx, "127.0.0.1", 5*time.Second)
	if alive {
		// May or may not succeed depending on timing — not asserting false.
		// Just verify it does not hang.
		t.Log("ping succeeded despite cancelled context (fast local response)")
	}
}

func TestPurgeStaleFailures(t *testing.T) {
	t.Parallel()

	failures := map[string]int{
		"tunnel-a": 5,
		"tunnel-b": 2,
		"tunnel-c": 10,
	}

	currentTargets := []HealthTarget{
		{Name: "tunnel-a", PingAddr: "10.0.0.1"},
		{Name: "tunnel-d", PingAddr: "10.0.0.4"},
	}

	purgeStaleFailures(currentTargets, failures)

	if _, ok := failures["tunnel-b"]; ok {
		t.Fatal("tunnel-b should have been purged")
	}
	if _, ok := failures["tunnel-c"]; ok {
		t.Fatal("tunnel-c should have been purged")
	}
	if failures["tunnel-a"] != 5 {
		t.Fatalf("tunnel-a should be preserved with count 5, got %d", failures["tunnel-a"])
	}
	if _, ok := failures["tunnel-d"]; ok {
		t.Fatal("tunnel-d should not be in failures map (never failed)")
	}
}

func TestPingAllParallelCompletesInBoundedTime(t *testing.T) {
	t.Parallel()

	hc := NewHealthChecker(HealthConfig{
		Timeout: 500 * time.Millisecond,
	}, zerolog.Nop(), nil)
	startTestChecker(t, hc)

	// 10 unreachable targets — if sequential, would take 5s; parallel should be ~500ms.
	targets := make([]HealthTarget, 10)
	for i := range targets {
		targets[i] = HealthTarget{
			Name:     "target-" + string(rune('a'+i)),
			PingAddr: "192.0.2." + string(rune('1'+i)), // TEST-NET-1
		}
	}

	start := time.Now()
	results := hc.pingAllParallel(context.Background(), targets)
	elapsed := time.Since(start)

	// All should be false (unreachable) or error (no CAP_NET_RAW).
	for i, r := range results {
		if r {
			t.Logf("target %d unexpectedly alive (skipping timing check)", i)
			return
		}
	}

	// Parallel: should complete in ~1× timeout, not 10× timeout.
	maxExpected := 3 * time.Second // generous bound (500ms × 1 + overhead)
	if elapsed > maxExpected {
		t.Fatalf("parallel ping took %v, expected < %v (sequential would be ~5s)", elapsed, maxExpected)
	}
	t.Logf("10 parallel pings completed in %v", elapsed)
}

// TestPingICMPConcurrentDemux verifies that N concurrent PingICMP calls on a shared
// socket do not starve each other. Each goroutine pings a distinct localhost alias;
// all must return (true, nil) across 100 iterations (FR-5.1, closes #20).
func TestPingICMPConcurrentDemux(t *testing.T) {
	// 127.0.0.0/8 is entirely loopback on Linux (each /32 in the range is a
	// valid destination). Windows and most other OSes only bind 127.0.0.1 —
	// pinging 127.0.0.2..127.0.0.8 returns "destination unreachable" there.
	// The test gate keeps the concurrent-demux verification on Linux CI only.
	if runtime.GOOS != "linux" {
		t.Skipf("TestPingICMPConcurrentDemux requires Linux 127.0.0.0/8 loopback (runtime.GOOS=%s)", runtime.GOOS)
	}

	const (
		goroutines  = 8
		iterations  = 100
		pingTimeout = 500 * time.Millisecond
	)

	hc := newTestChecker(t)
	if err := hc.Start(); err != nil {
		t.Skipf("requires CAP_NET_RAW: %v", err)
	}
	t.Cleanup(func() { _ = hc.Close() })

	// 127.0.0.1 through 127.0.0.8 are all loopback addresses on Linux.
	targets := make([]string, goroutines)
	for i := range targets {
		targets[i] = "127.0.0." + itoa(i+1)
	}

	for iter := 0; iter < iterations; iter++ {
		var (
			wg       sync.WaitGroup
			mu       sync.Mutex
			failures []string
		)

		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func(addr string) {
				defer wg.Done()
				alive, err := hc.PingICMP(context.Background(), addr, pingTimeout)
				if err != nil || !alive {
					mu.Lock()
					failures = append(failures, addr)
					mu.Unlock()
				}
			}(targets[g])
		}
		wg.Wait()

		if len(failures) > 0 {
			t.Fatalf("iteration %d: %d/%d pings failed (starvation?): %v",
				iter, len(failures), goroutines, failures)
		}
	}

	t.Logf("TestPingICMPConcurrentDemux: %d goroutines × %d iterations — all passed",
		goroutines, iterations)
}

// TestPingICMPBoundedReadLoop verifies that PingICMP returns within a bounded time
// even when the shared socket receives a flood of unrelated ICMP replies (FR-5.2,
// closes #25). The demux goroutine drops non-matching packets; PingICMP returns via
// the timeout arm, not by iterating over unrelated packets.
func TestPingICMPBoundedReadLoop(t *testing.T) {
	// Raw ICMP socket fan-out is a Linux-specific behavior: the kernel delivers
	// every ICMP packet to every raw socket bound to ICMP protocol. On Windows
	// (and other OSes) per-socket filtering prevents the "sibling flood" from
	// reaching the HealthChecker's socket — so the demux-drop path never fires.
	// The bounded-loop guarantee is still covered by TestPingICMPBoundedReadLoop
	// running on Linux CI (CAP_NET_RAW available).
	if runtime.GOOS != "linux" {
		t.Skipf("TestPingICMPBoundedReadLoop requires Linux raw ICMP fan-out semantics (runtime.GOOS=%s)", runtime.GOOS)
	}

	const (
		pingTimeout  = 200 * time.Millisecond
		maxWallClock = 350 * time.Millisecond
	)

	// Use a concurrent-safe log-capturing writer so we can assert event=icmp_demux_drop.
	// bytes.Buffer is not goroutine-safe; demuxLoop writes from its own goroutine.
	var logBuf syncBuffer
	logger := zerolog.New(&logBuf).Level(zerolog.DebugLevel)

	hc := NewHealthChecker(HealthConfig{
		Timeout: pingTimeout,
	}, logger, nil)
	if err := hc.Start(); err != nil {
		t.Skipf("requires CAP_NET_RAW: %v", err)
	}
	t.Cleanup(func() { _ = hc.Close() })

	// Flood localhost with ICMP echo-reply packets using a different ICMP id
	// (siblingID = hc.id + 1) so demuxLoop classifies them all as drops.
	siblingConn, err := icmp.ListenPacket("ip4:icmp", "")
	if err != nil {
		t.Skipf("requires CAP_NET_RAW for sibling socket: %v", err)
	}
	defer func() { _ = siblingConn.Close() }()

	// Sibling floods with its own id — guaranteed != hc.id unless wraparound
	// collision; we add 1 mod 0x10000 to stay distinct.
	siblingID := int((hc.id + 1) & 0xffff)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start the flood goroutine.
	go func() {
		dst := &net.IPAddr{IP: net.ParseIP("127.0.0.1")}
		seq := 0
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			noiseMsg := icmp.Message{
				Type: ipv4.ICMPTypeEchoReply,
				Code: 0,
				Body: &icmp.Echo{
					ID:   siblingID,
					Seq:  seq & 0xffff,
					Data: []byte("noise"),
				},
			}
			b, marshalErr := noiseMsg.Marshal(nil)
			if marshalErr != nil {
				return
			}
			// Errors on write are ignored — the conn may be closed on cleanup.
			_, _ = siblingConn.WriteTo(b, dst)
			seq++
			time.Sleep(time.Millisecond) // ~1000 noise packets/second
		}
	}()

	// Give the flood goroutine a head start.
	time.Sleep(10 * time.Millisecond)

	// PingICMP to RFC 5737 TEST-NET-1 (192.0.2.1) — guaranteed non-routable,
	// WriteTo succeeds (raw socket; kernel does not reject the send), but no reply
	// ever comes back. The call must return (false, nil) via the timeout arm within
	// maxWallClock, not hang spinning on noise packets from the sibling socket.
	start := time.Now()
	alive, err := hc.PingICMP(context.Background(), "192.0.2.1", pingTimeout)
	elapsed := time.Since(start)

	cancel() // stop flood

	if err != nil {
		t.Fatalf("unexpected structural error: %v", err)
	}
	if alive {
		t.Fatal("expected unreachable host to return false")
	}
	if elapsed > maxWallClock {
		t.Fatalf("PingICMP took %v, expected ≤ %v (bounded by timeout %v)",
			elapsed, maxWallClock, pingTimeout)
	}

	// Wait briefly for demuxLoop to log any pending drops (it logs per-packet).
	// Close flushes the goroutine via readerDone but we need log output before that.
	time.Sleep(20 * time.Millisecond)

	// Assert the demux drop event was logged at least once (NFR-3.1, T031 AC).
	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "icmp_demux_drop") {
		t.Errorf("expected at least one event=icmp_demux_drop log line; got: %s", logOutput)
	}

	t.Logf("TestPingICMPBoundedReadLoop: returned in %v (timeout=%v)", elapsed, pingTimeout)
}

// BenchmarkPingICMP measures per-ping latency on the shared-socket demux path.
// Target: p50 < 1ms, p99 < 5ms (NFR-2.1).
//
// Run with: go test -bench=BenchmarkPingICMP -benchtime=1s -run=^$ ./pkg/node/
func BenchmarkPingICMP(b *testing.B) {
	hc := NewHealthChecker(HealthConfig{
		Timeout: 100 * time.Millisecond,
	}, zerolog.Nop(), nil)

	if err := hc.Start(); err != nil {
		b.Skipf("requires CAP_NET_RAW: %v", err)
	}
	defer func() { _ = hc.Close() }()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		alive, err := hc.PingICMP(context.Background(), "127.0.0.1", 100*time.Millisecond)
		if err != nil {
			b.Fatalf("unexpected error: %v", err)
		}
		if !alive {
			b.Fatal("expected localhost to be alive")
		}
	}
}

// itoa converts a small non-negative int to a decimal string without importing strconv.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}
