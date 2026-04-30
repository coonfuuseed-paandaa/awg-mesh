#!/usr/bin/env bash
# F-004 FR-6: Sticky session migration on healthcheck flip module.
#
# Asserts that `docker pause master-01` triggers correct healthcheck behavior
# in the awg-mesh data plane, per spec FR-6 + US6 + CR-002 amendment:
#
#   1. Healthcheck removes master-01 from nexthop set within MIGRATION_TIMEOUT_S
#      (default 30s) (US6 AC-1).
#   2. NEW connections (initiated AFTER pause) MUST route through master-02
#      (US6 AC-1; FR-6 PASS condition).
#   3. EXISTING connections (initiated BEFORE pause) drop — this is the
#      explicit awg-mesh contract per CR-002 spec amendment: no built-in
#      connection migration on nexthop removal. Drop is the EXPECTED behavior.
#      Assertion A2 is therefore an advisory observation (not a strict gate)
#      with ±2 tolerance for packets in flight at pause moment.
#   4. `docker unpause master-01` restores topology — master-01 returns as a
#      nexthop option, balance recovers (US6 AC-3; recovery assertion A3).
#
# Method (ADR-003 + spec FR-6):
#   - tcpdump per-master pcap pattern (same as fr1, fr3, fr5). Two phases:
#     (a) pre-pause: 50 long-lived TCP flows from SRC_INITIATOR -> DST_TARGET
#         emit continuous data so each flow is visible to per-master tcpdump.
#         Capture src ports per master to learn pre-pause distribution.
#     (b) post-pause: tcpdump only on the OTHER (non-paused) master — pause
#         freezes ALL processes inside the paused container, so observation
#         flows from the healthy side. Pre-pause ports on other-master post-T0
#         = A2 existing-flow leakage. Post-pause ports = A1 PASS evidence.
#   - Recovery test: single TCP probe after unpause to verify topology lives.
#
# No NFR-5 conntrack gate required (fr3 uses conntrack-tools detect; FR-6
# uses tcpdump only — gate doesn't apply).
#
# Self-test mode (FR6_SELF_TEST=no-migration): bypasses real measurement,
# injects synthetic post-pause pcap state where new connections still hit
# master-01 (paused), and verifies the assertion correctly returns FAIL.
# Anti-stub regression guard.
#
# Helpers inline (T-001 lib extract deferred per TD-2026-04-30 — AGENTS.md
# / tasks.md): conntrack/tcpdump observation helpers copied verbatim from
# fr3-conntrack-sticky.sh with `fr6::` namespace prefix. Lib extract waits
# for Rule of Three trigger (third sim harness consumer).
#
# Topology assumption: invoked by tests/simulation/data-plane-extended.sh
# (T-009, future). For standalone runs: spin up issue-92 with NO_CLEANUP=1,
# then run this module. Defaults match the issue-92 fixture.
#
# Pause vs kill distinction (vs FR-4):
#   FR-4 uses `docker kill` — process terminated, container exits, data plane
#   loses the master entirely (recovery via container restart).
#   FR-6 uses `docker pause` — process frozen via SIGSTOP, container alive but
#   unresponsive. Healthcheck must detect via gRPC/probe timeout. unpause
#   restores instantly. Different recovery semantics; FR-6 specifically
#   exercises the healthcheck-flip path that FR-4 does not.
#
# Re-entrant: cleanup trap kills socats and tcpdumps, removes pcaps, AND
# unpauses the target master (re-entrancy critical — a stale paused container
# blocks subsequent runs). Never tears down topology (orchestrator owns that).
#
# Linux netns + CAP_NET_ADMIN (NFR-5): non-Linux exit 0 + skip message.
#
# Usage:
#   bash tests/simulation/modules/fr6-sticky-migration.sh [--help]
#
# Env overrides:
#   MASTER_PAUSE_CTR / MASTER_OTHER_CTR        master container names
#   SRC_INITIATOR_CTR / SRC_INITIATOR_OVERLAY  source endpoint container
#   SRC_INITIATOR_NAME                         source endpoint logical name (for wg-* iface)
#   DST_TARGET_CTR / TARGET_OVERLAY            destination endpoint
#   LISTEN_PORT                                dst TCP listener port (default 9996)
#   PRE_PAUSE_CONNECTIONS                      pre-pause flow count (default 50)
#   POST_PAUSE_CONNECTIONS                     post-pause flow count (default 20)
#   PRE_PORT_RANGE_START                       pre-pause src port base (default 50000)
#   POST_PORT_RANGE_START                      post-pause src port base (default 50100)
#   MIGRATION_TIMEOUT_S                        healthcheck flip window (default 30)
#   RECOVERY_SETTLE_S                          wait after unpause (default 10)
#   EXISTING_FLOW_TOLERANCE                    ±packets allowed post-T0 on paused master (default 2)
#   FR6_SELF_TEST                              "no-migration" to test the test
#
# Exit:
#   0  PASS or skip (non-Linux / topology not running but skip OK / self-test PASS)
#   1  assertion failed (new connections still hit paused master, OR recovery
#      probe fails, OR self-test mode succeeded in catching synthetic regression)
#   2  environment error (topology not running, tools install failed, no traffic)
set -euo pipefail

# ---------------------------------------------------------------------------
# Platform guard (NFR-5).
# ---------------------------------------------------------------------------
if [[ "$(uname -s)" != "Linux" ]]; then
    printf '[FR-6] SKIP: requires Linux (CAP_NET_ADMIN + netns).\n'
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
            printf '  Open %d long-lived TCP connections SRC -> DST via ECMP, capture per-\n' 50
            printf '  master tcpdump pre-pause distribution. docker pause master-01, wait up\n'
            printf '  to %ds for healthcheck flip. Open %d NEW TCP connections on a distinct\n' 30 20
            printf '  src-port range; assert ALL land on master-02 (A1, FR-6 PASS condition).\n'
            printf '  Observe pre-pause flows post-T0 on paused master (A2, existing-flow\n'
            printf '  contract per CR-002: drop expected, ±2 tolerance for in-flight). Then\n'
            printf '  docker unpause master-01 + recovery probe (A3).\n'
            printf '\n'
            printf '  Env overrides: MASTER_PAUSE_CTR MASTER_OTHER_CTR\n'
            printf '                 SRC_INITIATOR_CTR SRC_INITIATOR_OVERLAY SRC_INITIATOR_NAME\n'
            printf '                 DST_TARGET_CTR TARGET_OVERLAY\n'
            printf '                 LISTEN_PORT PRE_PAUSE_CONNECTIONS POST_PAUSE_CONNECTIONS\n'
            printf '                 PRE_PORT_RANGE_START POST_PORT_RANGE_START\n'
            printf '                 MIGRATION_TIMEOUT_S RECOVERY_SETTLE_S EXISTING_FLOW_TOLERANCE\n'
            printf '                 FR6_SELF_TEST\n'
            printf '\n'
            printf '  FR6_SELF_TEST=no-migration injects synthetic post-pause state where\n'
            printf '  new connections still hit the paused master and verifies the assertion\n'
            printf '  correctly returns FAIL. Module exits 0 when synthetic regression caught,\n'
            printf '  exits 1 if it slips through.\n'
            exit 0
            ;;
        *)
            printf '[FR-6] unknown arg: %s (try --help)\n' "${arg}" >&2
            exit 2
            ;;
    esac
done

# ---------------------------------------------------------------------------
# Test fixture parameters. Defaults align with issue-92-rotation.sh.
# Source initiator = ep-us-01 (overlay 172.21.92.34); ingress iface on each
# master = "wg-" + endpoint-name (per pkg/node/master.go:256).
# ---------------------------------------------------------------------------
MASTER_PAUSE_CTR="${MASTER_PAUSE_CTR:-issue92rot-mst-ru-01}"
MASTER_OTHER_CTR="${MASTER_OTHER_CTR:-issue92rot-mst-ru-02}"
SRC_INITIATOR_CTR="${SRC_INITIATOR_CTR:-issue92rot-ep-us-01}"
SRC_INITIATOR_OVERLAY="${SRC_INITIATOR_OVERLAY:-172.21.92.34}"
SRC_INITIATOR_NAME="${SRC_INITIATOR_NAME:-ep-us-01}"
DST_TARGET_CTR="${DST_TARGET_CTR:-issue92rot-node-asia-02}"
TARGET_OVERLAY="${TARGET_OVERLAY:-172.21.92.36}"

LISTEN_PORT="${LISTEN_PORT:-9996}"
PRE_PAUSE_CONNECTIONS="${PRE_PAUSE_CONNECTIONS:-50}"
POST_PAUSE_CONNECTIONS="${POST_PAUSE_CONNECTIONS:-20}"
PRE_PORT_RANGE_START="${PRE_PORT_RANGE_START:-50000}"
POST_PORT_RANGE_START="${POST_PORT_RANGE_START:-50100}"
MIGRATION_TIMEOUT_S="${MIGRATION_TIMEOUT_S:-30}"
RECOVERY_SETTLE_S="${RECOVERY_SETTLE_S:-10}"
EXISTING_FLOW_TOLERANCE="${EXISTING_FLOW_TOLERANCE:-2}"
FR6_SELF_TEST="${FR6_SELF_TEST:-}"

# Master-side iface from SRC_INITIATOR; capturing master = nexthop.
SRC_INGRESS_IFACE="wg-${SRC_INITIATOR_NAME}"
PRE_PORT_RANGE_END=$(( PRE_PORT_RANGE_START + PRE_PAUSE_CONNECTIONS - 1 ))
POST_PORT_RANGE_END=$(( POST_PORT_RANGE_START + POST_PAUSE_CONNECTIONS - 1 ))

# Pcap files inside each master container. Pre-pause captures both masters
# for baseline distribution. Post-pause captures only on the OTHER (non-
# paused) master because docker pause freezes any tcpdump inside the paused
# container; we observe migration FROM the healthy side.
M01_PRE_PCAP="/tmp/fr6-pre-master-01.pcap"
M02_PRE_PCAP="/tmp/fr6-pre-master-02.pcap"
M02_POSTNEW_PCAP="/tmp/fr6-postnew-master-02.pcap"

# Settle delays (seconds): WG handshake warm-up, pre-pause capture window,
# post-pause new-flow capture window. Trap PAUSED flag triggers unpause.
TCPDUMP_STARTUP_S=1
WARMUP_S=4
PRE_CAPTURE_S=4
POSTNEW_CAPTURE_S=4
PAUSED=0

# ---------------------------------------------------------------------------
# Cleanup trap. Always kills tcpdump + socats inside containers, removes
# pcaps, AND unpauses the target master if still paused. Re-entrant: stale
# processes / paused state from a prior run get reaped. Never tears down
# topology (orchestrator owns that lifecycle).
# ---------------------------------------------------------------------------
# shellcheck disable=SC2329  # invoked via trap below
cleanup() {
    for ctr in "${MASTER_PAUSE_CTR}" "${MASTER_OTHER_CTR}"; do
        docker exec "${ctr}" sh -c \
            'pkill -f "tcpdump.*fr6-(pre|postnew)-master" >/dev/null 2>&1; rm -f /tmp/fr6-pre-master-*.pcap /tmp/fr6-postnew-master-*.pcap' \
            >/dev/null 2>&1 || true
    done
    if [[ -n "${SRC_INITIATOR_CTR:-}" ]]; then
        # Connectors don't have predictable argv text — match against the
        # actual socat argv pattern (TCP4:<target>:<port>) which IS in the
        # process command line, plus the echo-loop sleep parents that pipe
        # into socat. The `fr6-conn-${port}` string is in payload only and
        # never reached pkill -f.
        docker exec "${SRC_INITIATOR_CTR}" sh -c "
            pkill -f 'socat .* TCP4:${TARGET_OVERLAY}:${LISTEN_PORT}' >/dev/null 2>&1 || true
            pkill -f 'while \\[ .* -lt 600 \\]' >/dev/null 2>&1 || true
            rm -f /tmp/fr6-conn-*.log /tmp/fr6-conn-*.pid
        " >/dev/null 2>&1 || true
    fi
    if [[ -n "${DST_TARGET_CTR:-}" ]]; then
        # Listener writes its PID to /tmp/fr6-listen.pid at start — use it
        # directly. pkill argv match also works here since LISTEN-port is in
        # socat's argv unlike the fr6-listen string (log redirect only).
        docker exec "${DST_TARGET_CTR}" sh -c "
            if [ -f /tmp/fr6-listen.pid ]; then
                kill \$(cat /tmp/fr6-listen.pid) 2>/dev/null || true
                rm -f /tmp/fr6-listen.pid
            fi
            pkill -f 'socat TCP4-LISTEN:${LISTEN_PORT}' >/dev/null 2>&1 || true
            rm -f /tmp/fr6-listen.log
        " >/dev/null 2>&1 || true
    fi
    # CRITICAL: unpause if still paused. Re-entrancy depends on this — a
    # stale paused container blocks subsequent runs. Defensive state check
    # also catches abort between docker pause and PAUSED=1.
    if [[ "${PAUSED}" == "1" ]]; then
        printf '[FR-6] cleanup: docker unpause %s\n' "${MASTER_PAUSE_CTR}"
        docker unpause "${MASTER_PAUSE_CTR}" >/dev/null 2>&1 || true
    else
        local state
        state=$(docker inspect -f '{{.State.Status}}' "${MASTER_PAUSE_CTR}" 2>/dev/null || true)
        if [[ "${state}" == "paused" ]]; then
            printf '[FR-6] cleanup: detected stale paused state, unpausing %s\n' "${MASTER_PAUSE_CTR}"
            docker unpause "${MASTER_PAUSE_CTR}" >/dev/null 2>&1 || true
        fi
    fi
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Helpers (inline, T-001 lib extract deferred). Copied verbatim from
# fr3-conntrack-sticky.sh with fr6:: namespace and FR-6-specific tcpdump
# pcap-name globs. Conntrack gate intentionally omitted — FR-6 uses tcpdump
# only (no L4 stateful conntrack -L parse), so NFR-5 SKIP gate doesn't apply.
# ---------------------------------------------------------------------------
fr6::require_running() {
    local ctr="$1"
    if ! docker inspect -f '{{.State.Running}}' "${ctr}" 2>/dev/null \
            | grep -q true; then
        printf '[FR-6] FAIL: container %s not running.\n' "${ctr}" >&2
        printf '       Bring up topology first (issue-92-rotation.sh NO_CLEANUP=1\n' >&2
        printf '       or data-plane-extended.sh).\n' >&2
        return 1
    fi
}

# Verify pause target is 'running' (not stale 'paused' from prior aborted run).
# .State.Running is true even for paused containers; must check .State.Status.
fr6::require_not_paused() {
    local ctr="$1"
    local state
    state=$(docker inspect -f '{{.State.Status}}' "${ctr}" 2>/dev/null || true)
    if [[ "${state}" == "paused" ]]; then
        printf '[FR-6] FAIL: container %s is in stale paused state.\n' "${ctr}" >&2
        printf '       Run: docker unpause %s\n' "${ctr}" >&2
        return 1
    fi
    if [[ "${state}" != "running" ]]; then
        printf '[FR-6] FAIL: container %s state=%s (expected running).\n' \
            "${ctr}" "${state}" >&2
        return 1
    fi
}

fr6::ensure_binary() {
    local ctr="$1"
    local bin="$2"
    if docker exec "${ctr}" sh -c "command -v ${bin}" >/dev/null 2>&1; then
        return 0
    fi
    printf '  [info] installing %s in %s...\n' "${bin}" "${ctr}"
    if docker exec "${ctr}" sh -c 'command -v apk' >/dev/null 2>&1; then
        if docker exec "${ctr}" apk add --no-cache "${bin}" >/dev/null 2>&1; then
            return 0
        fi
    fi
    if docker exec "${ctr}" sh -c 'command -v apt-get' >/dev/null 2>&1; then
        if docker exec "${ctr}" sh -c \
                "apt-get update >/dev/null && apt-get install -y ${bin} >/dev/null"; then
            return 0
        fi
    fi
    printf '[FR-6] FAIL: cannot install %s in %s (no apk/apt-get).\n' "${bin}" "${ctr}" >&2
    return 1
}

fr6::preflight_ping() {
    docker exec "${SRC_INITIATOR_CTR}" \
        ping -c 1 -W 5 "${TARGET_OVERLAY}" >/dev/null 2>&1 || true
    docker exec "${SRC_INITIATOR_CTR}" \
        ping -c 1 -W 5 "${TARGET_OVERLAY}" >/dev/null 2>&1 || true
    if ! docker exec "${SRC_INITIATOR_CTR}" \
            ping -c 2 -W 2 "${TARGET_OVERLAY}" >/dev/null 2>&1; then
        printf '[FR-6] FAIL: pre-flight ping %s -> %s failed.\n' \
            "${SRC_INITIATOR_CTR}" "${TARGET_OVERLAY}" >&2
        printf '       Topology not data-plane-ready; nothing to measure.\n' >&2
        return 1
    fi
}

# tcpdump on master scoped to TCP SRC->DST within src-port range. Distinct
# pcap per phase. nohup-bg pattern (fr1: docker exec -d races tcpdump bind).
fr6::start_tcpdump() {
    local ctr="$1"
    local pcap="$2"
    local port_lo="$3"
    local port_hi="$4"
    docker exec "${ctr}" sh -c "rm -f ${pcap}" >/dev/null 2>&1 || true
    docker exec "${ctr}" sh -c \
        "nohup tcpdump -i '${SRC_INGRESS_IFACE}' -nn -U -p \
            -w '${pcap}' \
            'tcp and src host ${SRC_INITIATOR_OVERLAY} and dst host ${TARGET_OVERLAY} and dst port ${LISTEN_PORT} and src portrange ${port_lo}-${port_hi}' \
            >/dev/null 2>&1 &" \
        >/dev/null 2>&1
}

fr6::stop_tcpdump() {
    local ctr="$1"
    docker exec "${ctr}" sh -c \
        'pkill -INT -f "tcpdump.*fr6-(pre|postnew)-master" >/dev/null 2>&1 || true' \
        >/dev/null 2>&1 || true
    sleep 1
}

# Long-lived TCP listener inside DST_TARGET_CTR. Each accepted connection
# spawns a sleep loop so the socket stays open until trap kills it.
fr6::start_listener() {
    docker exec "${DST_TARGET_CTR}" sh -c \
        "nohup socat TCP4-LISTEN:${LISTEN_PORT},reuseaddr,fork EXEC:'sleep 600',pty >/tmp/fr6-listen.log 2>&1 & echo \$! > /tmp/fr6-listen.pid" \
        >/dev/null 2>&1 || true
    sleep 1
}

# Open N TCP connections SRC->DST, each with a unique src port in
# [port_lo, port_lo+count). CRITICAL: continuous-data emission (1 byte / 500ms)
# — idle sockets are invisible to per-master tcpdump after handshake (fr3
# documented zero-packet symptom). Tagged "fr6-conn-${port}" for cleanup.
fr6::open_connections() {
    local count="$1"
    local port_lo="$2"
    local i port
    for (( i=0; i<count; i++ )); do
        port=$(( port_lo + i ))
        docker exec -d "${SRC_INITIATOR_CTR}" sh -c \
            "(i=0; while [ \$i -lt 600 ]; do echo fr6-conn-${port}; sleep 0.5; i=\$((i+1)); done) | socat -t 600 - TCP4:${TARGET_OVERLAY}:${LISTEN_PORT},sourceport=${port},reuseaddr,connect-timeout=5 >/dev/null 2>&1 &" \
            >/dev/null 2>&1 || true
    done
}

# Read pcap, emit sorted unique src ports within range, one per line.
fr6::pcap_src_ports() {
    local ctr="$1"
    local pcap="$2"
    local port_lo="$3"
    local port_hi="$4"
    docker exec "${ctr}" sh -c \
        "tcpdump -nn -r '${pcap}' 2>/dev/null \
            | sed -nE 's/.* IP ${SRC_INITIATOR_OVERLAY//./\\.}\\.([0-9]+) > ${TARGET_OVERLAY//./\\.}\\.${LISTEN_PORT}:.*/\\1/p' \
            | awk -v lo=${port_lo} -v hi=${port_hi} '\$1 >= lo && \$1 <= hi' \
            | sort -nu" \
        2>/dev/null || true
}

# ---------------------------------------------------------------------------
# Assertion A1 (FR-6 PASS condition): ALL post-pause new connections must
# appear on other-master, ZERO on paused-master. Returns 0 PASS / 1 FAIL.
# ---------------------------------------------------------------------------
fr6::assert_new_flows_migrated() {
    local on_paused="$1"
    local on_other="$2"
    local expected_count="$3"

    local paused_count other_count
    paused_count=$(printf '%s' "${on_paused}" | grep -c . || true)
    other_count=$(printf '%s' "${on_other}" | grep -c . || true)

    printf '  [A1] post-pause NEW flows: paused-master=%d ports  other-master=%d ports (expected on other: %d)\n' \
        "${paused_count}" "${other_count}" "${expected_count}"

    if (( paused_count > 0 )); then
        printf '[FR-6] FAIL (A1): %d NEW connection(s) still reached paused master after %ds healthcheck window.\n' \
            "${paused_count}" "${MIGRATION_TIMEOUT_S}" >&2
        printf '       Paused-master src ports observed: %s\n' \
            "$(printf '%s' "${on_paused}" | tr '\n' ',' | sed 's/,$//')" >&2
        printf '       Healthcheck regression — nexthop set still includes unhealthy master.\n' >&2
        return 1
    fi

    if (( other_count < expected_count )); then
        if (( other_count == 0 )); then
            printf '[FR-6] FAIL (A1): zero NEW connections observed on either master.\n' >&2
            printf '       Listener/connector setup failed, or pcap filter missed range %d-%d.\n' \
                "${POST_PORT_RANGE_START}" "${POST_PORT_RANGE_END}" >&2
        else
            printf '[FR-6] FAIL (A1): only %d/%d NEW connections migrated to other-master after %ds.\n' \
                "${other_count}" "${expected_count}" "${MIGRATION_TIMEOUT_S}" >&2
            printf '       Partial healthcheck regression — some new flows did not migrate cleanly.\n' >&2
        fi
        return 1
    fi

    printf '[FR-6] PASS (A1): all %d NEW post-pause connections routed to other-master within %ds healthcheck flip.\n' \
        "${other_count}" "${MIGRATION_TIMEOUT_S}"
    return 0
}

# ---------------------------------------------------------------------------
# Assertion A2 (CR-002 advisory observation — NOT strict gate). Pre-pause
# src ports observed on other-master post-T0 = existing-flow migration
# (unexpected per contract). Tolerance ±2 for in-flight packets at pause
# moment. Returns 0 always (advisory).
# ---------------------------------------------------------------------------
fr6::observe_existing_flows() {
    local pkt_count="$1"
    printf '  [A2] post-T0 pre-pause src ports observed on other-master: %d\n' \
        "${pkt_count}"
    if (( pkt_count <= EXISTING_FLOW_TOLERANCE )); then
        printf '[FR-6] OBSERVE (A2): %d packets ≤ ±%d tolerance — existing-flow drop per CR-002 contract.\n' \
            "${pkt_count}" "${EXISTING_FLOW_TOLERANCE}"
    else
        # Above tolerance: kernel netfilter on paused container may still
        # serve forwarded packets while userspace daemon is frozen. Document
        # as advisory finding, not a gating failure.
        printf '[FR-6] OBSERVE (A2): %d packets > ±%d tolerance — diverges from CR-002 drop contract (advisory only).\n' \
            "${pkt_count}" "${EXISTING_FLOW_TOLERANCE}"
    fi
    return 0
}

# ---------------------------------------------------------------------------
# Assertion A3 (US6 AC-3): after unpause, single TCP probe from SRC must
# complete (any master may forward). Returns 0 PASS / 1 FAIL.
# ---------------------------------------------------------------------------
fr6::assert_recovery() {
    local recovery_port=49999  # outside both pre and post ranges to avoid pcap noise
    if docker exec "${SRC_INITIATOR_CTR}" sh -c \
            "echo fr6-recovery | socat -t 5 - TCP4:${TARGET_OVERLAY}:${LISTEN_PORT},sourceport=${recovery_port},reuseaddr,connect-timeout=5 >/dev/null 2>&1"; then
        printf '[FR-6] PASS (A3): recovery probe succeeded after unpause.\n'
        return 0
    fi
    printf '[FR-6] FAIL (A3): recovery probe failed after unpause + %ds settle.\n' \
        "${RECOVERY_SETTLE_S}" >&2
    printf '       Topology did not recover — master-01 unpause may not have restored nexthop.\n' >&2
    return 1
}

# ---------------------------------------------------------------------------
# Self-test mode: inject synthetic post-pause distribution where new
# connections still hit the paused master, and verify A1 correctly returns
# FAIL. Anti-stub regression guard.
# ---------------------------------------------------------------------------
if [[ "${FR6_SELF_TEST}" == "no-migration" ]]; then
    printf '[FR-6] SELF-TEST mode: no-migration\n'
    printf '       Injecting synthetic state where 20 NEW connections still hit paused master.\n'

    fr6_self_on_paused=$(seq "${POST_PORT_RANGE_START}" "${POST_PORT_RANGE_END}")
    fr6_self_on_other=""

    if fr6::assert_new_flows_migrated \
            "${fr6_self_on_paused}" \
            "${fr6_self_on_other}" \
            "${POST_PAUSE_CONNECTIONS}"; then
        printf '[FR-6] SELF-TEST FAIL: A1 passed on synthetic regression input.\n' >&2
        printf '       The module fails to detect new-flow migration regressions.\n' >&2
        exit 1
    fi
    printf '[FR-6] SELF-TEST PASS: A1 correctly rejected synthetic no-migration scenario.\n'
    exit 0
fi

# ---------------------------------------------------------------------------
# Run.
# ---------------------------------------------------------------------------
printf '[FR-6] Sticky session migration on healthcheck flip\n'
printf '       initiator=%s (%s) target=%s (%s)\n' \
    "${SRC_INITIATOR_CTR}" "${SRC_INITIATOR_OVERLAY}" \
    "${DST_TARGET_CTR}" "${TARGET_OVERLAY}"
printf '       pause_target=%s  other=%s   ingress_iface=%s\n' \
    "${MASTER_PAUSE_CTR}" "${MASTER_OTHER_CTR}" "${SRC_INGRESS_IFACE}"
printf '       pre_connections=%d (ports %d-%d)  post_connections=%d (ports %d-%d)  listener_port=%d\n' \
    "${PRE_PAUSE_CONNECTIONS}" "${PRE_PORT_RANGE_START}" "${PRE_PORT_RANGE_END}" \
    "${POST_PAUSE_CONNECTIONS}" "${POST_PORT_RANGE_START}" "${POST_PORT_RANGE_END}" \
    "${LISTEN_PORT}"
printf '       migration_timeout=%ds  recovery_settle=%ds  existing_flow_tolerance=±%d packets\n' \
    "${MIGRATION_TIMEOUT_S}" "${RECOVERY_SETTLE_S}" "${EXISTING_FLOW_TOLERANCE}"

fr6::require_running "${MASTER_PAUSE_CTR}" || exit 2
fr6::require_running "${MASTER_OTHER_CTR}" || exit 2
fr6::require_running "${SRC_INITIATOR_CTR}" || exit 2
fr6::require_running "${DST_TARGET_CTR}" || exit 2
fr6::require_not_paused "${MASTER_PAUSE_CTR}" || exit 2

fr6::ensure_binary "${MASTER_PAUSE_CTR}" tcpdump || exit 2
fr6::ensure_binary "${MASTER_OTHER_CTR}" tcpdump || exit 2
fr6::ensure_binary "${SRC_INITIATOR_CTR}" socat || exit 2
fr6::ensure_binary "${DST_TARGET_CTR}" socat || exit 2

fr6::preflight_ping || exit 2

# --- Phase 1: listener + open pre-pause connections ---
printf '  [phase] starting listener on %s:%d\n' "${DST_TARGET_CTR}" "${LISTEN_PORT}"
fr6::start_listener

printf '  [phase] opening %d PRE-pause TCP connections (src ports %d..%d)\n' \
    "${PRE_PAUSE_CONNECTIONS}" "${PRE_PORT_RANGE_START}" "${PRE_PORT_RANGE_END}"
fr6::open_connections "${PRE_PAUSE_CONNECTIONS}" "${PRE_PORT_RANGE_START}"

# Warm-up: WireGuard handshake settle.
sleep "${WARMUP_S}"

# --- Phase 2: pre-pause distribution capture ---
printf '  [phase] arming pre-pause tcpdump on both masters (port range %d-%d)\n' \
    "${PRE_PORT_RANGE_START}" "${PRE_PORT_RANGE_END}"
fr6::start_tcpdump "${MASTER_PAUSE_CTR}" "${M01_PRE_PCAP}" \
    "${PRE_PORT_RANGE_START}" "${PRE_PORT_RANGE_END}"
fr6::start_tcpdump "${MASTER_OTHER_CTR}" "${M02_PRE_PCAP}" \
    "${PRE_PORT_RANGE_START}" "${PRE_PORT_RANGE_END}"
sleep "${TCPDUMP_STARTUP_S}"
sleep "${PRE_CAPTURE_S}"

fr6::stop_tcpdump "${MASTER_PAUSE_CTR}"
fr6::stop_tcpdump "${MASTER_OTHER_CTR}"

PRE_M01_PORTS=$(fr6::pcap_src_ports "${MASTER_PAUSE_CTR}" "${M01_PRE_PCAP}" \
    "${PRE_PORT_RANGE_START}" "${PRE_PORT_RANGE_END}")
PRE_M02_PORTS=$(fr6::pcap_src_ports "${MASTER_OTHER_CTR}" "${M02_PRE_PCAP}" \
    "${PRE_PORT_RANGE_START}" "${PRE_PORT_RANGE_END}")
PRE_M01_COUNT=$(printf '%s' "${PRE_M01_PORTS}" | grep -c . || true)
PRE_M02_COUNT=$(printf '%s' "${PRE_M02_PORTS}" | grep -c . || true)
printf '  [info] pre-pause distribution: paused-master=%d ports  other-master=%d ports\n' \
    "${PRE_M01_COUNT}" "${PRE_M02_COUNT}"

PRE_TOTAL=$(( PRE_M01_COUNT + PRE_M02_COUNT ))
if (( PRE_TOTAL == 0 )); then
    printf '[FR-6] FAIL: zero TCP flows observed pre-pause on either master.\n' >&2
    printf '       Listener/connector setup failed, or pcap filter missed range %d-%d.\n' \
        "${PRE_PORT_RANGE_START}" "${PRE_PORT_RANGE_END}" >&2
    exit 2
fi

# --- Phase 3: pause master + arm post-T0 captures + wait for healthcheck flip ---
T0=$(date +%s.%N)
printf '  [phase] T0=%s docker pause %s\n' "${T0}" "${MASTER_PAUSE_CTR}"
docker pause "${MASTER_PAUSE_CTR}" >/dev/null
PAUSED=1

# tcpdump only on MASTER_OTHER post-T0: pause freezes ALL processes inside
# the paused container including any tcpdump there, so we observe migration
# from the OTHER side. Pre-pause ports seen on other-master = A2 leakage
# (unexpected per CR-002); post-pause ports seen there = A1 PASS evidence.
printf '  [phase] arming post-pause tcpdump on other-master for combined port range %d-%d\n' \
    "${PRE_PORT_RANGE_START}" "${POST_PORT_RANGE_END}"
fr6::start_tcpdump "${MASTER_OTHER_CTR}" "${M02_POSTNEW_PCAP}" \
    "${PRE_PORT_RANGE_START}" "${POST_PORT_RANGE_END}"
sleep "${TCPDUMP_STARTUP_S}"

printf '  [phase] waiting %ds for healthcheck flip propagation...\n' "${MIGRATION_TIMEOUT_S}"
sleep "${MIGRATION_TIMEOUT_S}"

# --- Phase 4: open post-pause NEW connections ---
printf '  [phase] opening %d POST-pause NEW TCP connections (src ports %d..%d)\n' \
    "${POST_PAUSE_CONNECTIONS}" "${POST_PORT_RANGE_START}" "${POST_PORT_RANGE_END}"
fr6::open_connections "${POST_PAUSE_CONNECTIONS}" "${POST_PORT_RANGE_START}"

sleep "${POSTNEW_CAPTURE_S}"

fr6::stop_tcpdump "${MASTER_OTHER_CTR}"

# Pull pcap data from MASTER_OTHER for both port ranges.
NEW_ON_OTHER=$(fr6::pcap_src_ports "${MASTER_OTHER_CTR}" "${M02_POSTNEW_PCAP}" \
    "${POST_PORT_RANGE_START}" "${POST_PORT_RANGE_END}")
PREEXISTING_ON_OTHER=$(fr6::pcap_src_ports "${MASTER_OTHER_CTR}" "${M02_POSTNEW_PCAP}" \
    "${PRE_PORT_RANGE_START}" "${PRE_PORT_RANGE_END}")

# A1: paused-master observation always empty — frozen container can't tcpdump.
# Indirect proof via NEW_ON_OTHER coverage of the post-pause range.
NEW_ON_PAUSED=""

# A2: pre-pause ports on OTHER master post-T0 = existing-flow migration
# (unexpected per CR-002 contract, advisory).
PREEXIST_LEAK_COUNT=$(printf '%s' "${PREEXISTING_ON_OTHER}" | grep -c . || true)

# --- Phase 5: A1 + A2 assertions ---
A1_RESULT=0
fr6::assert_new_flows_migrated "${NEW_ON_PAUSED}" "${NEW_ON_OTHER}" "${POST_PAUSE_CONNECTIONS}" || A1_RESULT=1

# A2 advisory: count pre-pause ports that migrated to other master.
# Per CR-002 contract this should be ≤ tolerance (drop expected).
fr6::observe_existing_flows "${PREEXIST_LEAK_COUNT}"

# --- Phase 6: unpause + A3 recovery probe ---
printf '  [phase] docker unpause %s\n' "${MASTER_PAUSE_CTR}"
docker unpause "${MASTER_PAUSE_CTR}" >/dev/null
PAUSED=0
sleep "${RECOVERY_SETTLE_S}"

A3_RESULT=0
fr6::assert_recovery || A3_RESULT=1

# --- Phase 7: structured summary + final verdict ---
NEW_ON_OTHER_COUNT=$(printf '%s' "${NEW_ON_OTHER}" | grep -c . || true)
A1_LABEL=$( [[ "${A1_RESULT}" == "0" ]] && printf 'PASS' || printf 'FAIL')
A3_LABEL=$( [[ "${A3_RESULT}" == "0" ]] && printf 'PASS' || printf 'FAIL')
A2_LABEL=$( (( PREEXIST_LEAK_COUNT <= EXISTING_FLOW_TOLERANCE )) && printf 'CONTRACT-ALIGNED' || printf 'CONTRACT-DIVERGENT')

printf '\n[FR-6] Summary\n'
printf '       pre-pause distribution:    paused-master=%d  other-master=%d (total=%d)\n' \
    "${PRE_M01_COUNT}" "${PRE_M02_COUNT}" "${PRE_TOTAL}"
printf '       post-pause NEW flows:      on other-master=%d ports (expected: %d)\n' \
    "${NEW_ON_OTHER_COUNT}" "${POST_PAUSE_CONNECTIONS}"
printf '       existing-flow leakage:     %d pre-pause ports on other-master post-T0 (tolerance ±%d)\n' \
    "${PREEXIST_LEAK_COUNT}" "${EXISTING_FLOW_TOLERANCE}"
printf '       A1 (new-flow migration):   %s\n' "${A1_LABEL}"
printf '       A2 (existing-flow drop):   advisory %s\n' "${A2_LABEL}"
printf '       A3 (recovery probe):       %s\n' "${A3_LABEL}"

if [[ "${A1_RESULT}" == "0" && "${A3_RESULT}" == "0" ]]; then
    printf '[FR-6] PASS: healthcheck flip + recovery verified.\n'
    exit 0
fi
exit 1
