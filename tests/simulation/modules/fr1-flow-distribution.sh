#!/usr/bin/env bash
# F-004 FR-1: ECMP flow distribution assertion module.
#
# Generates 200 distinct-5-tuple UDP flows endpoint-asia-01 -> endpoint-asia-02
# overlay IP, captures per-master traffic via tcpdump on each master's
# wg-* iface, and asserts the flow distribution lands in [40%, 60%] per
# master (NFR-2). Catches degenerate ECMP regressions where the kernel
# multipath hash devolves to a single nexthop (e.g. broken sysctl,
# missing fib_multipath_hash_policy=1, or routing-table misbuild).
#
# Method (ADR-003): tcpdump per-master pcap + post-run packet count.
# Not eBPF — eBPF is F-005 candidate, see ADR-003.
#
# 5-tuple variation:
#   - src_ip      = endpoint-asia-01 overlay (constant)
#   - dst_ip      = endpoint-asia-02 overlay (constant)
#   - protocol    = UDP (constant)
#   - dst_port    = UDP_DEST_PORT (constant)
#   - src_port    = 30000 + flow_index (varies across 200 flows)
# kernel ECMP with fib_multipath_hash_policy=1 (pkg/routing/sysctl.go)
# hashes by full 5-tuple, so varying src_port yields distinct hash inputs.
#
# Topology assumption:
#   The module is invoked by tests/simulation/data-plane-extended.sh
#   (T-009, future). For standalone runs, operator first spins up the
#   issue-92 sim topology with NO_CLEANUP=1, then runs this module.
#   Defaults match the issue-92 fixture; override via env if needed.
#
# Self-test mode (FR1_SELF_TEST=single-master):
#   Runs the same flow generator but counts packets only on master-01
#   (master-02 count forced to 0). The assertion logic must FAIL with
#   a degenerate-distribution message — this validates the module's
#   own pass/fail logic against an anti-stub regression.
#
# Re-entrancy: cleanup trap kills tcpdump on each master and removes
# pcap files; module can be re-run without manual recovery. The trap
# never tears down the topology (orchestrator owns that lifecycle).
#
# Linux netns + CAP_NET_ADMIN (NFR-5): non-Linux exit 0 + skip message.
#
# Usage:
#   bash tests/simulation/modules/fr1-flow-distribution.sh [--help]
#
# Env overrides:
#   MASTER_01_CTR         master-01 container name (default issue92rot-mst-ru-01)
#   MASTER_02_CTR         master-02 container name (default issue92rot-mst-ru-02)
#   SRC_ENDPOINT_CTR      flow-source endpoint container (default issue92rot-node-asia-01)
#   SRC_INGRESS_IFACE     master-side iface that receives traffic from SRC_ENDPOINT
#                         (default wg-node-asia-01 — must exist on BOTH masters)
#   DST_OVERLAY_IP        flow-destination overlay IP (default 172.21.92.36)
#   FLOW_COUNT            number of distinct UDP flows (default 200)
#   UDP_DEST_PORT         destination UDP port for all flows (default 9999)
#   SRC_PORT_BASE         starting source port (default 30000)
#   BALANCE_LOW           lower bound of allowed per-master share (default 0.40)
#   BALANCE_HIGH          upper bound of allowed per-master share (default 0.60)
#   FR1_SELF_TEST         set to "single-master" to force the assertion to FAIL
#                         (anti-stub regression check; see header)
#
# Exit:
#   0  PASS or skip
#   1  assertion failed (any master > BALANCE_HIGH or below BALANCE_LOW)
#      OR self-test mode succeeded in catching the synthetic regression
#   2  environment error (topology not running, tools install failed,
#                         pre-flight ping failed, no traffic captured)
set -euo pipefail

# ---------------------------------------------------------------------------
# Platform guard (NFR-5).
# ---------------------------------------------------------------------------
if [[ "$(uname -s)" != "Linux" ]]; then
    printf '[FR-1] SKIP: requires Linux (CAP_NET_ADMIN + netns).\n'
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
            printf '  Generate FLOW_COUNT distinct-5-tuple UDP flows endpoint -> endpoint,\n'
            printf '  capture per-master via tcpdump, assert distribution in [BALANCE_LOW, BALANCE_HIGH].\n'
            printf '  Default: 200 flows, [0.40, 0.60] balance window (NFR-2).\n'
            printf '\n'
            printf '  Env overrides: MASTER_01_CTR MASTER_02_CTR SRC_ENDPOINT_CTR\n'
            printf '                 DST_OVERLAY_IP FLOW_COUNT UDP_DEST_PORT SRC_PORT_BASE\n'
            printf '                 BALANCE_LOW BALANCE_HIGH FR1_SELF_TEST\n'
            printf '\n'
            printf '  FR1_SELF_TEST=single-master forces the assertion to FAIL by counting\n'
            printf '  only master-01 traffic (anti-stub regression check). Module exits 0\n'
            printf '  when the synthetic regression is caught, exits 1 if it slips through.\n'
            exit 0
            ;;
        *)
            printf '[FR-1] unknown arg: %s (try --help)\n' "${arg}" >&2
            exit 2
            ;;
    esac
done

# ---------------------------------------------------------------------------
# Test fixture parameters. Defaults align with tests/simulation/issue-92-rotation.sh.
# Override via env when running standalone outside the F-004 orchestrator.
# ---------------------------------------------------------------------------
MASTER_01_CTR="${MASTER_01_CTR:-issue92rot-mst-ru-01}"
MASTER_02_CTR="${MASTER_02_CTR:-issue92rot-mst-ru-02}"
SRC_ENDPOINT_CTR="${SRC_ENDPOINT_CTR:-issue92rot-node-asia-01}"
# Master-side iface receiving traffic from SRC_ENDPOINT. Master mode names
# its per-endpoint wg ifaces "wg-<endpoint-name>" (pkg/node/master.go); both
# masters carry the same iface name for the same source endpoint, so we can
# use one constant. tcpdump in this alpine build does NOT honour wildcard
# patterns like "wg-+" (devolves to "no such device") — capture must target
# a specific iface. Using the source endpoint's ingress iface counts the
# packet exactly once on whichever master the source-endpoint kernel chose.
SRC_INGRESS_IFACE="${SRC_INGRESS_IFACE:-wg-node-asia-01}"
DST_OVERLAY_IP="${DST_OVERLAY_IP:-172.21.92.36}"
FLOW_COUNT="${FLOW_COUNT:-200}"
UDP_DEST_PORT="${UDP_DEST_PORT:-9999}"
SRC_PORT_BASE="${SRC_PORT_BASE:-30000}"
BALANCE_LOW="${BALANCE_LOW:-0.40}"
BALANCE_HIGH="${BALANCE_HIGH:-0.60}"
FR1_SELF_TEST="${FR1_SELF_TEST:-}"

# Pcap files inside each master container.
MASTER_01_PCAP="/tmp/fr1-master-01.pcap"
MASTER_02_PCAP="/tmp/fr1-master-02.pcap"

# Settle delays.
TCPDUMP_STARTUP_S=1   # let tcpdump arm before flows start
FLOW_DRAIN_S=5        # wait after flow generation for tcpdump buffers to flush

# ---------------------------------------------------------------------------
# Cleanup trap. Always kills tcpdump on each master and removes pcap files.
# Re-entrant: a stale tcpdump from a prior run will be reaped. Never tears
# down topology (orchestrator owns that lifecycle).
# ---------------------------------------------------------------------------
# shellcheck disable=SC2329  # invoked via trap below
cleanup() {
    for ctr in "${MASTER_01_CTR}" "${MASTER_02_CTR}"; do
        docker exec "${ctr}" sh -c \
            'pkill -f "tcpdump.*fr1-master" >/dev/null 2>&1; rm -f /tmp/fr1-master-*.pcap' \
            >/dev/null 2>&1 || true
    done
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Helpers.
# ---------------------------------------------------------------------------
fr1::require_running() {
    local ctr="$1"
    if ! docker inspect -f '{{.State.Running}}' "${ctr}" 2>/dev/null \
            | grep -q true; then
        printf '[FR-1] FAIL: container %s not running.\n' "${ctr}" >&2
        printf '       Bring up topology first (issue-92-rotation.sh NO_CLEANUP=1\n' >&2
        printf '       or data-plane-extended.sh).\n' >&2
        return 1
    fi
}

fr1::ensure_binary() {
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
    printf '[FR-1] FAIL: cannot install %s in %s (no apk/apt-get).\n' "${bin}" "${ctr}" >&2
    return 1
}

# Pre-warm the overlay path. Same handshake-warmup pattern as fr4 / R7.2.
fr1::preflight_ping() {
    docker exec "${SRC_ENDPOINT_CTR}" \
        ping -c 1 -W 5 "${DST_OVERLAY_IP}" >/dev/null 2>&1 || true
    docker exec "${SRC_ENDPOINT_CTR}" \
        ping -c 1 -W 5 "${DST_OVERLAY_IP}" >/dev/null 2>&1 || true
    if ! docker exec "${SRC_ENDPOINT_CTR}" \
            ping -c 2 -W 2 "${DST_OVERLAY_IP}" >/dev/null 2>&1; then
        printf '[FR-1] FAIL: pre-flight ping %s -> %s failed.\n' \
            "${SRC_ENDPOINT_CTR}" "${DST_OVERLAY_IP}" >&2
        printf '       Topology not data-plane-ready; nothing to measure.\n' >&2
        return 1
    fi
}

# Start tcpdump in the background inside a master container, capturing only
# UDP packets to the destination overlay IP on UDP_DEST_PORT, on the master
# iface that receives traffic from the source endpoint (SRC_INGRESS_IFACE).
# This counts each forwarded flow exactly once — on whichever master the
# source-endpoint kernel selected. Using -U for unbuffered writes, -w to a
# pcap file inside the container, and `nohup ... &` because `docker exec -d`
# can race the tcpdump bind on slower hosts.
fr1::start_tcpdump() {
    local ctr="$1"
    local pcap="$2"
    docker exec "${ctr}" sh -c "rm -f ${pcap}" >/dev/null 2>&1 || true
    docker exec "${ctr}" sh -c \
        "nohup tcpdump -i '${SRC_INGRESS_IFACE}' -nn -U -p \
            -w '${pcap}' \
            'udp and dst host ${DST_OVERLAY_IP} and dst port ${UDP_DEST_PORT}' \
            >/dev/null 2>&1 &" \
        >/dev/null 2>&1
}

# Generate FLOW_COUNT distinct-5-tuple UDP flows from the source endpoint.
# Each flow varies src_port across [SRC_PORT_BASE, SRC_PORT_BASE + FLOW_COUNT).
# socat binds an explicit source port via sourceport=N; one packet per flow
# is enough — the kernel's ECMP hash decision is per-flow, not per-packet.
# Output is silenced; transient errors (e.g. address-in-use if the kernel
# hasn't released a recent ephemeral) are tolerated by the loop and reflected
# in the captured packet count downstream.
fr1::generate_flows() {
    local count="$1"
    local base="$2"
    local script
    script=$(cat <<EOF
i=0
while [ \$i -lt ${count} ]; do
    src_port=\$((${base} + i))
    printf 'fr1-flow-%d' "\$i" \\
        | socat -T 1 - "UDP4-SENDTO:${DST_OVERLAY_IP}:${UDP_DEST_PORT},sourceport=\$src_port,reuseaddr" \\
        >/dev/null 2>&1 || true
    i=\$((i + 1))
done
EOF
)
    docker exec "${SRC_ENDPOINT_CTR}" sh -c "${script}"
}

# Stop tcpdump cleanly so pcap is flushed before parsing.
fr1::stop_tcpdump() {
    local ctr="$1"
    docker exec "${ctr}" sh -c \
        'pkill -INT -f "tcpdump.*fr1-master" >/dev/null 2>&1 || true' \
        >/dev/null 2>&1 || true
    # Brief settle so the SIGINT-driven flush completes before we read.
    sleep 1
}

# Count UDP packets in a master's pcap matching the expected dst port. Reads
# tcpdump output instead of pcap-parsing libraries — alpine ships no jq-pcap.
fr1::count_packets() {
    local ctr="$1"
    local pcap="$2"
    local out
    out=$(docker exec "${ctr}" \
        tcpdump -nn -r "${pcap}" "udp and dst port ${UDP_DEST_PORT}" 2>/dev/null \
        | wc -l)
    # wc -l yields a leading-whitespace integer in some busybox builds; trim.
    printf '%s' "${out}" | tr -d '[:space:]'
}

# Assert balance bounds. Inputs: m01_count m02_count low high. Returns 0
# on PASS, 1 on FAIL. Prints structured PASS/FAIL summary either way.
fr1::assert_distribution() {
    local m01="$1"
    local m02="$2"
    local low="$3"
    local high="$4"
    local total
    total=$(( m01 + m02 ))
    if (( total == 0 )); then
        printf '[FR-1] FAIL: zero packets captured on either master (total=0).\n' >&2
        printf '       Either flow generation failed or tcpdump filter missed.\n' >&2
        return 1
    fi
    # Integer math via awk: produce floats with 4 decimals.
    local m01_pct m02_pct in_window
    m01_pct=$(awk -v a="${m01}" -v t="${total}" 'BEGIN { printf "%.4f", a / t }')
    m02_pct=$(awk -v a="${m02}" -v t="${total}" 'BEGIN { printf "%.4f", a / t }')
    in_window=$(awk -v a="${m01_pct}" -v b="${m02_pct}" -v lo="${low}" -v hi="${high}" \
        'BEGIN {
            if (a < lo || a > hi) { print 0; exit }
            if (b < lo || b > hi) { print 0; exit }
            print 1
        }')
    printf '  [info] m01_count=%d m02_count=%d total=%d m01_pct=%s m02_pct=%s window=[%s, %s]\n' \
        "${m01}" "${m02}" "${total}" "${m01_pct}" "${m02_pct}" "${low}" "${high}"
    if [[ "${in_window}" == "1" ]]; then
        printf '[FR-1] PASS: distribution m01=%s m02=%s within [%s, %s] (NFR-2).\n' \
            "${m01_pct}" "${m02_pct}" "${low}" "${high}"
        return 0
    fi
    printf '[FR-1] FAIL: degenerate distribution m01=%s m02=%s outside [%s, %s] (NFR-2).\n' \
        "${m01_pct}" "${m02_pct}" "${low}" "${high}" >&2
    if (( m01 > m02 * 2 )); then
        printf '       master-01 dominant (>2x master-02). Check fib_multipath_hash_policy.\n' >&2
    elif (( m02 > m01 * 2 )); then
        printf '       master-02 dominant (>2x master-01). Check route weights / kernel hash.\n' >&2
    fi
    return 1
}

# ---------------------------------------------------------------------------
# Run.
# ---------------------------------------------------------------------------
printf '[FR-1] ECMP flow distribution assertion\n'
printf '       src=%s dst=%s flows=%d udp_port=%d\n' \
    "${SRC_ENDPOINT_CTR}" "${DST_OVERLAY_IP}" "${FLOW_COUNT}" "${UDP_DEST_PORT}"
printf '       masters: %s, %s\n' "${MASTER_01_CTR}" "${MASTER_02_CTR}"
printf '       balance window: [%s, %s] per master (NFR-2)\n' \
    "${BALANCE_LOW}" "${BALANCE_HIGH}"
if [[ -n "${FR1_SELF_TEST}" ]]; then
    printf '       SELF-TEST mode: %s\n' "${FR1_SELF_TEST}"
fi

fr1::require_running "${MASTER_01_CTR}" || exit 2
fr1::require_running "${MASTER_02_CTR}" || exit 2
fr1::require_running "${SRC_ENDPOINT_CTR}" || exit 2

# tcpdump on each master, socat on the source endpoint.
fr1::ensure_binary "${MASTER_01_CTR}" tcpdump || exit 2
fr1::ensure_binary "${MASTER_02_CTR}" tcpdump || exit 2
fr1::ensure_binary "${SRC_ENDPOINT_CTR}" socat || exit 2

fr1::preflight_ping || exit 2

# Start capture before generating flows.
fr1::start_tcpdump "${MASTER_01_CTR}" "${MASTER_01_PCAP}"
fr1::start_tcpdump "${MASTER_02_CTR}" "${MASTER_02_PCAP}"
sleep "${TCPDUMP_STARTUP_S}"

printf '  [info] generating %d UDP flows (src_port=%d..%d)...\n' \
    "${FLOW_COUNT}" "${SRC_PORT_BASE}" "$(( SRC_PORT_BASE + FLOW_COUNT - 1 ))"
fr1::generate_flows "${FLOW_COUNT}" "${SRC_PORT_BASE}"

# Drain: let in-flight packets settle into the pcap before we stop tcpdump.
sleep "${FLOW_DRAIN_S}"

fr1::stop_tcpdump "${MASTER_01_CTR}"
fr1::stop_tcpdump "${MASTER_02_CTR}"

M01_COUNT=$(fr1::count_packets "${MASTER_01_CTR}" "${MASTER_01_PCAP}") || exit 2
M02_COUNT=$(fr1::count_packets "${MASTER_02_CTR}" "${MASTER_02_PCAP}") || exit 2
if ! [[ "${M01_COUNT}" =~ ^[0-9]+$ ]]; then M01_COUNT=0; fi
if ! [[ "${M02_COUNT}" =~ ^[0-9]+$ ]]; then M02_COUNT=0; fi

# Self-test mode: force degenerate distribution by zeroing master-02 count.
# Module then expects assertion to FAIL — that's the regression-catch we want.
if [[ "${FR1_SELF_TEST}" == "single-master" ]]; then
    printf '  [self-test] zeroing master-02 count to simulate degenerate ECMP\n'
    M02_COUNT=0
    if fr1::assert_distribution "${M01_COUNT}" "${M02_COUNT}" \
            "${BALANCE_LOW}" "${BALANCE_HIGH}"; then
        printf '[FR-1] SELF-TEST FAIL: assertion passed on degenerate input.\n' >&2
        printf '       The module fails to detect single-master regressions.\n' >&2
        exit 1
    fi
    printf '[FR-1] SELF-TEST PASS: assertion correctly rejected degenerate input.\n'
    exit 0
fi

# Real assertion path.
if fr1::assert_distribution "${M01_COUNT}" "${M02_COUNT}" \
        "${BALANCE_LOW}" "${BALANCE_HIGH}"; then
    exit 0
fi
exit 1
