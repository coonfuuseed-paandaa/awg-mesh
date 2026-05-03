#!/usr/bin/env bash
# tests/emulation-playbook/run.sh - F-009 v2 customer-mode release walkthrough.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

GO="${GO_BIN:-/usr/local/go/bin/go}"
if ! command -v "${GO}" >/dev/null 2>&1; then
    GO=go
fi
if ! command -v "${GO}" >/dev/null 2>&1; then
    echo "FAIL - go toolchain not available" >&2
    exit 1
fi
if ! command -v timeout >/dev/null 2>&1; then
    echo "FAIL - coreutils timeout not available" >&2
    exit 1
fi

TMP_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/awg-mesh-emulation.XXXXXX")"
REPORT_DIR="${REPORT_DIR:-${REPO_ROOT}/.agent/reports}"
TIMESTAMP="$(date +%Y%m%d-%H%M%S)"
REPORT_FILE="${REPORT_DIR}/emulation-playbook-run-${TIMESTAMP}.md"
PLAYBOOK="${PLAYBOOK:-${REPO_ROOT}/docs/PRODUCTION-TESTING-PLAYBOOK.md}"
TOPOLOGY="${REPO_ROOT}/pkg/topology/testdata/v2-topology.yml"
MESH_CTL="${TMP_ROOT}/mesh-ctl"
NODE_BIN="${TMP_ROOT}/awg-mesh-node"
CONFIG_DIR="${TMP_ROOT}/config"
RESTORED_CONFIG="${TMP_ROOT}/restored-config"
RESTORED_TOPOLOGY="${TMP_ROOT}/restored-topology.yml"
ARCHIVE="${TMP_ROOT}/backup.zip"

pass=0
fail=0
declare -a rows=()
declare -a breakages=()

cleanup() {
    rm -rf "${TMP_ROOT}"
}
trap cleanup EXIT

record_row() {
    local id="$1"
    local name="$2"
    local verdict="$3"
    local notes="$4"
    rows+=("| ${id} | ${name} | ${verdict} | ${notes} |")
}

run_capture() {
    local label="$1"
    shift
    local output="${TMP_ROOT}/${label}.out"
    if "$@" >"${output}" 2>&1; then
        printf '%s\n' "${output}"
        return 0
    fi
    cat "${output}" >&2
    return 1
}

require_contains() {
    local file="$1"
    local needle="$2"
    if ! grep -Fq "${needle}" "${file}"; then
        echo "missing expected text: ${needle}" >&2
        cat "${file}" >&2
        return 1
    fi
}

require_not_contains() {
    local file="$1"
    local needle="$2"
    if grep -Fq "${needle}" "${file}"; then
        echo "unexpected stale text: ${needle}" >&2
        grep -Fn "${needle}" "${file}" >&2
        return 1
    fi
}

scenario() {
    local id="$1"
    local name="$2"
    shift 2
    echo "=== ${id} ${name} ==="
    if "$@"; then
        ((++pass))
        record_row "${id}" "${name}" "PASS" "-"
        echo "PASS - ${id} ${name}"
        return 0
    fi
    ((++fail))
    record_row "${id}" "${name}" "FAIL" "See command output above"
    breakages+=("${id} ${name}")
    echo "FAIL - ${id} ${name}" >&2
    return 0
}

preflight_playbook_surface() {
    [[ -f "${PLAYBOOK}" ]] || return 1
    require_contains "${PLAYBOOK}" "Production Testing Playbook - awg-mesh v2.0" || return 1
    require_contains "${PLAYBOOK}" "S1 - First-run binaries" || return 1
    require_contains "${PLAYBOOK}" "Scenario Index per F-ID" || return 1
    require_not_contains "${PLAYBOOK}" "issue-92-rotation.sh" || return 1
    require_not_contains "${PLAYBOOK}" "master prepare --name" || return 1
}

s1_first_run_binaries() {
    run_capture build_mesh_ctl "${GO}" build -o "${MESH_CTL}" ./cmd/mesh-ctl >/dev/null || return 1
    run_capture build_node "${GO}" build -o "${NODE_BIN}" ./cmd/awg-mesh-node >/dev/null || return 1

    local mesh_version node_version mesh_help
    mesh_version="$(run_capture mesh_ctl_version "${MESH_CTL}" version)" || return 1
    node_version="$(run_capture node_version "${NODE_BIN}" --version)" || return 1
    mesh_help="$(run_capture mesh_ctl_help "${MESH_CTL}" --help)" || return 1

    require_contains "${mesh_version}" "mesh-ctl version" || return 1
    require_contains "${node_version}" "awg-mesh-node v" || return 1
    require_contains "${mesh_help}" "topology" || return 1
    require_contains "${mesh_help}" "node" || return 1
    require_contains "${mesh_help}" "backup" || return 1
    require_contains "${mesh_help}" "restore" || return 1
    require_contains "${mesh_help}" "upgrade" || return 1
}

s2_topology_first_look() {
    local validate_human validate_json node_json
    validate_human="$(run_capture topology_validate_human "${MESH_CTL}" --topology "${TOPOLOGY}" topology validate)" || return 1
    validate_json="$(run_capture topology_validate_json "${MESH_CTL}" --topology "${TOPOLOGY}" topology validate --output json)" || return 1
    node_json="$(run_capture node_list_json "${MESH_CTL}" --topology "${TOPOLOGY}" node list --output json)" || return 1

    require_contains "${validate_human}" "valid: schema_version=2" || return 1
    require_contains "${validate_json}" '"status": "valid"' || return 1
    require_contains "${validate_json}" '"schema_version": 2' || return 1
    require_contains "${validate_json}" '"nodes": 5' || return 1
    require_contains "${node_json}" '"count": 5' || return 1
    require_contains "${node_json}" '"master-01"' || return 1
    require_contains "${node_json}" '"ingress-de-01"' || return 1
    require_contains "${node_json}" '"home-server-01"' || return 1
}

s3_admin_prepare_artifacts() {
    run_capture prepare_master "${MESH_CTL}" --topology "${TOPOLOGY}" --config-dir "${CONFIG_DIR}" node prepare master-01 >/dev/null || return 1
    run_capture prepare_client "${MESH_CTL}" --topology "${TOPOLOGY}" --config-dir "${CONFIG_DIR}" node prepare home-server-01 >/dev/null || return 1

    [[ -f "${CONFIG_DIR}/ca.crt" ]] || return 1
    [[ -f "${CONFIG_DIR}/nodes/master-01/token" ]] || return 1
    [[ -f "${CONFIG_DIR}/nodes/master-01/mesh.token" ]] || return 1
    [[ -f "${CONFIG_DIR}/nodes/master-01/node.crt" ]] || return 1
    [[ -f "${CONFIG_DIR}/nodes/master-01/node.key" ]] || return 1
    [[ -f "${CONFIG_DIR}/nodes/home-server-01/token" ]] || return 1
    [[ -f "${CONFIG_DIR}/nodes/home-server-01/mesh.token" ]] || return 1
    [[ -f "${CONFIG_DIR}/nodes/home-server-01/node.crt" ]] || return 1
    [[ -f "${CONFIG_DIR}/nodes/home-server-01/node.key" ]] || return 1
}

s4_backup_and_restore() {
    local backup_out restore_out
    backup_out="$(run_capture backup "${MESH_CTL}" --topology "${TOPOLOGY}" --config-dir "${CONFIG_DIR}" backup "${ARCHIVE}")" || return 1
    restore_out="$(run_capture restore "${MESH_CTL}" --topology "${RESTORED_TOPOLOGY}" --config-dir "${RESTORED_CONFIG}" restore "${ARCHIVE}" --confirm)" || return 1

    require_contains "${backup_out}" "backup written" || return 1
    require_contains "${restore_out}" "backup restored" || return 1
    [[ -s "${ARCHIVE}" ]] || return 1
    [[ -f "${RESTORED_TOPOLOGY}" ]] || return 1
    [[ -f "${RESTORED_CONFIG}/nodes/master-01/token" ]] || return 1
    [[ -f "${RESTORED_CONFIG}/nodes/master-01/mesh.token" ]] || return 1
    [[ -f "${RESTORED_CONFIG}/nodes/master-01/node.crt" ]] || return 1
    [[ -f "${RESTORED_CONFIG}/nodes/master-01/node.key" ]] || return 1
    [[ -f "${RESTORED_CONFIG}/nodes/home-server-01/token" ]] || return 1
    [[ -f "${RESTORED_CONFIG}/nodes/home-server-01/mesh.token" ]] || return 1
    [[ -f "${RESTORED_CONFIG}/nodes/home-server-01/node.crt" ]] || return 1
    [[ -f "${RESTORED_CONFIG}/nodes/home-server-01/node.key" ]] || return 1
}

s5_upgrade_and_control_plane() {
    local upgrade_out cp_out state_dir code
    state_dir="${TMP_ROOT}/control-plane-state"
    upgrade_out="$(run_capture upgrade_dry_run "${MESH_CTL}" --topology "${TOPOLOGY}" upgrade v2.0.1 --dry-run)" || return 1
    require_contains "${upgrade_out}" "PHASE" || return 1
    require_contains "${upgrade_out}" "masters" || return 1
    require_contains "${upgrade_out}" "mesh-roles" || return 1
    require_contains "${upgrade_out}" "clients" || return 1

    cp_out="${TMP_ROOT}/control-plane.out"
    set +e
    timeout 3 "${NODE_BIN}" --mode control-plane --listen 127.0.0.1:0 --state-dir "${state_dir}" >"${cp_out}" 2>&1
    code=$?
    set -e
    if [[ "${code}" -ne 124 ]]; then
        cat "${cp_out}" >&2
        echo "control-plane exited with ${code}, expected timeout-managed run" >&2
        return 1
    fi
    require_contains "${cp_out}" "control-plane: listening on 127.0.0.1:0" || return 1
    require_contains "${cp_out}" "shutting down" || return 1
}

s6_critical_suite_handoff() {
    local critical_out
    critical_out="$(run_capture critical_suite bash tests/critical/run-all.sh)" || return 1
    require_contains "${critical_out}" "Critical-suite summary:" || return 1
    require_contains "${critical_out}" "0 FAIL" || return 1
}

write_report() {
    local overall gate now commit
    now="$(date '+%Y-%m-%d %H:%M:%S %Z')"
    commit="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
    mkdir -p "${REPORT_DIR}"
    if [[ "${fail}" -eq 0 ]]; then
        overall="PRODUCT_WORKS"
        gate="PASS"
    else
        overall="BROKEN"
        gate="BLOCK_RELEASE"
    fi

    {
        echo "# Playbook Run - ${now}"
        echo
        echo "**Product:** awg-mesh v2.0"
        echo "**Release candidate:** ${commit}"
        echo "**Agent identity:** customer-mode"
        echo
        echo "## Scenario results"
        echo
        echo "| # | Scenario | Verdict | Notes |"
        echo "|---|---|---|---|"
        printf '%s\n' "${rows[@]}"
        echo
        echo "## Surprises"
        echo
        echo "- None"
        echo
        echo "## Breakages"
        echo
        if [[ "${#breakages[@]}" -eq 0 ]]; then
            echo "- None"
        else
            printf -- '- %s\n' "${breakages[@]}"
        fi
        echo
        echo "## Overall verdict"
        echo
        echo "${overall}"
        echo
        echo "## Gate decision"
        echo
        echo "${gate}"
    } >"${REPORT_FILE}"
}

echo "=== Preflight Playbook surface is v2.0 and runnable ==="
if preflight_playbook_surface; then
    echo "PASS - Preflight Playbook surface is v2.0 and runnable"
else
    fail=$((fail + 1))
    record_row "PRE" "Playbook surface is v2.0 and runnable" "FAIL" "See command output above"
    breakages+=("PRE Playbook surface is v2.0 and runnable")
    write_report
    echo "Scenario summary: ${pass} PASS, ${fail} FAIL"
    echo "Report: ${REPORT_FILE}"
    echo "Overall verdict: BROKEN"
    echo "Gate decision: BLOCK_RELEASE"
    exit 1
fi

scenario "S1" "First-run binaries" s1_first_run_binaries
scenario "S2" "Topology-as-code first look" s2_topology_first_look
scenario "S3" "Admin prepare artifacts" s3_admin_prepare_artifacts
scenario "S4" "Backup and restore" s4_backup_and_restore
scenario "S5" "Upgrade plan and control-plane startup" s5_upgrade_and_control_plane
scenario "S6" "Critical-suite handoff" s6_critical_suite_handoff

write_report

echo "Scenario summary: ${pass} PASS, ${fail} FAIL"
echo "Report: ${REPORT_FILE}"
if [[ "${fail}" -ne 0 ]]; then
    echo "Overall verdict: BROKEN"
    echo "Gate decision: BLOCK_RELEASE"
    exit 1
fi

echo "Overall verdict: PRODUCT_WORKS"
echo "Gate decision: PASS"
echo "PRODUCT_WORKS"
