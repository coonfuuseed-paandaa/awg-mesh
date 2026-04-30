#!/usr/bin/env bash
# F-004 FR-2: iperf3 multi-flow throughput baseline assertion module.
#
# Runs `iperf3 -P 8` from endpoint-asia-01 -> endpoint-asia-02 over the
# overlay through master ECMP, captures aggregate Mbps, and asserts the
# measurement is within +/-20% of the persisted baseline (NFR-3).
#
# Baseline policy (spec C1, ADR-005):
#   - On absent baseline + no --update-baseline flag: bootstrap mode.
#     The current measurement is written as the baseline, exit 0,
#     informative message. Operator re-runs to get the assertion.
#   - On present baseline + no flag: assert mode. Mbps must lie in
#     [baseline*0.8, baseline*1.2], otherwise exit 1.
#   - On --update-baseline: overwrite the baseline regardless of state,
#     exit 0. This is the ONLY way to mutate a committed baseline.
#     Auto-update is intentionally not implemented (C1 invariant).
#
# Topology assumption:
#   The module is invoked by tests/simulation/data-plane-extended.sh
#   (T-009, future) which bootstraps the standard 5-node topology via
#   tests/simulation/lib/topology-bootstrap.sh (T-001, future). For
#   standalone development runs, operator first spins up the issue-92
#   sim topology and exports IPERF3_CLIENT_CTR / IPERF3_SERVER_CTR /
#   IPERF3_SERVER_OVERLAY env vars. Sensible defaults match issue-92.
#
# Linux netns + CAP_NET_ADMIN (NFR-5): non-Linux exit 0 + skip message.
#
# Usage:
#   bash tests/simulation/modules/fr2-iperf3-baseline.sh [--update-baseline]
#
# Exit:
#   0  PASS or bootstrap or update or skip
#   1  regression > +/-20%
#   2  environment error (topology not running, iperf3 install failed)
set -euo pipefail

# ---------------------------------------------------------------------------
# Platform guard (NFR-5).
# ---------------------------------------------------------------------------
if [[ "$(uname -s)" != "Linux" ]]; then
    printf '[FR-2] SKIP: requires Linux (CAP_NET_ADMIN + netns).\n'
    printf '       Run inside WSL2 Ubuntu or a CI Linux runner.\n'
    exit 0
fi

# ---------------------------------------------------------------------------
# CLI parse.
# ---------------------------------------------------------------------------
UPDATE_BASELINE=0
for arg in "$@"; do
    case "${arg}" in
        --update-baseline) UPDATE_BASELINE=1 ;;
        -h|--help)
            printf 'Usage: %s [--update-baseline]\n' "${0##*/}"
            printf '  Run iperf3 -P 8 endpoint-asia-01 -> endpoint-asia-02,\n'
            printf '  assert within +/-20%% of baseline (NFR-3).\n'
            printf '  Pass --update-baseline to overwrite baseline (C1).\n'
            exit 0
            ;;
        *)
            printf '[FR-2] unknown arg: %s (try --help)\n' "${arg}" >&2
            exit 2
            ;;
    esac
done

# ---------------------------------------------------------------------------
# Paths.
# ---------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SIM_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
BASELINE_DIR="${SIM_DIR}/baseline"
BASELINE_FILE="${BASELINE_DIR}/iperf3.json"

# ---------------------------------------------------------------------------
# Test fixture parameters. Defaults align with tests/simulation/issue-92-rotation.sh.
# Override via env when running standalone outside the F-004 orchestrator.
# ---------------------------------------------------------------------------
IPERF3_CLIENT_CTR="${IPERF3_CLIENT_CTR:-issue92rot-node-asia-01}"
IPERF3_SERVER_CTR="${IPERF3_SERVER_CTR:-issue92rot-node-asia-02}"
IPERF3_SERVER_OVERLAY="${IPERF3_SERVER_OVERLAY:-172.21.92.36}"
IPERF3_PORT="${IPERF3_PORT:-5201}"
IPERF3_PARALLEL="${IPERF3_PARALLEL:-8}"
IPERF3_DURATION="${IPERF3_DURATION:-10}"
IPERF3_ARGS="-P ${IPERF3_PARALLEL} -t ${IPERF3_DURATION}"

# ---------------------------------------------------------------------------
# Cleanup trap. Stops any iperf3 server we started; never tears down
# the topology (orchestrator owns that lifecycle).
# ---------------------------------------------------------------------------
SERVER_STARTED=0
# shellcheck disable=SC2329  # invoked via trap below
cleanup() {
    if [[ "${SERVER_STARTED}" == "1" ]]; then
        docker exec "${IPERF3_SERVER_CTR}" pkill -f 'iperf3 -s' >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Helpers.
# ---------------------------------------------------------------------------
iperf3::ensure_binary() {
    local ctr="$1"
    if docker exec "${ctr}" sh -c 'command -v iperf3' >/dev/null 2>&1; then
        return 0
    fi
    printf '  [info] installing iperf3 in %s...\n' "${ctr}"
    if docker exec "${ctr}" sh -c 'command -v apk' >/dev/null 2>&1; then
        if docker exec "${ctr}" apk add --no-cache iperf3 >/dev/null 2>&1; then
            return 0
        fi
    fi
    if docker exec "${ctr}" sh -c 'command -v apt-get' >/dev/null 2>&1; then
        if docker exec "${ctr}" sh -c 'apt-get update >/dev/null && apt-get install -y iperf3 >/dev/null'; then
            return 0
        fi
    fi
    printf '[FR-2] FAIL: cannot install iperf3 in %s (no apk/apt-get).\n' "${ctr}" >&2
    return 1
}

iperf3::require_topology() {
    if ! docker inspect -f '{{.State.Running}}' "${IPERF3_CLIENT_CTR}" 2>/dev/null | grep -q true; then
        printf '[FR-2] FAIL: client container %s not running.\n' "${IPERF3_CLIENT_CTR}" >&2
        printf '       Bring up topology first (issue-92-rotation.sh or data-plane-extended.sh).\n' >&2
        return 1
    fi
    if ! docker inspect -f '{{.State.Running}}' "${IPERF3_SERVER_CTR}" 2>/dev/null | grep -q true; then
        printf '[FR-2] FAIL: server container %s not running.\n' "${IPERF3_SERVER_CTR}" >&2
        return 1
    fi
}

iperf3::start_server() {
    docker exec "${IPERF3_SERVER_CTR}" pkill -f 'iperf3 -s' >/dev/null 2>&1 || true
    docker exec -d "${IPERF3_SERVER_CTR}" iperf3 -s -p "${IPERF3_PORT}" -1 >/dev/null
    SERVER_STARTED=1
    sleep 1
}

# Run iperf3 client and emit aggregate Mbps on stdout. Stderr-clean on success.
iperf3::run_client() {
    local raw
    # iperf3 -J emits a single JSON document on stdout. The aggregate is
    # at .end.sum_sent.bits_per_second (TCP). bps -> Mbps via /1e6.
    raw=$(docker exec "${IPERF3_CLIENT_CTR}" iperf3 \
        -c "${IPERF3_SERVER_OVERLAY}" \
        -p "${IPERF3_PORT}" \
        -P "${IPERF3_PARALLEL}" \
        -t "${IPERF3_DURATION}" \
        -J 2>/dev/null)
    if [[ -z "${raw}" ]]; then
        printf '[FR-2] FAIL: iperf3 client produced no JSON output.\n' >&2
        return 1
    fi
    printf '%s' "${raw}" | jq -r '.end.sum_sent.bits_per_second / 1000000 | floor'
}

iperf3::baseline_read() {
    if [[ ! -r "${BASELINE_FILE}" ]]; then
        return 1
    fi
    jq -r '.iperf3_aggregate_mbps' "${BASELINE_FILE}"
}

iperf3::baseline_write() {
    local mbps="$1"
    local commit ts
    commit=$(git -C "${SIM_DIR}" rev-parse --short HEAD 2>/dev/null || echo unknown)
    ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    mkdir -p "${BASELINE_DIR}"
    jq -n \
        --arg captured_at "${ts}" \
        --arg git_commit "${commit}" \
        --argjson mbps "${mbps}" \
        --arg iperf3_args "${IPERF3_ARGS}" \
        '{
            version: 1,
            captured_at: $captured_at,
            git_commit: $git_commit,
            iperf3_aggregate_mbps: $mbps,
            iperf3_args: $iperf3_args,
            host_class: "warm-docker-daemon-wsl2-or-linux"
        }' > "${BASELINE_FILE}"
}

# ---------------------------------------------------------------------------
# Run.
# ---------------------------------------------------------------------------
printf '[FR-2] iperf3 multi-flow throughput baseline assertion\n'
printf '       client=%s server=%s overlay=%s args="%s"\n' \
    "${IPERF3_CLIENT_CTR}" "${IPERF3_SERVER_CTR}" "${IPERF3_SERVER_OVERLAY}" "${IPERF3_ARGS}"

iperf3::require_topology || exit 2
iperf3::ensure_binary "${IPERF3_SERVER_CTR}" || exit 2
iperf3::ensure_binary "${IPERF3_CLIENT_CTR}" || exit 2

iperf3::start_server

CURRENT_MBPS=$(iperf3::run_client) || exit 2
if ! [[ "${CURRENT_MBPS}" =~ ^[0-9]+$ ]] || [[ "${CURRENT_MBPS}" -le 0 ]]; then
    printf '[FR-2] FAIL: invalid Mbps reading: "%s"\n' "${CURRENT_MBPS}" >&2
    exit 2
fi
printf '  [info] measured aggregate: %s Mbps\n' "${CURRENT_MBPS}"

# --update-baseline: overwrite + exit 0 with hint.
if [[ "${UPDATE_BASELINE}" == "1" ]]; then
    iperf3::baseline_write "${CURRENT_MBPS}"
    printf '[FR-2] BASELINE UPDATED: %s Mbps -> %s\n' "${CURRENT_MBPS}" "${BASELINE_FILE}"
    printf '       Review with: git diff %s\n' "${BASELINE_FILE}"
    exit 0
fi

# No baseline yet + no flag: bootstrap mode (auto-write, exit 0, hint).
if ! BASELINE_MBPS=$(iperf3::baseline_read); then
    iperf3::baseline_write "${CURRENT_MBPS}"
    printf '[FR-2] BOOTSTRAP: no baseline found.\n'
    printf '       Wrote %s Mbps to %s\n' "${CURRENT_MBPS}" "${BASELINE_FILE}"
    printf '       Re-run module to assert against this baseline.\n'
    exit 0
fi

# Assert mode.
LOWER=$(awk -v b="${BASELINE_MBPS}" 'BEGIN { printf "%d", b * 0.8 }')
UPPER=$(awk -v b="${BASELINE_MBPS}" 'BEGIN { printf "%d", b * 1.2 }')
DELTA_PCT=$(awk -v c="${CURRENT_MBPS}" -v b="${BASELINE_MBPS}" \
    'BEGIN { if (b == 0) { print "NaN"; exit } printf "%+.1f", (c - b) * 100.0 / b }')

printf '  [info] baseline: %s Mbps  window: [%s, %s] Mbps  delta: %s%%\n' \
    "${BASELINE_MBPS}" "${LOWER}" "${UPPER}" "${DELTA_PCT}"

if (( CURRENT_MBPS < LOWER )); then
    printf '[FR-2] FAIL: throughput regression. %s Mbps < %s Mbps lower bound (delta %s%%).\n' \
        "${CURRENT_MBPS}" "${LOWER}" "${DELTA_PCT}" >&2
    printf '       Investigate before passing --update-baseline (C1).\n' >&2
    exit 1
fi
if (( CURRENT_MBPS > UPPER )); then
    printf '[FR-2] FAIL: throughput out of upper bound. %s Mbps > %s Mbps (delta %s%%).\n' \
        "${CURRENT_MBPS}" "${UPPER}" "${DELTA_PCT}" >&2
    printf '       Confirm with rerun, then --update-baseline if real perf gain (C1).\n' >&2
    exit 1
fi

printf '[FR-2] PASS: %s Mbps in [%s, %s] (baseline %s, delta %s%%).\n' \
    "${CURRENT_MBPS}" "${LOWER}" "${UPPER}" "${BASELINE_MBPS}" "${DELTA_PCT}"
exit 0
