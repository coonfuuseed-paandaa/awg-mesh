//go:build linux

package wg

import "fmt"

const defaultTransportMTU = 1420

func newTransportInterface(name string) (*Interface, error) {
	iface, createErr := NewInterface(name, defaultTransportMTU, nil)
	if createErr == nil {
		return iface, nil
	}
	iface, openErr := OpenExistingInterface(name)
	if openErr == nil {
		return iface, nil
	}
	return nil, fmt.Errorf("open transport interface %q: create failed: %w; open existing failed: %v", name, createErr, openErr)
}
