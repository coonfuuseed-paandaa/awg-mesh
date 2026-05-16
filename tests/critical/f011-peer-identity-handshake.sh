#!/usr/bin/env bash
# tests/critical/f011-peer-identity-handshake.sh - F-011 clientd peer identity handshake simulation.
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

if [[ "$("$GO" env GOOS)" != "linux" ]]; then
    echo "SKIP - F-011 handshake simulation requires linux GOOS"
    exit 0
fi

test_output="$(mktemp)"
trap 'rm -f "$test_output"' EXIT

critical_run_go_tests_required "$test_output" \
    'TestF011ClientdStreamSnapshotDrivesAmneziaWGHandshake' \
    ./tests/simulation -- \
    TestF011ClientdStreamSnapshotDrivesAmneziaWGHandshake

echo "PASS - f011-peer-identity-handshake.sh: master self-registration and egress clientd registration drive peer config, packet transit, and non-zero handshake"
