package topology

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
)

// Range describes a named overlay address range.
type Range struct {
	Name       string
	Network    netip.Prefix
	BalancerIP netip.Addr
}

// ParseRange parses and normalizes a named range.
func ParseRange(nr NamedRange) (Range, error) {
	prefix, err := netip.ParsePrefix(nr.CIDR)
	if err != nil {
		return Range{}, fmt.Errorf("parse CIDR %q: %w", nr.CIDR, err)
	}

	result := Range{
		Name:    nr.Name,
		Network: prefix.Masked(),
	}

	if nr.BalancerIP != "" {
		balancerIP, err := netip.ParseAddr(nr.BalancerIP)
		if err != nil {
			return Range{}, fmt.Errorf("parse balancer IP %q: %w", nr.BalancerIP, err)
		}
		result.BalancerIP = normalizeAddr(balancerIP)
	}

	return result, nil
}

// Contains reports whether ip belongs to r.
func (r Range) Contains(ip netip.Addr) bool {
	return r.Network.Contains(ip)
}

// AvailableIPs returns usable host addresses in the range.
func (r Range) AvailableIPs() []netip.Addr {
	network := r.Network.Masked().Addr()
	last := prefixLastAddr(r.Network.Masked())
	first := network.Next()

	if !first.IsValid() || !first.Less(last) {
		return []netip.Addr{}
	}

	ips := make([]netip.Addr, 0)
	for ip := first; ip.IsValid() && ip.Less(last); ip = ip.Next() {
		ips = append(ips, ip)
	}

	return ips
}

// AllocateIP allocates the first free IP across ranges.
// Uses iterative approach to avoid materializing all IPs for large CIDR blocks.
func AllocateIP(ranges []Range, existing []netip.Addr) (netip.Addr, error) {
	used := make(map[netip.Addr]struct{}, len(existing))
	for _, addr := range existing {
		if !addr.IsValid() {
			continue
		}
		used[normalizeAddr(addr)] = struct{}{}
	}

	for _, currentRange := range ranges {
		maskedPrefix := currentRange.Network.Masked()
		addr := maskedPrefix.Addr().Next() // skip network address
		last := prefixLastAddr(maskedPrefix)

		for addr.IsValid() && addr.Less(last) {
			normalized := normalizeAddr(addr)
			if _, exists := used[normalized]; !exists {
				return normalized, nil
			}
			addr = addr.Next()
		}
	}

	return netip.Addr{}, errors.New("no available IP addresses in provided ranges")
}

// RangesOverlap reports whether two ranges overlap.
func RangesOverlap(a, b Range) bool {
	aPrefix := a.Network.Masked()
	bPrefix := b.Network.Masked()
	return aPrefix.Contains(bPrefix.Addr()) || bPrefix.Contains(aPrefix.Addr())
}

func normalizeAddr(addr netip.Addr) netip.Addr {
	if addr.Is4In6() {
		return addr.Unmap()
	}
	return addr
}

func prefixLastAddr(prefix netip.Prefix) netip.Addr {
	maskedPrefix := prefix.Masked()
	if maskedPrefix.Addr().Is4() {
		addr4 := maskedPrefix.Addr().As4()
		base := binary.BigEndian.Uint32(addr4[:])

		hostBits := 32 - maskedPrefix.Bits()
		var hostMask uint32
		if hostBits == 32 {
			hostMask = ^uint32(0)
		} else if hostBits == 0 {
			hostMask = 0
		} else {
			hostMask = (uint32(1) << uint(hostBits)) - 1
		}

		var out [4]byte
		binary.BigEndian.PutUint32(out[:], base|hostMask)
		return netip.AddrFrom4(out)
	}

	addr16 := maskedPrefix.Addr().As16()
	prefixBits := maskedPrefix.Bits()
	fullBytes := prefixBits / 8
	remainderBits := prefixBits % 8

	if fullBytes < len(addr16) {
		if remainderBits != 0 {
			addr16[fullBytes] |= byte(0xFF >> uint(remainderBits))
			fullBytes++
		}
		for i := fullBytes; i < len(addr16); i++ {
			addr16[i] = 0xFF
		}
	}

	return netip.AddrFrom16(addr16)
}
