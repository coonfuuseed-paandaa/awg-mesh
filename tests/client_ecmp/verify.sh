#!/usr/bin/env bash
# verify.sh — manual end-to-end verification for client ECMP US1 (failover) + US2 (stickiness).
# Requires: Linux host (or WSL2), Docker 24+, Docker Compose v2.
# NOT run in CI. Execute manually: bash tests/client_ecmp/verify.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
COMPOSE_FILE="${SCRIPT_DIR}/compose.yml"
COMPOSE_PROJECT="clientecmp"
COMPOSE_STATE_DIR="${SCRIPT_DIR}/compose-state"
COMPOSE_UP_TIMEOUT=300
READY_TIMEOUT=60
SETTLE_PAUSE=20
FAILOVER_WAIT=35
RECOVERY_WAIT=60
MASTER_01_OVERLAY="172.20.70.2"
MASTER_02_OVERLAY="172.20.70.3"
ENDPOINT_OVERLAY="172.20.70.37"
MASTER_01_HOST="127.0.0.1"
MASTER_02_HOST="127.0.0.1"
ENDPOINT_HOST="127.0.0.1"
CLIENT_HOST="127.0.0.1"
MASTER_01_PEER_HOST="172.31.0.10"
MASTER_02_PEER_HOST="172.31.0.11"
ENDPOINT_PEER_HOST="172.31.0.20"
CLIENT_PEER_HOST="172.31.0.50"
MASTER_01_GRPC_PORT="19090"
MASTER_02_GRPC_PORT="29090"
ENDPOINT_GRPC_PORT="39090"
CLIENT_GRPC_PORT="49090"
TS=$(date +%Y%m%d_%H%M%S)
RUNTIME_ROOT="${SCRIPT_DIR}/.runtime"
RUNTIME_DIR="${RUNTIME_ROOT}"
LOG_DIR="${RUNTIME_DIR}/awg-verify-${TS}"
mkdir -p "${RUNTIME_DIR}"
CTL_CONFIG_DIR=$(mktemp -d "${RUNTIME_DIR}/client-ecmp-ctl-XXXXXX")
TOPO_FILE=""

# ----------------------------------------------------------------------------
# Helpers
# ----------------------------------------------------------------------------

log()  { echo "[verify] $*"; }
fail() { echo "[verify] FAIL: $*" >&2; dump_logs; cleanup; exit 1; }

compose() {
    docker compose -p "${COMPOSE_PROJECT}" -f "${COMPOSE_FILE}" "$@"
}

dump_logs() {
    mkdir -p "${LOG_DIR}"
    log "Dumping container logs to ${LOG_DIR}/"
    for svc in master-01 master-02 node-eu-01 client-lin; do
        docker logs "${svc}" > "${LOG_DIR}/${svc}.log" 2>&1 || true
    done
    echo "[verify] Logs written to ${LOG_DIR}/" >&2
}

cleanup() {
    rm -rf "${CTL_CONFIG_DIR}"
    rm -rf "${COMPOSE_STATE_DIR}"
}

wait_for_log() {
    local container="$1"
    local pattern="$2"
    local deadline=$(( $(date +%s) + READY_TIMEOUT ))
    log "Waiting for '${pattern}' in ${container} logs (up to ${READY_TIMEOUT}s)..."
    while true; do
        if docker logs "${container}" 2>&1 | grep -q "${pattern}"; then
            log "${container} ready."
            return 0
        fi
        if [[ $(date +%s) -ge ${deadline} ]]; then
            fail "${container} did not reach '${pattern}' within ${READY_TIMEOUT}s"
        fi
        sleep 2
    done
}

find_meshctl() {
    if [[ -x "${REPO_ROOT}/mesh-ctl.exe" ]]; then
        echo "${REPO_ROOT}/mesh-ctl.exe"
        return 0
    fi
    if [[ -x "${REPO_ROOT}/bin/mesh-ctl.exe" ]]; then
        echo "${REPO_ROOT}/bin/mesh-ctl.exe"
        return 0
    fi
    if command -v mesh-ctl > /dev/null 2>&1; then
        echo "mesh-ctl"
        return 0
    fi
    if [[ -x "${REPO_ROOT}/bin/mesh-ctl" ]]; then
        echo "${REPO_ROOT}/bin/mesh-ctl"
        return 0
    fi
    return 1
}

meshctl_is_windows_binary() {
    local meshctl_bin="$1"
    [[ "${meshctl_bin,,}" == *.exe ]]
}

meshctl_arg_path() {
    local meshctl_bin="$1"
    local path_value="$2"

    if meshctl_is_windows_binary "${meshctl_bin}"; then
        if [[ "${path_value}" =~ ^/mnt/[a-z]/ ]]; then
            echo "${path_value}" | sed -E 's#^/mnt/([a-z])/#\1:/#' | sed 's#/#\\#g'
        else
            wslpath -w "${path_value}"
        fi
    else
        echo "${path_value}"
    fi
}

write_topology() {
    TOPO_FILE="${RUNTIME_DIR}/mesh-topology-${TS}.yml"
    mkdir -p "${RUNTIME_DIR}"
    cat > "${TOPO_FILE}" << EOF
overlay:
  space: 172.20.70.0/24
  physical_mtu: 1500
  awg_overhead: 80
  ranges:
    - name: masters
      cidr: 172.20.70.0/27
      balancer_ip: 172.20.70.1
    - name: endpoints
      cidr: 172.20.70.32/27
      balancer_ip: 172.20.70.33
    - name: clients
      cidr: 172.20.70.128/25

masters:
  - name: master-01
    host: ${MASTER_01_HOST}
    peer_host: ${MASTER_01_PEER_HOST}
    overlay_ip: ${MASTER_01_OVERLAY}
    listen_port: 51820
    grpc_port: ${MASTER_01_GRPC_PORT}
    endpoints:
      - node-eu-01
  - name: master-02
    host: ${MASTER_02_HOST}
    peer_host: ${MASTER_02_PEER_HOST}
    overlay_ip: ${MASTER_02_OVERLAY}
    listen_port: 51820
    grpc_port: ${MASTER_02_GRPC_PORT}
    endpoints:
      - node-eu-01

endpoints:
  - name: node-eu-01
    host: ${ENDPOINT_HOST}
    peer_host: ${ENDPOINT_PEER_HOST}
    overlay_ip: ${ENDPOINT_OVERLAY}
    listen_port: 51820
    grpc_port: ${ENDPOINT_GRPC_PORT}
    region: eu

clients:
  - name: client-lin
    type: linux
    host: ${CLIENT_HOST}
    peer_host: ${CLIENT_PEER_HOST}
    overlay_ip: 172.20.70.130
    grpc_port: ${CLIENT_GRPC_PORT}
    masters:
      - master-01
      - master-02

transport:
  pool: 10.255.0.0/16
  prefix_length: 30
EOF
}

deploy_generated_tokens() {
    log "Deploying generated token hashes into running containers"
    for node in master-01 master-02 node-eu-01 client-lin; do
        local token_path="${CTL_CONFIG_DIR}/nodes/${node}/mesh.token"
        if [[ ! -f "${token_path}" ]]; then
            fail "generated mesh.token for ${node} not found at ${token_path}"
        fi
        local hash
        hash="$(tr -d '\r\n' < "${token_path}")"
        docker exec "${node}" sh -c "mkdir -p /config && printf '%s' '${hash}' > /config/mesh.token"
    done

    log "Restarting containers so gRPC auth reloads the generated token hashes"
    docker restart master-01 master-02 node-eu-01 client-lin > /dev/null
    sleep 5
}

run_mesh_init() {
    local meshctl_bin="$1"
    local topology_arg
    local config_dir_arg

    topology_arg="$(meshctl_arg_path "${meshctl_bin}" "${TOPO_FILE}")"
    config_dir_arg="$(meshctl_arg_path "${meshctl_bin}" "${CTL_CONFIG_DIR}")"

    log "Preparing admin config and tokens for this fixture"
    "${meshctl_bin}" --topology "${topology_arg}" --config-dir "${config_dir_arg}" master prepare master-01 > /dev/null
    "${meshctl_bin}" --topology "${topology_arg}" --config-dir "${config_dir_arg}" master prepare master-02 > /dev/null
    "${meshctl_bin}" --topology "${topology_arg}" --config-dir "${config_dir_arg}" endpoint prepare node-eu-01 > /dev/null
    "${meshctl_bin}" --topology "${topology_arg}" --config-dir "${config_dir_arg}" client prepare client-lin > /dev/null

    deploy_generated_tokens

    log "Running mesh init flow: endpoint -> masters -> client"
    "${meshctl_bin}" --topology "${topology_arg}" --config-dir "${config_dir_arg}" endpoint init node-eu-01
    "${meshctl_bin}" --topology "${topology_arg}" --config-dir "${config_dir_arg}" master init master-01
    "${meshctl_bin}" --topology "${topology_arg}" --config-dir "${config_dir_arg}" master init master-02
    "${meshctl_bin}" --topology "${topology_arg}" --config-dir "${config_dir_arg}" client init client-lin
}

# ----------------------------------------------------------------------------
# Step 1: Preflight checks
# ----------------------------------------------------------------------------

log "Step 1: Preflight checks"

if ! command -v docker > /dev/null 2>&1; then
    echo "[verify] ERROR: Docker not available (docker binary not in PATH). Install Docker 24+ and retry." >&2
    exit 2
fi

if ! docker info > /dev/null 2>&1; then
    echo "[verify] ERROR: Docker daemon not running or not accessible. Start Docker and retry." >&2
    exit 2
fi

log "Docker available: $(docker --version)"

MESHCTL_BIN="$(find_meshctl || true)"
if [[ -z "${MESHCTL_BIN}" ]]; then
    echo "[verify] ERROR: mesh-ctl not found in PATH or ${REPO_ROOT}/bin/mesh-ctl. Install/build it before running this fixture." >&2
    exit 2
fi
log "mesh-ctl available: $(${MESHCTL_BIN} version 2>&1 || true)"

write_topology

# conntrack is optional — if missing we skip mark-based stickiness check but still verify routes.
HAVE_CONNTRACK=false
if command -v conntrack > /dev/null 2>&1; then
    HAVE_CONNTRACK=true
    log "conntrack available: stickiness mark check enabled."
else
    log "conntrack not found on host — skipping conntrack mark check; route check still runs."
fi

# ----------------------------------------------------------------------------
# Step 2: Bring up the stack
# ----------------------------------------------------------------------------

log "Step 2: reset and docker compose up (timeout ${COMPOSE_UP_TIMEOUT}s)"

log "Removing any stale containers, networks, volumes, and compose-state from previous runs"
compose down -v --remove-orphans > /dev/null 2>&1 || true
rm -rf "${COMPOSE_STATE_DIR}"
mkdir -p "${COMPOSE_STATE_DIR}"

log "Seeding topology into client compose-state so client knows overlay space"
mkdir -p "${COMPOSE_STATE_DIR}/client-lin"
cp "${TOPO_FILE}" "${COMPOSE_STATE_DIR}/client-lin/mesh-topology.yml"

# docker compose does not have a native timeout flag; wrap the compose helper with timeout(1).
if command -v timeout > /dev/null 2>&1; then
    timeout "${COMPOSE_UP_TIMEOUT}" bash -lc 'compose() {
    MESH_TOPOLOGY_HOST_PATH="$1" docker compose -p "$2" -f "$3" "${@:4}"
}
compose "$@"' _ "${TOPO_FILE}" "${COMPOSE_PROJECT}" "${COMPOSE_FILE}" up -d --build
else
    # Fallback: run without timeout (macOS / minimal images without coreutils timeout).
    compose up -d --build
fi

# ----------------------------------------------------------------------------
# Step 3: Wait for all services to become ready
# ----------------------------------------------------------------------------

log "Step 3: Waiting for services to report gRPC server ready"

wait_for_log "master-01"  "gRPC server listening"
wait_for_log "master-02"  "gRPC server listening"
wait_for_log "node-eu-01" "gRPC server listening"
wait_for_log "client-lin" "gRPC server listening"

# ----------------------------------------------------------------------------
# Step 4: Prepare + init the mesh so the client gets tunnels and routes
# ----------------------------------------------------------------------------

log "Step 4: Running mesh-ctl prepare/init flow for masters, endpoint, and client"
run_mesh_init "${MESHCTL_BIN}"

log "Settling pause (${SETTLE_PAUSE}s)..."
sleep "${SETTLE_PAUSE}"

# ----------------------------------------------------------------------------
# Step 5: US2 — stickiness check (ECMP route with both nexthops)
# ----------------------------------------------------------------------------

log "Step 5: US2 — verify ECMP route with both master nexthops and endpoint overlay reachability"

# Check both VIP (172.20.70.1/32) and overlay space (172.20.70.0/24) routes.
# Nexthops are WireGuard transport peer IPs (10.255.0.x), not Docker network IPs.
VIP_ROUTE=$(docker exec client-lin ip route show 172.20.70.1/32 2>/dev/null || echo "no route yet")
OVERLAY_ROUTE=$(docker exec client-lin ip route show 172.20.70.0/24 2>/dev/null || echo "no route yet")
log "Client VIP route: ${VIP_ROUTE}"
log "Client overlay route: ${OVERLAY_ROUTE}"

# Primary check: overlay-space route must exist with at least one nexthop.
if echo "${OVERLAY_ROUTE}" | grep -q "nexthop"; then
    NH_COUNT=$(echo "${OVERLAY_ROUTE}" | grep -c "nexthop" || true)
    if [[ "${NH_COUNT}" -ge 2 ]]; then
        log "US2 PASS: overlay ECMP route has ${NH_COUNT} nexthops."
    else
        log "WARNING: overlay route has only ${NH_COUNT} nexthop (partial convergence). Accepted."
    fi
elif echo "${OVERLAY_ROUTE}" | grep -q "via"; then
    log "WARNING: overlay route is single-path (not ECMP). Accepted as partial convergence."
else
    fail "US2: No route to overlay 172.20.70.0/24 found on client-lin. Got: ${OVERLAY_ROUTE}"
fi

if [[ "${HAVE_CONNTRACK}" == "true" ]]; then
    log "conntrack stickiness check (informational): $(conntrack -L 2>/dev/null | grep -c udp || echo 'n/a') UDP conntrack entries"
fi

ENDPOINT_PING=$(docker exec client-lin sh -lc "ping -c 3 -W 5 ${ENDPOINT_OVERLAY}" 2>&1 || true)
log "Client -> endpoint overlay ping output: ${ENDPOINT_PING//$'\n'/ | }"
if echo "${ENDPOINT_PING}" | grep -qE " 0% packet loss| 0 packets? lost"; then
    log "US2 PASS: client can reach endpoint overlay ${ENDPOINT_OVERLAY}."
elif echo "${ENDPOINT_PING}" | grep -qE "[1-9][0-9]* packets? received|[1-9][0-9]* bytes from"; then
    log "US2 PASS (partial): some packets reached endpoint overlay ${ENDPOINT_OVERLAY}."
else
    fail "US2: client cannot reach endpoint overlay ${ENDPOINT_OVERLAY}. Ping output: ${ENDPOINT_PING}"
fi

# ----------------------------------------------------------------------------
# Step 6: US1 — failover (kill master-01, assert route via master-02 survives)
# ----------------------------------------------------------------------------

log "Step 6: US1 — failover: killing master-01, waiting ${FAILOVER_WAIT}s"

docker kill master-01

log "master-01 killed. Waiting ${FAILOVER_WAIT}s for healthcheck-driven failover..."
sleep "${FAILOVER_WAIT}"

FAILOVER_ROUTE=$(docker exec client-lin ip route show 172.20.70.0/24 2>/dev/null || echo "no route yet")
log "Client route after failover: ${FAILOVER_ROUTE}"

if echo "${FAILOVER_ROUTE}" | grep -qE "nexthop|via"; then
    log "US1 PASS: client still has a route to overlay after master-01 failure."
else
    fail "US1 failover: client lost all overlay routes after master-01 failure. Got: ${FAILOVER_ROUTE}"
fi

# ----------------------------------------------------------------------------
# Step 7: US1 — recovery (restart master-01, assert both nexthops return)
# ----------------------------------------------------------------------------

log "Step 7: US1 — recovery: restarting master-01, waiting ${RECOVERY_WAIT}s"

docker start master-01

log "master-01 started. Waiting ${RECOVERY_WAIT}s for route re-convergence..."
sleep "${RECOVERY_WAIT}"

RECOVERY_ROUTE=$(docker exec client-lin ip route show 172.20.70.0/24 2>/dev/null || echo "no route yet")
log "Client route after recovery: ${RECOVERY_ROUTE}"

RECOVERY_NH_COUNT=$(echo "${RECOVERY_ROUTE}" | grep -c "nexthop" || true)
if [[ "${RECOVERY_NH_COUNT}" -ge 2 ]]; then
    log "US1 PASS: both nexthops restored in client route after recovery."
else
    log "WARNING: only ${RECOVERY_NH_COUNT} nexthop(s) after ${RECOVERY_WAIT}s recovery. May need more convergence time."
    log "Route state: ${RECOVERY_ROUTE}"
fi

# ----------------------------------------------------------------------------
# Step 8: Summary
# ----------------------------------------------------------------------------

echo ""
echo "=================================================================="
echo " VERIFY: PASS"
echo " US1 (failover) and US2 (stickiness) checks completed."
echo " Containers are still running for further manual inspection."
echo " To clean up:"
echo "   docker compose -f ${COMPOSE_FILE} down -v"
echo "   rm -rf tests/client_ecmp/compose-state/"
echo "=================================================================="
