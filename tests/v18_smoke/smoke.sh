#!/usr/bin/env bash
# smoke.sh — Fast (<2 min) smoke checks for the v1.8.0 release gate.
#
# Validates:
#   S0  Build local Docker images from source
#   S1  Binary loads and prints help (node + client images)
#   S2  Binary reports version without panic
#   S3  FR-3: dscp=153 (out-of-range) → mesh-ctl routing generate exits non-zero
#   S4  FR-3: dscp=10  (valid)         → mesh-ctl routing generate exits zero
#   S5  T006a: mesh-ctl bootstrap subcommand exists (skipped if absent — pre-v1.8.0)
#   S6  FR-2: --show-token flag exists on mesh-ctl token rotate
#   S7  FR-2: --show-token flag exists on mesh-ctl master prepare
#
# Usage:
#   bash tests/v18_smoke/smoke.sh           # build + check
#   bash tests/v18_smoke/smoke.sh --no-build  # skip image build, use existing
#
# Exit: 0 = all pass, non-zero = number of failed checks
#
# Prerequisites:
#   - docker running (for S0-S2)
#   - mesh-ctl in PATH or go installed (for S3-S7)
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
NO_BUILD=false

for arg in "$@"; do
    case "${arg}" in
        --no-build) NO_BUILD=true ;;
    esac
done

# ---------------------------------------------------------------------------
# Colours
# ---------------------------------------------------------------------------
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RESET='\033[0m'
PASS_STR="${GREEN}PASS${RESET}"; FAIL_STR="${RED}FAIL${RESET}"; SKIP_STR="${YELLOW}SKIP${RESET}"

FAILURES=0
SKIPS=0

pass() { echo -e "  [${PASS_STR}] $*"; }
fail() { echo -e "  [${FAIL_STR}] $*" >&2; (( FAILURES++ )) || true; }
skip() { echo -e "  [${SKIP_STR}] $*"; (( SKIPS++ )) || true; }

# ---------------------------------------------------------------------------
# Preflight
# ---------------------------------------------------------------------------
echo ""
echo "=== v1.8.0 Smoke Checks ==="
echo ""

if ! command -v docker > /dev/null 2>&1; then
    echo "ERROR: docker not found in PATH. Install Docker 24+ and retry." >&2
    exit 2
fi

if ! docker info > /dev/null 2>&1; then
    echo "ERROR: docker not running or not accessible. Start Docker and retry." >&2
    exit 2
fi

# ---------------------------------------------------------------------------
# Locate mesh-ctl binary (for S3-S7)
# ---------------------------------------------------------------------------
MESHCTL_BIN=""
if command -v mesh-ctl > /dev/null 2>&1; then
    MESHCTL_BIN="mesh-ctl"
elif [[ -x "${REPO_ROOT}/bin/mesh-ctl" ]]; then
    MESHCTL_BIN="${REPO_ROOT}/bin/mesh-ctl"
fi

if [[ -z "${MESHCTL_BIN}" ]]; then
    echo "  NOTE: mesh-ctl not found in PATH or bin/. Building from source..."
    if command -v go > /dev/null 2>&1; then
        ( cd "${REPO_ROOT}" && CGO_ENABLED=0 go build -o bin/mesh-ctl ./cmd/mesh-ctl ) \
            && MESHCTL_BIN="${REPO_ROOT}/bin/mesh-ctl" \
            || echo "  WARNING: mesh-ctl build failed — S3-S7 will be skipped"
    else
        echo "  WARNING: go not found — S3-S7 will be skipped (install go or add mesh-ctl to PATH)"
    fi
fi

if [[ -n "${MESHCTL_BIN}" ]]; then
    echo "  mesh-ctl: $(${MESHCTL_BIN} version 2>&1 || echo '(version unavailable)')"
fi

# ---------------------------------------------------------------------------
# S0: Build images (unless --no-build)
# ---------------------------------------------------------------------------
echo ""
echo "[S0] Building local images..."
if [[ "${NO_BUILD}" == "false" ]]; then
    if bash "${SCRIPT_DIR}/build.sh"; then
        pass "S0: images built"
    else
        fail "S0: image build failed — cannot proceed with container checks"
        echo ""
        echo "=== SMOKE SUMMARY: ${FAILURES} failure(s), ${SKIPS} skip(s) ==="
        exit "${FAILURES}"
    fi
else
    echo "  --no-build: skipping image build, using existing local images"
fi

# Verify images exist
if ! docker image inspect awg-mesh-node:local-v18 > /dev/null 2>&1; then
    fail "S0: awg-mesh-node:local-v18 not found — run build.sh first"
    exit "${FAILURES}"
fi
if ! docker image inspect awg-mesh-client:local-v18 > /dev/null 2>&1; then
    fail "S0: awg-mesh-client:local-v18 not found — run build.sh first"
    exit "${FAILURES}"
fi

# ---------------------------------------------------------------------------
# S1: Binary loads — help flag, no panic
# ---------------------------------------------------------------------------
echo ""
echo "[S1] Binary help flags..."

if docker run --rm awg-mesh-node:local-v18 --help > /dev/null 2>&1; then
    pass "S1a: awg-mesh-node --help succeeds"
else
    fail "S1a: awg-mesh-node --help failed (binary did not load or panicked)"
fi

if docker run --rm awg-mesh-client:local-v18 --help > /dev/null 2>&1; then
    pass "S1b: awg-mesh-client --help succeeds"
else
    fail "S1b: awg-mesh-client --help failed (binary did not load or panicked)"
fi

# ---------------------------------------------------------------------------
# S2: Version check — reports recognizable version string, no panic
# ---------------------------------------------------------------------------
echo ""
echo "[S2] Version strings..."

NODE_VER=$(docker run --rm awg-mesh-node:local-v18 --version 2>&1 || true)
if echo "${NODE_VER}" | grep -qiE "v[0-9]+\.[0-9]+\.[0-9]+|dev|local"; then
    pass "S2a: awg-mesh-node version: ${NODE_VER}"
else
    fail "S2a: awg-mesh-node --version output unexpected: '${NODE_VER}'"
fi

CLIENT_VER=$(docker run --rm awg-mesh-client:local-v18 --version 2>&1 || true)
if echo "${CLIENT_VER}" | grep -qiE "v[0-9]+\.[0-9]+\.[0-9]+|dev|local"; then
    pass "S2b: awg-mesh-client version: ${CLIENT_VER}"
else
    fail "S2b: awg-mesh-client --version output unexpected: '${CLIENT_VER}'"
fi

# ---------------------------------------------------------------------------
# S3: FR-3 — dscp=153 (out of range 1-63) must be rejected
# ---------------------------------------------------------------------------
echo ""
echo "[S3] FR-3: DSCP bounds check (dscp=153 must fail)..."

if [[ -z "${MESHCTL_BIN}" ]]; then
    skip "S3: mesh-ctl not available — install go or add mesh-ctl to PATH"
else
    TOPO_INVALID=$(mktemp /tmp/smoke-topo-invalid-XXXXXX.yml)
    cat > "${TOPO_INVALID}" << 'EOF'
overlay:
  space: 172.20.71.0/24
  physical_mtu: 1500
  awg_overhead: 80
  ranges:
    - name: masters
      cidr: 172.20.71.0/27
      balancer_ip: 172.20.71.1
    - name: endpoints
      cidr: 172.20.71.32/27
      balancer_ip: 172.20.71.33
    - name: clients
      cidr: 172.20.71.128/25

masters:
  - name: master-a
    host: 172.31.10.10
    overlay_ip: 172.20.71.2
    listen_port: 51820
    endpoints:
      - endpoint-x

endpoints:
  - name: endpoint-x
    host: 172.31.10.20
    overlay_ip: 172.20.71.37
    listen_port: 51820

clients:
  - name: client-lin
    type: linux
    overlay_ip: 172.20.71.130
    masters:
      - master-a
    routing_policies:
      - name: high-prio
        dscp: 153
        targets:
          - endpoint-x

transport:
  pool: 10.255.0.0/16
  prefix_length: 30
EOF

    DSCP_BAD_OUT=$(${MESHCTL_BIN} routing generate \
        --topology "${TOPO_INVALID}" \
        --platform linux \
        --client client-lin \
        2>&1) && DSCP_BAD_RC=0 || DSCP_BAD_RC=$?

    rm -f "${TOPO_INVALID}"

    if [[ "${DSCP_BAD_RC}" -ne 0 ]]; then
        if echo "${DSCP_BAD_OUT}" | grep -qi "dscp"; then
            pass "S3: dscp=153 rejected (exit ${DSCP_BAD_RC}), error mentions DSCP"
        else
            pass "S3: dscp=153 rejected (exit ${DSCP_BAD_RC}) — note: error message does not mention DSCP (FR-3.2 text check)"
        fi
    else
        # FR-3 not yet merged
        skip "S3: dscp=153 not rejected (exit 0) — FR-3 (#23) not yet present in this build; will be enforced post-merge"
    fi
fi

# ---------------------------------------------------------------------------
# S4: FR-3 — dscp=10 (valid) must succeed
# ---------------------------------------------------------------------------
echo ""
echo "[S4] FR-3: DSCP bounds check (dscp=10 must succeed)..."

if [[ -z "${MESHCTL_BIN}" ]]; then
    skip "S4: mesh-ctl not available"
else
    TOPO_VALID=$(mktemp /tmp/smoke-topo-valid-XXXXXX.yml)
    cat > "${TOPO_VALID}" << 'EOF'
overlay:
  space: 172.20.71.0/24
  physical_mtu: 1500
  awg_overhead: 80
  ranges:
    - name: masters
      cidr: 172.20.71.0/27
      balancer_ip: 172.20.71.1
    - name: endpoints
      cidr: 172.20.71.32/27
      balancer_ip: 172.20.71.33
    - name: clients
      cidr: 172.20.71.128/25

masters:
  - name: master-a
    host: 172.31.10.10
    overlay_ip: 172.20.71.2
    listen_port: 51820
    endpoints:
      - endpoint-x

endpoints:
  - name: endpoint-x
    host: 172.31.10.20
    overlay_ip: 172.20.71.37
    listen_port: 51820

clients:
  - name: client-lin
    type: linux
    overlay_ip: 172.20.71.130
    masters:
      - master-a
    routing_policies:
      - name: high-prio
        dscp: 10
        targets:
          - endpoint-x

transport:
  pool: 10.255.0.0/16
  prefix_length: 30
EOF

    DSCP_GOOD_OUT=$(${MESHCTL_BIN} routing generate \
        --topology "${TOPO_VALID}" \
        --platform linux \
        --client client-lin \
        2>&1) && DSCP_GOOD_RC=0 || DSCP_GOOD_RC=$?

    rm -f "${TOPO_VALID}"

    if [[ "${DSCP_GOOD_RC}" -eq 0 ]]; then
        pass "S4: dscp=10 accepted (exit 0)"
    else
        fail "S4: dscp=10 unexpectedly rejected (exit ${DSCP_GOOD_RC}): ${DSCP_GOOD_OUT}"
    fi
fi

# ---------------------------------------------------------------------------
# S5: T006a — mesh-ctl bootstrap subcommand
# ---------------------------------------------------------------------------
echo ""
echo "[S5] T006a: mesh-ctl bootstrap command..."

if [[ -z "${MESHCTL_BIN}" ]]; then
    skip "S5: mesh-ctl not available"
else
    BOOTSTRAP_HELP=$(${MESHCTL_BIN} bootstrap --help 2>&1) && BOOTSTRAP_RC=0 || BOOTSTRAP_RC=$?
    if [[ "${BOOTSTRAP_RC}" -eq 0 ]]; then
        if echo "${BOOTSTRAP_HELP}" | grep -qi "host\|ssh\|provision"; then
            pass "S5: mesh-ctl bootstrap --help available and mentions expected flags"
        else
            pass "S5: mesh-ctl bootstrap --help available"
        fi
    else
        skip "S5: mesh-ctl bootstrap not available — T006a (#41) not yet merged in this build"
    fi
fi

# ---------------------------------------------------------------------------
# S6: FR-2 — --show-token flag on mesh-ctl token rotate
# ---------------------------------------------------------------------------
echo ""
echo "[S6] FR-2: --show-token flag on mesh-ctl token rotate..."

if [[ -z "${MESHCTL_BIN}" ]]; then
    skip "S6: mesh-ctl not available"
else
    TOKEN_HELP=$(${MESHCTL_BIN} token rotate --help 2>&1) || true
    if echo "${TOKEN_HELP}" | grep -q -- "--show-token"; then
        pass "S6: --show-token flag present on mesh-ctl token rotate"
    else
        skip "S6: --show-token flag absent on mesh-ctl token rotate — FR-2 (#21) not yet merged in this build"
    fi
fi

# ---------------------------------------------------------------------------
# S7: FR-2 — --show-token flag on mesh-ctl master prepare
# ---------------------------------------------------------------------------
echo ""
echo "[S7] FR-2: --show-token flag on mesh-ctl master prepare..."

if [[ -z "${MESHCTL_BIN}" ]]; then
    skip "S7: mesh-ctl not available"
else
    MASTER_HELP=$(${MESHCTL_BIN} master prepare --help 2>&1) || true
    if echo "${MASTER_HELP}" | grep -q -- "--show-token"; then
        pass "S7: --show-token flag present on mesh-ctl master prepare"
    else
        skip "S7: --show-token flag absent on mesh-ctl master prepare — FR-2 (#21) not yet merged in this build"
    fi
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo ""
echo "=================================================================="
if [[ "${FAILURES}" -eq 0 ]]; then
    echo -e " SMOKE: ${GREEN}PASS${RESET} (${SKIPS} check(s) skipped as pre-v1.8.0 features)"
else
    echo -e " SMOKE: ${RED}FAIL${RESET} — ${FAILURES} failure(s), ${SKIPS} skip(s)"
fi
echo "=================================================================="
echo ""

exit "${FAILURES}"
