package wg

import "errors"

// vanillaTransport implements Transport over standard WireGuard (no header
// obfuscation). Used on master's client-facing listener for Mikrotik native
// vanilla-WG compatibility per F-009 D-13.
//
// CR-001: stub — daemon implementation lands in CR-004 (master protocol bridge).
// The skeleton here proves the Transport interface compiles against a vanilla
// implementation; full kernel/userspace WG wiring is wired up in CR-004.
type vanillaTransport struct {
	name string
}

// NewVanillaTransport returns a Transport implementation for vanilla
// WireGuard. CR-001: returns an error — full constructor lands in CR-004.
func NewVanillaTransport(name string) (Transport, error) {
	return &vanillaTransport{name: name}, nil
}

// Protocol reports ProtocolVanilla.
func (t *vanillaTransport) Protocol() Protocol { return ProtocolVanilla }

// Name returns the underlying TUN device name.
func (t *vanillaTransport) Name() string { return t.name }

// Configure — CR-001: stub — implemented in CR-004.
func (t *vanillaTransport) Configure(cfg Config) error {
	return errors.New("vanillaTransport.Configure: not implemented in CR-001 — full impl in CR-004")
}

// AddPeer — CR-001: stub — implemented in CR-004.
func (t *vanillaTransport) AddPeer(p PeerConfig) error {
	return errors.New("vanillaTransport.AddPeer: not implemented in CR-001 — full impl in CR-004")
}

// RemovePeer — CR-001: stub — implemented in CR-004.
func (t *vanillaTransport) RemovePeer(key Key) error {
	return errors.New("vanillaTransport.RemovePeer: not implemented in CR-001 — full impl in CR-004")
}

// Stats — CR-001: stub — implemented in CR-004.
func (t *vanillaTransport) Stats() (*Device, error) {
	return nil, errors.New("vanillaTransport.Stats: not implemented in CR-001 — full impl in CR-004")
}

// Close — CR-001: stub — implemented in CR-004.
func (t *vanillaTransport) Close() error { return nil }
