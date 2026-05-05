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

# Inline compile-time conformance check. Do not run the constructors here:
# creating real TUN devices requires NET_ADMIN and would make this critical
# gate environment-dependent. The focused unit tests above cover behavior with
# fake transports; this build check proves the production constructors keep the
# expected Transport-returning shape.
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

cat >"${TMP}/check.go" <<'GOEOF'
package main

import "github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/wg"

func main() {
	var vanillaFactory func(string) (wg.Transport, error) = wg.NewVanillaTransport
	var awgFactory func(string) (wg.Transport, error) = wg.NewAWGTransport
	_, _ = vanillaFactory, awgFactory
	if wg.ProtocolVanilla == wg.ProtocolAmneziaWG {
		panic("transport protocol constants collapsed")
	}
}
GOEOF

# Build inside the repo's module context.
$GO build -o "${TMP}/check" "${TMP}/check.go" >/dev/null
echo "PASS — Transport factories compile to the Transport interface; dual-listener behavior tests green"
