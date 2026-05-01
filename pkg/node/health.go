package node

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
	"github.com/rs/zerolog"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

const (
	defaultHealthInterval         = 10 * time.Second
	defaultHealthTimeout          = 3 * time.Second
	defaultHealthFailureThreshold = 3
	icmpProtocol                  = 1 // IANA ICMP protocol number
)

// HealthConfig holds configuration for the health checker.
type HealthConfig struct {
	Interval         time.Duration
	Timeout          time.Duration
	FailureThreshold int
}

// HandshakeChecker is a callback that returns the time of the last WG handshake
// for a peer identified by its public key. If ICMP ping fails but the handshake
// is recent (within 2× health interval), the tunnel is considered alive. This
// prevents false positives when ICMP is blocked but UDP tunnel traffic flows.
// Pass nil to disable handshake-based fallback.
type HandshakeChecker func(peerKey wg.Key) time.Time

// icmpReply is the value delivered by the demux goroutine to a waiting PingICMP call.
// The struct carries no payload because the demux map key (seq) already identifies
// which ping the reply belongs to; the presence of the value on the channel is the
// signal.
type icmpReply struct{}

// HealthChecker monitors tunnel liveness and fires callbacks on state transitions.
//
// Shared-socket design (FR-1.6): a single raw ICMP socket is opened once per
// HealthChecker lifetime by Start() and closed by Close(). All concurrent PingICMP
// calls write their echo on this socket and register a per-seq reply channel in the
// demux map. A single reader goroutine (demuxLoop) owns ReadFrom, parses each reply,
// and dispatches it to the registered channel non-blocking. This eliminates the
// "every raw socket sees every packet" broadcast race (#20) and bounds the read loop
// to exactly as many system calls as needed (#25).
type HealthChecker struct {
	cfg              HealthConfig
	logger           zerolog.Logger
	handshakeChecker HandshakeChecker

	// Shared ICMP socket fields (FR-1.6).
	// id is set once in NewHealthChecker and never modified.
	id     uint16
	socket *icmp.PacketConn
	// socketMu guards all reads and writes of the socket field.
	// PingICMP and demuxLoop take RLock to read; Close takes Lock to nil it out.
	socketMu sync.RWMutex
	demux    map[uint16]chan icmpReply
	demuxMu  sync.Mutex
	// readerDone is closed by demuxLoop when it exits.
	readerDone chan struct{}
	// startOnce and closeOnce protect against concurrent Start/Close calls.
	startOnce sync.Once
	closeOnce sync.Once
	started   bool

	// vrfName is the SO_BINDTODEVICE target for the ICMP socket. When non-empty,
	// the socket is bound to the named VRF master device so probe replies for
	// transport peer IPs (now in VRF table 100 per F-008) reach the kernel via
	// the correct routing context. Empty means main netns (legacy F-006 SNAT
	// fallback or pre-F-008 deployments). Set via BindToVRF before Start().
	vrfName string
}

// NewHealthChecker creates a new HealthChecker with the given configuration.
// handshakeChecker may be nil to disable WG handshake fallback.
func NewHealthChecker(cfg HealthConfig, logger zerolog.Logger, handshakeChecker HandshakeChecker) *HealthChecker {
	return &HealthChecker{
		cfg:              cfg,
		logger:           logger,
		handshakeChecker: handshakeChecker,
		id:               uint16(os.Getpid() & 0xffff),
		demux:            make(map[uint16]chan icmpReply),
		readerDone:       make(chan struct{}),
	}
}

// Start opens the shared ICMP socket and launches the demux reader goroutine.
// It is idempotent: a second call while already started returns nil immediately.
// Start must be called before any PingICMP calls.
// BindToVRF sets the VRF master device for the ICMP socket. Must be called
// BEFORE Start(). Pass "" to disable VRF binding (default behaviour).
//
// When set, Start() opens the socket and applies SO_BINDTODEVICE=<vrfName>
// so transport peer probes (10.93.0.x in VRF table 100) resolve correctly
// per F-008 FR-7. No-op on non-Linux.
func (h *HealthChecker) BindToVRF(vrfName string) {
	h.vrfName = vrfName
}

func (h *HealthChecker) Start() error {
	var openErr error
	h.startOnce.Do(func() {
		conn, err := icmp.ListenPacket("ip4:icmp", "")
		if err != nil {
			h.logger.Error().
				Str("event", "icmp_socket_open").
				Err(err).
				Msg("failed to open shared ICMP socket")
			openErr = fmt.Errorf("icmp listen: %w", err)
			return
		}
		// F-008 FR-7: bind socket to VRF master device so probes for transport
		// peer IPs (now in VRF table 100) resolve via the correct routing
		// context. Skip silently when vrfName is empty (legacy / pre-F-008).
		if h.vrfName != "" {
			if bindErr := bindICMPSocketToVRF(conn, h.vrfName); bindErr != nil {
				_ = conn.Close()
				h.logger.Error().
					Str("event", "icmp_socket_bind_vrf").
					Str("vrf", h.vrfName).
					Err(bindErr).
					Msg("failed to bind ICMP socket to VRF")
				openErr = fmt.Errorf("icmp bind to vrf %q: %w", h.vrfName, bindErr)
				return
			}
			h.logger.Info().
				Str("event", "icmp_socket_bind_vrf").
				Str("vrf", h.vrfName).
				Msg("ICMP socket bound to VRF master device")
		}
		h.socket = conn
		h.started = true
		h.logger.Debug().Str("event", "icmp_socket_open").Msg("shared ICMP socket opened")
		go h.demuxLoop()
	})
	return openErr
}

// Close closes the shared ICMP socket and waits for the demux goroutine to exit.
// Safe to call multiple times and concurrently; only the first call performs work.
func (h *HealthChecker) Close() error {
	if !h.started {
		return nil
	}
	var closeErr error
	h.closeOnce.Do(func() {
		// Grab the socket under write lock: swap it for nil atomically so that
		// concurrent PingICMP callers (holding RLock) either see the live socket
		// or see nil and return an error — never a dangling pointer.
		h.socketMu.Lock()
		sock := h.socket
		h.socket = nil
		h.started = false
		h.socketMu.Unlock()

		if sock == nil {
			return
		}
		// Closing the socket causes demuxLoop's ReadFrom to return an error,
		// which drives the goroutine to exit and close readerDone.
		closeErr = sock.Close()
		<-h.readerDone
	})
	if closeErr != nil {
		return fmt.Errorf("icmp close: %w", closeErr)
	}
	return nil
}

// demuxLoop is the single goroutine that owns the shared socket's ReadFrom.
// It parses each incoming ICMP message; when the message is an EchoReply with
// id == h.id, it looks up the demux channel for the sequence number and delivers
// a reply non-blocking. All other packets are counted as drops.
//
// The goroutine exits when socket.ReadFrom returns any error (including the
// "use of closed network connection" error that Close() causes). On exit it logs
// the cumulative drop count and closes readerDone to unblock Close().
func (h *HealthChecker) demuxLoop() {
	defer close(h.readerDone)

	// Capture the socket reference once under RLock. demuxLoop is the sole
	// reader goroutine and holds the socket for its entire lifetime; it does
	// not race with Close because Close takes the write lock, nils h.socket,
	// then closes the underlying conn — which causes ReadFrom to return an
	// error and drives demuxLoop to exit, after which Close unblocks on
	// <-h.readerDone.
	h.socketMu.RLock()
	sock := h.socket
	h.socketMu.RUnlock()

	if sock == nil {
		return
	}

	buf := make([]byte, 1500)
	// dropCount is accessed only by this single goroutine — no atomic needed.
	var dropCount int64

	for {
		n, _, err := sock.ReadFrom(buf)
		if err != nil {
			// Socket was closed or had an I/O error; exit the loop.
			h.logger.Debug().
				Str("event", "icmp_demux_exit").
				Int64("count", dropCount).
				Msg("ICMP demux reader exiting")
			return
		}

		parsed, parseErr := icmp.ParseMessage(icmpProtocol, buf[:n])
		if parseErr != nil {
			dropCount++
			continue
		}

		if parsed.Type != ipv4.ICMPTypeEchoReply {
			dropCount++
			continue
		}

		echoReply, ok := parsed.Body.(*icmp.Echo)
		if !ok {
			dropCount++
			continue
		}

		// Only handle replies for this checker's ICMP id.
		if uint16(echoReply.ID) != h.id { //nolint:gosec // narrowing int→uint16 intentional (same & mask as sender)
			dropCount++
			h.logger.Debug().
				Str("event", "icmp_demux_drop").
				Int("id", echoReply.ID).
				Int("seq", echoReply.Seq).
				Msg("ICMP reply for different id dropped")
			continue
		}

		seq := uint16(echoReply.Seq) //nolint:gosec // narrowing int→uint16 intentional

		h.demuxMu.Lock()
		ch, found := h.demux[seq]
		h.demuxMu.Unlock()

		if !found {
			// The PingICMP call already timed out and cleaned up.
			dropCount++
			continue
		}

		// Non-blocking send: if the channel already has a value (duplicate reply),
		// or the PingICMP call already timed out and the channel was deleted,
		// we just drop. This prevents the reader goroutine from ever blocking on
		// user-side timing.
		select {
		case ch <- icmpReply{}:
		default:
			dropCount++
		}
	}
}

// HealthTarget is a generic health check target — used by both master and client.
type HealthTarget struct {
	Name          string
	PingAddr      string
	Healthy       bool
	PeerPublicKey wg.Key
}

// Run starts the healthcheck loop. It opens the shared ICMP socket via Start(),
// pings all targets in parallel using native ICMP, calls onDown when a target
// fails consecutively cfg.FailureThreshold times, and onUp when it recovers.
// Blocks until ctx is cancelled.
func (h *HealthChecker) Run(
	ctx context.Context,
	targets func() []HealthTarget,
	onDown func(name string),
	onUp func(name string),
) {
	if err := h.Start(); err != nil {
		h.logger.Error().Err(err).Msg("health checker failed to start ICMP socket")
		return
	}
	defer func() { _ = h.Close() }()

	failures := make(map[string]int)
	ticker := time.NewTicker(h.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			currentTargets := targets()
			purgeStaleFailures(currentTargets, failures)
			results := h.pingAllParallel(ctx, currentTargets)

			for i, t := range currentTargets {
				if t.PingAddr == "" {
					continue
				}

				alive := results[i]

				// Fallback: if ICMP fails but WG handshake is recent, tunnel is alive.
				// Uses PeerPublicKey as the stable WG peer identifier.
				if !alive && h.handshakeChecker != nil && !t.PeerPublicKey.IsZero() {
					lastHS := h.handshakeChecker(t.PeerPublicKey)
					if !lastHS.IsZero() && time.Since(lastHS) < 2*h.cfg.Interval {
						alive = true
						h.logger.Debug().
							Str("tunnel", t.Name).
							Time("last_handshake", lastHS).
							Msg("ICMP failed but WG handshake recent — tunnel alive")
					}
				}

				if alive {
					if failures[t.Name] > 0 || !t.Healthy {
						h.logger.Info().
							Str("tunnel", t.Name).
							Str("ping_target", t.PingAddr).
							Msg("tunnel recovered")
						onUp(t.Name)
					}
					failures[t.Name] = 0
				} else {
					failures[t.Name]++
					h.logger.Warn().
						Str("tunnel", t.Name).
						Str("ping_target", t.PingAddr).
						Int("consecutive_failures", failures[t.Name]).
						Msg("tunnel ping failed")

					if failures[t.Name] >= h.cfg.FailureThreshold && t.Healthy {
						h.logger.Error().
							Str("tunnel", t.Name).
							Str("ping_target", t.PingAddr).
							Msg("tunnel marked down")
						onDown(t.Name)
					}
				}
			}
		}
	}
}

// pingAllParallel pings all targets concurrently and returns results aligned
// with the input slice. Total time is bounded by a single timeout, not N×timeout.
func (h *HealthChecker) pingAllParallel(ctx context.Context, targets []HealthTarget) []bool {
	results := make([]bool, len(targets))
	if len(targets) == 0 {
		return results
	}

	var wg sync.WaitGroup
	for i, t := range targets {
		if t.PingAddr == "" {
			continue
		}
		wg.Add(1)
		go func(idx int, addr string) {
			defer wg.Done()
			alive, err := h.PingICMP(ctx, addr, h.cfg.Timeout)
			if err != nil {
				h.logger.Debug().Err(err).Str("target", addr).Msg("ping structural error")
			}
			results[idx] = alive
		}(i, t.PingAddr)
	}
	wg.Wait()
	return results
}

// purgeStaleFailures removes entries from the failures map that are no longer
// in the current targets list. Prevents stale failure counts from affecting
// recreated tunnels.
func purgeStaleFailures(targets []HealthTarget, failures map[string]int) {
	active := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		active[t.Name] = struct{}{}
	}
	for name := range failures {
		if _, ok := active[name]; !ok {
			delete(failures, name)
		}
	}
}

// PingICMP sends a single ICMP echo request to ip and returns whether a reply
// was received within timeout (or ctx deadline, whichever is earlier).
//
// Unlike the old per-call socket approach, this method uses the HealthChecker's
// shared raw ICMP socket (opened by Start). It registers a per-seq reply channel
// in the demux map, writes the echo, then blocks on the channel or deadline.
// This eliminates the broadcast-to-all-sockets race (issue #20) and the unbounded
// read loop (issue #25) — the demux reader goroutine does the reading; PingICMP
// only waits on a buffered channel.
//
// Returns (false, nil) for unreachable hosts or timeouts, and (false, err) for
// structural failures (invalid IP, socket write failure).
//
// Start() must be called before PingICMP. Calling PingICMP after Close() returns
// (false, error) immediately rather than panicking on the nil socket.
func (h *HealthChecker) PingICMP(ctx context.Context, ip string, timeout time.Duration) (bool, error) {
	// Read h.socket under RLock to prevent a race with Close() which takes Lock
	// to nil it out. We capture the pointer into a local variable; after RUnlock
	// the field may be nilled by a concurrent Close, but our local sock still
	// points to the live (or being-closed) *icmp.PacketConn. WriteTo on a
	// being-closed conn returns an error rather than panicking, so the call is safe.
	h.socketMu.RLock()
	sock := h.socket
	h.socketMu.RUnlock()

	if sock == nil {
		return false, fmt.Errorf("PingICMP called on stopped HealthChecker")
	}

	parsedIP := net.ParseIP(ip)
	if parsedIP == nil {
		return false, fmt.Errorf("invalid IP address: %s", ip)
	}

	seq := seqCounter.next()
	reply := make(chan icmpReply, 1)

	h.demuxMu.Lock()
	h.demux[seq] = reply
	h.demuxMu.Unlock()

	// Cleanup: remove the channel from the demux map on exit.
	// We do NOT close the channel here because demuxLoop may hold a reference to it
	// (obtained under the lock before the delete) and attempt a non-blocking send after
	// we delete. Closing a channel while another goroutine might send to it causes a
	// panic. The reply channel is buffered(1) and GC'd naturally once all references
	// (this stack frame + any in-flight demuxLoop send) drop. The send is safe:
	// the buffer absorbs it or the select-default arm drops it — either is a no-op.
	defer func() {
		h.demuxMu.Lock()
		delete(h.demux, seq)
		h.demuxMu.Unlock()
	}()

	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{
			ID:   int(h.id),
			Seq:  int(seq),
			Data: []byte("awgmesh"),
		},
	}
	msgBytes, err := msg.Marshal(nil)
	if err != nil {
		return false, fmt.Errorf("marshal icmp: %w", err)
	}

	dst := &net.IPAddr{IP: parsedIP}
	if _, err := sock.WriteTo(msgBytes, dst); err != nil {
		return false, fmt.Errorf("icmp write: %w", err)
	}

	// Effective deadline: whichever of ctx deadline and explicit timeout is earlier.
	effectiveTimeout := timeout
	if d, ok := ctx.Deadline(); ok {
		remaining := time.Until(d)
		if remaining < effectiveTimeout {
			effectiveTimeout = remaining
		}
	}

	select {
	case <-reply:
		return true, nil
	case <-ctx.Done():
		return false, nil
	case <-time.After(effectiveTimeout):
		return false, nil
	}
}

// seqCounter provides a process-wide atomic sequence number for ICMP echo requests.
// It is safe for concurrent use across multiple HealthChecker instances.
// Process-wide (rather than per-checker) ensures uniqueness even if multiple
// checkers share the same ICMP id (os.Getpid() & 0xffff).
// Uses atomic.AddUint32 to avoid mutex contention on the ICMP hot path.
var seqCounter = &icmpSeqCounter{}

type icmpSeqCounter struct {
	val uint32
}

func (c *icmpSeqCounter) next() uint16 {
	v := atomic.AddUint32(&c.val, 1)
	return uint16(v & 0xffff) //nolint:gosec // intentional wrap-around for ICMP seq field
}
