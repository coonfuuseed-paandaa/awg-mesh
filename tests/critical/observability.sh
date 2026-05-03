#!/usr/bin/env bash
# tests/critical/observability.sh - Prometheus and audit/log surface gate.
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

"$GO" test -count=1 -run 'TestRunTopologyGeneratePrometheusConfig|TestMetricsUseIsolatedRegistry|TestRunAuditLogQueryCommandOutputsPromTextfile|TestWithFields' \
    ./cmd/mesh-ctl/cmd ./pkg/ingress/... ./pkg/balancer/... ./pkg/logging/... >/dev/null

prometheus_out="$("$GO" run ./cmd/mesh-ctl topology generate-prometheus-config --topology pkg/topology/testdata/v2-topology.yml)"
if ! grep -q 'scrape_configs:' <<<"${prometheus_out}"; then
    echo "FAIL - generated Prometheus config missing scrape_configs" >&2
    exit 1
fi
if ! grep -q 'master-01' <<<"${prometheus_out}"; then
    echo "FAIL - generated Prometheus config missing topology-backed node target" >&2
    exit 1
fi

echo "PASS - observability.sh: metrics, Prometheus config, audit textfile, and structured logging verified"
