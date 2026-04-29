#!/usr/bin/env bash
# CHR import-lint: boots ephemeral RouterOS CHR via QEMU, SCPs the golden
# .rsc fixture, runs /import, fails on non-zero RouterOS import exit.
#
# Inputs (env):
#   CHR_IMAGE     path to CHR .img (default: chr.img)
#   GOLDEN_RSC    path to .rsc fixture (default: pkg/mikrotik/testdata/deploy-golden.rsc)
#   CHR_VERSION   RouterOS version baked into CHR_IMAGE (informational, used in logs)
#
# Boot model: usermode networking + hostfwd for SSH (port 2222 on host).
# First-boot CHR creds: admin with empty password. RouterOS forces an
# interactive password-change dialog inside the SSH session that plain
# sshpass cannot satisfy (sshpass only fills the initial SSH password
# prompt — see man sshpass(1)). The expect block below drives the
# interactive prompt by:
#   1. Logging in with empty password
#   2. Answering "y" to the EULA prompt (presented on first boot of
#      RouterOS 7.21+)
#   3. Supplying SSH_PASS at "new password" / "repeat new password"
#   4. Verifying the prompt by running :put "chr-ready"
#
# Subsequent SCP / /import calls then use SSH_PASS via plain sshpass.
#
# REQ-2 Phase 2 of F-001 mikrotik-generator-fixes (PR #90).
set -euo pipefail

CHR_IMAGE="${CHR_IMAGE:-chr.img}"
GOLDEN_RSC="${GOLDEN_RSC:-pkg/mikrotik/testdata/deploy-golden.rsc}"
CHR_VERSION="${CHR_VERSION:-unknown}"
SSH_PORT=2222
SSH_USER="admin"
SSH_PASS="lintpass"

echo "chr-import-lint: CHR_VERSION=${CHR_VERSION} image=${CHR_IMAGE}"

if [ ! -f "$CHR_IMAGE" ]; then
    echo "ERROR: CHR image not found at $CHR_IMAGE" >&2
    exit 1
fi

if [ ! -f "$GOLDEN_RSC" ]; then
    echo "ERROR: golden fixture not found at $GOLDEN_RSC" >&2
    exit 1
fi

# Pre-flight dependency check — fail fast with a useful message rather
# than letting downstream commands print cryptic errors.
for cmd in qemu-system-x86_64 nc sshpass scp ssh expect; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
        echo "ERROR: required dependency '$cmd' not in PATH" >&2
        exit 2
    fi
done

echo "Booting CHR via QEMU (SSH forwarded to host port ${SSH_PORT})..."
# -display none + -daemonize: -nographic is incompatible with -daemonize.
# Serial console output is discarded (acceptable for CI lint — we drive
# the box via SSH only). PID is captured in /tmp/chr-qemu.pid for cleanup.
qemu-system-x86_64 \
    -drive "file=${CHR_IMAGE},format=raw,if=virtio" \
    -netdev "user,id=net0,hostfwd=tcp::${SSH_PORT}-:22" \
    -device virtio-net-pci,netdev=net0 \
    -m 256 \
    -display none \
    -daemonize \
    -pidfile /tmp/chr-qemu.pid

cleanup() {
    if [ -f /tmp/chr-qemu.pid ]; then
        kill "$(cat /tmp/chr-qemu.pid)" 2>/dev/null || true
        rm -f /tmp/chr-qemu.pid
    fi
}
trap cleanup EXIT

# Wait for SSH port to open (CHR boots in ~30 s).
echo "Waiting for CHR SSH port..."
for i in $(seq 1 120); do
    if nc -z 127.0.0.1 "$SSH_PORT" 2>/dev/null; then
        echo "SSH port open after ${i} attempts"
        break
    fi
    if [ "$i" -eq 120 ]; then
        echo "ERROR: CHR SSH port did not open within 120 seconds" >&2
        exit 1
    fi
    sleep 1
done

# Port-open != sshd-ready. CHR opens TCP listener long before the SSH
# daemon is actually answering — observed: port-open at 1s into boot,
# but sshd does not emit "SSH-2.0" banner until cloud-init seeds the
# admin password (~60-120 s into boot on QEMU usermode networking).
# Banner-detect via /dev/tcp + head -c 32 fails because the half-open
# connection blocks indefinitely waiting for the server to write — and
# the server isn't writing yet.
#
# Empirical: 180 s after port-open is sufficient on RouterOS 7.21+ for
# the auth subsystem to be live. This is a lot of wall-clock budget but
# CHR cold-boot is genuinely slow on a non-KVM host runner.
echo "Sleeping 180s for CHR boot + cloud-init + sshd auth subsystem..."
sleep 180

# Now probe the SSH banner — by this point sshd MUST be ready. If the
# banner is still missing the box is broken and we should fail fast
# rather than hand off to a 90 s expect timeout.
echo "Verifying SSH banner..."
for i in $(seq 1 30); do
    banner=$(timeout 5 bash -c "exec 3<>/dev/tcp/127.0.0.1/${SSH_PORT}; head -c 32 <&3" 2>/dev/null || true)
    if echo "$banner" | grep -q "SSH-2.0"; then
        echo "SSH banner received after ${i} attempts: ${banner}"
        break
    fi
    if [ "$i" -eq 30 ]; then
        echo "ERROR: SSH banner not received within 150 seconds after sleep" >&2
        exit 1
    fi
    sleep 5
done

# Drive RouterOS first-boot password-change interactively via expect.
# This is the canonical fix for the limitation that sshpass only answers
# the initial SSH password prompt; once the SSH session is established
# RouterOS launches an in-session dialog that sshpass cannot satisfy.
#
# Idempotent: if the password is already "$SSH_PASS" (e.g. CHR was
# pre-baked or this script ran twice without recreating the image), the
# expect script logs in directly and returns 0 without touching the
# password.
echo "Establishing CHR admin password via expect..."
EXPECT_LOG=$(mktemp)
set +e
expect <<EXPECT_EOF >"$EXPECT_LOG" 2>&1
set timeout 90
log_user 1
# First, try the password we expect to set (idempotent re-run path).
spawn ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o PreferredAuthentications=password -o PubkeyAuthentication=no \
    -p ${SSH_PORT} ${SSH_USER}@127.0.0.1
expect {
    -re {(yes/no.*)}           { send "yes\r"; exp_continue }
    -re {[Pp]assword:}         { send "${SSH_PASS}\r" }
    timeout                    { send_user "TIMEOUT-AT-LOGIN\n"; exit 11 }
}
expect {
    -re {Permission denied}    {
        send_user "PASSWORD-NOT-YET-SET\n"
        exit 12
    }
    -re {EULA.*\(y/n\)}        { send "y\r"; exp_continue }
    -re {new password>}        { send "${SSH_PASS}\r"; exp_continue }
    -re {repeat new password>} { send "${SSH_PASS}\r"; exp_continue }
    -re {\[admin@.*\] >}       { send ":put \"chr-ready\"\r"; exp_continue }
    -re {chr-ready}            { send "/quit\r"; exp_continue }
    eof                        { exit 0 }
    timeout                    { send_user "TIMEOUT-AT-PROMPT\n"; exit 13 }
}
EXPECT_EOF
expect_rc=$?
set -e

if [ "$expect_rc" -eq 12 ]; then
    # First-boot path: log in with empty password, answer the
    # password-change dialog.
    echo "First-boot CHR detected — performing password seeding..."
    expect <<EXPECT_EOF >>"$EXPECT_LOG" 2>&1
set timeout 90
log_user 1
spawn ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o PreferredAuthentications=password -o PubkeyAuthentication=no \
    -p ${SSH_PORT} ${SSH_USER}@127.0.0.1
expect {
    -re {(yes/no.*)}           { send "yes\r"; exp_continue }
    -re {[Pp]assword:}         { send "\r" }
    timeout                    { send_user "TIMEOUT-FIRST-BOOT-LOGIN\n"; exit 21 }
}
expect {
    -re {EULA.*\(y/n\)}        { send "y\r"; exp_continue }
    -re {new password>}        { send "${SSH_PASS}\r"; exp_continue }
    -re {repeat new password>} { send "${SSH_PASS}\r"; exp_continue }
    -re {\[admin@.*\] >}       { send ":put \"chr-ready\"\r"; exp_continue }
    -re {chr-ready}            { send "/quit\r"; exp_continue }
    eof                        { exit 0 }
    timeout                    { send_user "TIMEOUT-FIRST-BOOT-DIALOG\n"; exit 22 }
}
EXPECT_EOF
    expect_rc=$?
fi

if [ "$expect_rc" -ne 0 ]; then
    echo "ERROR: failed to establish CHR admin password (expect rc=${expect_rc})" >&2
    echo "--- expect transcript ---" >&2
    cat "$EXPECT_LOG" >&2
    echo "--- end transcript ---" >&2
    rm -f "$EXPECT_LOG"
    exit 1
fi
rm -f "$EXPECT_LOG"

echo "Uploading golden fixture..."
sshpass -p "$SSH_PASS" scp \
    -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null \
    -P "$SSH_PORT" \
    "$GOLDEN_RSC" \
    "${SSH_USER}@127.0.0.1:deploy-golden.rsc"

echo "Running /import on CHR..."
# Capture exit + output without races. Plain `var=$(cmd); rc=$?` is
# unreliable under `set -e`: on a non-zero exit the assignment line
# itself triggers the trap, never reaching `rc=$?`. The `||` form
# captures the rc atomically with the assignment.
set +e
IMPORT_OUTPUT=$(sshpass -p "$SSH_PASS" ssh \
    -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null \
    -p "$SSH_PORT" \
    "${SSH_USER}@127.0.0.1" \
    "/import file-name=deploy-golden.rsc" 2>&1)
IMPORT_EXIT=$?
set -e

echo "--- /import output ---"
echo "$IMPORT_OUTPUT"
echo "--- end ---"

# RouterOS /import exits 0 on success. Any output containing "failure",
# "error", or "syntax error" indicates a problem even when exit is 0
# (RouterOS sometimes succeeds with warnings).
if [ "$IMPORT_EXIT" -ne 0 ]; then
    echo "ERROR: /import returned non-zero exit: $IMPORT_EXIT" >&2
    exit 1
fi

if echo "$IMPORT_OUTPUT" | grep -iE 'failure|syntax error|invalid value|unknown parameter' >/dev/null; then
    echo "ERROR: /import output contains failure indicator" >&2
    exit 1
fi

echo "OK: CHR import lint passed (CHR_VERSION=${CHR_VERSION})."
