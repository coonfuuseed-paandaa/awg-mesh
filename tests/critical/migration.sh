#!/usr/bin/env bash
# tests/critical/migration.sh - v1.x to v2.0 topology migration gate.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"
source "${REPO_ROOT}/tests/critical/lib.bash"

GO=${GO_BIN:-/usr/local/go/bin/go}
if ! command -v "$GO" >/dev/null 2>&1; then
    GO=go
fi
if ! command -v "$GO" >/dev/null 2>&1; then
    echo "FAIL - go toolchain not available; run inside Docker" >&2
    exit 1
fi

test_output="$(mktemp)"
tmp_dir="$(mktemp -d)"
trap 'rm -f "$test_output"; rm -rf "$tmp_dir"' EXIT

critical_run_go_tests_required "$test_output" \
    'TestMigrateV1ToV2|TestRunMigrate' \
    ./pkg/topology/... ./cmd/mesh-ctl/cmd -- \
    TestMigrateV1ToV2_ConvertsFixture \
    TestMigrateV1ToV2_RejectsAlreadyV2 \
    TestMigrateV1ToV2_MapsMasterExitToMixedRoles \
    TestRunMigrateCommandWritesFileAndProtectsOverwrite \
    TestRunMigrateCommandOutputsJSONWithoutWritingFile \
    TestRunMigrateCommandRejectsAlreadyV2

migrated_yaml="${tmp_dir}/v2-topology.yml"
human_out="${tmp_dir}/migrate-human.out"
json_out="${tmp_dir}/migrate-json.out"

"$GO" run ./cmd/mesh-ctl migrate \
    --from pkg/topology/testdata/v1x-topology.yml \
    --to "$migrated_yaml" >"$human_out"

grep -Fq "migration written" "$human_out"
grep -Fq "schema_version: 2" "$migrated_yaml"
if grep -Eq '^(transport|masters|endpoints|clients):' "$migrated_yaml"; then
    echo "FAIL - migrated topology leaked legacy root keys" >&2
    cat "$migrated_yaml" >&2
    exit 1
fi

"$GO" run ./cmd/mesh-ctl topology validate --topology "$migrated_yaml" >/dev/null
"$GO" run ./cmd/mesh-ctl migrate \
    --from pkg/topology/testdata/v1x-topology.yml \
    --output json >"$json_out"
grep -Fq '"schema_version": 2' "$json_out"
grep -Fq '"nodes"' "$json_out"

echo "PASS - migration.sh: v1.x topology converts to validated schema v2 YAML and JSON"
