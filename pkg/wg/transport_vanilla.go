//go:build linux

package wg

// vanillaTransport implements Transport over standard WireGuard (no header
// obfuscation). Used on master's client-facing listener for Mikrotik native
// vanilla-WG compatibility per F-009 D-13.
type vanillaTransport struct {
	name  string
	iface *Interface
}

// NewVanillaTransport returns a Transport implementation for vanilla WireGuard.
func NewVanillaTransport(name string) (Transport, error) {
	iface, err := newTransportInterface(name)
	if err != nil {
		return nil, err
	}
	return &vanillaTransport{name: name, iface: iface}, nil
}

// Protocol reports ProtocolVanilla.
func (t *vanillaTransport) Protocol() Protocol { return ProtocolVanilla }

// Name returns the underlying TUN device name.
func (t *vanillaTransport) Name() string { return t.name }

// Configure applies a device-level configuration through the UAPI-backed Interface.
func (t *vanillaTransport) Configure(cfg Config) error {
	return t.iface.Configure(cfg)
}

// AddPeer adds or updates one peer through UAPI.
func (t *vanillaTransport) AddPeer(p PeerConfig) error {
	return t.iface.Configure(Config{Peers: []PeerConfig{p}, ReplacePeers: false})
}

// RemovePeer removes one peer through UAPI.
func (t *vanillaTransport) RemovePeer(key Key) error {
	return t.iface.Configure(Config{Peers: []PeerConfig{{PublicKey: key, Remove: true}}, ReplacePeers: false})
}

// Stats returns current device state through UAPI.
func (t *vanillaTransport) Stats() (*Device, error) {
	return t.iface.GetDevice()
}

// Close releases interface resources.
func (t *vanillaTransport) Close() error {
	if t == nil || t.iface == nil {
		return nil
	}
	return t.iface.Close()
}
