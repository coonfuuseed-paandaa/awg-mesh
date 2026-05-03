#!/usr/bin/env bash
# tests/critical/rotation.sh — F-009 CR-008 mesh-wide rotation gate.
#
# Verifies the mesh-wide rotation boundary: orchestrator target filtering,
# partial failure reporting, control-plane RotateAWGParamsMeshWide streaming,
# and mesh-ctl --mesh-wide CLI dispatch.
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

if grep -q 'rotation orchestration lands in CR-008' pkg/control_plane/server.go; then
    echo "FAIL — RotateAWGParamsMeshWide still returns the CR-008 placeholder" >&2
    exit 1
fi
if grep -Eq '^# Implementation lands|^echo .*SKIP' "${BASH_SOURCE[0]}"; then
    echo "FAIL — rotation.sh still contains placeholder skip text" >&2
    exit 1
fi

echo "Running CR-008 mesh-wide rotation contract suite..."
RUN_RE='TestOrchestrator|TestServer_RotateAWGParamsMeshWide|TestRunRotateCommandMeshWide|TestValidateRotateOptionsMeshWide'
TEST_OUTPUT="$(mktemp)"
trap 'rm -f "$TEST_OUTPUT"' EXIT

"$GO" test -count=1 -run "$RUN_RE" -v \
    ./pkg/rotation/... ./pkg/control_plane/... ./cmd/mesh-ctl/... | tee "$TEST_OUTPUT"

required_test_families=(
    TestOrchestrator
    TestServer_RotateAWGParamsMeshWide
    TestRunRotateCommandMeshWide
    TestValidateRotateOptionsMeshWide
)
for test_family in "${required_test_families[@]}"; do
    if ! grep -Eq "^=== RUN[[:space:]]+${test_family}" "$TEST_OUTPUT"; then
        echo "FAIL — required contract test family did not run: ${test_family}" >&2
        exit 1
    fi
done

echo "PASS — rotation.sh: CR-008 mesh-wide rotation contract verified"
