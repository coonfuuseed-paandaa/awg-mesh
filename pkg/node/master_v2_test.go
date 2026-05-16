package node

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/control_plane"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/role"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/wg"
)

type fakeMasterTransport struct {
	name       string
	protocol   wg.Protocol
	pubkey     wg.Key
	statsStart chan struct{}
	statsBlock chan struct{}
	statsOnce  sync.Once
	closeCount int
	peers      []wg.PeerConfig
}

func (t *fakeMasterTransport) Protocol() wg.Protocol     { return t.protocol }
func (t *fakeMasterTransport) Name() string              { return t.name }
func (t *fakeMasterTransport) Configure(wg.Config) error { return nil }
func (t *fakeMasterTransport) AddPeer(peer wg.PeerConfig) error {
	t.peers = append(t.peers, peer)
	return nil
}
func (t *fakeMasterTransport) RemovePeer(wg.Key) error { return nil }
func (t *fakeMasterTransport) Stats() (*wg.Device, error) {
	if t.statsStart != nil {
		t.statsOnce.Do(func() { close(t.statsStart) })
	}
	if t.statsBlock != nil {
		<-t.statsBlock
	}
	return &wg.Device{Name: t.name, PublicKey: t.pubkey}, nil
}
func (t *fakeMasterTransport) Close() error {
	t.closeCount++
	return nil
}

type fakeMasterLinkConfigurator struct {
	mu       sync.Mutex
	addrs    []fakeMasterLinkAddress
	upIfcs   []string
	routes   []fakeMasterLinkRoute
	addrErr  error
	upErr    error
	routeErr error
}

type fakeMasterLinkAddress struct {
	iface string
	addr  string
}

type fakeMasterLinkRoute struct {
	iface string
	dest  string
	src   string
}

func (l *fakeMasterLinkConfigurator) AddrAdd(ifaceName string, addr *net.IPNet) error {
	if l.addrErr != nil {
		return l.addrErr
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.addrs = append(l.addrs, fakeMasterLinkAddress{iface: ifaceName, addr: addr.String()})
	return nil
}

func (l *fakeMasterLinkConfigurator) LinkSetUp(ifaceName string) error {
	if l.upErr != nil {
		return l.upErr
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.upIfcs = append(l.upIfcs, ifaceName)
	return nil
}

func (l *fakeMasterLinkConfigurator) RouteReplaceLinkWithSrc(dest *net.IPNet, ifaceName string, src net.IP) error {
	if l.routeErr != nil {
		return l.routeErr
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	srcText := ""
	if src != nil {
		srcText = src.String()
	}
	l.routes = append(l.routes, fakeMasterLinkRoute{iface: ifaceName, dest: dest.String(), src: srcText})
	return nil
}

func (l *fakeMasterLinkConfigurator) assertCalls(t *testing.T, meshIface, clientIface, overlayCIDR string) {
	t.Helper()
	if !l.hasCalls(meshIface, clientIface, overlayCIDR) {
		l.mu.Lock()
		defer l.mu.Unlock()
		t.Fatalf("link calls = addrs:%#v up:%#v, want addr %s %s and up %#v", l.addrs, l.upIfcs, meshIface, overlayCIDR, []string{meshIface, clientIface})
	}
}

func (l *fakeMasterLinkConfigurator) hasCalls(meshIface, clientIface, overlayCIDR string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.addrs) != 1 || l.addrs[0] != (fakeMasterLinkAddress{iface: meshIface, addr: overlayCIDR}) {
		return false
	}
	wantUp := []string{meshIface, clientIface}
	if len(l.upIfcs) != len(wantUp) {
		return false
	}
	for i := range wantUp {
		if l.upIfcs[i] != wantUp[i] {
			return false
		}
	}
	return true
}

func TestNewMasterValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     MasterConfig
		wantErr string
	}{
		{name: "missing name", cfg: MasterConfig{OverlayIP: "172.21.92.2"}, wantErr: "name is required"},
		{name: "missing overlay", cfg: MasterConfig{Name: "master-01"}, wantErr: "overlay IP is required"},
		{name: "bad overlay", cfg: MasterConfig{Name: "master-01", OverlayIP: "not-ip"}, wantErr: "parse master overlay IP"},
		{
			name: "coordination missing mesh endpoint",
			cfg: MasterConfig{
				Name:             "master-01",
				OverlayIP:        "172.21.92.2",
				LinkConfigurator: &fakeMasterLinkConfigurator{},
				Coordination: &MasterCoordinationConfig{
					ListenAddr: "127.0.0.1:0",
					StateDir:   t.TempDir(),
				},
			},
			wantErr: "mesh endpoint is required",
		},
		{
			name: "coordination invalid mesh endpoint",
			cfg: MasterConfig{
				Name:             "master-01",
				OverlayIP:        "172.21.92.2",
				MeshEndpointHost: "203.0.113.10",
				LinkConfigurator: &fakeMasterLinkConfigurator{},
				Coordination: &MasterCoordinationConfig{
					ListenAddr: "127.0.0.1:0",
					StateDir:   t.TempDir(),
				},
			},
			wantErr: "must be host:port",
		},
		{name: "missing link configurator", cfg: MasterConfig{Name: "master-01", OverlayIP: "172.21.92.2"}, wantErr: "link configurator is required"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewMaster(tt.cfg)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestMasterRunStartsAndClosesOnCancel(t *testing.T) {
	t.Parallel()

	var clientTransport *fakeMasterTransport
	var meshTransport *fakeMasterTransport
	link := &fakeMasterLinkConfigurator{}
	master, err := NewMaster(MasterConfig{
		Name:             "master-01",
		OverlayIP:        "172.21.92.2",
		LinkConfigurator: link,
		DualListener: wg.DualListenerConfig{
			VanillaFactory: func(name string) (wg.Transport, error) {
				clientTransport = &fakeMasterTransport{name: name, protocol: wg.ProtocolVanilla}
				return clientTransport, nil
			},
			AWGFactory: func(name string) (wg.Transport, error) {
				meshTransport = &fakeMasterTransport{name: name, protocol: wg.ProtocolAmneziaWG}
				return meshTransport, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewMaster: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- master.Run(ctx)
	}()

	waitFor(t, func() bool { return master.Status().Listeners.Started })
	status := master.Status()
	if status.Coordination.Enabled || status.Coordination.Started || status.Coordination.BoundAddr != "" {
		t.Fatalf("master without coordination config must keep coordination disabled, got %#v", status.Coordination)
	}
	waitFor(t, func() bool {
		return link.hasCalls(wg.DefaultMeshInterfaceName, wg.DefaultClientInterfaceName, "172.21.92.2/32")
	})
	link.assertCalls(t, wg.DefaultMeshInterfaceName, wg.DefaultClientInterfaceName, "172.21.92.2/32")
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned error after cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("master Run did not return after cancellation")
	}
	if clientTransport.closeCount != 1 || meshTransport.closeCount != 1 {
		t.Fatalf("expected both transports closed once, got client=%d mesh=%d", clientTransport.closeCount, meshTransport.closeCount)
	}
}

func TestMasterRunReturnsStartupError(t *testing.T) {
	t.Parallel()

	master, err := NewMaster(MasterConfig{
		Name:             "master-01",
		OverlayIP:        "172.21.92.2",
		LinkConfigurator: &fakeMasterLinkConfigurator{},
		DualListener: wg.DualListenerConfig{
			VanillaFactory: func(name string) (wg.Transport, error) {
				return &fakeMasterTransport{name: name, protocol: wg.ProtocolVanilla}, nil
			},
			AWGFactory: func(name string) (wg.Transport, error) {
				return nil, errors.New("mesh startup failed")
			},
		},
	})
	if err != nil {
		t.Fatalf("NewMaster: %v", err)
	}

	err = master.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "mesh startup failed") {
		t.Fatalf("expected startup error, got %v", err)
	}
}

func TestMasterRunClosesTransportsOnLinkSetupError(t *testing.T) {
	t.Parallel()

	var clientTransport *fakeMasterTransport
	var meshTransport *fakeMasterTransport
	master, err := NewMaster(MasterConfig{
		Name:             "master-01",
		OverlayIP:        "172.21.92.2",
		LinkConfigurator: &fakeMasterLinkConfigurator{upErr: errors.New("link denied")},
		DualListener: wg.DualListenerConfig{
			VanillaFactory: func(name string) (wg.Transport, error) {
				clientTransport = &fakeMasterTransport{name: name, protocol: wg.ProtocolVanilla}
				return clientTransport, nil
			},
			AWGFactory: func(name string) (wg.Transport, error) {
				meshTransport = &fakeMasterTransport{name: name, protocol: wg.ProtocolAmneziaWG}
				return meshTransport, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewMaster: %v", err)
	}

	err = master.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "configure master links") || !strings.Contains(err.Error(), "link denied") {
		t.Fatalf("expected link setup error, got %v", err)
	}
	if clientTransport.closeCount != 1 || meshTransport.closeCount != 1 {
		t.Fatalf("expected both transports closed after link failure, got client=%d mesh=%d", clientTransport.closeCount, meshTransport.closeCount)
	}
}

func TestApplyRegisteredMeshPeerAddsPeerAndRoute(t *testing.T) {
	t.Parallel()

	var meshTransport *fakeMasterTransport
	listener, err := wg.NewDualListener(wg.DualListenerConfig{
		VanillaFactory: func(name string) (wg.Transport, error) {
			return &fakeMasterTransport{name: name, protocol: wg.ProtocolVanilla}, nil
		},
		AWGFactory: func(name string) (wg.Transport, error) {
			meshTransport = &fakeMasterTransport{name: name, protocol: wg.ProtocolAmneziaWG}
			return meshTransport, nil
		},
	})
	if err != nil {
		t.Fatalf("NewDualListener: %v", err)
	}
	if err := listener.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	link := &fakeMasterLinkConfigurator{}
	peerKey := wg.Key{1, 2, 3}
	err = applyRegisteredMeshPeer(listener, "master-a", link, mustIPNet(t, "1.0.0.1/32"), control_plane.RegisteredNode{
		Name:      "egress-a",
		Roles:     []role.Role{role.RoleEgress},
		OverlayIP: "1.0.0.2",
		Pubkey:    peerKey[:],
		Protocol:  string(wg.ProtocolAmneziaWG),
	})
	if err != nil {
		t.Fatalf("applyRegisteredMeshPeer: %v", err)
	}
	if len(meshTransport.peers) != 1 {
		t.Fatalf("mesh peers = %#v, want one peer", meshTransport.peers)
	}
	peer := meshTransport.peers[0]
	if peer.PublicKey != peerKey {
		t.Fatalf("peer public key mismatch")
	}
	if len(peer.AllowedIPs) != 1 || peer.AllowedIPs[0].String() != "1.0.0.2/32" {
		t.Fatalf("peer allowed IPs = %#v, want 1.0.0.2/32", peer.AllowedIPs)
	}
	link.mu.Lock()
	routes := append([]fakeMasterLinkRoute(nil), link.routes...)
	link.mu.Unlock()
	wantRoute := fakeMasterLinkRoute{iface: wg.DefaultMeshInterfaceName, dest: "1.0.0.2/32", src: "1.0.0.1"}
	if len(routes) != 1 || routes[0] != wantRoute {
		t.Fatalf("routes = %#v, want %#v", routes, []fakeMasterLinkRoute{wantRoute})
	}
}

func mustIPNet(t *testing.T, cidr string) *net.IPNet {
	t.Helper()
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("parse CIDR %q: %v", cidr, err)
	}
	ipNet.IP = ip
	return ipNet
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not met before deadline")
}
