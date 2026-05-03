#!/usr/bin/env bash
# mikrotik-chr-e2e.sh — full E2E sim against REAL RouterOS CHR (no proxy).
#
# Replaces alpine-based mikrotik-onboard.sh with honest emulation: real CHR
# in Docker QEMU, real v2 control-plane registration, real master runtime
# startup, and real RouterOS native WireGuard /import flow.
#
# Architecture:
#   Docker network: awg-mesh-test-${SUFFIX}/24
#   ├── control-plane  (Docker Linux) — v2 registry/control-plane daemon
#   ├── master-01      (Docker Linux) — v2 native master runtime
#   └── chr-mikrotik   (Docker QEMU)  — real RouterOS CHR
#       inside CHR userspace:
#         /interface/wireguard/add name=awg-mesh (per generated .rsc)
#         peer endpoint → master-01 bridge IP on the local Docker network
#
# Pre-conditions (run build-chr-baseline.sh first):
#   - awg-mesh-chr-baseline:${CHR_VERSION} Docker image exists
#   - awg-mesh-node:local image (Linux mesh node)
#   - awg-mesh-client:local image (kept as release preflight parity with Docker builds)
#   - mesh-ctl in PATH or at <repo>/bin/mesh-ctl
#   - sshpass + ssh + scp on PATH
#
# Exit codes: 0 / 1 (assertion fail) / 2 (env) / 3 (CHR boot timeout) /
#             4 (deploy gen failed) / 5 (import failed)
set -euo pipefail

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------
readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly CHR_VERSION="${CHR_VERSION:-7.16.2}"
readonly BASELINE_IMAGE="awg-mesh-chr-baseline:${CHR_VERSION}"
readonly NODE_IMAGE="${NODE_IMAGE:-awg-mesh-node:local}"
readonly CLIENT_IMAGE="${CLIENT_IMAGE:-awg-mesh-client:local}"
readonly MESHCTL_BIN="${MESHCTL_BIN:-${REPO_ROOT}/../bin/mesh-ctl}"

readonly SUFFIX="$(printf "%05d" "${RANDOM}")"
readonly NET_NAME="awg-mesh-test-${SUFFIX}"
readonly NET_SUBNET="192.168.93.0/24"

# Docker bridge IPs must not collide with the overlay subnet under test.
readonly CP_BRIDGE_IP="192.168.93.5"
readonly CHR_HOST_BRIDGE_IP="192.168.93.250"
readonly MST_BRIDGE_IP="192.168.93.10"
readonly CP_GRPC_HOST_PORT="$((21000 + RANDOM % 1000))"
readonly SSH_HOST_PORT="$((22000 + RANDOM % 1000))"
readonly SSH_OPTS="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=5"
readonly CHR_PASS="lintpass"

# Mesh nodes
readonly MASTER_01="master-01"
readonly CLIENT_NAME="mtk-home"

readonly CTR_CP="chrmesh-${SUFFIX}-control-plane"
readonly CTR_MST_01="chrmesh-${SUFFIX}-${MASTER_01}"
readonly CTR_CHR="chrmesh-${SUFFIX}-chr"

readonly CTL_CONFIG_DIR="$(mktemp -d -t chr-e2e-ctl-XXXXXX)"
readonly TOPO_FILE="$(mktemp -t chr-e2e-topo-XXXXXX.yml)"

# ---------------------------------------------------------------------------
# Counters
# ---------------------------------------------------------------------------
PASSES=0
FAILURES=0

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RESET='\033[0m'

pass() { echo -e "  [${GREEN}PASS${RESET}] $*"; (( PASSES++ )) || true; }
fail() { echo -e "  [${RED}FAIL${RESET}] $*" >&2; (( FAILURES++ )) || true; }
info() { echo "  [info] $*"; }

# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------
cleanup() {
    local rc=$?
    echo ""
    if [[ "${NO_CLEANUP:-0}" == "1" ]]; then
        echo "[cleanup] NO_CLEANUP=1 — leaving everything for inspection."
        echo "  CHR:        ${CTR_CHR}  (SSH: sshpass -p '${CHR_PASS}' ssh -p ${SSH_HOST_PORT} admin@127.0.0.1)"
        echo "  Network:    ${NET_NAME}"
        echo "  Mesh nodes: ${CTR_CP} ${CTR_MST_01}"
        echo "  Topology:   ${TOPO_FILE}"
        echo "  Ctl config: ${CTL_CONFIG_DIR}"
        return
    fi
    echo "[cleanup] Tearing down..."
    docker rm -f "${CTR_CHR}" "${CTR_MST_01}" "${CTR_CP}" > /dev/null 2>&1 || true
    docker network rm "${NET_NAME}" > /dev/null 2>&1 || true
    rm -rf "${CTL_CONFIG_DIR}"
    rm -f "${TOPO_FILE}"
    if [[ "${rc}" -eq 0 && "${FAILURES}" -eq 0 ]]; then
        echo "[cleanup] Done. Test PASSED."
    else
        echo "[cleanup] Done. Test FAILED (${FAILURES} failure(s))."
    fi
}
trap cleanup EXIT
trap 'echo "[err-trap] line $LINENO: cmd exited $? — last cmd: ${BASH_COMMAND}" >&2' ERR

# ---------------------------------------------------------------------------
# Pre-flight
# ---------------------------------------------------------------------------
echo "=== mikrotik-chr-e2e.sh — CHR ${CHR_VERSION} E2E ==="
echo ""
echo "[pre-flight] Checking dependencies..."

for cmd in docker sshpass ssh scp; do
    if ! command -v "${cmd}" > /dev/null 2>&1; then
        echo "ERROR: ${cmd} not in PATH" >&2
        exit 2
    fi
done

if [[ ! -c /dev/kvm ]]; then
    echo "ERROR: /dev/kvm missing — CHR requires KVM" >&2
    exit 2
fi

if ! docker image inspect "${BASELINE_IMAGE}" > /dev/null 2>&1; then
    echo "ERROR: ${BASELINE_IMAGE} not found." >&2
    echo "       Run: bash tests/simulation/lib/build-chr-baseline.sh CHR_VERSION=${CHR_VERSION}" >&2
    exit 2
fi

if ! docker image inspect "${NODE_IMAGE}" > /dev/null 2>&1; then
    echo "ERROR: ${NODE_IMAGE} not found. Build via: docker build -t ${NODE_IMAGE} -f deploy/Dockerfile.node ." >&2
    exit 2
fi

if ! docker image inspect "${CLIENT_IMAGE}" > /dev/null 2>&1; then
    echo "ERROR: ${CLIENT_IMAGE} not found. Build via: docker build -t ${CLIENT_IMAGE} -f deploy/Dockerfile.client ." >&2
    exit 2
fi

if [[ ! -x "${MESHCTL_BIN}" ]]; then
    if command -v mesh-ctl > /dev/null 2>&1; then
        MESHCTL_BIN_RESOLVED="$(command -v mesh-ctl)"
    else
        echo "ERROR: mesh-ctl not at ${MESHCTL_BIN} and not in PATH" >&2
        exit 2
    fi
else
    MESHCTL_BIN_RESOLVED="${MESHCTL_BIN}"
fi

info "CHR baseline:    ${BASELINE_IMAGE}"
info "Node image:      ${NODE_IMAGE}"
info "Client image:    ${CLIENT_IMAGE}"
info "mesh-ctl:        ${MESHCTL_BIN_RESOLVED}"
info "CP gRPC port:    ${CP_GRPC_HOST_PORT}"
info "SSH host port:   ${SSH_HOST_PORT}"
info "Suffix:          ${SUFFIX}"

# ---------------------------------------------------------------------------
# T1: Provision Docker network
# ---------------------------------------------------------------------------
echo ""
echo "[T1] Creating Docker network ${NET_NAME} (${NET_SUBNET})..."
docker network create --subnet "${NET_SUBNET}" "${NET_NAME}" > /dev/null
pass "T1: network created"

# ---------------------------------------------------------------------------
# T2: Generate v2 topology + prepare node artifacts
#     This is a minimal v2 operator topology: one native-WG master and one
#     RouterOS client. Extra ingress/egress nodes are declared so generated
#     AllowedIPs prove route distribution without requiring their runtimes.
# ---------------------------------------------------------------------------
echo ""
echo "[T2] Writing topology + preparing node artifacts..."

cat > "${TOPO_FILE}" <<EOF
schema_version: 2

mesh:
  name: chr-e2e
  overlay_supernet: 172.21.92.0/24

nodes:
  - name: ${MASTER_01}
    roles: [master, balancer]
    overlay_ip: 172.21.92.2
    bridge_ip: ${MST_BRIDGE_IP}
    region: test
    client_protocol: vanilla-wg
  - name: ingress-de
    roles: [ingress]
    overlay_ip: 172.21.92.20
    region: de
  - name: egress-us
    roles: [egress]
    overlay_ip: 172.21.92.34
    region: us
  - name: ${CLIENT_NAME}
    roles: [client]
    platform: mikrotik
    overlay_ip: 172.21.92.130
    region: home
    preferred_master: ${MASTER_01}
EOF

meshctl() {
    "${MESHCTL_BIN_RESOLVED}" \
        --topology "${TOPO_FILE}" \
        --config-dir "${CTL_CONFIG_DIR}" \
        "$@"
}

meshctl topology validate > /dev/null
pass "T2.a: schema v2 topology validates"

meshctl node prepare "${MASTER_01}" > /dev/null
meshctl node prepare --platform mikrotik "${CLIENT_NAME}" > /dev/null
pass "T2.b: master + mikrotik node artifacts prepared"

MASTER_CLIENT_KEY="${CTL_CONFIG_DIR}/nodes/${MASTER_01}/client-wg-private.key"
if [[ ! -s "${MASTER_CLIENT_KEY}" ]]; then
    fail "T2.c: master client-facing private key missing at ${MASTER_CLIENT_KEY}"
    exit 4
fi
pass "T2.c: master client-facing WireGuard key exists"

# ---------------------------------------------------------------------------
# T3: Start v2 control-plane + master runtime and register prepared nodes
# ---------------------------------------------------------------------------
echo ""
echo "[T3] Starting v2 control-plane and master runtime..."

docker run -d \
    --name "${CTR_CP}" \
    --network "${NET_NAME}" \
    --ip "${CP_BRIDGE_IP}" \
    -p "${CP_GRPC_HOST_PORT}:9090" \
    --entrypoint /usr/local/bin/awg-mesh-node \
    "${NODE_IMAGE}" \
    --mode control-plane \
    --listen 0.0.0.0:9090 \
    --allow-insecure-public-bind \
    --state-dir /var/lib/awg-mesh > /dev/null

CP_READY=0
for i in $(seq 1 30); do
    if docker logs "${CTR_CP}" 2>&1 | grep -q "control-plane: listening"; then
        CP_READY=1
        break
    fi
    sleep 1
done
if [[ "${CP_READY}" -ne 1 ]]; then
    fail "T3.a: control-plane did not become ready"
    docker logs --tail 30 "${CTR_CP}" >&2 || true
    exit 1
fi
pass "T3.a: control-plane listening"

meshctl node init "${MASTER_01}" --control-plane "127.0.0.1:${CP_GRPC_HOST_PORT}" > /dev/null
meshctl node init "${CLIENT_NAME}" --control-plane "127.0.0.1:${CP_GRPC_HOST_PORT}" > /dev/null
pass "T3.b: master + mikrotik client registered with control-plane"

# Boot master
docker run -d \
    --name "${CTR_MST_01}" \
    --network "${NET_NAME}" \
    --ip "${MST_BRIDGE_IP}" \
    --privileged \
    -v "${CTL_CONFIG_DIR}/nodes/${MASTER_01}:/node-config:ro" \
    --entrypoint /usr/local/bin/awg-mesh-node \
    "${NODE_IMAGE}" \
    --mode master \
    --name "${MASTER_01}" \
    --overlay-ip 172.21.92.2 \
    --client-private-key-file /node-config/client-wg-private.key > /dev/null

MASTER_READY=0
for i in $(seq 1 15); do
    if ! docker inspect -f '{{.State.Running}}' "${CTR_MST_01}" 2>/dev/null | grep -q true; then
        break
    fi
    if docker logs "${CTR_MST_01}" 2>&1 | grep -q "mode=master"; then
        MASTER_READY=1
        break
    fi
    sleep 1
done
if [[ "${MASTER_READY}" -ne 1 ]]; then
    fail "T3.c: master runtime did not stay ready"
    docker logs --tail 30 "${CTR_MST_01}" >&2 || true
    exit 1
fi
pass "T3.c: master runtime started"

# ---------------------------------------------------------------------------
# T4: Locate generated RouterOS native WireGuard .rsc
# ---------------------------------------------------------------------------
echo ""
echo "[T4] Locating MikroTik .rsc generated by node prepare..."

RSC_FILE="${CTL_CONFIG_DIR}/nodes/${CLIENT_NAME}/routeros.rsc"
if [[ ! -s "${RSC_FILE}" ]]; then
    fail "T4: .rsc not found at ${RSC_FILE}"
    find "${CTL_CONFIG_DIR}/nodes" -type f >&2 || true
    exit 4
fi
RSC_LINES=$(wc -l < "${RSC_FILE}")
pass "T4: .rsc fixture ready: ${RSC_FILE} (${RSC_LINES} lines)"

# ---------------------------------------------------------------------------
# T5: Boot CHR from baseline + connect to mesh network
# ---------------------------------------------------------------------------
echo ""
echo "[T5] Booting CHR from baseline ${BASELINE_IMAGE}..."

docker run -d \
    --name "${CTR_CHR}" \
    --network "${NET_NAME}" \
    --ip "${CHR_HOST_BRIDGE_IP}" \
    --device /dev/kvm \
    --device /dev/net/tun \
    --cap-add NET_ADMIN \
    -p "${SSH_HOST_PORT}:22" \
    "${BASELINE_IMAGE}" > /dev/null

info "Waiting CHR SSH ready (up to 60s; baseline boots fast)..."
SSH_READY=0
for i in $(seq 1 12); do
    if sshpass -p "${CHR_PASS}" ssh ${SSH_OPTS} -p "${SSH_HOST_PORT}" admin@127.0.0.1 ":put ok" > /dev/null 2>&1; then
        SSH_READY=1
        break
    fi
    sleep 5
done
if [[ "${SSH_READY}" -ne 1 ]]; then
    fail "T5: CHR SSH not ready in 60s"
    docker logs --tail 30 "${CTR_CHR}" >&2 || true
    exit 3
fi
pass "T5: CHR booted + SSH ready"

# ---------------------------------------------------------------------------
# T6: SCP .rsc into CHR + /import
# ---------------------------------------------------------------------------
echo ""
echo "[T6] Importing deploy bundle into CHR..."

sshpass -p "${CHR_PASS}" scp ${SSH_OPTS} -P "${SSH_HOST_PORT}" "${RSC_FILE}" "admin@127.0.0.1:deploy.rsc" > /dev/null || {
    fail "T6.a: scp failed"; exit 5
}
pass "T6.a: .rsc uploaded to CHR"

IMPORT_OUT=$(sshpass -p "${CHR_PASS}" ssh ${SSH_OPTS} -p "${SSH_HOST_PORT}" admin@127.0.0.1 \
    "/import file-name=deploy.rsc verbose=yes" 2>&1) && IMPORT_RC=0 || IMPORT_RC=$?

if [[ "${IMPORT_RC}" -ne 0 ]]; then
    fail "T6.b: /import exit ${IMPORT_RC} on CHR ${CHR_VERSION}"
    echo "${IMPORT_OUT}" | tail -20 | sed 's/^/    /' >&2
    exit 5
fi
if echo "${IMPORT_OUT}" | grep -qiE "syntax error|failure|invalid value|unknown parameter"; then
    fail "T6.b: /import contained failure indicator"
    echo "${IMPORT_OUT}" | grep -iE "syntax error|failure|invalid|unknown" | sed 's/^/    /' >&2
    exit 5
fi
pass "T6.b: /import exit 0, no failure indicators"

WG_OUT=$(sshpass -p "${CHR_PASS}" ssh ${SSH_OPTS} -p "${SSH_HOST_PORT}" admin@127.0.0.1 \
    "/interface/wireguard/print detail where name=\"awg-mesh\"" 2>&1)
if echo "${WG_OUT}" | grep -q "awg-mesh"; then
    pass "T6.c: awg-mesh WireGuard interface present"
else
    fail "T6.c: awg-mesh WireGuard interface missing"
    echo "${WG_OUT}" | sed 's/^/    /' >&2
    exit 5
fi

PEER_OUT=$(sshpass -p "${CHR_PASS}" ssh ${SSH_OPTS} -p "${SSH_HOST_PORT}" admin@127.0.0.1 \
    "/interface/wireguard/peers/print detail" 2>&1)
if echo "${PEER_OUT}" | grep -q "endpoint-address=${MST_BRIDGE_IP}" \
    && echo "${PEER_OUT}" | grep -q "allowed-address=172.21.92.2/32"; then
    pass "T6.d: WireGuard peer targets master bridge endpoint and master overlay"
else
    fail "T6.d: WireGuard peer does not match expected endpoint/AllowedIPs"
    echo "${PEER_OUT}" | sed 's/^/    /' >&2
    exit 5
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo ""
echo "=================================================================="
if [[ "${FAILURES}" -eq 0 ]]; then
    echo -e " mikrotik-chr-e2e (${CHR_VERSION}): ${GREEN}PASS${RESET} (${PASSES} check(s))"
    EXIT_CODE=0
else
    echo -e " mikrotik-chr-e2e (${CHR_VERSION}): ${RED}FAIL${RESET} — ${FAILURES} failure(s), ${PASSES} pass(es)"
    EXIT_CODE="${FAILURES}"
fi
echo "=================================================================="
echo ""

exit "${EXIT_CODE}"
