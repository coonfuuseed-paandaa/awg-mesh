#!/usr/bin/env bash
# tests/critical/mikrotik-v2.sh - CR-014 RouterOS native WireGuard gate.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

go test -count=1 ./pkg/mikrotik/v2
go test -count=1 ./cmd/mesh-ctl/cmd -run 'TestRunNodePrepareCommandWritesMikrotikRouterOSScriptAndKeys'
go test -count=1 ./cmd/awg-mesh-node -run 'TestRunMasterDryRunAcceptsClientPrivateKeyFile|TestRunMasterDryRunRejectsInvalidClientPrivateKeyFile'

echo "PASS - CR-014 Mikrotik v2 native WireGuard generator and prepare path"
