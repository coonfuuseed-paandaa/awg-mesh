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
"$GO" test -count=1 -run 'TestOrchestrator|TestServer_RotateAWGParamsMeshWide|TestRunRotateCommandMeshWide|TestValidateRotateOptionsMeshWide' \
    ./pkg/rotation/... ./pkg/control_plane/... ./cmd/mesh-ctl/...

echo "PASS — rotation.sh: CR-008 mesh-wide rotation contract verified"
