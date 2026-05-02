#!/usr/bin/env bash
# tests/critical/federated-masters.sh — F-009 critical-suite test.
#
# Verifies multi-master registration + region-aware successor selection:
#   1. Multiple masters in different regions register cleanly
#   2. MastersInRegion returns only matching nodes
#   3. Decommission of a master in region X picks a successor from region X
#      when one exists, and falls back to mesh-wide otherwise
#   4. Cross-region masters do not collide on overlay-IP namespace
#
# This contract underpins the federated mesh topology (every master peers
# with every other master via mesh-internal AWG, but ownership stays scoped
# to the region for latency).
set -euo pipefail

cd "$(dirname "$0")/../.."

GO=${GO_BIN:-/usr/local/go/bin/go}
if ! command -v "$GO" >/dev/null 2>&1; then
    GO=go
fi

echo "Running federated-masters contract suite..."
$GO test -count=1 -race -run 'TestRegistry_MastersInRegion|TestRegistry_OverlayCollision|TestServer_DecommissionNode' \
    ./pkg/control_plane/...

echo 'PASS — federated-masters.sh: region-scoped registry + cross-region overlay isolation verified'
