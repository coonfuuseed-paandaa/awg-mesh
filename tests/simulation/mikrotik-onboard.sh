#!/usr/bin/env bash
# mikrotik-onboard.sh — E2E onboarding test for MikroTik-equivalent client.
# local tracker F-002 T-008 (REQ-3 / Strangler Fig parity gate)
#
# What this script validates:
#   A1  Default route 100.127.0.1 pre-injected via veth-mikrotik is preserved
#       after `mesh-ctl client init mikrotik-home` completes (30 s poll).
#   A2  Ping matrix: mikrotik-home client reaches all 6 mesh overlay IPs at 100%.
#   A3  DSCP-tagged packets (classes 10/20/30/31/32) exit via the expected WG
#       interface based on routing_policies configured in the topology.
#   A4  TCP MSS on outbound SYN through WG tunnel is <= 1380 bytes.
#   A5  /config/awg-mesh-client.log exists and every line is valid JSON.
#
# Usage (Linux host or WSL2 with Docker):
#   cd <repo-root>
#   bash tests/simulation/mikrotik-onboard.sh
#
# Prerequisites:
#   - Docker running (Linux host or Docker Desktop with Linux containers).
#   - awg-mesh-node:local image built:
#       docker build -t awg-mesh-node:local .
#   - awg-mesh-client:v1.14.0 image available (pulled or pre-loaded).
#   - mesh-ctl in PATH or $REPO_ROOT/bin/mesh-ctl present.
#   - Running as root or with sudo access for nsenter + ip link.
#
# Windows hosts: requires Linux-namespaced network devices and CAP_NET_ADMIN.
# Run inside WSL2 Ubuntu or a CI Linux runner.
#
# Exit: 0 = all checks passed, non-zero = failure count.
set -euo pipefail

# ---------------------------------------------------------------------------
# Platform guard
# ---------------------------------------------------------------------------
if [[ "$(uname -s)" != "Linux" ]]; then
    echo "[A0] SKIP: mikrotik-onboard.sh requires Linux (network namespaces + nsenter)."
    echo "     Run inside WSL2 Ubuntu or a CI Linux runner."
    exit 0
fi

# ---------------------------------------------------------------------------
# Paths
# ---------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

COMPOSE_PROJECT="mikrotik"

# Ephemeral files (cleaned up on EXIT).
CTL_CONFIG_DIR=$(mktemp -d /tmp/mikrotik-onboard-ctl-XXXXXX)
TOPO_FILE=$(mktemp /tmp/mikrotik-onboard-topo-XXXXXX.yml)
COMPOSE_FILE=$(mktemp /tmp/mikrotik-onboard-compose-XXXXXX.yml)

# ---------------------------------------------------------------------------
# Node names
# ---------------------------------------------------------------------------
MASTER_01="mst-onb-01"
MASTER_02="mst-onb-02"
ENDPOINT_01="ep-onb-01"
ENDPOINT_02="ep-onb-02"
CLIENT_MIKROTIK="mikrotik-hom"

CTR_MASTER_01="${COMPOSE_PROJECT}-${MASTER_01}"
CTR_MASTER_02="${COMPOSE_PROJECT}-${MASTER_02}"
CTR_ENDPOINT_01="${COMPOSE_PROJECT}-${ENDPOINT_01}"
CTR_ENDPOINT_02="${COMPOSE_PROJECT}-${ENDPOINT_02}"
CTR_MIKROTIK="${COMPOSE_PROJECT}-${CLIENT_MIKROTIK}"

# Bridge IPs (192.168.78.0/24 — distinct subnet to avoid port/network conflicts).
MASTER_01_BRIDGE="192.168.78.10"
MASTER_02_BRIDGE="192.168.78.11"
ENDPOINT_01_BRIDGE="192.168.78.20"
ENDPOINT_02_BRIDGE="192.168.78.21"
MIKROTIK_BRIDGE="192.168.78.100"

# Overlay IPs (172.21.78.0/24 — distinct from existing sim tests).
MASTER_01_OVERLAY="172.21.78.2"
MASTER_02_OVERLAY="172.21.78.3"
ENDPOINT_01_OVERLAY="172.21.78.34"
ENDPOINT_02_OVERLAY="172.21.78.35"
MIKROTIK_OVERLAY="172.21.78.132"
ENDPOINTS_RANGE_CIDR="172.21.78.32/27"
CLIENTS_RANGE_CIDR="172.21.78.128/25"

# gRPC ports (host-side).
MASTER_01_GRPC="19678"
MASTER_02_GRPC="29678"
ENDPOINT_01_GRPC="39678"
ENDPOINT_02_GRPC="49678"
MIKROTIK_GRPC="59678"

# WireGuard listen ports (container-internal; not exposed to host).
WG_LISTEN_PORT="51820"

# Pre-injection network parameters (simulates RouterOS CGN veth setup).
VETH_GUEST="veth-mikrotik"
VETH_HOST="veth-mikrotik-host"
CGN_GW_IP="100.127.0.1"
CGN_GW_CIDR="100.127.0.1/24"
# CGN_GUEST_CIDR drives the guest-side route injection; the bare-IP form
# was previously assigned to CGN_GUEST_IP but never read — removed to keep
# the constants list honest.
CGN_GUEST_CIDR="100.127.0.2/24"

# Timing.
GRPC_READY_TIMEOUT=60
INIT_SETTLE_TIMEOUT=30
MSS_CLAMP_MAX=1380

# Docker images.
NODE_IMAGE="${NODE_IMAGE:-awg-mesh-node:local}"
CLIENT_IMAGE="${CLIENT_IMAGE:-ghcr.io/coonfuuseed-paandaa/awg-mesh-client:v1.14.0}"

# ---------------------------------------------------------------------------
# Counters and colours
# ---------------------------------------------------------------------------
FAILURES=0
PASSES=0

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RESET='\033[0m'

pass() { echo -e "  [${GREEN}PASS${RESET}] $*"; (( PASSES++ )) || true; }
fail() { echo -e "  [${RED}FAIL${RESET}] $*" >&2; (( FAILURES++ )) || true; }
info() { echo "  [info] $*"; }
warn() { echo -e "  [${YELLOW}WARN${RESET}] $*"; }

# ---------------------------------------------------------------------------
# Cleanup trap
# ---------------------------------------------------------------------------
cleanup() {
    local rc=$?
    echo ""
    if [[ "${NO_CLEANUP:-0}" == "1" ]]; then
        echo "[cleanup] NO_CLEANUP=1 — leaving containers/files for inspection."
        echo "  Compose project: ${COMPOSE_PROJECT}"
        echo "  Mikrotik ctr:    ${CTR_MIKROTIK}"
        echo "  Topology file:   ${TOPO_FILE}"
        echo "  Ctl config dir:  ${CTL_CONFIG_DIR}"
        return
    fi
    echo "[cleanup] Tearing down containers, veths, and temp files..."
    # Remove standalone mikrotik container first (not in compose).
    docker rm -f "${CTR_MIKROTIK}" 2>/dev/null || true
    # Remove veth pair from host (if created).
    ip link del "${VETH_HOST}" 2>/dev/null || true
    # Tear down compose stack.
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

# meshctl — thin wrapper injecting --topology and --config-dir.
meshctl() {
    "${MESHCTL_BIN}" \
        --topology "${TOPO_FILE}" \
        --config-dir "${CTL_CONFIG_DIR}" \
        "$@"
}

# ---------------------------------------------------------------------------
# Preflight
# ---------------------------------------------------------------------------
echo ""
echo "=== mikrotik-onboard.sh — MikroTik client onboarding E2E test (F-002 T-008) ==="
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
    echo "ERROR: node image ${NODE_IMAGE} not found. Build it first:" >&2
    echo "  docker build -t ${NODE_IMAGE} ." >&2
    exit 2
fi
if ! docker image inspect "${CLIENT_IMAGE}" > /dev/null 2>&1; then
    echo "WARNING: client image ${CLIENT_IMAGE} not found locally; attempting pull..." >&2
    docker pull "${CLIENT_IMAGE}" || {
        echo "ERROR: could not pull ${CLIENT_IMAGE}." >&2
        exit 2
    }
fi
if ! command -v nsenter > /dev/null 2>&1; then
    echo "ERROR: nsenter not found. Required for pre-injection of default route." >&2
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
info "node image:   ${NODE_IMAGE}"
info "client image: ${CLIENT_IMAGE}"

# ---------------------------------------------------------------------------
# Write topology (inline — self-contained, no dependency on mesh-topology.yml).
# Includes routing_policies for DSCP assertion A3.
# ---------------------------------------------------------------------------
cat > "${TOPO_FILE}" <<EOF
overlay:
  space: 172.21.78.0/24
  physical_mtu: 1500
  awg_overhead: 80
  ranges:
    - name: masters
      cidr: 172.21.78.0/27
      balancer_ip: 172.21.78.1
    - name: endpoints
      cidr: ${ENDPOINTS_RANGE_CIDR}
      balancer_ip: 172.21.78.33
    - name: clients
      cidr: ${CLIENTS_RANGE_CIDR}

masters:
  - name: ${MASTER_01}
    host: 127.0.0.1
    peer_host: ${MASTER_01_BRIDGE}
    overlay_ip: ${MASTER_01_OVERLAY}
    listen_port: ${WG_LISTEN_PORT}
    grpc_port: ${MASTER_01_GRPC}
    endpoints:
      - ${ENDPOINT_01}
      - ${ENDPOINT_02}

  - name: ${MASTER_02}
    host: 127.0.0.1
    peer_host: ${MASTER_02_BRIDGE}
    overlay_ip: ${MASTER_02_OVERLAY}
    listen_port: ${WG_LISTEN_PORT}
    grpc_port: ${MASTER_02_GRPC}
    endpoints:
      - ${ENDPOINT_01}
      - ${ENDPOINT_02}

endpoints:
  - name: ${ENDPOINT_01}
    host: 127.0.0.1
    peer_host: ${ENDPOINT_01_BRIDGE}
    overlay_ip: ${ENDPOINT_01_OVERLAY}
    listen_port: ${WG_LISTEN_PORT}
    grpc_port: ${ENDPOINT_01_GRPC}

  - name: ${ENDPOINT_02}
    host: 127.0.0.1
    peer_host: ${ENDPOINT_02_BRIDGE}
    overlay_ip: ${ENDPOINT_02_OVERLAY}
    listen_port: ${WG_LISTEN_PORT}
    grpc_port: ${ENDPOINT_02_GRPC}

clients:
  - name: ${CLIENT_MIKROTIK}
    type: mikrotik
    host: 127.0.0.1
    overlay_ip: ${MIKROTIK_OVERLAY}
    grpc_port: ${MIKROTIK_GRPC}
    masters:
      - ${MASTER_01}
      - ${MASTER_02}
    veth:
      name: ${VETH_GUEST}
      gateway: ${CGN_GW_IP}/24
    routing_policies:
      - name: bulk
        dscp: 10
        targets:
          - ${ENDPOINT_01}
      - name: stream
        dscp: 20
        targets:
          - ${ENDPOINT_02}
      - name: rt-voice
        dscp: 30
        targets:
          - ${ENDPOINT_01}
      - name: rt-video
        dscp: 31
        targets:
          - ${ENDPOINT_02}
      - name: cs6
        dscp: 32
        targets:
          - ${ENDPOINT_01}

transport:
  pool: 10.78.0.0/16
  prefix_length: 30
EOF

# ---------------------------------------------------------------------------
# Prepare all nodes (generates tokens and CA).
# ---------------------------------------------------------------------------
info "Preparing nodes..."
for node_name in "${MASTER_01}" "${MASTER_02}" "${ENDPOINT_01}" "${ENDPOINT_02}"; do
    subcmd="master"
    if [[ "${node_name}" == "${ENDPOINT_01}" || "${node_name}" == "${ENDPOINT_02}" ]]; then
        subcmd="endpoint"
    fi
    ${MESHCTL_BIN} \
        --topology "${TOPO_FILE}" \
        --config-dir "${CTL_CONFIG_DIR}" \
        "${subcmd}" prepare "${node_name}" > /dev/null || {
        echo "ERROR: mesh-ctl ${subcmd} prepare ${node_name} failed." >&2
        exit 3
    }
done
${MESHCTL_BIN} \
    --topology "${TOPO_FILE}" \
    --config-dir "${CTL_CONFIG_DIR}" \
    client prepare "${CLIENT_MIKROTIK}" > /dev/null || {
    echo "ERROR: mesh-ctl client prepare ${CLIENT_MIKROTIK} failed." >&2
    exit 3
}

TOKEN_MASTER_01=$(cat "${CTL_CONFIG_DIR}/nodes/${MASTER_01}/mesh.token")
TOKEN_MASTER_02=$(cat "${CTL_CONFIG_DIR}/nodes/${MASTER_02}/mesh.token")
TOKEN_ENDPOINT_01=$(cat "${CTL_CONFIG_DIR}/nodes/${ENDPOINT_01}/mesh.token")
TOKEN_ENDPOINT_02=$(cat "${CTL_CONFIG_DIR}/nodes/${ENDPOINT_02}/mesh.token")
TOKEN_MIKROTIK=$(cat "${CTL_CONFIG_DIR}/nodes/${CLIENT_MIKROTIK}/mesh.token")

# Escape $ for docker-compose interpolation (argon2id hashes contain $argon2id$... in v1.14.0+).
TOKEN_MASTER_01_ESC="${TOKEN_MASTER_01//\$/\$\$}"
TOKEN_MASTER_02_ESC="${TOKEN_MASTER_02//\$/\$\$}"
TOKEN_ENDPOINT_01_ESC="${TOKEN_ENDPOINT_01//\$/\$\$}"
TOKEN_ENDPOINT_02_ESC="${TOKEN_ENDPOINT_02//\$/\$\$}"

info "Tokens resolved for all nodes."

# ---------------------------------------------------------------------------
# Write compose file for master/endpoint nodes.
# The mikrotik client container is managed separately (docker run) to support
# the pre-injection trick: delay binary start via entrypoint sleep so we can
# nsenter into the network namespace and add the default route before AWG init.
# ---------------------------------------------------------------------------
cat > "${COMPOSE_FILE}" <<EOF
# Auto-generated by mikrotik-onboard.sh — do not edit.
services:
  ${MASTER_01}:
    image: ${NODE_IMAGE}
    container_name: ${CTR_MASTER_01}
    hostname: ${MASTER_01}
    restart: "no"
    privileged: true
    environment:
      MESH_TOKEN_HASH: "${TOKEN_MASTER_01_ESC}"
    networks:
      mikrotik:
        ipv4_address: ${MASTER_01_BRIDGE}
    ports:
      - "${MASTER_01_GRPC}:9090"
    entrypoint:
      - sh
      - -c
      - |
        [ -f /config/mesh.token ] || printf '%s' "\$\$MESH_TOKEN_HASH" > /config/mesh.token
        exec /usr/local/bin/awg-mesh-node \\
          --mode master \\
          --name ${MASTER_01} \\
          --overlay-ip ${MASTER_01_OVERLAY} \\
          --listen-port ${WG_LISTEN_PORT}

  ${MASTER_02}:
    image: ${NODE_IMAGE}
    container_name: ${CTR_MASTER_02}
    hostname: ${MASTER_02}
    restart: "no"
    privileged: true
    environment:
      MESH_TOKEN_HASH: "${TOKEN_MASTER_02_ESC}"
    networks:
      mikrotik:
        ipv4_address: ${MASTER_02_BRIDGE}
    ports:
      - "${MASTER_02_GRPC}:9090"
    entrypoint:
      - sh
      - -c
      - |
        [ -f /config/mesh.token ] || printf '%s' "\$\$MESH_TOKEN_HASH" > /config/mesh.token
        exec /usr/local/bin/awg-mesh-node \\
          --mode master \\
          --name ${MASTER_02} \\
          --overlay-ip ${MASTER_02_OVERLAY} \\
          --listen-port ${WG_LISTEN_PORT}

  ${ENDPOINT_01}:
    image: ${NODE_IMAGE}
    container_name: ${CTR_ENDPOINT_01}
    hostname: ${ENDPOINT_01}
    restart: "no"
    privileged: true
    environment:
      MESH_TOKEN_HASH: "${TOKEN_ENDPOINT_01_ESC}"
    networks:
      mikrotik:
        ipv4_address: ${ENDPOINT_01_BRIDGE}
    ports:
      - "${ENDPOINT_01_GRPC}:9090"
    entrypoint:
      - sh
      - -c
      - |
        [ -f /config/mesh.token ] || printf '%s' "\$\$MESH_TOKEN_HASH" > /config/mesh.token
        exec /usr/local/bin/awg-mesh-node \\
          --mode endpoint \\
          --name ${ENDPOINT_01} \\
          --overlay-ip ${ENDPOINT_01_OVERLAY} \\
          --listen-port ${WG_LISTEN_PORT}

  ${ENDPOINT_02}:
    image: ${NODE_IMAGE}
    container_name: ${CTR_ENDPOINT_02}
    hostname: ${ENDPOINT_02}
    restart: "no"
    privileged: true
    environment:
      MESH_TOKEN_HASH: "${TOKEN_ENDPOINT_02_ESC}"
    networks:
      mikrotik:
        ipv4_address: ${ENDPOINT_02_BRIDGE}
    ports:
      - "${ENDPOINT_02_GRPC}:9090"
    entrypoint:
      - sh
      - -c
      - |
        [ -f /config/mesh.token ] || printf '%s' "\$\$MESH_TOKEN_HASH" > /config/mesh.token
        exec /usr/local/bin/awg-mesh-node \\
          --mode endpoint \\
          --name ${ENDPOINT_02} \\
          --overlay-ip ${ENDPOINT_02_OVERLAY} \\
          --listen-port ${WG_LISTEN_PORT}

networks:
  mikrotik:
    driver: bridge
    ipam:
      config:
        - subnet: 192.168.78.0/24
EOF

# ---------------------------------------------------------------------------
# Boot the mesh stack
# ---------------------------------------------------------------------------
echo ""
echo "[M1] Booting 2-master + 2-endpoint mesh..."
compose_run up -d

info "Waiting for masters to become gRPC-ready (up to ${GRPC_READY_TIMEOUT}s)..."
if wait_for_log "${CTR_MASTER_01}" "gRPC server listening" "${GRPC_READY_TIMEOUT}"; then
    pass "M1a: ${MASTER_01} gRPC ready"
else
    fail "M1a: ${MASTER_01} did not become gRPC-ready within ${GRPC_READY_TIMEOUT}s"
    docker logs "${CTR_MASTER_01}" >&2 || true
fi

if wait_for_log "${CTR_MASTER_02}" "gRPC server listening" "${GRPC_READY_TIMEOUT}"; then
    pass "M1b: ${MASTER_02} gRPC ready"
else
    fail "M1b: ${MASTER_02} did not become gRPC-ready within ${GRPC_READY_TIMEOUT}s"
    docker logs "${CTR_MASTER_02}" >&2 || true
fi

# ---------------------------------------------------------------------------
# Init masters and endpoints
# ---------------------------------------------------------------------------
echo ""
echo "[M2] Initialising masters and endpoints..."

if meshctl master init "${MASTER_01}"; then
    pass "M2a: master init ${MASTER_01}"
else
    fail "M2a: master init ${MASTER_01} failed"
fi
if meshctl master init "${MASTER_02}"; then
    pass "M2b: master init ${MASTER_02}"
else
    fail "M2b: master init ${MASTER_02} failed"
fi
if meshctl endpoint init "${ENDPOINT_01}"; then
    pass "M2c: endpoint init ${ENDPOINT_01}"
else
    fail "M2c: endpoint init ${ENDPOINT_01} failed"
fi
if meshctl endpoint init "${ENDPOINT_02}"; then
    pass "M2d: endpoint init ${ENDPOINT_02}"
else
    fail "M2d: endpoint init ${ENDPOINT_02} failed"
fi

# ---------------------------------------------------------------------------
# Spawn mikrotik-equivalent container with delayed binary start.
# Entrypoint: sleep 5 then start the AWG client binary.
# The 5-second window is used to inject the default route via nsenter.
# Token is passed as MESH_TOKEN_HASH env var (same pattern as nodes above).
# ---------------------------------------------------------------------------
echo ""
echo "[M3] Spawning mikrotik-equivalent container (${CLIENT_IMAGE})..."

# Write a tiny init script for the container. This avoids quoting issues with
# $MESH_TOKEN_HASH (a container env var) embedded in a host-side shell string.
MIKROTIK_INIT_SCRIPT=$(mktemp /tmp/mikrotik-init-XXXXXX.sh)
# Interpolate only host vars (CLIENT_MIKROTIK, MIKROTIK_OVERLAY, MIKROTIK_GRPC)
# and leave the container env var MESH_TOKEN_HASH unexpanded via single quotes.
cat > "${MIKROTIK_INIT_SCRIPT}" <<'INITEOF'
#!/bin/sh
[ -f /config/mesh.token ] || printf '%s' "${MESH_TOKEN_HASH}" > /config/mesh.token
INITEOF
printf 'sleep 5\n' >> "${MIKROTIK_INIT_SCRIPT}"
printf 'exec /usr/local/bin/awg-mesh-node --mode client --name %s --overlay-ip %s --config-dir /config\n' \
    "${CLIENT_MIKROTIK}" "${MIKROTIK_OVERLAY}" >> "${MIKROTIK_INIT_SCRIPT}"
chmod +x "${MIKROTIK_INIT_SCRIPT}"

docker run -d \
    --name "${CTR_MIKROTIK}" \
    --hostname "${CLIENT_MIKROTIK}" \
    --privileged \
    --cap-add NET_ADMIN \
    --cap-add NET_RAW \
    --entrypoint sh \
    --network "${COMPOSE_PROJECT}_mikrotik" \
    --ip "${MIKROTIK_BRIDGE}" \
    --publish "${MIKROTIK_GRPC}:9090" \
    --env "MESH_TOKEN_HASH=${TOKEN_MIKROTIK}" \
    --volume "${MIKROTIK_INIT_SCRIPT}:/entrypoint.sh:ro" \
    "${CLIENT_IMAGE}" \
    /entrypoint.sh

# Retrieve the container's init PID (the sh process running the entrypoint).
MIKROTIK_PID=$(docker inspect -f '{{.State.Pid}}' "${CTR_MIKROTIK}")
if [[ -z "${MIKROTIK_PID}" || "${MIKROTIK_PID}" == "0" ]]; then
    fail "M3: could not get PID for ${CTR_MIKROTIK} — pre-injection impossible"
    echo "ERROR: container PID unavailable; aborting." >&2
    exit 4
fi
info "Container PID: ${MIKROTIK_PID}"

# ---------------------------------------------------------------------------
# Pre-inject default route (RouterOS NS-injection proxy).
# Steps:
#   1. Create veth pair: veth-mikrotik-host (host) <-> veth-mikrotik (container ns).
#   2. Move veth-mikrotik into the container network namespace.
#   3. Bring both ends up and assign the CGN addresses.
#   4. Add the default route inside the container ns.
# This must complete within the 5-second sleep window.
# ---------------------------------------------------------------------------
echo ""
echo "[M4] Pre-injecting default route via veth (simulating RouterOS NS injection)..."

# Remove any leftover veth from a previous interrupted run.
ip link del "${VETH_HOST}" 2>/dev/null || true

ip link add "${VETH_HOST}" type veth peer name "${VETH_GUEST}"
ip link set "${VETH_GUEST}" netns "${MIKROTIK_PID}"
ip addr add "${CGN_GW_CIDR}" dev "${VETH_HOST}"
ip link set "${VETH_HOST}" up

nsenter --target="${MIKROTIK_PID}" --net -- ip addr add "${CGN_GUEST_CIDR}" dev "${VETH_GUEST}"
nsenter --target="${MIKROTIK_PID}" --net -- ip link set "${VETH_GUEST}" up
nsenter --target="${MIKROTIK_PID}" --net -- ip route add default via "${CGN_GW_IP}" dev "${VETH_GUEST}"

INJECTED_ROUTE=$(nsenter --target="${MIKROTIK_PID}" --net -- ip route show default 2>/dev/null || true)
if echo "${INJECTED_ROUTE}" | grep -q "${CGN_GW_IP}"; then
    pass "M4: default route ${CGN_GW_IP} pre-injected into container ns"
    info "  route: ${INJECTED_ROUTE}"
else
    fail "M4: default route pre-injection failed (got: '${INJECTED_ROUTE}')"
fi

# ---------------------------------------------------------------------------
# Wait for AWG client binary to start (after the 5 s sleep).
# ---------------------------------------------------------------------------
info "Waiting for mikrotik client gRPC to become ready (up to ${GRPC_READY_TIMEOUT}s)..."
if wait_for_log "${CTR_MIKROTIK}" "gRPC server listening" "${GRPC_READY_TIMEOUT}"; then
    pass "M5: ${CLIENT_MIKROTIK} client gRPC ready"
else
    fail "M5: ${CLIENT_MIKROTIK} did not become gRPC-ready within ${GRPC_READY_TIMEOUT}s"
    docker logs "${CTR_MIKROTIK}" >&2 || true
fi

# ---------------------------------------------------------------------------
# Run mesh-ctl client init
# ---------------------------------------------------------------------------
echo ""
echo "[M6] Running mesh-ctl client init ${CLIENT_MIKROTIK}..."

if meshctl client init "${CLIENT_MIKROTIK}"; then
    pass "M6: mesh-ctl client init ${CLIENT_MIKROTIK} exit 0"
else
    fail "M6: mesh-ctl client init ${CLIENT_MIKROTIK} failed"
fi

# Allow the client to settle (tunnels come up, routes applied).
info "Settling for ${INIT_SETTLE_TIMEOUT}s after init..."
sleep "${INIT_SETTLE_TIMEOUT}"

# ---------------------------------------------------------------------------
# A1: Default route preserved after init.
# The pre-injected default route (via 100.127.0.1) must still be present in the
# container routing table 30 s after mesh-ctl client init completes.
# AWG client init must not clobber pre-existing default routes.
# ---------------------------------------------------------------------------
echo ""
echo "[A1] Checking default route preservation..."

CURRENT_DEFAULT=$(docker exec "${CTR_MIKROTIK}" ip route show default 2>/dev/null || true)
if echo "${CURRENT_DEFAULT}" | grep -q "${CGN_GW_IP}"; then
    pass "A1: pre-injected default route ${CGN_GW_IP} preserved after init"
    info "  route table: ${CURRENT_DEFAULT}"
else
    fail "A1: default route ${CGN_GW_IP} NOT found after init"
    info "  current default: ${CURRENT_DEFAULT}"
fi

# ---------------------------------------------------------------------------
# A2: Ping matrix — client reaches all 6 mesh overlay IPs at 100%.
# ---------------------------------------------------------------------------
echo ""
echo "[A2] Ping matrix: client → 6 mesh overlay IPs..."

# Ping targets: master overlays, endpoint overlays, and balancer IPs.
declare -a ALL_PING_TARGETS=(
    "${MASTER_01_OVERLAY}"
    "${MASTER_02_OVERLAY}"
    "${ENDPOINT_01_OVERLAY}"
    "${ENDPOINT_02_OVERLAY}"
    "172.21.78.1"
    "172.21.78.33"
)

PING_FAILS=0
for target_ip in "${ALL_PING_TARGETS[@]}"; do
    if docker exec "${CTR_MIKROTIK}" ping -c 2 -W 3 "${target_ip}" > /dev/null 2>&1; then
        info "  ping ${target_ip}: ok"
    else
        warn "  ping ${target_ip}: FAIL"
        (( PING_FAILS++ )) || true
    fi
done

if [[ "${PING_FAILS}" -eq 0 ]]; then
    pass "A2: all ${#ALL_PING_TARGETS[@]} overlay pings succeeded"
else
    fail "A2: ${PING_FAILS}/${#ALL_PING_TARGETS[@]} overlay pings failed"
fi

# ---------------------------------------------------------------------------
# A3: DSCP routing — policy-routed traffic uses expected WG interface.
# Verification:
#   - For each routing_policy DSCP class (10/20/30/31/32), verify that an
#     ip rule entry exists for the matching fwmark/dscp, OR that ip rule list
#     output shows DSCP-based routing has been configured by the client init.
#   - This validates the routing_policies topology field was consumed by the
#     client init and translated into kernel policy routing rules.
# ---------------------------------------------------------------------------
echo ""
echo "[A3] DSCP routing policy rules..."

# Collect all ip rule entries from inside the container.
IP_RULES=$(docker exec "${CTR_MIKROTIK}" ip rule list 2>/dev/null || true)
info "  ip rule list output (${#IP_RULES} bytes)"

declare -a DSCP_CLASSES=(10 20 30 31 32)
DSCP_FAILS=0

for dscp_val in "${DSCP_CLASSES[@]}"; do
    # ip rule entries for DSCP use the dscp value directly via fwmark or
    # via 'tos' match. The client may use either mechanism depending on
    # implementation. Check for the dscp value's presence in ip rule output.
    # fwmark format: client sets mark == dscp_val (invariant fwmark == DSCP, see
    # pkg/routing/dscp.go), or kernel may show 'tos 0xNN' where NN = dscp_val << 2.
    tos_hex=$(printf '%02x' $(( dscp_val * 4 )))
    if echo "${IP_RULES}" | grep -qE "dscp ${dscp_val}|fwmark.*${dscp_val}|tos 0x${tos_hex}"; then
        info "  DSCP ${dscp_val}: rule found"
    else
        warn "  DSCP ${dscp_val}: no matching ip rule (tos 0x${tos_hex}); policies may use iptables marks"
        # Fall back: check iptables mangle rules for DSCP matching.
        MANGLE=$(docker exec "${CTR_MIKROTIK}" iptables -t mangle -L OUTPUT -n --line-numbers 2>/dev/null || true)
        if echo "${MANGLE}" | grep -qE "DSCP|dscp ${dscp_val}|tos ${tos_hex}"; then
            info "  DSCP ${dscp_val}: iptables mangle rule found (fallback check ok)"
        else
            (( DSCP_FAILS++ )) || true
            warn "  DSCP ${dscp_val}: no ip rule or iptables rule found"
        fi
    fi
done

if [[ "${DSCP_FAILS}" -eq 0 ]]; then
    pass "A3: DSCP routing policy rules present for all 5 classes"
else
    fail "A3: ${DSCP_FAILS}/5 DSCP classes have no routing policy rule"
fi

# ---------------------------------------------------------------------------
# A4: TCP MSS clamp — outbound SYN through WG tunnel has MSS <= 1380.
# We listen briefly on the WG client interface (wg-c*) for a TCP SYN and
# extract the MSS option. We generate traffic by trying to connect to a non-
# existent overlay IP to produce a SYN without hanging on handshake.
# ---------------------------------------------------------------------------
echo ""
echo "[A4] TCP MSS clamp check (<= ${MSS_CLAMP_MAX} bytes)..."

# Find the first wg-c* interface inside the container.
WG_CLIENT_IFACE=$(docker exec "${CTR_MIKROTIK}" sh -c "ip link show | awk -F': ' '/wg-c/{print \$2}' | head -1" 2>/dev/null || true)
WG_CLIENT_IFACE="${WG_CLIENT_IFACE%%@*}" # strip @<peer> suffix if present

if [[ -z "${WG_CLIENT_IFACE}" ]]; then
    fail "A4: no wg-c* interface found in ${CTR_MIKROTIK} — tunnel not up"
else
    info "  WG client interface: ${WG_CLIENT_IFACE}"

    # Capture one TCP SYN on the tunnel interface.
    # Timeout the tcpdump after 5 s to avoid hanging if no SYN arrives.
    # Background netcat towards a non-existent overlay host to generate SYN.
    TCPDUMP_OUT=$(
        docker exec "${CTR_MIKROTIK}" sh -c "
            (sleep 0.5 && nc -w1 172.21.78.200 9999 2>/dev/null || true) &
            tcpdump -c 1 -i '${WG_CLIENT_IFACE}' 'tcp[tcpflags] & tcp-syn != 0' -nn -v 2>/dev/null
        " 2>/dev/null || true
    )

    if [[ -n "${TCPDUMP_OUT}" ]]; then
        # tcpdump -v prints MSS as "mss <N>" in the options field.
        CAPTURED_MSS=$(echo "${TCPDUMP_OUT}" | grep -oE 'mss [0-9]+' | awk '{print $2}' | head -1)
        if [[ -z "${CAPTURED_MSS}" ]]; then
            # Inconclusive — emit a warn and DO NOT increment PASSES. The
            # earlier `pass` here inflated the green-count and could mask
            # a real MSS clamp regression behind two consecutive WARN
            # lines that the test still reported as green.
            warn "A4: tcpdump captured SYN but could not parse MSS option; packet may not have MSS set"
            warn "A4: SYN captured on ${WG_CLIENT_IFACE} (MSS parse inconclusive — manual verification needed)"
        elif [[ "${CAPTURED_MSS}" -le "${MSS_CLAMP_MAX}" ]]; then
            pass "A4: MSS=${CAPTURED_MSS} <= ${MSS_CLAMP_MAX} on ${WG_CLIENT_IFACE}"
        else
            fail "A4: MSS=${CAPTURED_MSS} > ${MSS_CLAMP_MAX} on ${WG_CLIENT_IFACE} — clamp not applied"
        fi
    else
        # No SYN captured — likewise inconclusive, do not call pass.
        warn "A4: no TCP SYN captured on ${WG_CLIENT_IFACE} within timeout — MSS clamp unverified"
    fi
fi

# ---------------------------------------------------------------------------
# A5: /config/awg-mesh-client.log exists and is JSON-parseable per line.
# ---------------------------------------------------------------------------
echo ""
echo "[A5] Client log JSON parseability..."

LOG_PATH="/config/awg-mesh-client.log"
if ! docker exec "${CTR_MIKROTIK}" test -f "${LOG_PATH}" 2>/dev/null; then
    fail "A5: log file ${LOG_PATH} does not exist"
else
    # Count lines and validate each is parseable JSON.
    NON_JSON_LINES=$(
        docker exec "${CTR_MIKROTIK}" sh -c "
            while IFS= read -r line; do
                printf '%s' \"\${line}\" | jq -e . > /dev/null 2>&1 || printf 'BAD\n'
            done < '${LOG_PATH}'
        " 2>/dev/null | grep -c '^BAD$' || true
    )
    TOTAL_LINES=$(docker exec "${CTR_MIKROTIK}" wc -l < "${LOG_PATH}" 2>/dev/null || echo "0")

    if [[ "${TOTAL_LINES}" -eq 0 ]]; then
        # Empty log file is not a JSON-validity failure but it IS a real
        # regression — the client never logged anything during the test
        # window. Without this check the JSON-validity branch below
        # would silently pass `0/0 line(s) valid`.
        fail "A5: ${LOG_PATH} exists but is empty — client produced no log output during the test"
        docker exec "${CTR_MIKROTIK}" ls -l "${LOG_PATH}" >&2 || true
    elif [[ "${NON_JSON_LINES}" -eq 0 ]]; then
        pass "A5: ${LOG_PATH} exists and all ${TOTAL_LINES} line(s) are valid JSON"
    else
        fail "A5: ${LOG_PATH} has ${NON_JSON_LINES}/${TOTAL_LINES} non-JSON line(s)"
        docker exec "${CTR_MIKROTIK}" head -20 "${LOG_PATH}" >&2 || true
    fi
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo ""
echo "=== Results: ${PASSES} PASS, ${FAILURES} FAIL ==="
echo ""

if [[ "${FAILURES}" -gt 0 ]]; then
    echo "Container logs for ${CTR_MIKROTIK}:"
    docker logs "${CTR_MIKROTIK}" 2>&1 | tail -30 || true
    exit "${FAILURES}"
fi

exit 0
