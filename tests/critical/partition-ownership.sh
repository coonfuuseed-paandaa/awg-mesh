#!/usr/bin/env bash
# tests/critical/partition-ownership.sh — F-009 critical-suite test.
#
# Verifies CR-002 ledger HA-2 partitioned ownership: when a master is drained,
# every overlay /32 it owned is reassigned to a surviving master, and the
# registry no longer lists the drained master. Driven through the in-process
# gRPC interface via Go test harness (no external Docker fleet needed for
# this layer — multi-container reachability is covered by hub-spoke.sh +
# failover.sh).
set -euo pipefail

cd "$(dirname "$0")/../.."

GO=${GO_BIN:-/usr/local/go/bin/go}
if ! command -v "$GO" >/dev/null 2>&1; then
    GO=go
fi

# Run the dedicated test suite under pkg/control_plane that covers
# Drain + DecommissionNode + MastersInRegion successor selection.
echo "Running ledger partition-ownership test suite..."
$GO test -count=1 -race -run 'TestLedger_OwnedByAndDrain|TestServer_DecommissionNode|TestRegistry_MastersInRegion' \
    ./pkg/control_plane/...

echo 'PASS — partition-ownership.sh: HA-2 drain + reassign + registry-cleanup verified'
