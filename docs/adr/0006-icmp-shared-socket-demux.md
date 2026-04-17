# ADR-0006: Shared-Socket ICMP Demux for HealthChecker

## Status

Accepted (v1.8.0)

## Context

Before v1.8.0, `pkg/node/health.go::PingICMP` opened a fresh raw ICMP socket (`icmp.ListenPacket("ip4:icmp", "")`) on every call. `pingAllParallel` spawned N goroutines per tick (one per tunnel), each holding its own socket and its own read loop filtering by `(pid, seq)`. This design is a cargo-culted common-example pattern and has a structural correctness flaw on Linux.

On Linux, raw ICMP sockets do NOT receive only replies to echoes sent from that socket. The kernel delivers every ICMP packet destined for the host to every raw socket bound to ICMP protocol. With N concurrent goroutines × N sockets, every socket sees:

- its own expected echo reply,
- every other goroutine's echo reply,
- destination-unreachable messages from unrelated flows,
- TTL-exceeded messages,
- ICMP echoes originated by other processes.

The per-goroutine loop discards non-matching packets but consumes them — meaning goroutine A's socket can starve goroutine B's socket under high packet rates. Worse, the loop has no early-exit on `ctx.Done()` and no upper bound on non-matching packets it will drain before the deadline fires. Under ICMP noise a single `PingICMP` call can spin through hundreds of discarded packets before timing out.

This is GitHub issue **#20 (C2 CRITICAL — ICMP raw socket demux race)** and **#25 (M5 MEDIUM — unbounded reply loop in PingICMP)**. Both root-cause the same architecture.

## Decision Drivers

- Fix #20 at its root (kernel behavior), not symptomatically.
- Fix #25 in the same change since it shares the same function.
- Preserve existing `HealthChecker.PingICMP` exported signature to avoid breaking `pkg/node/master.go`, `pkg/node/client_linux.go`, `pingAllParallel`, and test callers.
- Keep binary size and dependency surface minimal — resist adding a full third-party ping library.
- Test on Linux CI where `CAP_NET_RAW` is available; skip cleanly on Windows.

## Decision

Refactor `HealthChecker` to own one shared raw ICMP socket for its lifetime, with a demux map routing replies to per-request channels keyed on ICMP `seq`.

New structure:

```go
type HealthChecker struct {
    // ... existing fields ...
    socket     *icmp.PacketConn        // shared raw ICMP socket
    id         uint16                  // os.Getpid() & 0xffff
    demux      map[uint16]chan icmpReply
    socketMu   sync.RWMutex            // guards socket field
    demuxMu    sync.Mutex              // guards demux map
    readerDone chan struct{}           // closed when demuxLoop exits
    startOnce  sync.Once               // Start idempotency
    closeOnce  sync.Once               // Close idempotency
}
```

Lifecycle:

- `Start() error` — idempotent (`startOnce.Do`). Opens the shared socket once, launches one reader goroutine (`demuxLoop`) for the lifetime of the checker. Logs `event=icmp_socket_open`.
- `Close() error` — idempotent (`closeOnce.Do`). Takes `socketMu.Lock()`, reads socket into local, nils the field, releases lock, then calls `Close()` on the local socket. Waits on `readerDone`. Logs `event=icmp_demux_exit count=N`.
- `demuxLoop()` — single goroutine. Captures the socket under `socketMu.RLock()` at startup. Reads in a loop via `ReadFrom`. Parses ICMP; for `EchoReply` with matching `id`, looks up `demux[seq]` under `demuxMu`, sends reply on the channel non-blocking (`select default` on full-channel drop). Unrelated packets increment a drop counter and emit `event=icmp_demux_drop` with `id`, `seq`. Exits on any `ReadFrom` error (including socket-closed).

Per-call:

```go
func (h *HealthChecker) PingICMP(ctx, addr, timeout) (bool, error) {
    h.socketMu.RLock()
    sock := h.socket
    h.socketMu.RUnlock()
    if sock == nil { return false, errNotStarted }

    seq := seqCounter.next()
    reply := make(chan icmpReply, 1)
    h.demuxMu.Lock(); h.demux[seq] = reply; h.demuxMu.Unlock()
    defer func() { h.demuxMu.Lock(); delete(h.demux, seq); h.demuxMu.Unlock() }()

    // WriteTo(echo with id=h.id, seq=seq) — see full code
    effectiveTimeout := min(ctx.Deadline()-now, timeout)
    select {
      case <-reply:                  return true, nil
      case <-ctx.Done():             return false, nil
      case <-time.After(effectiveTimeout): return false, nil
    }
}
```

Key invariants enforced in code:

- `h.socket` is only mutated under `socketMu.Lock()`; readers take `RLock`.
- `h.demux[seq]` is only read/written under `demuxMu.Lock()`.
- The reply channel is buffered(1) and NEVER explicitly closed in `PingICMP` — the defer only deletes from the map. This prevents a send-on-closed-channel panic if `demuxLoop` acquires a reference to the channel under the lock, then `PingICMP` races to close it on timeout.
- `seqCounter` uses `sync/atomic` (not `sync.Mutex`) — hot-path perf matters when N tunnels tick every 10 seconds.

## Alternatives Considered

1. **Keep per-call socket, add `ctx.Done()` + max-iteration guard.** Symptomatically addresses #25 (bounded loop) but does NOT fix #20 (cross-socket starvation). Rejected — would leave the race in place under high tunnel count.

2. **Import `github.com/prometheus-community/pro-bing`.** Full-featured ping library with latency histograms, TCP/UDP ping, and rich statistics. Solves the demux problem internally. Rejected — we need ~100 lines of code and would pay 3× binary-size cost for features we do not use (and security surface area we do not want in a tunnel node). A future need for full ping statistics is a valid reason to revisit; for v1.8.0 it is overkill.

3. **Per-client `HealthChecker` instances rather than one global.** Does not help — every raw socket in the same process still sees every ICMP packet. The kernel behavior is process-wide, not per-socket.

4. **Non-raw sockets (UDP ICMP on Linux via `SOCK_DGRAM`).** Kernel-side demux, no `CAP_NET_RAW` required. Rejected — requires `/proc/sys/net/ipv4/ping_group_range` configuration on the host, a deployment complexity we do not want to impose on operators.

## Consequences

**Positive:**

- Correctness under concurrent load — N `PingICMP` calls against N targets run without cross-goroutine starvation on Linux.
- CPU reduction under high ICMP noise — unrelated packets are dropped in the single reader goroutine instead of being discarded N times across N per-goroutine loops.
- Fewer file descriptors — one raw socket per `HealthChecker` instead of one per in-flight ping.
- Clean `ctx.Done()` semantics — the select arm in `PingICMP` guarantees cancellation within one tick.
- Bounded iterations — `demuxLoop` is the only read site; its cap is the single reader's rate, not per-goroutine iteration count.

**Negative:**

- `HealthChecker.Run()` now calls `Start()` at entry and `defer Close()` at top of body. Existing callers that used the zero-value checker without calling `Run()` would panic if they called `PingICMP` directly. Mitigation: the nil-socket guard returns a clear error instead of panicking; tests exercise this path via `newTestChecker(t).Start()`.

- Test coverage is asymmetric — `TestPingICMPConcurrentDemux` and `TestPingICMPBoundedReadLoop` gate on `runtime.GOOS == "linux"` and `CAP_NET_RAW`. Windows dev hosts skip these; coverage lives in Linux CI only. Documented as expected.

- Lock contention under extremely high concurrent ping count is theoretically possible. NFR-2.1 benchmark `BenchmarkPingICMP` guards against p50 regression > 1 ms; measured below threshold on local dev host.

**Neutral:**

- `icmpSeqCounter` changed from `sync.Mutex + int` to `atomic.AddUint32` — behavior unchanged, perf improvement on the hot path.

- `HealthChecker` now owns more lifecycle state (`startOnce`, `closeOnce`, `readerDone`, socket). Double-Start and double-Close are safe (idempotent); this pattern matches the stdlib `net.Listener` convention.

## Evidence

- **Root cause citation:** `explorer` agent mapping (`.agent/reports/context-brief-internal-review-fixes.md`), which verified via direct `pkg/node/health.go` read that:
  - Each call opens its own socket at line 187.
  - `pid` is process-wide at line 201.
  - `seqCounter` is process-wide at line 251.
  - No context-cancellation arm existed in the pre-fix loop at lines 224-247.
- **Linux kernel behavior:** raw ICMP sockets receiving every ICMP packet is documented in `man 7 raw` and is not a Go-specific quirk.
- **Go stdlib guarantees:** `sync.Once.Do` happens-before subsequent reads (per Go memory model), which is why `Start()`'s write of `h.socket` is visible to `demuxLoop`'s capture under `socketMu.RLock()`.

## References

- GitHub issue #20 — ICMP raw socket demux race
- GitHub issue #25 — unbounded reply loop in PingICMP
- Spec: `.agent/specs/internal-review-fixes/spec.md` FR-1, FR-1.6, FR-5
- Pull request #40 — implementation + reviewer fixes (closeOnce, nil-guard, atomic seqCounter, socketMu)
- Predecessor ADR: `docs/adr/0005-transport-state-schema-versioning.md` (v1.7.0, unrelated subsystem but same release-discipline pattern)
