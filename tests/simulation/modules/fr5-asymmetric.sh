#!/usr/bin/env bash
# F-004 FR-5: Asymmetric routing detection module.
#
# Generates a single bidirectional ICMP flow src_ep -> dst_ep, captures
# forward (echo request) and reverse (echo reply) on each master via tcpdump
# on the per-endpoint wg-* ifaces, and asserts forward_master == reverse_master
# per (src,dst) tuple. Catches regressions where multipath could split
# forward/reverse without preserving stickiness.
#
# Method (ADR-003): tcpdump per-master pcap + post-run packet count.
# Not eBPF — eBPF is F-005 candidate, see ADR-003.
#
# Symmetry test, NOT distribution test:
#   FR-1 measures distribution across N flows. FR-5 measures FORWARD ==
#   REVERSE for a single flow per (src,dst) pair. PASS expected on issue-92
#   because awg-mesh pre-pins /32 routes per (src_ep, dst_ep) pair with a
#   single nexthop (no per-flow ECMP endpoint↔endpoint). The module guards
#   against future regressions where multipath could split forward/reverse
#   without preserving (src,dst) stickiness.
#
# Interface naming (pkg/node/master.go:256):
#   InterfaceName = "wg-" + endpoint-name. On each master, traffic from
#   endpoint-asia-01 enters via wg-node-asia-01 (forward); replies from
#   endpoint-asia-02 enter via wg-node-asia-02 (reverse). tcpdump in alpine
#   does NOT honour wildcards like "wg-+" — must target specific iface. Both
#   masters carry both iface names; we capture all four combinations and let
#   pcap counts reveal which master saw each direction.
#
# Topology assumption: invoked by tests/simulation/data-plane-extended.sh
# (T-009, future). For standalone runs: spin up issue-92 with NO_CLEANUP=1,
# then run this module. Defaults match the issue-92 fixture.
#
# Self-test mode (FR5_SELF_TEST=split-master): skips real capture, injects
# synthetic asymmetric result, verifies assertion correctly returns FAIL.
# Anti-stub regression guard.
#
# Re-entrant: cleanup trap kills tcpdump and removes pcaps. Never tears down
# topology (orchestrator owns that lifecycle).
#
# Linux netns + CAP_NET_ADMIN (NFR-5): non-Linux exit 0 + skip message.
#
# Usage:
#   bash tests/simulation/modules/fr5-asymmetric.sh [--help]
#
# Env overrides:
#   MASTER_01_CTR / MASTER_02_CTR             master container names
#   SRC_EP_CTR / SRC_EP_OVERLAY / SRC_EP_NAME source endpoint
#   DST_EP_CTR / DST_EP_OVERLAY / DST_EP_NAME destination endpoint
#   PING_COUNT                                ICMP echo packets (default 5)
#   FR5_SELF_TEST                             "split-master" to test the test
#
# Exit:
#   0  PASS or skip
#   1  assertion failed (forward_master != reverse_master, split path, etc.)
#      OR self-test mode succeeded in catching the synthetic regression
#   2  environment error (topology not running, tools install failed, no traffic)
set -euo pipefail

# ---------------------------------------------------------------------------
# Platform guard (NFR-5).
# ---------------------------------------------------------------------------
if [[ "$(uname -s)" != "Linux" ]]; then
    printf '[FR-5] SKIP: requires Linux (CAP_NET_ADMIN + netns).\n'
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
            printf '  Generate one bidirectional ICMP flow src_ep -> dst_ep, capture forward\n'
            printf '  (echo request) on each master via SRC_IFACE and reverse (echo reply)\n'
            printf '  via DST_IFACE. Assert forward_master == reverse_master per (src,dst)\n'
            printf '  tuple (FR-5). SRC_IFACE = "wg-" + SRC_EP_NAME, DST_IFACE = "wg-" + DST_EP_NAME.\n'
            printf '\n'
            printf '  Env overrides: MASTER_01_CTR MASTER_02_CTR\n'
            printf '                 SRC_EP_CTR SRC_EP_OVERLAY SRC_EP_NAME\n'
            printf '                 DST_EP_CTR DST_EP_OVERLAY DST_EP_NAME\n'
            printf '                 PING_COUNT FR5_SELF_TEST\n'
            printf '\n'
            printf '  FR5_SELF_TEST=split-master injects synthetic asymmetry\n'
            printf '  (forward_master=master-01, reverse_master=master-02 with non-zero counts)\n'
            printf '  and verifies the assertion correctly returns FAIL. Module exits 0\n'
            printf '  when the synthetic regression is caught, exits 1 if it slips through.\n'
            exit 0
            ;;
        *)
            printf '[FR-5] unknown arg: %s (try --help)\n' "${arg}" >&2
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
SRC_EP_CTR="${SRC_EP_CTR:-issue92rot-node-asia-01}"
SRC_EP_OVERLAY="${SRC_EP_OVERLAY:-172.21.92.35}"
SRC_EP_NAME="${SRC_EP_NAME:-node-asia-01}"
DST_EP_CTR="${DST_EP_CTR:-issue92rot-node-asia-02}"
DST_EP_OVERLAY="${DST_EP_OVERLAY:-172.21.92.36}"
DST_EP_NAME="${DST_EP_NAME:-node-asia-02}"
PING_COUNT="${PING_COUNT:-5}"
FR5_SELF_TEST="${FR5_SELF_TEST:-}"

# Master-side iface names per pkg/node/master.go:256 ("wg-" + endpoint-name).
# Both masters carry both iface names (one per bound endpoint). We capture
# forward (request) on the SRC iface and reverse (reply) on the DST iface.
SRC_IFACE="wg-${SRC_EP_NAME}"
DST_IFACE="wg-${DST_EP_NAME}"

# Pcap files inside each master container. Four pcaps total: forward + reverse
# per master. Names embed "fr5-" so the cleanup trap can pkill safely without
# touching fr1/fr2/fr4 captures that may be co-running.
MASTER_01_FWD_PCAP="/tmp/fr5-fwd-master-01.pcap"
MASTER_01_REV_PCAP="/tmp/fr5-rev-master-01.pcap"
MASTER_02_FWD_PCAP="/tmp/fr5-fwd-master-02.pcap"
MASTER_02_REV_PCAP="/tmp/fr5-rev-master-02.pcap"

# Settle delays.
TCPDUMP_STARTUP_S=1   # let tcpdump arm before flow starts
FLOW_DRAIN_S=5        # wait after ping for tcpdump buffers to flush

# Threshold for "this master saw this direction". A handful of stray packets
# can leak via cross-iface arp/handshake; require >= 2 echo packets to count
# as the path. PING_COUNT defaults to 5, so at minimum 2/5 must land.
PATH_PACKET_THRESHOLD=2

# ---------------------------------------------------------------------------
# Cleanup trap. Always kills tcpdump on each master and removes pcap files.
# Re-entrant: a stale tcpdump from a prior run will be reaped. Never tears
# down topology (orchestrator owns that lifecycle).
# ---------------------------------------------------------------------------
# shellcheck disable=SC2329  # invoked via trap below
cleanup() {
    for ctr in "${MASTER_01_CTR}" "${MASTER_02_CTR}"; do
        docker exec "${ctr}" sh -c \
            'pkill -f "tcpdump.*fr5-(fwd|rev)-master" >/dev/null 2>&1; rm -f /tmp/fr5-fwd-master-*.pcap /tmp/fr5-rev-master-*.pcap' \
            >/dev/null 2>&1 || true
    done
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Helpers.
# ---------------------------------------------------------------------------
fr5::require_running() {
    local ctr="$1"
    if ! docker inspect -f '{{.State.Running}}' "${ctr}" 2>/dev/null \
            | grep -q true; then
        printf '[FR-5] FAIL: container %s not running.\n' "${ctr}" >&2
        printf '       Bring up topology first (issue-92-rotation.sh NO_CLEANUP=1\n' >&2
        printf '       or data-plane-extended.sh).\n' >&2
        return 1
    fi
}

fr5::ensure_binary() {
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
    printf '[FR-5] FAIL: cannot install %s in %s (no apk/apt-get).\n' "${bin}" "${ctr}" >&2
    return 1
}

# Pre-warm the overlay path. Same handshake-warmup pattern as fr1 / R10:
# best-effort discard pings to settle WireGuard handshake state, then a
# real assertion ping. Without this, the first real ping can drop while
# the handshake is being negotiated, masking the actual capture results.
fr5::preflight_ping() {
    docker exec "${SRC_EP_CTR}" \
        ping -c 1 -W 5 "${DST_EP_OVERLAY}" >/dev/null 2>&1 || true
    docker exec "${SRC_EP_CTR}" \
        ping -c 1 -W 5 "${DST_EP_OVERLAY}" >/dev/null 2>&1 || true
    if ! docker exec "${SRC_EP_CTR}" \
            ping -c 2 -W 2 "${DST_EP_OVERLAY}" >/dev/null 2>&1; then
        printf '[FR-5] FAIL: pre-flight ping %s -> %s failed.\n' \
            "${SRC_EP_CTR}" "${DST_EP_OVERLAY}" >&2
        printf '       Topology not data-plane-ready; nothing to measure.\n' >&2
        return 1
    fi
}

# Start tcpdump in the background inside a master container, capturing only
# ICMP packets matching the (src,dst) pair on a specific iface. Using -U
# for unbuffered writes, -w to a pcap file inside the container, and
# `nohup ... &` because `docker exec -d` can race the tcpdump bind on slower
# hosts (fr1 documented this race; we reuse the working pattern).
fr5::start_tcpdump() {
    local ctr="$1"
    local iface="$2"
    local pcap="$3"
    local filter="$4"
    docker exec "${ctr}" sh -c "rm -f ${pcap}" >/dev/null 2>&1 || true
    docker exec "${ctr}" sh -c \
        "nohup tcpdump -i '${iface}' -nn -U -p \
            -w '${pcap}' \
            '${filter}' \
            >/dev/null 2>&1 &" \
        >/dev/null 2>&1
}

# Generate a single bidirectional ICMP flow from src_ep to dst_ep. ICMP echo
# request = forward (visible on master's wg-${SRC_EP_NAME} ingress); echo
# reply = reverse (visible on master's wg-${DST_EP_NAME} ingress). Using
# ping is simpler than socat-based UDP echo because reply is automatic and
# guaranteed bidirectional.
fr5::generate_flow() {
    docker exec "${SRC_EP_CTR}" \
        ping -c "${PING_COUNT}" -W 2 "${DST_EP_OVERLAY}" >/dev/null 2>&1 || true
}

# Stop tcpdump cleanly so pcap is flushed before parsing. SIGINT triggers
# tcpdump's normal flush path; brief sleep guarantees the flush completes.
fr5::stop_tcpdump() {
    local ctr="$1"
    docker exec "${ctr}" sh -c \
        'pkill -INT -f "tcpdump.*fr5-(fwd|rev)-master" >/dev/null 2>&1 || true' \
        >/dev/null 2>&1 || true
    sleep 1
}

# Count echo-request packets in a forward pcap. Reads pcap via tcpdump -r
# instead of pcap-parsing libraries — alpine ships no jq-pcap.
fr5::count_echo_request() {
    local ctr="$1"
    local pcap="$2"
    local out
    out=$(docker exec "${ctr}" \
        tcpdump -nn -r "${pcap}" 'icmp[icmptype]==icmp-echo' 2>/dev/null \
        | wc -l)
    printf '%s' "${out}" | tr -d '[:space:]'
}

# Count echo-reply packets in a reverse pcap.
fr5::count_echo_reply() {
    local ctr="$1"
    local pcap="$2"
    local out
    out=$(docker exec "${ctr}" \
        tcpdump -nn -r "${pcap}" 'icmp[icmptype]==icmp-echoreply' 2>/dev/null \
        | wc -l)
    printf '%s' "${out}" | tr -d '[:space:]'
}

# Determine which master saw a given direction. Inputs: m01_count m02_count
# direction-label. Returns master container name on stdout, exit 0 on success.
# Exit 1 (with FAIL message to stderr) if neither or both masters saw it —
# either case is an unrecoverable measurement error for a single-flow test.
fr5::path_master() {
    local m01="$1"
    local m02="$2"
    local label="$3"
    local m01_seen=0
    local m02_seen=0
    if (( m01 >= PATH_PACKET_THRESHOLD )); then m01_seen=1; fi
    if (( m02 >= PATH_PACKET_THRESHOLD )); then m02_seen=1; fi
    if (( m01_seen == 1 && m02_seen == 1 )); then
        printf '[FR-5] FAIL: split %s path — both masters saw >= %d packets.\n' \
            "${label}" "${PATH_PACKET_THRESHOLD}" >&2
        printf '       Defensive guard: single-flow ICMP should not split.\n' >&2
        return 1
    fi
    if (( m01_seen == 1 )); then printf '%s' "${MASTER_01_CTR}"; return 0; fi
    if (( m02_seen == 1 )); then printf '%s' "${MASTER_02_CTR}"; return 0; fi
    printf '[FR-5] FAIL: %s path traffic below threshold (m01=%d m02=%d, threshold=%d).\n' \
        "${label}" "${m01}" "${m02}" "${PATH_PACKET_THRESHOLD}" >&2
    return 1
}

# Assert forward_master == reverse_master. Inputs: m01_fwd m01_rev m02_fwd m02_rev.
# Returns 0 on PASS, 1 on FAIL. Prints structured PASS/FAIL summary either way.
fr5::assert_symmetric() {
    local m01_fwd="$1"
    local m01_rev="$2"
    local m02_fwd="$3"
    local m02_rev="$4"

    printf '  [info] master-01: fwd=%d rev=%d  master-02: fwd=%d rev=%d\n' \
        "${m01_fwd}" "${m01_rev}" "${m02_fwd}" "${m02_rev}"

    if (( m01_fwd + m02_fwd == 0 )); then
        printf '[FR-5] FAIL: zero forward (echo request) packets on either master.\n' >&2
        printf '       Ping failed, tcpdump filter missed, or %s iface absent.\n' "${SRC_IFACE}" >&2
        return 1
    fi
    if (( m01_rev + m02_rev == 0 )); then
        printf '[FR-5] FAIL: zero reverse (echo reply) packets on either master.\n' >&2
        printf '       dst_ep may not be replying, or %s iface missing on masters.\n' "${DST_IFACE}" >&2
        return 1
    fi

    local forward_master reverse_master
    forward_master=$(fr5::path_master "${m01_fwd}" "${m02_fwd}" "forward") || return 1
    reverse_master=$(fr5::path_master "${m01_rev}" "${m02_rev}" "reverse") || return 1

    if [[ "${forward_master}" == "${reverse_master}" ]]; then
        printf '[FR-5] PASS: symmetric routing — forward_master=%s reverse_master=%s.\n' \
            "${forward_master}" "${reverse_master}"
        return 0
    fi

    printf '[FR-5] FAIL: asymmetric routing detected — forward via %s, reverse via %s.\n' \
        "${forward_master}" "${reverse_master}" >&2
    printf '       Per (src,dst)=(%s,%s), forward and reverse paths must traverse\n' \
        "${SRC_EP_OVERLAY}" "${DST_EP_OVERLAY}" >&2
    printf '       the same master tunnel under awg-mesh single-nexthop routing contract.\n' >&2
    return 1
}

# ---------------------------------------------------------------------------
# Self-test mode: inject synthetic asymmetry and verify the assertion FAILs.
# ---------------------------------------------------------------------------
if [[ "${FR5_SELF_TEST}" == "split-master" ]]; then
    printf '[FR-5] SELF-TEST mode: split-master\n'
    printf '       Injecting forward_master=master-01 reverse_master=master-02 with non-zero counts.\n'
    # Inject: master-01 saw all forward, master-02 saw all reverse → asymmetric.
    if fr5::assert_symmetric 5 0 0 5; then
        printf '[FR-5] SELF-TEST FAIL: assertion passed on synthetic asymmetric input.\n' >&2
        printf '       The module fails to detect forward_master != reverse_master regressions.\n' >&2
        exit 1
    fi
    printf '[FR-5] SELF-TEST PASS: assertion correctly rejected synthetic asymmetric routing.\n'
    exit 0
fi

# ---------------------------------------------------------------------------
# Run.
# ---------------------------------------------------------------------------
printf '[FR-5] Asymmetric routing detection\n'
printf '       src=%s (%s) dst=%s (%s) ping=%d\n' \
    "${SRC_EP_CTR}" "${SRC_EP_OVERLAY}" "${DST_EP_CTR}" "${DST_EP_OVERLAY}" "${PING_COUNT}"
printf '       masters: %s, %s\n' "${MASTER_01_CTR}" "${MASTER_02_CTR}"
printf '       fwd_iface=%s rev_iface=%s\n' "${SRC_IFACE}" "${DST_IFACE}"

fr5::require_running "${MASTER_01_CTR}" || exit 2
fr5::require_running "${MASTER_02_CTR}" || exit 2
fr5::require_running "${SRC_EP_CTR}" || exit 2
fr5::require_running "${DST_EP_CTR}" || exit 2

fr5::ensure_binary "${MASTER_01_CTR}" tcpdump || exit 2
fr5::ensure_binary "${MASTER_02_CTR}" tcpdump || exit 2

fr5::preflight_ping || exit 2

# Start four tcpdumps: forward + reverse on each master. The forward pcap
# captures echo requests on the wg-${SRC_EP_NAME} iface (where src_ep traffic
# enters); the reverse pcap captures echo replies on the wg-${DST_EP_NAME}
# iface (where dst_ep traffic enters). Filter constraints scope the pcap
# tightly so unrelated overlay traffic doesn't pollute the count.
fr5::start_tcpdump "${MASTER_01_CTR}" "${SRC_IFACE}" "${MASTER_01_FWD_PCAP}" \
    "icmp and src host ${SRC_EP_OVERLAY} and dst host ${DST_EP_OVERLAY}"
fr5::start_tcpdump "${MASTER_01_CTR}" "${DST_IFACE}" "${MASTER_01_REV_PCAP}" \
    "icmp and src host ${DST_EP_OVERLAY} and dst host ${SRC_EP_OVERLAY}"
fr5::start_tcpdump "${MASTER_02_CTR}" "${SRC_IFACE}" "${MASTER_02_FWD_PCAP}" \
    "icmp and src host ${SRC_EP_OVERLAY} and dst host ${DST_EP_OVERLAY}"
fr5::start_tcpdump "${MASTER_02_CTR}" "${DST_IFACE}" "${MASTER_02_REV_PCAP}" \
    "icmp and src host ${DST_EP_OVERLAY} and dst host ${SRC_EP_OVERLAY}"

sleep "${TCPDUMP_STARTUP_S}"

printf '  [info] generating ICMP flow %s -> %s (%d packets)...\n' \
    "${SRC_EP_OVERLAY}" "${DST_EP_OVERLAY}" "${PING_COUNT}"
fr5::generate_flow

# Drain: let in-flight packets settle into the pcaps before we stop tcpdump.
sleep "${FLOW_DRAIN_S}"

fr5::stop_tcpdump "${MASTER_01_CTR}"
fr5::stop_tcpdump "${MASTER_02_CTR}"

M01_FWD=$(fr5::count_echo_request "${MASTER_01_CTR}" "${MASTER_01_FWD_PCAP}") || exit 2
M01_REV=$(fr5::count_echo_reply   "${MASTER_01_CTR}" "${MASTER_01_REV_PCAP}") || exit 2
M02_FWD=$(fr5::count_echo_request "${MASTER_02_CTR}" "${MASTER_02_FWD_PCAP}") || exit 2
M02_REV=$(fr5::count_echo_reply   "${MASTER_02_CTR}" "${MASTER_02_REV_PCAP}") || exit 2
if ! [[ "${M01_FWD}" =~ ^[0-9]+$ ]]; then M01_FWD=0; fi
if ! [[ "${M01_REV}" =~ ^[0-9]+$ ]]; then M01_REV=0; fi
if ! [[ "${M02_FWD}" =~ ^[0-9]+$ ]]; then M02_FWD=0; fi
if ! [[ "${M02_REV}" =~ ^[0-9]+$ ]]; then M02_REV=0; fi

if fr5::assert_symmetric "${M01_FWD}" "${M01_REV}" "${M02_FWD}" "${M02_REV}"; then
    exit 0
fi
exit 1
