package wg

import "errors"

// awgTransport implements Transport over AmneziaWG (vanilla WG with S/H/I/J
// header obfuscation). Used on every mesh-internal link per F-009.
//
// CR-001: stub — daemon implementation lands in CR-004 (master) and CR-003
// (clientd) which both consume the Transport interface. The skeleton here
// proves the Transport interface compiles against an AWG implementation.
type awgTransport struct {
	name string
}

// NewAWGTransport returns a Transport implementation for AmneziaWG.
// CR-001: stub constructor — full impl in CR-004 / CR-003.
func NewAWGTransport(name string) (Transport, error) {
	return &awgTransport{name: name}, nil
}

// Protocol reports ProtocolAmneziaWG.
func (t *awgTransport) Protocol() Protocol { return ProtocolAmneziaWG }

// Name returns the underlying TUN device name.
func (t *awgTransport) Name() string { return t.name }

// Configure — CR-001: stub — implemented in CR-004/CR-003.
func (t *awgTransport) Configure(cfg Config) error {
	return errors.New("awgTransport.Configure: not implemented in CR-001 — full impl in CR-004")
}

// AddPeer — CR-001: stub — implemented in CR-004/CR-003.
func (t *awgTransport) AddPeer(p PeerConfig) error {
	return errors.New("awgTransport.AddPeer: not implemented in CR-001 — full impl in CR-004")
}

// RemovePeer — CR-001: stub — implemented in CR-004/CR-003.
func (t *awgTransport) RemovePeer(key Key) error {
	return errors.New("awgTransport.RemovePeer: not implemented in CR-001 — full impl in CR-004")
}

// Stats — CR-001: stub — implemented in CR-004/CR-003.
func (t *awgTransport) Stats() (*Device, error) {
	return nil, errors.New("awgTransport.Stats: not implemented in CR-001 — full impl in CR-004")
}

// Close — CR-001: stub — implemented in CR-004/CR-003.
func (t *awgTransport) Close() error { return nil }
