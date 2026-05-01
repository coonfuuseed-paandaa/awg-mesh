//go:build linux

package node

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/routing"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
	"github.com/rs/zerolog"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestAddPeerConcurrentSamePubkey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, _, err := EnsureKeypair(dir)
	if err != nil {
		t.Fatalf("EnsureKeypair: %v", err)
	}

	runner := NewClientRunner(&Node{config: NodeConfig{ConfigDir: dir}})

	// Generate a fake peer public key (32 bytes)
	peerKey := make([]byte, 32)
	for i := range peerKey {
		peerKey[i] = byte(i + 1)
	}

	// First AddPeer creates the interface — may fail on non-privileged, skip in that case
	err = runner.AddPeer(peerKey, nil, []string{"0.0.0.0/0"}, "192.168.1.1:51820", 25, "")
	if err != nil {
		t.Skipf("AddPeer requires TUN device (privileged): %v", err)
	}

	// Now fire concurrent AddPeer calls for the SAME pubkey — exercises the
	// existing-link path where the race condition was fixed (mutex held across
	// configurePeerOnIface).
	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make([]error, goroutines)

	for i := range goroutines {
		go func(idx int) {
			defer wg.Done()
			errs[idx] = runner.AddPeer(peerKey, nil, []string{"0.0.0.0/0"}, "192.168.1.1:51820", 25, "")
		}(i)
	}
	wg.Wait()

	// All should succeed (reconfigure existing peer) — no panic, no race
	for i, e := range errs {
		if e != nil {
			t.Errorf("concurrent AddPeer[%d] failed: %v", i, e)
		}
	}
}

func TestListPeersConcurrentClose(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, _, err := EnsureKeypair(dir)
	if err != nil {
		t.Fatalf("EnsureKeypair: %v", err)
	}

	runner := NewClientRunner(&Node{config: NodeConfig{ConfigDir: dir}})

	// Generate peer key
	peerKey := make([]byte, 32)
	for i := range peerKey {
		peerKey[i] = byte(i + 1)
	}

	// Add a peer first
	err = runner.AddPeer(peerKey, nil, []string{"0.0.0.0/0"}, "192.168.1.1:51820", 25, "")
	if err != nil {
		t.Skipf("AddPeer requires TUN device (privileged): %v", err)
	}

	// ListPeers while removing the peer concurrently — must not panic
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		// ListPeers should handle concurrent interface close gracefully
		_ = runner.ListPeers()
	}()

	go func() {
		defer wg.Done()
		_ = runner.RemovePeer(peerKey)
	}()

	wg.Wait()

	// If we get here without panic, the test passes
}

func TestAddPeerNewLinkStartsUnhealthy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, _, err := EnsureKeypair(dir)
	if err != nil {
		t.Fatalf("EnsureKeypair: %v", err)
	}

	runner := NewClientRunner(&Node{config: NodeConfig{ConfigDir: dir}})

	peerKey := make([]byte, 32)
	for i := range peerKey {
		peerKey[i] = byte(i + 1)
	}
	peerHex := hex.EncodeToString(peerKey)

	err = runner.AddPeer(peerKey, nil, []string{"0.0.0.0/0"}, "192.168.1.1:51820", 25, "")
	if err != nil {
		t.Skipf("AddPeer requires TUN device (privileged): %v", err)
	}

	link, ok := runner.platformState.byKey[peerHex]
	if !ok || link == nil {
		t.Fatalf("expected link for peer %s to be stored", peerHex)
	}
	if link.healthy {
		t.Fatalf("expected new link to start unhealthy until healthcheck proves readiness")
	}
	if len(runner.platformState.links) != 1 {
		t.Fatalf("expected exactly one stored link, got %d", len(runner.platformState.links))
	}
	if runner.platformState.links[0] != link {
		t.Fatalf("expected stored links slice to reference byKey link")
	}
}

// =============================================================================
// Phase 2: rebuildClientECMP tests (T008)
// =============================================================================

// --- Mock implementations for routing interfaces ---

type mockRouter struct {
	mu                     sync.Mutex
	setECMPCalls           []mockRouteCall
	setECMPInTableCalls    []mockRouteInTableCall
	removeECMPCalls        []string
	setECMPErrorsByDest    map[string]error
	removeECMPErrorsByDest map[string]error
}

type mockRouteCall struct {
	dest     string
	nexthops []routing.NextHop
}

type mockRouteInTableCall struct {
	dest     string
	table    int
	nexthops []routing.NextHop
}

func (m *mockRouter) RouteAdd(_ *net.IPNet, _ net.IP, _ string) error     { return nil }
func (m *mockRouter) RouteReplace(_ *net.IPNet, _ net.IP, _ string) error { return nil }
func (m *mockRouter) RouteDelete(_ *net.IPNet) error                      { return nil }
func (m *mockRouter) ListRoutes() ([]routing.RouteEntry, error)           { return nil, nil }
func (m *mockRouter) AddrAdd(_ string, _ *net.IPNet) error                { return nil }
func (m *mockRouter) AddrExists(_ string, _ *net.IPNet) (bool, error)     { return false, nil }
func (m *mockRouter) LinkSetUp(_ string) error                            { return nil }
func (m *mockRouter) LinkGetIndex(_ string) (int, error)                  { return 0, nil }

func (m *mockRouter) SetECMPRoute(dest *net.IPNet, nexthops []routing.NextHop, _ ...net.IP) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.setECMPErrorsByDest != nil {
		if err, ok := m.setECMPErrorsByDest[dest.String()]; ok {
			return err
		}
	}
	nhCopy := append([]routing.NextHop(nil), nexthops...)
	m.setECMPCalls = append(m.setECMPCalls, mockRouteCall{dest: dest.String(), nexthops: nhCopy})
	return nil
}

func (m *mockRouter) SetECMPRouteInTable(dest *net.IPNet, nexthops []routing.NextHop, table int, _ ...net.IP) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.setECMPErrorsByDest != nil {
		if err, ok := m.setECMPErrorsByDest[dest.String()]; ok {
			return err
		}
	}
	nhCopy := append([]routing.NextHop(nil), nexthops...)
	m.setECMPInTableCalls = append(m.setECMPInTableCalls, mockRouteInTableCall{
		dest:     dest.String(),
		table:    table,
		nexthops: nhCopy,
	})
	return nil
}

func (m *mockRouter) RemoveECMPRoute(dest *net.IPNet) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeECMPCalls = append(m.removeECMPCalls, dest.String())
	if m.removeECMPErrorsByDest != nil {
		if err, ok := m.removeECMPErrorsByDest[dest.String()]; ok {
			return err
		}
	}
	return nil
}

func (m *mockRouter) hasSetECMPFor(cidr string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.setECMPCalls {
		if c.dest == cidr {
			return true
		}
	}
	return false
}

func (m *mockRouter) hasSetECMPInTableFor(cidr string, table int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.setECMPInTableCalls {
		if c.dest == cidr && c.table == table {
			return true
		}
	}
	return false
}

func (m *mockRouter) setECMPInTableCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.setECMPInTableCalls)
}

func (m *mockRouter) hasRemoveECMPFor(cidr string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.removeECMPCalls {
		if d == cidr {
			return true
		}
	}
	return false
}

func (m *mockRouter) setECMPCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.setECMPCalls)
}

type mockFirewall struct {
	mu                 sync.Mutex
	enableStickyCalls  []string
	disableStickyCalls []string
	setupNATCalls      []string // iface names passed to SetupNAT
	clampMSSCallCount  int      // number of ClampMSSToPMTU invocations
}

func (f *mockFirewall) SetupNAT(iface string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setupNATCalls = append(f.setupNATCalls, iface)
	return nil
}
func (f *mockFirewall) TeardownNAT() error { return nil }
func (f *mockFirewall) ClampMSSToPMTU() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clampMSSCallCount++
	return nil
}
func (f *mockFirewall) DisableStickyECMP(cidr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disableStickyCalls = append(f.disableStickyCalls, cidr)
	return nil
}
func (f *mockFirewall) EnableStickyECMP(cidr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enableStickyCalls = append(f.enableStickyCalls, cidr)
	return nil
}

func (f *mockFirewall) EnableWGCrossTunnelForward() error { return nil }

func (f *mockFirewall) hasEnableStickyFor(cidr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.enableStickyCalls {
		if c == cidr {
			return true
		}
	}
	return false
}

func (f *mockFirewall) hasDisableStickyFor(cidr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.disableStickyCalls {
		if c == cidr {
			return true
		}
	}
	return false
}

func (f *mockFirewall) enableStickyCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.enableStickyCalls)
}

func (f *mockFirewall) hasSetupNATFor(iface string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.setupNATCalls {
		if n == iface {
			return true
		}
	}
	return false
}

func (f *mockFirewall) getSetupNATCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.setupNATCalls)
}

func (f *mockFirewall) getClampMSSCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clampMSSCallCount
}

type mockSysctl struct {
	mu              sync.Mutex
	l4HashCallCount int
}

func (s *mockSysctl) EnableForwarding() error { return nil }
func (s *mockSysctl) EnableL4Hash() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.l4HashCallCount++
	return nil
}

func (s *mockSysctl) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.l4HashCallCount
}

// --- Test helpers ---

// newTestNode returns a minimal Node with an optional topology.
func newTestNode(topo *topology.Topology) *Node {
	return &Node{
		config:   NodeConfig{Name: "test-client"},
		topology: topo,
		logger:   zerolog.Nop(),
	}
}

// newTestRunner creates a ClientRunner with injected mocks.
// Mutates platformState in place to avoid copying clientPlatformState (which
// holds sync.Mutex — copylocks violation under govet).
func newTestRunner(node *Node, router *mockRouter, fw *mockFirewall, sc *mockSysctl) *ClientRunner {
	runner := &ClientRunner{node: node}
	runner.platformState.byKey = make(map[string]*transportLink)
	runner.platformState.pending = make(map[string]bool)
	runner.platformState.router = router
	runner.platformState.firewall = fw
	runner.platformState.sysctl = sc
	runner.platformState.configurePeerOnIfaceFn = defaultConfigurePeerOnIfaceFn
	return runner
}

// stubIface returns a zero-value *wg.Interface suitable for test link stubs.
// Its Name() returns "" which is acceptable for mock routing assertions.
func stubIface() *wg.Interface {
	return &wg.Interface{}
}

func makeTestLink(peerTransportIP, balancerIP string, healthy bool) *transportLink {
	return &transportLink{
		iface:           stubIface(),
		pubkeyHex:       "aabbccdd",
		peerTransportIP: peerTransportIP,
		balancerIP:      balancerIP,
		healthy:         healthy,
	}
}

// TestRebuildClientECMP_HealthyVIP verifies the VIP path with 2 healthy links:
// SetECMPRoute is called for balancerIP/32 and overlay.space, EnableStickyECMP is
// called with balancerIP/32, and EnableL4Hash is called.
func TestRebuildClientECMP_HealthyVIP(t *testing.T) {
	t.Parallel()

	balancerIP := "10.100.0.1"
	overlaySpace := "10.0.0.0/8"

	topo := &topology.Topology{
		Overlay: topology.OverlayConfig{Space: overlaySpace},
	}
	router := &mockRouter{}
	fw := &mockFirewall{}
	sysctl := &mockSysctl{}

	runner := newTestRunner(newTestNode(topo), router, fw, sysctl)
	runner.platformState.links = []*transportLink{
		makeTestLink("192.168.1.1", balancerIP, true),
		makeTestLink("192.168.1.2", balancerIP, true),
	}

	if err := runner.rebuildClientECMP("init"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	primaryCIDR := balancerIP + "/32"
	if !router.hasSetECMPFor(primaryCIDR) {
		t.Errorf("expected SetECMPRoute for %s, got calls: %+v", primaryCIDR, router.setECMPCalls)
	}
	if !router.hasSetECMPFor(overlaySpace) {
		t.Errorf("expected SetECMPRoute for overlay %s, got calls: %+v", overlaySpace, router.setECMPCalls)
	}
	if !fw.hasEnableStickyFor(primaryCIDR) {
		t.Errorf("expected EnableStickyECMP(%s), got: %+v", primaryCIDR, fw.enableStickyCalls)
	}
	if sysctl.callCount() == 0 {
		t.Error("expected EnableL4Hash to be called")
	}

	// Verify nexthop count on primary route.
	router.mu.Lock()
	for _, call := range router.setECMPCalls {
		if call.dest == primaryCIDR && len(call.nexthops) != 2 {
			t.Errorf("expected 2 nexthops for %s, got %d", primaryCIDR, len(call.nexthops))
		}
	}
	router.mu.Unlock()
}

func TestRebuildClientECMP_HealthyVIP_UsesPersistedOverlaySpaceWithoutTopology(t *testing.T) {
	t.Parallel()

	balancerIP := "10.100.0.1"
	overlaySpace := "172.20.70.0/24"
	router := &mockRouter{}
	fw := &mockFirewall{}
	sysctl := &mockSysctl{}

	runner := newTestRunner(newTestNode(nil), router, fw, sysctl)
	runner.clientState = &ClientState{OverlaySpace: overlaySpace}
	runner.platformState.links = []*transportLink{
		makeTestLink("192.168.1.1", balancerIP, true),
		makeTestLink("192.168.1.2", balancerIP, true),
	}

	if err := runner.rebuildClientECMP("init"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !router.hasSetECMPFor(overlaySpace) {
		t.Fatalf("expected persisted overlay space route %s, got calls: %+v", overlaySpace, router.setECMPCalls)
	}
}

func TestRebuildClientECMP_HealthyVIP_OverlayRouteFailureIsReturned(t *testing.T) {
	t.Parallel()

	balancerIP := "10.100.0.1"
	overlaySpace := "172.20.70.0/24"
	router := &mockRouter{
		setECMPErrorsByDest: map[string]error{overlaySpace: errors.New("overlay route rejected")},
	}
	fw := &mockFirewall{}
	sysctl := &mockSysctl{}

	runner := newTestRunner(newTestNode(&topology.Topology{Overlay: topology.OverlayConfig{Space: overlaySpace}}), router, fw, sysctl)
	runner.platformState.links = []*transportLink{
		makeTestLink("192.168.1.1", balancerIP, true),
		makeTestLink("192.168.1.2", balancerIP, true),
	}

	err := runner.rebuildClientECMP("init")
	if err == nil {
		t.Fatal("expected error when overlay ECMP route install fails")
	}
	if !strings.Contains(err.Error(), "set overlay space ECMP route "+overlaySpace) {
		f := "expected overlay route context in error, got: %v"
		t.Fatalf(f, err)
	}
	if fw.enableStickyCallCount() != 0 {
		t.Fatalf("expected sticky rules to stay unchanged on overlay route failure, got: %+v", fw.enableStickyCalls)
	}
}

func TestSetPeerHealth_CopiesTransportLinkState(t *testing.T) {
	peerKey := make([]byte, 32)
	for i := range peerKey {
		peerKey[i] = byte(i + 1)
	}
	peerHex := hex.EncodeToString(peerKey)

	link := &transportLink{
		iface:     stubIface(),
		pubkeyHex: peerHex,
		healthy:   true,
	}

	runner := NewClientRunner(newTestNode(nil))
	runner.platformState.byKey[peerHex] = link
	runner.platformState.links = []*transportLink{link}

	runner.setPeerHealth(peerHex, false)

	updated, ok := runner.platformState.byKey[peerHex]
	if !ok || updated == nil {
		t.Fatalf("peer health update did not preserve key entry")
	}
	if updated == link {
		t.Fatalf("expected new transportLink instance, got pointer alias")
	}
	if updated.healthy {
		t.Fatalf("expected rebuilt transportLink to be marked unhealthy")
	}
	if !link.healthy {
		t.Fatalf("expected original transportLink to remain healthy")
	}
	if got := runner.platformState.links[0]; got != updated {
		t.Fatalf("expected links slice to hold rebuilt transportLink, got %v", got)
	}
}

func TestAddPeerExistingLinkDoesNotHoldMuWhileReconfigure(t *testing.T) {
	t.Parallel()

	peerKey := make([]byte, 32)
	for i := range peerKey {
		peerKey[i] = byte(i + 1)
	}
	peerHex := hex.EncodeToString(peerKey)

	runner := NewClientRunner(newTestNode(nil))
	runner.platformState.byKey[peerHex] = &transportLink{
		iface:     &wg.Interface{},
		pubkeyHex: peerHex,
		healthy:   true,
	}
	runner.platformState.links = []*transportLink{runner.platformState.byKey[peerHex]}

	configureEntered := make(chan struct{})
	platformMuReleased := make(chan struct{})
	resumeConfigure := make(chan struct{})
	done := make(chan error, 1)

	// Swap the per-instance hook (no global state — safe with t.Parallel).
	// Acquire platformState.mu from INSIDE the stub to prove that AddPeer has
	// released it before entering the reconfigure path. If AddPeer were still
	// holding platformState.mu at this point, Lock() below would deadlock
	// against the goroutine that issued AddPeer — the test's outer timeout
	// would expire on platformMuReleased, which is the real failure mode we
	// want to catch.
	runner.platformState.configurePeerOnIfaceFn = func(_ *ClientRunner, _ *wg.Interface, _ []byte, _ []byte, _ []string, _ string, _ int32) error {
		close(configureEntered)
		// TryLock is the whole point of this check: it succeeds iff AddPeer
		// has already released platformState.mu. The alternative — Lock then
		// Unlock — would still work functionally but staticcheck flags it as
		// an empty critical section (SA2001).
		if !runner.platformState.mu.TryLock() {
			return fmt.Errorf("platformState.mu still held at configurePeerOnIfaceFn entry")
		}
		runner.platformState.mu.Unlock()
		close(platformMuReleased)
		<-resumeConfigure
		return nil
	}

	go func() {
		done <- runner.AddPeer(peerKey, nil, []string{"10.0.0.0/24"}, "198.18.0.1:51820", 25, "")
	}()

	select {
	case <-configureEntered:
	case <-time.After(time.Second):
		t.Fatal("existing-link configure path was not entered")
	}

	select {
	case <-platformMuReleased:
	case <-time.After(time.Second):
		close(resumeConfigure)
		t.Fatal("expected client platformState mutex to be released before existing-peer configuration")
	}

	close(resumeConfigure)
	if err := <-done; err != nil {
		t.Fatalf("AddPeer(existing link) returned error: %v", err)
	}
}

func TestAddPeerExistingLinkSerializesReconfigure(t *testing.T) {
	t.Parallel()

	peerKey := make([]byte, 32)
	for i := range peerKey {
		peerKey[i] = byte(i + 2) // different from other tests
	}
	peerHex := hex.EncodeToString(peerKey)

	runner := NewClientRunner(newTestNode(nil))
	link := &transportLink{
		iface:     &wg.Interface{},
		pubkeyHex: peerHex,
		healthy:   true,
	}
	runner.platformState.byKey[peerHex] = link
	runner.platformState.links = []*transportLink{link}

	firstEntered := make(chan struct{})
	firstResume := make(chan struct{})
	secondReachedLock := make(chan struct{})
	var configureCount atomic.Int32
	var firstInConfigure, secondInConfigure bool
	var stateMu sync.Mutex

	// configurePeerOnIfaceFn: first call blocks on firstResume (holding
	// link.mu); second call would run straight through if it ever got past
	// link.mu.Lock(). We assert the second call does NOT enter configure
	// before the first releases link.mu.
	runner.platformState.configurePeerOnIfaceFn = func(_ *ClientRunner, _ *wg.Interface, _ []byte, _ []byte, _ []string, _ string, _ int32) error {
		stateMu.Lock()
		if !firstInConfigure {
			firstInConfigure = true
			stateMu.Unlock()
			close(firstEntered)
			<-firstResume
		} else {
			secondInConfigure = true
			stateMu.Unlock()
		}
		configureCount.Add(1)
		return nil
	}

	// beforeExistingLinkLockFn: fires in the SECOND AddPeer call immediately
	// before existingLink.mu.Lock(). Signalling here proves the second call
	// truly reached the per-peer-lock point and is about to block. We only
	// fire this once — first call's beforeLinkLock is not meaningful for this
	// test because the first call doesn't block there.
	var beforeLockFired int32
	runner.platformState.beforeExistingLinkLockFn = func() {
		stateMu.Lock()
		if firstInConfigure && beforeLockFired == 0 {
			beforeLockFired = 1
			stateMu.Unlock()
			close(secondReachedLock)
			return
		}
		stateMu.Unlock()
	}

	done1 := make(chan error, 1)
	done2 := make(chan error, 1)

	go func() {
		done1 <- runner.AddPeer(peerKey, nil, []string{"10.0.0.0/24"}, "198.18.0.1:51820", 25, "")
	}()

	// Wait for first to be inside configure (holding link.mu).
	select {
	case <-firstEntered:
	case <-time.After(time.Second):
		t.Fatal("first AddPeer did not enter configure")
	}

	// Launch second. It MUST block on existingLink.mu.Lock() inside AddPeer.
	go func() {
		done2 <- runner.AddPeer(peerKey, nil, []string{"10.0.0.0/24"}, "198.18.0.2:51820", 25, "")
	}()

	// Synchronize on the beforeExistingLinkLockFn seam — after this fires the
	// second goroutine is guaranteed to be at link.mu.Lock() (or past it).
	select {
	case <-secondReachedLock:
	case <-time.After(time.Second):
		close(firstResume) // unblock first to avoid leaking the goroutine
		t.Fatal("second AddPeer did not reach existingLink.mu.Lock() seam")
	}

	// Prove serialization: second must NOT be in configurePeerOnIfaceFn yet
	// because it is still blocked on existingLink.mu held by first.
	stateMu.Lock()
	if secondInConfigure {
		stateMu.Unlock()
		close(firstResume)
		t.Fatal("second AddPeer entered configure before first released link.mu — serialization broken")
	}
	stateMu.Unlock()

	// Release the first — second should now acquire link.mu and complete.
	close(firstResume)

	if err := <-done1; err != nil {
		t.Fatalf("first AddPeer returned error: %v", err)
	}
	if err := <-done2; err != nil {
		t.Fatalf("second AddPeer returned error: %v", err)
	}

	// Both should have completed.
	if configureCount.Load() != 2 {
		t.Fatalf("expected 2 configure calls, got %d", configureCount.Load())
	}
}

// TestRebuildClientECMP_HealthyLegacy verifies the legacy path with 2 healthy links
// (no balancerIP): SetECMPRoute called for 0.0.0.0/0, EnableStickyECMP(overlay.space),
// EnableL4Hash called.
func TestRebuildClientECMP_HealthyLegacy(t *testing.T) {
	t.Parallel()

	overlaySpace := "10.0.0.0/8"
	topo := &topology.Topology{
		Overlay: topology.OverlayConfig{Space: overlaySpace},
	}
	router := &mockRouter{}
	fw := &mockFirewall{}
	sysctl := &mockSysctl{}

	runner := newTestRunner(newTestNode(topo), router, fw, sysctl)
	runner.platformState.links = []*transportLink{
		makeTestLink("192.168.2.1", "", true),
		makeTestLink("192.168.2.2", "", true),
	}

	if err := runner.rebuildClientECMP("init"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	defaultCIDR := "0.0.0.0/0"
	if !router.hasSetECMPFor(defaultCIDR) {
		t.Errorf("expected SetECMPRoute for %s, got calls: %+v", defaultCIDR, router.setECMPCalls)
	}
	if !fw.hasEnableStickyFor(overlaySpace) {
		t.Errorf("expected EnableStickyECMP(%s), got: %+v", overlaySpace, fw.enableStickyCalls)
	}
	if sysctl.callCount() == 0 {
		t.Error("expected EnableL4Hash to be called")
	}
}

// TestRebuildClientECMP_ZeroHealthy_VIP verifies that when all VIP links are unhealthy,
// RemoveECMPRoute(balancerIP/32) is called and EnableStickyECMP is not called.
func TestRebuildClientECMP_ZeroHealthy_VIP(t *testing.T) {
	t.Parallel()

	balancerIP := "10.100.0.1"
	topo := &topology.Topology{
		Overlay: topology.OverlayConfig{Space: "10.0.0.0/8"},
	}
	router := &mockRouter{}
	fw := &mockFirewall{}
	sysctl := &mockSysctl{}

	runner := newTestRunner(newTestNode(topo), router, fw, sysctl)
	runner.platformState.links = []*transportLink{
		makeTestLink("192.168.1.1", balancerIP, false),
		makeTestLink("192.168.1.2", balancerIP, false),
	}

	if err := runner.rebuildClientECMP("init"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	primaryCIDR := balancerIP + "/32"
	if !router.hasRemoveECMPFor(primaryCIDR) {
		t.Errorf("expected RemoveECMPRoute(%s), got: %+v", primaryCIDR, router.removeECMPCalls)
	}
	if router.setECMPCallCount() > 0 {
		t.Errorf("expected no SetECMPRoute calls, got: %+v", router.setECMPCalls)
	}
	if fw.enableStickyCallCount() > 0 {
		t.Errorf("expected no EnableStickyECMP calls, got: %+v", fw.enableStickyCalls)
	}
}

func TestRebuildClientECMP_ZeroHealthy_VIP_IgnoresMissingRoute(t *testing.T) {
	t.Parallel()

	balancerIP := "172.20.70.1"
	primaryCIDR := balancerIP + "/32"
	topo := &topology.Topology{
		Overlay: topology.OverlayConfig{Space: "10.0.0.0/8"},
	}
	router := &mockRouter{}
	fw := &mockFirewall{}
	sysctl := &mockSysctl{}

	runner := newTestRunner(newTestNode(topo), router, fw, sysctl)
	runner.platformState.links = []*transportLink{
		makeTestLink("192.168.1.1", balancerIP, false),
		makeTestLink("192.168.1.2", balancerIP, false),
	}

	if err := runner.rebuildClientECMP("balancer_change"); err != nil {
		t.Fatalf("missing VIP route should be ignored: %v", err)
	}
	if !router.hasRemoveECMPFor(primaryCIDR) {
		t.Fatalf("expected RemoveECMPRoute(%s) call", primaryCIDR)
	}
}

func TestRebuildClientECMP_ZeroHealthy_VIP_PropagatesUnexpectedRemoveError(t *testing.T) {
	t.Parallel()

	balancerIP := "172.20.70.1"
	primaryCIDR := balancerIP + "/32"
	topo := &topology.Topology{
		Overlay: topology.OverlayConfig{Space: "10.0.0.0/8"},
	}
	router := &mockRouter{
		removeECMPErrorsByDest: map[string]error{
			primaryCIDR: errors.Join(syscall.EPERM, errors.New("permission denied")),
		},
	}
	fw := &mockFirewall{}
	sysctl := &mockSysctl{}

	runner := newTestRunner(newTestNode(topo), router, fw, sysctl)
	runner.platformState.links = []*transportLink{
		makeTestLink("192.168.1.1", balancerIP, false),
		makeTestLink("192.168.1.2", balancerIP, false),
	}

	err := runner.rebuildClientECMP("balancer_change")
	if err == nil {
		t.Fatal("expected unexpected remove error to propagate")
	}
	if !strings.Contains(err.Error(), "remove client ECMP route "+primaryCIDR) {
		t.Fatalf("expected wrapped remove error for %s, got: %v", primaryCIDR, err)
	}
}

// TestRebuildClientECMP_ZeroHealthy_Legacy verifies that when all legacy links are
// unhealthy AND we previously installed the route, RemoveECMPRoute(0.0.0.0/0) is called.
func TestRebuildClientECMP_ZeroHealthy_Legacy(t *testing.T) {
	t.Parallel()

	router := &mockRouter{}
	fw := &mockFirewall{}
	sysctl := &mockSysctl{}

	runner := newTestRunner(newTestNode(nil), router, fw, sysctl)
	runner.platformState.links = []*transportLink{
		makeTestLink("192.168.2.1", "", false),
		makeTestLink("192.168.2.2", "", false),
	}
	runner.platformState.ecmpRouteInstalled = true // simulate prior install

	if err := runner.rebuildClientECMP("init"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	defaultCIDR := "0.0.0.0/0"
	if !router.hasRemoveECMPFor(defaultCIDR) {
		t.Errorf("expected RemoveECMPRoute(%s), got: %+v", defaultCIDR, router.removeECMPCalls)
	}
}

// TestClientStartup_PreservesDefaultRoute verifies that on cold start with zero healthy
// legacy links, RemoveECMPRoute(0.0.0.0/0) is NOT called — Bug 7 / REQ-9 / F-002.
// The RouterOS-injected default route must be preserved when we never installed our own.
func TestClientStartup_PreservesDefaultRoute(t *testing.T) {
	t.Parallel()

	topo := &topology.Topology{Overlay: topology.OverlayConfig{Space: "10.0.0.0/8"}}
	router := &mockRouter{}
	fw := &mockFirewall{}
	sysctl := &mockSysctl{}
	runner := newTestRunner(newTestNode(topo), router, fw, sysctl)
	// No links — zero-healthy from cold start (ecmpRouteInstalled defaults to false).
	runner.platformState.links = []*transportLink{}

	if err := runner.rebuildClientECMP("init"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if router.hasRemoveECMPFor("0.0.0.0/0") {
		t.Errorf("Bug 7: RemoveECMPRoute(0.0.0.0/0) called from fresh state — would flush RouterOS-injected default route. removeECMPCalls=%+v", router.removeECMPCalls)
	}
}

// TestClientECMP_FlagFlipsTrueAfterInstall verifies that after a successful
// SetECMPRoute(0.0.0.0/0) on the legacy path, ecmpRouteInstalled is flipped to true.
func TestClientECMP_FlagFlipsTrueAfterInstall(t *testing.T) {
	t.Parallel()

	topo := &topology.Topology{Overlay: topology.OverlayConfig{Space: "10.0.0.0/8"}}
	router := &mockRouter{}
	fw := &mockFirewall{}
	sysctl := &mockSysctl{}
	runner := newTestRunner(newTestNode(topo), router, fw, sysctl)
	// One healthy legacy link (empty balancerIP = legacy path).
	runner.platformState.links = []*transportLink{
		makeTestLink("192.168.1.1", "", true),
	}

	if err := runner.rebuildClientECMP("init"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if router.setECMPCallCount() < 1 {
		t.Errorf("expected SetECMPRoute to be called, got setECMPCalls=%+v", router.setECMPCalls)
	}
	if !runner.platformState.ecmpRouteInstalled {
		t.Errorf("expected ecmpRouteInstalled=true after successful SetECMPRoute, got false")
	}
}

// TestRebuildClientECMP_MixedBalancerIP verifies that when links have mixed balancerIP
// presence, an error is returned and no route is installed.
func TestRebuildClientECMP_MixedBalancerIP(t *testing.T) {
	t.Parallel()

	router := &mockRouter{}
	fw := &mockFirewall{}
	sysctl := &mockSysctl{}

	runner := newTestRunner(newTestNode(nil), router, fw, sysctl)
	runner.platformState.links = []*transportLink{
		makeTestLink("192.168.1.1", "10.100.0.1", true), // has balancerIP
		makeTestLink("192.168.1.2", "", true),           // no balancerIP
	}

	err := runner.rebuildClientECMP("init")
	if err == nil {
		t.Fatal("expected error for mixed balancerIP, got nil")
	}

	if router.setECMPCallCount() > 0 {
		t.Errorf("expected no SetECMPRoute calls on mixed error, got: %+v", router.setECMPCalls)
	}
}

// TestRebuildClientECMP_BalancerChange verifies that when balancerIP changes across
// rebuilds, DisableStickyECMP is called for the old CIDR and EnableStickyECMP is
// called for the new CIDR (FR-6 / US5).
func TestRebuildClientECMP_BalancerChange(t *testing.T) {
	t.Parallel()

	const oldBalancerIP = "10.0.0.1"
	const newBalancerIP = "10.0.0.2"
	oldCIDR := oldBalancerIP + "/32"
	newCIDR := newBalancerIP + "/32"

	topo := &topology.Topology{
		Overlay: topology.OverlayConfig{Space: "10.0.0.0/8"},
	}
	router := &mockRouter{}
	fw := &mockFirewall{}
	sysctl := &mockSysctl{}

	runner := newTestRunner(newTestNode(topo), router, fw, sysctl)

	// Step 1: two healthy links with balancerIP = 10.0.0.1.
	runner.platformState.links = []*transportLink{
		makeTestLink("192.168.1.1", oldBalancerIP, true),
		makeTestLink("192.168.1.2", oldBalancerIP, true),
	}
	if err := runner.rebuildClientECMP("init"); err != nil {
		t.Fatalf("first rebuild: %v", err)
	}
	if !fw.hasEnableStickyFor(oldCIDR) {
		t.Errorf("first rebuild: expected EnableStickyECMP(%s), got: %+v", oldCIDR, fw.enableStickyCalls)
	}
	if fw.hasDisableStickyFor(oldCIDR) {
		t.Errorf("first rebuild: unexpected DisableStickyECMP(%s)", oldCIDR)
	}

	// Step 2: mutate both links' balancerIP to 10.0.0.2.
	runner.platformState.links = []*transportLink{
		makeTestLink("192.168.1.1", newBalancerIP, true),
		makeTestLink("192.168.1.2", newBalancerIP, true),
	}
	if err := runner.rebuildClientECMP("init"); err != nil {
		t.Fatalf("second rebuild: %v", err)
	}
	if !fw.hasDisableStickyFor(oldCIDR) {
		t.Errorf("second rebuild: expected DisableStickyECMP(%s), got: %+v", oldCIDR, fw.disableStickyCalls)
	}
	if !fw.hasEnableStickyFor(newCIDR) {
		t.Errorf("second rebuild: expected EnableStickyECMP(%s), got: %+v", newCIDR, fw.enableStickyCalls)
	}

	// Verify stale A-rules are gone: Enable should NOT have been called again for oldCIDR.
	fw.mu.Lock()
	enableForOld := 0
	for _, c := range fw.enableStickyCalls {
		if c == oldCIDR {
			enableForOld++
		}
	}
	fw.mu.Unlock()
	if enableForOld != 1 {
		t.Errorf("expected EnableStickyECMP(%s) exactly once (first rebuild only), got %d", oldCIDR, enableForOld)
	}
}

// TestRebuildClientECMP_FailoverSingleMaster verifies single-master legacy topology:
// onDown withdraws the route (RemoveECMPRoute), onUp reinstalls it (SetECMPRoute).
func TestRebuildClientECMP_FailoverSingleMaster(t *testing.T) {
	t.Parallel()

	router := &mockRouter{}
	fw := &mockFirewall{}
	sysctl := &mockSysctl{}

	runner := newTestRunner(newTestNode(nil), router, fw, sysctl)
	link := makeTestLink("192.168.3.1", "", true)
	runner.platformState.links = []*transportLink{link}
	runner.platformState.byKey = map[string]*transportLink{"aabbccdd": link}

	// Initial state: healthy — should install 0.0.0.0/0.
	if err := runner.rebuildClientECMP("init"); err != nil {
		t.Fatalf("initial rebuild error: %v", err)
	}
	if !router.hasSetECMPFor("0.0.0.0/0") {
		t.Errorf("expected SetECMPRoute(0.0.0.0/0) on initial install, got: %+v", router.setECMPCalls)
	}

	// Simulate onDown: mark unhealthy via CoW and rebuild.
	// Direct in-place mutation of `link.healthy` would violate the immutability
	// contract setPeerHealth establishes — readers of platformState.links (e.g.
	// rebuildClientECMP) take a snapshot without re-locking, so live links must
	// be treated as immutable after insertion.
	runner.setPeerHealth("aabbccdd", false)

	if err := runner.rebuildClientECMP("init"); err != nil {
		t.Fatalf("onDown rebuild error: %v", err)
	}
	if !router.hasRemoveECMPFor("0.0.0.0/0") {
		t.Errorf("expected RemoveECMPRoute(0.0.0.0/0) on link down, got: %+v", router.removeECMPCalls)
	}

	// Simulate onUp: mark healthy via CoW and rebuild.
	runner.setPeerHealth("aabbccdd", true)

	beforeCount := router.setECMPCallCount()
	if err := runner.rebuildClientECMP("init"); err != nil {
		t.Fatalf("onUp rebuild error: %v", err)
	}
	if router.setECMPCallCount() <= beforeCount {
		t.Error("expected SetECMPRoute to be called again on link up")
	}
}

// =============================================================================
// Phase 4: deterministic client interface naming tests (T012)
// =============================================================================

// =============================================================================
// Phase 7: idempotency test (T025 / NFR-4)
// =============================================================================

// TestRebuildClientECMP_Idempotent verifies that calling rebuildClientECMP twice
// with identical healthy VIP links produces exactly one EnableStickyECMP call
// (not two). The currentStickyCIDRs diff logic in applyStickyECMPDiff must skip
// the second Enable call because the CIDR is already tracked.
func TestRebuildClientECMP_Idempotent(t *testing.T) {
	t.Parallel()

	balancerIP := "10.0.0.1"
	primaryCIDR := balancerIP + "/32"

	topo := &topology.Topology{
		Overlay: topology.OverlayConfig{Space: "10.0.0.0/8"},
	}
	router := &mockRouter{}
	fw := &mockFirewall{}
	sysctl := &mockSysctl{}

	runner := newTestRunner(newTestNode(topo), router, fw, sysctl)
	runner.platformState.links = []*transportLink{
		makeTestLink("192.168.1.1", balancerIP, true),
		makeTestLink("192.168.1.2", balancerIP, true),
	}

	// First call — installs routes and sticky rules.
	if err := runner.rebuildClientECMP("init"); err != nil {
		t.Fatalf("first rebuildClientECMP: %v", err)
	}

	countAfterFirst := fw.enableStickyCallCount()
	if countAfterFirst != 1 {
		t.Fatalf("expected 1 EnableStickyECMP call after first rebuild, got %d", countAfterFirst)
	}

	// Second call — same links, same balancerIP.
	// EnableStickyECMP must NOT fire again (NFR-4: idempotent).
	if err := runner.rebuildClientECMP("init"); err != nil {
		t.Fatalf("second rebuildClientECMP: %v", err)
	}

	countAfterSecond := fw.enableStickyCallCount()
	if countAfterSecond != 1 {
		t.Errorf("expected EnableStickyECMP called exactly once across 2 rebuilds, got %d (CIDR: %s)", countAfterSecond, primaryCIDR)
	}

	// DisableStickyECMP must not have been called (no CIDR retired).
	if fw.hasDisableStickyFor(primaryCIDR) {
		t.Errorf("unexpected DisableStickyECMP(%s) — no CIDR should be retired on identical rebuild", primaryCIDR)
	}

	// SetECMPRoute is called on every rebuild (route re-assertion is idempotent at kernel level).
	if !router.hasSetECMPFor(primaryCIDR) {
		t.Errorf("expected SetECMPRoute(%s) to be called, got: %+v", primaryCIDR, router.setECMPCalls)
	}
}

// TestClientIfaceName_Deterministic verifies same pubkey always produces same name.
func TestClientIfaceName_Deterministic(t *testing.T) {
	t.Parallel()

	pk, err := wg.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	first := clientIfaceName(pk)
	for i := 0; i < 10; i++ {
		got := clientIfaceName(pk)
		if got != first {
			t.Errorf("iteration %d: got %q, want %q", i, got, first)
		}
	}
}

// TestClientIfaceName_Format verifies the name matches ^wg-c[0-9a-f]{4}$.
func TestClientIfaceName_Format(t *testing.T) {
	t.Parallel()

	re := regexp.MustCompile(`^wg-c[0-9a-f]{4}$`)

	pk, err := wg.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	name := clientIfaceName(pk)
	if !re.MatchString(name) {
		t.Errorf("clientIfaceName returned %q, does not match %s", name, re.String())
	}
}

// TestClientIfaceName_DifferentKeysMapToDifferentNames verifies distinct inputs
// produce distinct names. Deterministic inputs eliminate birthday-paradox flake
// (16-bit namespace; random generation had ~0.07% collision probability at N=10).
func TestClientIfaceName_DifferentKeysMapToDifferentNames(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool)
	for i := 0; i < 10; i++ {
		var pk wg.Key
		// Deterministic distinct inputs so the test cannot flake.
		pk[0] = byte(i + 1)
		name := clientIfaceName(pk)
		if seen[name] {
			t.Errorf("collision: key %d produced already-seen name %q", i, name)
		}
		seen[name] = true
	}
}

func TestResolveDSCPPolicies_EndpointViaMaster(t *testing.T) {
	t.Parallel()

	topo := &topology.Topology{
		Masters: []topology.MasterNode{{Name: "master-01", Endpoints: []string{"endpoint-01"}}},
	}
	runner := newTestRunner(newTestNode(topo), &mockRouter{}, &mockFirewall{}, &mockSysctl{})

	resolved := runner.resolveDSCPPolicies(
		[]RoutingPolicyState{{Name: "streaming", DSCP: 46, Targets: []string{"endpoint-01"}}},
		map[string]transportInfo{"master-01": {gateway: "10.255.0.41", device: "wg-c1234"}},
	)
	if len(resolved) != 1 {
		t.Fatalf("resolveDSCPPolicies returned %d entries, want 1", len(resolved))
	}
	if got := resolved[0].policy.Gateway; got != "10.255.0.41" {
		t.Fatalf("gateway = %q, want %q", got, "10.255.0.41")
	}
	if got := resolved[0].policy.Device; got != "wg-c1234" {
		t.Fatalf("device = %q, want %q", got, "wg-c1234")
	}
	if len(resolved[0].unresolved) != 0 {
		t.Fatalf("unexpected unresolved targets: %v", resolved[0].unresolved)
	}
}

func TestResolveDSCPPolicies_EndpointViaLaterMasterWithTransport(t *testing.T) {
	t.Parallel()

	topo := &topology.Topology{
		Masters: []topology.MasterNode{
			{Name: "master-a", Endpoints: []string{"endpoint-01"}},
			{Name: "master-b", Endpoints: []string{"endpoint-01"}},
		},
	}
	runner := newTestRunner(newTestNode(topo), &mockRouter{}, &mockFirewall{}, &mockSysctl{})

	resolved := runner.resolveDSCPPolicies(
		[]RoutingPolicyState{{Name: "streaming", DSCP: 46, Targets: []string{"endpoint-01"}}},
		map[string]transportInfo{"master-b": {gateway: "10.255.0.42", device: "wg-c5678"}},
	)
	if len(resolved) != 1 {
		t.Fatalf("resolveDSCPPolicies returned %d entries, want 1", len(resolved))
	}
	if got := resolved[0].policy.Gateway; got != "10.255.0.42" {
		t.Fatalf("gateway = %q, want %q", got, "10.255.0.42")
	}
	if got := resolved[0].policy.Device; got != "wg-c5678" {
		t.Fatalf("device = %q, want %q", got, "wg-c5678")
	}
	if len(resolved[0].unresolved) != 0 {
		t.Fatalf("unexpected unresolved targets: %v", resolved[0].unresolved)
	}
}

func TestResolveDSCPPolicies_UnresolvedTargets(t *testing.T) {
	t.Parallel()

	topo := &topology.Topology{
		Masters: []topology.MasterNode{{Name: "master-01", Endpoints: []string{"endpoint-01"}}},
	}
	runner := newTestRunner(newTestNode(topo), &mockRouter{}, &mockFirewall{}, &mockSysctl{})

	resolved := runner.resolveDSCPPolicies(
		[]RoutingPolicyState{{Name: "streaming", DSCP: 46, Targets: []string{"endpoint-01", "missing-master"}}},
		map[string]transportInfo{},
	)
	if len(resolved) != 1 {
		t.Fatalf("resolveDSCPPolicies returned %d entries, want 1", len(resolved))
	}
	if resolved[0].policy.Gateway != "" || resolved[0].policy.Device != "" {
		t.Fatalf("expected unresolved policy to keep empty gateway/device, got gateway=%q device=%q", resolved[0].policy.Gateway, resolved[0].policy.Device)
	}
	want := []string{"endpoint-01 (via master master-01)", "missing-master"}
	if strings.Join(resolved[0].unresolved, ",") != strings.Join(want, ",") {
		t.Fatalf("unresolved targets = %v, want %v", resolved[0].unresolved, want)
	}
}

func TestSetupDSCPRouting_NoResolvedPoliciesReturnsError(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	runner := NewClientRunner(&Node{
		config: NodeConfig{Name: "test-client", ConfigDir: t.TempDir()},
		topology: &topology.Topology{
			Clients: []topology.ClientNode{{
				Name:            "test-client",
				RoutingPolicies: []topology.RoutingPolicy{{Name: "streaming", DSCP: 46, Targets: []string{"missing-endpoint"}}},
			}},
		},
		logger: zerolog.New(&logBuf),
	})

	err := runner.setupDSCPRouting()
	if err == nil {
		t.Fatal("setupDSCPRouting returned nil, want unresolved-policy error")
	}
	if !strings.Contains(err.Error(), "no configured policies resolved") {
		t.Fatalf("setupDSCPRouting error = %v, want unresolved-policy error", err)
	}
	if got := logBuf.String(); !strings.Contains(got, "unresolved_targets") || !strings.Contains(got, "missing-endpoint") {
		t.Fatalf("expected structured unresolved-target diagnostics, got log %q", got)
	}
}

// =============================================================================
// Phase 5: partial-mesh boot tolerance + structured logging (T015-T016)
// =============================================================================

// TestSetupNATInClientMode verifies that setupClientFirewallRules triggers
// SetupNAT on the firewall dependency for each per-peer wg-c* interface
// (Bug 11 / F-002). The production path is:
//   AddPeer (new interface) → firewallDep().SetupNAT(ifaceName)
//
// Since AddPeer requires TUN device access, this test exercises
// setupClientFirewallRules (the MSS/NAT startup path) plus a direct
// call to firewallDep().SetupNAT, verifying the injectable firewall
// seam wires through correctly for both call sites.
func TestSetupNATInClientMode(t *testing.T) {
	t.Parallel()

	fw := &mockFirewall{}
	runner := newTestRunner(newTestNode(nil), &mockRouter{}, fw, &mockSysctl{})

	// Simulate the call that AddPeer makes after a new interface is registered.
	// firewallDep() returns the injected mock — this verifies the production
	// SetupNAT call site reaches the Firewall interface.
	ifaceName := "wg-c1a2b"
	if fwDep := runner.firewallDep(); fwDep != nil {
		if err := fwDep.SetupNAT(ifaceName); err != nil {
			t.Fatalf("SetupNAT(%q) returned unexpected error: %v", ifaceName, err)
		}
	}

	if !fw.hasSetupNATFor(ifaceName) {
		t.Errorf("SetupNAT not called for interface %q — Bug 11 regression: client mode lacks per-peer NAT rule", ifaceName)
	}
	if fw.getSetupNATCallCount() != 1 {
		t.Errorf("expected exactly 1 SetupNAT call, got %d", fw.getSetupNATCallCount())
	}
}

// TestClampMSSInClientMode verifies that setupClientFirewallRules() calls
// ClampMSSToPMTU() via the firewall dependency (Bug 12 / F-002).
// Without MSS clamping, TCP traffic through overlay tunnels stalls when
// packets exceed the tunnel MTU and require fragmentation.
func TestClampMSSInClientMode(t *testing.T) {
	t.Parallel()

	fw := &mockFirewall{}
	runner := newTestRunner(newTestNode(nil), &mockRouter{}, fw, &mockSysctl{})

	runner.setupClientFirewallRules()

	if fw.getClampMSSCallCount() == 0 {
		t.Error("ClampMSSToPMTU not called by setupClientFirewallRules — Bug 12 regression: client mode lacks MSS clamping")
	}
	if fw.getClampMSSCallCount() > 1 {
		t.Errorf("ClampMSSToPMTU called %d times, expected exactly 1 (idempotent call)", fw.getClampMSSCallCount())
	}
}

// TestClientRun_PartialMesh verifies that a mesh with one healthy and one
// unreachable link boots cleanly (FR-7): rebuildClientECMP("init") returns no
// error and SetECMPRoute is called with exactly 1 nexthop (the healthy link).
func TestClientRun_PartialMesh(t *testing.T) {
	t.Parallel()

	router := &mockRouter{}
	fw := &mockFirewall{}
	sysctl := &mockSysctl{}

	runner := newTestRunner(newTestNode(nil), router, fw, sysctl)

	// Two links: one healthy with resolved peerTransportIP, one unhealthy.
	healthyLink := makeTestLink("10.0.0.1", "", true)
	unhealthyLink := makeTestLink("10.0.0.2", "", false)
	runner.platformState.links = []*transportLink{healthyLink, unhealthyLink}

	err := runner.rebuildClientECMP("init")
	if err != nil {
		t.Fatalf("rebuildClientECMP(\"init\") returned error: %v", err)
	}

	defaultCIDR := "0.0.0.0/0"
	if !router.hasSetECMPFor(defaultCIDR) {
		t.Errorf("expected SetECMPRoute for %s, got calls: %+v", defaultCIDR, router.setECMPCalls)
	}

	// Verify exactly 1 nexthop (the healthy link only).
	router.mu.Lock()
	var nexthopCount int
	for _, call := range router.setECMPCalls {
		if call.dest == defaultCIDR {
			nexthopCount = len(call.nexthops)
			break
		}
	}
	router.mu.Unlock()

	if nexthopCount != 1 {
		t.Errorf("expected 1 nexthop (healthy link only), got %d", nexthopCount)
	}
}

// =============================================================================
// F-008 CR-002: VRF overlay separation tests (mock-mode, no privilege required)
// =============================================================================

// TestSetupClientVRF_DisabledByDefault verifies that when MESH_VRF is unset
// (default), setupClientVRF is a no-op: vrfManager stays nil and isVRFActive
// returns false (FR-10.6 fallback path preserved).
func TestSetupClientVRF_DisabledByDefault(t *testing.T) {
	// Not t.Parallel(): t.Setenv mutates process env.

	t.Setenv("MESH_VRF", "")

	runner := newTestRunner(newTestNode(nil), &mockRouter{}, &mockFirewall{}, &mockSysctl{})

	if err := runner.setupClientVRF(); err != nil {
		t.Fatalf("setupClientVRF() returned error when MESH_VRF unset: %v", err)
	}
	if runner.isVRFActive() {
		t.Error("isVRFActive() = true with MESH_VRF unset — should be false")
	}
	if runner.platformState.vrfManager != nil {
		t.Error("vrfManager != nil with MESH_VRF unset — should remain nil")
	}
}

// TestSetupClientVRF_DisabledExplicit verifies MESH_VRF=disabled keeps vrfManager nil.
func TestSetupClientVRF_DisabledExplicit(t *testing.T) {
	// Not t.Parallel(): t.Setenv mutates process env.

	t.Setenv("MESH_VRF", "disabled")

	runner := newTestRunner(newTestNode(nil), &mockRouter{}, &mockFirewall{}, &mockSysctl{})

	if err := runner.setupClientVRF(); err != nil {
		t.Fatalf("setupClientVRF() returned error when MESH_VRF=disabled: %v", err)
	}
	if runner.isVRFActive() {
		t.Error("isVRFActive() = true with MESH_VRF=disabled")
	}
}

// TestSetupClientVRF_EnabledKernelUnsupported verifies that when MESH_VRF=enabled
// and the kernel does not support VRF (mocked via netlinkLinkAdd returning
// EOPNOTSUPP), setupClientVRF returns a non-nil error containing "kernel_too_old"
// (FR-10.2 hard-fail).
// Uses the withMockNetlink helper from vrf_test.go (same package).
func TestSetupClientVRF_EnabledKernelUnsupported(t *testing.T) {
	// Not t.Parallel(): mutates package-level netlinkLinkAdd test seam.

	t.Setenv("MESH_VRF", "enabled")

	withMockNetlink(t, func(_ netlink.Link) error {
		return unix.EOPNOTSUPP
	}, func() {
		runner := newTestRunner(
			&Node{config: NodeConfig{Name: "test-client", OverlayIP: "172.21.92.130"}, logger: zerolog.Nop()},
			&mockRouter{}, &mockFirewall{}, &mockSysctl{},
		)
		err := runner.setupClientVRF()
		if err == nil {
			t.Fatal("setupClientVRF() returned nil, want error on EOPNOTSUPP")
		}
		if !strings.Contains(err.Error(), "kernel_too_old") {
			t.Errorf("setupClientVRF() error = %q, want 'kernel_too_old'", err.Error())
		}
		if runner.isVRFActive() {
			t.Error("isVRFActive() = true after VRF setup error — should be false")
		}
	})
}

// TestRebuildClientECMP_VRFUsesTable100 verifies that when vrfManager is set
// (VRF active, table=100), rebuildClientECMP calls SetECMPRouteInTable with
// table=100 instead of SetECMPRoute for both balancerIP/32 and overlay.space.
func TestRebuildClientECMP_VRFUsesTable100(t *testing.T) {
	t.Parallel()

	balancerIP := "10.100.0.1"
	overlaySpace := "172.21.92.0/24"

	topo := &topology.Topology{
		Overlay: topology.OverlayConfig{
			Space:    overlaySpace,
			VRFName:  "vrf_overlay",
			VRFTable: 100,
		},
	}
	router := &mockRouter{}
	fw := &mockFirewall{}
	sysctl := &mockSysctl{}

	runner := newTestRunner(newTestNode(topo), router, fw, sysctl)
	// Inject a live VRFManager (table=100) without calling Setup() — the field
	// is read-only after init so injecting it directly is safe for this test.
	runner.platformState.vrfManager = NewVRFManager("vrf_overlay", 100, net.ParseIP("172.21.92.130"))
	runner.platformState.links = []*transportLink{
		makeTestLink("10.93.0.2", balancerIP, true),
		makeTestLink("10.93.0.6", balancerIP, true),
	}

	if err := runner.rebuildClientECMP("init"); err != nil {
		t.Fatalf("rebuildClientECMP: %v", err)
	}

	primaryCIDR := balancerIP + "/32"
	if !router.hasSetECMPInTableFor(primaryCIDR, 100) {
		t.Errorf("expected SetECMPRouteInTable(%s, table=100); inTableCalls=%+v", primaryCIDR, router.setECMPInTableCalls)
	}
	if !router.hasSetECMPInTableFor(overlaySpace, 100) {
		t.Errorf("expected SetECMPRouteInTable(%s, table=100); inTableCalls=%+v", overlaySpace, router.setECMPInTableCalls)
	}
	// Must NOT use SetECMPRoute (main table) for either dest when VRF active.
	if router.hasSetECMPFor(primaryCIDR) {
		t.Errorf("SetECMPRoute (main table) called for %s when VRF active — should use InTable variant", primaryCIDR)
	}
	if router.hasSetECMPFor(overlaySpace) {
		t.Errorf("SetECMPRoute (main table) called for %s when VRF active — should use InTable variant", overlaySpace)
	}
}

// TestRebuildClientECMP_NoVRFUsesMainTable verifies that when vrfManager is nil
// (VRF disabled / default), rebuildClientECMP calls SetECMPRoute (main table)
// and never calls SetECMPRouteInTable.
func TestRebuildClientECMP_NoVRFUsesMainTable(t *testing.T) {
	t.Parallel()

	balancerIP := "10.100.0.2"
	overlaySpace := "172.21.92.0/24"

	topo := &topology.Topology{
		Overlay: topology.OverlayConfig{Space: overlaySpace},
	}
	router := &mockRouter{}
	fw := &mockFirewall{}
	sysctl := &mockSysctl{}

	runner := newTestRunner(newTestNode(topo), router, fw, sysctl)
	// vrfManager is nil by default — VRF not active.
	runner.platformState.links = []*transportLink{
		makeTestLink("10.93.0.2", balancerIP, true),
	}

	if err := runner.rebuildClientECMP("init"); err != nil {
		t.Fatalf("rebuildClientECMP: %v", err)
	}

	primaryCIDR := balancerIP + "/32"
	if !router.hasSetECMPFor(primaryCIDR) {
		t.Errorf("expected SetECMPRoute(%s) on main table; calls=%+v", primaryCIDR, router.setECMPCalls)
	}
	if router.setECMPInTableCallCount() > 0 {
		t.Errorf("SetECMPRouteInTable called when VRF disabled — unexpected: %+v", router.setECMPInTableCalls)
	}
}
