#!/usr/bin/env bash
# issue-92-rotation.sh — Docker integration harness for tier-3 rotation fix.
# local tracker #117: tier-3 rotate used to fail with uapi errno=-22 (EINVAL)
# on the first ApplyParams call (S3/S4 emitted to amneziawg-go UAPI, which
# rejects them via the default branch). Fixed in PR #65 / commit 698e912 by
# dropping s3=/s4= from pkg/wg/uapi.go::writeConfig.
#
# What this script validates (post-#117):
#   R1  Two-master + one-endpoint mesh boots cleanly; both masters gRPC-ready.
#   R2  `mesh-ctl master init` + `mesh-ctl endpoint init` baseline succeeds —
#       CLI admin-state and master runtime peer entries are populated.
#   R3  `mesh-ctl rotate --tier 3 --endpoint <name>` completes without
#       `uapi errno=-22`. Stdout reports "tier 3 rotation succeeded" for every
#       master; stderr contains NO UAPI errno string. This is the #117 assertion.
#   R4  Post-rotation health: both masters still gRPC-reachable, tunnel
#       interface `wg-<endpoint>` still up on every master.
#   R5  Control plane still intact: a second `mesh-ctl endpoint init` exits 0.
#
# Introspection approach: amneziawg-go runs in userspace and exposes its UAPI
# via /run/amneziawg/<iface>.sock. The kernel-targeted `wg` CLI cannot access
# this socket ("Unable to access interface: Not supported"), so this script
# uses the control-plane path instead — `mesh-ctl inspect <node>` fetches
# runtime peer state via gRPC GetTransportState.
#
# Scope (post local tracker #125 / v1.12):
#   R6  mesh-ctl rotate --tier 3 — full keypair rotation (endpoint privKey
#       rebind via new RotateKeypair RPC + per-master UpdateTunnelPeer swap +
#       atomic admin-state write).
#   R6a admin-state pubkey on disk DIFFERS from pre-rotation.
#   R6b both masters show the NEW pubkey in mesh-ctl inspect runtime column.
#   R6c OLD pubkey is absent from runtime peer list on every master.
#   R6d `mesh-ctl inspect` reports zero drift on both masters post-rotation.
#
# Usage (Linux host with Docker):
#   cd <repo-root>
#   bash tests/simulation/issue-92-rotation.sh
#
# Prerequisites:
#   - Docker running (Linux host or Docker Desktop with Linux containers).
#   - awg-mesh-node:local image built:
#       docker build -t awg-mesh-node:local .
#   - mesh-ctl in PATH or $REPO_ROOT/bin/mesh-ctl present:
#       go install ./cmd/mesh-ctl
#       # or: go build -o bin/mesh-ctl ./cmd/mesh-ctl
#   - CAP_NET_ADMIN available inside containers (privileged: true in compose).
#
# Windows hosts: this script requires Linux-namespaced TUN devices and
# CAP_NET_ADMIN inside containers, which are unavailable on Windows Docker
# Desktop without WSL2 kernel support. Run inside WSL2 Ubuntu or a CI Linux
# runner instead.
#
# Exit: 0 = all checks passed, non-zero = failure count.
set -euo pipefail

# ---------------------------------------------------------------------------
# Platform guard — exit cleanly on non-Linux so CI skips rather than errors.
# ---------------------------------------------------------------------------
if [[ "$(uname -s)" != "Linux" ]]; then
    echo "[R0] SKIP: issue-92-rotation.sh requires Linux (WireGuard kernel module)."
    echo "     Run inside WSL2 Ubuntu or a CI Linux runner."
    exit 0
fi

# ---------------------------------------------------------------------------
# Paths
# ---------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

# Compose project name — unique to this script to avoid conflicts.
COMPOSE_PROJECT="issue92rot"

# Mesh names used throughout.
MASTER_RU_01="mst-ru-01"
MASTER_RU_02="mst-ru-02"
ENDPOINT_US_01="ep-us-01"

# Container names (project prefix + service name).
CTR_MASTER_RU_01="${COMPOSE_PROJECT}-${MASTER_RU_01}"
CTR_MASTER_RU_02="${COMPOSE_PROJECT}-${MASTER_RU_02}"
CTR_ENDPOINT_US_01="${COMPOSE_PROJECT}-${ENDPOINT_US_01}"

# Master-side WireGuard interface name for the endpoint tunnel. Master mode
# creates one userspace amneziawg-go interface per endpoint peer, named
# `wg-<endpoint-name>` by pkg/node/master.go::AddTunnel. The userspace driver
# exposes UAPI via /run/amneziawg/<iface>.sock (the kernel `wg` CLI cannot
# speak to it — see Dockerfile.node comment), so we query runtime state via
# `mesh-ctl inspect` over gRPC instead of `wg show`. The interface name is
# still useful as a data-plane liveness check via `ip link show`.
MASTER_IFACE_EP_US_01="wg-${ENDPOINT_US_01}"

# Timing
GRPC_READY_TIMEOUT=60   # seconds to wait for gRPC server ready log
KEY_PROPAGATE_TIMEOUT=5 # seconds allowed for new key to appear on master wg interface

# mesh-ctl config dir — scoped to this test run.
CTL_CONFIG_DIR=$(mktemp -d /tmp/issue92rot-ctl-XXXXXX)
TOPO_FILE=$(mktemp /tmp/issue92rot-topo-XXXXXX.yml)
COMPOSE_FILE=$(mktemp /tmp/issue92rot-compose-XXXXXX.yml)

# Overlay and bridge addresses (chosen to avoid conflicts with existing sims).
MASTER_RU_01_OVERLAY="172.21.92.2"
MASTER_RU_02_OVERLAY="172.21.92.3"
ENDPOINT_US_01_OVERLAY="172.21.92.34"

MASTER_RU_01_BRIDGE="192.168.92.10"
MASTER_RU_02_BRIDGE="192.168.92.11"
ENDPOINT_US_01_BRIDGE="192.168.92.20"

# Docker image — override with IMAGE env var if needed.
NODE_IMAGE="${IMAGE:-awg-mesh-node:local}"

# ---------------------------------------------------------------------------
# Counters
# ---------------------------------------------------------------------------
FAILURES=0
PASSES=0

# ---------------------------------------------------------------------------
# Colours
# ---------------------------------------------------------------------------
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RESET='\033[0m'

pass() { echo -e "  [${GREEN}PASS${RESET}] $*"; (( PASSES++ )) || true; }
fail() { echo -e "  [${RED}FAIL${RESET}] $*" >&2; (( FAILURES++ )) || true; }
info() { echo "  [info] $*"; }

# ---------------------------------------------------------------------------
# Cleanup trap
# ---------------------------------------------------------------------------
cleanup() {
    local rc=$?
    echo ""
    if [[ "${NO_CLEANUP:-0}" == "1" ]]; then
        echo "[cleanup] NO_CLEANUP=1 — leaving containers/files for inspection."
        echo "  Compose project: ${COMPOSE_PROJECT}"
        echo "  Compose file:    ${COMPOSE_FILE}"
        echo "  Topology file:   ${TOPO_FILE}"
        echo "  ctl config dir:  ${CTL_CONFIG_DIR}"
        return
    fi
    echo "[cleanup] Tearing down containers and temp files..."
    docker compose -p "${COMPOSE_PROJECT}" -f "${COMPOSE_FILE}" down -v --remove-orphans 2>/dev/null || true
    rm -f "${TOPO_FILE}" "${COMPOSE_FILE}"
    rm -rf "${CTL_CONFIG_DIR}"
    if [[ "${rc}" -eq 0 && "${FAILURES}" -eq 0 ]]; then
        echo "[cleanup] Done. Test PASSED."
    else
        echo "[cleanup] Done. Test FAILED (${FAILURES} failure(s))."
    fi
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

compose_run() {
    docker compose -p "${COMPOSE_PROJECT}" -f "${COMPOSE_FILE}" "$@"
}

# wait_for_log <container> <pattern> <timeout_seconds>
wait_for_log() {
    local container="$1"
    local pattern="$2"
    local timeout="${3:-${GRPC_READY_TIMEOUT}}"
    local deadline=$(( $(date +%s) + timeout ))
    while true; do
        if docker logs "${container}" 2>&1 | grep -q "${pattern}"; then
            return 0
        fi
        if [[ $(date +%s) -ge ${deadline} ]]; then
            return 1
        fi
        sleep 2
    done
}

# Master-side interface name for a given endpoint tunnel. Matches
# pkg/node/master.go::AddTunnel which sets InterfaceName = "wg-" + name.
master_iface_for() {
    local endpoint_name="$1"
    echo "wg-${endpoint_name}"
}

# meshctl — thin wrapper that injects the topology and config-dir flags every
# time. All `mesh-ctl` invocations past the prepare/init block go through this.
meshctl() {
    "${MESHCTL_BIN}" \
        --topology "${TOPO_FILE}" \
        --config-dir "${CTL_CONFIG_DIR}" \
        "$@"
}

# admin_pubkey_of <node-name> — prints full admin-state pubkey written by
# `mesh-ctl endpoint init` / `master init`. Empty string if absent.
admin_pubkey_of() {
    local node="$1"
    local f="${CTL_CONFIG_DIR}/nodes/${node}/pubkey"
    [[ -r "${f}" ]] && tr -d '[:space:]' < "${f}" || true
}

# inspect_node <node> — runs `mesh-ctl inspect <node>` and prints raw output.
# Exit status is discarded here; callers use inspect_has_drift / inspect_has_key.
inspect_node() {
    local node="$1"
    meshctl inspect "${node}" 2>&1 || true
}

# inspect_runtime_prefix_for <node> <peer-name> — extracts the RUNTIME_KEY
# column from `mesh-ctl inspect <node>` for a given peer row. Column values
# are truncated to 17 chars + `…` by the tabular renderer; callers should
# compare via `admin_key[0:17]` == `runtime_prefix%…`.
inspect_runtime_prefix_for() {
    local node="$1"
    local peer="$2"
    inspect_node "${node}" | awk -v p="${peer}" '$1 == p { print $4 }' | head -1
}

# inspect_has_no_drift <node> — returns 0 when admin == runtime for every peer.
inspect_has_no_drift() {
    local node="$1"
    meshctl inspect "${node}" > /dev/null 2>&1
}

# admin_prefix_matches_runtime <admin_full_key> <runtime_prefix_with_ellipsis>
# Returns 0 iff the admin key (full 64-hex) starts with the runtime prefix
# (17 leading chars before the trailing `…`). Safe against truncation format
# changes — if there is no `…` we compare verbatim.
admin_prefix_matches_runtime() {
    local admin="$1"
    local runtime="$2"
    [[ -z "${admin}" || -z "${runtime}" ]] && return 1
    # Strip any trailing unicode horizontal ellipsis (U+2026) from runtime.
    local runtime_stripped="${runtime%…}"
    local n="${#runtime_stripped}"
    [[ "${admin:0:${n}}" == "${runtime_stripped}" ]]
}

# iface_is_up <container> <iface> — returns 0 when iface exists and is
# UP/UNKNOWN (WireGuard TUN devices report state UNKNOWN by default).
iface_is_up() {
    local container="$1"
    local iface="$2"
    docker exec "${container}" ip link show "${iface}" 2>/dev/null \
        | grep -qE 'state (UP|UNKNOWN)'
}

# ---------------------------------------------------------------------------
# Preflight checks
# ---------------------------------------------------------------------------
echo ""
echo "=== issue-92-rotation.sh — Endpoint Key Rotation Integration Test ==="
echo ""

if ! command -v docker > /dev/null 2>&1; then
    echo "ERROR: docker not found in PATH." >&2
    exit 2
fi
if ! docker info > /dev/null 2>&1; then
    echo "ERROR: docker not running or not accessible." >&2
    exit 2
fi
if ! docker image inspect "${NODE_IMAGE}" > /dev/null 2>&1; then
    echo "ERROR: image ${NODE_IMAGE} not found. Build it first:" >&2
    echo "  docker build -t ${NODE_IMAGE} ." >&2
    exit 2
fi

MESHCTL_BIN=""
if command -v mesh-ctl > /dev/null 2>&1; then
    MESHCTL_BIN="mesh-ctl"
elif [[ -x "${REPO_ROOT}/bin/mesh-ctl" ]]; then
    MESHCTL_BIN="${REPO_ROOT}/bin/mesh-ctl"
else
    echo "ERROR: mesh-ctl not in PATH and not at ${REPO_ROOT}/bin/mesh-ctl." >&2
    echo "  Install: go install ${REPO_ROOT}/cmd/mesh-ctl" >&2
    exit 2
fi
info "mesh-ctl: $(${MESHCTL_BIN} version 2>&1 || echo '(version unknown)')"

# ---------------------------------------------------------------------------
# Generate ephemeral topology file FIRST — needed by `mesh-ctl prepare`.
# ---------------------------------------------------------------------------
cat > "${TOPO_FILE}" <<EOF
overlay:
  space: 172.21.92.0/24
  physical_mtu: 1500
  awg_overhead: 80
  ranges:
    - name: masters
      cidr: 172.21.92.0/27
      balancer_ip: 172.21.92.1
    - name: endpoints
      cidr: 172.21.92.32/27
      balancer_ip: 172.21.92.33
    - name: clients
      cidr: 172.21.92.128/25

masters:
  - name: ${MASTER_RU_01}
    host: 127.0.0.1
    peer_host: ${MASTER_RU_01_BRIDGE}
    overlay_ip: ${MASTER_RU_01_OVERLAY}
    listen_port: 51820
    grpc_port: 19290
    endpoints:
      - ${ENDPOINT_US_01}

  - name: ${MASTER_RU_02}
    host: 127.0.0.1
    peer_host: ${MASTER_RU_02_BRIDGE}
    overlay_ip: ${MASTER_RU_02_OVERLAY}
    listen_port: 51820
    grpc_port: 29290
    endpoints:
      - ${ENDPOINT_US_01}

endpoints:
  - name: ${ENDPOINT_US_01}
    host: 127.0.0.1
    peer_host: ${ENDPOINT_US_01_BRIDGE}
    overlay_ip: ${ENDPOINT_US_01_OVERLAY}
    listen_port: 51820
    grpc_port: 39290

transport:
  pool: 10.92.0.0/16
  prefix_length: 30
EOF

# ---------------------------------------------------------------------------
# Pre-prepare nodes so each container boots with the correct bcrypt token
# hash. mesh-ctl prepare writes the hash to ${CTL_CONFIG_DIR}/nodes/<name>/mesh.token.
# We inject it via MESH_TOKEN_HASH env var — the container entrypoint bootstraps
# /config/mesh.token from it on first boot, matching production compose
# semantics documented in `mesh-ctl master prepare` output.
# ---------------------------------------------------------------------------
info "Pre-preparing nodes to generate auth tokens..."
# stdout suppressed (noisy help text) but stderr preserved so failures surface.
${MESHCTL_BIN} \
    --topology "${TOPO_FILE}" \
    --config-dir "${CTL_CONFIG_DIR}" \
    master prepare "${MASTER_RU_01}" > /dev/null || {
    echo "ERROR: mesh-ctl master prepare ${MASTER_RU_01} failed (see stderr above)" >&2
    exit 3
}
${MESHCTL_BIN} \
    --topology "${TOPO_FILE}" \
    --config-dir "${CTL_CONFIG_DIR}" \
    master prepare "${MASTER_RU_02}" > /dev/null || {
    echo "ERROR: mesh-ctl master prepare ${MASTER_RU_02} failed (see stderr above)" >&2
    exit 3
}
${MESHCTL_BIN} \
    --topology "${TOPO_FILE}" \
    --config-dir "${CTL_CONFIG_DIR}" \
    endpoint prepare "${ENDPOINT_US_01}" > /dev/null || {
    echo "ERROR: mesh-ctl endpoint prepare ${ENDPOINT_US_01} failed (see stderr above)" >&2
    exit 3
}

TOKEN_MASTER_RU_01=$(cat "${CTL_CONFIG_DIR}/nodes/${MASTER_RU_01}/mesh.token")
TOKEN_MASTER_RU_02=$(cat "${CTL_CONFIG_DIR}/nodes/${MASTER_RU_02}/mesh.token")
TOKEN_ENDPOINT_US_01=$(cat "${CTL_CONFIG_DIR}/nodes/${ENDPOINT_US_01}/mesh.token")

# Escape $ -> $$ for docker-compose variable interpolation. bcrypt hashes
# contain multiple $ characters ($2a$12$...) which compose would otherwise
# try to expand as env var references.
TOKEN_MASTER_RU_01_ESC="${TOKEN_MASTER_RU_01//\$/\$\$}"
TOKEN_MASTER_RU_02_ESC="${TOKEN_MASTER_RU_02//\$/\$\$}"
TOKEN_ENDPOINT_US_01_ESC="${TOKEN_ENDPOINT_US_01//\$/\$\$}"
info "Tokens resolved: ${MASTER_RU_01}/${MASTER_RU_02} + ${ENDPOINT_US_01}"

# ---------------------------------------------------------------------------
# Generate ephemeral compose file with pre-seeded MESH_TOKEN_HASH per node.
# Container entrypoint reads the env var on first boot and writes to
# /config/mesh.token — matches the production compose semantics generated by
# `mesh-ctl master/endpoint prepare`.
# ---------------------------------------------------------------------------
cat > "${COMPOSE_FILE}" <<EOF
# Auto-generated by issue-92-rotation.sh — do not edit.
services:
  ${MASTER_RU_01}:
    image: ${NODE_IMAGE}
    container_name: ${CTR_MASTER_RU_01}
    hostname: ${MASTER_RU_01}
    restart: "no"
    privileged: true
    environment:
      MESH_TOKEN_HASH: "${TOKEN_MASTER_RU_01_ESC}"
    networks:
      issue92:
        ipv4_address: ${MASTER_RU_01_BRIDGE}
    ports:
      - "19290:9090"
      - "19291:9091"
    entrypoint:
      - sh
      - -c
      - |
        [ -f /config/mesh.token ] || printf '%s' "\$\$MESH_TOKEN_HASH" > /config/mesh.token
        exec /usr/local/bin/awg-mesh-node \\
          --mode master \\
          --name ${MASTER_RU_01} \\
          --overlay-ip ${MASTER_RU_01_OVERLAY} \\
          --listen-port 51820

  ${MASTER_RU_02}:
    image: ${NODE_IMAGE}
    container_name: ${CTR_MASTER_RU_02}
    hostname: ${MASTER_RU_02}
    restart: "no"
    privileged: true
    environment:
      MESH_TOKEN_HASH: "${TOKEN_MASTER_RU_02_ESC}"
    networks:
      issue92:
        ipv4_address: ${MASTER_RU_02_BRIDGE}
    ports:
      - "29290:9090"
      - "29291:9091"
    entrypoint:
      - sh
      - -c
      - |
        [ -f /config/mesh.token ] || printf '%s' "\$\$MESH_TOKEN_HASH" > /config/mesh.token
        exec /usr/local/bin/awg-mesh-node \\
          --mode master \\
          --name ${MASTER_RU_02} \\
          --overlay-ip ${MASTER_RU_02_OVERLAY} \\
          --listen-port 51820

  ${ENDPOINT_US_01}:
    image: ${NODE_IMAGE}
    container_name: ${CTR_ENDPOINT_US_01}
    hostname: ${ENDPOINT_US_01}
    restart: "no"
    privileged: true
    environment:
      MESH_TOKEN_HASH: "${TOKEN_ENDPOINT_US_01_ESC}"
    networks:
      issue92:
        ipv4_address: ${ENDPOINT_US_01_BRIDGE}
    ports:
      - "39290:9090"
    entrypoint:
      - sh
      - -c
      - |
        [ -f /config/mesh.token ] || printf '%s' "\$\$MESH_TOKEN_HASH" > /config/mesh.token
        exec /usr/local/bin/awg-mesh-node \\
          --mode endpoint \\
          --name ${ENDPOINT_US_01} \\
          --overlay-ip ${ENDPOINT_US_01_OVERLAY} \\
          --listen-port 51820

networks:
  issue92:
    driver: bridge
    ipam:
      config:
        - subnet: 192.168.92.0/24
EOF

# ---------------------------------------------------------------------------
# R1: Boot the mesh
# ---------------------------------------------------------------------------
echo ""
echo "[R1] Booting 2-master + 1-endpoint mesh..."

compose_run up -d

info "Waiting for masters to report gRPC ready (up to ${GRPC_READY_TIMEOUT}s)..."
if wait_for_log "${CTR_MASTER_RU_01}" "gRPC server listening" "${GRPC_READY_TIMEOUT}"; then
    pass "R1a: ${MASTER_RU_01} gRPC ready"
else
    fail "R1a: ${MASTER_RU_01} did not become gRPC ready within ${GRPC_READY_TIMEOUT}s"
    docker logs "${CTR_MASTER_RU_01}" >&2 || true
fi

if wait_for_log "${CTR_MASTER_RU_02}" "gRPC server listening" "${GRPC_READY_TIMEOUT}"; then
    pass "R1b: ${MASTER_RU_02} gRPC ready"
else
    fail "R1b: ${MASTER_RU_02} did not become gRPC ready within ${GRPC_READY_TIMEOUT}s"
    docker logs "${CTR_MASTER_RU_02}" >&2 || true
fi

# Abort early if mesh did not start — remaining checks are meaningless.
if [[ "${FAILURES}" -gt 0 ]]; then
    echo ""
    echo "[abort] Mesh failed to start — skipping remaining checks."
    exit "${FAILURES}"
fi

# Allow initial settle.
sleep 3

# ---------------------------------------------------------------------------
# Init masters (prepare already done above; tokens pre-seeded in containers).
# ---------------------------------------------------------------------------
echo ""
echo "[init] Initialising masters and endpoint..."

info "Initialising masters..."
INIT_OUT_A=$(${MESHCTL_BIN} \
    --topology "${TOPO_FILE}" \
    --config-dir "${CTL_CONFIG_DIR}" \
    master init "${MASTER_RU_01}" 2>&1) && INIT_RC_A=0 || INIT_RC_A=$?
if [[ "${INIT_RC_A}" -ne 0 ]]; then
    fail "master init ${MASTER_RU_01} failed (rc=${INIT_RC_A}): ${INIT_OUT_A}"
fi

INIT_OUT_B=$(${MESHCTL_BIN} \
    --topology "${TOPO_FILE}" \
    --config-dir "${CTL_CONFIG_DIR}" \
    master init "${MASTER_RU_02}" 2>&1) && INIT_RC_B=0 || INIT_RC_B=$?
if [[ "${INIT_RC_B}" -ne 0 ]]; then
    fail "master init ${MASTER_RU_02} failed (rc=${INIT_RC_B}): ${INIT_OUT_B}"
fi

info "Initialising endpoint (first time)..."
INIT_EP=$(${MESHCTL_BIN} \
    --topology "${TOPO_FILE}" \
    --config-dir "${CTL_CONFIG_DIR}" \
    endpoint init "${ENDPOINT_US_01}" 2>&1) && INIT_EP_RC=0 || INIT_EP_RC=$?
if [[ "${INIT_EP_RC}" -ne 0 ]]; then
    fail "endpoint init ${ENDPOINT_US_01} (initial) failed (rc=${INIT_EP_RC}): ${INIT_EP}"
    echo ""
    echo "[abort] Initial endpoint init failed — cannot capture old key."
    exit "${FAILURES}"
fi
info "Initial endpoint init output:"
echo "${INIT_EP}" | sed 's/^/    /'

sleep 2

# ---------------------------------------------------------------------------
# R2: Baseline — admin-state pubkey exists and both masters see the endpoint
#     as a registered peer via `mesh-ctl inspect`. This is the state the #117
#     tier-3 rotation fix needs to rotate from.
# ---------------------------------------------------------------------------
echo ""
echo "[R2] Verifying baseline state after initial endpoint init..."

BASELINE_ADMIN=$(admin_pubkey_of "${ENDPOINT_US_01}")
if [[ -z "${BASELINE_ADMIN}" || "${#BASELINE_ADMIN}" -lt 32 ]]; then
    fail "R2: admin-state pubkey for ${ENDPOINT_US_01} missing or malformed (len=${#BASELINE_ADMIN})"
    echo "[abort] Baseline setup is broken — aborting."
    exit "${FAILURES}"
fi
info "Baseline admin pubkey for ${ENDPOINT_US_01}: ${BASELINE_ADMIN:0:8}..."

BASELINE_RT_RU_01=$(inspect_runtime_prefix_for "${MASTER_RU_01}" "${ENDPOINT_US_01}")
BASELINE_RT_RU_02=$(inspect_runtime_prefix_for "${MASTER_RU_02}" "${ENDPOINT_US_01}")

if [[ -z "${BASELINE_RT_RU_01}" ]]; then
    fail "R2: ${MASTER_RU_01} has no runtime peer entry for ${ENDPOINT_US_01}"
fi
if [[ -z "${BASELINE_RT_RU_02}" ]]; then
    fail "R2: ${MASTER_RU_02} has no runtime peer entry for ${ENDPOINT_US_01}"
fi

if [[ "${FAILURES}" -gt 0 ]]; then
    echo "[abort] Baseline inconsistent — aborting before rotation."
    exit "${FAILURES}"
fi

pass "R2: baseline state healthy — admin pubkey set, both masters hold peer"

# ---------------------------------------------------------------------------
# R3: `mesh-ctl rotate --tier 3` — this is the PRIMARY #117 assertion. Before
#     the fix, this call returned `apply params: ... uapi errno=-22` (EINVAL)
#     because pkg/wg/uapi.go::writeConfig emitted s3=/s4= keys that
#     amneziawg-go v1.0.4 rejects via its UAPI default branch. After PR #65 /
#     commit 698e912 the call completes cleanly.
# ---------------------------------------------------------------------------
echo ""
echo "[R3] Rotating tier-3 params (#117 primary assertion — no uapi errno=-22)..."

ROTATE_OUT=$(meshctl rotate --tier 3 --endpoint "${ENDPOINT_US_01}" 2>&1) \
    && ROTATE_RC=0 || ROTATE_RC=$?

info "Rotation output:"
echo "${ROTATE_OUT}" | sed 's/^/    /'

if [[ "${ROTATE_RC}" -ne 0 ]]; then
    fail "R3: mesh-ctl rotate --tier 3 exited non-zero (rc=${ROTATE_RC})"
    echo "[abort] #117 regression — tier-3 rotation failed."
    exit "${FAILURES}"
fi
pass "R3: mesh-ctl rotate --tier 3 exited 0"

# Guard: the specific errno string must NOT appear anywhere in the output —
# that's the #117 regression marker.
if echo "${ROTATE_OUT}" | grep -qF "uapi errno=-22"; then
    fail "R3a: #117 REGRESSION — 'uapi errno=-22' appeared in rotation output"
    exit "${FAILURES}"
else
    pass "R3a: no 'uapi errno=-22' in rotation output — #117 fix holds"
fi

# Each bound master must report "tier 3 rotation succeeded" on stdout.
for m in "${MASTER_RU_01}" "${MASTER_RU_02}"; do
    if echo "${ROTATE_OUT}" | grep -qF "${m}: tier 3 rotation succeeded"; then
        pass "R3b: ${m} reported tier 3 rotation succeeded"
    else
        fail "R3b: ${m} did NOT report tier 3 rotation succeeded"
    fi
done

# ---------------------------------------------------------------------------
# R4: Post-rotation control-plane + data-plane liveness. Masters must remain
#     gRPC-reachable and the amneziawg-go tunnel interface must still be up.
#     A hard failure in tier-3 would leave the interface down or make the
#     gRPC server unresponsive — this check catches those regressions.
# ---------------------------------------------------------------------------
echo ""
echo "[R4] Verifying masters remain healthy after rotation..."

for m in "${MASTER_RU_01}" "${MASTER_RU_02}"; do
    ctr="${COMPOSE_PROJECT}-${m}"
    if iface_is_up "${ctr}" "${MASTER_IFACE_EP_US_01}"; then
        pass "R4: ${m} ${MASTER_IFACE_EP_US_01} interface is still up"
    else
        fail "R4: ${m} ${MASTER_IFACE_EP_US_01} interface is down after rotation"
        docker exec "${ctr}" ip link show "${MASTER_IFACE_EP_US_01}" 2>&1 | sed 's/^/    /' || true
    fi
done

# `mesh-ctl inspect` must still respond with structured data — if gRPC has
# crashed or transport state is corrupt, inspect will fail non-zero OR return
# empty. We tolerate pre-existing disk_runtime_diverge drift (tracked as #125)
# by only checking the command returns a row for the peer, not exit code.
for m in "${MASTER_RU_01}" "${MASTER_RU_02}"; do
    rt=$(inspect_runtime_prefix_for "${m}" "${ENDPOINT_US_01}")
    if [[ -n "${rt}" ]]; then
        pass "R4a: ${m} inspect reports runtime peer for ${ENDPOINT_US_01}"
    else
        fail "R4a: ${m} inspect returned no peer row for ${ENDPOINT_US_01}"
    fi
done

# ---------------------------------------------------------------------------
# R5: Control plane still intact — a second `mesh-ctl endpoint init` exits 0.
#     This exercises the full UpdateTunnelPeer propagation path against post-
#     rotation master state; any gRPC/transport regression would surface here.
# ---------------------------------------------------------------------------
echo ""
echo "[R5] Re-running endpoint init against post-rotation masters..."

POST_OUT=$(meshctl endpoint init "${ENDPOINT_US_01}" 2>&1) \
    && POST_RC=0 || POST_RC=$?
info "endpoint init output:"
echo "${POST_OUT}" | sed 's/^/    /'

if [[ "${POST_RC}" -eq 0 ]]; then
    pass "R5: post-rotation endpoint init exited 0 — control plane intact"
else
    fail "R5: post-rotation endpoint init exited ${POST_RC} — control plane broken"
fi

# ---------------------------------------------------------------------------
# R6: Actual keypair rotation via `mesh-ctl rotate --tier 3` (engram #125, v1.12).
#     After the tier-3 rotation fix the keypair is genuinely rotated end-to-end:
#     endpoint persists new privKey, every master swaps Remove(old)+Add(new)
#     via UpdateTunnelPeer, admin-state pubkey updated atomically.
# ---------------------------------------------------------------------------
echo ""
echo "[R6] Rotating endpoint keypair via mesh-ctl rotate --tier 3..."

# Capture admin-state BEFORE rotation — this is the oldPub reference.
BEFORE_ADMIN=$(admin_pubkey_of "${ENDPOINT_US_01}")
info "admin pubkey before rotation: ${BEFORE_ADMIN:0:8}..."

ROTATE_OUT=$(meshctl rotate --tier 3 --endpoint "${ENDPOINT_US_01}" 2>&1) \
    && ROTATE_RC=0 || ROTATE_RC=$?
info "rotation output:"
echo "${ROTATE_OUT}" | sed 's/^/    /'

if [[ "${ROTATE_RC}" -ne 0 ]]; then
    fail "R6: mesh-ctl rotate --tier 3 exited non-zero (rc=${ROTATE_RC})"
    echo "[abort] tier-3 rotation failed — cannot verify keypair-rotation assertions."
    exit "${FAILURES}"
fi
pass "R6: mesh-ctl rotate --tier 3 exited 0"

# R6a: admin-state pubkey on disk DIFFERS from pre-rotation value (keypair changed).
AFTER_ADMIN=$(admin_pubkey_of "${ENDPOINT_US_01}")
if [[ -z "${AFTER_ADMIN}" || "${#AFTER_ADMIN}" -lt 32 ]]; then
    fail "R6a: admin-state pubkey missing/malformed after rotation"
elif [[ "${AFTER_ADMIN}" == "${BEFORE_ADMIN}" ]]; then
    fail "R6a: admin-state pubkey did NOT change after rotation (still ${AFTER_ADMIN:0:8}...)"
else
    pass "R6a: admin-state pubkey CHANGED — ${BEFORE_ADMIN:0:8}... → ${AFTER_ADMIN:0:8}..."
fi

# R6b: both masters show the NEW pubkey in mesh-ctl inspect runtime column.
for master_name in "${MASTER_RU_01}" "${MASTER_RU_02}"; do
    rt=$(inspect_runtime_prefix_for "${master_name}" "${ENDPOINT_US_01}")
    if admin_prefix_matches_runtime "${AFTER_ADMIN}" "${rt}"; then
        pass "R6b: ${master_name} runtime key matches new admin key (${AFTER_ADMIN:0:8}...)"
    else
        fail "R6b: ${master_name} runtime key does NOT match new admin key"
        info "  admin new: ${AFTER_ADMIN:-<empty>}"
        info "  runtime:   ${rt:-<empty>}"
    fi
done

# R6c: OLD pubkey is absent from runtime peer list on every master.
OLD_PREFIX="${BEFORE_ADMIN:0:17}"
for master_name in "${MASTER_RU_01}" "${MASTER_RU_02}"; do
    if inspect_node "${master_name}" | grep -qF "${OLD_PREFIX}"; then
        fail "R6c: old pubkey prefix ${OLD_PREFIX}… still present on ${master_name}"
    else
        pass "R6c: old pubkey absent from ${master_name} runtime"
    fi
done

# R6d: inspect reports zero drift on both masters post-rotation.
for master_name in "${MASTER_RU_01}" "${MASTER_RU_02}"; do
    if inspect_has_no_drift "${master_name}"; then
        pass "R6d: ${master_name} reports zero drift (admin == disk == runtime)"
    else
        fail "R6d: ${master_name} still reports drift after rotation"
        inspect_node "${master_name}" | sed 's/^/    /'
    fi
done

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo ""
echo "=================================================================="
if [[ "${FAILURES}" -eq 0 ]]; then
    echo -e " issue-92-rotation: ${GREEN}PASS${RESET} (${PASSES} check(s) passed)"
else
    echo -e " issue-92-rotation: ${RED}FAIL${RESET} — ${FAILURES} failure(s), ${PASSES} pass(es)"
fi
echo "=================================================================="
echo ""

exit "${FAILURES}"
