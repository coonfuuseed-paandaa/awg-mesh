#!/usr/bin/env bash
# tests/critical/ingress-cdn.sh — F-009 CR-006 deliverable test.
#
# Verifies the ingress role boundary: immutable registry, SNI classification,
# HTTP/WebSocket/UDP proxy primitives, runtime lifecycle, and awg-mesh-node
# --mode ingress dry-run wiring.

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

"$GO" test -count=1 ./pkg/ingress/... ./cmd/awg-mesh-node/... >/dev/null

out="$("$GO" run ./cmd/awg-mesh-node \
    --mode ingress \
    --dry-run \
    --name ingress-01 \
    --overlay-ip 172.21.92.30 \
    --ingress-public-addr :8443 \
    --ingress-route media.example.com=172.21.92.10:8096 \
    --ingress-tenant tenant-a \
    --ingress-health-interval 2s \
    --ingress-acme-cache /tmp/awg-mesh-acme \
    --ingress-http3 2>&1)"

if ! printf '%s' "${out}" | grep -q 'ingress dry-run node=ingress-01'; then
    echo "FAIL — ingress dry-run missing node plan: ${out}" >&2
    exit 1
fi
if ! printf '%s' "${out}" | grep -q 'route=tenant-a:media.example.com->172.21.92.10:8096/tls_terminate'; then
    echo "FAIL — ingress dry-run ignored configured route: ${out}" >&2
    exit 1
fi
if printf '%s' "${out}" | grep -qi 'implementation lands\|placeholder\|skeleton'; then
    echo "FAIL — ingress mode still looks like a placeholder: ${out}" >&2
    exit 1
fi

echo "PASS — ingress CDN role contract green (registry/SNI/proxy/runtime/dry-run)"
