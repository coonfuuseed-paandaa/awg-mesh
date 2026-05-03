#!/usr/bin/env bash
# tests/critical/audit-log-query.sh - audit query CLI and gRPC gate.
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

"$GO" test -count=1 -run 'TestServer_QueryAudit_FiltersAndStreams|TestRunAuditLogQueryCommandSendsFiltersAndOutputsJSON|TestRunAuditLogQueryCommandOutputsHuman|TestRunAuditLogQueryCommandOutputsPromTextfile' \
    ./pkg/control_plane/... ./cmd/mesh-ctl/cmd >/dev/null

echo "PASS - audit-log-query.sh: QueryAudit stream and mesh-ctl audit-log output formats verified"
