#!/usr/bin/env bash
# tests/critical/pluggable-transport.sh — F-009 CR-001 deliverable test.
#
# Verifies the v2.0 Transport interface (pkg/wg/transport.go) is
# implementable: both vanilla-WG and AmneziaWG implementations conform.
# Daemon-side wiring lands in CR-004; CR-001 only proves the interface
# contract.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

if ! command -v go >/dev/null 2>&1; then
    echo "SKIP — go toolchain not available; run inside Docker"
    exit 0
fi

# Inline interface-conformance check. Compile-only — no runtime side effects.
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
go run "${TMP}/check.go" >/dev/null
echo "PASS — Transport interface implemented by vanilla-WG and AmneziaWG, Protocol/Name accessors work"
