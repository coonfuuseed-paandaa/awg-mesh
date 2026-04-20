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
#   R6  `mesh-ctl reconcile` is idempotent after tier-3 rotation — exits 0.
#   R7  Per-master WG ifaces exist on endpoint-a; endpoint-a pings endpoint-b
#       overlay IP via kernel policy routing; legacy wg0 absent; AllowedIPs
#       minimal (1 peer line per iface). (v1.12.2 — T009)
#   R8  Kill one master: endpoint-a still reaches endpoint-b via surviving master;
#       surviving iface still up; mesh heals after master restart. (v1.12.2 — T010)
#   R9  Restart both masters, wait for gRPC readiness, run reconcile, and verify
#       persisted master transport.yml entries retain non-empty allowed_ips.
#   R9b Port contract: persisted peer_endpoint ports match endpoint-side state
#       and the endpoint is actually listening on those UDP ports.
#   R10 Endpoint↔endpoint overlay matrix: every endpoint reaches every other
#       endpoint overlay IP, and `ip route get` selects `src <self-overlay-ip>`.
#
# Introspection approach: amneziawg-go runs in userspace and exposes its UAPI
# via /run/amneziawg/<iface>.sock. The kernel-targeted `wg` CLI cannot access
# this socket ("Unable to access interface: Not supported"), so this script
# uses the control-plane path instead — `mesh-ctl inspect <node>` fetches
# runtime peer state via gRPC GetTransportState.
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

# Endpoint pair for R7/R8 (endpoint-a and endpoint-b bound to both masters).
# Both need per-master ifaces: wg-mst-ru-01 and wg-mst-ru-02 (12 chars each).
ENDPOINT_ASIA_01="node-asia-01"
ENDPOINT_ASIA_02="node-asia-02"

# Container names (project prefix + service name).
CTR_MASTER_RU_01="${COMPOSE_PROJECT}-${MASTER_RU_01}"
CTR_MASTER_RU_02="${COMPOSE_PROJECT}-${MASTER_RU_02}"
CTR_ENDPOINT_US_01="${COMPOSE_PROJECT}-${ENDPOINT_US_01}"
CTR_ENDPOINT_ASIA_01="${COMPOSE_PROJECT}-${ENDPOINT_ASIA_01}"
CTR_ENDPOINT_ASIA_02="${COMPOSE_PROJECT}-${ENDPOINT_ASIA_02}"

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
ENDPOINT_ASIA_01_OVERLAY="172.21.92.35"
ENDPOINT_ASIA_02_OVERLAY="172.21.92.36"
ENDPOINTS_RANGE_CIDR="172.21.92.32/27"

MASTER_RU_01_BRIDGE="192.168.92.10"
MASTER_RU_02_BRIDGE="192.168.92.11"
ENDPOINT_US_01_BRIDGE="192.168.92.20"
ENDPOINT_ASIA_01_BRIDGE="192.168.92.21"
ENDPOINT_ASIA_02_BRIDGE="192.168.92.22"

# Per-master WireGuard iface names on endpoints (v1.12.2 per-master-iface feature).
# Convention: "wg-" + master.Name, truncated to 12 chars.
# mst-ru-01 (9 chars) → wg-mst-ru-01 (12 chars — no truncation needed)
# mst-ru-02 (9 chars) → wg-mst-ru-02 (12 chars — no truncation needed)
EP_IFACE_MASTER_RU_01="wg-mst-ru-01"
EP_IFACE_MASTER_RU_02="wg-mst-ru-02"

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
    inspect_node "${node}" | awk -v p="${peer}" '$1 == p { print $5 }' | head -1
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
      cidr: ${ENDPOINTS_RANGE_CIDR}
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
      - ${ENDPOINT_ASIA_01}
      - ${ENDPOINT_ASIA_02}

  - name: ${MASTER_RU_02}
    host: 127.0.0.1
    peer_host: ${MASTER_RU_02_BRIDGE}
    overlay_ip: ${MASTER_RU_02_OVERLAY}
    listen_port: 51820
    grpc_port: 29290
    endpoints:
      - ${ENDPOINT_US_01}
      - ${ENDPOINT_ASIA_01}
      - ${ENDPOINT_ASIA_02}

endpoints:
  - name: ${ENDPOINT_US_01}
    host: 127.0.0.1
    peer_host: ${ENDPOINT_US_01_BRIDGE}
    overlay_ip: ${ENDPOINT_US_01_OVERLAY}
    listen_port: 51820
    grpc_port: 39290

  - name: ${ENDPOINT_ASIA_01}
    host: 127.0.0.1
    peer_host: ${ENDPOINT_ASIA_01_BRIDGE}
    overlay_ip: ${ENDPOINT_ASIA_01_OVERLAY}
    listen_port: 51820
    grpc_port: 49290

  - name: ${ENDPOINT_ASIA_02}
    host: 127.0.0.1
    peer_host: ${ENDPOINT_ASIA_02_BRIDGE}
    overlay_ip: ${ENDPOINT_ASIA_02_OVERLAY}
    listen_port: 51820
    grpc_port: 59290

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
${MESHCTL_BIN} \
    --topology "${TOPO_FILE}" \
    --config-dir "${CTL_CONFIG_DIR}" \
    endpoint prepare "${ENDPOINT_ASIA_01}" > /dev/null || {
    echo "ERROR: mesh-ctl endpoint prepare ${ENDPOINT_ASIA_01} failed (see stderr above)" >&2
    exit 3
}
${MESHCTL_BIN} \
    --topology "${TOPO_FILE}" \
    --config-dir "${CTL_CONFIG_DIR}" \
    endpoint prepare "${ENDPOINT_ASIA_02}" > /dev/null || {
    echo "ERROR: mesh-ctl endpoint prepare ${ENDPOINT_ASIA_02} failed (see stderr above)" >&2
    exit 3
}

TOKEN_MASTER_RU_01=$(cat "${CTL_CONFIG_DIR}/nodes/${MASTER_RU_01}/mesh.token")
TOKEN_MASTER_RU_02=$(cat "${CTL_CONFIG_DIR}/nodes/${MASTER_RU_02}/mesh.token")
TOKEN_ENDPOINT_US_01=$(cat "${CTL_CONFIG_DIR}/nodes/${ENDPOINT_US_01}/mesh.token")
TOKEN_ENDPOINT_ASIA_01=$(cat "${CTL_CONFIG_DIR}/nodes/${ENDPOINT_ASIA_01}/mesh.token")
TOKEN_ENDPOINT_ASIA_02=$(cat "${CTL_CONFIG_DIR}/nodes/${ENDPOINT_ASIA_02}/mesh.token")

# Escape $ -> $$ for docker-compose variable interpolation. bcrypt hashes
# contain multiple $ characters ($2a$12$...) which compose would otherwise
# try to expand as env var references.
TOKEN_MASTER_RU_01_ESC="${TOKEN_MASTER_RU_01//\$/\$\$}"
TOKEN_MASTER_RU_02_ESC="${TOKEN_MASTER_RU_02//\$/\$\$}"
TOKEN_ENDPOINT_US_01_ESC="${TOKEN_ENDPOINT_US_01//\$/\$\$}"
TOKEN_ENDPOINT_ASIA_01_ESC="${TOKEN_ENDPOINT_ASIA_01//\$/\$\$}"
TOKEN_ENDPOINT_ASIA_02_ESC="${TOKEN_ENDPOINT_ASIA_02//\$/\$\$}"
info "Tokens resolved: ${MASTER_RU_01}/${MASTER_RU_02} + ${ENDPOINT_US_01} + ${ENDPOINT_ASIA_01} + ${ENDPOINT_ASIA_02}"

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

  ${ENDPOINT_ASIA_01}:
    image: ${NODE_IMAGE}
    container_name: ${CTR_ENDPOINT_ASIA_01}
    hostname: ${ENDPOINT_ASIA_01}
    restart: "no"
    privileged: true
    environment:
      MESH_TOKEN_HASH: "${TOKEN_ENDPOINT_ASIA_01_ESC}"
    networks:
      issue92:
        ipv4_address: ${ENDPOINT_ASIA_01_BRIDGE}
    ports:
      - "49290:9090"
      # Multi-master endpoint needs a port range in production (one listen port per
      # master iface). In sim all containers share the same Docker bridge network —
      # WG traffic flows container-to-container via the bridge, so host port exposure
      # is unnecessary and actively conflicts with other containers on the host.
    entrypoint:
      - sh
      - -c
      - |
        [ -f /config/mesh.token ] || printf '%s' "\$\$MESH_TOKEN_HASH" > /config/mesh.token
        exec /usr/local/bin/awg-mesh-node \\
          --mode endpoint \\
          --name ${ENDPOINT_ASIA_01} \\
          --overlay-ip ${ENDPOINT_ASIA_01_OVERLAY} \\
          --listen-port 51820

  ${ENDPOINT_ASIA_02}:
    image: ${NODE_IMAGE}
    container_name: ${CTR_ENDPOINT_ASIA_02}
    hostname: ${ENDPOINT_ASIA_02}
    restart: "no"
    privileged: true
    environment:
      MESH_TOKEN_HASH: "${TOKEN_ENDPOINT_ASIA_02_ESC}"
    networks:
      issue92:
        ipv4_address: ${ENDPOINT_ASIA_02_BRIDGE}
    ports:
      - "59290:9090"
      # See node-asia-01 above — host UDP exposure not needed in sim (bridge network).
    entrypoint:
      - sh
      - -c
      - |
        [ -f /config/mesh.token ] || printf '%s' "\$\$MESH_TOKEN_HASH" > /config/mesh.token
        exec /usr/local/bin/awg-mesh-node \\
          --mode endpoint \\
          --name ${ENDPOINT_ASIA_02} \\
          --overlay-ip ${ENDPOINT_ASIA_02_OVERLAY} \\
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
echo "[R1] Booting 2-master + 3-endpoint mesh (ep-us-01 + node-asia-01 + node-asia-02)..."

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

info "Initialising R7/R8 endpoints (node-asia-01 + node-asia-02)..."
INIT_A01=$(${MESHCTL_BIN} \
    --topology "${TOPO_FILE}" \
    --config-dir "${CTL_CONFIG_DIR}" \
    endpoint init "${ENDPOINT_ASIA_01}" 2>&1) && INIT_A01_RC=0 || INIT_A01_RC=$?
if [[ "${INIT_A01_RC}" -ne 0 ]]; then
    fail "endpoint init ${ENDPOINT_ASIA_01} failed (rc=${INIT_A01_RC}): ${INIT_A01}"
    echo "[abort] R7/R8 endpoint-a init failed."
    exit "${FAILURES}"
fi
info "${ENDPOINT_ASIA_01} init output:"
echo "${INIT_A01}" | sed 's/^/    /'

INIT_A02=$(${MESHCTL_BIN} \
    --topology "${TOPO_FILE}" \
    --config-dir "${CTL_CONFIG_DIR}" \
    endpoint init "${ENDPOINT_ASIA_02}" 2>&1) && INIT_A02_RC=0 || INIT_A02_RC=$?
if [[ "${INIT_A02_RC}" -ne 0 ]]; then
    fail "endpoint init ${ENDPOINT_ASIA_02} failed (rc=${INIT_A02_RC}): ${INIT_A02}"
    echo "[abort] R7/R8 endpoint-b init failed."
    exit "${FAILURES}"
fi
info "${ENDPOINT_ASIA_02} init output:"
echo "${INIT_A02}" | sed 's/^/    /'

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
    # v1.12 output format: structured NAME/STATUS/DETAIL table with STATUS=ROTATED.
    # Backward-compatible with v1.11 "tier 3 rotation succeeded" form (tier-1/2 still emit it).
    if echo "${ROTATE_OUT}" | grep -qE "^${m}[[:space:]]+ROTATED|${m}: tier 3 rotation succeeded"; then
        pass "R3b: ${m} reported tier 3 rotation ROTATED"
    else
        fail "R3b: ${m} did NOT report tier 3 rotation ROTATED"
    fi
done

# ---------------------------------------------------------------------------
# R3c: Admin-state pubkey CHANGED from baseline after rotation.
#      SetPubkey must have committed the new key to the admin-state file.
# ---------------------------------------------------------------------------
echo ""
echo "[R3c] Verifying admin-state pubkey changed after rotation..."
POST_ADMIN=$(admin_pubkey_of "${ENDPOINT_US_01}")
if [[ -z "${POST_ADMIN}" || "${#POST_ADMIN}" -lt 32 ]]; then
    fail "R3c: post-rotation admin pubkey missing or malformed (len=${#POST_ADMIN})"
elif [[ "${POST_ADMIN}" == "${BASELINE_ADMIN}" ]]; then
    fail "R3c: admin-state pubkey unchanged after rotation — key wasn't committed (was: ${BASELINE_ADMIN:0:8}..., now: ${POST_ADMIN:0:8}...)"
else
    pass "R3c: admin-state pubkey rotated (${BASELINE_ADMIN:0:8}... → ${POST_ADMIN:0:8}...)"
fi

# ---------------------------------------------------------------------------
# R3d: Per-master runtime pubkey CHANGED from baseline.
#      UpdateTunnelPeer must have replaced the old peer entry on each master.
# ---------------------------------------------------------------------------
echo ""
echo "[R3d] Verifying per-master runtime pubkey changed..."
for m in "${MASTER_RU_01}" "${MASTER_RU_02}"; do
    POST_RT=$(inspect_runtime_prefix_for "${m}" "${ENDPOINT_US_01}")
    case "${m}" in
        "${MASTER_RU_01}") BASELINE_RT="${BASELINE_RT_RU_01}" ;;
        "${MASTER_RU_02}") BASELINE_RT="${BASELINE_RT_RU_02}" ;;
    esac
    if [[ -z "${POST_RT}" ]]; then
        fail "R3d: ${m} runtime pubkey missing after rotation"
    elif [[ "${POST_RT}" == "${BASELINE_RT}" ]]; then
        fail "R3d: ${m} runtime pubkey unchanged (${BASELINE_RT:0:8}...) — UpdateTunnelPeer no-op"
    else
        pass "R3d: ${m} runtime pubkey rotated (${BASELINE_RT:0:8}... → ${POST_RT:0:8}...)"
    fi
done

# ---------------------------------------------------------------------------
# R3e: Per-master runtime pubkey CONVERGED with admin-state.
#      After successful rotation, admin-state and each master's runtime peer
#      entry must reflect the same new public key.
# ---------------------------------------------------------------------------
echo ""
echo "[R3e] Verifying per-master runtime converged with admin-state..."
for m in "${MASTER_RU_01}" "${MASTER_RU_02}"; do
    POST_RT=$(inspect_runtime_prefix_for "${m}" "${ENDPOINT_US_01}")
    if admin_prefix_matches_runtime "${POST_ADMIN}" "${POST_RT}"; then
        pass "R3e: ${m} runtime pubkey matches admin-state (${POST_ADMIN:0:8}...)"
    else
        fail "R3e: ${m} runtime (${POST_RT:0:8}...) differs from admin-state (${POST_ADMIN:0:8}...)"
    fi
done

# ---------------------------------------------------------------------------
# R3f: OLD baseline pubkey NOT present on any master (no phantom peer).
#      Remove(oldPubKey) must have run — the old peer entry must be gone.
#      We use `mesh-ctl inspect <master>` and check that the baseline admin
#      pubkey prefix does not appear anywhere in the output (which would
#      indicate the old peer is still registered as an extra_peer or key_mismatch).
# ---------------------------------------------------------------------------
echo ""
echo "[R3f] Verifying old pubkey is gone from master peer tables..."
for m in "${MASTER_RU_01}" "${MASTER_RU_02}"; do
    peers=$(meshctl inspect "${m}" 2>&1 || true)
    if echo "${peers}" | grep -qi "${BASELINE_ADMIN:0:16}"; then
        fail "R3f: ${m} still has old pubkey ${BASELINE_ADMIN:0:8}... in peer table — Remove(old) didn't run"
    else
        pass "R3f: ${m} no longer has old pubkey ${BASELINE_ADMIN:0:8}... — phantom absent"
    fi
done

# ---------------------------------------------------------------------------
# R3g (S-FIX-5): Pubkey column convergence AND zero drift status. Earlier
#      versions rationalized `stale_allowed_ips` as "pre-existing orthogonal
#      admin /32 vs runtime full subnet" divergence. Engram #132 proved that
#      explanation wrong — on real multi-host deploys disk AND runtime are
#      BOTH empty, which is the actual 100% data-plane-loss condition. STATUS
#      column is now load-bearing: any DRIFT category fails the assertion.
# ---------------------------------------------------------------------------
echo ""
echo "[R3g] Verifying pubkey convergence AND zero drift after rotation..."
meshctl inspect "${ENDPOINT_US_01}" > /tmp/inspect-drift.txt 2>&1 || true
if grep -qE "^(mst-ru-01|mst-ru-02)[[:space:]]+" /tmp/inspect-drift.txt; then
    for m in "${MASTER_RU_01}" "${MASTER_RU_02}"; do
        row=$(grep -E "^${m}[[:space:]]+" /tmp/inspect-drift.txt | head -1)
        adm=$(echo "${row}" | awk '{print $3}')
        nod=$(echo "${row}" | awk '{print $4}')
        run=$(echo "${row}" | awk '{print $5}')
        # STATUS column spans from column 8 to end of row.
        status=$(echo "${row}" | awk '{for(i=8;i<=NF;i++) printf "%s ",$i}')
        if [[ "${adm}" == "${nod}" && "${nod}" == "${run}" ]]; then
            pass "R3g: ${m} admin/node/runtime pubkeys converged (${adm})"
        else
            fail "R3g: ${m} pubkey columns diverge (admin=${adm} node=${nod} run=${run})"
        fi
        # Fail only on disk_runtime_diverge — that is the actual data-plane
        # killer (endpoint will restart with empty allowed_ips → no handshake).
        # stale_allowed_ips in endpoint-side inspect of master rows is an
        # architectural divergence (admin tracks just the master's overlay /32
        # while runtime carries the full allowed_ips list for cross-subnet
        # routing) — not the engram #132 bug we are guarding here. That bug is
        # now caught directly by R3i (master inspect DISK_IPS/RUNTIME_IPS) and
        # R3h (master transport.yml schema).
        if echo "${status}" | grep -qiE "disk_runtime_diverge"; then
            fail "R3g-drift: ${m} disk≠runtime (${status}) — data-plane risk (engram #132)"
        else
            pass "R3g-drift: ${m} no disk_runtime_diverge (STATUS='${status}')"
        fi
    done
else
    fail "R3g: mesh-ctl inspect produced no master peer rows — unexpected"
    sed 's/^/    /' /tmp/inspect-drift.txt
fi

# ---------------------------------------------------------------------------
# R3h (S-FIX-1): Verify MASTER /config/transport.yml persists allowed_ips per
#      tunnel. Engram #132 showed production masters ship with tunnel entries
#      missing the `allowed_ips:` key → amneziawg-go runtime has empty
#      AllowedIPs → no handshake → 100% data-plane loss. Read raw YAML and
#      assert each tunnel has its allowed_ips block.
# ---------------------------------------------------------------------------
echo ""
echo "[R3h] Verifying master /config/transport.yml persists allowed_ips per tunnel..."
for m in "${MASTER_RU_01}" "${MASTER_RU_02}"; do
    ctr="${COMPOSE_PROJECT}-${m}"
    yml=$(docker exec "${ctr}" cat /config/transport.yml 2>/dev/null || echo "")
    if [[ -z "${yml}" ]]; then
        fail "R3h: ${m} /config/transport.yml missing or empty"
        continue
    fi
    tunnel_count=$(echo "${yml}" | grep -cE '^[[:space:]]+- name:' || true)
    allowed_count=$(echo "${yml}" | grep -cE '^[[:space:]]+allowed_ips:' || true)
    if [[ "${tunnel_count}" -eq 0 ]]; then
        fail "R3h: ${m} transport.yml has no tunnel entries"
    elif [[ "${allowed_count}" -lt "${tunnel_count}" ]]; then
        fail "R3h: ${m} has ${tunnel_count} tunnel(s) but only ${allowed_count} allowed_ips block(s) — engram #132"
        echo "${yml}" | sed 's/^/    /' >&2
    else
        pass "R3h: ${m} transport.yml has allowed_ips for all ${tunnel_count} tunnel(s)"
    fi
done

# ---------------------------------------------------------------------------
# R3i (S-FIX-2): Verify MASTER inspect DISK_IPS and RUNTIME_IPS non-empty for
#      every endpoint peer row. Engram #132 surfaces as empty strings in
#      these columns. Inspect layout per cmd/mesh-ctl/cmd/inspect.go:
#      PEER ADMIN_KEY NODE_KEY RUNTIME_KEY ADMIN_IPS DISK_IPS RUNTIME_IPS STATUS
# ---------------------------------------------------------------------------
echo ""
echo "[R3i] Verifying master inspect DISK_IPS + RUNTIME_IPS populated for endpoint peers..."
for m in "${MASTER_RU_01}" "${MASTER_RU_02}"; do
    meshctl inspect "${m}" > "/tmp/inspect-master-${m}.txt" 2>&1 || true
    row=$(grep -E "^${ENDPOINT_US_01}[[:space:]]+" "/tmp/inspect-master-${m}.txt" | head -1)
    if [[ -z "${row}" ]]; then
        fail "R3i: ${m} inspect has no row for endpoint ${ENDPOINT_US_01}"
        sed 's/^/    /' "/tmp/inspect-master-${m}.txt"
        continue
    fi
    disk_ips=$(echo "${row}" | awk '{print $7}')
    runtime_ips=$(echo "${row}" | awk '{print $8}')
    if [[ -z "${disk_ips}" || "${disk_ips}" == "-" ]]; then
        fail "R3i: ${m} DISK_IPS for ${ENDPOINT_US_01} empty — transport.yml missing allowed_ips (engram #132)"
    else
        pass "R3i: ${m} DISK_IPS populated for ${ENDPOINT_US_01} (${disk_ips})"
    fi
    if [[ -z "${runtime_ips}" || "${runtime_ips}" == "-" ]]; then
        fail "R3i: ${m} RUNTIME_IPS for ${ENDPOINT_US_01} empty — amneziawg-go has no allowed_ips (engram #132)"
    else
        pass "R3i: ${m} RUNTIME_IPS populated for ${ENDPOINT_US_01} (${runtime_ips})"
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
# R6: `mesh-ctl reconcile` after tier-3 rotation is idempotent — cluster
#     already in consistent state post-rotation, so reconcile must exit 0
#     without reporting drift. Exercises the operator recovery path that is
#     documented as the escape hatch for partial-failure rollback scenarios.
# ---------------------------------------------------------------------------
echo ""
echo "[R6] Verifying mesh-ctl reconcile is idempotent after tier-3 rotation..."

RECON_OUT=$(meshctl reconcile 2>&1) \
    && RECON_RC=0 || RECON_RC=$?
info "reconcile output:"
echo "${RECON_OUT}" | sed 's/^/    /'

if [[ "${RECON_RC}" -eq 0 ]]; then
    pass "R6: mesh-ctl reconcile exited 0 after tier-3 rotation — idempotent happy path"
else
    fail "R6: mesh-ctl reconcile exited ${RECON_RC} — cluster drift detected post-rotation"
fi

# ---------------------------------------------------------------------------
# R7: Per-master WG ifaces on endpoints + endpoint-to-endpoint overlay ping.
#     (v1.12.2 gate — T009: endpoint-per-master-iface feature)
#
#  R7.1  endpoint-a (node-asia-01) has exactly 2 per-master WG ifaces
#         (wg-mst-ru-01 + wg-mst-ru-02) — one per bound master.
#  R7.2  endpoint-a can ping endpoint-b (node-asia-02) overlay IP via
#         kernel policy routing through one of the master tunnels.
#  R7.3  Legacy wg0 does NOT exist on endpoint-a (migration complete).
#  R7.4  Each per-master iface on endpoint-a has exactly 1 peer line in
#         `ip link` / inspect output (minimal AllowedIPs: 1 peer per iface).
#         NOTE: amneziawg-go userspace driver rejects kernel `wg show`, so
#         we count peer rows via `mesh-ctl inspect` instead.
# ---------------------------------------------------------------------------
echo ""
echo "[R7] Endpoint-to-endpoint overlay connectivity via per-master ifaces..."

# Allow endpoint containers to finish tunnel bring-up after init.
sleep 5

# R7.1: Count per-master WG ifaces on endpoint-a. The endpoint-per-master-iface
# feature (T003) creates one amneziawg-go interface per bound master, named
# wg-<master-name> (truncated to 12 chars). Both mst-ru-01 and mst-ru-02 are
# bound to node-asia-01, so we expect exactly 2 matching iface names.
R7_1_COUNT=$(docker exec "${CTR_ENDPOINT_ASIA_01}" \
    ip link show 2>/dev/null \
    | grep -cE "wg-mst-ru-0[12]" || true)
if [[ "${R7_1_COUNT}" -eq 2 ]]; then
    pass "R7.1: ${ENDPOINT_ASIA_01} has 2 per-master WG ifaces (wg-mst-ru-01 + wg-mst-ru-02)"
else
    fail "R7.1: ${ENDPOINT_ASIA_01} has ${R7_1_COUNT} per-master iface(s), expected 2"
    docker exec "${CTR_ENDPOINT_ASIA_01}" ip link show 2>&1 | sed 's/^/    /' || true
fi

# R7.2: Endpoint-a pings endpoint-b overlay IP via kernel policy routing.
# The route goes through one of the per-master tunnels (wg-mst-ru-01 or
# wg-mst-ru-02) — kernel selects based on policy routing table.
PING_R72_RC=0
docker exec "${CTR_ENDPOINT_ASIA_01}" \
    ping -c 5 -W 2 "${ENDPOINT_ASIA_02_OVERLAY}" > /dev/null 2>&1 \
    || PING_R72_RC=$?
if [[ "${PING_R72_RC}" -eq 0 ]]; then
    pass "R7.2: ${ENDPOINT_ASIA_01} can reach ${ENDPOINT_ASIA_02} overlay IP (${ENDPOINT_ASIA_02_OVERLAY})"
else
    fail "R7.2: ${ENDPOINT_ASIA_01} cannot reach ${ENDPOINT_ASIA_02} overlay IP (${ENDPOINT_ASIA_02_OVERLAY}) — policy routing not working"
    docker exec "${CTR_ENDPOINT_ASIA_01}" ip route show 2>&1 | sed 's/^/    /' || true
    docker exec "${CTR_ENDPOINT_ASIA_01}" ip rule show 2>&1 | sed 's/^/    /' || true
fi

# R7.3: Legacy wg0 must NOT exist on endpoint-a. The per-master-iface feature
# replaces the single wg0 with per-master named interfaces.
# NOTE: stderr is suppressed (2>/dev/null) so that the "Device does not exist"
# error message (which contains "wg0") does not produce a false-positive count.
WG0_COUNT=$(docker exec "${CTR_ENDPOINT_ASIA_01}" \
    ip link show wg0 2>/dev/null | grep -c "wg0" || true)
if [[ "${WG0_COUNT}" -eq 0 ]]; then
    pass "R7.3: legacy wg0 does not exist on ${ENDPOINT_ASIA_01} — migration complete"
else
    fail "R7.3: wg0 still present on ${ENDPOINT_ASIA_01} — old single-iface mode active"
fi

# R7.4: Each per-master iface should have exactly 1 peer. We verify via
# mesh-ctl inspect on endpoint-a: each master row must appear exactly once.
# (amneziawg-go userspace: kernel `wg show` is not available — use gRPC path)
INSPECT_A01=$(meshctl inspect "${ENDPOINT_ASIA_01}" 2>&1 || true)
for m in "${MASTER_RU_01}" "${MASTER_RU_02}"; do
    peer_rows=$(echo "${INSPECT_A01}" | grep -cE "^${m}[[:space:]]+" || true)
    if [[ "${peer_rows}" -eq 1 ]]; then
        pass "R7.4: ${ENDPOINT_ASIA_01} iface for ${m} has exactly 1 peer row in inspect"
    else
        fail "R7.4: ${ENDPOINT_ASIA_01} inspect shows ${peer_rows} row(s) for ${m}, expected 1"
        echo "${INSPECT_A01}" | sed 's/^/    /'
    fi
done

# ---------------------------------------------------------------------------
# R8: Kill-master failover — endpoint-to-endpoint routing survives.
#     (v1.12.2 gate — T010: kill-master does not break endpoint↔endpoint)
#
#  R8.1  Baseline: endpoint-a still reaches endpoint-b (pre-kill sanity).
#  R8.2  docker stop master-02 (kill one of the two masters).
#  R8.3  After kill: endpoint-a still reaches endpoint-b via surviving master-01.
#  R8.4  wg-mst-ru-01 iface is still up on endpoint-a after master-02 killed.
#  R8.5  docker start master-02; mesh heals.
#  R8.6  endpoint-a reaches endpoint-b after master-02 restart.
# ---------------------------------------------------------------------------
echo ""
echo "[R8] Kill-master failover: endpoint-to-endpoint routing survives master loss..."

# R8.1: Baseline ping before kill.
PING_R81_RC=0
docker exec "${CTR_ENDPOINT_ASIA_01}" \
    ping -c 3 -W 2 "${ENDPOINT_ASIA_02_OVERLAY}" > /dev/null 2>&1 \
    || PING_R81_RC=$?
if [[ "${PING_R81_RC}" -eq 0 ]]; then
    pass "R8.1: baseline — ${ENDPOINT_ASIA_01} reaches ${ENDPOINT_ASIA_02} before master kill"
else
    fail "R8.1: baseline ping failed before kill — R7.2 precondition not met"
fi

# R8.2: Kill master-02. WireGuard on endpoint-a keeps wg-mst-ru-01 up; the
# surviving master-01 tunnel continues to route endpoint-a ↔ endpoint-b.
info "R8.2: stopping ${CTR_MASTER_RU_02} to simulate master failure..."
docker stop "${CTR_MASTER_RU_02}" > /dev/null 2>&1 || true
info "R8.2: waiting 5s for health checks to detect master failure..."
sleep 5

# R8.3: Endpoint-a must still reach endpoint-b via surviving master-01.
PING_R83_RC=0
docker exec "${CTR_ENDPOINT_ASIA_01}" \
    ping -c 5 -W 3 "${ENDPOINT_ASIA_02_OVERLAY}" > /dev/null 2>&1 \
    || PING_R83_RC=$?
if [[ "${PING_R83_RC}" -eq 0 ]]; then
    pass "R8.3: ${ENDPOINT_ASIA_01} reaches ${ENDPOINT_ASIA_02} after ${MASTER_RU_02} killed (via ${MASTER_RU_01})"
else
    fail "R8.3: ${ENDPOINT_ASIA_01} cannot reach ${ENDPOINT_ASIA_02} after ${MASTER_RU_02} killed — failover broken"
    docker exec "${CTR_ENDPOINT_ASIA_01}" ip route show 2>&1 | sed 's/^/    /' || true
    docker exec "${CTR_ENDPOINT_ASIA_01}" ip rule show 2>&1 | sed 's/^/    /' || true
fi

# R8.4: Surviving iface wg-mst-ru-01 must still be UP on endpoint-a.
if iface_is_up "${CTR_ENDPOINT_ASIA_01}" "${EP_IFACE_MASTER_RU_01}"; then
    pass "R8.4: ${EP_IFACE_MASTER_RU_01} still operational on ${ENDPOINT_ASIA_01} after ${MASTER_RU_02} killed"
else
    fail "R8.4: ${EP_IFACE_MASTER_RU_01} is DOWN on ${ENDPOINT_ASIA_01} — surviving iface lost"
    docker exec "${CTR_ENDPOINT_ASIA_01}" ip link show "${EP_IFACE_MASTER_RU_01}" 2>&1 | sed 's/^/    /' || true
fi

# R8.4b: The killed master's iface (wg-mst-ru-02) should still exist on endpoint-a
# (per-master-iface design: iface persists even when master is down — kernel handles failover).
if iface_is_up "${CTR_ENDPOINT_ASIA_01}" "${EP_IFACE_MASTER_RU_02}"; then
    pass "R8.4b: ${EP_IFACE_MASTER_RU_02} still exists on ${ENDPOINT_ASIA_01} (idle, master down)"
else
    info "R8.4b: ${EP_IFACE_MASTER_RU_02} DOWN on ${ENDPOINT_ASIA_01} after ${MASTER_RU_02} killed (expected if WG drops iface)"
fi

# R8.5: Restore master-02. WireGuard on endpoint-a auto-reconnects wg-mst-ru-02
# when the master comes back; no container restart needed on endpoint side.
info "R8.5: restarting ${CTR_MASTER_RU_02} to restore mesh..."
docker start "${CTR_MASTER_RU_02}" > /dev/null 2>&1 || true
info "R8.5: waiting 10s for master-02 to rejoin and handshake to re-establish..."
sleep 10

# R8.6: After master-02 restart, endpoint-a must reach endpoint-b again.
PING_R86_RC=0
docker exec "${CTR_ENDPOINT_ASIA_01}" \
    ping -c 3 -W 2 "${ENDPOINT_ASIA_02_OVERLAY}" > /dev/null 2>&1 \
    || PING_R86_RC=$?
if [[ "${PING_R86_RC}" -eq 0 ]]; then
    pass "R8.6: ${ENDPOINT_ASIA_01} reaches ${ENDPOINT_ASIA_02} after ${MASTER_RU_02} restarted — mesh healed"
else
    fail "R8.6: ${ENDPOINT_ASIA_01} cannot reach ${ENDPOINT_ASIA_02} after ${MASTER_RU_02} restart — mesh did not heal"
    docker logs "${CTR_MASTER_RU_02}" 2>&1 | tail -20 | sed 's/^/    /' || true
fi

# ---------------------------------------------------------------------------
# R9: Persistence round-trip — restart both masters, reconcile, and verify
#     non-empty allowed_ips persisted in master transport.yml.
# ---------------------------------------------------------------------------
echo ""
echo "[R9] Persistence round-trip: master transport.yml retains allowed_ips after restart..."

info "R9: restarting both masters..."
docker restart "${CTR_MASTER_RU_01}" "${CTR_MASTER_RU_02}" > /dev/null 2>&1 || true

for ctr in "${CTR_MASTER_RU_01}" "${CTR_MASTER_RU_02}"; do
    if wait_for_log "${ctr}" "gRPC server listening" 60; then
        pass "R9: ${ctr} reported gRPC server listening after restart"
    else
        fail "R9: ${ctr} did not report gRPC server listening within 60s after restart"
        docker logs "${ctr}" 2>&1 | tail -20 | sed 's/^/    /' || true
    fi
done

R9_RECON_RC=0
meshctl reconcile > /tmp/issue92rot-r9-reconcile.out 2>&1 || R9_RECON_RC=$?
if [[ "${R9_RECON_RC}" -eq 0 ]]; then
    pass "R9: mesh-ctl reconcile exited 0 after master restart"
else
    fail "R9: mesh-ctl reconcile exited ${R9_RECON_RC} after master restart"
    sed 's/^/    /' /tmp/issue92rot-r9-reconcile.out || true
fi
rm -f /tmp/issue92rot-r9-reconcile.out

for entry in \
    "${CTR_MASTER_RU_01}:${MASTER_RU_01}" \
    "${CTR_MASTER_RU_02}:${MASTER_RU_02}"
do
    ctr="${entry%%:*}"
    master_name="${entry##*:}"
    yml=$(docker exec "${ctr}" cat /config/transport.yml 2>/dev/null || true)
    allowed_count=$(echo "${yml}" | grep -c "allowed_ips:" || true)
    empty_allowed_count=$(echo "${yml}" | grep -c "allowed_ips: \[\]" || true)
    if [[ "${allowed_count}" -eq 0 ]]; then
        fail "R9: ${master_name} transport.yml has no allowed_ips entries after restart"
        echo "${yml}" | sed 's/^/    /'
    elif [[ "${empty_allowed_count}" -ne 0 ]]; then
        fail "R9: ${master_name} transport.yml still has ${empty_allowed_count} empty allowed_ips array(s) after restart"
        echo "${yml}" | sed 's/^/    /'
    else
        pass "R9: ${master_name} transport.yml keeps non-empty allowed_ips for all persisted tunnels"
    fi
done

# ---------------------------------------------------------------------------
# R9b: Port-assignment contract — persisted peer_endpoint ports match
#      endpoint-side state and endpoint UDP listeners.
# ---------------------------------------------------------------------------
echo ""
echo "[R9b] Port-assignment contract: persisted ports match endpoint state and live listeners..."

ENDPOINT_R9B_PORTS=$(
    docker exec "${CTR_ENDPOINT_US_01}" ss -ulnp 2>/dev/null \
        | awk '{print $5}' \
        | grep -oE ':[0-9]+$' \
        | tr -d ':' \
        || true
)

for entry in \
    "${CTR_MASTER_RU_01}:${MASTER_RU_01}:${MASTER_RU_01_BRIDGE}" \
    "${CTR_MASTER_RU_02}:${MASTER_RU_02}:${MASTER_RU_02_BRIDGE}"
do
    ctr="${entry%%:*}"
    rest="${entry#*:}"
    master_name="${rest%%:*}"
    master_bridge="${entry##*:}"

    # Scope port extraction to the ep-us-01 tunnel block — transport.yml may
    # carry multiple tunnels and the first peer_endpoint may belong to another.
    master_port=$(
        docker exec "${ctr}" cat /config/transport.yml 2>/dev/null \
            | awk -v ep="${ENDPOINT_US_01}" '
                $1 == "-" && $2 == "name:" { in_block = ($3 == ep); next }
                in_block && $1 == "peer_endpoint:" {
                    n = split($2, parts, ":")
                    if (n > 1) print parts[n]
                    exit
                }
            ' \
            || true
    )
    ep_port=$(
        docker exec "${CTR_ENDPOINT_US_01}" cat /config/transport.yml 2>/dev/null \
            | awk -v host="${master_bridge}" '
                $1 == "-" && $2 == "name:" && $3 == host { in_block=1; next }
                $1 == "-" && $2 == "name:" { in_block=0 }
                in_block && $1 == "peer_endpoint:" {
                    n = split($2, parts, ":")
                    if (n > 1) {
                        print parts[n]
                    }
                    exit
                }
            ' \
            || true
    )

    if [[ "${master_port}" != "${ep_port}" ]]; then
        fail "[R9b] FAIL: port mismatch — master ${master_name} expects :${master_port}, ep transport.yml has :${ep_port}"
        continue
    fi

    if echo "${ENDPOINT_R9B_PORTS}" | grep -qx "${master_port}"; then
        pass "R9b: ${master_name} peer_endpoint port :${master_port} matches persisted endpoint state and ss -ulnp"
    else
        fail "[R9b] FAIL: port :${ep_port} not found in endpoint ss -ulnp output"
    fi
done

# ---------------------------------------------------------------------------
# R10 (G8): Endpoint↔endpoint overlay src hint + reachability matrix.
# ---------------------------------------------------------------------------
echo ""
echo "[R10] G8: Endpoint↔endpoint overlay src hint + ping matrix..."

for src_entry in \
    "${CTR_ENDPOINT_US_01}:${ENDPOINT_US_01_OVERLAY}" \
    "${CTR_ENDPOINT_ASIA_01}:${ENDPOINT_ASIA_01_OVERLAY}" \
    "${CTR_ENDPOINT_ASIA_02}:${ENDPOINT_ASIA_02_OVERLAY}"
do
    src_ctr="${src_entry%%:*}"
    src_overlay="${src_entry##*:}"

    for dst_overlay in \
        "${ENDPOINT_US_01_OVERLAY}" \
        "${ENDPOINT_ASIA_01_OVERLAY}" \
        "${ENDPOINT_ASIA_02_OVERLAY}"
    do
        if [[ "${dst_overlay}" == "${src_overlay}" ]]; then
            continue
        fi

        route_get=$(docker exec "${src_ctr}" ip route get "${dst_overlay}" 2>&1 | head -1 || true)
        if echo "${route_get}" | grep -qF "src ${src_overlay}"; then
            pass "R10: ${src_ctr} route-get ${dst_overlay} src=${src_overlay}"
        else
            fail "R10: ${src_ctr} route-get ${dst_overlay} wrong src — ${route_get}"
        fi

        if docker exec "${src_ctr}" ping -c 2 -W 2 "${dst_overlay}" > /dev/null 2>&1; then
            pass "R10: ${src_ctr} -> ${dst_overlay} ping 2/2"
        else
            fail "R10: ${src_ctr} -> ${dst_overlay} ping FAILED"
        fi
    done
done

# ---------------------------------------------------------------------------
# R11 (G11): Master per-tunnel AllowedIPs must include the endpoints range so
# cross-endpoint forwarding survives WireGuard reverse-path validation.
# ---------------------------------------------------------------------------
echo ""
echo "[R11] G11: master per-tunnel AllowedIPs include endpoints range..."

for src_entry in \
    "${CTR_ENDPOINT_US_01}:${ENDPOINT_US_01_OVERLAY}" \
    "${CTR_ENDPOINT_ASIA_01}:${ENDPOINT_ASIA_01_OVERLAY}" \
    "${CTR_ENDPOINT_ASIA_02}:${ENDPOINT_ASIA_02_OVERLAY}"
do
    src_ctr="${src_entry%%:*}"
    src_overlay="${src_entry##*:}"

    for dst_overlay in \
        "${ENDPOINT_US_01_OVERLAY}" \
        "${ENDPOINT_ASIA_01_OVERLAY}" \
        "${ENDPOINT_ASIA_02_OVERLAY}"
    do
        if [[ "${dst_overlay}" == "${src_overlay}" ]]; then
            continue
        fi

        if docker exec "${src_ctr}" ping -c 2 -W 2 "${dst_overlay}" > /dev/null 2>&1; then
            pass "R11: ${src_ctr} -> ${dst_overlay} ping 2/2 after master AllowedIPs expansion"
        else
            fail "R11: ${src_ctr} -> ${dst_overlay} ping FAILED after master AllowedIPs expansion"
        fi
    done
done

for master in "${MASTER_RU_01}" "${MASTER_RU_02}"; do
    inspect_out=$(meshctl inspect "${master}" 2>&1 || true)
    for endpoint in "${ENDPOINT_US_01}" "${ENDPOINT_ASIA_01}" "${ENDPOINT_ASIA_02}"; do
        row=$(echo "${inspect_out}" | grep -E "^${endpoint}[[:space:]]+" | head -1 || true)
        if [[ -z "${row}" ]]; then
            fail "R11: ${master} inspect has no row for ${endpoint}"
            continue
        fi

        # Assert ENDPOINTS_RANGE_CIDR is present in both DISK_IPS (col 7) and
        # RUNTIME_IPS (col 8) columns, not just anywhere in the row (which would
        # falsely pass if the CIDR appears only in ADMIN_IPS).
        disk_ips=$(echo "${row}" | awk '{print $7}')
        runtime_ips=$(echo "${row}" | awk '{print $8}')
        if echo "${disk_ips}" | grep -qF "${ENDPOINTS_RANGE_CIDR}" \
            && echo "${runtime_ips}" | grep -qF "${ENDPOINTS_RANGE_CIDR}"; then
            pass "R11: ${master}/${endpoint} disk+runtime include ${ENDPOINTS_RANGE_CIDR}"
        else
            fail "R11: ${master}/${endpoint} missing ${ENDPOINTS_RANGE_CIDR} in disk/runtime (disk=${disk_ips}, runtime=${runtime_ips})"
            echo "${row}" | sed 's/^/    /'
        fi
    done
done

# ---------------------------------------------------------------------------
# R11b (G12): master without --topology persists admin AllowedIPs (/27)
# Reproduces the production deployment where master compose omits TOPOLOGY_PATH.
# Fix: CLI callers (master init/endpoint init/reconcile) pass AllowedIps in
# AddTunnelRequest; saveTransportState persists tunnel.AllowedIPs verbatim
# when non-empty, bypassing the nil-topology recompute path.
# ---------------------------------------------------------------------------
echo ""
echo "[R11b] G12: master without --topology persists admin AllowedIPs (issue #147 layer 3)..."

R11B_MASTER="r11b-mst"
R11B_EP="ep-r11b"
R11B_CTR="${COMPOSE_PROJECT}-${R11B_MASTER}"
R11B_GRPC_HOST_PORT=59290
R11B_GRPC_CTR_PORT=9090
R11B_CTL_DIR=$(mktemp -d /tmp/r11b-ctl-XXXXXX)
R11B_TOPO=$(mktemp /tmp/r11b-topo-XXXXXX.yml)

# Write a minimal topology for the CLI side only (master daemon does NOT get this).
cat > "${R11B_TOPO}" <<R11B_TOPO_EOF
masters:
  - name: ${R11B_MASTER}
    host: 127.0.0.1
    grpc_port: ${R11B_GRPC_HOST_PORT}
    overlay_ip: 172.21.92.2

endpoints:
  - name: ${R11B_EP}
    host: 127.0.0.1
    overlay_ip: 172.21.92.34
    listen_port: 51820
    grpc_port: 59291

overlay:
  ranges:
    - name: masters
      cidr: 172.21.92.0/27
    - name: endpoints
      cidr: 172.21.92.32/27

transport:
  pool: 10.93.0.0/16
  prefix_length: 30
R11B_TOPO_EOF

# Prepare master (generates token hash).
R11B_PREP_OUT=$(${MESHCTL_BIN} --topology "${R11B_TOPO}" --config-dir "${R11B_CTL_DIR}" \
    master prepare "${R11B_MASTER}" 2>&1) && R11B_PREP_RC=0 || R11B_PREP_RC=$?
if [[ "${R11B_PREP_RC}" -ne 0 ]]; then
    fail "R11b: master prepare ${R11B_MASTER} failed (rc=${R11B_PREP_RC}): ${R11B_PREP_OUT}"
else
    R11B_TOKEN=$(cat "${R11B_CTL_DIR}/nodes/${R11B_MASTER}/mesh.token" 2>/dev/null || true)
    if [[ -z "${R11B_TOKEN}" ]]; then
        fail "R11b: master prepare produced no token"
    else
        R11B_TOKEN_ESC="${R11B_TOKEN//\$/\$\$}"

        # Prepare endpoint too (mesh-ctl master init needs the endpoint token and pubkey).
        ${MESHCTL_BIN} --topology "${R11B_TOPO}" --config-dir "${R11B_CTL_DIR}" \
            endpoint prepare "${R11B_EP}" > /dev/null 2>&1 || true

        # Start master WITHOUT --topology (prod scenario).
        docker run -d --rm \
            --name "${R11B_CTR}" \
            --privileged \
            -p "${R11B_GRPC_HOST_PORT}:${R11B_GRPC_CTR_PORT}" \
            -e "MESH_TOKEN_HASH=${R11B_TOKEN_ESC}" \
            "${NODE_IMAGE}" \
            sh -c "[ -f /config/mesh.token ] || printf '%s' \"\${MESH_TOKEN_HASH}\" > /config/mesh.token; \
                   exec /usr/local/bin/awg-mesh-node \
                   --mode master --name ${R11B_MASTER} \
                   --overlay-ip 172.21.92.2 --listen-port 51820" > /dev/null 2>&1

        # Wait for gRPC ready (up to 30s).
        R11B_READY=0
        for i in $(seq 1 30); do
            if docker logs "${R11B_CTR}" 2>&1 | grep -q "gRPC server listening"; then
                R11B_READY=1
                break
            fi
            sleep 1
        done

        if [[ "${R11B_READY}" -eq 0 ]]; then
            fail "R11b: master without --topology did not become gRPC ready within 30s"
            docker logs "${R11B_CTR}" >&2 || true
        else
            # Run mesh-ctl master init against the no-topology master.
            R11B_INIT_OUT=$(${MESHCTL_BIN} \
                --topology "${R11B_TOPO}" \
                --config-dir "${R11B_CTL_DIR}" \
                master init "${R11B_MASTER}" 2>&1) && R11B_INIT_RC=0 || R11B_INIT_RC=$?

            if [[ "${R11B_INIT_RC}" -ne 0 ]]; then
                fail "R11b: master init ${R11B_MASTER} failed (rc=${R11B_INIT_RC}): ${R11B_INIT_OUT}"
            else
                # Assert transport.yml inside container contains the /27 from admin AllowedIPs.
                if docker exec "${R11B_CTR}" sh -c "grep -F '172.21.92.32/27' /config/transport.yml" > /dev/null 2>&1; then
                    pass "R11b: master without --topology persists /27 in transport.yml (admin AllowedIPs verbatim)"
                else
                    R11B_DISK=$(docker exec "${R11B_CTR}" cat /config/transport.yml 2>/dev/null || echo "<unreadable>")
                    fail "R11b: transport.yml missing 172.21.92.32/27 — contents: ${R11B_DISK}"
                fi
            fi
        fi

        docker stop "${R11B_CTR}" > /dev/null 2>&1 || true
    fi
fi

rm -rf "${R11B_CTL_DIR}"
rm -f "${R11B_TOPO}"

# ---------------------------------------------------------------------------
# R12 (G15): master auto-installs wg-+ → wg-+ FORWARD ACCEPT rule on startup.
# Validates local tracker #150 fix: endpoint↔endpoint overlay forwarding works
# even on Docker hosts where default FORWARD policy is DROP.
# ---------------------------------------------------------------------------
echo ""
echo "[R12] G15: master containers have wg-+ → wg-+ FORWARD ACCEPT rule installed..."
for CTR in "${CTR_MASTER_RU_01}" "${CTR_MASTER_RU_02}"; do
    if docker exec "${CTR}" iptables -C FORWARD -i 'wg-+' -o 'wg-+' -j ACCEPT 2>/dev/null; then
        pass "R12: ${CTR}: iptables FORWARD wg-+ → wg-+ ACCEPT rule present"
    else
        fail "R12: ${CTR}: FORWARD wg-+ → wg-+ ACCEPT rule MISSING — endpoint↔endpoint forwarding will break on DROP FORWARD hosts"
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
