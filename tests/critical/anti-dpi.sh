#!/usr/bin/env bash
# tests/critical/anti-dpi.sh — F-009 CR-008 adaptive anti-DPI rotation gate.
#
# Verifies adaptive tier-1 trigger detection for throughput drops, handshake
# retry storms, RTT spikes, and healthy/no-trigger samples.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

GO=${GO_BIN:-/usr/local/go/bin/go}
if ! command -v "$GO" >/dev/null 2>&1; then
    GO=go
fi
if ! command -v "$GO" >/dev/null 2>&1; then
    echo "FAIL — go toolchain not available; run inside Docker" >&2
    exit 1
fi

if grep -Eq '^# Implementation lands|^echo .*SKIP' "${BASH_SOURCE[0]}"; then
    echo "FAIL — anti-dpi.sh still contains placeholder skip text" >&2
    exit 1
fi

echo "Running CR-008 adaptive anti-DPI detector suite..."
"$GO" test -count=1 -run 'TestAdaptiveDetector' ./pkg/rotation/...

echo "PASS — anti-dpi.sh: adaptive anti-DPI trigger contract verified"
