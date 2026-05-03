#!/usr/bin/env bash
# tests/critical/availability.sh - control-plane availability primitives gate.
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

"$GO" test -count=1 -run 'TestLedger_OwnedByAndDrain|TestServer_StreamOwnership_LiveUpdate|TestServer_DecommissionNode|TestHealthTrackerProbeOnceUpdatesTargets|TestRuntimeStartsAndStops' \
    ./pkg/control_plane/... ./pkg/ingress/... >/dev/null

echo "PASS - availability.sh: ownership failover stream, decommission recovery, health tracking, and ingress runtime verified"
