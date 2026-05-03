#!/usr/bin/env bash
# tests/critical/memory.sh - copy-on-write and state isolation smoke.
#
# This gate catches the class of memory/regression bugs where shared maps or
# slices escape and mutate across role registries, clientd state, or ledgers.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"
source "${REPO_ROOT}/tests/critical/lib.bash"

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

critical_run_go_tests_required "$test_output" \
    'TestRegistryReplaceIsCopyOnWrite|TestStateUpdateImmutableAndConfiguratorPayloadsDiffer|TestRegistry_ClonesMutableFields|TestLedger_ListenerFires' \
    ./pkg/balancer/... ./pkg/ingress/... ./pkg/clientd/... ./pkg/control_plane/... -- \
    TestRegistryReplaceIsCopyOnWrite \
    TestStateUpdateImmutableAndConfiguratorPayloadsDiffer \
    TestRegistry_ClonesMutableFields \
    TestLedger_ListenerFires

echo "PASS - memory.sh: copy-on-write and state isolation contracts verified"
