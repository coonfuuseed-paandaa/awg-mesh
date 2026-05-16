//go:build linux && !race

package simulation

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/conn"
	"github.com/amnezia-vpn/amneziawg-go/device"
	"github.com/amnezia-vpn/amneziawg-go/tun/tuntest"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/clientd"
	meshnode "github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/node"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/role"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/wg"
	pb "github.com/coonfuuseed-paandaa/awg-mesh/v2/proto/control_plane"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestF011ClientdStreamSnapshotDrivesAmneziaWGHandshake(t *testing.T) {
	if testing.Short() {
		t.Skip("F-011 handshake simulation opens in-process UDP binds")
	}

	masterIP := netip.MustParseAddr("1.0.0.1")
	clientIP := netip.MustParseAddr("1.0.0.2")

	masterPriv := mustPrivateKey(t)
	clientPriv := mustPrivateKey(t)
	masterPub := masterPriv.PublicKey()
	clientPub := clientPriv.PublicKey()

	masterDevice := newSimulationDevice(t, "sim-master", masterPriv)
	clientDevice := newSimulationDevice(t, "sim-client", clientPriv)
	clientTransport := newSimulationTransport("sim-client", clientDevice.dev)

	configurePeer(t, masterDevice.dev, wg.PeerConfig{
		PublicKey:         clientPub,
		Endpoint:          mustUDPAddr(t, clientDevice.endpoint()),
		ReplaceAllowedIPs: true,
		AllowedIPs:        []net.IPNet{mustIPNet(t, clientIP.String()+"/32")},
	})

	cpClient, cleanupControlPlane := startSimulationMasterCoordination(t, masterPub, masterDevice.endpoint(), masterIP)
	defer cleanupControlPlane()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cachePath := t.TempDir() + "/clientd-state.json"
	staleCache := clientd.State{
		PeerListVersion:  1,
		OwnershipVersion: 1,
		Peers: []clientd.PeerEntry{{
			PeerName:      "master-a",
			PeerOverlayIP: masterIP.String(),
			AllowedIPs:    []string{masterIP.String() + "/32"},
			Protocol:      wg.ProtocolAmneziaWG,
		}},
		Ownership: []clientd.OwnershipEntry{{
			OverlayIP:            masterIP.String(),
			OwningMaster:         "master-a",
			LastReassignedAtUnix: time.Now().Unix(),
			Reason:               "stale-cache",
		}},
	}
	if err := clientd.NewStateCache(cachePath).Save(staleCache); err != nil {
		t.Fatalf("seed stale clientd cache: %v", err)
	}

	agent, err := clientd.NewAgent(clientd.Config{
		NodeName:      "client-a",
		Roles:         []role.Role{role.RoleClient},
		OverlayIP:     clientIP.String(),
		Region:        "home",
		NodeCertPEM:   []byte("client-cert"),
		Version:       "sim",
		InterfaceName: clientTransport.Name(),
		Protocol:      wg.ProtocolAmneziaWG,
		StatePath:     cachePath,
		ApplyTimeout:  2 * time.Second,
	}, cpClient, clientd.TransportConfigurator{
		Transport:  clientTransport,
		LocalRoles: []role.Role{role.RoleClient},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	agentCtx, stopAgent := context.WithCancel(ctx)
	agentDone := make(chan error, 1)
	go func() {
		agentDone <- agent.Run(agentCtx)
	}()
	t.Cleanup(func() {
		stopAgent()
		select {
		case err := <-agentDone:
			if err != nil {
				t.Errorf("clientd agent stopped with error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("clientd agent did not stop within timeout")
		}
	})

	waitForAppliedPeer(t, clientTransport.applied, masterPub, masterDevice.endpoint(), 5*time.Second)
	assertPacketTransit(t, clientDevice.tun, masterDevice.tun, clientIP, masterIP, 5*time.Second)
	assertPeerHandshake(t, clientDevice.dev, masterPub, 5*time.Second)
}

type simulationDevice struct {
	tun  *tuntest.ChannelTUN
	dev  *device.Device
	port int
}

func newSimulationDevice(t *testing.T, name string, privateKey wg.Key) *simulationDevice {
	t.Helper()
	tun := tuntest.NewChannelTUN()
	dev := device.NewDevice(tun.TUN(), conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, name+": "))
	t.Cleanup(dev.Close)

	listenPort := 0
	if err := dev.IpcSet(encodeWGConfig(wg.Config{
		PrivateKey:   &privateKey,
		ListenPort:   &listenPort,
		ReplacePeers: true,
	})); err != nil {
		t.Fatalf("%s initial IpcSet: %v", name, err)
	}
	if err := dev.Up(); err != nil {
		t.Fatalf("%s Up: %v", name, err)
	}
	return &simulationDevice{tun: tun, dev: dev, port: readListenPort(t, dev)}
}

func (d *simulationDevice) endpoint() string {
	return fmt.Sprintf("127.0.0.1:%d", d.port)
}

type simulationTransport struct {
	name    string
	dev     *device.Device
	applied chan wg.Config
	mu      sync.Mutex
}

func newSimulationTransport(name string, dev *device.Device) *simulationTransport {
	return &simulationTransport{name: name, dev: dev, applied: make(chan wg.Config, 8)}
}

func (t *simulationTransport) Protocol() wg.Protocol { return wg.ProtocolAmneziaWG }
func (t *simulationTransport) Name() string          { return t.name }
func (t *simulationTransport) Close() error          { return nil }

func (t *simulationTransport) Configure(cfg wg.Config) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.dev.IpcSet(encodeWGConfig(cfg)); err != nil {
		return err
	}
	select {
	case t.applied <- cloneWGConfig(cfg):
	default:
	}
	return nil
}

func (t *simulationTransport) AddPeer(peer wg.PeerConfig) error {
	return t.Configure(wg.Config{Peers: []wg.PeerConfig{peer}})
}

func (t *simulationTransport) RemovePeer(key wg.Key) error {
	return t.Configure(wg.Config{Peers: []wg.PeerConfig{{PublicKey: key, Remove: true}}})
}

func (t *simulationTransport) Stats() (*wg.Device, error) {
	peers, err := readPeerStats(t.dev)
	if err != nil {
		return nil, err
	}
	out := &wg.Device{Name: t.name}
	for _, peer := range peers {
		key, err := keyFromHex(peer.publicKeyHex)
		if err != nil {
			return nil, err
		}
		out.Peers = append(out.Peers, wg.Peer{
			PublicKey:         key,
			LastHandshakeTime: time.Unix(peer.lastHandshakeSec, 0).UTC(),
		})
	}
	return out, nil
}

type masterCoordinationTransport struct {
	name     string
	protocol wg.Protocol
	pubkey   wg.Key
}

func (t *masterCoordinationTransport) Protocol() wg.Protocol       { return t.protocol }
func (t *masterCoordinationTransport) Name() string                { return t.name }
func (t *masterCoordinationTransport) Configure(wg.Config) error   { return nil }
func (t *masterCoordinationTransport) AddPeer(wg.PeerConfig) error { return nil }
func (t *masterCoordinationTransport) RemovePeer(wg.Key) error     { return nil }
func (t *masterCoordinationTransport) Close() error                { return nil }
func (t *masterCoordinationTransport) Stats() (*wg.Device, error) {
	return &wg.Device{Name: t.name, PublicKey: t.pubkey}, nil
}

func startSimulationMasterCoordination(t *testing.T, pubkey wg.Key, endpoint string, overlayIP netip.Addr) (pb.ControlPlaneClient, func()) {
	t.Helper()
	master, err := meshnode.NewMaster(meshnode.MasterConfig{
		Name:             "master-a",
		OverlayIP:        overlayIP.String(),
		MeshEndpointHost: endpoint,
		DualListener: wg.DualListenerConfig{
			VanillaFactory: func(name string) (wg.Transport, error) {
				return &masterCoordinationTransport{name: name, protocol: wg.ProtocolVanilla}, nil
			},
			AWGFactory: func(name string) (wg.Transport, error) {
				return &masterCoordinationTransport{name: name, protocol: wg.ProtocolAmneziaWG, pubkey: pubkey}, nil
			},
		},
		Coordination: &meshnode.MasterCoordinationConfig{
			ListenAddr: "127.0.0.1:0",
			StateDir:   t.TempDir(),
		},
	})
	if err != nil {
		t.Fatalf("NewMaster: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- master.Run(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status := master.Status().Coordination
		if status.Enabled && status.Started && status.BoundAddr != "" {
			conn, err := grpc.NewClient(status.BoundAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				cancel()
				t.Fatalf("dial master coordination: %v", err)
			}
			return pb.NewControlPlaneClient(conn), func() {
				_ = conn.Close()
				cancel()
				select {
				case err := <-runErr:
					if err != nil {
						t.Errorf("master coordination Run: %v", err)
					}
				case <-time.After(2 * time.Second):
					t.Errorf("master coordination Run did not stop")
				}
			}
		}
		select {
		case err := <-runErr:
			t.Fatalf("master coordination stopped before ready: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	t.Fatal("master coordination did not become ready")
	return nil, nil
}

func configurePeer(t *testing.T, dev *device.Device, peer wg.PeerConfig) {
	t.Helper()
	if err := dev.IpcSet(encodeWGConfig(wg.Config{Peers: []wg.PeerConfig{peer}})); err != nil {
		t.Fatalf("configure peer: %v", err)
	}
}

func waitForAppliedPeer(t *testing.T, applied <-chan wg.Config, want wg.Key, wantEndpoint string, timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case cfg := <-applied:
			for _, peer := range cfg.Peers {
				if peer.PublicKey == want {
					if peer.Endpoint == nil || peer.Endpoint.String() != wantEndpoint {
						t.Fatalf("REGRESSION: applied peer endpoint = %v, want %s", peer.Endpoint, wantEndpoint)
					}
					return
				}
			}
		case <-timer.C:
			t.Fatalf("clientd did not apply peer %s within %s", want.String(), timeout)
		}
	}
}

func assertPacketTransit(t *testing.T, src, dst *tuntest.ChannelTUN, srcIP, dstIP netip.Addr, timeout time.Duration) {
	t.Helper()
	want := tuntest.Ping(dstIP, srcIP)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		src.Outbound <- want
		select {
		case got := <-dst.Inbound:
			if bytes.Equal(got, want) {
				return
			}
		case <-time.After(250 * time.Millisecond):
		}
	}
	t.Fatalf("packet did not transit from %s to %s within %s", srcIP, dstIP, timeout)
}

func assertPeerHandshake(t *testing.T, dev *device.Device, want wg.Key, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		peers, err := readPeerStats(dev)
		if err != nil {
			t.Fatalf("read peer stats: %v", err)
		}
		for _, peer := range peers {
			if peer.publicKeyHex == hexKey(want) && peer.lastHandshakeSec > 0 {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	raw, _ := dev.IpcGet()
	t.Fatalf("peer %s did not report a non-zero handshake within %s\n%s", want.String(), timeout, raw)
}

func readListenPort(t *testing.T, dev *device.Device) int {
	t.Helper()
	raw, err := dev.IpcGet()
	if err != nil {
		t.Fatalf("IpcGet listen port: %v", err)
	}
	for _, line := range strings.Split(raw, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "listen_port="); ok {
			port, err := strconv.Atoi(value)
			if err != nil {
				t.Fatalf("parse listen_port %q: %v", value, err)
			}
			if port <= 0 {
				t.Fatalf("listen_port = %d, want assigned UDP port", port)
			}
			return port
		}
	}
	t.Fatalf("listen_port missing from IpcGet:\n%s", raw)
	return 0
}

type peerStats struct {
	publicKeyHex     string
	lastHandshakeSec int64
}

func readPeerStats(dev *device.Device) ([]peerStats, error) {
	raw, err := dev.IpcGet()
	if err != nil {
		return nil, err
	}
	var peers []peerStats
	var current *peerStats
	commit := func() {
		if current != nil {
			peers = append(peers, *current)
			current = nil
		}
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if value, ok := strings.CutPrefix(line, "public_key="); ok {
			commit()
			current = &peerStats{publicKeyHex: value}
			continue
		}
		if current == nil {
			continue
		}
		if value, ok := strings.CutPrefix(line, "last_handshake_time_sec="); ok {
			sec, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parse last_handshake_time_sec %q: %w", value, err)
			}
			current.lastHandshakeSec = sec
		}
	}
	commit()
	return peers, nil
}

func encodeWGConfig(cfg wg.Config) string {
	var b strings.Builder
	if cfg.PrivateKey != nil {
		writeUAPIKV(&b, "private_key", hexKey(*cfg.PrivateKey))
	}
	if cfg.ListenPort != nil {
		writeUAPIKV(&b, "listen_port", strconv.Itoa(*cfg.ListenPort))
	}
	if cfg.ReplacePeers {
		writeUAPIKV(&b, "replace_peers", "true")
	}
	for _, peer := range cfg.Peers {
		writeUAPIKV(&b, "public_key", hexKey(peer.PublicKey))
		if peer.Remove {
			writeUAPIKV(&b, "remove", "true")
		}
		if peer.Endpoint != nil {
			writeUAPIKV(&b, "endpoint", peer.Endpoint.String())
		}
		if peer.ReplaceAllowedIPs {
			writeUAPIKV(&b, "replace_allowed_ips", "true")
		}
		for _, allowed := range peer.AllowedIPs {
			writeUAPIKV(&b, "allowed_ip", allowed.String())
		}
		if peer.PersistentKeepaliveInterval != nil {
			writeUAPIKV(&b, "persistent_keepalive_interval", strconv.Itoa(int(peer.PersistentKeepaliveInterval.Seconds())))
		}
	}
	return b.String()
}

func writeUAPIKV(b *strings.Builder, key, value string) {
	b.WriteString(key)
	b.WriteByte('=')
	b.WriteString(value)
	b.WriteByte('\n')
}

func cloneWGConfig(cfg wg.Config) wg.Config {
	out := cfg
	out.Peers = append([]wg.PeerConfig(nil), cfg.Peers...)
	for i := range out.Peers {
		out.Peers[i].AllowedIPs = append([]net.IPNet(nil), cfg.Peers[i].AllowedIPs...)
	}
	return out
}

func mustPrivateKey(t *testing.T) wg.Key {
	t.Helper()
	key, err := wg.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey: %v", err)
	}
	return key
}

func mustUDPAddr(t *testing.T, value string) *net.UDPAddr {
	t.Helper()
	addr, err := net.ResolveUDPAddr("udp", value)
	if err != nil {
		t.Fatalf("ResolveUDPAddr(%q): %v", value, err)
	}
	return addr
}

func mustIPNet(t *testing.T, cidr string) net.IPNet {
	t.Helper()
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", cidr, err)
	}
	return *ipNet
}

func keyFromHex(value string) (wg.Key, error) {
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return wg.Key{}, err
	}
	return wg.NewKey(decoded)
}

func hexKey(key wg.Key) string {
	return hex.EncodeToString(key[:])
}
