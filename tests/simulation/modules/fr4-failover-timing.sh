#!/usr/bin/env bash
# F-004 FR-4: failover timing assertion module.
#
# Measures data-plane recovery after a master container is killed mid-flow.
# Method (ADR-006): persistent ICMP ping endpoint -> endpoint at 1 pkt/sec
# with kernel timestamps, then `docker kill <master>` at recorded T0,
# continue capturing replies for a recovery window, parse the gap.
#
# Assertions (NFR-1 + spec Edge Case):
#   - recovery_seconds  <= 10  (NFR-1: data plane восстанавливается ≤ 10s)
#   - lost_packets      <= 10 + 2 (FR-4 + Edge Case: ±2 docker-kill race)
#
# Topology assumption:
#   The module is invoked by tests/simulation/data-plane-extended.sh
#   (T-009, future). For standalone development, operator first spins up
#   the issue-92 sim topology with NO_CLEANUP=1, then runs this module.
#   Defaults match the issue-92 fixture; override via env if needed.
#
# Re-entrancy: cleanup trap restarts the killed master container, so the
# module can be re-run without manual recovery. If a prior run aborted
# without trap firing, operator must `docker start <master>` once.
#
# Linux netns + CAP_NET_ADMIN (NFR-5): non-Linux exit 0 + skip message.
#
# Usage:
#   bash tests/simulation/modules/fr4-failover-timing.sh [--help]
#
# Exit:
#   0  PASS or skip
#   1  assertion failed (recovery > 10s OR lost > 12 packets)
#   2  environment error (topology not running, kill target missing,
#                         pre-flight ping failed, master already killed)
set -euo pipefail

# ---------------------------------------------------------------------------
# Platform guard (NFR-5).
# ---------------------------------------------------------------------------
if [[ "$(uname -s)" != "Linux" ]]; then
    printf '[FR-4] SKIP: requires Linux (CAP_NET_ADMIN + netns).\n'
    printf '       Run inside WSL2 Ubuntu or a CI Linux runner.\n'
    exit 0
fi

# ---------------------------------------------------------------------------
# CLI parse.
# ---------------------------------------------------------------------------
for arg in "$@"; do
    case "${arg}" in
        -h|--help)
            printf 'Usage: %s [--help]\n' "${0##*/}"
            printf '  Persistent ping endpoint -> endpoint, docker kill master at T0,\n'
            printf '  measure recovery_seconds + lost_packets. PASS: <=10s and <=12.\n'
            printf '  Env override: MASTER_KILL_CTR PING_CLIENT_CTR PING_TARGET_OVERLAY.\n'
            exit 0
            ;;
        *)
            printf '[FR-4] unknown arg: %s (try --help)\n' "${arg}" >&2
            exit 2
            ;;
    esac
done

# ---------------------------------------------------------------------------
# Test fixture parameters. Defaults align with tests/simulation/issue-92-rotation.sh.
# Override via env when running standalone outside the F-004 orchestrator.
# ---------------------------------------------------------------------------
MASTER_KILL_CTR="${MASTER_KILL_CTR:-issue92rot-mst-ru-01}"
PING_CLIENT_CTR="${PING_CLIENT_CTR:-issue92rot-node-asia-01}"
PING_TARGET_OVERLAY="${PING_TARGET_OVERLAY:-172.21.92.36}"

# Timing budget.
RECOVERY_BUDGET_S=10                # NFR-1
LOST_PACKETS_BUDGET=10              # FR-4
LOST_PACKETS_TOLERANCE=2            # Edge Case (docker kill race)
LOST_PACKETS_LIMIT=$(( LOST_PACKETS_BUDGET + LOST_PACKETS_TOLERANCE ))
WAIT_BEFORE_KILL_S=5                # baseline ping flow stabilisation
WAIT_AFTER_KILL_S=20                # capture window (>RECOVERY_BUDGET with margin)

# Working files.
PING_LOG="$(mktemp /tmp/fr4-ping-XXXXXX.log)"
PING_PID=""
T0=""

# ---------------------------------------------------------------------------
# Cleanup trap. Restarts the killed master to keep the topology re-runnable;
# never tears down the topology itself (orchestrator owns that lifecycle).
# ---------------------------------------------------------------------------
# shellcheck disable=SC2329  # invoked via trap below
cleanup() {
    local exit_rc=$?
    if [[ -n "${PING_PID}" ]]; then
        kill "${PING_PID}" 2>/dev/null || true
        wait "${PING_PID}" 2>/dev/null || true
    fi
    # Always try to restart the kill target; idempotent (no-op if already up).
    if ! docker inspect -f '{{.State.Running}}' "${MASTER_KILL_CTR}" 2>/dev/null \
            | grep -q true; then
        printf '[FR-4] cleanup: docker start %s\n' "${MASTER_KILL_CTR}"
        docker start "${MASTER_KILL_CTR}" >/dev/null 2>&1 || true
        # Brief settle delay so subsequent invocations / orchestrator steps
        # see a master that is at least past container init.
        sleep 5
    fi
    # Preserve the ping log on failure (operator inspects it for diagnosis)
    # or when KEEP_ARTIFACTS=1 is set explicitly. Only remove on success.
    if (( exit_rc != 0 )) || [[ -n "${KEEP_ARTIFACTS:-}" ]]; then
        printf '[FR-4] cleanup: preserving ping log %s (exit_rc=%s, KEEP_ARTIFACTS=%s)\n' \
            "${PING_LOG}" "${exit_rc}" "${KEEP_ARTIFACTS:-unset}"
    else
        rm -f "${PING_LOG}"
    fi
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Helpers.
# ---------------------------------------------------------------------------
fr4::require_running() {
    local ctr="$1"
    if ! docker inspect -f '{{.State.Running}}' "${ctr}" 2>/dev/null \
            | grep -q true; then
        printf '[FR-4] FAIL: container %s not running.\n' "${ctr}" >&2
        printf '       Bring up topology first (issue-92-rotation.sh NO_CLEANUP=1\n' >&2
        printf '       or data-plane-extended.sh).\n' >&2
        return 1
    fi
}

# Pre-warm the overlay path. After a fresh topology bring-up the wireguard
# handshake may be cold; first ping can race the timer. Two best-effort
# attempts then one strict probe — matches issue-92 R7.2 / R10 pattern.
fr4::preflight_ping() {
    docker exec "${PING_CLIENT_CTR}" \
        ping -c 1 -W 5 "${PING_TARGET_OVERLAY}" >/dev/null 2>&1 || true
    docker exec "${PING_CLIENT_CTR}" \
        ping -c 1 -W 5 "${PING_TARGET_OVERLAY}" >/dev/null 2>&1 || true
    if ! docker exec "${PING_CLIENT_CTR}" \
            ping -c 2 -W 2 "${PING_TARGET_OVERLAY}" >/dev/null 2>&1; then
        printf '[FR-4] FAIL: pre-flight ping %s -> %s failed.\n' \
            "${PING_CLIENT_CTR}" "${PING_TARGET_OVERLAY}" >&2
        printf '       Topology not data-plane-ready; nothing to measure.\n' >&2
        return 1
    fi
}

# Parse PING_LOG and emit two whitespace-separated values: recovery_seconds lost_packets.
# `ping -D` line shape: "[1714400000.123456] 64 bytes from 172.21.92.36: icmp_seq=15 ttl=64 time=1.23 ms"
# We consider only successful reply lines. Recovery = ts(first_seq_after_T0) - T0.
# Lost = first_seq_after_T0 - last_seq_before_T0 - 1.
# Emits "NaN <count>" if no reply observed after T0 (permanent failure).
fr4::parse_log() {
    local t0="$1"
    awk -v t0="${t0}" '
        /icmp_seq=/ {
            ts_field = $1
            gsub(/[\[\]]/, "", ts_field)
            for (i = 1; i <= NF; i++) {
                if ($i ~ /icmp_seq=/) {
                    seq_str = $i
                    sub(/^icmp_seq=/, "", seq_str)
                    seq = seq_str + 0
                    break
                }
            }
            ts = ts_field + 0
            if (ts < t0) {
                if (seq > last_pre_seq) {
                    last_pre_seq = seq
                }
                pre_seen = 1
            } else {
                if (first_post_ts == 0 || ts < first_post_ts) {
                    first_post_ts = ts
                    first_post_seq = seq
                }
                post_seen = 1
            }
        }
        END {
            if (!post_seen) {
                printf "NaN %d\n", 0
                exit 0
            }
            if (!pre_seen) {
                printf "NaN %d\n", 0
                exit 0
            }
            recovery = first_post_ts - t0
            lost = first_post_seq - last_pre_seq - 1
            if (lost < 0) { lost = 0 }
            printf "%.3f %d\n", recovery, lost
        }
    ' "${PING_LOG}"
}

# ---------------------------------------------------------------------------
# Run.
# ---------------------------------------------------------------------------
printf '[FR-4] failover timing assertion\n'
printf '       kill_ctr=%s ping_client=%s target=%s\n' \
    "${MASTER_KILL_CTR}" "${PING_CLIENT_CTR}" "${PING_TARGET_OVERLAY}"
printf '       budget: recovery <= %ds, lost <= %d (= %d + %d tolerance)\n' \
    "${RECOVERY_BUDGET_S}" "${LOST_PACKETS_LIMIT}" \
    "${LOST_PACKETS_BUDGET}" "${LOST_PACKETS_TOLERANCE}"

fr4::require_running "${MASTER_KILL_CTR}" || exit 2
fr4::require_running "${PING_CLIENT_CTR}" || exit 2
fr4::preflight_ping || exit 2

# Validate that the kill target IS the master currently carrying the flow.
# If the ping is already pinned to MASTER_OTHER, killing MASTER_KILL is a
# no-op for this flow — module would falsely report recovery=0 / lost=0
# without ever exercising failover. Per AGENTS.md per-master-iface pattern,
# the kill target's iface on the source endpoint is `wg-<master-short-name>`.
master_short="${MASTER_KILL_CTR#*-}"
expected_iface="wg-${master_short}"
active_iface=$(docker exec "${PING_CLIENT_CTR}" \
    ip route get "${PING_TARGET_OVERLAY}" 2>/dev/null \
    | awk '/dev/ { for (i=1; i<=NF; i++) if ($i == "dev") { print $(i+1); exit } }')
if [[ -z "${active_iface}" ]]; then
    printf '[FR-4] FAIL: could not determine active path iface from %s -> %s.\n' \
        "${PING_CLIENT_CTR}" "${PING_TARGET_OVERLAY}" >&2
    printf '       Topology may not be data-plane-ready; aborting before kill.\n' >&2
    exit 2
fi
if [[ "${active_iface}" != "${expected_iface}" ]]; then
    printf '[FR-4] FAIL: ping flow pinned to %s but kill target maps to %s.\n' \
        "${active_iface}" "${expected_iface}" >&2
    printf '       Killing %s would not exercise failover — set MASTER_KILL_CTR\n' "${MASTER_KILL_CTR}" >&2
    printf '       to the master whose iface matches %s, or change PING_CLIENT_CTR.\n' "${active_iface}" >&2
    exit 2
fi
printf '  [info] flow pinned to %s (matches kill target %s)\n' "${active_iface}" "${MASTER_KILL_CTR}"

# Launch persistent ping with kernel timestamps. -O prints "no answer yet"
# lines so the awk parser is unaffected (we filter on /icmp_seq=/).
docker exec "${PING_CLIENT_CTR}" \
    ping -D -O -i 1 -W 1 "${PING_TARGET_OVERLAY}" >"${PING_LOG}" 2>&1 &
PING_PID=$!
printf '  [info] persistent ping started (pid=%s log=%s)\n' "${PING_PID}" "${PING_LOG}"

sleep "${WAIT_BEFORE_KILL_S}"

T0=$(date +%s.%N)
printf '  [info] T0=%s — docker kill %s\n' "${T0}" "${MASTER_KILL_CTR}"
docker kill "${MASTER_KILL_CTR}" >/dev/null

sleep "${WAIT_AFTER_KILL_S}"

# Stop the ping cleanly so the log is flushed before parsing.
kill "${PING_PID}" 2>/dev/null || true
wait "${PING_PID}" 2>/dev/null || true
PING_PID=""

read -r RECOVERY_S LOST <<<"$(fr4::parse_log "${T0}")"

if [[ "${RECOVERY_S}" == "NaN" ]]; then
    printf '[FR-4] FAIL: no reply observed after T0 (permanent failure).\n' >&2
    printf '       T0=%s log=%s\n' "${T0}" "${PING_LOG}" >&2
    exit 1
fi

printf '  [info] T0=%s recovery_seconds=%s lost_packets=%s\n' \
    "${T0}" "${RECOVERY_S}" "${LOST}"

# Recovery gate: floor compare via awk because RECOVERY_S is float.
RECOVERY_OVER=$(awk -v r="${RECOVERY_S}" -v b="${RECOVERY_BUDGET_S}" \
    'BEGIN { print (r > b) ? 1 : 0 }')
if [[ "${RECOVERY_OVER}" == "1" ]]; then
    printf '[FR-4] FAIL: recovery %.3fs > %ds budget (NFR-1).\n' \
        "${RECOVERY_S}" "${RECOVERY_BUDGET_S}" >&2
    exit 1
fi
if (( LOST > LOST_PACKETS_LIMIT )); then
    printf '[FR-4] FAIL: lost %d packets > %d limit (= %d + %d tolerance).\n' \
        "${LOST}" "${LOST_PACKETS_LIMIT}" \
        "${LOST_PACKETS_BUDGET}" "${LOST_PACKETS_TOLERANCE}" >&2
    exit 1
fi

printf '[FR-4] PASS: recovery=%ss loss=%d (budget %ds / %d).\n' \
    "${RECOVERY_S}" "${LOST}" "${RECOVERY_BUDGET_S}" "${LOST_PACKETS_LIMIT}"
exit 0
