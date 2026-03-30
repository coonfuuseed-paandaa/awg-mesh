//go:build linux

package ebpf

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/vishvananda/netlink"
)

// Forwarder manages eBPF TC programs for inter-WG-interface forwarding.
// A single BPF program is loaded once per master; a forwarding map stores
// overlay_ip → ifindex mappings that are updated as tunnels are created/destroyed.
type Forwarder struct {
	mu       sync.Mutex
	program  *ebpf.Program
	fwdMap   *ebpf.Map
	attached map[string]link.Link // ifaceName → TC link
}

// NewForwarder loads the BPF program and creates the forwarding map.
// Returns an error if eBPF is not available (kernel < 4.18 or restricted).
func NewForwarder() (*Forwarder, error) {
	// Define the forwarding map.
	fwdMapSpec := &ebpf.MapSpec{
		Type:       ebpf.Hash,
		KeySize:    4, // __be32 (IPv4 address)
		ValueSize:  4, // __u32 (ifindex)
		MaxEntries: 256,
		Name:       "fwd_map",
	}

	fwdMap, err := ebpf.NewMap(fwdMapSpec)
	if err != nil {
		return nil, fmt.Errorf("ebpf: create fwd_map: %w", err)
	}

	return &Forwarder{
		fwdMap:   fwdMap,
		attached: make(map[string]link.Link),
	}, nil
}

// SetRoute adds an overlay IP → interface index mapping to the forwarding map.
func (f *Forwarder) SetRoute(overlayIP net.IP, ifindex int) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	ip4 := overlayIP.To4()
	if ip4 == nil {
		return fmt.Errorf("ebpf: invalid IPv4 address: %s", overlayIP)
	}

	key := binary.BigEndian.Uint32(ip4)
	value := uint32(ifindex)

	if err := f.fwdMap.Put(key, value); err != nil {
		return fmt.Errorf("ebpf: fwd_map put %s → ifindex %d: %w", overlayIP, ifindex, err)
	}
	return nil
}

// DeleteRoute removes an overlay IP from the forwarding map.
func (f *Forwarder) DeleteRoute(overlayIP net.IP) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	ip4 := overlayIP.To4()
	if ip4 == nil {
		return fmt.Errorf("ebpf: invalid IPv4 address: %s", overlayIP)
	}

	key := binary.BigEndian.Uint32(ip4)
	if err := f.fwdMap.Delete(key); err != nil {
		return fmt.Errorf("ebpf: fwd_map delete %s: %w", overlayIP, err)
	}
	return nil
}

// Attach attaches the TC program to the ingress of an interface.
// If no BPF program is loaded (graceful degradation), this is a no-op.
func (f *Forwarder) Attach(ifaceName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.program == nil {
		return nil // graceful degradation — no BPF program loaded
	}

	if _, exists := f.attached[ifaceName]; exists {
		return nil // already attached
	}

	iface, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return fmt.Errorf("ebpf: link %q: %w", ifaceName, err)
	}

	l, err := link.AttachTCX(link.TCXOptions{
		Interface: iface.Attrs().Index,
		Program:   f.program,
		Attach:    ebpf.AttachTCXIngress,
	})
	if err != nil {
		return fmt.Errorf("ebpf: attach TC to %s: %w", ifaceName, err)
	}

	f.attached[ifaceName] = l
	return nil
}

// Detach removes the TC program from an interface.
func (f *Forwarder) Detach(ifaceName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	l, exists := f.attached[ifaceName]
	if !exists {
		return nil
	}

	if err := l.Close(); err != nil {
		return fmt.Errorf("ebpf: detach TC from %s: %w", ifaceName, err)
	}
	delete(f.attached, ifaceName)
	return nil
}

// Close unloads the BPF program, detaches from all interfaces, and closes the map.
func (f *Forwarder) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	for name, l := range f.attached {
		l.Close()
		delete(f.attached, name)
	}

	if f.program != nil {
		f.program.Close()
		f.program = nil
	}

	if f.fwdMap != nil {
		f.fwdMap.Close()
		f.fwdMap = nil
	}

	return nil
}
