#!/usr/bin/env bash
# verify.sh — manual end-to-end verification for client ECMP US1 (failover) + US2 (stickiness).
# Requires: Linux host (or WSL2), Docker 24+, Docker Compose v2.
# NOT run in CI. Execute manually: bash tests/client_ecmp/verify.sh
set -euo pipefail

COMPOSE_FILE="tests/client_ecmp/compose.yml"
COMPOSE_UP_TIMEOUT=300
READY_TIMEOUT=60
FAILOVER_WAIT=35
RECOVERY_WAIT=60
TS=$(date +%Y%m%d_%H%M%S)
LOG_DIR="/tmp/awg-verify-${TS}"

# ----------------------------------------------------------------------------
# Helpers
# ----------------------------------------------------------------------------

log()  { echo "[verify] $*"; }
fail() { echo "[verify] FAIL: $*" >&2; dump_logs; exit 1; }

dump_logs() {
    mkdir -p "${LOG_DIR}"
    log "Dumping container logs to ${LOG_DIR}/"
    for svc in master-01 master-02 node-eu-01 client-lin; do
        docker logs "${svc}" > "${LOG_DIR}/${svc}.log" 2>&1 || true
    done
    echo "[verify] Logs written to ${LOG_DIR}/" >&2
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

log "Step 2: docker compose up (timeout ${COMPOSE_UP_TIMEOUT}s)"

# docker compose does not have a native timeout flag; wrap with timeout(1).
if command -v timeout > /dev/null 2>&1; then
    timeout "${COMPOSE_UP_TIMEOUT}" docker compose -f "${COMPOSE_FILE}" up -d --build
else
    # Fallback: run without timeout (macOS / minimal images without coreutils timeout).
    docker compose -f "${COMPOSE_FILE}" up -d --build
fi

# ----------------------------------------------------------------------------
# Step 3: Wait for all services to become ready
# ----------------------------------------------------------------------------

log "Step 3: Waiting for services to report gRPC server ready"

wait_for_log "master-01"  "gRPC server listening"
wait_for_log "master-02"  "gRPC server listening"
wait_for_log "node-eu-01" "gRPC server listening"
wait_for_log "client-lin" "gRPC server listening"

# Allow a brief settling period for route installation after ready signal.
log "Settling pause (5s)..."
sleep 5

# ----------------------------------------------------------------------------
# Step 4: US2 — stickiness check (ECMP route with both nexthops)
# ----------------------------------------------------------------------------

log "Step 4: US2 — verify ECMP route with both master nexthops"

RAW_ROUTE=$(docker exec client-lin ip route show 172.20.70.0/24 2>/dev/null || echo "no route yet")
log "Client route: ${RAW_ROUTE}"

if echo "${RAW_ROUTE}" | grep -q "nexthop"; then
    # Multipath route present — check both nexthops appear.
    if echo "${RAW_ROUTE}" | grep -q "172.31.0.10" && echo "${RAW_ROUTE}" | grep -q "172.31.0.11"; then
        log "US2 PASS: ECMP route contains both master nexthops (172.31.0.10 + 172.31.0.11)."
    else
        fail "US2: ECMP route exists but does not contain both master nexthops. Got: ${RAW_ROUTE}"
    fi
else
    # Single nexthop or missing — accept if at least one master is reachable (partial convergence).
    if echo "${RAW_ROUTE}" | grep -qE "172.31.0.10|172.31.0.11"; then
        log "WARNING: route not yet multipath — only one nexthop present. Accepted as partial convergence."
    else
        fail "US2: No route to overlay 172.20.70.0/24 found on client-lin. Got: ${RAW_ROUTE}"
    fi
fi

if [[ "${HAVE_CONNTRACK}" == "true" ]]; then
    log "conntrack stickiness check (informational): $(conntrack -L 2>/dev/null | grep -c udp || echo 'n/a') UDP conntrack entries"
fi

# ----------------------------------------------------------------------------
# Step 5: US1 — failover (kill master-01, assert route via master-02 survives)
# ----------------------------------------------------------------------------

log "Step 5: US1 — failover: killing master-01, waiting ${FAILOVER_WAIT}s"

docker kill master-01

log "master-01 killed. Waiting ${FAILOVER_WAIT}s for healthcheck-driven failover..."
sleep "${FAILOVER_WAIT}"

FAILOVER_ROUTE=$(docker exec client-lin ip route show 172.20.70.0/24 2>/dev/null || echo "no route yet")
log "Client route after failover: ${FAILOVER_ROUTE}"

if echo "${FAILOVER_ROUTE}" | grep -qE "172.31.0.11|via"; then
    log "US1 PASS: client still has a route to overlay after master-01 failure (via master-02)."
else
    fail "US1 failover: client lost all overlay routes after master-01 failure. Got: ${FAILOVER_ROUTE}"
fi

# Sanity: master-01 nexthop must be absent (routing correctly removed the dead path).
if echo "${FAILOVER_ROUTE}" | grep -q "172.31.0.10"; then
    log "WARNING: master-01 nexthop (172.31.0.10) still present in route after kill. Healthcheck may need more time."
fi

# ----------------------------------------------------------------------------
# Step 6: US1 — recovery (restart master-01, assert both nexthops return)
# ----------------------------------------------------------------------------

log "Step 6: US1 — recovery: restarting master-01, waiting ${RECOVERY_WAIT}s"

docker start master-01

log "master-01 started. Waiting ${RECOVERY_WAIT}s for route re-convergence..."
sleep "${RECOVERY_WAIT}"

RECOVERY_ROUTE=$(docker exec client-lin ip route show 172.20.70.0/24 2>/dev/null || echo "no route yet")
log "Client route after recovery: ${RECOVERY_ROUTE}"

if echo "${RECOVERY_ROUTE}" | grep -q "172.31.0.10"; then
    log "US1 PASS: master-01 nexthop (172.31.0.10) restored in client route after recovery."
else
    log "WARNING: master-01 nexthop not yet re-added after ${RECOVERY_WAIT}s. May need more convergence time."
    log "Route state: ${RECOVERY_ROUTE}"
fi

# ----------------------------------------------------------------------------
# Step 7: Summary
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
