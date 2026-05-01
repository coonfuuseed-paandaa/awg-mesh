//go:build linux

package node

import (
	"fmt"
	"net"
	"syscall"

	"golang.org/x/net/icmp"
	"golang.org/x/sys/unix"
)

// bindICMPSocketToVRF applies SO_BINDTODEVICE=<vrfName> to the underlying
// raw socket so subsequent send/recv resolves routes via the named VRF master
// device. F-008 FR-7 — required when transport peer IPs live in a VRF table
// after wg-* iface enslavement.
//
// Implementation note: icmp.PacketConn.IPv4PacketConn().PacketConn is a
// net.PacketConn FIELD (not method) holding the underlying *net.IPConn; we
// type-assert to *net.IPConn to access SyscallConn().
func bindICMPSocketToVRF(conn *icmp.PacketConn, vrfName string) error {
	ipv4Conn := conn.IPv4PacketConn()
	if ipv4Conn == nil {
		return fmt.Errorf("icmp socket has no IPv4PacketConn")
	}
	inner := ipv4Conn.PacketConn
	ipConn, ok := inner.(*net.IPConn)
	if !ok {
		return fmt.Errorf("icmp inner conn type %T, expected *net.IPConn", inner)
	}
	var sc syscall.RawConn
	sc, err := ipConn.SyscallConn()
	if err != nil {
		return fmt.Errorf("syscall conn: %w", err)
	}
	var setErr error
	ctrlErr := sc.Control(func(fd uintptr) {
		setErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, vrfName)
	})
	if ctrlErr != nil {
		return fmt.Errorf("control fd: %w", ctrlErr)
	}
	if setErr != nil {
		return fmt.Errorf("setsockopt SO_BINDTODEVICE=%q: %w", vrfName, setErr)
	}
	return nil
}
