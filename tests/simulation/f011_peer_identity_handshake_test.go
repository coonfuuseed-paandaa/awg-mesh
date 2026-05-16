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
	clientDevice := newUnconfiguredSimulationDevice(t, "sim-client")
	clientTransport := newSimulationTransport("sim-client", clientDevice.dev)
	clientLink := &simulationLinkConfigurator{}
	masterLink := &simulationLinkConfigurator{}

	cpClient, cleanupControlPlane := startSimulationMasterCoordination(t, masterDevice, masterPriv, masterDevice.endpoint(), masterIP, masterLink)
	defer cleanupControlPlane()
	assertMasterLinks(t, masterLink, wg.DefaultMeshInterfaceName, wg.DefaultClientInterfaceName, masterIP.String()+"/32")

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

	clientOverlay := mustIPNet(t, clientIP.String()+"/32")
	agent, err := clientd.NewAgent(clientd.Config{
		NodeName:      "egress-a",
		Roles:         []role.Role{role.RoleEgress},
		OverlayIP:     clientIP.String(),
		Region:        "home",
		NodeCertPEM:   []byte("client-cert"),
		Version:       "sim",
		InterfaceName: clientTransport.Name(),
		Protocol:      wg.ProtocolAmneziaWG,
		PublicKey:     clientPub,
		StatePath:     cachePath,
		ApplyTimeout:  2 * time.Second,
	}, cpClient, clientd.TransportConfigurator{
		Transport:        clientTransport,
		LocalRoles:       []role.Role{role.RoleEgress},
		PrivateKey:       &clientPriv,
		OverlayAddress:   &clientOverlay,
		LinkConfigurator: clientLink,
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
	waitForPeerRoute(t, masterLink, wg.DefaultMeshInterfaceName, clientIP.String()+"/32", masterIP.String(), 5*time.Second)
	waitForPeerRoute(t, clientLink, clientTransport.Name(), masterIP.String()+"/32", clientIP.String(), 5*time.Second)
	assertPacketTransit(t, clientDevice.tun, masterDevice.tun, clientIP, masterIP, 5*time.Second)
	assertPacketTransit(t, masterDevice.tun, clientDevice.tun, masterIP, clientIP, 5*time.Second)
	assertFreshPeerHandshake(t, clientDevice.dev, masterPub, 130*time.Second, 5*time.Second)
	assertFreshPeerHandshake(t, masterDevice.dev, clientPub, 130*time.Second, 5*time.Second)
}

type simulationDevice struct {
	tun  *tuntest.ChannelTUN
	dev  *device.Device
	port int
}

func newSimulationDevice(t *testing.T, name string, privateKey wg.Key) *simulationDevice {
	t.Helper()
	simDevice := newUnconfiguredSimulationDevice(t, name)

	listenPort := 0
	if err := simDevice.dev.IpcSet(encodeWGConfig(wg.Config{
		PrivateKey:   &privateKey,
		ListenPort:   &listenPort,
		ReplacePeers: true,
	})); err != nil {
		t.Fatalf("%s initial IpcSet: %v", name, err)
	}
	if err := simDevice.dev.Up(); err != nil {
		t.Fatalf("%s Up: %v", name, err)
	}
	simDevice.port = readListenPort(t, simDevice.dev)
	return simDevice
}

func newUnconfiguredSimulationDevice(t *testing.T, name string) *simulationDevice {
	t.Helper()
	tun := tuntest.NewChannelTUN()
	dev := device.NewDevice(tun.TUN(), conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, name+": "))
	t.Cleanup(dev.Close)
	return &simulationDevice{tun: tun, dev: dev}
}

func (d *simulationDevice) endpoint() string {
	return fmt.Sprintf("127.0.0.1:%d", d.port)
}

type simulationTransport struct {
	name    string
	dev     *device.Device
	applied chan wg.Config
	pubkey  wg.Key
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
	if err := t.dev.Up(); err != nil {
		return err
	}
	if cfg.PrivateKey != nil {
		t.pubkey = cfg.PrivateKey.PublicKey()
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
	t.mu.Lock()
	pubkey := t.pubkey
	t.mu.Unlock()

	peers, err := readPeerStats(t.dev)
	if err != nil {
		return nil, err
	}
	out := &wg.Device{Name: t.name, PublicKey: pubkey}
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

type simulationLinkConfigurator struct {
	mu     sync.Mutex
	addrs  []simulationAddress
	upIfcs []string
	routes []simulationRoute
}

type simulationAddress struct {
	iface string
	addr  string
}

type simulationRoute struct {
	iface string
	dest  string
	src   string
}

func (l *simulationLinkConfigurator) AddrAdd(ifaceName string, addr *net.IPNet) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.addrs = append(l.addrs, simulationAddress{iface: ifaceName, addr: addr.String()})
	return nil
}

func (l *simulationLinkConfigurator) LinkSetUp(ifaceName string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.upIfcs = append(l.upIfcs, ifaceName)
	return nil
}

func (l *simulationLinkConfigurator) RouteReplaceLinkWithSrc(dest *net.IPNet, ifaceName string, src net.IP) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	srcText := ""
	if src != nil {
		srcText = src.String()
	}
	l.routes = append(l.routes, simulationRoute{iface: ifaceName, dest: dest.String(), src: srcText})
	return nil
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

func startSimulationMasterCoordination(t *testing.T, masterDevice *simulationDevice, privateKey wg.Key, endpoint string, overlayIP netip.Addr, linkConfigurator meshnode.MasterLinkConfigurator) (pb.ControlPlaneClient, func()) {
	t.Helper()
	masterTransport := newSimulationTransport("sim-master", masterDevice.dev)
	master, err := meshnode.NewMaster(meshnode.MasterConfig{
		Name:             "master-a",
		OverlayIP:        overlayIP.String(),
		MeshEndpointHost: endpoint,
		LinkConfigurator: linkConfigurator,
		DualListener: wg.DualListenerConfig{
			MeshPrivateKey: &privateKey,
			MeshListenPort: masterDevice.port,
			VanillaFactory: func(name string) (wg.Transport, error) {
				return &masterCoordinationTransport{name: name, protocol: wg.ProtocolVanilla}, nil
			},
			AWGFactory: func(name string) (wg.Transport, error) {
				return masterTransport, nil
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

func assertMasterLinks(t *testing.T, link *simulationLinkConfigurator, wantMeshIface, wantClientIface, wantOverlay string) {
	t.Helper()
	link.mu.Lock()
	defer link.mu.Unlock()
	if len(link.addrs) != 1 || link.addrs[0] != (simulationAddress{iface: wantMeshIface, addr: wantOverlay}) {
		t.Fatalf("master link AddrAdd calls = %#v, want %s %s", link.addrs, wantMeshIface, wantOverlay)
	}
	wantUp := []string{wantMeshIface, wantClientIface}
	if len(link.upIfcs) != len(wantUp) {
		t.Fatalf("master link LinkSetUp calls = %#v, want %#v", link.upIfcs, wantUp)
	}
	for i := range wantUp {
		if link.upIfcs[i] != wantUp[i] {
			t.Fatalf("master link LinkSetUp calls = %#v, want %#v", link.upIfcs, wantUp)
		}
	}
}

func waitForPeerRoute(t *testing.T, link *simulationLinkConfigurator, wantIface, wantDest, wantSrc string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		link.mu.Lock()
		for _, route := range link.routes {
			if route.iface == wantIface && route.dest == wantDest && route.src == wantSrc {
				link.mu.Unlock()
				return
			}
		}
		routes := append([]simulationRoute(nil), link.routes...)
		link.mu.Unlock()
		if time.Now().Add(100 * time.Millisecond).After(deadline) {
			t.Fatalf("clientd did not install peer route dev=%s dest=%s src=%s within %s; routes=%#v", wantIface, wantDest, wantSrc, timeout, routes)
		}
		time.Sleep(100 * time.Millisecond)
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

func assertFreshPeerHandshake(t *testing.T, dev *device.Device, want wg.Key, maxAge, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		peers, err := readPeerStats(dev)
		if err != nil {
			t.Fatalf("read peer stats: %v", err)
		}
		for _, peer := range peers {
			if peer.publicKeyHex != hexKey(want) || peer.lastHandshakeSec <= 0 {
				continue
			}
			age := time.Since(time.Unix(peer.lastHandshakeSec, 0))
			if age <= maxAge {
				return
			}
			t.Fatalf("peer %s latest handshake age = %s, want <= %s", want.String(), age, maxAge)
		}
		time.Sleep(100 * time.Millisecond)
	}
	raw, _ := dev.IpcGet()
	t.Fatalf("peer %s did not report a fresh handshake within %s\n%s", want.String(), timeout, raw)
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
	for _, kv := range []struct {
		key string
		val *int
	}{
		{"jc", cfg.Jc},
		{"jmin", cfg.Jmin},
		{"jmax", cfg.Jmax},
		{"s1", cfg.S1},
		{"s2", cfg.S2},
	} {
		if kv.val != nil {
			writeUAPIKV(&b, kv.key, strconv.Itoa(*kv.val))
		}
	}
	for _, kv := range []struct {
		key string
		val *string
	}{
		{"h1", cfg.H1},
		{"h2", cfg.H2},
		{"h3", cfg.H3},
		{"h4", cfg.H4},
		{"i1", cfg.I1},
		{"i2", cfg.I2},
		{"i3", cfg.I3},
		{"i4", cfg.I4},
		{"i5", cfg.I5},
	} {
		if kv.val != nil {
			writeUAPIKV(&b, kv.key, *kv.val)
		}
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
