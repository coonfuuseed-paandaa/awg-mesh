//go:build linux

package routing

import (
	"fmt"
	"net"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

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

	for _, p := range policies {
		if p.DSCP < 1 || p.DSCP > 63 {
			return fmt.Errorf("DSCP value %d out of range (1-63)", p.DSCP)
		}

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
			// Ignore "file exists" — rule may already exist from previous run.
			if err.Error() != "file exists" {
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

// TeardownDSCPPolicyRouting removes all DSCP routing rules and the nftables table.
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
	if err := conn.Flush(); err != nil {
		// Table may not exist — not an error.
		return nil
	}

	// Clean up ip rules with marks matching DSCP range.
	rules, err := netlink.RuleList(unix.AF_INET)
	if err != nil {
		return fmt.Errorf("list ip rules: %w", err)
	}

	for _, rule := range rules {
		if rule.Mark >= 1 && rule.Mark <= 63 && rule.Priority >= 101 && rule.Priority <= 163 {
			if deleteErr := netlink.RuleDel(&rule); deleteErr != nil {
				// Best effort cleanup.
				continue
			}
		}
	}

	return nil
}

// encodeUint32 encodes a uint32 in native byte order for nftables expressions.
func encodeUint32(v uint32) []byte {
	return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
}
