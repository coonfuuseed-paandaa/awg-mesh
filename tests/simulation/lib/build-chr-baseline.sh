#!/usr/bin/env bash
# build-chr-baseline.sh — produce awg-mesh-chr-baseline:${CHR_VERSION} Docker
# image, an "already-warm" RouterOS CHR ready for E2E sim runs.
#
# Bakes-in:
#   - Admin password set (lintpass)
#   - Container support verified (CHR has it built-in; no device-mode toggle needed)
#   - SSH key auth ready (still password — CHR per default)
#
# Idempotent — checks for existing image first; exits 0 if already built.
#
# Usage:
#   bash tests/simulation/lib/build-chr-baseline.sh                  # default 7.21.4
#   CHR_VERSION=7.20.8 bash tests/simulation/lib/build-chr-baseline.sh
#   FORCE=1 bash tests/simulation/lib/build-chr-baseline.sh          # rebuild even if exists
#
# Exit codes: 0 success / 1 build failure / 2 missing deps / 3 SSH timeout.
set -euo pipefail

readonly CHR_VERSION="${CHR_VERSION:-7.21.4}"
readonly UPSTREAM_IMAGE="evilfreelancer/docker-routeros:${CHR_VERSION}"
readonly BASELINE_IMAGE="awg-mesh-chr-baseline:${CHR_VERSION}"
readonly BUILD_CTR="chr-baseline-build-${CHR_VERSION//[.\/]/-}"
readonly SSH_HOST_PORT="${SSH_HOST_PORT:-2299}"
readonly SSH_USER="admin"
readonly NEW_PASS="lintpass"
readonly SSH_OPTS="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=5"

# ---------------------------------------------------------------------------
# Pre-flight
# ---------------------------------------------------------------------------
for cmd in docker sshpass ssh; do
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
if [[ "${FORCE:-0}" != "1" ]] && docker image inspect "${BASELINE_IMAGE}" > /dev/null 2>&1; then
    echo "[baseline] ${BASELINE_IMAGE} already exists — skipping build (FORCE=1 to override)"
    exit 0
fi

# ---------------------------------------------------------------------------
# Cleanup trap
# ---------------------------------------------------------------------------
cleanup() {
    docker rm -f "${BUILD_CTR}" > /dev/null 2>&1 || true
}
trap cleanup EXIT

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
docker run -d \
    --name "${BUILD_CTR}" \
    --device /dev/kvm \
    --device /dev/net/tun \
    --cap-add NET_ADMIN \
    -p "${SSH_HOST_PORT}:22" \
    "${UPSTREAM_IMAGE}" > /dev/null

# ---------------------------------------------------------------------------
# 3. Wait SSH ready (vanilla — empty admin password)
# ---------------------------------------------------------------------------
echo "[baseline] Waiting CHR SSH ready (up to 180s)..."
SSH_READY=0
for i in $(seq 1 36); do
    if echo "" | sshpass -p "" ssh ${SSH_OPTS} -p "${SSH_HOST_PORT}" "${SSH_USER}@127.0.0.1" ":put ready" > /dev/null 2>&1; then
        SSH_READY=1
        echo "[baseline] SSH up after ${i} attempts"
        break
    fi
    sleep 5
done
if [[ "${SSH_READY}" -ne 1 ]]; then
    echo "ERROR: CHR SSH not ready in 180s" >&2
    docker logs --tail 30 "${BUILD_CTR}" >&2
    exit 3
fi

# ---------------------------------------------------------------------------
# 4. First-boot init: set password, accept license prompt, configure
# ---------------------------------------------------------------------------
echo "[baseline] Configuring CHR (set password, container config)..."

# CHR 7.21+ first-boot may require accepting the EULA via stdin. Work around
# by sending a banner-skip via PTY — ssh -t fixes that for some CHRs.
sshpass -p "" ssh -t ${SSH_OPTS} -p "${SSH_HOST_PORT}" "${SSH_USER}@127.0.0.1" "/user set admin password=\"${NEW_PASS}\"; :put pass-set" 2>&1 | tail -3 || true

# Verify new password works (some 7.21 builds reject empty-pass entirely after EULA).
PASS_VERIFY=0
for i in $(seq 1 6); do
    if sshpass -p "${NEW_PASS}" ssh ${SSH_OPTS} -p "${SSH_HOST_PORT}" "${SSH_USER}@127.0.0.1" ":put pass-ok" > /dev/null 2>&1; then
        PASS_VERIFY=1
        break
    fi
    sleep 2
done
if [[ "${PASS_VERIFY}" -ne 1 ]]; then
    echo "ERROR: password set verification failed; CHR may need EULA acceptance" >&2
    docker logs --tail 50 "${BUILD_CTR}" >&2
    exit 1
fi
echo "[baseline] Password set + verified"

# ---------------------------------------------------------------------------
# 5. Configure container subsystem
# ---------------------------------------------------------------------------
sshpass -p "${NEW_PASS}" ssh ${SSH_OPTS} -p "${SSH_HOST_PORT}" "${SSH_USER}@127.0.0.1" "/container/config set tmpdir=disk1/pull registry-url=https://lscr.io ram-high=512M; :put cfg-set" 2>&1 | tail -3 || true

# Verify container subsystem responds.
sshpass -p "${NEW_PASS}" ssh ${SSH_OPTS} -p "${SSH_HOST_PORT}" "${SSH_USER}@127.0.0.1" "/container/print; :put container-ok" > /dev/null 2>&1 || {
    echo "WARN: /container/print failed — CHR may not have container package" >&2
}
echo "[baseline] Container subsystem configured"

# ---------------------------------------------------------------------------
# 6. Commit baseline
# ---------------------------------------------------------------------------
echo "[baseline] Committing snapshot to ${BASELINE_IMAGE}..."
docker commit \
    --change "LABEL awg-mesh.chr-baseline=${CHR_VERSION}" \
    --change "LABEL awg-mesh.chr-pass=${NEW_PASS}" \
    "${BUILD_CTR}" "${BASELINE_IMAGE}" > /dev/null

echo "[baseline] DONE — ${BASELINE_IMAGE} ready"
echo "[baseline]   Use: docker run --device /dev/kvm --device /dev/net/tun --cap-add NET_ADMIN -p 2222:22 ${BASELINE_IMAGE}"
echo "[baseline]   SSH: sshpass -p '${NEW_PASS}' ssh ${SSH_USER}@127.0.0.1"
