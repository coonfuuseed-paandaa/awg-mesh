#!/usr/bin/env bash
# tests/critical/release-readiness.sh - aggregate v2.0 release readiness gate.
#
# Developer-mode run-all skips this aggregate gate so local critical checks can
# stay useful while later CRs are still open. Direct execution, or run-all
# --strict, reports every release blocker and exits non-zero until v2.0 is
# actually shippable.
set -euo pipefail

if [[ "${CRITICAL_RUNNER_MODE:-}" == "developer" ]]; then
    echo "SKIP - release-readiness is a release-mode aggregate gate; run directly or via run-all.sh --strict"
    exit 0
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

blockers=()

require_file() {
    local path="$1"
    local blocker="$2"
    if [[ ! -f "${path}" ]]; then
        blockers+=("${blocker}: missing ${path}")
    fi
}

require_no_placeholder() {
    local path="$1"
    local blocker="$2"
    if [[ ! -f "${path}" ]]; then
        blockers+=("${blocker}: missing ${path}")
        return
    fi
    if grep -Eq 'Implementation lands|echo .?SKIP|placeholder|stub' "${path}"; then
        blockers+=("${blocker}: placeholder text remains in ${path}")
    fi
}

require_no_placeholder "tests/critical/latency.sh" "CR-011-latency"
require_no_placeholder "tests/critical/throughput.sh" "CR-011-throughput"
require_no_placeholder "tests/critical/memory.sh" "CR-011-memory"
require_no_placeholder "tests/critical/decommission.sh" "CR-011-decommission"
require_no_placeholder "tests/critical/cert-rotation.sh" "CR-011-cert-rotation"
require_no_placeholder "tests/critical/observability.sh" "CR-011-observability"
require_no_placeholder "tests/critical/control-plane-dr.sh" "CR-011-control-plane-dr"
require_no_placeholder "tests/critical/availability.sh" "CR-011-availability"
require_no_placeholder "tests/critical/logging.sh" "CR-011-logging"
require_no_placeholder "tests/critical/upgrade.sh" "CR-011-upgrade"
require_no_placeholder "tests/critical/audit-log-query.sh" "CR-011-audit-log-query"
require_no_placeholder "tests/critical/capacity.sh" "CR-011-capacity"
require_no_placeholder "tests/critical/migration.sh" "CR-013-migration-tooling"

require_file "docs/PRODUCTION-TESTING-PLAYBOOK.md" "CR-012-emulation-playbook"
require_no_placeholder "tests/emulation-playbook/run.sh" "CR-012-emulation-playbook"
require_no_placeholder "cmd/mesh-ctl/cmd/migrate.go" "CR-013-migration-tooling"
require_no_placeholder "pkg/topology/migrate_v1_to_v2.go" "CR-013-migration-tooling"
require_file "pkg/mikrotik/v2/generator.go" "CR-014-mikrotik-v2"

if awk '/func \(s \*Server\) StreamCertUpdate/ { in_func = 1 } in_func { print; if ($0 == "}") exit }' pkg/control_plane/server.go | grep -q 'codes.Unimplemented'; then
    blockers+=("CR-015-cert-lifecycle: StreamCertUpdate still returns Unimplemented")
fi

if [[ ${#blockers[@]} -gt 0 ]]; then
    echo "FAIL - v2.0 release readiness has ${#blockers[@]} blocker(s)"
    for blocker in "${blockers[@]}"; do
        printf 'BLOCKER %s\n' "${blocker}"
    done
    exit 1
fi

echo "PASS - v2.0 release readiness blockers cleared"
