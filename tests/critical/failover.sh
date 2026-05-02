#!/usr/bin/env bash
# tests/critical/failover.sh — F-009 critical-suite test.
#
# Verifies failover-driven ownership reassignment:
#   1. Ledger.Drain reassigns every overlay /32 to a surviving master
#   2. Reassignment bumps each entry's Version (clients detect via stream)
#   3. PreviousOwner is recorded so audit can trace
#   4. Drain aborts when no surviving master can take over (chooser empty)
#   5. Live ownership stream pushes the post-drain snapshot to subscribers
#
# Real-world packet failover (master goes dark → traffic re-routes within
# 10s) is exercised by tests/simulation/F-009-multinode.sh in CR-011.
set -euo pipefail

cd "$(dirname "$0")/../.."

GO=${GO_BIN:-/usr/local/go/bin/go}
if ! command -v "$GO" >/dev/null 2>&1; then
    GO=go
fi

echo "Running failover/drain contract suite..."
$GO test -count=1 -race -run 'TestLedger_OwnedByAndDrain|TestLedger_ReassignAndLookup|TestServer_StreamOwnership_LiveUpdate|TestServer_DecommissionNode' \
    ./pkg/control_plane/...

echo 'PASS — failover.sh: drain + version-vector + live stream propagation verified'
