#!/usr/bin/env bash
# tests/critical/single-tun.sh — F-009 CR-001 deliverable test.
#
# Verifies the v2.0 schema validator: v1.x topology fixtures are rejected
# (DetectSchemaVersion returns SchemaV1 → callers raise SCHEMA-V1-DEPRECATED)
# and v2.0 fixtures pass ValidateV2.
#
# CR-004 adds runtime-level evidence for FR-3's master exception: the master
# owns exactly two configured listeners, while other roles remain single-TUN
# scoped in later CRs.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

GO=${GO_BIN:-/usr/local/go/bin/go}
if ! command -v "$GO" >/dev/null 2>&1; then
    GO=go
fi
if ! command -v "$GO" >/dev/null 2>&1; then
    echo "SKIP — go toolchain not available; run inside Docker"
    exit 0
fi

$GO test -count=1 -short -run 'TestValidateV2|TestDetectSchemaVersion|TestMigrateV1ToV2' ./pkg/topology/... >/dev/null
$GO test -count=1 -run 'TestMasterRunStartsAndClosesOnCancel|TestRunMasterDryRunUsesDefaultDualListener' ./pkg/node/... ./cmd/awg-mesh-node/... >/dev/null
echo "PASS — v1.x topology rejected, v2.0 topology validated, master dual-listener invariant enforced"
