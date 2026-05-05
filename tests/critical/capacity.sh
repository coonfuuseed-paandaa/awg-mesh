#!/usr/bin/env bash
# tests/critical/capacity.sh - synthetic registry/ledger capacity gate.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

GO=${GO_BIN:-/usr/local/go/bin/go}
if ! command -v "$GO" >/dev/null 2>&1; then
    GO=go
fi
if ! command -v "$GO" >/dev/null 2>&1; then
    echo "FAIL - go toolchain not available; run inside Docker" >&2
    exit 1
fi

tmp_test="pkg/control_plane/capacity_critical_test.go"
cleanup() {
    rm -f "${tmp_test}"
}
trap cleanup EXIT

cat > "${tmp_test}" <<'GO'
package control_plane

import (
	"fmt"
	"testing"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/role"
)

func TestCriticalCapacitySynthetic(t *testing.T) {
	registry := NewRegistry()
	ledger := NewLedger()

	for i := 0; i < 1000; i++ {
		node := RegisteredNode{
			Name:        fmt.Sprintf("node-%04d", i),
			Roles:       []role.Role{role.RoleEgress},
			OverlayIP:   fmt.Sprintf("172.21.%d.%d", i/254, (i%254)+1),
			NodeCertPEM: []byte("cert"),
			Region:      fmt.Sprintf("r-%02d", i%10),
		}
		if i%10 == 0 {
			node.Roles = []role.Role{role.RoleMaster}
		}
		if err := registry.Register(node); err != nil {
			t.Fatalf("register %s: %v", node.Name, err)
		}
	}

	for i := 0; i < 10000; i++ {
		owner := fmt.Sprintf("node-%04d", (i%100)*10)
		ip := fmt.Sprintf("10.%d.%d.%d", (i/64516)+1, (i/254)%254, (i%254)+1)
		if _, err := ledger.Reassign(ip, owner, "capacity"); err != nil {
			t.Fatalf("reassign %s to %s: %v", ip, owner, err)
		}
	}

	if got := len(registry.MastersInRegion("")); got != 100 {
		t.Fatalf("masters = %d, want 100", got)
	}
	if got := len(ledger.OwnedBy("node-0000")); got == 0 {
		t.Fatalf("node-0000 should own synthetic overlays")
	}
}
GO

"$GO" test -count=1 -run TestCriticalCapacitySynthetic ./pkg/control_plane/... >/dev/null

echo "PASS - capacity.sh: synthetic 1000-node registry and 10000-overlay ledger capacity verified"
