#!/usr/bin/env bash
# tests/critical/control-plane-dr.sh - backup/restore and control-plane restart gate.
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
    'TestRunBackupCommandCapturesLocalTopologyAndControlPlaneState|TestRunRestoreCommandRestoresConfirmedArchive|TestRunRestoreCommandValidatesManifestBeforeOverwrite|TestDaemon_LifecycleAndAcceptsRegister' \
    ./cmd/mesh-ctl/cmd ./pkg/control_plane/... -- \
    TestRunBackupCommandCapturesLocalTopologyAndControlPlaneState \
    TestRunRestoreCommandRestoresConfirmedArchive \
    TestRunRestoreCommandValidatesManifestBeforeOverwrite \
    TestDaemon_LifecycleAndAcceptsRegister

echo "PASS - control-plane-dr.sh: backup archive, restore validation, and daemon restart lifecycle verified"
