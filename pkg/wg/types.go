package wg

import (
	"net"
	"time"
)

// Key is a 32-byte WireGuard/AWG key.
type Key [32]byte

// Device describes a WireGuard/AWG device state.
type Device struct {
	Name         string
	PrivateKey   Key
	PublicKey    Key
	ListenPort   int
	FirewallMark int
	IsAmnezia    bool
	Peers        []Peer
}

// Peer describes runtime peer state.
type Peer struct {
	PublicKey                   Key
	PresharedKey                Key
	Endpoint                    *net.UDPAddr
	PersistentKeepaliveInterval time.Duration
	LastHandshakeTime           time.Time
	ReceiveBytes                int64
	TransmitBytes               int64
	AllowedIPs                  []net.IPNet
	ProtocolVersion             int
}

// Config describes device configuration changes.
type Config struct {
	PrivateKey   *Key
	ListenPort   *int
	FirewallMark *int
	ReplacePeers bool
	Peers        []PeerConfig

	Jc   *int
	Jmin *int
	Jmax *int
	S1   *int
	S2   *int
	S3   *int
	S4   *int
	H1   *string
	H2   *string
	H3   *string
	H4   *string
	I1   *string
	I2   *string
	I3   *string
	I4   *string
	I5   *string
}

// PeerConfig describes a peer configuration change.
type PeerConfig struct {
	PublicKey                   Key
	Remove                      bool
	UpdateOnly                  bool
	PresharedKey                *Key
	Endpoint                    *net.UDPAddr
	PersistentKeepaliveInterval *time.Duration
	ReplaceAllowedIPs           bool
	AllowedIPs                  []net.IPNet
}

// IntPtr returns a pointer to v.
func IntPtr(v int) *int {
	return &v
}

// StrPtr returns a pointer to v.
func StrPtr(v string) *string {
	return &v
}
