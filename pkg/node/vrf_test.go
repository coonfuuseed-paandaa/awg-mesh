//go:build linux

package node

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// requireRoot skips the test if the process is not running as root.
func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root (CAP_NET_ADMIN) — run with sudo")
	}
}

// cleanupVRF removes VRF and anchor interfaces created during a privileged test.
func cleanupVRF(t *testing.T, mgr *VRFManager) {
	t.Helper()
	if err := mgr.Teardown(); err != nil {
		t.Logf("cleanup Teardown() error (non-fatal): %v", err)
	}
}

// TestVRFManagerSetupIdempotent verifies that calling Setup() twice on the
// same VRF does not duplicate the VRF link and both calls return nil.
func TestVRFManagerSetupIdempotent(t *testing.T) {
	requireRoot(t)

	name := fmt.Sprintf("vrf_ti%d", os.Getpid()%9999)
	mgr := NewVRFManager(name, 10001, net.ParseIP("172.21.99.1"))
	t.Cleanup(func() { cleanupVRF(t, mgr) })

	if err := mgr.Setup(); err != nil {
		t.Fatalf("first Setup() failed: %v", err)
	}

	if err := mgr.Setup(); err != nil {
		t.Fatalf("second Setup() (idempotent) failed: %v", err)
	}

	// Assert exactly one VRF link with our name exists.
	link, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("VRF link %q not found after double Setup: %v", name, err)
	}
	vrf, ok := link.(*netlink.Vrf)
	if !ok {
		t.Fatalf("link %q is %T, want *netlink.Vrf", name, link)
	}
	if vrf.Table != 10001 {
		t.Errorf("VRF table = %d, want 10001", vrf.Table)
	}
	if !mgr.IsCreated() {
		t.Error("IsCreated() = false after Setup()")
	}
}

// TestVRFManagerEnslaveInterface verifies that EnslaveInterface correctly
// wires a dummy interface under the VRF master device.
func TestVRFManagerEnslaveInterface(t *testing.T) {
	requireRoot(t)

	vrfName := fmt.Sprintf("vrf_en%d", os.Getpid()%9999)
	dummyName := fmt.Sprintf("vrftd%d", os.Getpid()%9999)

	mgr := NewVRFManager(vrfName, 10002, net.ParseIP("172.21.99.2"))
	t.Cleanup(func() { cleanupVRF(t, mgr) })

	if err := mgr.Setup(); err != nil {
		t.Fatalf("Setup() failed: %v", err)
	}

	// Create a dummy interface to enslave.
	dummy := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: dummyName}}
	if err := netlink.LinkAdd(dummy); err != nil {
		t.Fatalf("create test dummy %q: %v", dummyName, err)
	}
	t.Cleanup(func() {
		if link, err := netlink.LinkByName(dummyName); err == nil {
			_ = netlink.LinkDel(link)
		}
	})

	if err := mgr.EnslaveInterface(dummyName); err != nil {
		t.Fatalf("EnslaveInterface(%q) failed: %v", dummyName, err)
	}

	// Verify the dummy's master index matches the VRF's index.
	vrfLink, err := netlink.LinkByName(vrfName)
	if err != nil {
		t.Fatalf("get VRF link %q: %v", vrfName, err)
	}
	dummyLink, err := netlink.LinkByName(dummyName)
	if err != nil {
		t.Fatalf("get dummy link %q: %v", dummyName, err)
	}
	if dummyLink.Attrs().MasterIndex != vrfLink.Attrs().Index {
		t.Errorf("dummy MasterIndex = %d, want %d (VRF index)",
			dummyLink.Attrs().MasterIndex, vrfLink.Attrs().Index)
	}
}

// TestVRFManagerOverlayIPOnAnchor verifies that Setup() assigns the overlay
// IP to the anchor dummy interface with a /32 mask.
func TestVRFManagerOverlayIPOnAnchor(t *testing.T) {
	requireRoot(t)

	vrfName := fmt.Sprintf("vrf_ov%d", os.Getpid()%9999)
	overlayIP := net.ParseIP("172.21.92.130")

	mgr := NewVRFManager(vrfName, 10003, overlayIP)
	t.Cleanup(func() { cleanupVRF(t, mgr) })

	if err := mgr.Setup(); err != nil {
		t.Fatalf("Setup() failed: %v", err)
	}

	anchorLink, err := netlink.LinkByName(mgr.anchorName)
	if err != nil {
		t.Fatalf("get anchor %q: %v", mgr.anchorName, err)
	}

	addrs, err := netlink.AddrList(anchorLink, netlink.FAMILY_V4)
	if err != nil {
		t.Fatalf("AddrList(%q): %v", mgr.anchorName, err)
	}

	found := false
	for _, a := range addrs {
		ones, bits := a.Mask.Size()
		if a.IP.Equal(overlayIP) && ones == 32 && bits == 32 {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("anchor %q does not have %s/32; got %v", mgr.anchorName, overlayIP, addrs)
	}
}

// TestVRFManagerTeardownCleanup verifies that Teardown() removes the VRF
// master device and anchor dummy, leaving no kernel objects.
func TestVRFManagerTeardownCleanup(t *testing.T) {
	requireRoot(t)

	vrfName := fmt.Sprintf("vrf_td%d", os.Getpid()%9999)
	mgr := NewVRFManager(vrfName, 10004, net.ParseIP("172.21.99.4"))

	if err := mgr.Setup(); err != nil {
		t.Fatalf("Setup() failed: %v", err)
	}
	anchorName := mgr.anchorName

	if err := mgr.Teardown(); err != nil {
		t.Fatalf("Teardown() returned error: %v", err)
	}

	if _, err := netlink.LinkByName(vrfName); err == nil {
		t.Errorf("VRF link %q still exists after Teardown()", vrfName)
	}
	if _, err := netlink.LinkByName(anchorName); err == nil {
		t.Errorf("anchor link %q still exists after Teardown()", anchorName)
	}
	if mgr.IsCreated() {
		t.Error("IsCreated() = true after Teardown()")
	}
}

// TestVRFManagerEnslaveRaceFree spawns 10 goroutines, each creating a unique
// dummy interface and calling EnslaveInterface concurrently, then verifies all
// succeed and no data race is detected.
func TestVRFManagerEnslaveRaceFree(t *testing.T) {
	requireRoot(t)

	vrfName := fmt.Sprintf("vrf_rf%d", os.Getpid()%9999)
	mgr := NewVRFManager(vrfName, 10005, net.ParseIP("172.21.99.5"))
	t.Cleanup(func() { cleanupVRF(t, mgr) })

	if err := mgr.Setup(); err != nil {
		t.Fatalf("Setup() failed: %v", err)
	}

	const n = 10
	dummies := make([]string, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("rt%d%d", i, os.Getpid()%999)
		dummies[i] = name
		dummy := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}}
		if err := netlink.LinkAdd(dummy); err != nil {
			t.Fatalf("create dummy[%d] %q: %v", i, name, err)
		}
	}
	t.Cleanup(func() {
		for _, name := range dummies {
			if link, err := netlink.LinkByName(name); err == nil {
				_ = netlink.LinkDel(link)
			}
		}
	})

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = mgr.EnslaveInterface(dummies[i])
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d EnslaveInterface(%q) error: %v", i, dummies[i], err)
		}
	}
}

// TestVRFManagerSetupRaceFree drives N concurrent Setup() callers on the same
// manager and asserts every call returns nil. Without the m.mu serialisation
// applied in Setup(), the check-then-create paths around netlinkLinkAdd race
// and one of the goroutines fails with EEXIST.
func TestVRFManagerSetupRaceFree(t *testing.T) {
	requireRoot(t)

	vrfName := fmt.Sprintf("vrf_sr%d", os.Getpid()%9999)
	mgr := NewVRFManager(vrfName, 10006, net.ParseIP("172.21.99.6"))
	t.Cleanup(func() { cleanupVRF(t, mgr) })

	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = mgr.Setup()
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d Setup() error: %v", i, err)
		}
	}

	if !mgr.IsCreated() {
		t.Fatalf("manager.IsCreated() = false after concurrent Setup")
	}
}

// ---------------------------------------------------------------------------
// Mock-mode tests — no privilege required, use test seams to inject errors.
// ---------------------------------------------------------------------------

// withMockNetlink temporarily replaces the netlink function variables with the
// provided mock functions for the duration of fn. It also resets the
// IsVRFSupported cache so the injected behaviour is observed.
func withMockNetlink(
	t *testing.T,
	mockAdd func(netlink.Link) error,
	fn func(),
) {
	t.Helper()
	orig := netlinkLinkAdd
	netlinkLinkAdd = mockAdd
	resetIsVRFSupportedOnce()
	t.Cleanup(func() {
		netlinkLinkAdd = orig
		resetIsVRFSupportedOnce()
	})
	fn()
}

// TestVRFManagerUnsupportedKernel verifies that Setup() propagates a
// vrf_unsupported error returned by IsVRFSupported() without performing any
// further netlink operations.
func TestVRFManagerUnsupportedKernel(t *testing.T) {
	cases := []struct {
		name      string
		injectErr error
		wantSub   string
	}{
		{
			name:      "EOPNOTSUPP → kernel_too_old",
			injectErr: unix.EOPNOTSUPP,
			wantSub:   "kernel_too_old",
		},
		{
			name:      "ENODEV → module_not_loaded",
			injectErr: unix.ENODEV,
			wantSub:   "module_not_loaded",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			withMockNetlink(t, func(_ netlink.Link) error {
				return tc.injectErr
			}, func() {
				mgr := NewVRFManager("vrf_mock", 19999, net.ParseIP("172.21.99.9"))
				err := mgr.Setup()
				if err == nil {
					t.Fatal("Setup() returned nil, want vrf_unsupported error")
				}
				if !strings.Contains(err.Error(), tc.wantSub) {
					t.Errorf("Setup() error = %q, want substring %q", err.Error(), tc.wantSub)
				}
			})
		})
	}
}

// TestIsVRFSupportedDistinguishesFailureModes verifies the three classification
// branches (EOPNOTSUPP → kernel_too_old, ENODEV → module_not_loaded, other → unknown).
func TestIsVRFSupportedDistinguishesFailureModes(t *testing.T) {
	cases := []struct {
		name      string
		injectErr error
		wantSub   string
	}{
		{
			name:      "EOPNOTSUPP",
			injectErr: unix.EOPNOTSUPP,
			wantSub:   "kernel_too_old",
		},
		{
			name:      "ENODEV",
			injectErr: unix.ENODEV,
			wantSub:   "module_not_loaded",
		},
		{
			name:      "other error",
			injectErr: errors.New("some unexpected error"),
			wantSub:   "unknown",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			withMockNetlink(t, func(_ netlink.Link) error {
				return tc.injectErr
			}, func() {
				err := IsVRFSupported()
				if err == nil {
					t.Fatal("IsVRFSupported() returned nil, want error")
				}
				if !strings.Contains(err.Error(), tc.wantSub) {
					t.Errorf("IsVRFSupported() error = %q, want substring %q", err.Error(), tc.wantSub)
				}
			})
		})
	}
}
