//go:build !linux

package ebpf

import "errors"

var errNotSupported = errors.New("ebpf: not supported on this platform")

// Forwarder manages eBPF TC programs for inter-interface packet forwarding.
type Forwarder struct{}

// NewForwarder returns an error on non-Linux platforms.
func NewForwarder() (*Forwarder, error) { return nil, errNotSupported }

// Attach is not supported on non-Linux platforms.
func (f *Forwarder) Attach(ifaceName string) error { return errNotSupported }

// Detach is not supported on non-Linux platforms.
func (f *Forwarder) Detach(ifaceName string) error { return errNotSupported }

// SetRoute is not supported on non-Linux platforms.
func (f *Forwarder) SetRoute(overlayIP [4]byte, ifindex uint32) error { return errNotSupported }

// DeleteRoute is not supported on non-Linux platforms.
func (f *Forwarder) DeleteRoute(overlayIP [4]byte) error { return errNotSupported }

// Close is not supported on non-Linux platforms.
func (f *Forwarder) Close() error { return nil }
