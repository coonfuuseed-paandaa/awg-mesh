#!/usr/bin/env bash
# CHR import-lint: boots ephemeral RouterOS CHR via QEMU, SCPs the golden
# .rsc fixture, runs /import, fails on non-zero RouterOS import exit.
#
# Inputs (env):
#   CHR_IMAGE   path to CHR .img (default: chr.img)
#   GOLDEN_RSC  path to .rsc fixture (default: pkg/mikrotik/testdata/deploy-golden.rsc)
#
# Boot model: usermode networking + hostfwd for SSH (port 2222 on host).
# Default CHR creds on first boot: admin / no password — first SSH triggers
# a password-set prompt; we set "lintpass" non-interactively via sshpass.
#
# REQ-2 Phase 2 of F-001 mikrotik-generator-fixes.
set -euo pipefail

CHR_IMAGE="${CHR_IMAGE:-chr.img}"
GOLDEN_RSC="${GOLDEN_RSC:-pkg/mikrotik/testdata/deploy-golden.rsc}"
SSH_PORT=2222
SSH_USER="admin"
SSH_PASS="lintpass"

if [ ! -f "$CHR_IMAGE" ]; then
    echo "ERROR: CHR image not found at $CHR_IMAGE" >&2
    exit 1
fi

if [ ! -f "$GOLDEN_RSC" ]; then
    echo "ERROR: golden fixture not found at $GOLDEN_RSC" >&2
    exit 1
fi

echo "Booting CHR via QEMU (SSH forwarded to host port ${SSH_PORT})..."
qemu-system-x86_64 \
    -drive "file=${CHR_IMAGE},format=raw,if=virtio" \
    -netdev "user,id=net0,hostfwd=tcp::${SSH_PORT}-:22" \
    -device virtio-net-pci,netdev=net0 \
    -m 256 \
    -nographic \
    -daemonize \
    -pidfile /tmp/chr-qemu.pid

# Wait for SSH to come up (CHR boots in ~30 s).
echo "Waiting for CHR SSH..."
for i in $(seq 1 60); do
    if nc -z 127.0.0.1 "$SSH_PORT" 2>/dev/null; then
        echo "SSH port open after ${i} attempts"
        break
    fi
    if [ "$i" -eq 60 ]; then
        echo "ERROR: CHR SSH did not come up within 60 seconds" >&2
        kill "$(cat /tmp/chr-qemu.pid)" 2>/dev/null || true
        exit 1
    fi
    sleep 1
done

# First-boot SSH triggers password-set prompt. CHR accepts an empty current
# password and prompts for a new one. We use SSH_ASKPASS-free approach via
# sshpass + interactive expect-style script.
echo "Setting CHR admin password..."
sshpass -p "" ssh \
    -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null \
    -o PreferredAuthentications=password \
    -o PubkeyAuthentication=no \
    -p "$SSH_PORT" \
    "${SSH_USER}@127.0.0.1" \
    ":put \"chr-ready\"" 2>/dev/null || \
sshpass -p "$SSH_PASS" ssh \
    -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null \
    -o PreferredAuthentications=password \
    -o PubkeyAuthentication=no \
    -p "$SSH_PORT" \
    "${SSH_USER}@127.0.0.1" \
    ":put \"chr-ready\""

echo "Uploading golden fixture..."
sshpass -p "$SSH_PASS" scp \
    -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null \
    -P "$SSH_PORT" \
    "$GOLDEN_RSC" \
    "${SSH_USER}@127.0.0.1:deploy-golden.rsc"

echo "Running /import on CHR..."
IMPORT_OUTPUT=$(sshpass -p "$SSH_PASS" ssh \
    -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null \
    -p "$SSH_PORT" \
    "${SSH_USER}@127.0.0.1" \
    "/import file-name=deploy-golden.rsc" 2>&1)
IMPORT_EXIT=$?

echo "--- /import output ---"
echo "$IMPORT_OUTPUT"
echo "--- end ---"

# RouterOS /import exits 0 on success. Any output containing "failure",
# "error", or "syntax error" indicates a problem even when exit is 0
# (RouterOS sometimes succeeds with warnings).
if [ "$IMPORT_EXIT" -ne 0 ]; then
    echo "ERROR: /import returned non-zero exit: $IMPORT_EXIT" >&2
    kill "$(cat /tmp/chr-qemu.pid)" 2>/dev/null || true
    exit 1
fi

if echo "$IMPORT_OUTPUT" | grep -iE 'failure|syntax error|invalid value|unknown parameter' >/dev/null; then
    echo "ERROR: /import output contains failure indicator" >&2
    kill "$(cat /tmp/chr-qemu.pid)" 2>/dev/null || true
    exit 1
fi

echo "OK: CHR import lint passed."
kill "$(cat /tmp/chr-qemu.pid)" 2>/dev/null || true
