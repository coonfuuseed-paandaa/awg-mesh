#!/usr/bin/env bash
# tests/critical/nat-prohibition.sh — F-009 CR-005 critical-suite test.
#
# Verifies that egress NAT is scoped to the configured internet interface and
# mesh-looking interfaces are rejected before any kernel mutation is attempted.
set -euo pipefail

cd "$(dirname "$0")/../.."

GO=${GO_BIN:-/usr/local/go/bin/go}
if ! command -v "$GO" >/dev/null 2>&1; then
    GO=go
fi
if ! command -v "$GO" >/dev/null 2>&1; then
    echo "FAIL — go toolchain not available; run inside Docker" >&2
    exit 1
fi

echo "Running CR-005 NAT boundary contract suite..."
"$GO" test -count=1 -run 'TestPlanValidatesInternetInterface|TestMasqueradeInstaller|TestNewEgress|TestEgressRun|TestRunEgress' \
    ./pkg/nftables/... ./pkg/node/... ./cmd/awg-mesh-node/...

dry_run="$("$GO" run ./cmd/awg-mesh-node --mode egress --dry-run --name egress-01 --overlay-ip 172.21.92.20 --internet-iface eth0)"
if ! grep -q 'nat=awg_mesh:nat_postrouting/oifname eth0 masquerade' <<<"${dry_run}"; then
    echo "FAIL — egress dry-run NAT plan missing or wrong: ${dry_run}" >&2
    exit 1
fi

set +e
bad_iface="$("$GO" run ./cmd/awg-mesh-node --mode egress --dry-run --name egress-01 --overlay-ip 172.21.92.20 --internet-iface awg-mesh0 2>&1)"
bad_status=$?
set -e
if [[ "${bad_status}" -eq 0 ]] || ! grep -q 'mesh interface' <<<"${bad_iface}"; then
    echo "FAIL — egress accepted a mesh interface for internet MASQUERADE: ${bad_iface}" >&2
    exit 1
fi

echo 'PASS — nat-prohibition.sh: CR-005 egress NAT boundary verified'
