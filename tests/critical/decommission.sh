#!/usr/bin/env bash
# tests/critical/decommission.sh - node decommissioning lifecycle gate.
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
    'TestServer_DecommissionNode|TestLedger_OwnedByAndDrain|TestLedger_Remove|TestRegistry_Remove|TestRunNodeRemoveCommand|TestValidateNodeRemoveOptionsRejectsSubSecondDrain' \
    ./pkg/control_plane/... ./cmd/mesh-ctl/cmd -- \
    TestServer_DecommissionNode \
    TestLedger_OwnedByAndDrain \
    TestLedger_Remove \
    TestRegistry_Remove \
    TestRunNodeRemoveCommandSendsRequestAndOutputsJSON \
    TestValidateNodeRemoveOptionsRejectsSubSecondDrain

echo "PASS - decommission.sh: drain, reassignment, registry removal, and mesh-ctl node remove verified"
