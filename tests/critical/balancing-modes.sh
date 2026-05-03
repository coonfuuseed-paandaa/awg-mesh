#!/usr/bin/env bash
# tests/critical/balancing-modes.sh — F-009 CR-007 deliverable test.
#
# Verifies the balancer role boundary: immutable registry, dumb/labeled
# selection, health-aware fallback, sticky flows, metrics, runtime lifecycle,
# and awg-mesh-node --mode balancer dry-run wiring.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

GO=${GO_BIN:-/usr/local/go/bin/go}
if ! command -v "$GO" >/dev/null 2>&1; then
    GO=go
fi
if ! command -v "$GO" >/dev/null 2>&1; then
    echo "SKIP — go toolchain not available; run inside Docker"
    exit 0
fi

"$GO" test -count=1 ./pkg/balancer/... ./cmd/awg-mesh-node/... >/dev/null

out="$("$GO" run ./cmd/awg-mesh-node \
    --mode balancer \
    --dry-run \
    --name master-01 \
    --overlay-ip 172.21.92.1 \
    --balancer-mode labeled \
    --balancer-egress egress-ru=172.21.92.10:51821,weight=2 \
    --balancer-egress egress-eu=172.21.92.11:51821,weight=1 \
    --balancer-dscp 10=egress-ru \
    --balancer-health-interval 2s 2>&1)"

if ! printf '%s' "${out}" | grep -q 'balancer dry-run node=master-01'; then
    echo "FAIL — balancer dry-run missing node plan: ${out}" >&2
    exit 1
fi
if ! printf '%s' "${out}" | grep -q 'egress=egress-ru->172.21.92.10:51821/weight=2'; then
    echo "FAIL — balancer dry-run ignored weighted egress: ${out}" >&2
    exit 1
fi
if ! printf '%s' "${out}" | grep -q 'dscp=10->egress-ru'; then
    echo "FAIL — balancer dry-run ignored DSCP mapping: ${out}" >&2
    exit 1
fi
if printf '%s' "${out}" | grep -qi 'implementation lands\|placeholder\|skeleton'; then
    echo "FAIL — balancer mode still looks like a placeholder: ${out}" >&2
    exit 1
fi

echo "PASS — balancing modes role contract green (policy/health/sticky/runtime/dry-run)"
