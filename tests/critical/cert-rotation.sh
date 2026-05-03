#!/usr/bin/env bash
# tests/critical/cert-rotation.sh - certificate issuance and wire-schema gate.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

GO=${GO_BIN:-/usr/local/go/bin/go}
if ! command -v "$GO" >/dev/null 2>&1; then
    GO=go
fi
if ! command -v "$GO" >/dev/null 2>&1; then
    echo "FAIL - go toolchain not available; run inside Docker" >&2
    exit 1
fi

"$GO" test -count=1 -run 'TestGenerateCA|TestIssueCert|TestSaveLoadCertKey|TestValidateCert|TestCertInfo|TestRunNodePrepareCommandWritesV2TokenMaterialAndCerts|TestServer_StreamCertUpdate|TestNewDaemon_ConfiguresCertLifecycleFromCADir|TestNewDaemon_RejectsIncompleteDefaultCAMaterial|TestAgentAppliesCertUpdateToLocalFiles|TestApplyCertUpdateRemovesNewKeyWhenCertWriteFails' \
    ./pkg/tls/... ./cmd/mesh-ctl/cmd ./pkg/control_plane ./pkg/clientd >/dev/null

if ! grep -q 'rpc StreamCertUpdate' proto/control_plane.proto; then
    echo "FAIL - StreamCertUpdate RPC missing from proto/control_plane.proto" >&2
    exit 1
fi
if ! grep -q 'message CertUpdate' proto/control_plane.proto; then
    echo "FAIL - CertUpdate message missing from proto/control_plane.proto" >&2
    exit 1
fi

echo "PASS - cert-rotation.sh: cert issuance, node prepare cert material, cert lifecycle streams, clientd persistence, and cert-update wire schema verified"
