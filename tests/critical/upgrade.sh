#!/usr/bin/env bash
# tests/critical/upgrade.sh - rolling upgrade planner and state gate.
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

"$GO" test -count=1 -run 'TestRunUpgradeCommandDryRunV2Plan|TestRunUpgradeCommandHonorsManualOrder|TestRunUpgradeCommandNonDryRunFailsExplicitUnsupported|TestRunUpgradePauseResumeStatusMutateState|TestComputeOrder|TestComputePlan|TestUpgradeNode_PhaseVerify' \
    ./cmd/mesh-ctl/cmd ./pkg/upgrade/... >/dev/null

echo "PASS - upgrade.sh: v2 upgrade planning, manual order, pause/resume state, and verify phase contracts verified"
