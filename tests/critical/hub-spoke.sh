#!/usr/bin/env bash
# tests/critical/hub-spoke.sh — F-009 critical-suite test.
#
# Verifies the master-owned coordination primitive contract from CR-002:
#   1. compatibility control-plane wrapper binds + accepts RegisterNode
#   2. registered nodes appear in registry with correct overlay /32 mapping
#   3. ownership ledger streams to subscribers (clientd analog)
#   4. peer-list distribution filters by subject_node
#   5. unregistered nodes are rejected from heartbeat (NotFound)
#
# Driven by the Go test harness in pkg/control_plane/server_test.go +
# daemon_test.go. The daemon tests are retained compatibility/internal coverage;
# the customer path is responsible-master coordination. The full multi-container
# topology — actual WireGuard interfaces, kernel routing, real packets — lands in
# tests/simulation/F-009-multinode.sh in CR-011.
set -euo pipefail

cd "$(dirname "$0")/../.."

GO=${GO_BIN:-/usr/local/go/bin/go}
if ! command -v "$GO" >/dev/null 2>&1; then
    GO=go
fi

echo "Running master-owned coordination primitive suite..."
$GO test -count=1 -race -run 'TestServer_RegisterNode|TestServer_Heartbeat|TestServer_StreamPeerList|TestServer_StreamOwnership|TestDaemon_Lifecycle' \
    ./pkg/control_plane/...

echo 'PASS — hub-spoke.sh: master-owned coordination primitives verified (register / heartbeat / streams / compatibility lifecycle)'
