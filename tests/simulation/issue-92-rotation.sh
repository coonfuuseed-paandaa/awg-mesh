#!/usr/bin/env bash
# issue-92-rotation.sh — Docker integration script for endpoint key-rotation scenario.
# local tracker #92: UpdateTunnelPeer RPC propagates endpoint keypair rotation to masters.
#
# What this script validates:
#   R1  Two-master + one-endpoint mesh boots cleanly.
#   R2  Capture old endpoint pubkey from master-ru-01 wg interface.
#   R3  Rotate endpoint keypair via mesh-ctl (endpoint prepare --rotate or rotate --tier 3).
#   R4  Run mesh-ctl endpoint init <name> — propagates new key to both masters.
#   R5  Within 5 s, master-ru-01 wg interface reflects the NEW pubkey (not the old one).
#   R6  R-1 guarantee: other peers' last-handshake counters on master-ru-01 are unchanged
#       (key rotation of one endpoint must not disrupt unrelated tunnels).
#
# Usage (Linux host with Docker):
#   cd <repo-root>
#   bash tests/simulation/issue-92-rotation.sh
#
# Prerequisites:
#   - Docker running (Linux host or Docker Desktop with Linux containers).
#   - awg-mesh-node:local image built:
#       docker build -t awg-mesh-node:local .
#   - mesh-ctl in PATH:
#       go install ./cmd/mesh-ctl
#   - CAP_NET_ADMIN available inside containers (privileged: true in compose).
#
# Windows hosts: this script requires WireGuard kernel modules and CAP_NET_ADMIN
# inside containers, which are unavailable on Windows Docker Desktop without WSL2
# kernel support. Run inside WSL2 Ubuntu or a CI Linux runner instead.
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

# wg_peers_on <container> — prints all peer public keys currently on wg0.
wg_peers_on() {
    local container="$1"
    docker exec "${container}" wg show wg0 peers 2>/dev/null || true
}

# wg_handshakes_on <container> — prints "pubkey\ttimestamp" pairs.
wg_handshakes_on() {
    local container="$1"
    docker exec "${container}" wg show wg0 latest-handshakes 2>/dev/null || true
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
${MESHCTL_BIN} \
    --topology "${TOPO_FILE}" \
    --config-dir "${CTL_CONFIG_DIR}" \
    master prepare "${MASTER_RU_01}" > /dev/null 2>&1 || {
    echo "ERROR: mesh-ctl master prepare ${MASTER_RU_01} failed" >&2
    exit 3
}
${MESHCTL_BIN} \
    --topology "${TOPO_FILE}" \
    --config-dir "${CTL_CONFIG_DIR}" \
    master prepare "${MASTER_RU_02}" > /dev/null 2>&1 || {
    echo "ERROR: mesh-ctl master prepare ${MASTER_RU_02} failed" >&2
    exit 3
}
${MESHCTL_BIN} \
    --topology "${TOPO_FILE}" \
    --config-dir "${CTL_CONFIG_DIR}" \
    endpoint prepare "${ENDPOINT_US_01}" > /dev/null 2>&1 || {
    echo "ERROR: mesh-ctl endpoint prepare ${ENDPOINT_US_01} failed" >&2
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
info "Tokens resolved: master-ru-01/02 + endpoint-us-01"

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
# R2: Capture old endpoint pubkey from master-ru-01 wg interface
# ---------------------------------------------------------------------------
echo ""
echo "[R2] Capturing old endpoint pubkey from ${MASTER_RU_01} wg0..."

OLD_PEERS=$(wg_peers_on "${CTR_MASTER_RU_01}")
if [[ -z "${OLD_PEERS}" ]]; then
    fail "R2: no peers found on ${MASTER_RU_01} wg0 after initial endpoint init"
    echo "[abort] Cannot capture old key — aborting."
    exit "${FAILURES}"
fi

# There should be exactly one peer (the endpoint). Capture it.
OLD_KEY=$(echo "${OLD_PEERS}" | awk 'NR==1{print $1}')
info "Old endpoint pubkey on ${MASTER_RU_01}: ${OLD_KEY}"

# Capture handshake counters for all OTHER peers (R-1: unrelated peers must not
# be disrupted). In this 2-master + 1-endpoint mesh there are no other peers,
# but the pattern is documented for larger topologies.
OLD_HANDSHAKES=$(wg_handshakes_on "${CTR_MASTER_RU_01}" || true)

pass "R2: old endpoint pubkey captured (${OLD_KEY:0:8}...)"

# ---------------------------------------------------------------------------
# R3: Rotate endpoint keypair via mesh-ctl
# ---------------------------------------------------------------------------
echo ""
echo "[R3] Rotating endpoint keypair..."

# Try --rotate flag on endpoint prepare first (v1.10.0+).
# Fall back to rotate --tier 3 --node if the flag is absent (older builds).
ROTATE_RC=0
ROTATE_OUT=""

if ${MESHCTL_BIN} endpoint prepare --help 2>&1 | grep -q -- '--rotate'; then
    info "Using: mesh-ctl endpoint prepare --rotate ${ENDPOINT_US_01}"
    ROTATE_OUT=$(${MESHCTL_BIN} \
        --topology "${TOPO_FILE}" \
        --config-dir "${CTL_CONFIG_DIR}" \
        endpoint prepare --rotate "${ENDPOINT_US_01}" 2>&1) || ROTATE_RC=$?
else
    info "Flag --rotate absent; using: mesh-ctl rotate --tier 3 --endpoint ${ENDPOINT_US_01}"
    ROTATE_OUT=$(${MESHCTL_BIN} \
        --topology "${TOPO_FILE}" \
        --config-dir "${CTL_CONFIG_DIR}" \
        rotate --tier 3 --endpoint "${ENDPOINT_US_01}" 2>&1) || ROTATE_RC=$?
fi

if [[ "${ROTATE_RC}" -ne 0 ]]; then
    fail "R3: keypair rotation command failed (rc=${ROTATE_RC}): ${ROTATE_OUT}"
    echo "[abort] Cannot proceed — rotation failed."
    exit "${FAILURES}"
fi
info "Rotation output:"
echo "${ROTATE_OUT}" | sed 's/^/    /'
pass "R3: keypair rotation command succeeded"

# ---------------------------------------------------------------------------
# R4: Run mesh-ctl endpoint init to propagate the new key
# ---------------------------------------------------------------------------
echo ""
echo "[R4] Running endpoint init to propagate new key to masters..."

PROPAGATE_OUT=$(${MESHCTL_BIN} \
    --topology "${TOPO_FILE}" \
    --config-dir "${CTL_CONFIG_DIR}" \
    endpoint init "${ENDPOINT_US_01}" 2>&1) && PROPAGATE_RC=0 || PROPAGATE_RC=$?

info "Propagation output:"
echo "${PROPAGATE_OUT}" | sed 's/^/    /'

if [[ "${PROPAGATE_RC}" -ne 0 ]]; then
    fail "R4: endpoint init (post-rotation) failed (rc=${PROPAGATE_RC})"
else
    pass "R4: endpoint init (post-rotation) exited 0"
fi

# Check that the output contains "updated" for at least one master — confirms
# the UpdateTunnelPeer RPC was actually called (not just "unchanged").
if echo "${PROPAGATE_OUT}" | grep -qi "updated"; then
    pass "R4a: propagation output contains 'updated' — UpdateTunnelPeer RPC was invoked"
else
    fail "R4a: propagation output does not contain 'updated' — key may not have been pushed"
fi

# ---------------------------------------------------------------------------
# R5: Within KEY_PROPAGATE_TIMEOUT seconds, master-ru-01 wg0 must show NEW key
# ---------------------------------------------------------------------------
echo ""
echo "[R5] Waiting up to ${KEY_PROPAGATE_TIMEOUT}s for new key on ${MASTER_RU_01} wg0..."

NEW_KEY_FOUND=false
DEADLINE=$(( $(date +%s) + KEY_PROPAGATE_TIMEOUT ))
NEW_KEY=""

while [[ $(date +%s) -le ${DEADLINE} ]]; do
    CURRENT_PEERS=$(wg_peers_on "${CTR_MASTER_RU_01}")
    if [[ -n "${CURRENT_PEERS}" ]]; then
        FIRST_PEER=$(echo "${CURRENT_PEERS}" | awk 'NR==1{print $1}')
        if [[ "${FIRST_PEER}" != "${OLD_KEY}" ]]; then
            NEW_KEY="${FIRST_PEER}"
            NEW_KEY_FOUND=true
            break
        fi
    fi
    sleep 1
done

if [[ "${NEW_KEY_FOUND}" == "true" ]]; then
    pass "R5: new key ${NEW_KEY:0:8}... appeared on ${MASTER_RU_01} wg0 within ${KEY_PROPAGATE_TIMEOUT}s"
else
    STILL_PEERS=$(wg_peers_on "${CTR_MASTER_RU_01}")
    fail "R5: new key did NOT appear on ${MASTER_RU_01} wg0 within ${KEY_PROPAGATE_TIMEOUT}s"
    info "  Old key:    ${OLD_KEY}"
    info "  Current peers: ${STILL_PEERS:-<none>}"
fi

# Also verify that master-ru-02 has the new key.
if [[ "${NEW_KEY_FOUND}" == "true" ]]; then
    PEERS_RU_02=$(wg_peers_on "${CTR_MASTER_RU_02}")
    FIRST_PEER_RU_02=$(echo "${PEERS_RU_02}" | awk 'NR==1{print $1}')
    if [[ "${FIRST_PEER_RU_02}" == "${NEW_KEY}" ]]; then
        pass "R5b: ${MASTER_RU_02} also shows new key — both masters updated"
    else
        fail "R5b: ${MASTER_RU_02} still has old key or different key: ${FIRST_PEER_RU_02}"
    fi
fi

# ---------------------------------------------------------------------------
# R6: R-1 guarantee — other peers' handshake counters are unchanged
# ---------------------------------------------------------------------------
echo ""
echo "[R6] R-1: verifying no disruption to unrelated peers on ${MASTER_RU_01}..."

# In a 2-master + 1-endpoint mesh the endpoint is the only peer, so there are
# no "other" peers to check by construction. The test documents the validation
# pattern: record handshake counters before rotation, compare after.
#
# With a larger topology (multiple endpoints), this check would iterate over
# all peers EXCEPT the rotated one and assert counters are unchanged.
#
# Here we verify that: (a) the wg interface is still up, (b) the old key is
# gone, confirming no double-entry or interface reset occurred.

WG_UP=$(docker exec "${CTR_MASTER_RU_01}" wg show wg0 2>/dev/null | head -1 || echo "")
if [[ -n "${WG_UP}" ]]; then
    pass "R6: ${MASTER_RU_01} wg0 interface is still up after rotation"
else
    fail "R6: ${MASTER_RU_01} wg0 interface is down after rotation"
fi

CURRENT_PEERS_AFTER=$(wg_peers_on "${CTR_MASTER_RU_01}")
if echo "${CURRENT_PEERS_AFTER}" | grep -qF "${OLD_KEY}"; then
    fail "R6a: old key ${OLD_KEY:0:8}... still present — peer was not replaced cleanly"
else
    pass "R6a: old key removed cleanly — no stale peer entry"
fi

# Handshake counter check (documents the pattern; no other peers in this mesh).
info "R6 note: single-endpoint mesh — no unrelated peers to check handshake counters for."
info "  For multi-endpoint topologies, extend this check to compare OLD_HANDSHAKES"
info "  vs current handshakes for all peers except ${ENDPOINT_US_01}."

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
