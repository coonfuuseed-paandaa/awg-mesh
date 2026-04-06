package node

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
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

// HealthChecker monitors tunnel liveness and fires callbacks on state transitions.
type HealthChecker struct {
	cfg              HealthConfig
	logger           zerolog.Logger
	handshakeChecker HandshakeChecker
}

// NewHealthChecker creates a new HealthChecker with the given configuration.
// handshakeChecker may be nil to disable WG handshake fallback.
func NewHealthChecker(cfg HealthConfig, logger zerolog.Logger, handshakeChecker HandshakeChecker) *HealthChecker {
	return &HealthChecker{
		cfg:              cfg,
		logger:           logger,
		handshakeChecker: handshakeChecker,
	}
}

// HealthTarget is a generic health check target — used by both master and client.
type HealthTarget struct {
	Name          string
	PingAddr      string
	Healthy       bool
	PeerPublicKey wg.Key
}

// Run starts the healthcheck loop. It pings all targets in parallel using native
// ICMP, calls onDown when a target fails consecutively cfg.FailureThreshold times,
// and onUp when it recovers. Blocks until ctx is cancelled.
func (h *HealthChecker) Run(
	ctx context.Context,
	targets func() []HealthTarget,
	onDown func(name string),
	onUp func(name string),
) {
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
					if failures[t.Name] > 0 {
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
			alive, err := PingICMP(ctx, addr, h.cfg.Timeout)
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
// was received. Unlike exec.Command("ping"), this uses native ICMP sockets with
// zero fork overhead. Returns (false, nil) for unreachable hosts and (false, err)
// for structural failures (invalid IP, permission denied).
func PingICMP(ctx context.Context, ip string, timeout time.Duration) (bool, error) {
	parsedIP := net.ParseIP(ip)
	if parsedIP == nil {
		return false, fmt.Errorf("invalid IP address: %s", ip)
	}

	conn, err := icmp.ListenPacket("ip4:icmp", "")
	if err != nil {
		return false, fmt.Errorf("icmp listen: %w", err)
	}
	defer func() { _ = conn.Close() }()

	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return false, fmt.Errorf("set deadline: %w", err)
	}

	pid := os.Getpid() & 0xffff
	seq := seqCounter.next()

	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{
			ID:   pid,
			Seq:  seq,
			Data: []byte("awgmesh"),
		},
	}
	msgBytes, err := msg.Marshal(nil)
	if err != nil {
		return false, fmt.Errorf("marshal icmp: %w", err)
	}

	dst := &net.IPAddr{IP: parsedIP}
	if _, err := conn.WriteTo(msgBytes, dst); err != nil {
		return false, fmt.Errorf("icmp write: %w", err)
	}

	reply := make([]byte, 1500)
	for {
		n, _, readErr := conn.ReadFrom(reply)
		if readErr != nil {
			return false, nil // timeout or read error — host unreachable
		}

		parsed, parseErr := icmp.ParseMessage(icmpProtocol, reply[:n])
		if parseErr != nil {
			continue
		}

		if parsed.Type != ipv4.ICMPTypeEchoReply {
			continue
		}

		echoReply, ok := parsed.Body.(*icmp.Echo)
		if !ok {
			continue
		}

		if echoReply.ID == pid && echoReply.Seq == seq {
			return true, nil
		}
	}
}

// seqCounter provides a process-wide atomic sequence number for ICMP echo requests.
var seqCounter = &icmpSeqCounter{}

type icmpSeqCounter struct {
	mu  sync.Mutex
	val int
}

func (c *icmpSeqCounter) next() int {
	c.mu.Lock()
	c.val++
	seq := c.val & 0xffff
	c.mu.Unlock()
	return seq
}


