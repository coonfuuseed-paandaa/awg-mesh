#!/usr/bin/env bash
# tests/critical/mikrotik-v2.sh - MikroTik RouterOS container gate.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

go test -count=1 ./pkg/mikrotik ./pkg/mikrotik/v2
go test -count=1 ./cmd/mesh-ctl/cmd -run 'TestRunNodePrepareCommandWritesMikrotikContainerRouterOSScript'
go test -count=1 ./cmd/awg-mesh-node -run 'TestRunMasterDryRunAcceptsClientPrivateKeyFile|TestRunMasterDryRunRejectsInvalidClientPrivateKeyFile'

echo "PASS - Mikrotik RouterOS container prepare path; native WireGuard generator remains deferred/unwired"
