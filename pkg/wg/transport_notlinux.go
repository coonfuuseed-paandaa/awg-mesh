//go:build !linux

package wg

import (
	"fmt"
	"runtime"
)

// NewVanillaTransport returns a clear unsupported error on platforms without AWG UAPI support.
func NewVanillaTransport(name string) (Transport, error) {
	return nil, fmt.Errorf("vanilla transport %q is unsupported on %s", name, runtime.GOOS)
}

// NewAWGTransport returns a clear unsupported error on platforms without AWG UAPI support.
func NewAWGTransport(name string) (Transport, error) {
	return nil, fmt.Errorf("amneziawg transport %q is unsupported on %s", name, runtime.GOOS)
}
