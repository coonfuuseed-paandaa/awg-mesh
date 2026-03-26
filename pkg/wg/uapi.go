package wg

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const defaultUAPISocketDir = "/var/run/amneziawg"

// UAPIClient is a client for the AWG UAPI unix socket.
type UAPIClient struct {
	socketDir string
}

// UAPIOption configures a UAPIClient.
type UAPIOption func(*UAPIClient)

// WithSocketDir configures a custom UAPI socket directory.
func WithSocketDir(dir string) UAPIOption {
	return func(c *UAPIClient) {
		c.socketDir = dir
	}
}

// NewUAPIClient creates a configured UAPI client.
func NewUAPIClient(opts ...UAPIOption) *UAPIClient {
	client := &UAPIClient{socketDir: defaultUAPISocketDir}
	for _, opt := range opts {
		opt(client)
	}
	return client
}

// ConfigureDevice applies cfg to a device via UAPI set.
func (c *UAPIClient) ConfigureDevice(name string, cfg Config) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("device name is required")
	}

	conn, err := net.Dial("unix", filepath.Join(c.socketDir, name+".sock"))
	if err != nil {
		return fmt.Errorf("dial uapi socket: %w", err)
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, "set=1\n"); err != nil {
		return fmt.Errorf("write uapi set header: %w", err)
	}
	if err := writeConfig(conn, cfg); err != nil {
		return fmt.Errorf("write device config: %w", err)
	}
	if _, err := io.WriteString(conn, "\n"); err != nil {
		return fmt.Errorf("write uapi set terminator: %w", err)
	}

	if err := readErrno(conn); err != nil {
		return fmt.Errorf("uapi set failed: %w", err)
	}

	return nil
}

// Device retrieves device state using UAPI get.
func (c *UAPIClient) Device(name string) (*Device, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("device name is required")
	}

	conn, err := net.Dial("unix", filepath.Join(c.socketDir, name+".sock"))
	if err != nil {
		return nil, fmt.Errorf("dial uapi socket: %w", err)
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, "get=1\n\n"); err != nil {
		return nil, fmt.Errorf("write uapi get command: %w", err)
	}

	device, err := parseDevice(conn)
	if err != nil {
		return nil, fmt.Errorf("parse uapi device response: %w", err)
	}
	device.Name = name
	return device, nil
}

func writeConfig(w io.Writer, cfg Config) error {
	if cfg.PrivateKey != nil {
		if err := writeKV(w, "private_key", hexKey(*cfg.PrivateKey)); err != nil {
			return err
		}
	}
	if cfg.ListenPort != nil {
		if err := writeKV(w, "listen_port", strconv.Itoa(*cfg.ListenPort)); err != nil {
			return err
		}
	}
	if cfg.FirewallMark != nil {
		if err := writeKV(w, "fwmark", strconv.Itoa(*cfg.FirewallMark)); err != nil {
			return err
		}
	}
	if cfg.ReplacePeers {
		if err := writeKV(w, "replace_peers", "true"); err != nil {
			return err
		}
	}

	awgIntFields := map[string]*int{
		"jc":   cfg.Jc,
		"jmin": cfg.Jmin,
		"jmax": cfg.Jmax,
		"s1":   cfg.S1,
		"s2":   cfg.S2,
		"s3":   cfg.S3,
		"s4":   cfg.S4,
	}
	for key, val := range awgIntFields {
		if val != nil {
			if err := writeKV(w, key, strconv.Itoa(*val)); err != nil {
				return err
			}
		}
	}

	awgStringFields := map[string]*string{
		"h1": cfg.H1,
		"h2": cfg.H2,
		"h3": cfg.H3,
		"h4": cfg.H4,
		"i1": cfg.I1,
		"i2": cfg.I2,
		"i3": cfg.I3,
		"i4": cfg.I4,
		"i5": cfg.I5,
	}
	for key, val := range awgStringFields {
		if val != nil {
			if err := writeKV(w, key, *val); err != nil {
				return err
			}
		}
	}

	for i, peer := range cfg.Peers {
		if peer.PublicKey.IsZero() {
			return fmt.Errorf("peer[%d] public key is required", i)
		}
		if err := writeKV(w, "public_key", hexKey(peer.PublicKey)); err != nil {
			return err
		}
		if peer.Remove {
			if err := writeKV(w, "remove", "true"); err != nil {
				return err
			}
		}
		if peer.UpdateOnly {
			if err := writeKV(w, "update_only", "true"); err != nil {
				return err
			}
		}
		if peer.PresharedKey != nil {
			if err := writeKV(w, "preshared_key", hexKey(*peer.PresharedKey)); err != nil {
				return err
			}
		}
		if peer.Endpoint != nil {
			if err := writeKV(w, "endpoint", peer.Endpoint.String()); err != nil {
				return err
			}
		}
		if peer.PersistentKeepaliveInterval != nil {
			seconds := int(peer.PersistentKeepaliveInterval.Seconds())
			if err := writeKV(w, "persistent_keepalive_interval", strconv.Itoa(seconds)); err != nil {
				return err
			}
		}
		if peer.ReplaceAllowedIPs {
			if err := writeKV(w, "replace_allowed_ips", "true"); err != nil {
				return err
			}
		}
		for _, allowedIP := range peer.AllowedIPs {
			if err := writeKV(w, "allowed_ip", allowedIP.String()); err != nil {
				return err
			}
		}
	}

	return nil
}

func parseDevice(r io.Reader) (*Device, error) {
	device := &Device{}
	scanner := bufio.NewScanner(r)

	var currentPeer *Peer
	var seenDevicePublicKey bool
	var handshakeSec int64
	var handshakeNSec int64

	commitPeer := func() {
		if currentPeer == nil {
			return
		}
		if handshakeSec != 0 || handshakeNSec != 0 {
			currentPeer.LastHandshakeTime = time.Unix(handshakeSec, handshakeNSec).UTC()
		}
		device.Peers = append(device.Peers, *currentPeer)
		currentPeer = nil
		handshakeSec = 0
		handshakeNSec = 0
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "errno=") {
			errno, err := strconv.Atoi(strings.TrimPrefix(line, "errno="))
			if err != nil {
				return nil, fmt.Errorf("invalid errno value %q: %w", line, err)
			}
			if errno != 0 {
				return nil, fmt.Errorf("uapi errno=%d", errno)
			}
			commitPeer()
			return device, nil
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("invalid uapi line %q", line)
		}

		switch key {
		case "private_key":
			parsed, err := parseHexKey(value)
			if err != nil {
				return nil, fmt.Errorf("parse private_key: %w", err)
			}
			device.PrivateKey = parsed
		case "public_key":
			parsed, err := parseHexKey(value)
			if err != nil {
				return nil, fmt.Errorf("parse public_key: %w", err)
			}
			if !seenDevicePublicKey {
				device.PublicKey = parsed
				seenDevicePublicKey = true
				continue
			}
			commitPeer()
			currentPeer = &Peer{PublicKey: parsed}
		case "listen_port":
			port, err := strconv.Atoi(value)
			if err != nil {
				return nil, fmt.Errorf("parse listen_port: %w", err)
			}
			device.ListenPort = port
		case "fwmark":
			fwmark, err := strconv.Atoi(value)
			if err != nil {
				return nil, fmt.Errorf("parse fwmark: %w", err)
			}
			device.FirewallMark = fwmark
		case "protocol_version":
			if currentPeer == nil {
				continue
			}
			version, err := strconv.Atoi(value)
			if err != nil {
				return nil, fmt.Errorf("parse protocol_version: %w", err)
			}
			currentPeer.ProtocolVersion = version
		case "endpoint":
			if currentPeer == nil {
				continue
			}
			endpoint, err := net.ResolveUDPAddr("udp", value)
			if err != nil {
				return nil, fmt.Errorf("parse endpoint %q: %w", value, err)
			}
			currentPeer.Endpoint = endpoint
		case "persistent_keepalive_interval":
			if currentPeer == nil {
				continue
			}
			seconds, err := strconv.Atoi(value)
			if err != nil {
				return nil, fmt.Errorf("parse persistent_keepalive_interval: %w", err)
			}
			currentPeer.PersistentKeepaliveInterval = time.Duration(seconds) * time.Second
		case "last_handshake_time_sec":
			if currentPeer == nil {
				continue
			}
			sec, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parse last_handshake_time_sec: %w", err)
			}
			handshakeSec = sec
		case "last_handshake_time_nsec":
			if currentPeer == nil {
				continue
			}
			nsec, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parse last_handshake_time_nsec: %w", err)
			}
			handshakeNSec = nsec
		case "rx_bytes":
			if currentPeer == nil {
				continue
			}
			bytes, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parse rx_bytes: %w", err)
			}
			currentPeer.ReceiveBytes = bytes
		case "tx_bytes":
			if currentPeer == nil {
				continue
			}
			bytes, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parse tx_bytes: %w", err)
			}
			currentPeer.TransmitBytes = bytes
		case "allowed_ip":
			if currentPeer == nil {
				continue
			}
			_, allowedIP, err := net.ParseCIDR(value)
			if err != nil {
				return nil, fmt.Errorf("parse allowed_ip %q: %w", value, err)
			}
			currentPeer.AllowedIPs = append(currentPeer.AllowedIPs, *allowedIP)
		case "preshared_key":
			if currentPeer == nil {
				continue
			}
			psk, err := parseHexKey(value)
			if err != nil {
				return nil, fmt.Errorf("parse preshared_key: %w", err)
			}
			currentPeer.PresharedKey = psk
		case "jc", "jmin", "jmax", "s1", "s2", "s3", "s4", "h1", "h2", "h3", "h4", "i1", "i2", "i3", "i4", "i5":
			device.IsAmnezia = true
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan uapi response: %w", err)
	}
	return nil, errors.New("uapi response ended before errno")
}

func hexKey(k Key) string {
	return hex.EncodeToString(k[:])
}

func parseHexKey(s string) (Key, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return Key{}, fmt.Errorf("decode hex key: %w", err)
	}
	key, err := NewKey(raw)
	if err != nil {
		return Key{}, fmt.Errorf("construct key: %w", err)
	}
	return key, nil
}

func writeKV(w io.Writer, key string, value string) error {
	if _, err := io.WriteString(w, key+"="+value+"\n"); err != nil {
		return fmt.Errorf("write %s: %w", key, err)
	}
	return nil
}

func readErrno(r io.Reader) error {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "errno=") {
			continue
		}

		errno, err := strconv.Atoi(strings.TrimPrefix(line, "errno="))
		if err != nil {
			return fmt.Errorf("invalid errno value %q: %w", line, err)
		}
		if errno != 0 {
			return fmt.Errorf("uapi errno=%d", errno)
		}
		return nil
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read uapi response: %w", err)
	}
	return errors.New("uapi response missing errno")
}
