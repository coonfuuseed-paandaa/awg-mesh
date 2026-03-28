//go:build !linux

package node

import "log"

// AssignOverlayIP is a no-op on non-Linux platforms.
func AssignOverlayIP(ip string) error {
	log.Printf("warn: AssignOverlayIP: not implemented on this platform (ip=%s)", ip)
	return nil
}

// RemoveOverlayIP is a no-op on non-Linux platforms.
func RemoveOverlayIP(ip string) error {
	log.Printf("warn: RemoveOverlayIP: not implemented on this platform (ip=%s)", ip)
	return nil
}
