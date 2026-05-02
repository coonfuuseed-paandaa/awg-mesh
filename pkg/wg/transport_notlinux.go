//go:build !linux

package wg

import (
	"fmt"
	"runtime"
)

type vanillaTransport struct {
	name string
}

type awgTransport struct {
	name string
}

// NewVanillaTransport returns a clear unsupported error on platforms without AWG UAPI support.
func NewVanillaTransport(name string) (Transport, error) {
	return nil, fmt.Errorf("vanilla transport %q is unsupported on %s", name, runtime.GOOS)
}

// NewAWGTransport returns a clear unsupported error on platforms without AWG UAPI support.
func NewAWGTransport(name string) (Transport, error) {
	return nil, fmt.Errorf("amneziawg transport %q is unsupported on %s", name, runtime.GOOS)
}

func (t *vanillaTransport) Protocol() Protocol { return ProtocolVanilla }
func (t *vanillaTransport) Name() string       { return t.name }
func (t *vanillaTransport) Configure(Config) error {
	return fmt.Errorf("vanilla transport %q is unsupported on %s", t.name, runtime.GOOS)
}
func (t *vanillaTransport) AddPeer(PeerConfig) error {
	return fmt.Errorf("vanilla transport %q is unsupported on %s", t.name, runtime.GOOS)
}
func (t *vanillaTransport) RemovePeer(Key) error {
	return fmt.Errorf("vanilla transport %q is unsupported on %s", t.name, runtime.GOOS)
}
func (t *vanillaTransport) Stats() (*Device, error) {
	return nil, fmt.Errorf("vanilla transport %q is unsupported on %s", t.name, runtime.GOOS)
}
func (t *vanillaTransport) Close() error { return nil }

func (t *awgTransport) Protocol() Protocol { return ProtocolAmneziaWG }
func (t *awgTransport) Name() string       { return t.name }
func (t *awgTransport) Configure(Config) error {
	return fmt.Errorf("amneziawg transport %q is unsupported on %s", t.name, runtime.GOOS)
}
func (t *awgTransport) AddPeer(PeerConfig) error {
	return fmt.Errorf("amneziawg transport %q is unsupported on %s", t.name, runtime.GOOS)
}
func (t *awgTransport) RemovePeer(Key) error {
	return fmt.Errorf("amneziawg transport %q is unsupported on %s", t.name, runtime.GOOS)
}
func (t *awgTransport) Stats() (*Device, error) {
	return nil, fmt.Errorf("amneziawg transport %q is unsupported on %s", t.name, runtime.GOOS)
}
func (t *awgTransport) Close() error { return nil }
