#!/usr/bin/env bash
# tests/critical/hub-spoke.sh — F-009 critical-suite test.
#
# Verifies the hub-spoke control-plane contract from CR-002:
#   1. control-plane daemon binds + accepts RegisterNode
#   2. registered nodes appear in registry with correct overlay /32 mapping
#   3. ownership ledger streams to subscribers (clientd analog)
#   4. peer-list distribution filters by subject_node
#   5. unregistered nodes are rejected from heartbeat (NotFound)
#
# Driven by the Go test harness in pkg/control_plane/server_test.go +
# daemon_test.go. The full multi-container hub-spoke topology — actual
# WireGuard interfaces, kernel routing, real packets — lands in
# tests/simulation/F-009-multinode.sh in CR-011.
set -euo pipefail

cd "$(dirname "$0")/../.."

GO=${GO_BIN:-/usr/local/go/bin/go}
if ! command -v "$GO" >/dev/null 2>&1; then
    GO=go
fi

echo "Running control-plane hub-spoke contract suite..."
$GO test -count=1 -race -run 'TestServer_RegisterNode|TestServer_Heartbeat|TestServer_StreamPeerList|TestServer_StreamOwnership|TestDaemon_Lifecycle' \
    ./pkg/control_plane/...

echo 'PASS — hub-spoke.sh: control-plane contract verified (register / heartbeat / streams / lifecycle)'
