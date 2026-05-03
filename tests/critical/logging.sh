#!/usr/bin/env bash
# tests/critical/logging.sh - structured logging contract gate.
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
    'TestNewLoggerComponent|TestSetGlobalLevel|TestWithFields|TestLogger_AppendReadAll|TestLogger_ConcurrentAppend' \
    ./pkg/logging/... ./pkg/upgrade/... -- \
    TestNewLoggerComponent \
    TestSetGlobalLevel \
    TestWithFields \
    TestLogger_AppendReadAll \
    TestLogger_ConcurrentAppend

echo "PASS - logging.sh: JSON logger fields, level filtering, and concurrent JSONL upgrade log writes verified"
