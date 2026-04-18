#!/usr/bin/env bash
# issue-93-upgrade.sh — smoke-test for the guided rolling upgrade feature
# (local tracker issue #93)
#
# This script validates CLI surface, dry-run output, and compose-migration
# behaviour WITHOUT a running Docker daemon or live AWG nodes.
#
# Requirements:
#   - mesh-ctl binary in PATH or MESH_CTL env var
#   - bash 4+, diff, mktemp
#
# Run:
#   bash -n tests/simulation/issue-93-upgrade.sh   # syntax check
#   bash    tests/simulation/issue-93-upgrade.sh   # full run (requires mesh-ctl in PATH)
#
# Exit codes:
#   0  — all checks passed
#   1  — one or more checks failed (details on stderr)

set -euo pipefail

MESH_CTL="${MESH_CTL:-mesh-ctl}"
PASS=0
FAIL=0

# ─── helpers ────────────────────────────────────────────────────────────────

# Invocations in this script use `… 2>&1 || true` to capture output without
# letting `set -e` abort before the assertion block that follows each call.
# Outcomes are verified by assert_* helpers below, not by exit code.

pass() { echo "  PASS: $*"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $*" >&2; FAIL=$((FAIL + 1)); }

assert_contains() {
    local label="$1" needle="$2" haystack="$3"
    if [[ "$haystack" == *"$needle"* ]]; then
        pass "$label"
    else
        fail "$label — expected to find: $needle"
        echo "    full output:" >&2
        echo "$haystack" | sed 's/^/    /' >&2
    fi
}

assert_not_contains() {
    local label="$1" needle="$2" haystack="$3"
    if [[ "$haystack" != *"$needle"* ]]; then
        pass "$label"
    else
        fail "$label — should not contain: $needle"
    fi
}

assert_exit_nonzero() {
    local label="$1"
    shift
    if ! "$@" >/dev/null 2>&1; then
        pass "$label"
    else
        fail "$label — expected non-zero exit"
    fi
}

# ─── environment setup ───────────────────────────────────────────────────────

TMPDIR_ROOT=$(mktemp -d)
trap 'rm -rf "$TMPDIR_ROOT"' EXIT

CFG_DIR="$TMPDIR_ROOT/mesh-ctl"
TOPO="$TMPDIR_ROOT/mesh-topology.yml"

# Minimal topology for dry-run tests.
cat > "$TOPO" <<'YAML'
masters:
  - name: master-eu-1
    host: 10.0.0.1
    listen_port: 51820
    overlay_ip: 172.16.0.1/24
  - name: master-us-1
    host: 10.0.0.2
    listen_port: 51820
    overlay_ip: 172.16.0.2/24
endpoints:
  - name: ep-eu-1
    region: eu
    overlay_ip: 172.16.1.1/24
    listen_port: 51821
    masters: [master-eu-1]
  - name: ep-us-1
    region: us
    overlay_ip: 172.16.1.2/24
    listen_port: 51822
    masters: [master-us-1]
overlay:
  pool: 172.16.0.0/16
  ranges: []
transport:
  pool: 192.168.100.0/24
  prefix_length: 30
image: ghcr.io/example/awg-mesh-node
YAML

# ─── Section 1: help / CLI surface ──────────────────────────────────────────

echo ""
echo "=== 1. CLI surface checks ==="

HELP_OUT=$("$MESH_CTL" --topology "$TOPO" --config-dir "$CFG_DIR" upgrade --help 2>&1 || true)
assert_contains "upgrade --help shows --dry-run"    "--dry-run"   "$HELP_OUT"
assert_contains "upgrade --help shows --order"      "--order"     "$HELP_OUT"
assert_contains "upgrade --help shows --ssh"        "--ssh"       "$HELP_OUT"

COMPOSE_HELP=$("$MESH_CTL" --topology "$TOPO" --config-dir "$CFG_DIR" upgrade compose --help 2>&1 || true)
assert_contains "upgrade compose --in-place flag"   "--in-place"      "$COMPOSE_HELP"
assert_contains "upgrade compose --from-schema flag" "--from-schema"  "$COMPOSE_HELP"

STATUS_HELP=$("$MESH_CTL" --topology "$TOPO" --config-dir "$CFG_DIR" upgrade status --help 2>&1 || true)
assert_contains "upgrade status --help" "status" "$STATUS_HELP"

# ─── Section 2: upgrade --dry-run ────────────────────────────────────────────

echo ""
echo "=== 2. upgrade --dry-run ==="

DRY_OUT=$("$MESH_CTL" \
    --topology "$TOPO" \
    --config-dir "$CFG_DIR" \
    upgrade v1.10.2 \
    --dry-run \
    2>&1 || true)

assert_contains "plan shows ep-eu-1"     "ep-eu-1"     "$DRY_OUT"
assert_contains "plan shows ep-us-1"     "ep-us-1"     "$DRY_OUT"
assert_contains "plan shows master-eu-1" "master-eu-1" "$DRY_OUT"
assert_contains "plan shows master-us-1" "master-us-1" "$DRY_OUT"
assert_contains "plan shows target version" "v1.10.2"  "$DRY_OUT"
assert_contains "dry-run note" "Dry run"               "$DRY_OUT"
assert_not_contains "dry-run must NOT execute" "Upgrading" "$DRY_OUT"

# ─── Section 3: upgrade --order override ─────────────────────────────────────

echo ""
echo "=== 3. --order override ==="

ORDER_OUT=$("$MESH_CTL" \
    --topology "$TOPO" \
    --config-dir "$CFG_DIR" \
    upgrade v1.10.2 \
    --dry-run \
    --order master-eu-1,ep-eu-1 \
    2>&1 || true)

assert_contains "order: master-eu-1 in plan" "master-eu-1" "$ORDER_OUT"
assert_contains "order: ep-eu-1 in plan"     "ep-eu-1"     "$ORDER_OUT"

# ─── Section 4: upgrade compose — stdout migration ───────────────────────────

echo ""
echo "=== 4. upgrade compose (stdout / schema detection) ==="

# Write a v1.9.0-style compose to a temp file.
V190_FILE="$TMPDIR_ROOT/v190-docker-compose.yml"
cat > "$V190_FILE" <<'EOF'
services:
  awg-mesh-node:
    image: ghcr.io/example/awg-mesh-node:v1.9.0
    network_mode: host
    restart: unless-stopped
    cap_add:
      - NET_ADMIN
    environment:
      - MESH_TOKEN_HASH=$2b$10$examplehash
      - MESH_MODE=endpoint
      - MESH_NAME=ep-eu-1
      - MESH_OVERLAY_IP=172.16.1.1/24
      - MESH_LISTEN_PORT=51821
    volumes:
      - /var/lib/awg-mesh/ep-eu-1:/data
EOF

MIGRATE_OUT=$("$MESH_CTL" \
    --topology "$TOPO" \
    --config-dir "$CFG_DIR" \
    upgrade compose "$V190_FILE" \
    2>&1 || true)

assert_contains "migrated: MESH_CONFIG_DIR present"  "MESH_CONFIG_DIR=/config" "$MIGRATE_OUT"
assert_contains "migrated: MESH_NAME preserved"      "MESH_NAME=ep-eu-1"       "$MIGRATE_OUT"
assert_contains "migrated: MESH_MODE preserved"      "MESH_MODE=endpoint"      "$MIGRATE_OUT"
assert_contains "migrated: volume uses /config"      ":/config"                "$MIGRATE_OUT"

# ─── Section 5: upgrade compose --in-place ───────────────────────────────────

echo ""
echo "=== 5. upgrade compose --in-place ==="

INPLACE_FILE="$TMPDIR_ROOT/inplace-docker-compose.yml"
cp "$V190_FILE" "$INPLACE_FILE"

"$MESH_CTL" \
    --topology "$TOPO" \
    --config-dir "$CFG_DIR" \
    upgrade compose "$INPLACE_FILE" \
    --in-place \
    2>&1 || true

# Original should be backed up.
if [[ -f "${INPLACE_FILE}.bak" ]]; then
    pass "in-place: .bak file created"
else
    fail "in-place: .bak file NOT created"
fi

# Migrated file should have MESH_CONFIG_DIR.
if [[ -f "$INPLACE_FILE" ]]; then
    INPLACE_CONTENT=$(cat "$INPLACE_FILE")
    assert_contains "in-place: MESH_CONFIG_DIR in result"  "MESH_CONFIG_DIR=/config" "$INPLACE_CONTENT"
    assert_contains "in-place: volume /config in result"   ":/config"                "$INPLACE_CONTENT"
else
    fail "in-place: output file NOT found at $INPLACE_FILE"
fi

# ─── Section 6: upgrade compose --from-schema override ───────────────────────

echo ""
echo "=== 6. upgrade compose --from-schema override ==="

SCHEMA_OUT=$("$MESH_CTL" \
    --topology "$TOPO" \
    --config-dir "$CFG_DIR" \
    upgrade compose "$V190_FILE" \
    --from-schema v1.9.0 \
    2>&1 || true)

assert_contains "from-schema: MESH_CONFIG_DIR present" "MESH_CONFIG_DIR=/config" "$SCHEMA_OUT"

# ─── Section 7: upgrade compose — current schema is idempotent ───────────────

echo ""
echo "=== 7. upgrade compose — current schema (idempotent) ==="

CURRENT_FILE="$TMPDIR_ROOT/current-docker-compose.yml"
cat > "$CURRENT_FILE" <<'EOF'
services:
  awg-mesh-node:
    image: ghcr.io/example/awg-mesh-node:v1.10.0
    network_mode: host
    restart: unless-stopped
    cap_add:
      - NET_ADMIN
    environment:
      - MESH_TOKEN_HASH=$2b$10$examplehash
      - MESH_MODE=master
      - MESH_NAME=master-eu-1
      - MESH_OVERLAY_IP=172.16.0.1/24
      - MESH_LISTEN_PORT=51820
      - MESH_CONFIG_DIR=/config
    volumes:
      - /var/lib/awg-mesh/master-eu-1:/config
EOF

IDEM_OUT=$("$MESH_CTL" \
    --topology "$TOPO" \
    --config-dir "$CFG_DIR" \
    upgrade compose "$CURRENT_FILE" \
    2>&1 || true)

assert_contains "idempotent: 'already current schema' message" "already current" "$IDEM_OUT"

# ─── Section 8: upgrade compose — missing file ───────────────────────────────

echo ""
echo "=== 8. error handling ==="

assert_exit_nonzero "missing file returns error" \
    "$MESH_CTL" \
    --topology "$TOPO" \
    --config-dir "$CFG_DIR" \
    upgrade compose /nonexistent/path/compose.yml

# ─── Section 9: upgrade status — no log ──────────────────────────────────────

echo ""
echo "=== 9. upgrade status (no log) ==="

STATUS_OUT=$("$MESH_CTL" \
    --topology "$TOPO" \
    --config-dir "$CFG_DIR" \
    upgrade status \
    2>&1 || true)

assert_contains "no log message" "No upgrade log" "$STATUS_OUT"

# ─── Summary ─────────────────────────────────────────────────────────────────

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Results: PASS=$PASS  FAIL=$FAIL"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

if [[ "$FAIL" -gt 0 ]]; then
    echo "FAILED" >&2
    exit 1
fi

echo "ALL PASSED"
exit 0
