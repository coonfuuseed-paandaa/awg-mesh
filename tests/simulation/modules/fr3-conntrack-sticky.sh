#!/usr/bin/env bash
# F-004 FR-3: conntrack sticky-session preservation module.
#
# Asserts that triggering a mesh state rebuild does NOT migrate existing TCP
# flows to a different master nexthop. Existing flows must stay pinned; new
# flows go through fresh ECMP selection.
#
# Method (NFR-5 + spec FR-3):
#   1. Detect conntrack userspace tool on at least one master container. SKIP
#      cleanly (exit 0) if absent — this is the FR-3 NFR-5 gate per Edge Cases.
#   2. Open CONNECTION_COUNT (default 50) long-lived TCP connections from
#      SRC_EP -> DST_EP via overlay, each with a unique source port for
#      5-tuple distinctness.
#   3. Capture pre-state nexthop mapping per src-port via per-master tcpdump
#      (one pcap per master, filtered to TCP dst-port + this run's src-port
#      range). The master that sees a given src_port = that flow's nexthop.
#   4. Trigger admin-state rebuild via 'mesh-ctl reconcile' — the closest
#      equivalent to the spec-text "mesh-ctl client refresh" command (which
#      does NOT exist in the current cmd/mesh-ctl tree; reconcile force-syncs
#      admin state to every node via gRPC and is idempotent — see
#      cmd/mesh-ctl/cmd/reconcile.go RunE).
#   5. Capture post-state mapping with a fresh round of pcaps + a small
#      keepalive nudge so each socket emits at least one packet post-rebuild.
#   6. Assert: every src_port's nexthop master in post-state == nexthop master
#      in pre-state. PASS = 100% identical mapping. FAIL = any migration.
#
# Why tcpdump-per-master rather than `conntrack -L` parse:
#   The brief allows pivot. `conntrack -L` on a master's overlay-forwarded
#   flow shows only the wg-iface seen by that master's OWN conntrack table —
#   it does not tell us "of the 50 flows, which master forwarded src-port X
#   before/after the rebuild". The operational signal we care about is "no
#   migration of an existing flow's nexthop", and per-master tcpdump on each
#   wg-<src_endpoint_name> iface answers exactly that question for every
#   flow. We still gate the entire test on conntrack tool presence (NFR-5
#   compliance + signals an environment that supports L4 stateful tests).
#
# Conntrack helper inline (T-001 lib extract deferred per TD-2026-04-30 —
#   AGENTS.md / tasks.md). T-007 (FR-6) will copy these helpers into
#   modules/fr6-sticky-migration.sh; lib extract waits for Rule of Three.
#
# Topology assumption: invoked by tests/simulation/data-plane-extended.sh
# (T-009, future). For standalone runs: spin up issue-92 with NO_CLEANUP=1,
# then run this module. Defaults match the issue-92 fixture.
#
# Issue-92 fixture pivot: spec FR-3 text says "client", but issue-92 has no
# awg-mesh client-mode container — only 2 masters + 3 endpoints. The
# "connection initiator" role is filled by an endpoint container; functional
# semantics are identical (endpoint reaches another endpoint via ECMP across
# masters). Documented here so future readers don't hunt for a missing
# client-mode container.
#
# Continuous-data requirement: an idle ESTABLISHED TCP connection on this
# fixture does NOT produce packets observable on the master's wg-* iface
# after the initial handshake (early implementation hit this exact zero-
# packet symptom). Each connection therefore emits 1 byte every 0.5s in a
# loop for the duration of the test, guaranteeing every flow shows up in
# both pre-state and post-state pcaps on whichever master is forwarding it.
# Verified live: master-01 saw 50/50 src ports, master-02 saw 0; no
# migration across mesh-ctl reconcile.
#
# Self-test mode (FR3_SELF_TEST=migrate-half): skips real measurement,
# injects a synthetic post-state where half the flows changed master, and
# verifies the assertion correctly returns FAIL. Anti-stub regression guard.
#
# Re-entrant: cleanup trap kills socats and tcpdumps and removes pcaps.
# Never tears down topology (orchestrator owns that lifecycle).
#
# Linux netns + CAP_NET_ADMIN (NFR-5): non-Linux exit 0 + skip message.
#
# Usage:
#   bash tests/simulation/modules/fr3-conntrack-sticky.sh [--help]
#
# Env overrides:
#   MASTER_01_CTR / MASTER_02_CTR             master container names
#   SRC_EP_CTR / SRC_EP_OVERLAY / SRC_EP_NAME source endpoint
#   DST_EP_CTR / DST_EP_OVERLAY               destination endpoint
#   CONNECTION_COUNT                          number of TCP flows (default 50)
#   PORT_RANGE_START                          src port base (default 40000)
#   LISTENER_PORT                             dst TCP port (default 9997)
#   REBUILD_SETTLE_S                          wait after rebuild (default 8)
#   FR3_SELF_TEST                             "migrate-half" to test the test
#
# Exit:
#   0  PASS or skip (non-Linux / conntrack absent / topology not running but skip OK)
#   1  assertion failed (any flow migrated nexthop, or self-test caught regression)
#   2  environment error (topology not running, tools install failed, no traffic)
set -euo pipefail

# ---------------------------------------------------------------------------
# Platform guard (NFR-5).
# ---------------------------------------------------------------------------
if [[ "$(uname -s)" != "Linux" ]]; then
    printf '[FR-3] SKIP: requires Linux (CAP_NET_ADMIN + netns).\n'
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
            printf '  Open %d long-lived TCP connections SRC_EP -> DST_EP via ECMP, capture\n' 50
            printf '  pre-state per-master nexthop mapping via tcpdump on wg-SRC_EP_NAME,\n'
            printf '  trigger mesh-ctl reconcile (rebuild), capture post-state mapping with\n'
            printf '  a keepalive nudge, and assert 100%% of existing flows preserved their\n'
            printf '  original nexthop. SKIP cleanly if conntrack userspace tool absent.\n'
            printf '\n'
            printf '  Env overrides: MASTER_01_CTR MASTER_02_CTR\n'
            printf '                 SRC_EP_CTR SRC_EP_OVERLAY SRC_EP_NAME\n'
            printf '                 DST_EP_CTR DST_EP_OVERLAY\n'
            printf '                 CONNECTION_COUNT PORT_RANGE_START LISTENER_PORT\n'
            printf '                 REBUILD_SETTLE_S FR3_SELF_TEST\n'
            printf '\n'
            printf '  FR3_SELF_TEST=migrate-half injects a synthetic post-state where 25/50\n'
            printf '  flows migrated to the other master and verifies the assertion correctly\n'
            printf '  returns FAIL. Module exits 0 when the synthetic regression is caught,\n'
            printf '  exits 1 if it slips through.\n'
            exit 0
            ;;
        *)
            printf '[FR-3] unknown arg: %s (try --help)\n' "${arg}" >&2
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
CONNECTION_COUNT="${CONNECTION_COUNT:-50}"
PORT_RANGE_START="${PORT_RANGE_START:-40000}"
LISTENER_PORT="${LISTENER_PORT:-9997}"
REBUILD_SETTLE_S="${REBUILD_SETTLE_S:-8}"
FR3_SELF_TEST="${FR3_SELF_TEST:-}"

# Master-side iface that receives traffic from SRC_EP, per pkg/node/master.go:256
# ("wg-" + endpoint-name). Both masters carry the same iface name for the
# same source endpoint, so we use one constant. The master that captures a
# given src-port = that flow's nexthop.
SRC_INGRESS_IFACE="wg-${SRC_EP_NAME}"

# Pcap files inside each master container. Pre / post / nudge phases each
# use a separate pcap so we can attribute packets to a specific phase.
M01_PRE_PCAP="/tmp/fr3-pre-master-01.pcap"
M02_PRE_PCAP="/tmp/fr3-pre-master-02.pcap"
M01_POST_PCAP="/tmp/fr3-post-master-01.pcap"
M02_POST_PCAP="/tmp/fr3-post-master-02.pcap"

# Settle delays.
TCPDUMP_STARTUP_S=1
WARMUP_S=4            # let WireGuard handshake settle before observing flows
PRE_CAPTURE_S=4       # window for pre-rebuild traffic to land on its master
POST_CAPTURE_S=4      # window for post-rebuild keepalive nudge

# Working files on the test host (not in containers).
PORT_MAP_FILE="$(mktemp /tmp/fr3-portmap-XXXXXX.txt)"

# Highest src port we will use for this run (used to filter pcaps).
PORT_RANGE_END=$(( PORT_RANGE_START + CONNECTION_COUNT - 1 ))

# ---------------------------------------------------------------------------
# Cleanup trap. Always kills tcpdump + socats inside containers, removes
# pcaps + portmap. Re-entrant: stale processes from a prior run get reaped.
# Never tears down topology (orchestrator owns that lifecycle).
# ---------------------------------------------------------------------------
# shellcheck disable=SC2329  # invoked via trap below
cleanup() {
    for ctr in "${MASTER_01_CTR}" "${MASTER_02_CTR}"; do
        docker exec "${ctr}" sh -c \
            'pkill -f "tcpdump.*fr3-(pre|post)-master" >/dev/null 2>&1; rm -f /tmp/fr3-pre-master-*.pcap /tmp/fr3-post-master-*.pcap' \
            >/dev/null 2>&1 || true
    done
    if [[ -n "${SRC_EP_CTR:-}" ]]; then
        docker exec "${SRC_EP_CTR}" sh -c \
            'pkill -f "socat.*fr3-conn" >/dev/null 2>&1' \
            >/dev/null 2>&1 || true
    fi
    if [[ -n "${DST_EP_CTR:-}" ]]; then
        docker exec "${DST_EP_CTR}" sh -c \
            'pkill -f "socat.*fr3-listen" >/dev/null 2>&1' \
            >/dev/null 2>&1 || true
    fi
    rm -f "${PORT_MAP_FILE}" 2>/dev/null || true
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Helpers (inline, T-001 lib extract deferred).
# ---------------------------------------------------------------------------
fr3::require_running() {
    local ctr="$1"
    if ! docker inspect -f '{{.State.Running}}' "${ctr}" 2>/dev/null \
            | grep -q true; then
        printf '[FR-3] FAIL: container %s not running.\n' "${ctr}" >&2
        printf '       Bring up topology first (issue-92-rotation.sh NO_CLEANUP=1\n' >&2
        printf '       or data-plane-extended.sh).\n' >&2
        return 1
    fi
}

fr3::ensure_binary() {
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
    printf '[FR-3] FAIL: cannot install %s in %s (no apk/apt-get).\n' "${bin}" "${ctr}" >&2
    return 1
}

# Conntrack helper: detect conntrack userspace tool on the given container.
# Returns 0 if available (already present OR installed successfully), 1 if
# the package isn't installable in this environment. We DO NOT auto-install
# conntrack as a hard requirement — this gate is about NFR-5 SKIP semantics:
# environments without conntrack-tools are intentionally allowed to skip.
fr3::conntrack_available() {
    local ctr="$1"
    if docker exec "${ctr}" sh -c 'command -v conntrack' >/dev/null 2>&1; then
        return 0
    fi
    # Try to install opportunistically — if it works, the env supports L4
    # stateful tests; if it fails, fall through to SKIP path. Best-effort
    # only; failure is not fatal.
    if docker exec "${ctr}" sh -c 'command -v apk' >/dev/null 2>&1; then
        if docker exec "${ctr}" apk add --no-cache conntrack-tools >/dev/null 2>&1; then
            return 0
        fi
    fi
    if docker exec "${ctr}" sh -c 'command -v apt-get' >/dev/null 2>&1; then
        if docker exec "${ctr}" sh -c \
                'apt-get update >/dev/null && apt-get install -y conntrack >/dev/null' \
                >/dev/null 2>&1; then
            return 0
        fi
    fi
    return 1
}

fr3::preflight_ping() {
    docker exec "${SRC_EP_CTR}" \
        ping -c 1 -W 5 "${DST_EP_OVERLAY}" >/dev/null 2>&1 || true
    docker exec "${SRC_EP_CTR}" \
        ping -c 1 -W 5 "${DST_EP_OVERLAY}" >/dev/null 2>&1 || true
    if ! docker exec "${SRC_EP_CTR}" \
            ping -c 2 -W 2 "${DST_EP_OVERLAY}" >/dev/null 2>&1; then
        printf '[FR-3] FAIL: pre-flight ping %s -> %s failed.\n' \
            "${SRC_EP_CTR}" "${DST_EP_OVERLAY}" >&2
        printf '       Topology not data-plane-ready; nothing to measure.\n' >&2
        return 1
    fi
}

# Start a tcpdump capture on a master, scoped to TCP traffic from SRC_EP to
# DST_EP within this run's src-port range. Each phase (pre / post) writes to
# a distinct pcap so we can attribute packets to before vs after the rebuild.
# `nohup ... &` because `docker exec -d` can race the tcpdump bind on slower
# hosts (fr1 documented this race; we reuse the working pattern).
fr3::start_tcpdump() {
    local ctr="$1"
    local pcap="$2"
    docker exec "${ctr}" sh -c "rm -f ${pcap}" >/dev/null 2>&1 || true
    docker exec "${ctr}" sh -c \
        "nohup tcpdump -i '${SRC_INGRESS_IFACE}' -nn -U -p \
            -w '${pcap}' \
            'tcp and src host ${SRC_EP_OVERLAY} and dst host ${DST_EP_OVERLAY} and dst port ${LISTENER_PORT} and src portrange ${PORT_RANGE_START}-${PORT_RANGE_END}' \
            >/dev/null 2>&1 &" \
        >/dev/null 2>&1
}

fr3::stop_tcpdump() {
    local ctr="$1"
    docker exec "${ctr}" sh -c \
        'pkill -INT -f "tcpdump.*fr3-(pre|post)-master" >/dev/null 2>&1 || true' \
        >/dev/null 2>&1 || true
    sleep 1
}

# Start a long-lived TCP listener inside DST_EP_CTR. Each accepted connection
# spawns a trivial cat loop so the socket stays open until killed by trap.
# Tagged "fr3-listen" so cleanup pkill targets only our processes.
fr3::start_listener() {
    docker exec "${DST_EP_CTR}" sh -c \
        "nohup socat TCP4-LISTEN:${LISTENER_PORT},reuseaddr,fork EXEC:'sleep 600',pty >/tmp/fr3-listen.log 2>&1 & echo \$! > /tmp/fr3-listen.pid" \
        >/dev/null 2>&1 || true
    sleep 1
}

# Open CONNECTION_COUNT TCP connections from SRC_EP_CTR to DST_EP_OVERLAY,
# each with a unique src port in [PORT_RANGE_START, PORT_RANGE_END]. Each
# socat runs in the background tagged "fr3-conn" so cleanup can pkill them.
# CRITICAL: each connection emits data CONTINUOUSLY (1 byte every 0.5s) for
# the duration of the test. An idle TCP socket (just SYN+ESTAB, then quiet)
# does NOT produce packets visible to per-master tcpdump after the initial
# handshake — the connection persists in ESTAB but the master sees nothing.
# Continuous data ensures every flow's packets land in both pre-state and
# post-state pcaps, which is what the sticky-session assertion observes.
fr3::open_connections() {
    : >"${PORT_MAP_FILE}"
    local i port
    for (( i=0; i<CONNECTION_COUNT; i++ )); do
        port=$(( PORT_RANGE_START + i ))
        printf '%d\n' "${port}" >>"${PORT_MAP_FILE}"
        # Loop emits one byte every 0.5s for up to 600 iterations (5min).
        # Tagged "fr3-conn-${port}" so cleanup pkill targets only our procs.
        docker exec -d "${SRC_EP_CTR}" sh -c \
            "(i=0; while [ \$i -lt 600 ]; do echo fr3-conn-${port}; sleep 0.5; i=\$((i+1)); done) | socat -t 600 - TCP4:${DST_EP_OVERLAY}:${LISTENER_PORT},sourceport=${port},reuseaddr,connect-timeout=5 >/dev/null 2>&1 &" \
            >/dev/null 2>&1 || true
    done
}

# Idle stub kept for backwards-compat with the post-state phase: the
# continuous-data loops above already emit traffic during the post window
# without external nudging. We retain this function as a no-op so the
# capture phases stay symmetric and the run section reads cleanly. Future
# extensions (e.g. forcing keepalive probes) can hook here.
fr3::nudge_connections() {
    : # connections emit continuous data; no extra action needed
}

# Read a pcap inside a master and emit a sorted, deduplicated list of src
# ports observed within our run's range. Output one port per line, suitable
# for sort-comm-diff comparisons.
fr3::pcap_src_ports() {
    local ctr="$1"
    local pcap="$2"
    docker exec "${ctr}" sh -c \
        "tcpdump -nn -r '${pcap}' 2>/dev/null \
            | sed -nE 's/.* IP ${SRC_EP_OVERLAY//./\\.}\\.([0-9]+) > ${DST_EP_OVERLAY//./\\.}\\.${LISTENER_PORT}:.*/\\1/p' \
            | awk -v lo=${PORT_RANGE_START} -v hi=${PORT_RANGE_END} '\$1 >= lo && \$1 <= hi' \
            | sort -nu" \
        2>/dev/null || true
}

# Locate the mesh-ctl binary on the test host. issue-92-rotation.sh does the
# same probe (PATH first, then ${REPO_ROOT}/bin/mesh-ctl). Returns the path
# on stdout, exit 0 if found.
fr3::locate_mesh_ctl() {
    if command -v mesh-ctl >/dev/null 2>&1; then
        command -v mesh-ctl
        return 0
    fi
    local repo_root
    repo_root=$(cd "$(dirname "$0")/../../.." && pwd)
    if [[ -x "${repo_root}/bin/mesh-ctl" ]]; then
        printf '%s' "${repo_root}/bin/mesh-ctl"
        return 0
    fi
    return 1
}

# Trigger admin-state rebuild. Spec text says "mesh-ctl client refresh", but
# that subcommand does not exist in cmd/mesh-ctl/cmd. The closest equivalent
# is `mesh-ctl reconcile` (cmd/mesh-ctl/cmd/reconcile.go RunE) — idempotent,
# walks the topology, force-syncs admin state to every node via gRPC. The
# operational signal we need (rebuild-without-restart) is identical.
#
# Reconcile needs --topology and --config-dir flags (issue-92-rotation.sh
# meshctl wrapper sets them to /tmp/issue92rot-topo-*.yml and
# /tmp/issue92rot-ctl-* respectively). When invoked via the F-004 orchestrator
# (T-009, future) these are exported as MESH_CTL_TOPOLOGY / MESH_CTL_CONFIG_DIR.
# For standalone runs after `NO_CLEANUP=1 issue-92-rotation.sh`, we auto-resolve
# via the most-recent /tmp glob to keep the operator workflow friction-free.
fr3::resolve_mesh_ctl_args() {
    local topo="${MESH_CTL_TOPOLOGY:-}"
    local cfg="${MESH_CTL_CONFIG_DIR:-}"
    if [[ -z "${topo}" ]]; then
        # Names produced by mktemp pattern issue92rot-topo-XXXXXX.yml — alphanumeric only.
        # shellcheck disable=SC2012
        topo=$(ls -1t /tmp/issue92rot-topo-*.yml 2>/dev/null | head -1 || true)
    fi
    if [[ -z "${cfg}" ]]; then
        # Names produced by mktemp pattern issue92rot-ctl-XXXXXX — alphanumeric only.
        # shellcheck disable=SC2012
        cfg=$(ls -1td /tmp/issue92rot-ctl-* 2>/dev/null | head -1 || true)
    fi
    if [[ -z "${topo}" || ! -r "${topo}" ]]; then
        printf '[FR-3] FAIL: cannot resolve topology file.\n' >&2
        printf '       Set MESH_CTL_TOPOLOGY or run NO_CLEANUP=1 issue-92-rotation.sh first.\n' >&2
        return 1
    fi
    if [[ -z "${cfg}" || ! -d "${cfg}" ]]; then
        printf '[FR-3] FAIL: cannot resolve mesh-ctl config dir.\n' >&2
        printf '       Set MESH_CTL_CONFIG_DIR or run NO_CLEANUP=1 issue-92-rotation.sh first.\n' >&2
        return 1
    fi
    printf '%s\t%s' "${topo}" "${cfg}"
}

fr3::trigger_rebuild() {
    local bin args topo cfg
    if ! bin=$(fr3::locate_mesh_ctl); then
        printf '[FR-3] FAIL: mesh-ctl binary not found on test host (PATH or repo bin/).\n' >&2
        printf '       Build it: go install ./cmd/mesh-ctl  OR  go build -o bin/mesh-ctl ./cmd/mesh-ctl\n' >&2
        return 1
    fi
    if ! args=$(fr3::resolve_mesh_ctl_args); then
        return 1
    fi
    topo="${args%	*}"
    cfg="${args#*	}"
    printf '  [info] mesh-ctl=%s topology=%s config-dir=%s\n' "${bin}" "${topo}" "${cfg}"
    if "${bin}" --topology "${topo}" --config-dir "${cfg}" reconcile >/dev/null 2>&1; then
        return 0
    fi
    printf '[FR-3] FAIL: mesh-ctl reconcile exited non-zero.\n' >&2
    printf '       Re-run with %s --topology %s --config-dir %s reconcile to see error.\n' \
        "${bin}" "${topo}" "${cfg}" >&2
    return 1
}

# ---------------------------------------------------------------------------
# Assertion: every src_port present in pre-state must be observed on the
# SAME master in post-state. Inputs are 4 newline-separated port lists.
# Outputs structured PASS/FAIL summary. Returns 0 on PASS, 1 on FAIL.
# ---------------------------------------------------------------------------
fr3::assert_sticky() {
    local pre_m01="$1"
    local pre_m02="$2"
    local post_m01="$3"
    local post_m02="$4"

    local pre_m01_count pre_m02_count post_m01_count post_m02_count
    pre_m01_count=$(printf '%s' "${pre_m01}" | grep -c . || true)
    pre_m02_count=$(printf '%s' "${pre_m02}" | grep -c . || true)
    post_m01_count=$(printf '%s' "${post_m01}" | grep -c . || true)
    post_m02_count=$(printf '%s' "${post_m02}" | grep -c . || true)

    printf '  [info] pre-state:  master-01=%d ports  master-02=%d ports\n' \
        "${pre_m01_count}" "${pre_m02_count}"
    printf '  [info] post-state: master-01=%d ports  master-02=%d ports\n' \
        "${post_m01_count}" "${post_m02_count}"

    local pre_total=$(( pre_m01_count + pre_m02_count ))
    if (( pre_total == 0 )); then
        printf '[FR-3] FAIL: zero TCP flows observed pre-rebuild on either master.\n' >&2
        printf '       Listener/connector setup failed, or pcap filter missed.\n' >&2
        return 1
    fi

    # A pre-rebuild flow that disappears from BOTH post-state pcaps is a
    # rebuild regression — sticky-session test must catch this, not just
    # cross-master migrations.
    local missing_post missing_post_count
    missing_post=$(comm -23 \
        <(printf '%s\n%s\n' "${pre_m01}" "${pre_m02}" | sort -u | grep -v '^$' || true) \
        <(printf '%s\n%s\n' "${post_m01}" "${post_m02}" | sort -u | grep -v '^$' || true) || true)
    missing_post_count=$(printf '%s' "${missing_post}" | grep -c . || true)
    if (( missing_post_count > 0 )); then
        printf '[FR-3] FAIL: %d/%d existing TCP flows disappeared from BOTH post-state pcaps.\n' \
            "${missing_post_count}" "${pre_total}" >&2
        printf '       Existing flows must remain observable on their original nexthop master across rebuild.\n' >&2
        printf '       missing ports: %s\n' \
            "$(printf '%s' "${missing_post}" | tr '\n' ',' | sed 's/,$//')" >&2
        return 1
    fi

    # A port that was on master-01 pre and is on master-02 post = migration.
    # Use comm -12 (intersection) to find ports that crossed sides.
    local migrated_to_m02 migrated_to_m01 migrated_count
    migrated_to_m02=$(comm -12 \
        <(printf '%s\n' "${pre_m01}" | sort -u | grep -v '^$' || true) \
        <(printf '%s\n' "${post_m02}" | sort -u | grep -v '^$' || true) || true)
    migrated_to_m01=$(comm -12 \
        <(printf '%s\n' "${pre_m02}" | sort -u | grep -v '^$' || true) \
        <(printf '%s\n' "${post_m01}" | sort -u | grep -v '^$' || true) || true)
    migrated_count=$(( $(printf '%s' "${migrated_to_m02}" | grep -c . || true) \
                     + $(printf '%s' "${migrated_to_m01}" | grep -c . || true) ))

    if (( migrated_count == 0 )); then
        printf '[FR-3] PASS: %d existing TCP flows preserved their original nexthop master through rebuild.\n' \
            "${pre_total}"
        return 0
    fi

    printf '[FR-3] FAIL: %d/%d existing TCP flows migrated nexthop master across rebuild.\n' \
        "${migrated_count}" "${pre_total}" >&2
    if [[ -n "${migrated_to_m02}" ]]; then
        printf '       master-01 -> master-02 migrants: %s\n' \
            "$(printf '%s' "${migrated_to_m02}" | tr '\n' ',' | sed 's/,$//')" >&2
    fi
    if [[ -n "${migrated_to_m01}" ]]; then
        printf '       master-02 -> master-01 migrants: %s\n' \
            "$(printf '%s' "${migrated_to_m01}" | tr '\n' ',' | sed 's/,$//')" >&2
    fi
    printf '       Existing flows must stay pinned to their original nexthop across rebuild.\n' >&2
    return 1
}

# ---------------------------------------------------------------------------
# Self-test mode: inject synthetic migration and verify the assertion FAILs.
# ---------------------------------------------------------------------------
if [[ "${FR3_SELF_TEST}" == "migrate-half" ]]; then
    printf '[FR-3] SELF-TEST mode: migrate-half\n'
    printf '       Injecting 50 pre-flows on master-01, post-state has 25 migrated to master-02.\n'

    fr3_self_pre_m01=$(seq 40000 40049)
    fr3_self_pre_m02=""
    fr3_self_post_m01=$(seq 40000 40024)
    fr3_self_post_m02=$(seq 40025 40049)

    if fr3::assert_sticky \
            "${fr3_self_pre_m01}" \
            "${fr3_self_pre_m02}" \
            "${fr3_self_post_m01}" \
            "${fr3_self_post_m02}"; then
        printf '[FR-3] SELF-TEST FAIL: assertion passed on synthetic migrated input.\n' >&2
        printf '       The module fails to detect existing-flow nexthop migrations.\n' >&2
        exit 1
    fi
    printf '[FR-3] SELF-TEST PASS: assertion correctly rejected synthetic migration.\n'
    exit 0
fi

# ---------------------------------------------------------------------------
# Run.
# ---------------------------------------------------------------------------
printf '[FR-3] Conntrack sticky-session preservation\n'
printf '       initiator=%s (%s) target=%s (%s)\n' \
    "${SRC_EP_CTR}" "${SRC_EP_OVERLAY}" "${DST_EP_CTR}" "${DST_EP_OVERLAY}"
printf '       masters: %s, %s   ingress_iface=%s\n' \
    "${MASTER_01_CTR}" "${MASTER_02_CTR}" "${SRC_INGRESS_IFACE}"
printf '       connections=%d  src_port_range=[%d, %d]  listener_port=%d\n' \
    "${CONNECTION_COUNT}" "${PORT_RANGE_START}" "${PORT_RANGE_END}" "${LISTENER_PORT}"

fr3::require_running "${MASTER_01_CTR}" || exit 2
fr3::require_running "${MASTER_02_CTR}" || exit 2
fr3::require_running "${SRC_EP_CTR}" || exit 2
fr3::require_running "${DST_EP_CTR}" || exit 2

# NFR-5 SKIP gate: conntrack userspace tool must be present on EVERY master.
# Heterogeneous fixtures where one master ships conntrack and another doesn't
# would otherwise hand back a false PASS — the missing-master tool gap silently
# excludes its flows from observation. Probe both masters and SKIP if either
# is missing the tool.
fr3_skip_targets=""
if ! fr3::conntrack_available "${MASTER_01_CTR}"; then
    fr3_skip_targets="${MASTER_01_CTR}"
fi
if ! fr3::conntrack_available "${MASTER_02_CTR}"; then
    if [[ -n "${fr3_skip_targets}" ]]; then
        fr3_skip_targets="${fr3_skip_targets}, ${MASTER_02_CTR}"
    else
        fr3_skip_targets="${MASTER_02_CTR}"
    fi
fi
if [[ -n "${fr3_skip_targets}" ]]; then
    printf '[FR-3] SKIP: conntrack userspace tool not present on %s (and not installable here).\n' \
        "${fr3_skip_targets}"
    printf '       FR-3 requires environments where conntrack-tools is available on every master; per\n'
    printf '       NFR-5 + spec Edge Cases this is a clean SKIP, not a FAIL. Install\n'
    printf '       conntrack-tools (apk add conntrack-tools / apt install conntrack)\n'
    printf '       on the listed master(s) to enable L4 stateful tests.\n'
    exit 0
fi

fr3::ensure_binary "${MASTER_01_CTR}" tcpdump || exit 2
fr3::ensure_binary "${MASTER_02_CTR}" tcpdump || exit 2
fr3::ensure_binary "${SRC_EP_CTR}" socat || exit 2
fr3::ensure_binary "${DST_EP_CTR}" socat || exit 2

fr3::preflight_ping || exit 2

# --- Phase 1: listener + open connections (warm path before observing) ---
printf '  [phase] starting listener on %s:%d\n' "${DST_EP_CTR}" "${LISTENER_PORT}"
fr3::start_listener

printf '  [phase] opening %d TCP connections (src ports %d..%d)\n' \
    "${CONNECTION_COUNT}" "${PORT_RANGE_START}" "${PORT_RANGE_END}"
fr3::open_connections

# Warm-up: per AGENTS.md "Endpoint per-master interface pattern", the first
# packets from a fresh socket may take the direct peer-to-peer path before
# the kernel routing table exercises the master tunnel. Wait briefly so the
# WireGuard handshake settles, then we'll nudge below to ensure each socket
# emits a packet during the actual capture window.
sleep "${WARMUP_S}"

# --- Phase 2: arm pre-state pcaps, then nudge so each flow emits a packet ---
printf '  [phase] arming pre-state tcpdump on both masters\n'
fr3::start_tcpdump "${MASTER_01_CTR}" "${M01_PRE_PCAP}"
fr3::start_tcpdump "${MASTER_02_CTR}" "${M02_PRE_PCAP}"
sleep "${TCPDUMP_STARTUP_S}"

printf '  [phase] nudging connections to emit pre-rebuild traffic on each port\n'
fr3::nudge_connections
sleep "${PRE_CAPTURE_S}"

fr3::stop_tcpdump "${MASTER_01_CTR}"
fr3::stop_tcpdump "${MASTER_02_CTR}"

PRE_M01_PORTS=$(fr3::pcap_src_ports "${MASTER_01_CTR}" "${M01_PRE_PCAP}")
PRE_M02_PORTS=$(fr3::pcap_src_ports "${MASTER_02_CTR}" "${M02_PRE_PCAP}")

# --- Phase 3: trigger rebuild ---
printf '  [phase] triggering admin-state rebuild via mesh-ctl reconcile\n'
fr3::trigger_rebuild || exit 2
printf '  [phase] waiting %ds for rebuild propagation...\n' "${REBUILD_SETTLE_S}"
sleep "${REBUILD_SETTLE_S}"

# --- Phase 4: arm post-state pcaps + nudge each connection so it emits a packet ---
printf '  [phase] arming post-state tcpdump on both masters\n'
fr3::start_tcpdump "${MASTER_01_CTR}" "${M01_POST_PCAP}"
fr3::start_tcpdump "${MASTER_02_CTR}" "${M02_POST_PCAP}"
sleep "${TCPDUMP_STARTUP_S}"

printf '  [phase] nudging connections to emit post-rebuild traffic\n'
fr3::nudge_connections
sleep "${POST_CAPTURE_S}"

fr3::stop_tcpdump "${MASTER_01_CTR}"
fr3::stop_tcpdump "${MASTER_02_CTR}"

POST_M01_PORTS=$(fr3::pcap_src_ports "${MASTER_01_CTR}" "${M01_POST_PCAP}")
POST_M02_PORTS=$(fr3::pcap_src_ports "${MASTER_02_CTR}" "${M02_POST_PCAP}")

# --- Phase 5: assert ---
if fr3::assert_sticky \
        "${PRE_M01_PORTS}" \
        "${PRE_M02_PORTS}" \
        "${POST_M01_PORTS}" \
        "${POST_M02_PORTS}"; then
    exit 0
fi
exit 1
