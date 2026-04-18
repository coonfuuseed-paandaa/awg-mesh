#!/usr/bin/env bash
# issue-99-allowedips.sh — Docker integration test for AllowedIPs overlay fix.
# local tracker issue #99 (CRITICAL): master->endpoint AddPeer must include
# overlay range CIDRs + master overlay /32 in allowed_ips; master must install
# per-peer overlay /32 routes.
#
# What this script validates:
#   A1  2-master + 2-endpoint mesh boots with Docker.
#   A2  mesh-ctl master init + endpoint init complete without error.
#   A3  mesh-ctl reconcile completes without error.
#   A4  Endpoint transport.yml contains overlay range CIDRs in allowed_ips.
#   A5  Endpoint transport.yml has overlay_ip populated (not empty string).
#   A6  Master ip route contains <endpoint-overlay>/32 for each endpoint.
#   A7  Overlay ping from master to each endpoint succeeds within 5s.
#   A8  Overlay ping from each endpoint to bound master succeeds within 5s.
#   A9  mesh-ctl status --verify-data-plane returns 0 broken pairs.
#
# Usage (Linux host with Docker):
#   cd <repo-root>
#   bash tests/simulation/issue-99-allowedips.sh
#
# Prerequisites:
#   - Docker running (Linux host or Docker Desktop with Linux containers).
#   - awg-mesh-node:local image built:
#       docker build -t awg-mesh-node:local .
#   - mesh-ctl in PATH or at <repo-root>/bin/mesh-ctl:
#       go install ./cmd/mesh-ctl
#   - CAP_NET_ADMIN available inside containers (privileged: true in compose).
#
# Windows hosts: requires WireGuard kernel modules and CAP_NET_ADMIN inside
# containers, unavailable on Windows Docker Desktop without WSL2 kernel support.
# Run inside WSL2 Ubuntu or a CI Linux runner instead.
#
# Exit: 0 = all checks passed, non-zero = failure count.
set -euo pipefail

# ---------------------------------------------------------------------------
# Platform guard — exit cleanly on non-Linux so CI skips rather than errors.
# ---------------------------------------------------------------------------
if [[ "$(uname -s)" != "Linux" ]]; then
    echo "[A0] SKIP: issue-99-allowedips.sh requires Linux (WireGuard kernel module)."
    echo "     Run inside WSL2 Ubuntu or a CI Linux runner."
    exit 0
fi

# ---------------------------------------------------------------------------
# Paths
# ---------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

# Compose project name — unique per run (PID + timestamp) to avoid collisions
# when two test instances run concurrently on the same host.
COMPOSE_PROJECT="issue99aip-$$-$(date +%s)"

# Mesh node names (max 12 chars each for IFNAMSIZ: "wg-" + 12 = 15 chars).
MASTER_01="mst-a-01"
MASTER_02="mst-a-02"
ENDPOINT_01="ep-b-01"
ENDPOINT_02="ep-b-02"

# Container names (project prefix + service name).
CTR_MASTER_01="${COMPOSE_PROJECT}-${MASTER_01}"
CTR_MASTER_02="${COMPOSE_PROJECT}-${MASTER_02}"
CTR_ENDPOINT_01="${COMPOSE_PROJECT}-${ENDPOINT_01}"
CTR_ENDPOINT_02="${COMPOSE_PROJECT}-${ENDPOINT_02}"

# Timing
GRPC_READY_TIMEOUT=60   # seconds to wait for gRPC server ready log
PING_TIMEOUT=10         # seconds to poll for overlay ping to succeed

# mesh-ctl config dir — scoped to this test run.
CTL_CONFIG_DIR=$(mktemp -d /tmp/issue99aip-ctl-XXXXXX)
TOPO_FILE=$(mktemp /tmp/issue99aip-topo-XXXXXX.yml)
COMPOSE_FILE=$(mktemp /tmp/issue99aip-compose-XXXXXX.yml)

# Overlay addresses — use a range unlikely to conflict with host routing tables.
MASTER_01_OVERLAY="172.22.99.2"
MASTER_02_OVERLAY="172.22.99.3"
ENDPOINT_01_OVERLAY="172.22.99.34"
ENDPOINT_02_OVERLAY="172.22.99.35"

# Bridge addresses (Docker bridge network for management/gRPC connectivity).
MASTER_01_BRIDGE="192.168.99.10"
MASTER_02_BRIDGE="192.168.99.11"
ENDPOINT_01_BRIDGE="192.168.99.20"
ENDPOINT_02_BRIDGE="192.168.99.21"

# Docker image — override with IMAGE env var if needed.
NODE_IMAGE="${IMAGE:-awg-mesh-node:local}"

# ---------------------------------------------------------------------------
# Counters + colours
# ---------------------------------------------------------------------------
FAILURES=0
PASSES=0

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RESET='\033[0m'

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
    echo "[cleanup] Tearing down containers and temp files..."
    docker compose -p "${COMPOSE_PROJECT}" -f "${COMPOSE_FILE}" down -v --remove-orphans 2>/dev/null || true
    rm -f "${TOPO_FILE}" "${COMPOSE_FILE}"
    rm -rf "${CTL_CONFIG_DIR}"
    if [[ "${rc}" -eq 0 && "${FAILURES}" -eq 0 ]]; then
        echo "[cleanup] Done. Test PASSED (${PASSES} checks)."
    else
        echo "[cleanup] Done. Test FAILED (${FAILURES} failure(s), ${PASSES} pass(es))."
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

# ping_overlay <src_container> <dest_overlay_ip> [timeout_seconds]
# Polls with 1s retry until ping succeeds or timeout expires.
ping_overlay() {
    local container="$1"
    local dest_ip="$2"
    local timeout="${3:-${PING_TIMEOUT}}"
    local deadline=$(( $(date +%s) + timeout ))
    while true; do
        if docker exec "${container}" ping -c 1 -W 2 "${dest_ip}" > /dev/null 2>&1; then
            return 0
        fi
        if [[ $(date +%s) -ge ${deadline} ]]; then
            return 1
        fi
        sleep 1
    done
}

# ---------------------------------------------------------------------------
# Preflight checks
# ---------------------------------------------------------------------------
echo ""
echo "=== issue-99-allowedips.sh — AllowedIPs Overlay Integration Test ==="
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
# Generate ephemeral topology
# ---------------------------------------------------------------------------
cat > "${TOPO_FILE}" <<EOF
overlay:
  space: 172.22.99.0/24
  physical_mtu: 1500
  awg_overhead: 80
  ranges:
    - name: masters
      cidr: 172.22.99.0/27
      balancer_ip: 172.22.99.1
    - name: endpoints
      cidr: 172.22.99.32/27
      balancer_ip: 172.22.99.33
    - name: clients
      cidr: 172.22.99.128/25

masters:
  - name: ${MASTER_01}
    host: 127.0.0.1
    peer_host: ${MASTER_01_BRIDGE}
    overlay_ip: ${MASTER_01_OVERLAY}
    listen_port: 51820
    grpc_port: 11290
    endpoints:
      - ${ENDPOINT_01}
      - ${ENDPOINT_02}

  - name: ${MASTER_02}
    host: 127.0.0.1
    peer_host: ${MASTER_02_BRIDGE}
    overlay_ip: ${MASTER_02_OVERLAY}
    listen_port: 51820
    grpc_port: 21290
    endpoints:
      - ${ENDPOINT_01}
      - ${ENDPOINT_02}

endpoints:
  - name: ${ENDPOINT_01}
    host: 127.0.0.1
    peer_host: ${ENDPOINT_01_BRIDGE}
    overlay_ip: ${ENDPOINT_01_OVERLAY}
    listen_port: 51820
    grpc_port: 31290
    region: eu

  - name: ${ENDPOINT_02}
    host: 127.0.0.1
    peer_host: ${ENDPOINT_02_BRIDGE}
    overlay_ip: ${ENDPOINT_02_OVERLAY}
    listen_port: 51820
    grpc_port: 41290
    region: eu

transport:
  pool: 10.99.0.0/16
  prefix_length: 30
EOF

# ---------------------------------------------------------------------------
# Pre-prepare nodes to generate auth tokens before booting containers.
# ---------------------------------------------------------------------------
info "Pre-preparing nodes to generate auth tokens..."
for NODE_NAME in "${MASTER_01}" "${MASTER_02}"; do
    ${MESHCTL_BIN} \
        --topology "${TOPO_FILE}" \
        --config-dir "${CTL_CONFIG_DIR}" \
        master prepare "${NODE_NAME}" > /dev/null || {
        echo "ERROR: mesh-ctl master prepare ${NODE_NAME} failed" >&2
        exit 3
    }
done

for NODE_NAME in "${ENDPOINT_01}" "${ENDPOINT_02}"; do
    ${MESHCTL_BIN} \
        --topology "${TOPO_FILE}" \
        --config-dir "${CTL_CONFIG_DIR}" \
        endpoint prepare "${NODE_NAME}" > /dev/null || {
        echo "ERROR: mesh-ctl endpoint prepare ${NODE_NAME} failed" >&2
        exit 3
    }
done

TOKEN_MASTER_01=$(cat "${CTL_CONFIG_DIR}/nodes/${MASTER_01}/mesh.token")
TOKEN_MASTER_02=$(cat "${CTL_CONFIG_DIR}/nodes/${MASTER_02}/mesh.token")
TOKEN_ENDPOINT_01=$(cat "${CTL_CONFIG_DIR}/nodes/${ENDPOINT_01}/mesh.token")
TOKEN_ENDPOINT_02=$(cat "${CTL_CONFIG_DIR}/nodes/${ENDPOINT_02}/mesh.token")

# Escape $ -> $$ for docker-compose variable interpolation.
# bcrypt hashes contain literal $ characters ($2a$12$...).
TOKEN_MASTER_01_ESC="${TOKEN_MASTER_01//\$/\$\$}"
TOKEN_MASTER_02_ESC="${TOKEN_MASTER_02//\$/\$\$}"
TOKEN_ENDPOINT_01_ESC="${TOKEN_ENDPOINT_01//\$/\$\$}"
TOKEN_ENDPOINT_02_ESC="${TOKEN_ENDPOINT_02//\$/\$\$}"
info "Tokens resolved for all 4 nodes."

# ---------------------------------------------------------------------------
# Generate ephemeral compose file with pre-seeded MESH_TOKEN_HASH per node.
# ---------------------------------------------------------------------------
cat > "${COMPOSE_FILE}" <<EOF
# Auto-generated by issue-99-allowedips.sh — do not edit.
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
      issue99:
        ipv4_address: ${MASTER_01_BRIDGE}
    ports:
      - "11290:9090"
    entrypoint:
      - sh
      - -c
      - |
        [ -f /config/mesh.token ] || printf '%s' "\$\$MESH_TOKEN_HASH" > /config/mesh.token
        exec /usr/local/bin/awg-mesh-node \\
          --mode master \\
          --name ${MASTER_01} \\
          --overlay-ip ${MASTER_01_OVERLAY} \\
          --listen-port 51820

  ${MASTER_02}:
    image: ${NODE_IMAGE}
    container_name: ${CTR_MASTER_02}
    hostname: ${MASTER_02}
    restart: "no"
    privileged: true
    environment:
      MESH_TOKEN_HASH: "${TOKEN_MASTER_02_ESC}"
    networks:
      issue99:
        ipv4_address: ${MASTER_02_BRIDGE}
    ports:
      - "21290:9090"
    entrypoint:
      - sh
      - -c
      - |
        [ -f /config/mesh.token ] || printf '%s' "\$\$MESH_TOKEN_HASH" > /config/mesh.token
        exec /usr/local/bin/awg-mesh-node \\
          --mode master \\
          --name ${MASTER_02} \\
          --overlay-ip ${MASTER_02_OVERLAY} \\
          --listen-port 51820

  ${ENDPOINT_01}:
    image: ${NODE_IMAGE}
    container_name: ${CTR_ENDPOINT_01}
    hostname: ${ENDPOINT_01}
    restart: "no"
    privileged: true
    environment:
      MESH_TOKEN_HASH: "${TOKEN_ENDPOINT_01_ESC}"
    networks:
      issue99:
        ipv4_address: ${ENDPOINT_01_BRIDGE}
    ports:
      - "31290:9090"
    entrypoint:
      - sh
      - -c
      - |
        [ -f /config/mesh.token ] || printf '%s' "\$\$MESH_TOKEN_HASH" > /config/mesh.token
        exec /usr/local/bin/awg-mesh-node \\
          --mode endpoint \\
          --name ${ENDPOINT_01} \\
          --overlay-ip ${ENDPOINT_01_OVERLAY} \\
          --listen-port 51820

  ${ENDPOINT_02}:
    image: ${NODE_IMAGE}
    container_name: ${CTR_ENDPOINT_02}
    hostname: ${ENDPOINT_02}
    restart: "no"
    privileged: true
    environment:
      MESH_TOKEN_HASH: "${TOKEN_ENDPOINT_02_ESC}"
    networks:
      issue99:
        ipv4_address: ${ENDPOINT_02_BRIDGE}
    ports:
      - "41290:9090"
    entrypoint:
      - sh
      - -c
      - |
        [ -f /config/mesh.token ] || printf '%s' "\$\$MESH_TOKEN_HASH" > /config/mesh.token
        exec /usr/local/bin/awg-mesh-node \\
          --mode endpoint \\
          --name ${ENDPOINT_02} \\
          --overlay-ip ${ENDPOINT_02_OVERLAY} \\
          --listen-port 51820

networks:
  issue99:
    driver: bridge
    ipam:
      config:
        - subnet: 192.168.99.0/24
EOF

# ---------------------------------------------------------------------------
# A1: Boot the mesh
# ---------------------------------------------------------------------------
echo ""
echo "[A1] Booting 2-master + 2-endpoint mesh..."

compose_run up -d

info "Waiting for all nodes to report gRPC ready (up to ${GRPC_READY_TIMEOUT}s)..."
for CTR in "${CTR_MASTER_01}" "${CTR_MASTER_02}" "${CTR_ENDPOINT_01}" "${CTR_ENDPOINT_02}"; do
    if wait_for_log "${CTR}" "gRPC server listening" "${GRPC_READY_TIMEOUT}"; then
        pass "A1: ${CTR} gRPC ready"
    else
        fail "A1: ${CTR} did not become gRPC ready within ${GRPC_READY_TIMEOUT}s"
        docker logs "${CTR}" 2>&1 | tail -20 >&2 || true
    fi
done

if [[ "${FAILURES}" -gt 0 ]]; then
    echo ""
    echo "[abort] Mesh failed to start — skipping remaining checks."
    exit "${FAILURES}"
fi

sleep 3

# ---------------------------------------------------------------------------
# A2: Initialize masters then endpoints
# ---------------------------------------------------------------------------
echo ""
echo "[A2] Initialising masters and endpoints..."

for MASTER_NAME in "${MASTER_01}" "${MASTER_02}"; do
    INIT_OUT=$(${MESHCTL_BIN} \
        --topology "${TOPO_FILE}" \
        --config-dir "${CTL_CONFIG_DIR}" \
        master init "${MASTER_NAME}" 2>&1) && INIT_RC=0 || INIT_RC=$?
    if [[ "${INIT_RC}" -ne 0 ]]; then
        fail "A2: master init ${MASTER_NAME} failed (rc=${INIT_RC}): ${INIT_OUT}"
    else
        pass "A2: master init ${MASTER_NAME} succeeded"
    fi
done

for EP_NAME in "${ENDPOINT_01}" "${ENDPOINT_02}"; do
    INIT_OUT=$(${MESHCTL_BIN} \
        --topology "${TOPO_FILE}" \
        --config-dir "${CTL_CONFIG_DIR}" \
        endpoint init "${EP_NAME}" 2>&1) && INIT_RC=0 || INIT_RC=$?
    if [[ "${INIT_RC}" -ne 0 ]]; then
        fail "A2: endpoint init ${EP_NAME} failed (rc=${INIT_RC}): ${INIT_OUT}"
    else
        pass "A2: endpoint init ${EP_NAME} succeeded"
    fi
done

sleep 3

# ---------------------------------------------------------------------------
# A3: Reconcile
# ---------------------------------------------------------------------------
echo ""
echo "[A3] Running reconcile..."

RECONCILE_OUT=$(${MESHCTL_BIN} \
    --topology "${TOPO_FILE}" \
    --config-dir "${CTL_CONFIG_DIR}" \
    reconcile 2>&1) && RECONCILE_RC=0 || RECONCILE_RC=$?

if [[ "${RECONCILE_RC}" -ne 0 ]]; then
    fail "A3: reconcile failed (rc=${RECONCILE_RC}): ${RECONCILE_OUT}"
else
    pass "A3: reconcile succeeded"
fi

sleep 3

# ---------------------------------------------------------------------------
# A4: Endpoint transport.yml contains overlay range CIDRs in allowed_ips
# ---------------------------------------------------------------------------
echo ""
echo "[A4] Verifying endpoint transport.yml contains overlay CIDRs in allowed_ips..."

OVERLAY_RANGES=("172.22.99.0/27" "172.22.99.32/27" "172.22.99.128/25")

for EP_CTR in "${CTR_ENDPOINT_01}" "${CTR_ENDPOINT_02}"; do
    TRANSPORT_YAML=$(docker exec "${EP_CTR}" cat /config/transport.yml 2>/dev/null || echo "")
    if [[ -z "${TRANSPORT_YAML}" ]]; then
        fail "A4: ${EP_CTR}: /config/transport.yml not found or empty"
        continue
    fi

    # Extract only the allowed_ips block(s) from the YAML to prevent false-passes
    # where overlay CIDRs appear elsewhere in the file (comments, other fields).
    # The awk pattern captures lines from "    allowed_ips:" until the next
    # same-or-lesser-indented key or end of file.
    ALLOWED_IPS_BLOB=$(echo "${TRANSPORT_YAML}" | awk '/^    allowed_ips:/,/^  [a-z]|^[a-z]/')

    if [[ -z "${ALLOWED_IPS_BLOB}" ]]; then
        fail "A4: ${EP_CTR}: transport.yml has no allowed_ips block"
        echo "  --- transport.yml content ---"
        echo "${TRANSPORT_YAML}" | head -40 | sed 's/^/    /'
        continue
    fi

    for CIDR in "${OVERLAY_RANGES[@]}"; do
        if echo "${ALLOWED_IPS_BLOB}" | grep -qF "${CIDR}"; then
            pass "A4: ${EP_CTR}: transport.yml allowed_ips contains overlay CIDR ${CIDR}"
        else
            fail "A4: ${EP_CTR}: transport.yml allowed_ips MISSING overlay CIDR ${CIDR}"
            echo "  --- allowed_ips block ---"
            echo "${ALLOWED_IPS_BLOB}" | sed 's/^/    /'
        fi
    done
done

# ---------------------------------------------------------------------------
# A5: Endpoint transport.yml has overlay_ip populated
# ---------------------------------------------------------------------------
echo ""
echo "[A5] Verifying endpoint transport.yml has overlay_ip populated..."

EP_01_TRANSPORT=$(docker exec "${CTR_ENDPOINT_01}" cat /config/transport.yml 2>/dev/null || echo "")
EP_02_TRANSPORT=$(docker exec "${CTR_ENDPOINT_02}" cat /config/transport.yml 2>/dev/null || echo "")

for EP_CHECK in "${CTR_ENDPOINT_01}:${ENDPOINT_01_OVERLAY}:${EP_01_TRANSPORT}" \
                "${CTR_ENDPOINT_02}:${ENDPOINT_02_OVERLAY}:${EP_02_TRANSPORT}"; do
    EP_CTR="${EP_CHECK%%:*}"
    REST="${EP_CHECK#*:}"
    EXPECTED_IP="${REST%%:*}"
    TRANSPORT_CONTENT="${REST#*:}"

    if echo "${TRANSPORT_CONTENT}" | grep -q "overlay_ip: ${EXPECTED_IP}"; then
        pass "A5: ${EP_CTR}: transport.yml overlay_ip=${EXPECTED_IP} (populated)"
    else
        fail "A5: ${EP_CTR}: overlay_ip missing or malformed in transport.yml (expected ${EXPECTED_IP})"
        echo "${TRANSPORT_CONTENT}" | grep "overlay_ip" | sed 's/^/    /' >&2
    fi
done

# ---------------------------------------------------------------------------
# A6: Master ip route contains <endpoint-overlay>/32 for each endpoint
# ---------------------------------------------------------------------------
echo ""
echo "[A6] Verifying master routing table has per-peer overlay /32 routes..."

for MASTER_CTR in "${CTR_MASTER_01}" "${CTR_MASTER_02}"; do
    IP_ROUTE=$(docker exec "${MASTER_CTR}" ip route 2>/dev/null || echo "")
    for EP_OVERLAY in "${ENDPOINT_01_OVERLAY}" "${ENDPOINT_02_OVERLAY}"; do
        if echo "${IP_ROUTE}" | grep -q "${EP_OVERLAY}/32\|${EP_OVERLAY} "; then
            pass "A6: ${MASTER_CTR}: ip route contains ${EP_OVERLAY}/32"
        else
            fail "A6: ${MASTER_CTR}: ip route MISSING ${EP_OVERLAY}/32"
            info "  --- ip route output ---"
            echo "${IP_ROUTE}" | sed 's/^/    /'
        fi
    done
done

# ---------------------------------------------------------------------------
# A7: Overlay ping from master to each endpoint
# ---------------------------------------------------------------------------
echo ""
echo "[A7] Testing overlay pings from masters to endpoints..."

for MASTER_CTR in "${CTR_MASTER_01}" "${CTR_MASTER_02}"; do
    for EP_OVERLAY in "${ENDPOINT_01_OVERLAY}" "${ENDPOINT_02_OVERLAY}"; do
        if ping_overlay "${MASTER_CTR}" "${EP_OVERLAY}" "${PING_TIMEOUT}"; then
            pass "A7: ${MASTER_CTR} → ${EP_OVERLAY} overlay ping OK"
        else
            fail "A7: ${MASTER_CTR} → ${EP_OVERLAY} overlay ping FAILED (timeout ${PING_TIMEOUT}s)"
            # Diagnostics to aid debugging.
            info "  ${MASTER_CTR} ip route:"
            docker exec "${MASTER_CTR}" ip route 2>/dev/null | sed 's/^/    /' || true
            info "  wg show:"
            docker exec "${MASTER_CTR}" sh -c 'for i in $(ls /sys/class/net/ | grep ^wg-); do echo "=== $i ==="; wg show $i 2>/dev/null; done' | sed 's/^/    /' || true
        fi
    done
done

# ---------------------------------------------------------------------------
# A8: Overlay ping from each endpoint to bound master
# ---------------------------------------------------------------------------
echo ""
echo "[A8] Testing overlay pings from endpoints to masters..."

for EP_CTR in "${CTR_ENDPOINT_01}" "${CTR_ENDPOINT_02}"; do
    for MASTER_OVERLAY in "${MASTER_01_OVERLAY}" "${MASTER_02_OVERLAY}"; do
        if ping_overlay "${EP_CTR}" "${MASTER_OVERLAY}" "${PING_TIMEOUT}"; then
            pass "A8: ${EP_CTR} → ${MASTER_OVERLAY} overlay ping OK"
        else
            fail "A8: ${EP_CTR} → ${MASTER_OVERLAY} overlay ping FAILED (timeout ${PING_TIMEOUT}s)"
            info "  ${EP_CTR} ip route:"
            docker exec "${EP_CTR}" ip route 2>/dev/null | sed 's/^/    /' || true
            info "  wg show:"
            docker exec "${EP_CTR}" sh -c 'wg show 2>/dev/null || echo "(no wg interfaces)"' | sed 's/^/    /' || true
        fi
    done
done

# ---------------------------------------------------------------------------
# A9: mesh-ctl status --verify-data-plane returns 0 broken pairs
# ---------------------------------------------------------------------------
echo ""
echo "[A9] Checking mesh-ctl status --verify-data-plane..."

STATUS_OUT=$(${MESHCTL_BIN} \
    --topology "${TOPO_FILE}" \
    --config-dir "${CTL_CONFIG_DIR}" \
    status --verify-data-plane 2>&1) && STATUS_RC=0 || STATUS_RC=$?

if [[ "${STATUS_RC}" -eq 0 ]]; then
    pass "A9: status --verify-data-plane returned 0 (no broken pairs)"
else
    fail "A9: status --verify-data-plane returned non-zero (rc=${STATUS_RC})"
    echo "${STATUS_OUT}" | sed 's/^/    /'
fi

# ---------------------------------------------------------------------------
# Final summary
# ---------------------------------------------------------------------------
echo ""
echo "============================================================"
echo " Test Summary: ${PASSES} PASS / ${FAILURES} FAIL"
echo "============================================================"
echo ""

exit "${FAILURES}"
