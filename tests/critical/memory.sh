#!/usr/bin/env bash
# tests/critical/memory.sh - copy-on-write and state isolation smoke.
#
# This gate catches the class of memory/regression bugs where shared maps or
# slices escape and mutate across role registries, clientd state, or ledgers.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

GO=${GO_BIN:-/usr/local/go/bin/go}
if ! command -v "$GO" >/dev/null 2>&1; then
    GO=go
fi
if ! command -v "$GO" >/dev/null 2>&1; then
    echo "FAIL - go toolchain not available; run inside Docker" >&2
    exit 1
fi

"$GO" test -count=1 -run 'TestRegistryReplaceIsCopyOnWrite|TestRegistryClonesMutableFields|TestStateUpdateImmutableAndConfiguratorPayloadsDiffer|TestRegistry_ClonesMutableFields|TestLedger_ListenerFires' \
    ./pkg/balancer/... ./pkg/ingress/... ./pkg/clientd/... ./pkg/control_plane/... >/dev/null

echo "PASS - memory.sh: copy-on-write and state isolation contracts verified"
