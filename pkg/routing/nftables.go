//go:build linux

package routing

import (
	"fmt"
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

// EnableStickyECMP sets up connmark-based session stickiness for ECMP routes.
func (f *NftablesFirewall) EnableStickyECMP(balancerCIDR string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.ensureTable()

	// Prerouting: restore connmark on established connections
	preChain := f.conn.AddChain(&nftables.Chain{
		Name:     "mangle_prerouting",
		Table:    f.table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookPrerouting,
		Priority: nftables.ChainPriorityMangle,
	})

	f.conn.AddRule(&nftables.Rule{
		Table: f.table,
		Chain: preChain,
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
			// ct mark set mark (restore)
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

	// Postrouting: save connmark on new connections
	postChain := f.conn.AddChain(&nftables.Chain{
		Name:     "mangle_postrouting",
		Table:    f.table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityMangle,
	})

	f.conn.AddRule(&nftables.Rule{
		Table: f.table,
		Chain: postChain,
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
			// meta mark set ct mark (save)
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

	if err := f.conn.Flush(); err != nil {
		return fmt.Errorf("nftables: enable sticky ECMP for %s: %w", balancerCIDR, err)
	}
	return nil
}

// DisableStickyECMP is handled by TeardownNAT (deletes entire table).
func (f *NftablesFirewall) DisableStickyECMP(_ string) error {
	return nil // Cleanup happens via TeardownNAT
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
