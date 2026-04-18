#!/usr/bin/env bash
# tests/simulation/issue-100-scp-compose.sh
# Smoke gate for local tracker issue #100: SFTP compose upload in
# mesh-ctl upgrade --ssh.
#
# Current scope:
#   1. Run the pkg/upgrade unit tests that validate path construction,
#      SSHUpload hook wiring, rollback symmetry, and fallback behaviour.
#   2. If Docker is available, boot an Alpine-SSHD container as a liveness
#      check only — the full upload→deploy→verify→rollback harness needs a
#      live AWG node gRPC server for the wait_ready phase and is deferred
#      to the v1.10.5 harness (tracked via local tracker #110).
#
# Exit codes:
#   0  — all checks passed
#   1  — unit tests failed (Docker phase is NOT reached)
#   77 — unit tests passed but required tools for Docker phase missing

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"

PASS=0
FAIL=0

pass() { echo "  PASS: $*"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $*" >&2; FAIL=$((FAIL + 1)); }

# ─── unit test gate ──────────────────────────────────────────────────────────
# Always run the unit tests first; they validate the path logic and hook wiring
# without requiring Docker or SSH infrastructure.

echo "=== issue-100-scp-compose: unit tests ==="
cd "$ROOT_DIR"

GO_BIN="${GO_BIN:-$(command -v go || true)}"
if [[ -z "$GO_BIN" ]]; then
    fail "go binary not found in PATH (set GO_BIN or install Go)"
    echo ""
    echo "=== issue-100-scp-compose results: ${PASS} passed, ${FAIL} failed ==="
    exit 1
fi

if ! UNIT_OUT=$("$GO_BIN" test ./pkg/upgrade/... -count=1 \
    -run 'TestRemoteComposePath|TestSSHDeploy|TestRemoteBackupComposePath|TestRollbackNode_UploadsBakCompose' \
    -v 2>&1); then
    fail "unit tests failed (exit non-zero)"
    echo "$UNIT_OUT" >&2
elif echo "$UNIT_OUT" | grep -q "^--- FAIL"; then
    fail "unit tests reported FAIL"
    echo "$UNIT_OUT" >&2
else
    pass "unit tests: remoteComposePath, sshDeploy wiring, rollback upload"
fi

# Hard gate — do NOT proceed to Docker phase if unit tests failed. The Docker
# phase is a no-op liveness check today; running it after a broken unit suite
# just adds noise without any coverage gain.
if [[ $FAIL -gt 0 ]]; then
    echo ""
    echo "=== issue-100-scp-compose results: ${PASS} passed, ${FAIL} failed ==="
    exit 1
fi

# ─── Docker SSH-server liveness check (no SFTP upload performed yet) ─────────
# The full upload->deploy->verify->rollback harness is tracked for v1.10.5
# (needs a live AWG node gRPC server for the wait_ready phase; see engram
# #110). For now we only verify the sshd+sftp subsystem boots so operators
# have a fast signal that prerequisites are in place.

DOCKER="${DOCKER:-docker}"

if ! command -v "$DOCKER" >/dev/null 2>&1; then
    echo "Docker not available — skipping sshd liveness (exit 77)"
    exit 77
fi

if ! "$DOCKER" info >/dev/null 2>&1; then
    echo "Docker daemon not reachable — skipping sshd liveness (exit 77)"
    exit 77
fi

echo ""
echo "=== issue-100-scp-compose: Docker sshd liveness ==="
echo "mesh-ctl: $MESH_CTL"

# Unique test run ID for container and network names to avoid conflicts.
RUN_ID="issue100-$$"
SSH_CONTAINER="${RUN_ID}-remote"
SSH_PORT=2222
REMOTE_COMPOSE_DIR="/tmp/awg-compose-test"

cleanup() {
    "$DOCKER" rm -f "$SSH_CONTAINER" 2>/dev/null || true
}
trap cleanup EXIT

# Launch a minimal SSH server container (Alpine with OpenSSH).
# Uses the linsir/alpine-sshd image which exposes sshd on port 2222 with a
# built-in SFTP subsystem.
echo "  Starting SSH server container..."
"$DOCKER" run -d \
    --name "$SSH_CONTAINER" \
    -p "${SSH_PORT}:2222" \
    -e PUID=1000 \
    -e PGID=1000 \
    -e USER_NAME=testuser \
    -e USER_PASSWORD=testpass \
    -e PASSWORD_ACCESS=true \
    -e SUDO_ACCESS=true \
    linsir/alpine-sshd:latest \
    >/dev/null 2>&1 || {
    echo "  Could not pull SSH server image — skipping Docker integration test (exit 77)"
    if [[ $FAIL -gt 0 ]]; then exit 1; fi
    exit 77
}

# Wait for SSH server to start.
echo "  Waiting for SSH server to be ready..."
READY=0
for _ in $(seq 1 15); do
    if "$DOCKER" exec "$SSH_CONTAINER" true 2>/dev/null; then
        READY=1
        break
    fi
    sleep 1
done

if [[ $READY -eq 0 ]]; then
    fail "SSH container did not start in time"
    echo ""
    echo "=== Results: ${PASS} passed, ${FAIL} failed ==="
    exit 1
fi

# Check if SFTP works in the container.
if ! "$DOCKER" exec "$SSH_CONTAINER" which sftp-server 2>/dev/null && \
   ! "$DOCKER" exec "$SSH_CONTAINER" which /usr/lib/openssh/sftp-server 2>/dev/null; then
    echo "  SFTP subsystem not available in container — skipping Docker integration test"
    if [[ $FAIL -gt 0 ]]; then exit 1; fi
    exit 77
fi

# SSH container boots OK but we do NOT yet exercise the SFTP upload path
# here — that requires wiring mesh-ctl against a live AWG node gRPC server
# for the wait_ready phase of `mesh-ctl upgrade --ssh`. Adding that
# harness is a separate piece of work (tracked for the v1.10.5 bucket
# alongside the issue-92-rotation.sh auth-refresh at local tracker #110).
#
# Rather than calling pass() here and giving false confidence, emit an
# explicit SKIP so operators see what was NOT verified. Unit tests above
# already cover path construction, hook wiring, and rollback symmetry.

echo "  SKIP: SFTP upload end-to-end verification deferred to v1.10.5 harness"
echo "  Unit tests above cover: remoteComposePath / remoteBackupComposePath /"
echo "                          SSHDeploy_UsesSSHUploadWhenConfigured /"
echo "                          SSHDeploy_FallsBackToSSHDeployWhenSSHUploadNil /"
echo "                          RollbackNode_UploadsBakCompose"

# ─── summary ─────────────────────────────────────────────────────────────────
echo ""
echo "=== issue-100-scp-compose results: ${PASS} passed, ${FAIL} failed ==="

if [[ $FAIL -gt 0 ]]; then
    exit 1
fi
exit 0
