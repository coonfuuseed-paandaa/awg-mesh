//go:build linux

package wg

// awgTransport implements Transport over AmneziaWG (vanilla WG with S/H/I/J
// header obfuscation). Used on every mesh-internal link per F-009.
type awgTransport struct {
	name  string
	iface *Interface
}

// NewAWGTransport returns a Transport implementation for AmneziaWG.
func NewAWGTransport(name string) (Transport, error) {
	iface, err := newTransportInterface(name)
	if err != nil {
		return nil, err
	}
	return &awgTransport{name: name, iface: iface}, nil
}

// Protocol reports ProtocolAmneziaWG.
func (t *awgTransport) Protocol() Protocol { return ProtocolAmneziaWG }

// Name returns the underlying TUN device name.
func (t *awgTransport) Name() string { return t.name }

// Configure applies a device-level configuration through the UAPI-backed Interface.
func (t *awgTransport) Configure(cfg Config) error {
	return t.iface.Configure(cfg)
}

// AddPeer adds or updates one peer through UAPI.
func (t *awgTransport) AddPeer(p PeerConfig) error {
	return t.iface.Configure(Config{Peers: []PeerConfig{p}, ReplacePeers: false})
}

// RemovePeer removes one peer through UAPI.
func (t *awgTransport) RemovePeer(key Key) error {
	return t.iface.Configure(Config{Peers: []PeerConfig{{PublicKey: key, Remove: true}}, ReplacePeers: false})
}

// Stats returns current device state through UAPI.
func (t *awgTransport) Stats() (*Device, error) {
	return t.iface.GetDevice()
}

// Close releases interface resources.
func (t *awgTransport) Close() error {
	if t == nil || t.iface == nil {
		return nil
	}
	return t.iface.Close()
}
