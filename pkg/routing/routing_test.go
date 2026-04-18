//go:build !linux

package routing

import (
	"net"
	"testing"
)

func TestNetlinkRouterStubsReturnNotSupported(t *testing.T) {
	t.Parallel()
	r := NewNetlinkRouter()
	dest := &net.IPNet{IP: net.ParseIP("10.0.0.0"), Mask: net.CIDRMask(24, 32)}

	tests := []struct {
		name string
		run  func() error
	}{
		{"RouteAdd", func() error { return r.RouteAdd(dest, net.ParseIP("10.0.0.1"), "wg0") }},
		{"RouteReplace", func() error { return r.RouteReplace(dest, net.ParseIP("10.0.0.1"), "wg0") }},
		{"RouteReplaceLink", func() error { return r.RouteReplaceLink(dest, "wg0") }},
		{"RouteDelete", func() error { return r.RouteDelete(dest) }},
		{"SetECMPRoute", func() error { return r.SetECMPRoute(dest, []NextHop{{Via: "10.0.0.1", Dev: "wg0", Weight: 1}}) }},
		{"RemoveECMPRoute", func() error { return r.RemoveECMPRoute(dest) }},
		{"AddrAdd", func() error { return r.AddrAdd("lo", dest) }},
		{"LinkSetUp", func() error { return r.LinkSetUp("lo") }},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.run(); err == nil {
				t.Fatal("expected not-supported error")
			}
		})
	}
}

func TestProcSysctlStubsReturnNotSupported(t *testing.T) {
	t.Parallel()
	s := NewProcSysctl()
	if err := s.EnableForwarding(); err == nil {
		t.Fatal("expected error")
	}
	if err := s.EnableL4Hash(); err == nil {
		t.Fatal("expected error")
	}
}

func TestNftablesFirewallStubReturnsNotSupported(t *testing.T) {
	t.Parallel()
	_, err := NewNftablesFirewall()
	if err == nil {
		t.Fatal("expected error on non-Linux")
	}
}
