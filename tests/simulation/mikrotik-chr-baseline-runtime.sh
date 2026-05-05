#!/usr/bin/env bash
# mikrotik-chr-baseline-runtime.sh - prove a clean RouterOS CHR /container
# runtime before awg-mesh-client is imported or started.
#
# This harness validates the RouterOS execution environment only:
#   - CHR baseline image has container package + device-mode enabled
#   - docker-routeros hostfwd/slirp reachability matches the CHR product path
#   - RouterOS veth/bridge/IP, NAT, and forward firewall rules work
#   - RouterOS container logging exposes probe output through /log
#   - a Linux container inside RouterOS can reach both a simulation node and
#     the exact control-plane address later passed to awg-mesh-client
#
# Exit codes: 0 / 1 assertion failure / 2 env failure / 3 CHR boot timeout /
#             4 RouterOS config failure / 5 probe image/container failure.
set -euo pipefail

readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly CHR_VERSION="${CHR_VERSION:-7.21.4}"
readonly BASELINE_IMAGE="${BASELINE_IMAGE:-awg-mesh-chr-baseline:${CHR_VERSION}}"
readonly BASELINE_READY_LABEL="awg-mesh.chr-container-enabled"
readonly CHR_ROUTEROS_NIC_MAC="${CHR_ROUTEROS_NIC_MAC:-3e:b1:b2:e4:28:54}"
readonly CHR_PASS="${CHR_PASS:-lintpass}"
readonly SSH_HOST_PORT="${SSH_HOST_PORT:-$((23000 + RANDOM % 1000))}"
readonly HTTP_PORT="${HTTP_PORT:-8080}"
readonly HTTP_HOST_PORT="${HTTP_HOST_PORT:-$((23500 + RANDOM % 1000))}"
readonly CP_GRPC_HOST_PORT="${CP_GRPC_HOST_PORT:-$((24000 + RANDOM % 1000))}"
readonly CP_GRPC_CONTAINER_PORT="${CP_GRPC_CONTAINER_PORT:-9090}"
readonly CHR_BOOT_ATTEMPTS="${CHR_BOOT_ATTEMPTS:-3}"
readonly PROBE_BASE_IMAGE="${PROBE_BASE_IMAGE:-alpine:3.21}"
readonly PROBE_IMAGE="${PROBE_IMAGE:-awg-mesh-routeros-probe:local-${CHR_VERSION}-${RANDOM}}"
readonly PROBE_CONTAINER_NAME="${PROBE_CONTAINER_NAME:-AWG_MESH_PROBE}"
readonly PROBE_BRIDGE="${PROBE_BRIDGE:-BR_AWG_MESH_BASELINE}"
readonly PROBE_VETH="${PROBE_VETH:-AWG_MESH_PROBE}"
readonly PROBE_GATEWAY="${PROBE_GATEWAY:-100.127.0.1}"
readonly PROBE_ADDRESS="${PROBE_ADDRESS:-100.127.0.2/24}"
readonly PROBE_SUBNET="${PROBE_SUBNET:-100.127.0.0/24}"
readonly PROBE_ENVLIST="${PROBE_ENVLIST:-AWG_MESH_BASELINE_ENVS}"
readonly PROBE_ROOT_DIR="${PROBE_ROOT_DIR:-/docker/awg-mesh-baseline-probe}"
readonly PROBE_MARKER="AWG_MESH_BASELINE_PROBE_DONE"
readonly NAT_COMMENT="awg-mesh baseline: container masquerade"
readonly FW_ESTABLISHED_COMMENT="awg-mesh baseline: established return traffic"
readonly FW_OUTBOUND_COMMENT="awg-mesh baseline: container outbound"
readonly CP_ROUTEROS_HOST="${CP_ROUTEROS_HOST:-10.0.2.2}"
readonly CP_ROUTEROS_ADDR="${CP_ROUTEROS_HOST}:${CP_GRPC_HOST_PORT}"
readonly CP_PROXY_TARGET_HOST="${CP_PROXY_TARGET_HOST:-172.17.0.1}"
readonly REQUIRE_SHARED_NETWORK="${REQUIRE_SHARED_NETWORK:-0}"
if [[ -n "${CHR_REACHABILITY_MODE:-}" ]]; then
    readonly CHR_REACHABILITY_MODE
elif [[ "${REQUIRE_SHARED_NETWORK}" == "1" ]]; then
    readonly CHR_REACHABILITY_MODE="direct"
else
    readonly CHR_REACHABILITY_MODE="slirp-proxy"
fi

readonly SUFFIX="$(printf "%05d" "${RANDOM}")"
readonly NET_NAME="chr-baseline-runtime-net-${SUFFIX}"
readonly CTR_CHR="chr-baseline-runtime-${SUFFIX}-chr"
readonly CTR_HTTP="chr-baseline-runtime-${SUFFIX}-http"
readonly CTR_CP="chr-baseline-runtime-${SUFFIX}-control-plane"
readonly WORK_DIR="$(mktemp -d -t chr-baseline-runtime-XXXXXX)"
readonly PROBE_IMAGE_OCI_TAR="${WORK_DIR}/routeros-probe.oci.tar"
readonly PROBE_IMAGE_TAR="${WORK_DIR}/routeros-probe.tar"

readonly -a SSH_OPTS=(
    -o StrictHostKeyChecking=no
    -o UserKnownHostsFile=/dev/null
    -o LogLevel=ERROR
    -o PreferredAuthentications=password
    -o PubkeyAuthentication=no
    -o ConnectTimeout=5
)

DOCKER_CHR_BIN="${DOCKER_CHR_BIN:-docker}"
if [[ "${DOCKER_CHR_BIN}" == "docker" && -n "${WSL_DISTRO_NAME:-}" && -x /Docker/host/bin/docker.exe ]]; then
    DOCKER_CHR_BIN="/Docker/host/bin/docker.exe"
fi

PASSES=0
FAILURES=0
HTTP_TARGET_IP=""
PROBE_HTTP_TARGET=""
PROBE_HTTP_PORT=""
PROBE_PING_TARGET=""
SHARED_NETWORK_MODE=""

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RESET='\033[0m'

pass() { echo -e "  [${GREEN}PASS${RESET}] $*"; (( PASSES++ )) || true; }
fail() { echo -e "  [${RED}FAIL${RESET}] $*" >&2; (( FAILURES++ )) || true; }
info() { echo "  [info] $*"; }
warn() { echo -e "  [${YELLOW}warn${RESET}] $*" >&2; }

docker_chr() {
    "${DOCKER_CHR_BIN}" "$@"
}

routeros_ssh() {
    sshpass -p "${CHR_PASS}" ssh "${SSH_OPTS[@]}" -p "${SSH_HOST_PORT}" admin@127.0.0.1 "$@"
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

cleanup() {
    local rc=$?
    echo ""
    if [[ "${NO_CLEANUP:-0}" == "1" ]]; then
        echo "[cleanup] NO_CLEANUP=1 - leaving runtime baseline artifacts."
        echo "  CHR:     ${CTR_CHR}  (SSH: sshpass -p '${CHR_PASS}' ssh -p ${SSH_HOST_PORT} admin@127.0.0.1)"
        echo "  HTTP:    ${CTR_HTTP}"
        echo "  CP:      ${CTR_CP}"
        echo "  Network: ${NET_NAME}"
        echo "  Workdir: ${WORK_DIR}"
        return
    fi

    echo "[cleanup] Tearing down runtime baseline..."
    docker_chr rm -f "${CTR_CHR}" "${CTR_HTTP}" "${CTR_CP}" > /dev/null 2>&1 || true
    docker_chr network rm "${NET_NAME}" > /dev/null 2>&1 || true
    docker image rm "${PROBE_IMAGE}" > /dev/null 2>&1 || true
    rm -rf "${WORK_DIR}"
    if [[ "${rc}" -eq 0 && "${FAILURES}" -eq 0 ]]; then
        echo "[cleanup] Done. Test PASSED."
    else
        echo "[cleanup] Done. Test FAILED (${FAILURES} failure(s))."
    fi
}
trap cleanup EXIT
trap 'echo "[err-trap] line $LINENO: cmd exited $? - last cmd: ${BASH_COMMAND}" >&2' ERR

connect_secondary_network() {
    local network="${1}"
    local container="${2}"
    shift 2

    if ! docker_chr network connect --help | grep -q -- '--gw-priority'; then
        echo "ERROR: Docker lacks 'docker network connect --gw-priority'." >&2
        echo "       CHR requires eth0 to remain hostfwd while eth1 is bridged into QEMU." >&2
        return 1
    fi

    docker_chr network connect \
        --gw-priority -1 \
        --driver-opt com.docker.network.endpoint.ifname=eth1 \
        "$@" \
        "${network}" \
        "${container}"
}

wait_chr_ssh_ready() {
    local label="${1}"
    local attempts="${2}"

    info "Waiting CHR SSH ready after ${label} (up to $((attempts * 5))s)..."
    for _attempt in $(seq 1 "${attempts}"); do
        if routeros_ssh ":put ok" > /dev/null 2>&1; then
            return 0
        fi
        sleep 5
    done
    return 1
}

build_probe_image() {
cat > "${WORK_DIR}/probe.sh" <<'EOF'
#!/bin/sh
set -u

failures=0

required() {
    label="$1"
    shift
    echo "probe: required ${label}"
    "$@"
    rc=$?
    if [ "${rc}" -eq 0 ]; then
        echo "probe: required ${label} ok"
        return 0
    fi
    echo "probe: required ${label} failed rc=${rc}"
    failures=$((failures + 1))
    return 0
}

optional() {
    label="$1"
    shift
    echo "probe: optional ${label}"
    "$@"
    rc=$?
    if [ "${rc}" -eq 0 ]; then
        echo "probe: optional ${label} ok"
        return 0
    fi
    echo "probe: optional ${label} failed rc=${rc}"
    return 0
}

echo "AWG_MESH_BASELINE_PROBE_BEGIN"
echo "probe: target=${HTTP_TARGET}:${HTTP_PORT} control=${CONTROL_PLANE_HOST}:${CONTROL_PLANE_PORT}"
echo "probe: ip addr"
ip addr
echo "probe: ip route"
ip route

optional "http target tcp" nc -z -w 5 "${HTTP_TARGET}" "${HTTP_PORT}"
required "http target request" sh -c "printf 'GET /healthz HTTP/1.0\r\nHost: probe\r\n\r\n' | nc -w 5 \"${HTTP_TARGET}\" \"${HTTP_PORT}\""
optional "ping target" ping -c 1 -W 2 "${PING_TARGET}"
optional "traceroute target" traceroute -m 3 -w 1 "${PING_TARGET}"
required "control-plane tcp" sh -c "printf '\n' | nc -w 5 \"${CONTROL_PLANE_HOST}\" \"${CONTROL_PLANE_PORT}\" >/dev/null"

if [ "${failures}" -eq 0 ]; then
    echo "AWG_MESH_BASELINE_PROBE_DONE"
else
    echo "AWG_MESH_BASELINE_PROBE_FAILED failures=${failures}"
    exit 1
fi
sleep "${PROBE_SLEEP_SECONDS:-120}"
EOF
    chmod +x "${WORK_DIR}/probe.sh"

    cat > "${WORK_DIR}/Dockerfile" <<EOF
FROM ${PROBE_BASE_IMAGE}
RUN apk add --no-cache iproute2 busybox-extras
COPY probe.sh /usr/local/bin/routeros-probe.sh
ENTRYPOINT ["/usr/local/bin/routeros-probe.sh"]
EOF

    docker build -t "${PROBE_IMAGE}" "${WORK_DIR}" > "${WORK_DIR}/probe-build.log" 2>&1 || {
        tail -80 "${WORK_DIR}/probe-build.log" >&2 || true
        return 1
    }
    docker buildx build \
        --platform linux/amd64 \
        --provenance=false \
        --tag "${PROBE_IMAGE}" \
        --output "type=docker,dest=${PROBE_IMAGE_OCI_TAR}" \
        "${WORK_DIR}" > "${WORK_DIR}/probe-buildx.log" 2>&1 || {
            tail -80 "${WORK_DIR}/probe-buildx.log" >&2 || true
            return 1
        }
    convert_oci_archive_to_docker_archive "${PROBE_IMAGE_OCI_TAR}" "${PROBE_IMAGE_TAR}" "${PROBE_IMAGE}"
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

start_simulation_targets() {
    docker_chr run -d \
        --name "${CTR_HTTP}" \
        --network "${NET_NAME}" \
        -p "${HTTP_HOST_PORT}:${HTTP_PORT}" \
        --entrypoint /bin/sh \
        "${PROBE_IMAGE}" \
        -lc "while true; do printf 'HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok' | nc -l -p ${HTTP_PORT}; done" > /dev/null

    HTTP_TARGET_IP="$(docker_chr inspect -f "{{ with index .NetworkSettings.Networks \"${NET_NAME}\" }}{{ .IPAddress }}{{ end }}" "${CTR_HTTP}")"
    if [[ -z "${HTTP_TARGET_IP}" ]]; then
        echo "ERROR: could not resolve HTTP target IP on ${NET_NAME}" >&2
        return 1
    fi

    docker_chr run -d \
        --name "${CTR_CP}" \
        --network "${NET_NAME}" \
        -p "${CP_GRPC_HOST_PORT}:${CP_GRPC_CONTAINER_PORT}" \
        --entrypoint /bin/sh \
        "${PROBE_IMAGE}" \
        -lc "while true; do nc -lk -p ${CP_GRPC_CONTAINER_PORT} >/dev/null 2>&1 || sleep 1; done" > /dev/null
}

start_chr_tcp_proxy() {
    local listen_port="${1}"
    local target_host="${2}"
    local target_port="${3}"

    docker_chr exec -d \
        -e TCP_PROXY_LISTEN_PORT="${listen_port}" \
        -e TCP_PROXY_TARGET_HOST="${target_host}" \
        -e TCP_PROXY_TARGET_PORT="${target_port}" \
        "${CTR_CHR}" \
        python3 -c '
import os
import socket
import threading

listen = ("0.0.0.0", int(os.environ["TCP_PROXY_LISTEN_PORT"]))
target = (os.environ["TCP_PROXY_TARGET_HOST"], int(os.environ["TCP_PROXY_TARGET_PORT"]))

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
        if docker_chr exec "${CTR_CHR}" sh -lc "nc -z 127.0.0.1 '${listen_port}'"; then
            return 0
        fi
        sleep 1
    done
    return 1
}

apply_routeros_baseline_config() {
    routeros_ssh "
/interface/bridge/add name=${PROBE_BRIDGE} comment=\"awg-mesh baseline container bridge\"
/ip/address/add address=${PROBE_GATEWAY}/24 interface=${PROBE_BRIDGE} comment=\"awg-mesh baseline container gateway\"
/interface/veth/add name=${PROBE_VETH} address=${PROBE_ADDRESS} gateway=${PROBE_GATEWAY}
/interface/bridge/port/add bridge=${PROBE_BRIDGE} interface=${PROBE_VETH}
/ip/firewall/nat/add chain=srcnat action=masquerade src-address=${PROBE_SUBNET} comment=\"${NAT_COMMENT}\"
/ip/firewall/filter/add chain=forward action=accept connection-state=established,related comment=\"${FW_ESTABLISHED_COMMENT}\"
/ip/firewall/filter/add chain=forward action=accept in-interface=${PROBE_BRIDGE} comment=\"${FW_OUTBOUND_COMMENT}\"
/container/envs/add list=${PROBE_ENVLIST} key=HTTP_TARGET value=${PROBE_HTTP_TARGET}
/container/envs/add list=${PROBE_ENVLIST} key=HTTP_PORT value=${PROBE_HTTP_PORT}
/container/envs/add list=${PROBE_ENVLIST} key=PING_TARGET value=${PROBE_PING_TARGET}
/container/envs/add list=${PROBE_ENVLIST} key=CONTROL_PLANE_HOST value=${CP_ROUTEROS_HOST}
/container/envs/add list=${PROBE_ENVLIST} key=CONTROL_PLANE_PORT value=${CP_GRPC_HOST_PORT}
/container/envs/add list=${PROBE_ENVLIST} key=PROBE_SLEEP_SECONDS value=120
:put baseline-config-ok
" > "${WORK_DIR}/routeros-config.log" 2>&1
}

routeros_counter() {
    local menu="${1}"
    local comment="${2}"
    local out

    out="$(routeros_ssh ":local id [/${menu}/find where comment=\"${comment}\"]; :if ([:len \$id] = 0) do={:put missing} else={:put [/${menu}/get \$id packets]}" 2>&1 || true)"
    printf '%s' "${out}" | tr -d '\r' | awk '/^[0-9]+$/ { value=$1 } END { if (value == "") { print "0" } else { print value } }'
}

require_counter_increased() {
    local label="${1}"
    local menu="${2}"
    local comment="${3}"
    local value

    value="$(routeros_counter "${menu}" "${comment}")"
    if [[ "${value}" =~ ^[0-9]+$ && "${value}" -gt 0 ]]; then
        pass "${label}: ${comment} packets=${value}"
        return 0
    fi
    fail "${label}: ${comment} counter did not increase"
    routeros_ssh "/${menu}/print stats detail where comment=\"${comment}\"" 2>&1 | sed 's/^/    /' >&2 || true
    return 1
}

wait_for_container_running() {
    local out normalized

    for _attempt in $(seq 1 36); do
        out="$(routeros_ssh "/container/print detail where name=\"${PROBE_CONTAINER_NAME}\"" 2>&1 || true)"
        normalized="$(printf '%s' "${out}" | tr -d '\r' | tr '\n' ' ' | tr -s '[:space:]' ' ')"
        if [[ "${normalized}" == *"status=running"* || "${normalized}" =~ (^|[[:space:]])[0-9]+[[:space:]]+R[[:space:]] ]]; then
            return 0
        fi
        sleep 5
    done
    printf '%s\n' "${out:-}" >&2
    return 1
}

wait_for_probe_marker() {
    local log_out

    for _attempt in $(seq 1 36); do
        log_out="$(routeros_ssh "/log/print where topics~\"container\"" 2>&1 || true)"
        if printf '%s' "${log_out}" | grep -q "${PROBE_MARKER}"; then
            return 0
        fi
        sleep 5
    done
    printf '%s\n' "${log_out:-}" | sed 's/^/    /' >&2
    return 1
}

echo "=== mikrotik-chr-baseline-runtime.sh - CHR ${CHR_VERSION} ==="
echo ""
echo "[pre-flight] Checking runtime baseline dependencies..."

if ! routeros_runtime_supported "${CHR_VERSION}"; then
    echo "ERROR: CHR ${CHR_VERSION} is not a runtime/data-plane target." >&2
    echo "       Runtime RouterOS validation is scoped to 7.21+ except 7.22.x." >&2
    exit 2
fi

for cmd in docker sshpass ssh scp python3; do
    if ! command -v "${cmd}" > /dev/null 2>&1; then
        echo "ERROR: ${cmd} not in PATH" >&2
        exit 2
    fi
done
if ! docker buildx version > /dev/null 2>&1; then
    echo "ERROR: docker buildx is required to export a RouterOS-compatible probe image archive" >&2
    exit 2
fi

if [[ ! -c /dev/kvm ]]; then
    echo "ERROR: /dev/kvm missing - CHR requires KVM for this runtime gate" >&2
    exit 2
fi

if ! docker image inspect "${BASELINE_IMAGE}" > /dev/null 2>&1; then
    echo "ERROR: ${BASELINE_IMAGE} not found." >&2
    echo "       Run: CHR_VERSION=${CHR_VERSION} bash tests/simulation/lib/build-chr-baseline.sh" >&2
    exit 2
fi
BASELINE_READY="$(docker image inspect "${BASELINE_IMAGE}" --format '{{ index .Config.Labels "awg-mesh.chr-container-enabled" }}' 2>/dev/null || true)"
if [[ "${BASELINE_READY}" != "true" ]]; then
    echo "ERROR: ${BASELINE_IMAGE} is missing ${BASELINE_READY_LABEL}=true." >&2
    echo "       Rebuild it with: FORCE=1 CHR_VERSION=${CHR_VERSION} bash tests/simulation/lib/build-chr-baseline.sh" >&2
    exit 2
fi
pass "pre-flight: ${BASELINE_IMAGE} has ${BASELINE_READY_LABEL}=true"

info "CHR Docker CLI:  ${DOCKER_CHR_BIN}"
info "Probe image:     ${PROBE_IMAGE}"
info "RouterOS CP:     ${CP_ROUTEROS_ADDR}"
info "Reachability:    ${CHR_REACHABILITY_MODE}"
info "SSH host port:   ${SSH_HOST_PORT}"
info "Suffix:          ${SUFFIX}"

case "${CHR_REACHABILITY_MODE}" in
    direct|slirp-proxy|auto) ;;
    *)
        echo "ERROR: CHR_REACHABILITY_MODE must be one of: direct, slirp-proxy, auto" >&2
        exit 2
        ;;
esac

echo ""
echo "[T1] Building bare Linux probe image..."
build_probe_image || {
    fail "T1: probe image build/export failed"
    exit 5
}
pass "T1: probe image built and exported"

echo ""
echo "[T2] Creating simulation network and target containers..."
docker_chr network create "${NET_NAME}" > /dev/null
start_simulation_targets || {
    fail "T2: simulation targets failed to start"
    exit 2
}
pass "T2.a: HTTP/ICMP target running at ${HTTP_TARGET_IP}:${HTTP_PORT} and host port ${HTTP_HOST_PORT}"
pass "T2.b: control-plane TCP probe published as ${CP_ROUTEROS_ADDR}"

echo ""
echo "[T3] Booting CHR from ${BASELINE_IMAGE}..."
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

    if wait_chr_ssh_ready "baseline boot" 36; then
        CHR_BOOTED=1
        break
    fi

    warn "CHR SSH not ready on attempt ${boot_attempt}; QEMU log tail:"
    docker_chr logs --tail 40 "${CTR_CHR}" >&2 || true
done
if [[ "${CHR_BOOTED}" -ne 1 ]]; then
    fail "T3: CHR SSH not ready after ${CHR_BOOT_ATTEMPTS} boot attempt(s)"
    exit 3
fi
pass "T3.a: CHR booted and SSH is ready"

if ! docker_chr exec "${CTR_CHR}" sh -lc "ip route | grep -q '^default .* dev eth0'"; then
    fail "T3.b: docker-routeros default route moved away from eth0"
    docker_chr exec "${CTR_CHR}" sh -lc "ip route" >&2 || true
    exit 3
fi
pass "T3.b: docker-routeros eth0 remains hostfwd default route"

if ! start_chr_tcp_proxy "${CP_GRPC_HOST_PORT}" "${CP_PROXY_TARGET_HOST}" "${CP_GRPC_HOST_PORT}"; then
    fail "T3.c: CHR slirp control-plane proxy did not start"
    exit 3
fi
pass "T3.c: CHR slirp control-plane proxy listens on ${CP_ROUTEROS_ADDR}"

if ! start_chr_tcp_proxy "${HTTP_HOST_PORT}" "${CP_PROXY_TARGET_HOST}" "${HTTP_HOST_PORT}"; then
    fail "T3.d: CHR slirp HTTP target proxy did not start"
    exit 3
fi
pass "T3.d: CHR slirp HTTP target proxy listens on ${CP_ROUTEROS_HOST}:${HTTP_HOST_PORT}"

echo ""
echo "[T4] Verifying RouterOS package/device-mode and selected reachability mode..."
DEVICE_MODE_OUT="$(routeros_ssh "/system/device-mode/print" 2>&1 || true)"
if printf '%s' "${DEVICE_MODE_OUT}" | grep -q "container: yes"; then
    pass "T4.a: /system/device-mode has container=yes"
else
    fail "T4.a: /system/device-mode lacks container=yes"
    printf '%s\n' "${DEVICE_MODE_OUT}" | sed 's/^/    /' >&2
    exit 3
fi

PACKAGE_OUT="$(routeros_ssh "/system/package/print detail where name=container" 2>&1 || true)"
if printf '%s' "${PACKAGE_OUT}" | grep -Eq 'name="?container"?'; then
    pass "T4.b: container package is installed"
else
    fail "T4.b: container package is not installed"
    printf '%s\n' "${PACKAGE_OUT}" | sed 's/^/    /' >&2
    exit 3
fi

routeros_ssh "/ip/dhcp-client/add interface=ether2 add-default-route=no use-peer-dns=no disabled=no comment=\"awg-mesh baseline shared net\"; :delay 5s; /interface/print detail; /ip/address/print detail; /ip/dhcp-client/print detail; /ip/route/print detail" > "${WORK_DIR}/routeros-network-inventory.log" 2>&1 || true

SHARED_NETWORK_MODE=""
if [[ "${CHR_REACHABILITY_MODE}" == "direct" || "${CHR_REACHABILITY_MODE}" == "auto" ]]; then
    SHARED_PING_OK=0
    if routeros_ssh "/ping address=${HTTP_TARGET_IP} count=1" 2>&1 | tee "${WORK_DIR}/routeros-ping-http.log" | grep -qi "received=1"; then
        SHARED_PING_OK=1
    fi

    SHARED_FETCH_OUT="$(routeros_ssh "/tool/fetch url=http://${HTTP_TARGET_IP}:${HTTP_PORT}/healthz keep-result=no" 2>&1 || true)"
    if [[ "${SHARED_PING_OK}" -eq 1 ]] && printf '%s' "${SHARED_FETCH_OUT}" | grep -qiE "status: finished|downloaded"; then
        SHARED_NETWORK_MODE="direct"
        PROBE_HTTP_TARGET="${HTTP_TARGET_IP}"
        PROBE_HTTP_PORT="${HTTP_PORT}"
        PROBE_PING_TARGET="${HTTP_TARGET_IP}"
        pass "T4.c: RouterOS guest reaches simulation target on docker-routeros shared network"
    elif [[ "${CHR_REACHABILITY_MODE}" == "direct" ]]; then
        fail "T4.c: RouterOS guest cannot reach simulation target ${HTTP_TARGET_IP} on shared network"
        sed 's/^/    /' "${WORK_DIR}/routeros-ping-http.log" >&2
        printf '%s\n' "${SHARED_FETCH_OUT}" | sed 's/^/    /' >&2
        exit 3
    fi
fi

if [[ -z "${SHARED_NETWORK_MODE}" ]]; then
    SHARED_NETWORK_MODE="slirp-proxy"
    PROBE_HTTP_TARGET="${CP_ROUTEROS_HOST}"
    PROBE_HTTP_PORT="${HTTP_HOST_PORT}"
    PROBE_PING_TARGET="${CP_ROUTEROS_HOST}"
    pass "T4.c: RouterOS guest uses slirp-proxied simulation target"
fi

FETCH_OUT="$(routeros_ssh "/tool/fetch url=http://${PROBE_HTTP_TARGET}:${PROBE_HTTP_PORT}/healthz keep-result=no" 2>&1 || true)"
if printf '%s' "${FETCH_OUT}" | grep -qiE "status: finished|downloaded"; then
    pass "T4.d: RouterOS /tool/fetch reaches HTTP target via ${SHARED_NETWORK_MODE}"
else
    fail "T4.d: RouterOS /tool/fetch cannot reach HTTP target via ${SHARED_NETWORK_MODE}"
    printf '%s\n' "${FETCH_OUT}" | sed 's/^/    /' >&2
    exit 3
fi

CP_FETCH_OUT="$(routeros_ssh "/tool/fetch url=http://${CP_ROUTEROS_ADDR} keep-result=no" 2>&1 || true)"
if printf '%s' "${CP_FETCH_OUT}" | grep -qiE "status: finished|downloaded|connection reset by peer|remote disconnected|closed"; then
    pass "T4.e: RouterOS guest reaches exact future control-plane address"
else
    fail "T4.e: RouterOS guest cannot reach ${CP_ROUTEROS_ADDR}"
    printf '%s\n' "${CP_FETCH_OUT}" | sed 's/^/    /' >&2
    exit 3
fi

echo ""
echo "[T5] Applying RouterOS container LAN, NAT, and firewall config..."
apply_routeros_baseline_config || {
    fail "T5: RouterOS baseline config import failed"
    sed 's/^/    /' "${WORK_DIR}/routeros-config.log" >&2 || true
    exit 4
}
pass "T5.a: veth/bridge/IP/NAT/firewall/env config applied"

routeros_ssh "/interface/bridge/port/print detail where bridge=${PROBE_BRIDGE}; /ip/address/print detail where interface=${PROBE_BRIDGE}; /ip/firewall/nat/print detail where comment=\"${NAT_COMMENT}\"; /ip/firewall/filter/print detail where comment~\"awg-mesh baseline\"" > "${WORK_DIR}/routeros-baseline-config-inventory.log" 2>&1
pass "T5.b: RouterOS baseline config inventory is queryable"

echo ""
echo "[T6] Importing and starting bare probe container..."
sshpass -p "${CHR_PASS}" scp "${SSH_OPTS[@]}" -P "${SSH_HOST_PORT}" "${PROBE_IMAGE_TAR}" "admin@127.0.0.1:routeros-probe.tar" > /dev/null || {
    fail "T6.a: probe image upload failed"
    exit 5
}
pass "T6.a: probe image uploaded"

ADD_OUT="$(routeros_ssh "/container/add file=routeros-probe.tar interface=${PROBE_VETH} root-dir=${PROBE_ROOT_DIR} envlist=${PROBE_ENVLIST} hostname=awg-mesh-probe name=${PROBE_CONTAINER_NAME} logging=yes start-on-boot=no" 2>&1 || true)"
if printf '%s' "${ADD_OUT}" | grep -qiE "failure|syntax error|invalid value|unknown parameter"; then
    fail "T6.b: /container/add failed"
    printf '%s\n' "${ADD_OUT}" | sed 's/^/    /' >&2
    exit 5
fi
pass "T6.b: RouterOS probe container object created"

STARTED=0
for _attempt in $(seq 1 36); do
    START_OUT="$(routeros_ssh "/container/start [find where name=\"${PROBE_CONTAINER_NAME}\"]" 2>&1 || true)"
    if printf '%s' "${START_OUT}" | grep -qi "not stopped"; then
        STARTED=1
        break
    fi
    if ! printf '%s' "${START_OUT}" | grep -qiE "failure|no such item|invalid"; then
        STARTED=1
        break
    fi
    sleep 5
done
if [[ "${STARTED}" -ne 1 ]]; then
    fail "T6.c: RouterOS probe container did not accept start"
    printf '%s\n' "${START_OUT:-}" | sed 's/^/    /' >&2
    exit 5
fi
pass "T6.c: RouterOS probe container start requested"

if wait_for_container_running; then
    pass "T6.d: RouterOS probe container is running"
else
    fail "T6.d: RouterOS probe container did not reach running state"
    routeros_ssh "/container/print detail where name=\"${PROBE_CONTAINER_NAME}\"" 2>&1 | sed 's/^/    /' >&2 || true
    routeros_ssh "/log/print where topics~\"container\"" 2>&1 | sed 's/^/    log: /' >&2 || true
    exit 5
fi

if wait_for_probe_marker; then
    pass "T6.e: RouterOS /log contains probe output marker"
else
    fail "T6.e: RouterOS /log does not contain probe output marker"
    exit 5
fi

require_counter_increased "T6.f" "ip/firewall/nat" "${NAT_COMMENT}" || exit 5
require_counter_increased "T6.g" "ip/firewall/filter" "${FW_OUTBOUND_COMMENT}" || exit 5

echo ""
echo "=================================================================="
if [[ "${FAILURES}" -eq 0 ]]; then
    echo -e " mikrotik-chr-baseline-runtime (${CHR_VERSION}): ${GREEN}PASS${RESET} (${PASSES} check(s))"
    EXIT_CODE=0
else
    echo -e " mikrotik-chr-baseline-runtime (${CHR_VERSION}): ${RED}FAIL${RESET} - ${FAILURES} failure(s), ${PASSES} pass(es)"
    EXIT_CODE="${FAILURES}"
fi
echo "=================================================================="
echo ""

exit "${EXIT_CODE}"
