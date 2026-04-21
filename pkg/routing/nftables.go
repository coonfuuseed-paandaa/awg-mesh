//go:build linux

package routing

import (
	"bytes"
	"fmt"
	"net"
	"os/exec"
	"sync"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// NftablesFirewall implements Firewall using the nftables kernel API.
// Uses a dedicated table "awg_mesh" to isolate rules from host config.
type NftablesFirewall struct {
	mu    sync.Mutex
	table *nftables.Table
	conn  *nftables.Conn
}

// NewNftablesFirewall creates a new nftables-based firewall manager.
func NewNftablesFirewall() (*NftablesFirewall, error) {
	conn, err := nftables.New()
	if err != nil {
		return nil, fmt.Errorf("nftables: create connection: %w", err)
	}
	return &NftablesFirewall{conn: conn}, nil
}

// ensureTable creates the awg_mesh table if it doesn't exist.
func (f *NftablesFirewall) ensureTable() {
	if f.table != nil {
		return
	}
	f.table = f.conn.AddTable(&nftables.Table{
		Family: nftables.TableFamilyIPv4,
		Name:   "awg_mesh",
	})
}

// SetupNAT creates a masquerade rule for outbound traffic on iface.
func (f *NftablesFirewall) SetupNAT(iface string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.ensureTable()

	chain := f.conn.AddChain(&nftables.Chain{
		Name:     "nat_postrouting",
		Table:    f.table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityNATSource,
	})

	// Pad interface name to 16 bytes (IFNAMSIZ)
	ifaceBytes := make([]byte, 16)
	copy(ifaceBytes, iface)

	f.conn.AddRule(&nftables.Rule{
		Table: f.table,
		Chain: chain,
		Exprs: []expr.Any{
			&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     ifaceBytes,
			},
			&expr.Masq{},
		},
	})

	if err := f.conn.Flush(); err != nil {
		return fmt.Errorf("nftables: setup NAT on %s: %w", iface, err)
	}
	return nil
}

// ClampMSSToPMTU adds a rule to clamp TCP MSS to path MTU on forwarded packets.
func (f *NftablesFirewall) ClampMSSToPMTU() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.ensureTable()

	chain := f.conn.AddChain(&nftables.Chain{
		Name:     "forward_mss",
		Table:    f.table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookForward,
		Priority: nftables.ChainPriorityFilter,
	})

	f.conn.AddRule(&nftables.Rule{
		Table: f.table,
		Chain: chain,
		Exprs: []expr.Any{
			// Match TCP protocol
			&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     []byte{unix.IPPROTO_TCP},
			},
			// Match SYN flag (tcp flags & (SYN|RST) == SYN)
			&expr.Payload{
				DestRegister: 1,
				Base:         expr.PayloadBaseTransportHeader,
				Offset:       13, // TCP flags byte
				Len:          1,
			},
			&expr.Bitwise{
				SourceRegister: 1,
				DestRegister:   1,
				Len:            1,
				Mask:           []byte{0x06}, // SYN|RST
				Xor:            []byte{0x00},
			},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     []byte{0x02}, // SYN only
			},
			// Clamp MSS to PMTU
			&expr.Exthdr{
				DestRegister: 1,
				Type:         2, // TCP MSS option
				Offset:       2,
				Len:          2,
				Op:           expr.ExthdrOpTcpopt,
			},
			&expr.Rt{
				Register: 1,
				Key:      expr.RtTCPMSS,
			},
			&expr.Exthdr{
				SourceRegister: 1,
				Type:           2,
				Offset:         2,
				Len:            2,
				Op:             expr.ExthdrOpTcpopt,
			},
		},
	})

	if err := f.conn.Flush(); err != nil {
		return fmt.Errorf("nftables: clamp MSS to PMTU: %w", err)
	}
	return nil
}

// cidrFilterExprs returns 3 nftables expressions that match packets whose
// destination IP falls within the given CIDR. They use register 2 so as not to
// clobber the conntrack state in register 1 used by the sticky ECMP rules.
//
// Expressions:
//  1. Payload — load IPv4 dst (offset 16, len 4) from network header into reg 2.
//  2. Bitwise — AND reg 2 with the CIDR mask, result stays in reg 2.
//  3. Cmp     — compare reg 2 == network address bytes.
func cidrFilterExprs(cidr *net.IPNet) []expr.Any {
	networkIP := cidr.IP.To4()
	mask := []byte(cidr.Mask)
	return []expr.Any{
		// Load IPv4 dst into register 2 (offset 16 = dst addr in IPv4 header).
		&expr.Payload{
			DestRegister: 2,
			Base:         expr.PayloadBaseNetworkHeader,
			Offset:       16,
			Len:          4,
		},
		// Mask with CIDR mask.
		&expr.Bitwise{
			SourceRegister: 2,
			DestRegister:   2,
			Len:            4,
			Mask:           mask,
			Xor:            []byte{0x00, 0x00, 0x00, 0x00},
		},
		// Compare masked result == network address.
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 2,
			Data:     networkIP,
		},
	}
}

// EnableStickyECMP sets up connmark-based session stickiness for ECMP routes,
// scoped to packets whose destination IP falls within balancerCIDR.
func (f *NftablesFirewall) EnableStickyECMP(balancerCIDR string) error {
	_, ipNet, err := net.ParseCIDR(balancerCIDR)
	if err != nil {
		return fmt.Errorf("nftables: parse balancer CIDR %q: %w", balancerCIDR, err)
	}

	cidrExprs := cidrFilterExprs(ipNet)

	f.mu.Lock()
	defer f.mu.Unlock()

	f.ensureTable()

	// Prerouting: restore connmark on established connections scoped to CIDR.
	preChain := f.conn.AddChain(&nftables.Chain{
		Name:     "mangle_prerouting",
		Table:    f.table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookPrerouting,
		Priority: nftables.ChainPriorityMangle,
	})

	preExprs := make([]expr.Any, 0, len(cidrExprs)+5)
	preExprs = append(preExprs, cidrExprs...)
	preExprs = append(preExprs,
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
		// ct mark → meta mark (restore)
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
	)

	f.conn.AddRule(&nftables.Rule{
		Table: f.table,
		Chain: preChain,
		Exprs: preExprs,
	})

	// Postrouting: save connmark on new connections scoped to CIDR.
	postChain := f.conn.AddChain(&nftables.Chain{
		Name:     "mangle_postrouting",
		Table:    f.table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityMangle,
	})

	postExprs := make([]expr.Any, 0, len(cidrExprs)+5)
	postExprs = append(postExprs, cidrExprs...)
	postExprs = append(postExprs,
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
		// meta mark → ct mark (save)
		&expr.Meta{
			Key:      expr.MetaKeyMARK,
			Register: 1,
		},
		&expr.Ct{
			Key:            expr.CtKeyMARK,
			Register:       1,
			SourceRegister: true,
		},
	)

	f.conn.AddRule(&nftables.Rule{
		Table: f.table,
		Chain: postChain,
		Exprs: postExprs,
	})

	if err := f.conn.Flush(); err != nil {
		return fmt.Errorf("nftables: enable sticky ECMP for %s: %w", balancerCIDR, err)
	}
	return nil
}

// ruleMatchesCIDR reports whether rule was installed by EnableStickyECMP for
// the given CIDR. It checks the first 3 expressions (payload/bitwise/cmp)
// against the CIDR's mask and network bytes.
func ruleMatchesCIDR(rule *nftables.Rule, ipNet *net.IPNet) bool {
	if len(rule.Exprs) < 3 {
		return false
	}
	pl, ok := rule.Exprs[0].(*expr.Payload)
	if !ok || pl.Base != expr.PayloadBaseNetworkHeader || pl.Offset != 16 || pl.Len != 4 {
		return false
	}
	bw, ok := rule.Exprs[1].(*expr.Bitwise)
	if !ok || !bytes.Equal(bw.Mask, []byte(ipNet.Mask)) {
		return false
	}
	cmp, ok := rule.Exprs[2].(*expr.Cmp)
	if !ok || cmp.Op != expr.CmpOpEq {
		return false
	}
	return bytes.Equal(cmp.Data, ipNet.IP.To4())
}

// DisableStickyECMP removes the CIDR-scoped sticky ECMP rules installed by
// EnableStickyECMP for cidr. Rules for other CIDRs are not touched.
// Idempotent: no error if cidr was never enabled.
func (f *NftablesFirewall) DisableStickyECMP(cidr string) error {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("nftables: parse CIDR %q: %w", cidr, err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.table == nil {
		return nil
	}

	chainNames := []string{"mangle_prerouting", "mangle_postrouting"}
	for _, chainName := range chainNames {
		chain := &nftables.Chain{Name: chainName, Table: f.table}
		rules, err := f.conn.GetRules(f.table, chain)
		if err != nil {
			// Chain may not exist yet — idempotent, skip.
			continue
		}
		for _, rule := range rules {
			if ruleMatchesCIDR(rule, ipNet) {
				if err := f.conn.DelRule(rule); err != nil {
					return fmt.Errorf("nftables: delete sticky ECMP rule for %s in %s: %w", cidr, chainName, err)
				}
			}
		}
	}

	if err := f.conn.Flush(); err != nil {
		return fmt.Errorf("nftables: disable sticky ECMP for %s: %w", cidr, err)
	}
	return nil
}

// TeardownNAT removes the entire awg_mesh table and all its chains/rules.
func (f *NftablesFirewall) TeardownNAT() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.table == nil {
		return nil
	}

	f.conn.DelTable(f.table)
	if err := f.conn.Flush(); err != nil {
		return fmt.Errorf("nftables: teardown: %w", err)
	}
	f.table = nil
	return nil
}

// EnableWGCrossTunnelForward adds an iptables FORWARD ACCEPT rule for wg-+ → wg-+
// traffic. Required on Docker/VPS hosts where the FORWARD chain default policy is
// DROP — without this rule, endpoint↔endpoint overlay packets forwarded between
// wg-<ep-a> and wg-<ep-b> on the master are silently discarded (local tracker #150).
//
// Uses the iptables CLI (already present in the container image via the iptables
// apk package) because the go-nftables expr API does not support ifname-prefix
// matching ("wg-+" wildcard). Shells out to iptables -C (check) then iptables -I
// (insert at top) — idempotent: no-op if the rule already exists.
//
// Non-fatal by design: callers should log a warning and continue if this returns
// an error; master→endpoint traffic is unaffected by the FORWARD chain.
//
// Teardown: the rule is intentionally NOT removed on master shutdown. In a
// container workload the host network namespace is torn down with the container,
// making explicit cleanup redundant. For restarts without container removal the
// retained rule means iptables -C finds it on the next startup and skips the
// insert — idempotency is preserved across restarts at no extra cost.
func (f *NftablesFirewall) EnableWGCrossTunnelForward() error {
	// iptables -C exits 0 if the rule exists, non-zero otherwise.
	check := exec.Command("iptables", "-C", "FORWARD", "-i", "wg-+", "-o", "wg-+", "-j", "ACCEPT")
	if err := check.Run(); err == nil {
		return nil // rule already present — idempotent
	}

	insert := exec.Command("iptables", "-I", "FORWARD", "-i", "wg-+", "-o", "wg-+", "-j", "ACCEPT")
	out, err := insert.CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables -I FORWARD wg-+ wg-+ ACCEPT: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}
