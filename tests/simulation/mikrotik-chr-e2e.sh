#!/usr/bin/env bash
# mikrotik-chr-e2e.sh — full E2E sim against REAL RouterOS CHR (no proxy).
#
# Replaces alpine-based mikrotik-onboard.sh with honest emulation: real CHR
# in Docker QEMU, real v2 control-plane registration, and real RouterOS
# container import/start flow for awg-mesh-client.
#
# Architecture:
#   Docker network: chr-e2e-net-${SUFFIX}
#   ├── control-plane  (Docker Linux) — v2 registry/control-plane daemon
#   └── chr-mikrotik   (Docker QEMU)  — real RouterOS CHR
#       inside CHR userspace:
#         /interface/veth/add + /container/add for awg-mesh-client
#         awg-mesh-client starts with clientd args from generated .rsc
#
# Pre-conditions (run build-chr-baseline.sh first):
#   - awg-mesh-chr-baseline:${CHR_VERSION} Docker image exists
#   - awg-mesh-node:local image (Linux control-plane node)
#   - awg-mesh-client:local image (exported to CHR as a local image tar)
#   - mesh-ctl in PATH or at <repo>/bin/mesh-ctl
#   - sshpass + ssh + scp on PATH
#
# Exit codes: 0 / 1 (assertion fail) / 2 (env) / 3 (CHR boot timeout) /
#             4 (deploy gen failed) / 5 (import failed)
set -euo pipefail

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly REPO_ROOT
readonly CHR_VERSION="${CHR_VERSION:-7.21.4}"
readonly TARGET_ROS_VERSION="${TARGET_ROS_VERSION:-${CHR_VERSION}}"
readonly BASELINE_IMAGE="awg-mesh-chr-baseline:${CHR_VERSION}"
readonly BASELINE_READY_LABEL="awg-mesh.chr-container-enabled"
readonly NODE_IMAGE="${NODE_IMAGE:-awg-mesh-node:local}"
readonly CLIENT_IMAGE="${CLIENT_IMAGE:-awg-mesh-client:local}"
readonly MESHCTL_BIN="${MESHCTL_BIN:-${REPO_ROOT}/../bin/mesh-ctl}"
readonly RUNTIME_BASELINE_SCRIPT="${REPO_ROOT}/simulation/mikrotik-chr-baseline-runtime.sh"
readonly RUN_RUNTIME_BASELINE="${RUN_RUNTIME_BASELINE:-1}"
readonly CHR_ROUTEROS_NIC_MAC="${CHR_ROUTEROS_NIC_MAC:-3e:b1:b2:e4:28:54}"

SUFFIX="$(printf "%05d" "${RANDOM}")"
readonly SUFFIX
readonly NET_NAME="chr-e2e-net-${SUFFIX}"
NET_SUBNET=""
CP_BRIDGE_IP=""
CHR_HOST_BRIDGE_IP=""
CP_ROUTEROS_HOST=""
CP_ROUTEROS_ADDR=""
CLIENT_RUNTIME_SINCE_UNIX=0

readonly CP_GRPC_HOST_PORT="${CP_GRPC_HOST_PORT:-$((21000 + RANDOM % 1000))}"
readonly SSH_HOST_PORT="${SSH_HOST_PORT:-$((22000 + RANDOM % 1000))}"
readonly CHR_BOOT_ATTEMPTS="${CHR_BOOT_ATTEMPTS:-3}"
readonly CHR_PASS="lintpass"
readonly ROUTEROS_CONTAINER_NAME="AWG_MESH_MTK_HOME"
readonly -a SSH_OPTS=(
    -o StrictHostKeyChecking=no
    -o UserKnownHostsFile=/dev/null
    -o LogLevel=ERROR
    -o ConnectTimeout=5
)

# Mesh nodes
readonly MASTER_01="master-01"
readonly CLIENT_NAME="mtk-home"

readonly CTR_CP="chr-e2e-${SUFFIX}-control-plane"
readonly CTR_CHR="chr-e2e-${SUFFIX}-chr"

DOCKER_CHR_BIN="${DOCKER_CHR_BIN:-docker}"
if [[ "${DOCKER_CHR_BIN}" == "docker" && -n "${WSL_DISTRO_NAME:-}" && -x /Docker/host/bin/docker.exe ]]; then
    DOCKER_CHR_BIN="/Docker/host/bin/docker.exe"
fi

CTL_CONFIG_DIR="$(mktemp -d -t chr-e2e-ctl-XXXXXX)"
readonly CTL_CONFIG_DIR
readonly CLIENT_IMAGE_TAR="${CTL_CONFIG_DIR}/awg-mesh-client.tar"
TOPO_FILE="$(mktemp -t chr-e2e-topo-XXXXXX.yml)"
readonly TOPO_FILE

# ---------------------------------------------------------------------------
# Counters
# ---------------------------------------------------------------------------
PASSES=0
FAILURES=0

RED='\033[0;31m'; GREEN='\033[0;32m'; RESET='\033[0m'

pass() { echo -e "  [${GREEN}PASS${RESET}] $*"; (( PASSES++ )) || true; }
fail() { echo -e "  [${RED}FAIL${RESET}] $*" >&2; (( FAILURES++ )) || true; }
info() { echo "  [info] $*"; }
indent_stderr() {
    while IFS= read -r line; do
        printf '    %s\n' "${line}" >&2
    done
}

docker_chr() {
    "${DOCKER_CHR_BIN}" "$@"
}

docker_host_path() {
    local path="${1}"
    if [[ "${DOCKER_CHR_BIN}" == *.exe ]] && command -v wslpath > /dev/null 2>&1; then
        wslpath -w "${path}"
        return
    fi
    printf '%s\n' "${path}"
}

routeros_runtime_supported() {
    local version="${1}"
    local major minor
    major="$(printf '%s' "${version}" | cut -d. -f1)"
    minor="$(printf '%s' "${version}" | cut -d. -f2)"

    [[ "${major}" == "7" ]] || return 1
    [[ "${minor}" =~ ^[0-9]+$ ]] || return 1
    [[ "${minor}" -ge 21 ]] || return 1
    [[ "${minor}" -ne 22 ]] || return 1
    return 0
}

routeros_transitional_container_dialect() {
    local version="${1}"
    local major minor patch
    IFS=. read -r major minor patch _extra <<EOF
${version}
EOF
    patch="${patch:-0}"

    [[ "${major}" == "7" ]] || return 1
    [[ "${minor}" == "21" ]] || return 1
    [[ "${patch}" =~ ^[0-9]+$ ]] || return 1
    [[ "${patch}" -lt 4 ]]
}

create_docker_network() {
    local err_file="${CTL_CONFIG_DIR}/docker-network-create.err"
    local subnet

    if docker_chr network create "${NET_NAME}" > /dev/null 2>"${err_file}"; then
        subnet="$(docker_chr network inspect "${NET_NAME}" --format '{{range .IPAM.Config}}{{.Subnet}}{{end}}' 2>/dev/null || true)"
        NET_SUBNET="${subnet:-auto}"
        CP_BRIDGE_IP="outer-slirp-proxy"
        CHR_HOST_BRIDGE_IP="auto"
        CP_ROUTEROS_HOST="10.0.2.2"
        CP_ROUTEROS_ADDR="${CP_ROUTEROS_HOST}:${CP_GRPC_HOST_PORT}"
        return 0
    fi
    if [[ -s "${err_file}" ]]; then
        cat "${err_file}" >&2
    fi
    return 1
}

connect_secondary_network() {
    local network="${1}"
    local container="${2}"
    shift 2

    if ! docker_chr network connect --help | grep -q -- '--gw-priority'; then
        echo "ERROR: Docker lacks 'docker network connect --gw-priority'." >&2
        echo "       CHR requires eth0 to remain the default route while eth1 is bridged into QEMU." >&2
        return 1
    fi

    docker_chr network connect \
        --gw-priority -1 \
        --driver-opt com.docker.network.endpoint.ifname=eth1 \
        "$@" \
        "${network}" \
        "${container}"
}

start_chr_control_plane_proxy() {
    docker_chr exec -d \
        -e CP_PROXY_LISTEN_PORT="${CP_GRPC_HOST_PORT}" \
        -e CP_PROXY_TARGET_HOST="172.17.0.1" \
        -e CP_PROXY_TARGET_PORT="${CP_GRPC_HOST_PORT}" \
        "${CTR_CHR}" \
        python3 -c '
import os
import socket
import threading

listen = ("0.0.0.0", int(os.environ["CP_PROXY_LISTEN_PORT"]))
target = (os.environ["CP_PROXY_TARGET_HOST"], int(os.environ["CP_PROXY_TARGET_PORT"]))

def close_socket(sock):
    try:
        sock.shutdown(socket.SHUT_RDWR)
    except OSError:
        pass
    try:
        sock.close()
    except OSError:
        pass

def relay(src, dst):
    try:
        while True:
            data = src.recv(65536)
            if not data:
                break
            dst.sendall(data)
    except OSError:
        pass
    finally:
        try:
            dst.shutdown(socket.SHUT_WR)
        except OSError:
            pass

def handle(client):
    try:
        upstream = socket.create_connection(target, timeout=10)
        upstream.settimeout(None)
        client.settimeout(None)
    except OSError:
        close_socket(client)
        return
    threading.Thread(target=relay, args=(client, upstream), daemon=True).start()
    threading.Thread(target=relay, args=(upstream, client), daemon=True).start()

with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as server:
    server.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    server.bind(listen)
    server.listen(64)
    while True:
        client, _ = server.accept()
        threading.Thread(target=handle, args=(client,), daemon=True).start()
'

    for _attempt in $(seq 1 20); do
        if docker_chr exec "${CTR_CHR}" sh -lc "nc -z 127.0.0.1 '${CP_GRPC_HOST_PORT}'"; then
            return 0
        fi
        sleep 1
    done
    return 1
}

export_client_image_tar() {
    local build_log="${CTL_CONFIG_DIR}/docker-buildx-client.log"
    local oci_archive="${CTL_CONFIG_DIR}/awg-mesh-client.oci.tar"
    local dockerfile_path
    local context_path
    local output_archive
    dockerfile_path="$(docker_host_path "${REPO_ROOT}/../deploy/Dockerfile.client")"
    context_path="$(docker_host_path "${REPO_ROOT}/..")"
    output_archive="$(docker_host_path "${oci_archive}")"

    docker_chr buildx build \
        --platform linux/amd64 \
        --provenance=false \
        --tag "${CLIENT_IMAGE}" \
        --output "type=docker,dest=${output_archive}" \
        -f "${dockerfile_path}" \
        "${context_path}" > "${build_log}" 2>&1 || {
            tail -80 "${build_log}" >&2 || true
            return 1
        }
    convert_oci_archive_to_docker_archive "${oci_archive}" "${CLIENT_IMAGE_TAR}" "${CLIENT_IMAGE}"
}

convert_oci_archive_to_docker_archive() {
    local source_archive="${1}"
    local target_archive="${2}"
    local repo_tag="${3}"

    python3 - "${source_archive}" "${target_archive}" "${repo_tag}" <<'PY'
import gzip
import json
import os
import shutil
import sys
import tarfile
import tempfile

source_archive, target_archive, repo_tag = sys.argv[1:4]

with tempfile.TemporaryDirectory() as tmp:
    source_root = os.path.join(tmp, "source")
    output_root = os.path.join(tmp, "docker")
    os.makedirs(source_root)
    os.makedirs(output_root)

    with tarfile.open(source_archive) as archive:
        archive.extractall(source_root)

    with open(os.path.join(source_root, "manifest.json"), "r", encoding="utf-8") as fh:
        source_manifest = json.load(fh)[0]

    config_blob = source_manifest["Config"]
    config_name = os.path.basename(config_blob)
    shutil.copyfile(
        os.path.join(source_root, config_blob),
        os.path.join(output_root, f"{config_name}.json"),
    )

    docker_layers = []
    for layer_blob in source_manifest["Layers"]:
        layer_name = os.path.basename(layer_blob)
        layer_dir = os.path.join(output_root, layer_name)
        os.makedirs(layer_dir)
        with gzip.open(os.path.join(source_root, layer_blob), "rb") as src:
            with open(os.path.join(layer_dir, "layer.tar"), "wb") as dst:
                shutil.copyfileobj(src, dst)
        with open(os.path.join(layer_dir, "VERSION"), "w", encoding="utf-8") as fh:
            fh.write("1.0\n")
        with open(os.path.join(layer_dir, "json"), "w", encoding="utf-8") as fh:
            json.dump({"id": layer_name}, fh)
        docker_layers.append(f"{layer_name}/layer.tar")

    docker_manifest = [{
        "Config": f"{config_name}.json",
        "RepoTags": [repo_tag],
        "Layers": docker_layers,
    }]
    with open(os.path.join(output_root, "manifest.json"), "w", encoding="utf-8") as fh:
        json.dump(docker_manifest, fh)

    image_name, tag = repo_tag.rsplit(":", 1) if ":" in repo_tag else (repo_tag, "latest")
    with open(os.path.join(output_root, "repositories"), "w", encoding="utf-8") as fh:
        json.dump({image_name: {tag: os.path.dirname(docker_layers[-1])}}, fh)

    with tarfile.open(target_archive, "w") as archive:
        for entry in sorted(os.listdir(output_root)):
            archive.add(os.path.join(output_root, entry), arcname=entry)
PY
}

# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------
# shellcheck disable=SC2329 # cleanup is invoked by trap.
cleanup() {
    local rc=$?
    echo ""
    if [[ "${NO_CLEANUP:-0}" == "1" ]]; then
        echo "[cleanup] NO_CLEANUP=1 — leaving everything for inspection."
        echo "  CHR:        ${CTR_CHR}  (SSH: sshpass -p '${CHR_PASS}' ssh -p ${SSH_HOST_PORT} admin@127.0.0.1)"
        echo "  Network:    ${NET_NAME}"
        echo "  Mesh nodes: ${CTR_CP}"
        echo "  Topology:   ${TOPO_FILE}"
        echo "  Ctl config: ${CTL_CONFIG_DIR}"
        return
    fi
    echo "[cleanup] Tearing down..."
    docker_chr rm -f "${CTR_CHR}" "${CTR_CP}" > /dev/null 2>&1 || true
    docker_chr network rm "${NET_NAME}" > /dev/null 2>&1 || true
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

if ! routeros_runtime_supported "${CHR_VERSION}"; then
    echo "ERROR: CHR ${CHR_VERSION} is not a runtime/data-plane target for this E2E." >&2
    echo "       Use pkg/mikrotik generator tests for pre-7.21 syntax pivots; 7.22.x is blocked by the documented ip-rule regression." >&2
    exit 2
fi
if ! routeros_runtime_supported "${TARGET_ROS_VERSION}"; then
    echo "ERROR: target RouterOS ${TARGET_ROS_VERSION} is not a runtime/data-plane target for this E2E." >&2
    echo "       Runtime CHR validation is scoped to RouterOS 7.21+ except 7.22.x." >&2
    exit 2
fi

if ! command -v "${DOCKER_CHR_BIN}" > /dev/null 2>&1; then
    echo "ERROR: ${DOCKER_CHR_BIN} not found or not executable" >&2
    exit 2
fi

for cmd in python3 sshpass ssh scp; do
    if ! command -v "${cmd}" > /dev/null 2>&1; then
        echo "ERROR: ${cmd} not in PATH" >&2
        exit 2
    fi
done
if ! docker_chr buildx version > /dev/null 2>&1; then
    echo "ERROR: docker buildx is required to export a RouterOS-compatible client image archive" >&2
    exit 2
fi

if [[ ! -c /dev/kvm ]]; then
    echo "ERROR: /dev/kvm missing — CHR requires KVM" >&2
    exit 2
fi

if ! docker_chr image inspect "${BASELINE_IMAGE}" > /dev/null 2>&1; then
    echo "ERROR: ${BASELINE_IMAGE} not found." >&2
    echo "       Run: bash tests/simulation/lib/build-chr-baseline.sh CHR_VERSION=${CHR_VERSION}" >&2
    exit 2
fi
BASELINE_READY="$(docker_chr image inspect "${BASELINE_IMAGE}" --format '{{ index .Config.Labels "awg-mesh.chr-container-enabled" }}' 2>/dev/null || true)"
if [[ "${BASELINE_READY}" != "true" ]]; then
    echo "ERROR: ${BASELINE_IMAGE} is missing ${BASELINE_READY_LABEL}=true." >&2
    echo "       Rebuild it with: FORCE=1 CHR_VERSION=${CHR_VERSION} bash tests/simulation/lib/build-chr-baseline.sh" >&2
    exit 2
fi

if [[ "${RUN_RUNTIME_BASELINE}" == "1" ]]; then
    if [[ ! -x "${RUNTIME_BASELINE_SCRIPT}" ]]; then
        chmod +x "${RUNTIME_BASELINE_SCRIPT}" 2>/dev/null || true
    fi
    if [[ ! -x "${RUNTIME_BASELINE_SCRIPT}" ]]; then
        echo "ERROR: runtime baseline script is not executable: ${RUNTIME_BASELINE_SCRIPT}" >&2
        exit 2
    fi
    echo ""
    echo "[pre-flight] Running bare RouterOS runtime baseline before product deploy..."
    env CHR_VERSION="${CHR_VERSION}" \
        BASELINE_IMAGE="${BASELINE_IMAGE}" \
        bash "${RUNTIME_BASELINE_SCRIPT}"
else
    echo ""
    echo "[pre-flight] RUN_RUNTIME_BASELINE=0 - skipping bare RouterOS runtime baseline"
fi

if ! docker_chr image inspect "${NODE_IMAGE}" > /dev/null 2>&1; then
    echo "ERROR: ${NODE_IMAGE} not found. Build via: docker build -t ${NODE_IMAGE} -f deploy/Dockerfile.node ." >&2
    exit 2
fi

if ! docker_chr image inspect "${CLIENT_IMAGE}" > /dev/null 2>&1; then
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
info "Target ROS:      ${TARGET_ROS_VERSION}"
info "Node image:      ${NODE_IMAGE}"
info "Client image:    ${CLIENT_IMAGE}"
info "mesh-ctl:        ${MESHCTL_BIN_RESOLVED}"
info "CHR Docker CLI:  ${DOCKER_CHR_BIN}"
info "CP gRPC port:    ${CP_GRPC_HOST_PORT}"
info "SSH host port:   ${SSH_HOST_PORT}"
info "Suffix:          ${SUFFIX}"

# ---------------------------------------------------------------------------
# T1: Provision Docker network
# ---------------------------------------------------------------------------
echo ""
echo "[T1] Creating Docker network ${NET_NAME}..."
create_docker_network
pass "T1: network created (${NET_SUBNET})"
info "Control-plane bridge IP: ${CP_BRIDGE_IP}"
info "RouterOS control-plane:  ${CP_ROUTEROS_ADDR}"
info "CHR bridge IP:          ${CHR_HOST_BRIDGE_IP} (Docker IPAM)"

# docker-routeros enables QEMU user-mode hostfwd only when eth1 exists.
# Start CHR before the rest of the harness work; Docker Desktop + WSL can
# otherwise intermittently leave RouterOS stuck at the boot banner.
echo ""
echo "[T1.b] Booting CHR from baseline ${BASELINE_IMAGE}..."
CHR_BOOTED=0
for boot_attempt in $(seq 1 "${CHR_BOOT_ATTEMPTS}"); do
    docker_chr rm -f "${CTR_CHR}" > /dev/null 2>&1 || true
    info "CHR boot attempt ${boot_attempt}/${CHR_BOOT_ATTEMPTS}"
    docker_chr create \
        --name "${CTR_CHR}" \
        --network name=bridge,driver-opt=com.docker.network.endpoint.ifname=eth0 \
        -e ROUTEROS_NIC_MAC="${CHR_ROUTEROS_NIC_MAC}" \
        --device /dev/kvm \
        --device /dev/net/tun \
        --cap-add NET_ADMIN \
        -p "${SSH_HOST_PORT}:22" \
        "${BASELINE_IMAGE}" > /dev/null
    connect_secondary_network "${NET_NAME}" "${CTR_CHR}"
    docker_chr start "${CTR_CHR}" > /dev/null

    info "Waiting CHR SSH ready (up to 120s; baseline boots fast)..."
    SSH_READY=0
    for _attempt in $(seq 1 24); do
        if sshpass -p "${CHR_PASS}" ssh "${SSH_OPTS[@]}" -p "${SSH_HOST_PORT}" admin@127.0.0.1 ":put ok" > /dev/null 2>&1; then
            SSH_READY=1
            break
        fi
        sleep 5
    done
    if [[ "${SSH_READY}" -eq 1 ]]; then
        CHR_BOOTED=1
        break
    fi

    echo "  [warn] CHR SSH not ready on attempt ${boot_attempt}; QEMU log tail:" >&2
    docker_chr logs --tail 30 "${CTR_CHR}" >&2 || true
done
if [[ "${CHR_BOOTED}" -ne 1 ]]; then
    fail "T1.b: CHR SSH not ready after ${CHR_BOOT_ATTEMPTS} boot attempt(s)"
    exit 3
fi
if ! docker_chr exec "${CTR_CHR}" sh -lc "ip route | grep -q '^default .* dev eth0'"; then
    fail "T1.b: docker-routeros default route moved away from eth0"
    docker_chr exec "${CTR_CHR}" sh -lc "ip route" >&2 || true
    exit 3
fi
if ! start_chr_control_plane_proxy; then
    fail "T1.b: CHR slirp control-plane proxy did not start"
    docker_chr exec "${CTR_CHR}" sh -lc "ps -ef | grep CP_PROXY | grep -v grep || true" >&2 || true
    exit 3
fi
pass "T1.b: CHR booted + SSH ready"

# ---------------------------------------------------------------------------
# T2: Generate v2 topology + prepare node artifacts
#     This is a minimal v2 operator topology: one Linux control-plane
#     registration target and one RouterOS client container deployment.
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
    region: test
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
meshctl node prepare --platform mikrotik --target-ros "${TARGET_ROS_VERSION}" --control-plane "${CP_ROUTEROS_ADDR}" "${CLIENT_NAME}" > /dev/null
pass "T2.b: master + mikrotik node artifacts prepared"

CLIENT_CERT="${CTL_CONFIG_DIR}/nodes/${CLIENT_NAME}/node.crt"
CLIENT_KEY="${CTL_CONFIG_DIR}/nodes/${CLIENT_NAME}/node.key"
if [[ ! -s "${CLIENT_CERT}" || ! -s "${CLIENT_KEY}" ]]; then
    fail "T2.c: mikrotik client certificate material missing"
    exit 4
fi
pass "T2.c: mikrotik client certificate material exists"

# ---------------------------------------------------------------------------
# T3: Start v2 control-plane and register prepared nodes
# ---------------------------------------------------------------------------
echo ""
echo "[T3] Starting v2 control-plane and registering prepared nodes..."

docker_chr run -d \
    --name "${CTR_CP}" \
    --network "${NET_NAME}" \
    -p "${CP_GRPC_HOST_PORT}:9090" \
    --entrypoint /usr/local/bin/awg-mesh-node \
    "${NODE_IMAGE}" \
    --mode control-plane \
    --listen 0.0.0.0:9090 \
    --allow-insecure-public-bind \
    --state-dir /var/lib/awg-mesh > /dev/null

CP_READY=0
for _attempt in $(seq 1 30); do
    if docker_chr logs "${CTR_CP}" 2>&1 | grep -q "control-plane: listening"; then
        CP_READY=1
        break
    fi
    sleep 1
done
if [[ "${CP_READY}" -ne 1 ]]; then
    fail "T3.a: control-plane did not become ready"
    docker_chr logs --tail 30 "${CTR_CP}" >&2 || true
    exit 1
fi
pass "T3.a: control-plane listening"

meshctl node init "${MASTER_01}" --control-plane "127.0.0.1:${CP_GRPC_HOST_PORT}" > /dev/null
meshctl node init "${CLIENT_NAME}" --control-plane "127.0.0.1:${CP_GRPC_HOST_PORT}" > /dev/null
pass "T3.b: master + mikrotik client registered with control-plane"
CLIENT_RUNTIME_SINCE_UNIX="$(($(date +%s) + 1))"
sleep 1

# ---------------------------------------------------------------------------
# T4: Locate generated RouterOS container .rsc
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

if grep -q "/container/add interface=${ROUTEROS_CONTAINER_NAME}" "${RSC_FILE}" \
    && grep -Eq '(remote-image|image)=ghcr.io/coonfuuseed-paandaa/awg-mesh-client:[^"[:space:]]+' "${RSC_FILE}" \
    && grep -q "cmd=\"--mode client --control-plane ${CP_ROUTEROS_ADDR}" "${RSC_FILE}"; then
    pass "T4.a: generated .rsc defines awg-mesh-client container command"
else
    fail "T4.a: generated .rsc does not define expected container command"
    sed 's/^/    /' "${RSC_FILE}" >&2
    exit 4
fi
if grep -q "/interface/wireguard" "${RSC_FILE}"; then
    fail "T4.b: generated .rsc unexpectedly configures native RouterOS WireGuard"
    sed 's/^/    /' "${RSC_FILE}" >&2
    exit 4
fi
pass "T4.b: generated .rsc stays on RouterOS container path"

if routeros_transitional_container_dialect "${TARGET_ROS_VERSION}"; then
    if grep -q "/container/mounts/add name=${ROUTEROS_CONTAINER_NAME}_CONFIG" "${RSC_FILE}" \
        && grep -q "mounts=${ROUTEROS_CONTAINER_NAME}_CONFIG" "${RSC_FILE}" \
        && grep -q "/container/envs/add name=${ROUTEROS_CONTAINER_NAME}_ENVS" "${RSC_FILE}"; then
        pass "T4.c: generated .rsc uses transitional RouterOS runtime dialect"
    else
        fail "T4.c: generated .rsc does not use expected RouterOS runtime dialect for ${TARGET_ROS_VERSION}"
        indent_stderr < "${RSC_FILE}"
        exit 4
    fi
elif grep -q "/container/mounts/add list=${ROUTEROS_CONTAINER_NAME}_CONFIG" "${RSC_FILE}" \
    && grep -q "mountlists=${ROUTEROS_CONTAINER_NAME}_CONFIG" "${RSC_FILE}" \
    && grep -q "/container/envs/add list=${ROUTEROS_CONTAINER_NAME}_ENVS" "${RSC_FILE}"; then
    pass "T4.c: generated .rsc uses canonical RouterOS runtime dialect"
else
    fail "T4.c: generated .rsc does not use expected RouterOS runtime dialect for ${TARGET_ROS_VERSION}"
    indent_stderr < "${RSC_FILE}"
    exit 4
fi

# ---------------------------------------------------------------------------
# T5: Boot CHR from baseline + connect to mesh network
# ---------------------------------------------------------------------------
echo ""
echo "[T5] Verifying CHR runtime from baseline ${BASELINE_IMAGE}..."

if ! sshpass -p "${CHR_PASS}" ssh "${SSH_OPTS[@]}" -p "${SSH_HOST_PORT}" admin@127.0.0.1 ":put ok" > /dev/null 2>&1; then
    fail "T5: CHR SSH is no longer ready"
    exit 3
fi
pass "T5: CHR SSH still ready"

DEVICE_MODE_OUT=$(sshpass -p "${CHR_PASS}" ssh "${SSH_OPTS[@]}" -p "${SSH_HOST_PORT}" admin@127.0.0.1 \
    "/system/device-mode/print" 2>&1 || true)
if printf '%s' "${DEVICE_MODE_OUT}" | grep -q "container: yes"; then
    pass "T5.a: CHR device-mode has container=yes"
else
    fail "T5.a: CHR device-mode does not have container=yes"
    printf '%s\n' "${DEVICE_MODE_OUT}" | sed 's/^/    /' >&2
    exit 3
fi

PACKAGE_OUT=$(sshpass -p "${CHR_PASS}" ssh "${SSH_OPTS[@]}" -p "${SSH_HOST_PORT}" admin@127.0.0.1 \
    "/system/package/print detail where name=container" 2>&1 || true)
if printf '%s' "${PACKAGE_OUT}" | grep -Eq 'name="?container"?'; then
    pass "T5.b: CHR container package is installed"
else
    fail "T5.b: CHR container package is not installed"
    printf '%s\n' "${PACKAGE_OUT}" | sed 's/^/    /' >&2
    exit 3
fi

CP_FETCH_OUT=$(sshpass -p "${CHR_PASS}" ssh "${SSH_OPTS[@]}" -p "${SSH_HOST_PORT}" admin@127.0.0.1 \
    "/tool/fetch url=http://${CP_ROUTEROS_ADDR} keep-result=no" 2>&1) && CP_FETCH_RC=0 || CP_FETCH_RC=$?
if [[ "${CP_FETCH_RC}" -eq 0 ]] || echo "${CP_FETCH_OUT}" | grep -qiE "connection reset by peer|remote disconnected"; then
    pass "T5.c: CHR reaches slirp-proxied control-plane port"
else
    fail "T5.c: CHR cannot reach slirp-proxied control-plane port"
    indent_stderr <<< "${CP_FETCH_OUT}"
    exit 3
fi

# ---------------------------------------------------------------------------
# T6: SCP .rsc into CHR + /import
# ---------------------------------------------------------------------------
echo ""
echo "[T6] Importing deploy bundle into CHR..."

info "Building RouterOS-compatible client image archive at ${CLIENT_IMAGE_TAR}..."
export_client_image_tar || {
    fail "T6.a: client image archive export failed"; exit 5
}
RSC_IMPORT_FILE="${CTL_CONFIG_DIR}/routeros-local-image.rsc"
sed -E 's#(remote-image|image)=ghcr.io/coonfuuseed-paandaa/awg-mesh-client:[^"[:space:]]+#file=awg-mesh-client.tar#' \
    "${RSC_FILE}" > "${RSC_IMPORT_FILE}"
pass "T6.a: local client image tar + import script ready"

sshpass -p "${CHR_PASS}" scp "${SSH_OPTS[@]}" -P "${SSH_HOST_PORT}" "${CLIENT_IMAGE_TAR}" "admin@127.0.0.1:awg-mesh-client.tar" > /dev/null || {
    fail "T6.b: client image tar upload failed"; exit 5
}
pass "T6.b: local awg-mesh-client image tar uploaded to CHR"

sshpass -p "${CHR_PASS}" scp "${SSH_OPTS[@]}" -P "${SSH_HOST_PORT}" "${RSC_IMPORT_FILE}" "admin@127.0.0.1:deploy.rsc" > /dev/null || {
    fail "T6.c: .rsc upload failed"; exit 5
}
pass "T6.c: .rsc uploaded to CHR"

IMPORT_OUT=$(sshpass -p "${CHR_PASS}" ssh "${SSH_OPTS[@]}" -p "${SSH_HOST_PORT}" admin@127.0.0.1 \
    "/import file-name=deploy.rsc verbose=yes" 2>&1) && IMPORT_RC=0 || IMPORT_RC=$?

if [[ "${IMPORT_RC}" -ne 0 ]]; then
    fail "T6.d: /import exit ${IMPORT_RC} on CHR ${CHR_VERSION}"
    echo "${IMPORT_OUT}" | tail -20 | sed 's/^/    /' >&2
    exit 5
fi
if echo "${IMPORT_OUT}" | grep -qiE "syntax error|failure|invalid value|unknown parameter"; then
    fail "T6.d: /import contained failure indicator"
    echo "${IMPORT_OUT}" | grep -iE "syntax error|failure|invalid|unknown" | sed 's/^/    /' >&2
    exit 5
fi
pass "T6.d: /import exit 0, no failure indicators"

START_OUT=$(sshpass -p "${CHR_PASS}" ssh "${SSH_OPTS[@]}" -p "${SSH_HOST_PORT}" admin@127.0.0.1 \
    "/container/start [find where name=\"${ROUTEROS_CONTAINER_NAME}\"]" 2>&1) && START_RC=0 || START_RC=$?
if [[ "${START_RC}" -ne 0 ]]; then
    fail "T6.e: /container/start exit ${START_RC}"
    indent_stderr <<< "${START_OUT}"
    exit 5
fi
pass "T6.e: awg-mesh-client RouterOS container start requested"

CONTAINER_PRESENT=0
CONTAINER_RUNNING=0
CONTAINER_OUT=""
info "Waiting for RouterOS container ${ROUTEROS_CONTAINER_NAME} to run (up to 180s)..."
for _attempt in $(seq 1 36); do
    CONTAINER_OUT=$(sshpass -p "${CHR_PASS}" ssh "${SSH_OPTS[@]}" -p "${SSH_HOST_PORT}" admin@127.0.0.1 \
        "/container/print detail where name=\"${ROUTEROS_CONTAINER_NAME}\"" 2>&1 || true)
    NORMALIZED_CONTAINER_OUT="$(printf '%s' "${CONTAINER_OUT}" | tr -d '\r' | tr '\n' ' ' | tr -s '[:space:]' ' ')"
    if [[ "${NORMALIZED_CONTAINER_OUT}" == *"name=\"${ROUTEROS_CONTAINER_NAME}\""* || "${NORMALIZED_CONTAINER_OUT}" == *"name=${ROUTEROS_CONTAINER_NAME}"* ]]; then
        CONTAINER_PRESENT=1
    fi
    if [[ "${NORMALIZED_CONTAINER_OUT}" == *"status=running"* || "${NORMALIZED_CONTAINER_OUT}" =~ (^|[[:space:]])[0-9]+[[:space:]]+R[[:space:]] ]]; then
        CONTAINER_RUNNING=1
        break
    fi
    sleep 5
done

if [[ "${CONTAINER_PRESENT}" -eq 1 ]]; then
    pass "T6.f: RouterOS container object exists"
else
    fail "T6.f: RouterOS container object missing"
    indent_stderr <<< "${CONTAINER_OUT}"
    exit 5
fi

if [[ "${CONTAINER_RUNNING}" -eq 1 ]]; then
    pass "T6.g: awg-mesh-client RouterOS container is running"
else
    fail "T6.g: awg-mesh-client RouterOS container did not reach running state"
    indent_stderr <<< "${CONTAINER_OUT}"
    exit 5
fi

CLIENT_REGISTERED=0
CLIENT_AUDIT_OUT=""
info "Waiting for clientd runtime registration audit event (up to 60s)..."
for _attempt in $(seq 1 12); do
    CLIENT_AUDIT_OUT=$(meshctl audit-log query \
        --control-plane "127.0.0.1:${CP_GRPC_HOST_PORT}" \
        --since-unix "${CLIENT_RUNTIME_SINCE_UNIX}" \
        --event-type register \
        --node "${CLIENT_NAME}" \
        --output json 2>&1 || true)
    if printf '%s' "${CLIENT_AUDIT_OUT}" | grep -Fq '"event_type": "register"' \
        && printf '%s' "${CLIENT_AUDIT_OUT}" | grep -Fq "\"node_name\": \"${CLIENT_NAME}\"" \
        && printf '%s' "${CLIENT_AUDIT_OUT}" | grep -Fq "roles=[client] overlay=172.21.92.130 region=home"; then
        CLIENT_REGISTERED=1
        break
    fi
    sleep 5
done

if [[ "${CLIENT_REGISTERED}" -eq 1 ]]; then
    pass "T6.h: awg-mesh-client registered with control-plane from RouterOS container"
else
    fail "T6.h: no post-import clientd registration audit event from RouterOS container"
    while IFS= read -r line; do printf '    audit: %s\n' "${line}"; done <<< "${CLIENT_AUDIT_OUT}" >&2
    docker_chr logs --tail 50 "${CTR_CP}" | sed 's/^/    control-plane: /' >&2 || true
    while IFS= read -r line; do printf '    routeros-container: %s\n' "${line}"; done <<< "${CONTAINER_OUT}" >&2
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
