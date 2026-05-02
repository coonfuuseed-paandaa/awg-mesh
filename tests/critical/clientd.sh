#!/usr/bin/env bash
# tests/critical/clientd.sh — F-009 CR-003 critical-suite test.
#
# Verifies the clientd contract: immutable state/cache, registration, stream
# consumption, NAT capability seam, transport peer conversion, and CLI validation.
set -euo pipefail

cd "$(dirname "$0")/../.."

GO=${GO_BIN:-/usr/local/go/bin/go}
if ! command -v "$GO" >/dev/null 2>&1; then
    GO=go
fi
if ! command -v "$GO" >/dev/null 2>&1; then
    echo "FAIL — go toolchain not available; run inside Docker" >&2
    exit 1
fi

echo "Running clientd CR-003 contract suite..."
$GO test -count=1 -race ./pkg/clientd/... ./cmd/clientd/...

echo 'PASS — clientd.sh: CR-003 clientd contract verified'
