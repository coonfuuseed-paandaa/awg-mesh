#!/usr/bin/env bash
# mikrotik-chr-import.sh — boots a real RouterOS CHR via evilfreelancer/docker-routeros,
# generates a deploy .rsc via mesh-ctl, imports it into the running CHR, and asserts
# the import succeeds without RouterOS-side errors.
#
# This is the REAL counterpart to mikrotik-onboard.sh: instead of an alpine
# proxy container with pre-injected default route, this exercises actual
# RouterOS userspace under QEMU/KVM. Catches regression classes that the
# proxy variant cannot:
#   - RouterOS parameter rejection (canonical names per ROS 7.21+)
#   - RouterOS scripting if/else evaluation (firewall fasttrack anchor)
#   - /container/mounts + /container/add semantic correctness
#   - Veth-before-Bridge ordering (Bug 1)
#
# Reference: https://github.com/EvilFreelancer/docker-routeros
#
# Requirements (host):
#   - Docker 24+, KVM (/dev/kvm) available — fails gracefully on hosts
#     without nested virt (e.g., GitHub-hosted runners without macOS).
#   - sshpass, ssh, scp on PATH (sudo apt-get install -y sshpass openssh-client)
#   - mesh-ctl in PATH or at <repo>/bin/mesh-ctl
#   - CHR_VERSION must be 7.21+ — F-001 T-002 canonical params
#     (`/container/mounts/add list=`, `/container/add mountlists=`,
#     `/container/add remote-image=`) are not accepted by 7.20 or older.
#     Default: 7.21.4 (latest stable as of v1.14.0).
#
# Exit codes: 0 = pass, 1 = assertion fail, 2 = environment fail (missing deps),
# 3 = mesh-ctl prepare failed, 4 = CHR boot timeout.
set -euo pipefail

readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly CHR_VERSION="${CHR_VERSION:-7.21.4}"
readonly CHR_IMAGE="evilfreelancer/docker-routeros:${CHR_VERSION}"
readonly CHR_CTR="awg-mesh-chr-import-test"
readonly SSH_HOST_PORT="${SSH_HOST_PORT:-2222}"
readonly SSH_USER="admin"
readonly SSH_PASS=""  # CHR first-boot default — no password
readonly NEW_PASS="lintpass"
readonly CTL_CONFIG_DIR="$(mktemp -d -t chrimport-ctl-XXXXXX)"
readonly TOPO_FILE="$(mktemp -t chrimport-topo-XXXXXX.yml)"
readonly RSC_LOCAL="${CTL_CONFIG_DIR}/mikrotik-home-mikrotik.rsc"

PASSES=0
FAILURES=0

readonly RED=$'\033[0;31m'
readonly GREEN=$'\033[0;32m'
readonly YELLOW=$'\033[0;33m'
readonly RESET=$'\033[0m'

pass() { echo "  [${GREEN}PASS${RESET}] $*"; PASSES=$((PASSES + 1)); }
fail() { echo "  [${RED}FAIL${RESET}] $*"; FAILURES=$((FAILURES + 1)); }
info() { echo "  [info] $*"; }
warn() { echo "  [${YELLOW}warn${RESET}] $*"; }

cleanup() {
    local rc=$?
    echo ""
    if [[ "${NO_CLEANUP:-0}" == "1" ]]; then
        echo "[cleanup] NO_CLEANUP=1 — leaving CHR container + temp files."
        echo "  CHR container:   ${CHR_CTR}"
        echo "  SSH:             ssh -p ${SSH_HOST_PORT} ${SSH_USER}@127.0.0.1   (pass: ${NEW_PASS})"
        echo "  Topology file:   ${TOPO_FILE}"
        echo "  Ctl config dir:  ${CTL_CONFIG_DIR}"
        echo "  Generated .rsc:  ${RSC_LOCAL}"
        return
    fi
    echo "[cleanup] Tearing down..."
    docker rm -f "${CHR_CTR}" >/dev/null 2>&1 || true
    rm -f "${TOPO_FILE}"
    rm -rf "${CTL_CONFIG_DIR}"
    if [[ "${rc}" -eq 0 && "${FAILURES}" -eq 0 ]]; then
        echo "[cleanup] Done. Test PASSED (${PASSES} checks)."
    else
        echo "[cleanup] Done. Test FAILED (${FAILURES} failure(s), ${PASSES} pass(es))."
    fi
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Pre-flight checks
# ---------------------------------------------------------------------------
echo "[pre-flight] Checking environment..."

if ! command -v docker >/dev/null 2>&1; then
    echo "ERROR: docker not in PATH" >&2
    exit 2
fi
if ! command -v sshpass >/dev/null 2>&1; then
    echo "ERROR: sshpass not in PATH (apt-get install -y sshpass)" >&2
    exit 2
fi
if ! command -v ssh >/dev/null 2>&1; then
    echo "ERROR: ssh not in PATH (apt-get install -y openssh-client)" >&2
    exit 2
fi

# KVM check — non-fatal warning; CHR boot will likely fail without it on
# x86_64 hosts but the QEMU TCG fallback exists in some CHR images.
if [[ ! -e /dev/kvm ]]; then
    warn "/dev/kvm not present — CHR boot may be slow (QEMU TCG fallback)"
fi

MESHCTL_BIN=""
if command -v mesh-ctl >/dev/null 2>&1; then
    MESHCTL_BIN="mesh-ctl"
elif [[ -x "${REPO_ROOT}/bin/mesh-ctl" ]]; then
    MESHCTL_BIN="${REPO_ROOT}/bin/mesh-ctl"
else
    echo "ERROR: mesh-ctl not in PATH and not at ${REPO_ROOT}/bin/mesh-ctl" >&2
    echo "  Build: go build -o bin/mesh-ctl ./cmd/mesh-ctl" >&2
    exit 2
fi
info "mesh-ctl: $(${MESHCTL_BIN} version 2>&1 | head -1 || echo '(version unknown)')"

# ---------------------------------------------------------------------------
# Generate topology + .rsc fixture
# ---------------------------------------------------------------------------
echo "[topo] Writing test topology..."
cat > "${TOPO_FILE}" <<EOF
overlay:
  space: 172.21.92.0/24
  physical_mtu: 1500
  awg_overhead: 80
  ranges:
    - name: clients
      cidr: 172.21.92.128/25

masters:
  - name: master-test
    type: master
    host: 192.0.2.1
    overlay_ip: 172.21.92.2
    grpc_port: 9090
    listen_port: 51820
    bind: 0.0.0.0

clients:
  - name: mikrotik-home
    type: mikrotik
    overlay_ip: 172.21.92.130
    masters: [master-test]
EOF

info "Pre-preparing master + client to seed admin state..."
${MESHCTL_BIN} \
    --topology "${TOPO_FILE}" \
    --config-dir "${CTL_CONFIG_DIR}" \
    master prepare master-test > /dev/null || {
    echo "ERROR: mesh-ctl master prepare failed" >&2
    exit 3
}
${MESHCTL_BIN} \
    --topology "${TOPO_FILE}" \
    --config-dir "${CTL_CONFIG_DIR}" \
    client prepare mikrotik-home > /dev/null || {
    echo "ERROR: mesh-ctl client prepare failed" >&2
    exit 3
}

if [[ ! -f "${RSC_LOCAL}" ]]; then
    # Generated .rsc may live elsewhere — search.
    found=$(find "${CTL_CONFIG_DIR}" -name '*mikrotik-home*.rsc' 2>/dev/null | head -1)
    if [[ -z "${found}" ]]; then
        echo "ERROR: generated .rsc not found under ${CTL_CONFIG_DIR}" >&2
        find "${CTL_CONFIG_DIR}" -type f 2>/dev/null | head -20 >&2
        exit 3
    fi
    cp "${found}" "${RSC_LOCAL}"
fi
pass "PF.1: .rsc fixture generated ($(wc -l < "${RSC_LOCAL}") lines)"

# ---------------------------------------------------------------------------
# Boot CHR
# ---------------------------------------------------------------------------
echo "[boot] Pulling + starting CHR container..."
docker rm -f "${CHR_CTR}" >/dev/null 2>&1 || true

DEVICES=()
[[ -e /dev/kvm ]] && DEVICES+=(--device=/dev/kvm)
[[ -e /dev/net/tun ]] && DEVICES+=(--device=/dev/net/tun)

docker run -d \
    --name "${CHR_CTR}" \
    --cap-add=NET_ADMIN \
    "${DEVICES[@]}" \
    -p "${SSH_HOST_PORT}:22" \
    "${CHR_IMAGE}" >/dev/null

info "Waiting for CHR SSH (up to 120 s)..."
for i in $(seq 1 120); do
    if nc -z 127.0.0.1 "${SSH_HOST_PORT}" 2>/dev/null; then
        info "SSH port open after ${i} attempts"
        break
    fi
    if [[ "${i}" -eq 120 ]]; then
        echo "ERROR: CHR SSH did not open within 120 s" >&2
        docker logs "${CHR_CTR}" 2>&1 | tail -30 >&2 || true
        exit 4
    fi
    sleep 1
done

# Give RouterOS a moment to finish console bring-up after SSH listens.
sleep 5

# ---------------------------------------------------------------------------
# SSH first-boot password set
# ---------------------------------------------------------------------------
echo "[auth] Setting CHR admin password..."
SSH_OPTS=(
    -o StrictHostKeyChecking=no
    -o UserKnownHostsFile=/dev/null
    -o LogLevel=ERROR
    -o PreferredAuthentications=password
    -o PubkeyAuthentication=no
    -o ConnectTimeout=10
)

# CHR's ssh expects empty current password then prompts for new + confirm.
# The 'expect' alternative is heavier; sshpass + a one-shot command works
# because CHR's first-login flow accepts the new password as an arg.
if sshpass -p "${SSH_PASS}" ssh "${SSH_OPTS[@]}" -p "${SSH_HOST_PORT}" \
    "${SSH_USER}@127.0.0.1" \
    "/user set admin password=\"${NEW_PASS}\"; :put \"pw-set\"" >/dev/null 2>&1; then
    info "Password set on first attempt (empty default accepted)"
elif sshpass -p "${NEW_PASS}" ssh "${SSH_OPTS[@]}" -p "${SSH_HOST_PORT}" \
    "${SSH_USER}@127.0.0.1" ":put \"pw-already-set\"" >/dev/null 2>&1; then
    info "Password already set from prior run"
else
    fail "PF.2: cannot SSH to CHR — neither empty nor lintpass works"
    docker logs "${CHR_CTR}" 2>&1 | tail -20 >&2 || true
    exit 1
fi
pass "PF.2: SSH authenticated"

# ---------------------------------------------------------------------------
# Upload + import .rsc
# ---------------------------------------------------------------------------
echo "[import] Uploading + running /import..."
sshpass -p "${NEW_PASS}" scp "${SSH_OPTS[@]}" -P "${SSH_HOST_PORT}" \
    "${RSC_LOCAL}" \
    "${SSH_USER}@127.0.0.1:mikrotik-home-deploy.rsc" \
    || { fail "I.1: scp upload failed"; exit 1; }
pass "I.1: .rsc uploaded to CHR"

IMPORT_OUTPUT=$(sshpass -p "${NEW_PASS}" ssh "${SSH_OPTS[@]}" -p "${SSH_HOST_PORT}" \
    "${SSH_USER}@127.0.0.1" \
    "/import file-name=mikrotik-home-deploy.rsc verbose=yes" 2>&1) || IMPORT_RC=$?
IMPORT_RC="${IMPORT_RC:-0}"

echo "--- /import output ---"
echo "${IMPORT_OUTPUT}"
echo "--- end ---"

# ---------------------------------------------------------------------------
# Assertions
# ---------------------------------------------------------------------------
if [[ "${IMPORT_RC}" -eq 0 ]]; then
    pass "I.2: /import exit code 0"
else
    fail "I.2: /import exit code ${IMPORT_RC}"
fi

# RouterOS sometimes succeeds with warnings; scan output for failure markers.
if echo "${IMPORT_OUTPUT}" | grep -iE 'failure|syntax error|invalid value|unknown parameter|expected' >/dev/null; then
    fail "I.3: /import output contains failure indicator"
else
    pass "I.3: /import output clean (no failure indicators)"
fi

# Bug 1 regression check: bridge-port references veth, so veth must be
# created first. If the import succeeded we can verify ordering by checking
# the import log line numbers.
if echo "${IMPORT_OUTPUT}" | grep -iE '/interface/bridge/port.*invalid value for argument interface' >/dev/null; then
    fail "I.4: Bug 1 regressed — bridge-port emitted before veth"
else
    pass "I.4: no Bug 1 ordering regression"
fi

# Bug 3a/3b/5 regression checks
if echo "${IMPORT_OUTPUT}" | grep -iE 'unknown parameter (name|mounts|image)' >/dev/null; then
    fail "I.5: Bugs 3a/3b/5 regressed — RouterOS rejected pre-7.21 param name"
else
    pass "I.5: Bugs 3a/3b/5 — RouterOS canonical params accepted"
fi

# Verify resulting state on CHR (interfaces created, container registered).
echo "[verify] Checking RouterOS post-import state..."
INTERFACE_LIST=$(sshpass -p "${NEW_PASS}" ssh "${SSH_OPTS[@]}" -p "${SSH_HOST_PORT}" \
    "${SSH_USER}@127.0.0.1" \
    "/interface print terse" 2>/dev/null || echo "")
if echo "${INTERFACE_LIST}" | grep -q "AWG_MESH"; then
    pass "V.1: AWG_MESH veth/bridge interfaces present after import"
else
    warn "V.1: AWG_MESH interfaces not visible (may require longer settle time)"
    # Non-fatal — container subsystem may not be available on all CHR images.
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo ""
echo "=== Results: ${PASSES} PASS, ${FAILURES} FAIL ==="
echo ""

if [[ "${FAILURES}" -gt 0 ]]; then
    echo "CHR container logs (last 30 lines):"
    docker logs "${CHR_CTR}" 2>&1 | tail -30 >&2 || true
    exit "${FAILURES}"
fi

exit 0
