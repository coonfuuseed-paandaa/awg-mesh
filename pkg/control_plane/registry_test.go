package control_plane

import (
	"errors"
	"testing"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/role"
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

func TestRegistry_AllowCertRolloverRejectsDifferentActivePendingCert(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, RegisteredNode{Name: "n1", Roles: []role.Role{role.RoleMaster}, OverlayIP: "10.0.0.1", NodeCertPEM: fakeCert})

	firstPending := []byte("pending-cert-1")
	if err := r.AllowCertRollover("n1", firstPending, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("AllowCertRollover first pending: %v", err)
	}
	if err := r.AllowCertRollover("n1", firstPending, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatalf("same pending cert should be idempotent: %v", err)
	}
	err := r.AllowCertRollover("n1", []byte("pending-cert-2"), time.Now().Add(time.Hour))
	if !errors.Is(err, ErrRegistryPendingCert) {
		t.Fatalf("expected ErrRegistryPendingCert for competing pending cert, got %v", err)
	}
	got, ok := r.Lookup("n1")
	if !ok {
		t.Fatal("n1 missing after rejected pending cert")
	}
	if string(got.PendingCertPEM) != string(firstPending) {
		t.Fatalf("pending cert overwritten after rejection: %q", got.PendingCertPEM)
	}
}

func TestRegistry_AllowCertRolloverAllowsDifferentCertAfterOverlap(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, RegisteredNode{Name: "n1", Roles: []role.Role{role.RoleMaster}, OverlayIP: "10.0.0.1", NodeCertPEM: fakeCert})

	mustAllowCertRollover(t, r, "n1", []byte("expired-pending"), time.Now().Add(time.Hour))
	r.mu.Lock()
	r.byName["n1"].CertOverlapUntil = time.Now().Add(-time.Minute)
	r.mu.Unlock()
	nextPending := []byte("next-pending")
	if err := r.AllowCertRollover("n1", nextPending, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("different pending cert after overlap should be allowed: %v", err)
	}
	got, ok := r.Lookup("n1")
	if !ok {
		t.Fatal("n1 missing after pending cert replacement")
	}
	if string(got.PendingCertPEM) != string(nextPending) {
		t.Fatalf("pending cert = %q, want %q", got.PendingCertPEM, nextPending)
	}
}

func TestRegistry_AllowCertRolloverRejectsInvalidOverlap(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, RegisteredNode{Name: "n1", Roles: []role.Role{role.RoleMaster}, OverlayIP: "10.0.0.1", NodeCertPEM: fakeCert})

	for _, overlapUntil := range []time.Time{time.Time{}, time.Now().Add(-time.Minute)} {
		err := r.AllowCertRollover("n1", []byte("pending-cert"), overlapUntil)
		if !errors.Is(err, ErrRegistryOverlap) {
			t.Fatalf("expected ErrRegistryOverlap for %s, got %v", overlapUntil, err)
		}
	}
	got, ok := r.Lookup("n1")
	if !ok {
		t.Fatal("n1 missing after rejected rollover")
	}
	if len(got.PendingCertPEM) != 0 || !got.CertOverlapUntil.IsZero() {
		t.Fatalf("invalid overlap mutated pending state: %+v", got)
	}
}

func TestRegistry_RegisterIgnoresCallerProvidedPendingRollover(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, RegisteredNode{
		Name:             "n1",
		Roles:            []role.Role{role.RoleMaster},
		OverlayIP:        "10.0.0.1",
		NodeCertPEM:      fakeCert,
		PendingCertPEM:   fakeCertOther,
		CertOverlapUntil: time.Now().Add(time.Hour),
	})
	got, ok := r.Lookup("n1")
	if !ok {
		t.Fatal("n1 missing after Register")
	}
	if len(got.PendingCertPEM) != 0 || !got.CertOverlapUntil.IsZero() {
		t.Fatalf("caller-controlled pending rollover persisted: %+v", got)
	}

	allowedPending := []byte("allowed-pending")
	if err := r.AllowCertRollover("n1", allowedPending, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("AllowCertRollover: %v", err)
	}
	if err := r.Register(RegisteredNode{
		Name:             "n1",
		Roles:            []role.Role{role.RoleMaster},
		OverlayIP:        "10.0.0.1",
		NodeCertPEM:      fakeCert,
		PendingCertPEM:   fakeCertOther,
		CertOverlapUntil: time.Now().Add(2 * time.Hour),
	}); err != nil {
		t.Fatalf("Register same cert: %v", err)
	}
	got, ok = r.Lookup("n1")
	if !ok {
		t.Fatal("n1 missing after re-register")
	}
	if string(got.PendingCertPEM) != string(allowedPending) {
		t.Fatalf("registry-controlled pending cert not preserved: %q", got.PendingCertPEM)
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

func mustAllowCertRollover(t *testing.T, r *Registry, name string, certPEM []byte, overlapUntil time.Time) {
	t.Helper()
	if err := r.AllowCertRollover(name, certPEM, overlapUntil); err != nil {
		t.Fatalf("AllowCertRollover(%s): %v", name, err)
	}
}
