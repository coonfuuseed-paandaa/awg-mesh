//go:build linux

package routing

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strings"
	"syscall"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// rtTablesPath is the path to the iproute2 rt_tables.d file written by
// writeRtTables. Overridable in tests via package-level assignment.
var rtTablesPath = "/etc/iproute2/rt_tables.d/awg-mesh.conf"

// DSCPPolicy defines a single DSCP->fwmark->table routing policy.
type DSCPPolicy struct {
	DSCP    int    // DSCP value (1-63) to match in IP header
	Fwmark  int    // Firewall mark to set (typically same as DSCP)
	TableID int    // Routing table ID for policy routing
	Gateway string // Next-hop gateway IP (WG tunnel transport IP)
	Device  string // Network device (WG interface name)
}

// SetupDSCPPolicyRouting creates nftables rules for DSCP->fwmark mapping
// and ip rule/route entries for policy-based routing.
func SetupDSCPPolicyRouting(policies []DSCPPolicy) error {
	if len(policies) == 0 {
		return nil
	}

	conn, err := nftables.New()
	if err != nil {
		return fmt.Errorf("nftables connection: %w", err)
	}

	table := conn.AddTable(&nftables.Table{
		Family: nftables.TableFamilyIPv4,
		Name:   "awg_dscp",
	})

	chain := conn.AddChain(&nftables.Chain{
		Name:     "dscp_mark",
		Table:    table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookPrerouting,
		Priority: nftables.ChainPriorityMangle,
	})

	seenDSCP := make(map[int]bool, len(policies))
	for _, p := range policies {
		if p.DSCP < 1 || p.DSCP > 63 {
			return fmt.Errorf("DSCP value %d out of range (1-63)", p.DSCP)
		}
		if p.Fwmark != p.DSCP {
			return fmt.Errorf("fwmark %d must equal DSCP %d (invariant: mark == DSCP)", p.Fwmark, p.DSCP)
		}
		expectedTable := dscpPriorityBase + p.DSCP
		if p.TableID != expectedTable {
			return fmt.Errorf("table ID %d must equal %d + DSCP (%d)", p.TableID, dscpPriorityBase, expectedTable)
		}
		if seenDSCP[p.DSCP] {
			return fmt.Errorf("duplicate DSCP value %d in routing policies", p.DSCP)
		}
		seenDSCP[p.DSCP] = true

		// Match IP DSCP field and set fwmark.
		// DSCP is stored in TOS byte bits 7-2 (shifted left by 2).
		tosValue := byte(p.DSCP << 2)
		conn.AddRule(&nftables.Rule{
			Table: table,
			Chain: chain,
			Exprs: []expr.Any{
				// Load TOS byte from IP header (offset 1, 1 byte)
				&expr.Payload{
					DestRegister: 1,
					Base:         expr.PayloadBaseNetworkHeader,
					Offset:       1, // TOS/DSCP byte
					Len:          1,
				},
				// Mask to get DSCP bits only (upper 6 bits: 0xFC)
				&expr.Bitwise{
					SourceRegister: 1,
					DestRegister:   1,
					Len:            1,
					Mask:           []byte{0xFC},
					Xor:            []byte{0x00},
				},
				// Compare with expected DSCP value (shifted left 2)
				&expr.Cmp{
					Op:       expr.CmpOpEq,
					Register: 1,
					Data:     []byte{tosValue},
				},
				// Set fwmark
				&expr.Immediate{
					Register: 1,
					Data:     encodeUint32(uint32(p.Fwmark)),
				},
				&expr.Meta{
					Key:            expr.MetaKeyMARK,
					SourceRegister: true,
					Register:       1,
				},
			},
		})
	}

	// Connmark restore chain: for ESTABLISHED/RELATED connections, restore
	// fwmark from connmark so reply packets use the same policy route as the
	// original DSCP-marked packet. Without this, return traffic falls through
	// to the default route because replies typically have DSCP 0.
	restoreChain := conn.AddChain(&nftables.Chain{
		Name:     "connmark_restore",
		Table:    table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookPrerouting,
		Priority: nftables.ChainPriorityMangle,
	})

	conn.AddRule(&nftables.Rule{
		Table: table,
		Chain: restoreChain,
		Exprs: []expr.Any{
			// ct state established,related
			&expr.Ct{Key: expr.CtKeySTATE, Register: 1},
			&expr.Bitwise{
				SourceRegister: 1,
				DestRegister:   1,
				Len:            4,
				Mask:           []byte{0x06, 0x00, 0x00, 0x00}, // ESTABLISHED|RELATED
				Xor:            []byte{0x00, 0x00, 0x00, 0x00},
			},
			&expr.Cmp{
				Op:       expr.CmpOpNeq,
				Register: 1,
				Data:     []byte{0x00, 0x00, 0x00, 0x00},
			},
			// meta mark set ct mark (restore connmark → fwmark)
			&expr.Ct{
				Key:            expr.CtKeyMARK,
				Register:       1,
				SourceRegister: false,
			},
			&expr.Meta{
				Key:            expr.MetaKeyMARK,
				SourceRegister: true,
				Register:       1,
			},
		},
	})

	// Connmark save chain: for NEW connections with a fwmark set by the
	// dscp_mark chain, save fwmark to connmark so it persists across packets
	// in the same connection (used by connmark_restore above).
	saveChain := conn.AddChain(&nftables.Chain{
		Name:     "connmark_save",
		Table:    table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityMangle,
	})

	conn.AddRule(&nftables.Rule{
		Table: table,
		Chain: saveChain,
		Exprs: []expr.Any{
			// ct state new
			&expr.Ct{Key: expr.CtKeySTATE, Register: 1},
			&expr.Bitwise{
				SourceRegister: 1,
				DestRegister:   1,
				Len:            4,
				Mask:           []byte{0x08, 0x00, 0x00, 0x00}, // NEW
				Xor:            []byte{0x00, 0x00, 0x00, 0x00},
			},
			&expr.Cmp{
				Op:       expr.CmpOpNeq,
				Register: 1,
				Data:     []byte{0x00, 0x00, 0x00, 0x00},
			},
			// ct mark set meta mark (save fwmark → connmark)
			&expr.Meta{
				Key:      expr.MetaKeyMARK,
				Register: 1,
			},
			&expr.Ct{
				Key:            expr.CtKeyMARK,
				Register:       1,
				SourceRegister: true,
			},
		},
	})

	// Write named routing table entries so 'ip route show table awg-dscp-N'
	// resolves the table name. Non-fatal: DSCP routing still functions without
	// the name file; only ops tooling (ip route show) breaks.
	if err := writeRtTables(policies); err != nil {
		log.Printf("awg-mesh/routing: writeRtTables: %v (non-fatal)", err)
	}

	if err := conn.Flush(); err != nil {
		return fmt.Errorf("nftables flush DSCP rules: %w", err)
	}

	// Set up ip rules and routes for each policy.
	for _, p := range policies {
		// ip rule add fwmark <mark> lookup <table>
		rule := netlink.NewRule()
		rule.Mark = uint32(p.Fwmark)
		rule.Table = p.TableID
		rule.Priority = 100 + p.DSCP
		if err := netlink.RuleAdd(rule); err != nil {
			// Ignore EEXIST — rule may already exist from previous run.
			if !errors.Is(err, syscall.EEXIST) {
				return fmt.Errorf("ip rule add fwmark %d table %d: %w", p.Fwmark, p.TableID, err)
			}
		}

		// ip route add default via <gateway> dev <device> table <tableID>
		if p.Gateway != "" && p.Device != "" {
			gw := net.ParseIP(p.Gateway)
			if gw == nil {
				return fmt.Errorf("invalid gateway %q for DSCP %d", p.Gateway, p.DSCP)
			}

			link, err := netlink.LinkByName(p.Device)
			if err != nil {
				return fmt.Errorf("link %q for DSCP %d: %w", p.Device, p.DSCP, err)
			}

			route := &netlink.Route{
				Dst:       &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
				Gw:        gw,
				LinkIndex: link.Attrs().Index,
				Table:     p.TableID,
			}
			if err := netlink.RouteReplace(route); err != nil {
				return fmt.Errorf("route add default via %s dev %s table %d: %w", p.Gateway, p.Device, p.TableID, err)
			}
		}
	}

	return nil
}

// TeardownDSCPPolicyRouting removes all DSCP routing rules and the nftables
// table. It logs and continues ip rule cleanup if table teardown fails, but still
// returns the captured nftables flush error.
func TeardownDSCPPolicyRouting() error {
	conn, err := nftables.New()
	if err != nil {
		return fmt.Errorf("nftables connection: %w", err)
	}

	// Delete the entire awg_dscp table.
	conn.DelTable(&nftables.Table{
		Family: nftables.TableFamilyIPv4,
		Name:   "awg_dscp",
	})
	var teardownErr error
	if err := conn.Flush(); err != nil {
		// Missing table (ENOENT) is the expected fast path: first teardown or
		// already-removed. Log and return nil for idempotence. Anything else
		// (daemon down, EPERM, transient kernel error) is captured and returned
		// after ip rule cleanup so operators see the signal without leaking
		// stale fwmark routing rules.
		if errors.Is(err, syscall.ENOENT) {
			log.Printf("awg-mesh/routing: awg_dscp table absent; continuing with ip rule cleanup (expected)")
		} else {
			log.Printf("awg-mesh/routing: nftables DelTable flush returned %v; continuing with ip rule cleanup", err)
			teardownErr = fmt.Errorf("nftables flush awg_dscp table: %w", err)
		}
	}

	// Clean up ip rules with marks matching DSCP range.
	rules, err := netlink.RuleList(unix.AF_INET)
	if err != nil {
		return fmt.Errorf("list ip rules: %w", err)
	}

	for _, rule := range rules {
		if shouldCleanupDSCPRule(rule) {
			if deleteErr := netlink.RuleDel(&rule); deleteErr != nil {
				// Best effort cleanup.
				continue
			}
		}
	}

	return teardownErr
}

// DSCP policy-routing invariant established by SetupDSCPPolicyRouting:
// - mark values are DSCP codepoints 1..63
// - priority and table are both exactly equal to dscpPriorityBase + mark
// This stricter predicate prevents deleting foreign rules that coincidentally
// fall in broad ranges but do not follow awg-mesh's exact mapping.
const (
	dscpPriorityBase        = 100
	dscpMarkMin      uint32 = 1
	dscpMarkMax      uint32 = 63
)

func shouldCleanupDSCPRule(rule netlink.Rule) bool {
	if rule.Mark < dscpMarkMin || rule.Mark > dscpMarkMax {
		return false
	}
	expected := dscpPriorityBase + int(rule.Mark)
	return rule.Priority == expected && rule.Table == expected
}

// writeRtTables writes /etc/iproute2/rt_tables.d/awg-mesh.conf (or the path
// overridden by rtTablesPath) with one line per DSCP policy in the form:
//
//	<id> <name>
//
// where id = 100 + DSCPValue and name = "awg-dscp-<DSCPValue>".
// Output is sorted by DSCP value for deterministic, idempotent output.
// An atomic write (write to .tmp + rename) is used to avoid partial reads.
func writeRtTables(policies []DSCPPolicy) error {
	sorted := make([]DSCPPolicy, len(policies))
	copy(sorted, policies)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].DSCP < sorted[j].DSCP
	})

	var sb strings.Builder
	for _, p := range sorted {
		id := dscpPriorityBase + p.DSCP
		fmt.Fprintf(&sb, "%d awg-dscp-%d\n", id, p.DSCP)
	}
	content := []byte(sb.String())

	tmpPath := rtTablesPath + ".tmp"
	if err := os.WriteFile(tmpPath, content, 0o644); err != nil {
		return fmt.Errorf("writeRtTables: write tmp: %w", err)
	}
	if err := os.Rename(tmpPath, rtTablesPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("writeRtTables: rename: %w", err)
	}
	return nil
}

// encodeUint32 encodes a uint32 in little-endian byte order for nftables register values.
// This matches the native byte order on x86_64/arm64 (the only target architectures).
func encodeUint32(v uint32) []byte {
	return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
}
