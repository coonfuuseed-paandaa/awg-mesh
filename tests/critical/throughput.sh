#!/usr/bin/env bash
# tests/critical/throughput.sh - packet/flow throughput contract smoke.
#
# The real line-rate measurement belongs to the staged dev stand. This wrapper
# protects the flow-distribution and proxy primitives that the staged test uses.
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

test_output="$(mktemp)"
trap 'rm -f "$test_output"' EXIT

"$GO" test -count=1 -run 'TestDumbModeUsesWeightedRoundRobin|TestFlowStickinessExpiresAndTracksHealth|TestHTTPProxyForwardsToOverlayTargetAndPreservesHeaders|TestUDPForwarderMapsFlowsAndRejectsUnknownHost' -v \
    ./pkg/balancer/... ./pkg/ingress/... | tee "$test_output"

for required in TestDumbModeUsesWeightedRoundRobin TestFlowStickinessExpiresAndTracksHealth TestHTTPProxyForwardsToOverlayTargetAndPreservesHeaders TestUDPForwarderMapsFlowsAndRejectsUnknownHost; do
    if ! grep -q "=== RUN[[:space:]]*${required}" "$test_output"; then
        echo "FAIL - throughput primitive test did not run: ${required}" >&2
        exit 1
    fi
done

echo "PASS - throughput.sh: balancer and ingress flow primitives verified"
