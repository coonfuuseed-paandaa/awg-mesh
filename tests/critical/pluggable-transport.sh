#!/usr/bin/env bash
# tests/critical/pluggable-transport.sh — F-009 CR-001 deliverable test.
#
# Verifies the v2.0 Transport interface and CR-004 master dual-listener
# contract: both vanilla-WG and AmneziaWG implementations conform, and the
# master bridge configures separate protocol listeners.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

GO=${GO_BIN:-/usr/local/go/bin/go}
if ! command -v "$GO" >/dev/null 2>&1; then
    GO=go
fi
if ! command -v "$GO" >/dev/null 2>&1; then
    echo "SKIP — go toolchain not available; run inside Docker"
    exit 0
fi

# Focused CR-004 tests use fake transports, so they do not require privileged
# kernel TUN creation.
$GO test -count=1 -run 'TestDualListener' ./pkg/wg/... >/dev/null

# Inline interface-conformance check. Runtime construction may use the real
# UAPI-backed implementation; if the current environment lacks kernel support,
# the focused unit tests above remain the authoritative CR-004 gate.
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

cat >"${TMP}/check.go" <<'GOEOF'
package main

import "github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"

func main() {
	v, err := wg.NewVanillaTransport("wg-clients")
	if err != nil {
		panic(err)
	}
	a, err := wg.NewAWGTransport("wg-mesh")
	if err != nil {
		panic(err)
	}
	if v.Protocol() != wg.ProtocolVanilla {
		panic("vanilla transport reports wrong protocol")
	}
	if a.Protocol() != wg.ProtocolAmneziaWG {
		panic("awg transport reports wrong protocol")
	}
	if v.Name() != "wg-clients" || a.Name() != "wg-mesh" {
		panic("transport name accessor broken")
	}
}
GOEOF

# Build inside the repo's module context.
$GO run "${TMP}/check.go" >/dev/null
echo "PASS — Transport interface implemented by vanilla-WG and AmneziaWG, Protocol/Name accessors work"
