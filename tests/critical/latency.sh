#!/usr/bin/env bash
# tests/critical/latency.sh - fast-path runtime latency smoke.
#
# This is not a WAN benchmark. It guards the local v2 control/runtime paths that
# must remain quick enough for the heavier CR-011 simulation suite to be useful.
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

start_seconds=${SECONDS}
"$GO" test -count=1 -timeout 30s -run 'TestDaemon_LifecycleAndAcceptsRegister|TestMasterRunStartsAndClosesOnCancel|TestRuntimeStartsAndStops|TestRuntimeStartsMetricsAndStopsWithContext' \
    ./pkg/control_plane/... ./pkg/node/... ./pkg/ingress/... ./pkg/balancer/... >/dev/null
elapsed=$((SECONDS - start_seconds))

if [[ "${elapsed}" -gt 30 ]]; then
    echo "FAIL - local runtime latency smoke exceeded 30s: ${elapsed}s" >&2
    exit 1
fi

echo "PASS - latency.sh: local runtime/control-plane smoke completed in ${elapsed}s"
