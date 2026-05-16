package clientd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/role"
	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/tls"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/wg"
	pb "github.com/coonfuuseed-paandaa/awg-mesh/v2/proto/control_plane"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

func TestStateUpdateImmutableAndConfiguratorPayloadsDiffer(t *testing.T) {
	base := State{Peers: []PeerEntry{{PeerName: "master-a", AllowedIPs: []string{"10.0.0.1/32"}}}, PeerListVersion: 1}
	updated, changed := base.WithPeerList(&pb.PeerListUpdate{Version: 2, Peers: []*pb.PeerEntry{{PeerName: "master-b", AllowedIps: []string{"10.0.0.2/32"}}}})
	if !changed {
		t.Fatalf("expected newer peer-list to change state")
	}
	updated.Peers[0].AllowedIPs[0] = "10.9.9.9/32"
	if base.Peers[0].AllowedIPs[0] != "10.0.0.1/32" {
		t.Fatalf("base state was mutated: %#v", base.Peers[0])
	}

	configurator := &recordingConfigurator{}
	first := State{Peers: []PeerEntry{{PeerName: "master-a", AllowedIPs: []string{"10.0.0.1/32"}}}, PeerListVersion: 1}
	second := State{Peers: []PeerEntry{{PeerName: "master-b", AllowedIPs: []string{"10.0.0.2/32"}}}, PeerListVersion: 2}
	if err := configurator.Apply(context.Background(), first); err != nil {
		t.Fatalf("apply first: %v", err)
	}
	if err := configurator.Apply(context.Background(), second); err != nil {
		t.Fatalf("apply second: %v", err)
	}
	calls := configurator.callsSnapshot()
	if len(calls) != 2 || reflect.DeepEqual(calls[0], calls[1]) {
		t.Fatalf("expected different configurator payloads, got %#v", calls)
	}
}

func TestAgentRegistersAndAppliesNewerStreamVersions(t *testing.T) {
	const streamTestTimeout = 10 * time.Second

	server := &streamingTestServer{
		registered: make(chan *pb.RegisterNodeRequest, 1),
		peerUpdates: []*pb.PeerListUpdate{
			{SubjectNode: "client-a", Version: 1, Peers: []*pb.PeerEntry{{PeerName: "master-a", AllowedIps: []string{"10.0.0.1/32"}}}},
			{SubjectNode: "client-a", Version: 1, Peers: []*pb.PeerEntry{{PeerName: "stale", AllowedIps: []string{"10.0.0.9/32"}}}},
			{SubjectNode: "client-a", Version: 2, Peers: []*pb.PeerEntry{{PeerName: "master-b", AllowedIps: []string{"10.0.0.2/32"}}}},
		},
		ownershipUpdates: []*pb.OwnershipUpdate{
			{Version: 3, FullSnapshot: true, Entries: []*pb.OwnershipEntry{{OverlayIp: "10.0.0.2", OwningMaster: "master-b", Reason: "scheduled"}}},
			{Version: 3, FullSnapshot: false, Entries: []*pb.OwnershipEntry{{OverlayIp: "10.0.0.3", OwningMaster: "stale"}}},
		},
	}
	addr, cleanup := startTestControlPlane(t, server)
	defer cleanup()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer func() { _ = conn.Close() }()

	configurator := &recordingConfigurator{notify: make(chan State, 8)}
	cachePath := t.TempDir() + "/clientd-state.json"
	publicKey := wg.Key{}
	copy(publicKey[:], bytesOf(0x7a))
	agent, err := NewAgent(Config{
		NodeName:      "client-a",
		Roles:         []role.Role{role.RoleClient},
		OverlayIP:     "10.10.0.10",
		Region:        "eu-test",
		NodeCertPEM:   []byte("cert-bytes"),
		Version:       "test-version",
		InterfaceName: "awg-test0",
		Protocol:      wg.ProtocolAmneziaWG,
		PublicKey:     publicKey,
		StatePath:     cachePath,
	}, pb.NewControlPlaneClient(conn), configurator)
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx) }()
	defer cancel()

	select {
	case got := <-server.registered:
		if got.NodeName != "client-a" || got.OverlayIp != "10.10.0.10" || got.Region != "eu-test" || got.NodeVersion != "test-version" {
			t.Fatalf("unexpected registration: %#v", got)
		}
		if !reflect.DeepEqual(got.Roles, []string{"client"}) {
			t.Fatalf("unexpected roles: %#v", got.Roles)
		}
		if string(got.NodeCertPem) != "cert-bytes" {
			t.Fatalf("unexpected cert bytes: %q", string(got.NodeCertPem))
		}
		if !bytes.Equal(got.Pubkey, publicKey[:]) {
			t.Fatalf("unexpected pubkey: %x", got.Pubkey)
		}
		if got.Protocol != string(wg.ProtocolAmneziaWG) {
			t.Fatalf("unexpected protocol: %q", got.Protocol)
		}
	case <-time.After(streamTestTimeout):
		t.Fatalf("timed out waiting for registration")
	}

	var sawPeerV2, sawOwnershipV3 bool
	for i := 0; i < 4 && (!sawPeerV2 || !sawOwnershipV3); i++ {
		select {
		case state := <-configurator.notify:
			if state.PeerListVersion == 2 && len(state.Peers) == 1 && state.Peers[0].PeerName == "master-b" {
				sawPeerV2 = true
			}
			if state.OwnershipVersion == 3 && len(state.Ownership) == 1 && state.Ownership[0].OwningMaster == "master-b" {
				sawOwnershipV3 = true
			}
		case <-time.After(streamTestTimeout):
			t.Fatalf("timed out waiting for configurator calls")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("agent returned error: %v", err)
	}
	calls := configurator.callsSnapshot()
	for _, call := range calls {
		if len(call.Peers) == 1 && call.Peers[0].PeerName == "stale" {
			t.Fatalf("stale peer-list version was applied: %#v", calls)
		}
	}
}

func TestAgentRegistersBeforeApplyingCachedState(t *testing.T) {
	const streamTestTimeout = 10 * time.Second

	server := &streamingTestServer{
		registered: make(chan *pb.RegisterNodeRequest, 1),
	}
	addr, cleanup := startTestControlPlane(t, server)
	defer cleanup()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer func() { _ = conn.Close() }()

	cachePath := filepath.Join(t.TempDir(), "clientd-state.json")
	if err := NewStateCache(cachePath).Save(State{
		PeerListVersion: 7,
		Peers:           []PeerEntry{{PeerName: "cached-master", PeerPubkey: bytesOf(0x42), AllowedIPs: []string{"10.0.0.1/32"}}},
	}); err != nil {
		t.Fatalf("seed cached state: %v", err)
	}

	configurator := &recordingConfigurator{notify: make(chan State, 1)}
	agent, err := NewAgent(Config{
		NodeName:      "client-a",
		Roles:         []role.Role{role.RoleClient},
		OverlayIP:     "10.10.0.10",
		Region:        "eu-test",
		NodeCertPEM:   []byte("cert-bytes"),
		Version:       "test-version",
		InterfaceName: "awg-test0",
		Protocol:      wg.ProtocolAmneziaWG,
		StatePath:     cachePath,
	}, pb.NewControlPlaneClient(conn), configurator)
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx) }()
	defer cancel()

	select {
	case state := <-configurator.notify:
		t.Fatalf("cached state applied before registration: %#v", state)
	case <-server.registered:
	case <-time.After(streamTestTimeout):
		t.Fatalf("timed out waiting for registration")
	}

	select {
	case state := <-configurator.notify:
		if state.PeerListVersion != 7 || len(state.Peers) != 1 || state.Peers[0].PeerName != "cached-master" {
			t.Fatalf("unexpected cached state apply: %#v", state)
		}
	case <-time.After(streamTestTimeout):
		t.Fatalf("timed out waiting for cached state apply")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(streamTestTimeout):
		t.Fatalf("timed out waiting for agent shutdown")
	}
}

func TestAgentAppliesCertUpdateToLocalFiles(t *testing.T) {
	const streamTestTimeout = 10 * time.Second

	caCert, caKey, err := pkgtls.GenerateCA("mesh-ca")
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	certPEM, keyPEM, err := pkgtls.IssueCert(caCert, caKey, "client-a", []string{"client-a", "10.10.0.10"})
	if err != nil {
		t.Fatalf("IssueCert: %v", err)
	}

	server := &streamingTestServer{
		registered: make(chan *pb.RegisterNodeRequest, 1),
		certUpdates: []*pb.CertUpdate{{
			CertPem:        certPEM,
			KeyPem:         keyPEM,
			ValidFromUnix:  time.Now().Add(-time.Minute).Unix(),
			ValidUntilUnix: time.Now().Add(90 * 24 * time.Hour).Unix(),
		}},
	}
	addr, cleanup := startTestControlPlane(t, server)
	defer cleanup()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer func() { _ = conn.Close() }()

	dir := t.TempDir()
	certPath := filepath.Join(dir, "node.crt")
	keyPath := filepath.Join(dir, "node.key")
	agent, err := NewAgent(Config{
		NodeName:      "client-a",
		Roles:         []role.Role{role.RoleClient},
		OverlayIP:     "10.10.0.10",
		Region:        "eu-test",
		NodeCertPEM:   []byte("cert-bytes"),
		Version:       "test-version",
		InterfaceName: "awg-test0",
		Protocol:      wg.ProtocolAmneziaWG,
		StatePath:     filepath.Join(dir, "clientd-state.json"),
		CertPath:      certPath,
		KeyPath:       keyPath,
	}, pb.NewControlPlaneClient(conn), &recordingConfigurator{})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx) }()
	defer cancel()

	select {
	case <-server.registered:
	case <-time.After(streamTestTimeout):
		t.Fatalf("timed out waiting for registration")
	}

	deadline := time.Now().Add(streamTestTimeout)
	for time.Now().Before(deadline) {
		gotCert, certErr := os.ReadFile(certPath)
		gotKey, keyErr := os.ReadFile(keyPath)
		if certErr == nil && keyErr == nil && string(gotCert) == string(certPEM) && string(gotKey) == string(keyPEM) {
			select {
			case got := <-server.registered:
				if string(got.GetNodeCertPem()) != string(certPEM) {
					t.Fatalf("cert update re-registration used old cert bytes: %q", string(got.GetNodeCertPem()))
				}
			case <-time.After(streamTestTimeout):
				t.Fatalf("timed out waiting for cert update re-registration")
			}
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("agent returned error after cancel: %v", err)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for cert update files")
}

func TestApplyCertUpdateRemovesNewKeyWhenCertWriteFails(t *testing.T) {
	caCert, caKey, err := pkgtls.GenerateCA("mesh-ca")
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	certPEM, keyPEM, err := pkgtls.IssueCert(caCert, caKey, "client-a", []string{"client-a", "10.10.0.10"})
	if err != nil {
		t.Fatalf("IssueCert: %v", err)
	}

	dir := t.TempDir()
	certPath := filepath.Join(dir, "node.crt")
	keyPath := filepath.Join(dir, "node.key")
	if err := os.Mkdir(certPath, 0o755); err != nil {
		t.Fatalf("mkdir certPath: %v", err)
	}

	err = ApplyCertUpdate(certPath, keyPath, &pb.CertUpdate{
		CertPem:        certPEM,
		KeyPem:         keyPEM,
		ValidUntilUnix: time.Now().Add(90 * 24 * time.Hour).Unix(),
	})
	if err == nil {
		t.Fatalf("expected cert write failure")
	}
	if _, statErr := os.Stat(keyPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("new key was not removed after cert write failure: %v", statErr)
	}
}

func TestStateCacheMissingLoadWriteOverwriteAndInvalidJSON(t *testing.T) {
	path := t.TempDir() + "/state/cache.json"
	cache := NewStateCache(path)
	missing, err := cache.Load()
	if err != nil {
		t.Fatalf("load missing: %v", err)
	}
	if !missing.IsZero() {
		t.Fatalf("missing cache should load empty state: %#v", missing)
	}
	first := State{PeerListVersion: 1, Peers: []PeerEntry{{PeerName: "master-a"}}}
	second := State{PeerListVersion: 2, Peers: []PeerEntry{{PeerName: "master-b"}}}
	if err := cache.Save(first); err != nil {
		t.Fatalf("save first: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatalf("stat state dir: %v", err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("state dir is group/other accessible: mode=%#o", info.Mode().Perm())
		}
	}
	loadedFirst, err := cache.Load()
	if err != nil {
		t.Fatalf("load first: %v", err)
	}
	if loadedFirst.Peers[0].PeerName != "master-a" {
		t.Fatalf("unexpected first load: %#v", loadedFirst)
	}
	if err := cache.Save(second); err != nil {
		t.Fatalf("save second: %v", err)
	}
	loadedSecond, err := cache.Load()
	if err != nil {
		t.Fatalf("load second: %v", err)
	}
	if loadedSecond.Peers[0].PeerName != "master-b" || reflect.DeepEqual(loadedFirst, loadedSecond) {
		t.Fatalf("overwrite did not change state: first=%#v second=%#v", loadedFirst, loadedSecond)
	}
	if err := os.WriteFile(path, []byte("{not-json"), 0o644); err != nil {
		t.Fatalf("write invalid json: %v", err)
	}
	_, err = cache.Load()
	if err == nil {
		t.Fatalf("expected invalid JSON error")
	}
}

func TestBuildPeerConfigsValidationAndStrippedSeed(t *testing.T) {
	_, _, err := BuildPeerConfigs(ReloadInput{LocalRoles: []role.Role{role.RoleClient}, Peers: []PeerEntry{{PeerName: "endpoint-a", PeerRole: role.RoleEgress}}})
	if !errors.Is(err, ErrNonMasterPeerRejected) {
		t.Fatalf("expected non-master rejection, got %v", err)
	}
	key := bytesOf(0x42)
	_, _, err = BuildPeerConfigs(ReloadInput{LocalRoles: []role.Role{role.RoleEgress}, Peers: []PeerEntry{{PeerName: "master-a", PeerRole: role.RoleMaster, PeerPubkey: key, AllowedIPs: []string{"10.0.0.1/32"}}}})
	if err != nil {
		t.Fatalf("egress role should accept master peer: %v", err)
	}
	_, err = PeerEntryToWGConfig(PeerEntry{PeerName: "master-a", AllowedIPs: []string{"10.0.0.1/32"}})
	if !errors.Is(err, ErrPeerPublicKeyRequired) {
		t.Fatalf("expected stripped peer-list public-key error, got %v", err)
	}
	configs, _, err := BuildPeerConfigs(ReloadInput{LocalRoles: []role.Role{role.RoleClient}, Peers: []PeerEntry{{PeerName: "master-a", PeerRole: role.RoleMaster, PeerPubkey: key, AllowedIPs: []string{"10.0.0.1/32"}, PersistentKeepaliveSecs: 25, Protocol: wg.ProtocolAmneziaWG}}})
	if err != nil {
		t.Fatalf("valid master peer rejected: %v", err)
	}
	if len(configs) != 1 || len(configs[0].AllowedIPs) != 1 || configs[0].PersistentKeepaliveInterval == nil {
		t.Fatalf("unexpected peer config: %#v", configs)
	}
}

func TestTransportConfiguratorReplacesCompleteDesiredPeers(t *testing.T) {
	desiredKeyBytes := bytesOf(0x42)
	staleKeyBytes := bytesOf(0x24)
	transport := &fakeTransport{protocol: wg.ProtocolAmneziaWG, name: "awg-test0"}
	configurator := TransportConfigurator{Transport: transport}

	state := State{Peers: []PeerEntry{{PeerName: "master-a", PeerRole: role.RoleMaster, PeerPubkey: desiredKeyBytes, AllowedIPs: []string{"10.0.0.1/32"}}}}
	if err := configurator.Apply(context.Background(), state); err != nil {
		t.Fatalf("apply complete peers: %v", err)
	}

	configs := transport.configsSnapshot()
	if len(configs) != 1 {
		t.Fatalf("expected one Configure call, got %d", len(configs))
	}
	if !configs[0].ReplacePeers {
		t.Fatalf("expected ReplacePeers=true: %#v", configs[0])
	}
	if len(configs[0].Peers) != 1 {
		t.Fatalf("expected exactly desired peer, got %#v", configs[0].Peers)
	}
	desiredKey, err := wg.NewKey(desiredKeyBytes)
	if err != nil {
		t.Fatalf("desired key: %v", err)
	}
	staleKey, err := wg.NewKey(staleKeyBytes)
	if err != nil {
		t.Fatalf("stale key: %v", err)
	}
	if configs[0].Peers[0].PublicKey != desiredKey {
		t.Fatalf("unexpected desired peer key: %#v", configs[0].Peers[0].PublicKey)
	}
	if configs[0].Peers[0].PublicKey == staleKey {
		t.Fatalf("stale peer was included in desired config")
	}
	if got := transport.addPeerCount(); got != 0 {
		t.Fatalf("expected no AddPeer calls, got %d", got)
	}
}

func TestTransportConfiguratorAppliesDeviceIdentityAndOverlayLink(t *testing.T) {
	privateKey, err := wg.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	overlay := mustIPNet(t, "172.21.92.130/32")
	transport := &fakeTransport{protocol: wg.ProtocolAmneziaWG, name: "awg-test0"}
	link := &fakeLinkConfigurator{}
	configurator := TransportConfigurator{
		Transport:        transport,
		LocalRoles:       []role.Role{role.RoleClient},
		PrivateKey:       &privateKey,
		OverlayAddress:   overlay,
		LinkConfigurator: link,
	}

	state := State{Peers: []PeerEntry{{
		PeerName:         "master-a",
		PeerRole:         role.RoleMaster,
		PeerPubkey:       bytesOf(0x42),
		PeerEndpointHost: "127.0.0.1:51820",
		AllowedIPs:       []string{"172.21.92.1/32"},
	}}}
	if err := configurator.Apply(context.Background(), state); err != nil {
		t.Fatalf("apply production transport config: %v", err)
	}

	configs := transport.configsSnapshot()
	if len(configs) != 1 {
		t.Fatalf("expected one Configure call, got %d", len(configs))
	}
	if configs[0].PrivateKey == nil || *configs[0].PrivateKey != privateKey {
		t.Fatalf("Configure did not apply private key: %#v", configs[0].PrivateKey)
	}
	if !configs[0].ReplacePeers || len(configs[0].Peers) != 1 {
		t.Fatalf("Configure did not apply full peer snapshot: %#v", configs[0])
	}
	link.assertCalls(t, "awg-test0", overlay, []fakeLinkRoute{{iface: "awg-test0", dest: "172.21.92.1/32", src: "172.21.92.130"}})
}

func TestTransportConfiguratorInstallsPeerAllowedIPRoutes(t *testing.T) {
	overlay := mustIPNet(t, "172.21.92.130/32")
	transport := &fakeTransport{protocol: wg.ProtocolAmneziaWG, name: "awg-test0"}
	link := &fakeLinkConfigurator{}
	configurator := TransportConfigurator{
		Transport:        transport,
		LocalRoles:       []role.Role{role.RoleClient},
		OverlayAddress:   overlay,
		LinkConfigurator: link,
	}

	state := State{Peers: []PeerEntry{
		{
			PeerName:   "master-a",
			PeerRole:   role.RoleMaster,
			PeerPubkey: bytesOf(0x42),
			AllowedIPs: []string{
				"172.21.92.1/32",
				"172.21.92.130/32",
				"172.21.92.1/32",
			},
		},
		{
			PeerName:   "master-b",
			PeerRole:   role.RoleMaster,
			PeerPubkey: bytesOf(0x43),
			AllowedIPs: []string{"172.21.92.2/32"},
		},
	}}
	if err := configurator.Apply(context.Background(), state); err != nil {
		t.Fatalf("apply production transport routes: %v", err)
	}

	link.assertCalls(t, "awg-test0", overlay, []fakeLinkRoute{
		{iface: "awg-test0", dest: "172.21.92.1/32", src: "172.21.92.130"},
		{iface: "awg-test0", dest: "172.21.92.2/32", src: "172.21.92.130"},
	})
}

func TestTransportConfiguratorSkipsStrippedSnapshot(t *testing.T) {
	transport := &fakeTransport{protocol: wg.ProtocolAmneziaWG, name: "awg-test0"}
	configurator := TransportConfigurator{Transport: transport}

	state := State{Peers: []PeerEntry{{PeerName: "master-a", PeerRole: role.RoleMaster, AllowedIPs: []string{"10.0.0.1/32"}}}}
	if err := configurator.Apply(context.Background(), state); err != nil {
		t.Fatalf("stripped snapshot should not fail: %v", err)
	}
	configs := transport.configsSnapshot()
	if got := len(configs); got != 1 {
		t.Fatalf("expected 1 Configure call (empty peers), got %d", got)
	}
	if got := len(configs[0].Peers); got != 0 {
		t.Fatalf("expected 0 peers in Configure (all skipped), got %d", got)
	}
}

func TestTransportConfiguratorUsesLocalRoles(t *testing.T) {
	transport := &fakeTransport{protocol: wg.ProtocolAmneziaWG, name: "awg-test0"}
	configurator := TransportConfigurator{Transport: transport, LocalRoles: []role.Role{role.RoleEgress}}

	state := State{Peers: []PeerEntry{{PeerName: "egress-peer", PeerRole: role.RoleEgress, PeerPubkey: bytesOf(0x42), AllowedIPs: []string{"10.0.0.2/32"}}}}
	if err := configurator.Apply(context.Background(), state); err != nil {
		t.Fatalf("egress configurator should accept non-master peer: %v", err)
	}
	if got := len(transport.configsSnapshot()); got != 1 {
		t.Fatalf("expected one Configure call, got %d", got)
	}
}

func TestAgentRunStrippedPeerUpdateDoesNotExit(t *testing.T) {
	const streamTestTimeout = 10 * time.Second

	server := &streamingTestServer{
		registered: make(chan *pb.RegisterNodeRequest, 1),
		peerUpdates: []*pb.PeerListUpdate{
			{SubjectNode: "client-a", Version: 1, Peers: []*pb.PeerEntry{{PeerName: "master-a", AllowedIps: []string{"10.0.0.1/32"}}}},
		},
	}
	addr, cleanup := startTestControlPlane(t, server)
	defer cleanup()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer func() { _ = conn.Close() }()

	transport := &fakeTransport{protocol: wg.ProtocolAmneziaWG, name: "awg-test0"}
	agent, err := NewAgent(Config{
		NodeName:      "client-a",
		Roles:         []role.Role{role.RoleClient},
		OverlayIP:     "10.10.0.10",
		Region:        "eu-test",
		NodeCertPEM:   []byte("cert-bytes"),
		Version:       "test-version",
		InterfaceName: "awg-test0",
		Protocol:      wg.ProtocolAmneziaWG,
		StatePath:     t.TempDir() + "/clientd-state.json",
	}, pb.NewControlPlaneClient(conn), TransportConfigurator{Transport: transport})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx) }()
	defer cancel()

	select {
	case <-server.registered:
	case <-time.After(streamTestTimeout):
		t.Fatalf("timed out waiting for registration")
	}

	select {
	case err := <-done:
		t.Fatalf("agent exited before cancel: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	configs := transport.configsSnapshot()
	if got := len(configs); got != 1 {
		t.Fatalf("expected 1 Configure call (empty peers for stripped snapshot), got %d", got)
	}
	if got := len(configs[0].Peers); got != 0 {
		t.Fatalf("expected 0 peers in Configure (all stripped), got %d", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("agent returned error after cancel: %v", err)
		}
	case <-time.After(streamTestTimeout):
		t.Fatalf("timed out waiting for agent shutdown")
	}
}

func TestAgentRunFirstPeerSnapshotOverridesCachedVersion(t *testing.T) {
	const streamTestTimeout = 10 * time.Second

	key := bytesOf(0x42)
	server := &streamingTestServer{
		registered: make(chan *pb.RegisterNodeRequest, 1),
		peerUpdates: []*pb.PeerListUpdate{
			{
				SubjectNode: "client-a",
				Version:     1,
				Peers: []*pb.PeerEntry{{
					PeerName:      "master-a",
					PeerPubkey:    key,
					AllowedIps:    []string{"10.0.0.1/32"},
					Protocol:      string(wg.ProtocolAmneziaWG),
					PeerOverlayIp: "10.0.0.1",
				}},
			},
		},
	}
	addr, cleanup := startTestControlPlane(t, server)
	defer cleanup()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer func() { _ = conn.Close() }()

	cachePath := filepath.Join(t.TempDir(), "clientd-state.json")
	if err := NewStateCache(cachePath).Save(State{
		PeerListVersion: 1,
		Peers:           []PeerEntry{{PeerName: "master-a", AllowedIPs: []string{"10.0.0.1/32"}}},
	}); err != nil {
		t.Fatalf("seed cached state: %v", err)
	}

	transport := &fakeTransport{protocol: wg.ProtocolAmneziaWG, name: "awg-test0"}
	agent, err := NewAgent(Config{
		NodeName:      "client-a",
		Roles:         []role.Role{role.RoleClient},
		OverlayIP:     "10.10.0.10",
		Region:        "eu-test",
		NodeCertPEM:   []byte("cert-bytes"),
		Version:       "test-version",
		InterfaceName: "awg-test0",
		Protocol:      wg.ProtocolAmneziaWG,
		StatePath:     cachePath,
	}, pb.NewControlPlaneClient(conn), TransportConfigurator{Transport: transport})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx) }()
	defer cancel()

	select {
	case <-server.registered:
	case <-time.After(streamTestTimeout):
		t.Fatalf("timed out waiting for registration")
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, cfg := range transport.configsSnapshot() {
			if len(cfg.Peers) == 1 {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Fatalf("agent returned error after cancel: %v", err)
					}
				case <-time.After(streamTestTimeout):
					t.Fatalf("timed out waiting for agent shutdown")
				}
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("first stream snapshot with populated pubkey was not applied; configs=%#v", transport.configsSnapshot())
}

func TestAgentRunReturnsErrorWhenPeerStreamEndsBeforeCancel(t *testing.T) {
	const streamTestTimeout = 10 * time.Second

	server := &streamingTestServer{
		registered:            make(chan *pb.RegisterNodeRequest, 1),
		closePeerAfterUpdates: true,
	}
	addr, cleanup := startTestControlPlane(t, server)
	defer cleanup()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer func() { _ = conn.Close() }()

	agent, err := NewAgent(Config{
		NodeName:      "client-a",
		Roles:         []role.Role{role.RoleClient},
		OverlayIP:     "10.10.0.10",
		Region:        "eu-test",
		NodeCertPEM:   []byte("cert-bytes"),
		Version:       "test-version",
		InterfaceName: "awg-test0",
		Protocol:      wg.ProtocolAmneziaWG,
		StatePath:     t.TempDir() + "/clientd-state.json",
	}, pb.NewControlPlaneClient(conn), &recordingConfigurator{})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	ctx := t.Context()
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx) }()

	select {
	case <-server.registered:
	case <-time.After(streamTestTimeout):
		t.Fatalf("timed out waiting for registration")
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("expected unexpected EOF error")
		}
		if !errors.Is(err, io.EOF) {
			t.Fatalf("expected EOF-wrapped error, got %v", err)
		}
	case <-time.After(streamTestTimeout):
		t.Fatalf("timed out waiting for stream EOF")
	}
}

type fakeTransport struct {
	mu       sync.Mutex
	protocol wg.Protocol
	name     string
	configs  []wg.Config
	addPeers []wg.PeerConfig
}

func (t *fakeTransport) Protocol() wg.Protocol { return t.protocol }

func (t *fakeTransport) Name() string { return t.name }

func (t *fakeTransport) Configure(cfg wg.Config) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.configs = append(t.configs, cfg)
	return nil
}

func (t *fakeTransport) AddPeer(peer wg.PeerConfig) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.addPeers = append(t.addPeers, peer)
	return nil
}

func (t *fakeTransport) RemovePeer(wg.Key) error { return nil }

func (t *fakeTransport) Stats() (*wg.Device, error) { return &wg.Device{Name: t.name}, nil }

func (t *fakeTransport) Close() error { return nil }

func (t *fakeTransport) configsSnapshot() []wg.Config {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]wg.Config, len(t.configs))
	copy(out, t.configs)
	return out
}

func (t *fakeTransport) addPeerCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.addPeers)
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

type fakeLinkConfigurator struct {
	mu     sync.Mutex
	addrs  []fakeLinkAddress
	upIfcs []string
	routes []fakeLinkRoute
}

type fakeLinkAddress struct {
	iface string
	addr  string
}

type fakeLinkRoute struct {
	iface string
	dest  string
	src   string
}

func (l *fakeLinkConfigurator) AddrAdd(ifaceName string, addr *net.IPNet) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.addrs = append(l.addrs, fakeLinkAddress{iface: ifaceName, addr: addr.String()})
	return nil
}

func (l *fakeLinkConfigurator) LinkSetUp(ifaceName string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.upIfcs = append(l.upIfcs, ifaceName)
	return nil
}

func (l *fakeLinkConfigurator) RouteReplaceLinkWithSrc(dest *net.IPNet, ifaceName string, src net.IP) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	srcText := ""
	if src != nil {
		srcText = src.String()
	}
	l.routes = append(l.routes, fakeLinkRoute{iface: ifaceName, dest: dest.String(), src: srcText})
	return nil
}

func (l *fakeLinkConfigurator) assertCalls(t *testing.T, wantIface string, wantAddr *net.IPNet, wantRoutes []fakeLinkRoute) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.addrs) != 1 || l.addrs[0].iface != wantIface || l.addrs[0].addr != wantAddr.String() {
		t.Fatalf("AddrAdd calls = %#v, want %s %s", l.addrs, wantIface, wantAddr)
	}
	if len(l.upIfcs) != 1 || l.upIfcs[0] != wantIface {
		t.Fatalf("LinkSetUp calls = %#v, want %s", l.upIfcs, wantIface)
	}
	if !reflect.DeepEqual(l.routes, wantRoutes) {
		t.Fatalf("RouteReplaceLinkWithSrc calls = %#v, want %#v", l.routes, wantRoutes)
	}
}

type recordingConfigurator struct {
	mu     sync.Mutex
	calls  []State
	notify chan State
}

func (c *recordingConfigurator) Apply(_ context.Context, state State) error {
	state = state.Clone()
	c.mu.Lock()
	c.calls = append(c.calls, state)
	c.mu.Unlock()
	if c.notify != nil {
		select {
		case c.notify <- state:
		default:
		}
	}
	return nil
}

func (c *recordingConfigurator) callsSnapshot() []State {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]State, len(c.calls))
	for i, call := range c.calls {
		out[i] = call.Clone()
	}
	return out
}

type streamingTestServer struct {
	pb.UnimplementedControlPlaneServer
	registered                 chan *pb.RegisterNodeRequest
	peerUpdates                []*pb.PeerListUpdate
	ownershipUpdates           []*pb.OwnershipUpdate
	certUpdates                []*pb.CertUpdate
	closePeerAfterUpdates      bool
	closeOwnershipAfterUpdates bool
	closeCertAfterUpdates      bool
}

func (s *streamingTestServer) RegisterNode(_ context.Context, req *pb.RegisterNodeRequest) (*pb.RegisterNodeResponse, error) {
	s.registered <- req
	return &pb.RegisterNodeResponse{Accepted: true}, nil
}

func (s *streamingTestServer) StreamPeerList(_ *pb.StreamPeerListRequest, stream grpc.ServerStreamingServer[pb.PeerListUpdate]) error {
	for _, update := range s.peerUpdates {
		if err := stream.Send(update); err != nil {
			return err
		}
	}
	if s.closePeerAfterUpdates {
		return nil
	}
	<-stream.Context().Done()
	return nil
}

func (s *streamingTestServer) StreamOwnership(_ *pb.StreamOwnershipRequest, stream grpc.ServerStreamingServer[pb.OwnershipUpdate]) error {
	for _, update := range s.ownershipUpdates {
		if err := stream.Send(update); err != nil {
			return err
		}
	}
	if s.closeOwnershipAfterUpdates {
		return nil
	}
	<-stream.Context().Done()
	return nil
}

func (s *streamingTestServer) StreamCertUpdate(_ *pb.StreamCertRequest, stream grpc.ServerStreamingServer[pb.CertUpdate]) error {
	for _, update := range s.certUpdates {
		if err := stream.Send(update); err != nil {
			return err
		}
	}
	if s.closeCertAfterUpdates {
		return nil
	}
	<-stream.Context().Done()
	return nil
}

func startTestControlPlane(t *testing.T, server pb.ControlPlaneServer) (string, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcServer := grpc.NewServer()
	pb.RegisterControlPlaneServer(grpcServer, server)
	go func() { _ = grpcServer.Serve(lis) }()
	addr := lis.Addr().String()
	waitForTestControlPlane(t, addr)
	return addr, func() {
		grpcServer.Stop()
		_ = lis.Close()
	}
}

func waitForTestControlPlane(t *testing.T, addr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("new readiness client: %v", err)
	}
	defer func() { _ = conn.Close() }()
	conn.Connect()
	for state := conn.GetState(); state != connectivity.Ready; state = conn.GetState() {
		if !conn.WaitForStateChange(ctx, state) {
			t.Fatalf("test control-plane did not become ready: state=%s err=%v", state, ctx.Err())
		}
	}
}

func bytesOf(v byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = v
	}
	return out
}
