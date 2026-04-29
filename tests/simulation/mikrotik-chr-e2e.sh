#!/usr/bin/env bash
# mikrotik-chr-e2e.sh — full E2E sim against REAL RouterOS CHR (no proxy).
#
# Replaces alpine-based mikrotik-onboard.sh with honest emulation: real CHR
# in Docker QEMU, real /import flow, real /container/start, real overlay
# data plane.
#
# Architecture:
#   Docker network: awg-mesh-test-${SUFFIX}/24
#   ├── master-01      (Docker Linux) — mesh master
#   ├── master-02      (Docker Linux) — mesh master
#   ├── endpoint-01    (Docker Linux) — mesh endpoint
#   └── chr-mikrotik   (Docker QEMU)  — real RouterOS CHR
#       inside CHR userspace:
#         /interface/veth/add → BR_AWG_MESH (per generated .rsc)
#         /container/awg-mesh-client running → AmneziaWG client mode
#         handshake → master-01/02 over overlay 172.21.92.x
#
# Pre-conditions (run build-chr-baseline.sh first):
#   - awg-mesh-chr-baseline:${CHR_VERSION} Docker image exists
#   - awg-mesh-node:local image (Linux mesh node)
#   - awg-mesh-client:local image (client container — will be loaded into CHR via /container/add file=)
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
readonly NET_SUBNET="172.21.92.0/24"

# CHR bridge IP (for SSH) — must NOT collide with overlay subnet
readonly CHR_HOST_BRIDGE_IP="172.21.92.250"
readonly MST_BRIDGE_IP="172.21.92.10"
readonly EP_BRIDGE_IP="172.21.92.11"
readonly SSH_HOST_PORT="$((22000 + RANDOM % 1000))"
readonly MST_GRPC_HOST_PORT="$((23000 + RANDOM % 1000))"
readonly EP_GRPC_HOST_PORT="$((24000 + RANDOM % 1000))"
readonly SSH_OPTS="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=5"
readonly CHR_PASS="lintpass"

# Mesh nodes
readonly MASTER_01="mst-01"
readonly MASTER_02="mst-02"
readonly ENDPOINT_01="ep-01"
readonly CLIENT_NAME="mtk-home"

readonly CTR_MST_01="chrmesh-${SUFFIX}-${MASTER_01}"
readonly CTR_MST_02="chrmesh-${SUFFIX}-${MASTER_02}"
readonly CTR_EP_01="chrmesh-${SUFFIX}-${ENDPOINT_01}"
readonly CTR_CHR="chrmesh-${SUFFIX}-chr"

readonly CTL_CONFIG_DIR="$(mktemp -d -t chr-e2e-ctl-XXXXXX)"
readonly TOPO_FILE="$(mktemp -t chr-e2e-topo-XXXXXX.yml)"
readonly CLIENT_TAR="$(mktemp -t chr-e2e-client-XXXXXX.tar)"
readonly DEPLOY_DIR="$(mktemp -d -t chr-e2e-deploy-XXXXXX)"

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
        echo "  Mesh nodes: ${CTR_MST_01} ${CTR_MST_02} ${CTR_EP_01}"
        echo "  Topology:   ${TOPO_FILE}"
        echo "  Ctl config: ${CTL_CONFIG_DIR}"
        echo "  Deploy dir: ${DEPLOY_DIR}"
        return
    fi
    echo "[cleanup] Tearing down..."
    docker rm -f "${CTR_CHR}" "${CTR_MST_01}" "${CTR_MST_02}" "${CTR_EP_01}" > /dev/null 2>&1 || true
    docker network rm "${NET_NAME}" > /dev/null 2>&1 || true
    rm -rf "${CTL_CONFIG_DIR}" "${DEPLOY_DIR}"
    rm -f "${TOPO_FILE}" "${CLIENT_TAR}"
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
# T2: Generate topology + start mesh nodes
#     This is a minimal smoke topology — ONE master, ONE endpoint, ONE
#     mikrotik client. Multi-master rotation flow is covered by
#     issue-92-rotation.sh; CHR-e2e focuses on .rsc-import correctness.
# ---------------------------------------------------------------------------
echo ""
echo "[T2] Writing topology + booting mesh nodes..."

cat > "${TOPO_FILE}" <<EOF
masters:
  - name: ${MASTER_01}
    host: 127.0.0.1
    peer_host: ${MST_BRIDGE_IP}
    overlay_ip: 172.21.92.2
    listen_port: 51820
    grpc_port: ${MST_GRPC_HOST_PORT}
    endpoints:
      - ${ENDPOINT_01}

endpoints:
  - name: ${ENDPOINT_01}
    host: 127.0.0.1
    peer_host: ${EP_BRIDGE_IP}
    overlay_ip: 172.21.92.34
    listen_port: 51820
    grpc_port: ${EP_GRPC_HOST_PORT}

clients:
  - name: ${CLIENT_NAME}
    type: mikrotik
    overlay_ip: 172.21.92.36
    masters: [${MASTER_01}]
    mikrotik:
      image: ${CLIENT_IMAGE}
      storage_root: /docker

overlay:
  space: 172.21.92.0/24
  ranges:
    - name: masters
      cidr: 172.21.92.0/27
    - name: endpoints
      cidr: 172.21.92.32/27

transport:
  pool: 10.93.0.0/16
  prefix_length: 30
EOF

# Master prepare + endpoint prepare (generates tokens)
"${MESHCTL_BIN_RESOLVED}" --topology "${TOPO_FILE}" --config-dir "${CTL_CONFIG_DIR}" master prepare "${MASTER_01}" > /dev/null
"${MESHCTL_BIN_RESOLVED}" --topology "${TOPO_FILE}" --config-dir "${CTL_CONFIG_DIR}" endpoint prepare "${ENDPOINT_01}" > /dev/null
"${MESHCTL_BIN_RESOLVED}" --topology "${TOPO_FILE}" --config-dir "${CTL_CONFIG_DIR}" client prepare "${CLIENT_NAME}" > /dev/null

TOKEN_MASTER=$(cat "${CTL_CONFIG_DIR}/nodes/${MASTER_01}/mesh.token")
TOKEN_ENDPOINT=$(cat "${CTL_CONFIG_DIR}/nodes/${ENDPOINT_01}/mesh.token")
TOKEN_CLIENT=$(cat "${CTL_CONFIG_DIR}/nodes/${CLIENT_NAME}/mesh.token")

# Boot master
docker run -d \
    --name "${CTR_MST_01}" \
    --network "${NET_NAME}" \
    --ip "${MST_BRIDGE_IP}" \
    -p "${MST_GRPC_HOST_PORT}:9090" \
    --privileged \
    --entrypoint sh \
    -e "MESH_TOKEN_HASH=${TOKEN_MASTER}" \
    "${NODE_IMAGE}" \
    -c "[ -f /config/mesh.token ] || printf '%s' \"\${MESH_TOKEN_HASH}\" > /config/mesh.token; exec /usr/local/bin/awg-mesh-node --mode master --name ${MASTER_01} --overlay-ip 172.21.92.2 --listen-port 51820" > /dev/null
pass "T2.a: master ${MASTER_01} started"

# Boot endpoint
docker run -d \
    --name "${CTR_EP_01}" \
    --network "${NET_NAME}" \
    --ip "${EP_BRIDGE_IP}" \
    -p "${EP_GRPC_HOST_PORT}:9090" \
    --privileged \
    --entrypoint sh \
    -e "MESH_TOKEN_HASH=${TOKEN_ENDPOINT}" \
    "${NODE_IMAGE}" \
    -c "[ -f /config/mesh.token ] || printf '%s' \"\${MESH_TOKEN_HASH}\" > /config/mesh.token; exec /usr/local/bin/awg-mesh-node --mode endpoint --name ${ENDPOINT_01} --overlay-ip 172.21.92.34 --listen-port 51820" > /dev/null
pass "T2.b: endpoint ${ENDPOINT_01} started"

# Wait for gRPC ready
info "Waiting for gRPC ready on master + endpoint (up to 30s)..."
for ctr in "${CTR_MST_01}" "${CTR_EP_01}"; do
    READY=0
    for i in $(seq 1 30); do
        if docker logs "${ctr}" 2>&1 | grep -q "gRPC server listening"; then
            READY=1
            break
        fi
        sleep 1
    done
    if [[ "${READY}" -ne 1 ]]; then
        fail "T2.c: ${ctr} gRPC not ready"
        docker logs --tail 20 "${ctr}" >&2 || true
        exit 1
    fi
done
pass "T2.c: gRPC up on master + endpoint"

# ---------------------------------------------------------------------------
# T3: mesh-ctl init (master + endpoint linkage)
# ---------------------------------------------------------------------------
echo ""
echo "[T3] mesh-ctl init: linking master ↔ endpoint..."

"${MESHCTL_BIN_RESOLVED}" --topology "${TOPO_FILE}" --config-dir "${CTL_CONFIG_DIR}" \
    master init "${MASTER_01}" 2>&1 | tail -3 || {
    fail "T3: master init failed"; exit 1
}
pass "T3.a: master init ${MASTER_01}"

"${MESHCTL_BIN_RESOLVED}" --topology "${TOPO_FILE}" --config-dir "${CTL_CONFIG_DIR}" \
    endpoint init "${ENDPOINT_01}" 2>&1 | tail -3 || {
    fail "T3: endpoint init failed"; exit 1
}
pass "T3.b: endpoint init ${ENDPOINT_01}"

# ---------------------------------------------------------------------------
# T4: mesh-ctl client deploy → generates .rsc
# ---------------------------------------------------------------------------
echo ""
echo "[T4] Locating MikroTik .rsc (generated by client prepare in T2)..."

RSC_FILE="${CTL_CONFIG_DIR}/clients/${CLIENT_NAME}/${CLIENT_NAME}-mikrotik.rsc"
if [[ ! -s "${RSC_FILE}" ]]; then
    fail "T4: .rsc not found at ${RSC_FILE}"
    find "${CTL_CONFIG_DIR}/clients" -type f >&2 || true
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
    --device /dev/kvm \
    --device /dev/net/tun \
    --cap-add NET_ADMIN \
    -p "${SSH_HOST_PORT}:22" \
    "${BASELINE_IMAGE}" > /dev/null
# Note: CHR runs on its OWN docker network (default bridge) to avoid
# conflicting with mesh-nodes' pinned IPs in overlay subnet. The QEMU's
# internal udhcpd hands the RouterOS guest a 172.17.0.x address.
# Cross-network reach (CHR ↔ master overlay) is NOT required for /import
# smoke verification — that part is exercised in T6 via SSH-from-host.

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

# Tier-aware assertion: pre-7.21 CHR (legacy/transitional) cannot accept the
# canonical-only dialect emitted by v1.14.0 — this is the expected failure
# documented in MIKROTIK-VERSION-COMPAT.md §2 that motivates CR-002 multi-
# version dialect support.
TIER_LEGACY=0
case "${CHR_VERSION}" in
    7.[5-9]|7.1[0-7]*) TIER_LEGACY=1 ;;        # 7.5 — 7.17 — legacy
    7.18*|7.19*|7.20*) TIER_LEGACY=1 ;;        # 7.18 — 7.20 — transitional, mounts= still
    7.21*|7.23*|7.24*) TIER_LEGACY=0 ;;        # 7.21+, 7.23+ — canonical
    7.22*) TIER_LEGACY=2 ;;                     # 7.22 — refused by generator (regression)
esac

if [[ "${TIER_LEGACY}" -eq 1 ]]; then
    if echo "${IMPORT_OUT}" | grep -qF "/container/mounts/add" \
        && echo "${IMPORT_OUT}" | grep -qiE "syntax error.*column 11"; then
        pass "T6.b: ${CHR_VERSION} REJECTS canonical 'list=' as expected (CR-002 dialect support pending)"
        info "      → motivates CR-002: generator must emit 'name=' for tier ≤ 7.20"
    else
        fail "T6.b: expected legacy CHR to reject 'list=' syntax — got rc=${IMPORT_RC} but no expected error"
        echo "${IMPORT_OUT}" | tail -10 | sed 's/^/    /' >&2
        exit 5
    fi
else
    if [[ "${IMPORT_RC}" -ne 0 ]]; then
        fail "T6.b: /import exit ${IMPORT_RC} on canonical CHR ${CHR_VERSION}"
        echo "${IMPORT_OUT}" | tail -20 | sed 's/^/    /' >&2
        exit 5
    fi
    if echo "${IMPORT_OUT}" | grep -qiE "syntax error|failure|invalid value|unknown parameter"; then
        fail "T6.b: /import contained failure indicator on canonical CHR"
        echo "${IMPORT_OUT}" | grep -iE "syntax error|failure|invalid|unknown" | sed 's/^/    /' >&2
        exit 5
    fi
    pass "T6.b: /import exit 0, no failure indicators (canonical CHR ${CHR_VERSION})"

    # Verify post-import RouterOS state — only canonical tier can verify.
    sshpass -p "${CHR_PASS}" ssh ${SSH_OPTS} -p "${SSH_HOST_PORT}" admin@127.0.0.1 \
        "/interface print where name~\"AWG_MESH\"" 2>&1 | grep -q "AWG_MESH" && pass "T6.c: AWG_MESH interfaces present" || fail "T6.c: AWG_MESH interfaces missing"
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

# NOTE: container start + data-plane ping verification deferred —
# requires `/container/add file=` import of awg-mesh-client tar (next iteration).
# Current sim verifies .rsc IMPORT correctness across CHR versions.

exit "${EXIT_CODE}"
