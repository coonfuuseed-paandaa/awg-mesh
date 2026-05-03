#!/usr/bin/env bash
# tests/critical/decommission.sh - node decommissioning lifecycle gate.
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

"$GO" test -count=1 -run 'TestServer_DecommissionNode|TestLedger_OwnedByAndDrain|TestLedger_Remove|TestRegistry_Remove|TestRunNodeRemoveCommand|TestValidateNodeRemoveOptionsRejectsSubSecondDrain' \
    ./pkg/control_plane/... ./cmd/mesh-ctl/cmd >/dev/null

echo "PASS - decommission.sh: drain, reassignment, registry removal, and mesh-ctl node remove verified"
