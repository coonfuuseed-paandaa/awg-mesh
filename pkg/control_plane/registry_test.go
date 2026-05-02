package control_plane

import (
	"errors"
	"testing"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/role"
)

var fakeCert = []byte("-----BEGIN CERTIFICATE-----\nMIIfake\n-----END CERTIFICATE-----\n")
var fakeCertOther = []byte("-----BEGIN CERTIFICATE-----\nMIIfakeOTHER\n-----END CERTIFICATE-----\n")

func TestRegistry_RegisterAndLookup(t *testing.T) {
	r := NewRegistry()
	err := r.Register(RegisteredNode{
		Name:        "master-01",
		Roles:       []role.Role{role.RoleMaster, role.RoleBalancer},
		OverlayIP:   "172.21.92.2",
		NodeCertPEM: fakeCert,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, ok := r.Lookup("master-01")
	if !ok {
		t.Fatalf("Lookup failed")
	}
	if got.OverlayIP != "172.21.92.2" {
		t.Fatalf("overlay mismatch: %s", got.OverlayIP)
	}
}

func TestRegistry_RejectsBadInputs(t *testing.T) {
	r := NewRegistry()
	cases := []struct {
		name    string
		node    RegisteredNode
		wantErr error
	}{
		{"empty name", RegisteredNode{Roles: []role.Role{role.RoleMaster}, NodeCertPEM: fakeCert, OverlayIP: "1.2.3.4"}, ErrRegistryEmptyName},
		{"empty roles", RegisteredNode{Name: "x", NodeCertPEM: fakeCert, OverlayIP: "1.2.3.4"}, ErrRegistryEmptyRoles},
		{"no cert", RegisteredNode{Name: "x", Roles: []role.Role{role.RoleMaster}, OverlayIP: "1.2.3.4"}, ErrRegistryNoCert},
		{"client+master rejected", RegisteredNode{Name: "x", Roles: []role.Role{role.RoleClient, role.RoleMaster}, NodeCertPEM: fakeCert, OverlayIP: "1.2.3.4"}, role.ErrRoleClientExclusive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := r.Register(tc.node)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestRegistry_OverlayCollision(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(RegisteredNode{Name: "n1", Roles: []role.Role{role.RoleMaster}, OverlayIP: "10.0.0.1", NodeCertPEM: fakeCert}); err != nil {
		t.Fatal(err)
	}
	err := r.Register(RegisteredNode{Name: "n2", Roles: []role.Role{role.RoleEgress}, OverlayIP: "10.0.0.1", NodeCertPEM: fakeCert})
	if !errors.Is(err, ErrRegistryOverlayDup) {
		t.Fatalf("expected ErrRegistryOverlayDup, got %v", err)
	}
}

func TestRegistry_NameWithDifferentCertRejected(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(RegisteredNode{Name: "n1", Roles: []role.Role{role.RoleMaster}, OverlayIP: "10.0.0.1", NodeCertPEM: fakeCert}); err != nil {
		t.Fatal(err)
	}
	err := r.Register(RegisteredNode{Name: "n1", Roles: []role.Role{role.RoleMaster}, OverlayIP: "10.0.0.1", NodeCertPEM: fakeCertOther})
	if !errors.Is(err, ErrRegistryNameDup) {
		t.Fatalf("expected ErrRegistryNameDup, got %v", err)
	}
}

func TestRegistry_ReRegisterRejectsOverlayMove(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, RegisteredNode{Name: "n1", Roles: []role.Role{role.RoleMaster}, OverlayIP: "10.0.0.1", NodeCertPEM: fakeCert})

	err := r.Register(RegisteredNode{Name: "n1", Roles: []role.Role{role.RoleMaster}, OverlayIP: "10.0.0.2", NodeCertPEM: fakeCert})
	if !errors.Is(err, ErrRegistryOverlayMove) {
		t.Fatalf("expected ErrRegistryOverlayMove, got %v", err)
	}
	mustRegister(t, r, RegisteredNode{Name: "n2", Roles: []role.Role{role.RoleEgress}, OverlayIP: "10.0.0.2", NodeCertPEM: fakeCertOther})
	got, ok := r.Lookup("n1")
	if !ok || got.OverlayIP != "10.0.0.1" {
		t.Fatalf("original overlay changed after rejected move: %+v", got)
	}
}

func TestRegistry_HeartbeatUnknown(t *testing.T) {
	r := NewRegistry()
	err := r.Heartbeat("ghost", nil)
	if !errors.Is(err, ErrRegistryNotFound) {
		t.Fatalf("expected ErrRegistryNotFound, got %v", err)
	}
}

func TestRegistry_MastersInRegion(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, RegisteredNode{Name: "m-ru", Roles: []role.Role{role.RoleMaster}, OverlayIP: "10.0.0.1", NodeCertPEM: fakeCert, Region: "ru"})
	mustRegister(t, r, RegisteredNode{Name: "m-de", Roles: []role.Role{role.RoleMaster}, OverlayIP: "10.0.0.2", NodeCertPEM: fakeCert, Region: "de"})
	mustRegister(t, r, RegisteredNode{Name: "e-us", Roles: []role.Role{role.RoleEgress}, OverlayIP: "10.0.0.3", NodeCertPEM: fakeCert, Region: "us"})

	ru := r.MastersInRegion("ru")
	if len(ru) != 1 || ru[0] != "m-ru" {
		t.Fatalf("MastersInRegion(ru) = %v, want [m-ru]", ru)
	}
	all := r.MastersInRegion("")
	if len(all) != 2 {
		t.Fatalf("MastersInRegion('') should return 2 masters, got %d", len(all))
	}
}

func TestRegistry_Remove(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, RegisteredNode{Name: "n1", Roles: []role.Role{role.RoleMaster}, OverlayIP: "10.0.0.1", NodeCertPEM: fakeCert})
	if err := r.Remove("n1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Lookup("n1"); ok {
		t.Fatalf("node still present after Remove")
	}
	if err := r.Remove("n1"); !errors.Is(err, ErrRegistryNotFound) {
		t.Fatalf("second Remove should return ErrRegistryNotFound, got %v", err)
	}
}

func TestRegistry_ClonesMutableFields(t *testing.T) {
	r := NewRegistry()
	roles := []role.Role{role.RoleMaster}
	cert := []byte("cert-a")
	if err := r.Register(RegisteredNode{Name: "n1", Roles: roles, OverlayIP: "10.0.0.1", NodeCertPEM: cert}); err != nil {
		t.Fatal(err)
	}
	roles[0] = role.RoleClient
	cert[0] = 'X'

	health := map[string]string{"state": "ok"}
	if err := r.Heartbeat("n1", health); err != nil {
		t.Fatal(err)
	}
	health["state"] = "mutated"

	got, ok := r.Lookup("n1")
	if !ok {
		t.Fatal("missing n1")
	}
	if got.Roles[0] != role.RoleMaster {
		t.Fatalf("stored roles mutated: %v", got.Roles)
	}
	if string(got.NodeCertPEM) != "cert-a" {
		t.Fatalf("stored cert mutated: %q", got.NodeCertPEM)
	}
	if got.HealthIndicators["state"] != "ok" {
		t.Fatalf("stored health mutated: %v", got.HealthIndicators)
	}

	got.Roles[0] = role.RoleClient
	got.NodeCertPEM[0] = 'Y'
	got.HealthIndicators["state"] = "lookup-mutated"
	again, _ := r.Lookup("n1")
	if again.Roles[0] != role.RoleMaster || string(again.NodeCertPEM) != "cert-a" || again.HealthIndicators["state"] != "ok" {
		t.Fatalf("lookup returned mutable internals: %+v", again)
	}

	listed := r.List()
	listed[0].Roles[0] = role.RoleClient
	listed[0].HealthIndicators["state"] = "list-mutated"
	again, _ = r.Lookup("n1")
	if again.Roles[0] != role.RoleMaster || again.HealthIndicators["state"] != "ok" {
		t.Fatalf("list returned mutable internals: %+v", again)
	}
}

func mustRegister(t *testing.T, r *Registry, node RegisteredNode) {
	t.Helper()
	if err := r.Register(node); err != nil {
		t.Fatalf("Register(%s): %v", node.Name, err)
	}
}
