#!/usr/bin/env bash
# build-chr-baseline.sh — produce awg-mesh-chr-baseline:${CHR_VERSION} Docker
# image, an "already-warm" RouterOS CHR ready for E2E sim runs.
#
# Bakes-in:
#   - Admin password set (lintpass)
#   - Container package installed
#   - Container device-mode enabled and verified after QEMU reset/power cycle
#   - Container support verified via /container/print
#   - SSH key auth ready (still password — CHR per default)
#
# Idempotent — checks for existing image first; exits 0 if already built.
#
# Usage:
#   bash tests/simulation/lib/build-chr-baseline.sh                  # default 7.21.4
#   CHR_VERSION=7.21.4 bash tests/simulation/lib/build-chr-baseline.sh
#   FORCE=1 bash tests/simulation/lib/build-chr-baseline.sh          # rebuild even if exists
#
# Exit codes: 0 success / 1 build failure / 2 missing deps / 3 SSH timeout.
set -euo pipefail

readonly CHR_VERSION="${CHR_VERSION:-7.21.4}"
readonly UPSTREAM_IMAGE="evilfreelancer/docker-routeros:${CHR_VERSION}"
readonly BASELINE_IMAGE="awg-mesh-chr-baseline:${CHR_VERSION}"
readonly BASELINE_READY_LABEL="awg-mesh.chr-container-enabled"
readonly BUILD_CTR="chr-baseline-build-${CHR_VERSION//[.\/]/-}"
readonly BUILD_NET="chr-baseline-net-${CHR_VERSION//[.\/]/-}"
readonly VALIDATE_CTR="${BUILD_CTR}-validate"
readonly VALIDATE_NET="${BUILD_NET}-validate"
readonly SSH_HOST_PORT="${SSH_HOST_PORT:-2299}"
readonly TELNET_HOST_PORT="${TELNET_HOST_PORT:-2300}"
readonly VALIDATE_SSH_HOST_PORT="${VALIDATE_SSH_HOST_PORT:-$((24000 + RANDOM % 1000))}"
readonly ROUTEROS_FIRST_BOOT_WAIT="${ROUTEROS_FIRST_BOOT_WAIT:-90}"
readonly SSH_USER="admin"
readonly NEW_PASS="lintpass"
readonly ROUTEROS_NIC_MAC="${ROUTEROS_NIC_MAC:-3e:b1:b2:e4:28:54}"
readonly CONTAINER_NPK_URL="${CONTAINER_NPK_URL:-https://download.mikrotik.com/routeros/${CHR_VERSION}/container-${CHR_VERSION}.npk}"
readonly QMP_SOCKET="/tmp/awg-mesh-chr-qmp.sock"
readonly QMP_POWER_COMMAND="${QMP_POWER_COMMAND:-system_reset}"
readonly DEVICE_MODE_ACTIVATION_TIMEOUT="${DEVICE_MODE_ACTIVATION_TIMEOUT:-00:01:00}"
TMP_DIR=""
readonly -a SSH_OPTS=(
    -o StrictHostKeyChecking=no
    -o UserKnownHostsFile=/dev/null
    -o LogLevel=ERROR
    -o PreferredAuthentications=password
    -o PubkeyAuthentication=no
    -o ConnectTimeout=10
)

# ---------------------------------------------------------------------------
# Pre-flight
# ---------------------------------------------------------------------------
for cmd in curl docker python3 scp sshpass ssh timeout; do
    if ! command -v "${cmd}" > /dev/null 2>&1; then
        echo "ERROR: ${cmd} not found in PATH" >&2
        exit 2
    fi
done

if [[ ! -c /dev/kvm ]]; then
    echo "ERROR: /dev/kvm missing — RouterOS CHR requires KVM" >&2
    exit 2
fi

# ---------------------------------------------------------------------------
# Idempotency check
# ---------------------------------------------------------------------------
baseline_ready_label() {
    docker image inspect "${BASELINE_IMAGE}" --format '{{ index .Config.Labels "awg-mesh.chr-container-enabled" }}' 2>/dev/null || true
}

if [[ "${FORCE:-0}" != "1" ]] && docker image inspect "${BASELINE_IMAGE}" > /dev/null 2>&1; then
    if [[ "$(baseline_ready_label)" == "true" ]]; then
        echo "[baseline] ${BASELINE_IMAGE} already exists with ${BASELINE_READY_LABEL}=true — skipping build (FORCE=1 to override)"
        exit 0
    fi
    echo "[baseline] ${BASELINE_IMAGE} exists but is missing ${BASELINE_READY_LABEL}=true — rebuilding"
fi

# ---------------------------------------------------------------------------
# Cleanup trap
# ---------------------------------------------------------------------------
cleanup() {
    if [[ "${NO_CLEANUP:-0}" == "1" ]]; then
        echo "[baseline] NO_CLEANUP=1 — keeping ${BUILD_CTR}, ${BUILD_NET}, ${VALIDATE_CTR}, and ${VALIDATE_NET}"
        return
    fi
    docker rm -f "${BUILD_CTR}" > /dev/null 2>&1 || true
    docker rm -f "${VALIDATE_CTR}" > /dev/null 2>&1 || true
    docker network rm "${BUILD_NET}" > /dev/null 2>&1 || true
    docker network rm "${VALIDATE_NET}" > /dev/null 2>&1 || true
    if [[ -n "${TMP_DIR}" && -d "${TMP_DIR}" ]]; then
        rm -rf "${TMP_DIR}"
    fi
}
trap cleanup EXIT

routeros_ssh() {
    sshpass -p "${NEW_PASS}" ssh "${SSH_OPTS[@]}" -p "${SSH_HOST_PORT}" "${SSH_USER}@127.0.0.1" "$@"
}

wait_ssh_ready() {
    local label="${1}"
    local attempts="${2}"

    echo "[baseline] Waiting CHR SSH ready after ${label} (up to $((attempts * 5))s)..."
    for i in $(seq 1 "${attempts}"); do
        if routeros_ssh ":put ssh-ready" > /dev/null 2>&1; then
            echo "[baseline] SSH ready after ${label} (${i} attempts)"
            return 0
        fi
        sleep 5
    done

    echo "ERROR: CHR SSH not ready after ${label}" >&2
    docker logs --tail 80 "${BUILD_CTR}" >&2
    return 1
}

connect_secondary_network() {
    local network="${1}"
    local container="${2}"

    if ! docker network connect --help | grep -q -- '--gw-priority'; then
        echo "ERROR: Docker lacks 'docker network connect --gw-priority'." >&2
        echo "       CHR requires eth0 to remain the default route while eth1 is bridged into QEMU." >&2
        return 1
    fi

    docker network connect \
        --gw-priority -1 \
        --driver-opt com.docker.network.endpoint.ifname=eth1 \
        "${network}" \
        "${container}"
}

cold_power_cycle() {
    local reason="${1}"

    echo "[baseline] QEMU activation cycle (${reason}; ${QMP_POWER_COMMAND})..."
    docker exec -i "${BUILD_CTR}" python3 - "${QMP_SOCKET}" "${QMP_POWER_COMMAND}" <<'PY'
import json
import socket
import sys

power_command = sys.argv[2]
sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
sock.settimeout(10)
sock.connect(sys.argv[1])
sock.recv(4096)
for command in ("qmp_capabilities", power_command):
    sock.sendall(json.dumps({"execute": command}).encode() + b"\r\n")
    if command == "qmp_capabilities":
        sock.recv(4096)
sock.close()
PY
    if [[ "${QMP_POWER_COMMAND}" == "system_reset" ]]; then
        sleep 20
        wait_ssh_ready "${reason}" 60
        return
    fi
    for _attempt in $(seq 1 60); do
        if [[ "$(docker inspect -f '{{.State.Running}}' "${BUILD_CTR}" 2>/dev/null || echo false)" == "false" ]]; then
            break
        fi
        sleep 1
    done
    sleep 20
    docker start "${BUILD_CTR}" > /dev/null
    wait_ssh_ready "${reason}" 60
}

routeros_reboot() {
    local reason="${1}"

    echo "[baseline] RouterOS reboot (${reason})..."
    timeout 20s sshpass -p "${NEW_PASS}" ssh "${SSH_OPTS[@]}" -p "${SSH_HOST_PORT}" "${SSH_USER}@127.0.0.1" "/system/reboot" > /dev/null 2>&1 || true
    sleep 20
    wait_ssh_ready "${reason}" 60
}

stop_qemu_for_snapshot() {
    echo "[baseline] Stopping CHR before snapshot..."
    timeout 20s sshpass -p "${NEW_PASS}" ssh "${SSH_OPTS[@]}" -p "${SSH_HOST_PORT}" "${SSH_USER}@127.0.0.1" '/system/script/remove [find name=awg_mesh_shutdown_once]; /system/script/add name=awg_mesh_shutdown_once source="/system/shutdown"; /system/script/run awg_mesh_shutdown_once' > /dev/null 2>&1 || true
    for _attempt in $(seq 1 60); do
        if [[ "$(docker inspect -f '{{.State.Running}}' "${BUILD_CTR}" 2>/dev/null || echo false)" == "false" ]]; then
            echo "[baseline] CHR stopped for snapshot"
            return 0
        fi
        sleep 1
    done

    echo "WARN: RouterOS guest shutdown did not stop CHR; falling back to QMP quit" >&2
    docker exec -i "${BUILD_CTR}" python3 - "${QMP_SOCKET}" <<'PY'
import json
import socket
import sys

sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
sock.settimeout(10)
sock.connect(sys.argv[1])
sock.recv(4096)
for command in ("qmp_capabilities", "quit"):
    sock.sendall(json.dumps({"execute": command}).encode() + b"\r\n")
    if command == "qmp_capabilities":
        sock.recv(4096)
sock.close()
PY
    for _attempt in $(seq 1 60); do
        if [[ "$(docker inspect -f '{{.State.Running}}' "${BUILD_CTR}" 2>/dev/null || echo false)" == "false" ]]; then
            echo "[baseline] CHR stopped for snapshot"
            return 0
        fi
        sleep 1
    done

    echo "WARN: QMP quit did not stop CHR before snapshot; forcing container stop" >&2
    docker kill "${BUILD_CTR}" > /dev/null
}

validate_committed_baseline() {
    echo "[baseline] Validating committed snapshot boot..."
    docker rm -f "${VALIDATE_CTR}" > /dev/null 2>&1 || true
    docker network rm "${VALIDATE_NET}" > /dev/null 2>&1 || true
    docker network create "${VALIDATE_NET}" > /dev/null
    docker create \
        --name "${VALIDATE_CTR}" \
        --network name=bridge,driver-opt=com.docker.network.endpoint.ifname=eth0 \
        -e ROUTEROS_NIC_MAC="${ROUTEROS_NIC_MAC}" \
        --device /dev/kvm \
        --device /dev/net/tun \
        --cap-add NET_ADMIN \
        -p "${VALIDATE_SSH_HOST_PORT}:22" \
        "${BASELINE_IMAGE}" > /dev/null
    connect_secondary_network "${VALIDATE_NET}" "${VALIDATE_CTR}"
    docker start "${VALIDATE_CTR}" > /dev/null

    echo "[baseline] Waiting committed snapshot SSH ready (up to 300s)..."
    for i in $(seq 1 60); do
        if sshpass -p "${NEW_PASS}" ssh "${SSH_OPTS[@]}" -p "${VALIDATE_SSH_HOST_PORT}" "${SSH_USER}@127.0.0.1" ":put committed-ready" > /dev/null 2>&1; then
            echo "[baseline] Committed snapshot SSH ready (${i} attempts)"
            DEVICE_MODE_OUT="$(sshpass -p "${NEW_PASS}" ssh "${SSH_OPTS[@]}" -p "${VALIDATE_SSH_HOST_PORT}" "${SSH_USER}@127.0.0.1" "/system/device-mode/print" 2>&1 || true)"
            PACKAGE_OUT="$(sshpass -p "${NEW_PASS}" ssh "${SSH_OPTS[@]}" -p "${VALIDATE_SSH_HOST_PORT}" "${SSH_USER}@127.0.0.1" "/system/package/print detail where name=container" 2>&1 || true)"
            if ! printf '%s' "${DEVICE_MODE_OUT}" | grep -q "container: yes"; then
                echo "ERROR: committed snapshot lost device-mode container=yes" >&2
                printf '%s\n' "${DEVICE_MODE_OUT}" | sed 's/^/    /' >&2
                return 1
            fi
            if ! printf '%s' "${PACKAGE_OUT}" | grep -Eq 'name="?container"?'; then
                echo "ERROR: committed snapshot lost container package" >&2
                printf '%s\n' "${PACKAGE_OUT}" | sed 's/^/    /' >&2
                return 1
            fi
            sshpass -p "${NEW_PASS}" ssh "${SSH_OPTS[@]}" -p "${VALIDATE_SSH_HOST_PORT}" "${SSH_USER}@127.0.0.1" "/container/print; :put committed-container-ok" > /dev/null
            docker rm -f "${VALIDATE_CTR}" > /dev/null 2>&1 || true
            docker network rm "${VALIDATE_NET}" > /dev/null 2>&1 || true
            echo "[baseline] Committed snapshot validation passed"
            return 0
        fi
        sleep 5
    done

    echo "ERROR: committed snapshot did not boot to SSH" >&2
    docker logs --tail 80 "${VALIDATE_CTR}" >&2 || true
    return 1
}

seed_password_via_telnet() {
    python3 - "${TELNET_HOST_PORT}" "${NEW_PASS}" <<'PY'
import select
import socket
import sys
import time

port = int(sys.argv[1])
password = sys.argv[2].encode()

def connect(deadline):
    last_error = None
    while time.time() < deadline:
        try:
            sock = socket.create_connection(("127.0.0.1", port), timeout=10)
            sock.setblocking(False)
            return sock
        except OSError as exc:
            last_error = exc
            time.sleep(5)
    raise SystemExit(f"telnet connect timeout on port {port}: {last_error}")

def strip_telnet(sock, data):
    out = bytearray()
    reply = bytearray()
    i = 0
    while i < len(data):
        byte = data[i]
        if byte == 255 and i + 2 < len(data):
            command = data[i + 1]
            option = data[i + 2]
            if command in (251, 252):
                reply += bytes([255, 254, option])
            elif command in (253, 254):
                reply += bytes([255, 252, option])
            i += 3
            continue
        out.append(byte)
        i += 1
    if reply:
        sock.sendall(reply)
    return bytes(out)

connect_deadline = time.time() + 300
sock = connect(connect_deadline)
buffer = b""
state = "login"
deadline = time.time() + 180
last_data = time.time()

while time.time() < deadline:
    ready, _, _ = select.select([sock], [], [], 0.2)
    if ready:
        chunk = sock.recv(4096)
        if not chunk:
            sock.close()
            if state == "login":
                time.sleep(5)
                sock = connect(connect_deadline)
                buffer = b""
                continue
            raise SystemExit("telnet closed before password seed completed")
        cleaned = strip_telnet(sock, chunk).lower()
        if cleaned:
            last_data = time.time()
            buffer += cleaned
    elif state == "login" and time.time() - last_data > 10:
        sock.close()
        sock = connect(connect_deadline)
        buffer = b""
        last_data = time.time()

    if state == "login" and b"login:" in buffer:
        sock.sendall(b"admin\r")
        buffer = b""
        state = "password"
    elif state == "password" and b"password:" in buffer:
        sock.sendall(b"\r")
        buffer = b""
        state = "license"
    elif state == "license" and b"license" in buffer:
        sock.sendall(b"n\r")
        buffer = b""
        state = "new_password"
    elif state == "new_password" and b"new password>" in buffer:
        sock.sendall(password + b"\r")
        buffer = b""
        state = "repeat_password"
    elif state == "repeat_password" and b"repeat new password>" in buffer:
        sock.sendall(password + b"\r")
        buffer = b""
        state = "prompt"
    elif state == "prompt" and b"] >" in buffer:
        sock.sendall(b":put password-seeded\r")
        buffer = b""
        state = "done"
    elif state == "done" and b"password-seeded" in buffer:
        sock.close()
        print("password-seeded")
        raise SystemExit(0)

tail = buffer[-500:].decode("latin1", errors="replace")
raise SystemExit(f"telnet password seed timed out in state {state}; tail={tail!r}")
PY
}

# ---------------------------------------------------------------------------
# 1. Pull upstream
# ---------------------------------------------------------------------------
echo "[baseline] Pulling ${UPSTREAM_IMAGE}..."
docker pull "${UPSTREAM_IMAGE}" > /dev/null

# ---------------------------------------------------------------------------
# 2. Boot vanilla CHR
# ---------------------------------------------------------------------------
echo "[baseline] Starting vanilla CHR..."
docker rm -f "${BUILD_CTR}" > /dev/null 2>&1 || true
docker network rm "${BUILD_NET}" > /dev/null 2>&1 || true
docker network create "${BUILD_NET}" > /dev/null
# docker-routeros enables QEMU hostfwd only when a second Docker NIC exists.
docker create \
    --name "${BUILD_CTR}" \
    --network name=bridge,driver-opt=com.docker.network.endpoint.ifname=eth0 \
    -e ROUTEROS_NIC_MAC="${ROUTEROS_NIC_MAC}" \
    --device /dev/kvm \
    --device /dev/net/tun \
    --cap-add NET_ADMIN \
    -p "${SSH_HOST_PORT}:22" \
    -p "${TELNET_HOST_PORT}:23" \
    "${UPSTREAM_IMAGE}" \
    -qmp "unix:${QMP_SOCKET},server,nowait" > /dev/null
connect_secondary_network "${BUILD_NET}" "${BUILD_CTR}"
docker start "${BUILD_CTR}" > /dev/null

# ---------------------------------------------------------------------------
# 3. Seed first-boot password over local telnet
# ---------------------------------------------------------------------------
echo "[baseline] Seeding first-boot admin password over local telnet..."
sleep "${ROUTEROS_FIRST_BOOT_WAIT}"
seed_password_via_telnet
wait_ssh_ready "first-boot password seed" 60

# ---------------------------------------------------------------------------
# 4. First-boot init verification
# ---------------------------------------------------------------------------
echo "[baseline] Password set + verified"

# ---------------------------------------------------------------------------
# 5. Install RouterOS container package
# ---------------------------------------------------------------------------
TMP_DIR="$(mktemp -d)"
CONTAINER_NPK="${TMP_DIR}/container-${CHR_VERSION}.npk"
echo "[baseline] Downloading RouterOS container package: ${CONTAINER_NPK_URL}"
curl -fL "${CONTAINER_NPK_URL}" -o "${CONTAINER_NPK}" > /dev/null

echo "[baseline] Uploading container package into CHR..."
sshpass -p "${NEW_PASS}" scp "${SSH_OPTS[@]}" -P "${SSH_HOST_PORT}" "${CONTAINER_NPK}" "${SSH_USER}@127.0.0.1:container-${CHR_VERSION}.npk" > /dev/null

FILE_OUT="$(routeros_ssh "/file/print detail where name=\"container-${CHR_VERSION}.npk\"" 2>&1 || true)"
if ! printf '%s' "${FILE_OUT}" | grep -q "container-${CHR_VERSION}.npk"; then
    echo "ERROR: uploaded container package is not visible in RouterOS files" >&2
    printf '%s\n' "${FILE_OUT}" | sed 's/^/    /' >&2
    exit 1
fi

# RouterOS installs uploaded .npk files during an orderly RouterOS reboot.
routeros_reboot "container package install"

PACKAGE_OUT="$(routeros_ssh "/system/package/print detail where name=container" 2>&1 || true)"
if ! printf '%s' "${PACKAGE_OUT}" | grep -Eq 'name="?container"?'; then
    echo "ERROR: container package did not install" >&2
    printf '%s\n' "${PACKAGE_OUT}" | sed 's/^/    /' >&2
    exit 1
fi
echo "[baseline] Container package installed"

# ---------------------------------------------------------------------------
# 6. Enable device-mode container support
# ---------------------------------------------------------------------------
DEVICE_MODE_OUT="$(routeros_ssh "/system/device-mode/print" 2>&1 || true)"
if printf '%s' "${DEVICE_MODE_OUT}" | grep -q "container: yes"; then
    echo "[baseline] Device-mode container already enabled"
else
    echo "[baseline] Enabling device-mode container support..."
    DEVICE_MODE_LOG="${TMP_DIR}/device-mode-update.log"
    routeros_ssh "/system/device-mode/update container=yes activation-timeout=${DEVICE_MODE_ACTIVATION_TIMEOUT}" > "${DEVICE_MODE_LOG}" 2>&1 &
    DEVICE_MODE_PID=$!
    sleep 5
    cold_power_cycle "device-mode container enable"
    if kill -0 "${DEVICE_MODE_PID}" 2>/dev/null; then
        kill "${DEVICE_MODE_PID}" 2>/dev/null || true
    fi
    wait "${DEVICE_MODE_PID}" > /dev/null 2>&1 || true
fi

DEVICE_MODE_OUT="$(routeros_ssh "/system/device-mode/print" 2>&1 || true)"
if ! printf '%s' "${DEVICE_MODE_OUT}" | grep -q "container: yes"; then
    echo "ERROR: device-mode container support is not enabled after QEMU power cycle" >&2
    echo "       RouterOS requires x86 cold-reboot confirmation for container=yes." >&2
    echo "       If this repeats with attempt-count > 0, this docker-routeros/QEMU host did not provide" >&2
    echo "       the reset/power event RouterOS accepts; do not commit or reuse this baseline image." >&2
    printf '%s\n' "${DEVICE_MODE_OUT}" | sed 's/^/    /' >&2
    exit 1
fi
echo "[baseline] Device-mode container enabled"

# ---------------------------------------------------------------------------
# 7. Configure container subsystem
# ---------------------------------------------------------------------------
routeros_ssh "/container/config/set registry-url=https://lscr.io memory-high=512M; :put cfg-set" > /dev/null

# Verify container subsystem responds.
routeros_ssh "/container/print; :put container-ok" > /dev/null 2>&1 || {
    echo "ERROR: /container/print failed — CHR cannot serve the RouterOS container gate" >&2
    exit 1
}
echo "[baseline] Container subsystem configured"

stop_qemu_for_snapshot

# ---------------------------------------------------------------------------
# 8. Commit baseline
# ---------------------------------------------------------------------------
echo "[baseline] Committing snapshot to ${BASELINE_IMAGE}..."
docker commit \
    --change "LABEL awg-mesh.chr-baseline=${CHR_VERSION}" \
    --change "LABEL awg-mesh.chr-pass=${NEW_PASS}" \
    --change "LABEL ${BASELINE_READY_LABEL}=true" \
    --change "LABEL desktop.docker.io/ports.scheme=" \
    --change "LABEL desktop.docker.io/ports/22/tcp=" \
    --change "LABEL desktop.docker.io/ports/23/tcp=" \
    "${BUILD_CTR}" "${BASELINE_IMAGE}" > /dev/null

validate_committed_baseline

echo "[baseline] DONE — ${BASELINE_IMAGE} ready"
echo "[baseline]   Use: docker run --device /dev/kvm --device /dev/net/tun --cap-add NET_ADMIN -p 2222:22 ${BASELINE_IMAGE}"
echo "[baseline]   SSH: sshpass -p '${NEW_PASS}' ssh ${SSH_USER}@127.0.0.1"
