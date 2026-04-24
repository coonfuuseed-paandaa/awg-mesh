//go:build linux

package routing

import (
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
)

func TestNewNetlinkRouter(t *testing.T) {
	t.Parallel()
	r := NewNetlinkRouter()
	if r == nil {
		t.Fatal("expected non-nil router")
	}
}

func TestLinkSetUpLoopback(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root (netlink operations)")
	}
	t.Parallel()
	r := NewNetlinkRouter()
	// lo is always present and UP — should be idempotent
	if err := r.LinkSetUp("lo"); err != nil {
		t.Fatalf("LinkSetUp(lo): %v", err)
	}
}

func TestLinkGetIndexLoopback(t *testing.T) {
	t.Parallel()
	r := NewNetlinkRouter()
	idx, err := r.LinkGetIndex("lo")
	if err != nil {
		t.Fatalf("LinkGetIndex(lo): %v", err)
	}
	if idx != 1 {
		t.Logf("loopback index = %d (expected 1 on most systems)", idx)
	}
}

func TestLinkSetUpNonExistent(t *testing.T) {
	t.Parallel()
	r := NewNetlinkRouter()
	err := r.LinkSetUp("nonexistent_iface_xyz")
	if err == nil {
		t.Fatal("expected error for non-existent interface")
	}
}

func TestAddrExistsLoopback(t *testing.T) {
	t.Parallel()
	r := NewNetlinkRouter()
	addr := &net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)}
	exists, err := r.AddrExists("lo", addr)
	if err != nil {
		t.Fatalf("AddrExists: %v", err)
	}
	if !exists {
		t.Fatal("expected 127.0.0.1/8 to exist on lo")
	}
}

func TestListRoutes(t *testing.T) {
	t.Parallel()
	r := NewNetlinkRouter()
	routes, err := r.ListRoutes()
	if err != nil {
		t.Fatalf("ListRoutes: %v", err)
	}
	if len(routes) == 0 {
		t.Fatal("expected at least one route")
	}
}

func TestRouteDeleteIgnoresMissingRoute(t *testing.T) {
	t.Parallel()
	dest := &net.IPNet{IP: net.ParseIP("172.20.70.1"), Mask: net.CIDRMask(32, 32)}
	r := NewNetlinkRouter()

	if err := r.RouteDelete(dest); err != nil {
		t.Fatalf("RouteDelete should ignore missing route: %v", err)
	}
}

func TestRemoveECMPRouteIgnoresMissingRoute(t *testing.T) {
	t.Parallel()
	dest := &net.IPNet{IP: net.ParseIP("172.20.70.1"), Mask: net.CIDRMask(32, 32)}
	r := NewNetlinkRouter()

	if err := r.RemoveECMPRoute(dest); err != nil {
		t.Fatalf("RemoveECMPRoute should ignore missing route: %v", err)
	}
}

func TestIsMissingRouteDeleteError(t *testing.T) {
	t.Parallel()

	if !isMissingRouteDeleteError(errors.Join(syscall.ESRCH, errors.New("route missing"))) {
		t.Fatal("expected ESRCH to be treated as missing-route delete")
	}
	if isMissingRouteDeleteError(errors.Join(syscall.EPERM, errors.New("permission denied"))) {
		t.Fatal("expected non-ESRCH error to remain fatal")
	}
}
