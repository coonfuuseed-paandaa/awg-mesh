#!/usr/bin/env bash
# tests/critical/control-plane-dr.sh - backup/restore and control-plane restart gate.
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

"$GO" test -count=1 -run 'TestRunBackupCommandCapturesLocalTopologyAndControlPlaneState|TestRunRestoreCommandRestoresConfirmedArchive|TestRunRestoreCommandValidatesManifestBeforeOverwrite|TestDaemon_LifecycleAndAcceptsRegister' \
    ./cmd/mesh-ctl/cmd ./pkg/control_plane/... >/dev/null

echo "PASS - control-plane-dr.sh: backup archive, restore validation, and daemon restart lifecycle verified"
