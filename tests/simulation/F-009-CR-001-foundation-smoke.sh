#!/usr/bin/env bash
# tests/simulation/F-009-CR-001-foundation-smoke.sh
#
# Foundation-level Docker simulation for F-009 CR-001/CR-002.
#
# CR-001 ships the v2.0 role/topology/transport substrate. CR-002 adds the
# control-plane daemon. The simulation proves the substrate builds, the
# control-plane starts, and CR-scoped critical tests keep passing:
#
#   R1  go build ./... succeeds
#   R2  go vet ./... clean
#   R3  gofmt -l . clean
#   R4  go test -p 1 -count=1 -short ./... PASS
#   R5  awg-mesh-node binary builds, --version reports v2.0.0
#   R6  control-plane starts; implemented role dry-runs validate; client/clientd reports required flags
#   R7  awg-mesh-node with no --mode exits 2 (usage error)
#   R8  awg-mesh-node --mode invalid exits 2 (usage error)
#   R9  mesh-ctl binary builds, version subcommand reports v2.0.0
#   R10 pkg/topology unit tests verify v1.x rejected, v2.0 accepted
#   R11 pkg/role unit tests verify role composability validator
#   R12 tests/critical/run-all.sh runs without crash, returns 0 FAIL
#
# Replaces tests/simulation/issue-92-rotation.sh (v1.x release gate). Per
# F-009 plan, CR-011 (critical-suite v2) implements production-grade v2.0
# release-gate sim with real container deployments. This smoke is foundation
# scope only — compile/build/start/control-plane contract, not data-plane packets.
#
# Usage (from repo root, requires Docker + libpcap-dev image build):
#   bash tests/simulation/F-009-CR-001-foundation-smoke.sh
#
# Exit: 0 = all 12 checks PASS, non-zero = failed check count.

set -euo pipefail

readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly WSL_REPO_PATH="${WSL_REPO_PATH:-${REPO_ROOT}}"
readonly DOCKER_IMAGE="${DOCKER_IMAGE:-awg-mesh-smoke:foundation}"
readonly DOCKER_DOCKERFILE="${REPO_ROOT}/tests/simulation/Dockerfile.smoke"
# Per-check timeout cap. No single Docker invocation should exceed this — if
# it does, we kill the run and report the failure rather than letting the
# whole smoke hang for hours (lesson from 1h-23m apt-get-update wedge).
readonly DOCKER_RUN_TIMEOUT="${DOCKER_RUN_TIMEOUT:-180}"
# Volume names for cached Go module + build cache so dependencies are not
# re-downloaded on every R-check.
readonly GOMODCACHE_VOL="${GOMODCACHE_VOL:-awg-mesh-smoke-gomod}"
readonly GOCACHE_VOL="${GOCACHE_VOL:-awg-mesh-smoke-gocache}"

cd "${REPO_ROOT}"

pass=0
fail=0
fail_names=()

ok() {
    echo "[PASS] $1"
    ((++pass))
}

bad() {
    echo "[FAIL] $1: $2"
    ((++fail))
    fail_names+=("$1")
}

run_host_with_timeout() {
    local limit="$1"
    shift
    "$@" &
    local pid=$!
    local deadline=$((SECONDS + limit))
    while kill -0 "${pid}" >/dev/null 2>&1; do
        if (( SECONDS >= deadline )); then
            kill "${pid}" >/dev/null 2>&1 || true
            wait "${pid}" 2>/dev/null || true
            return 124
        fi
        sleep 1
    done
    wait "${pid}"
}

run_in_docker() {
    local cmd="$1"
    local limit="${2:-${DOCKER_RUN_TIMEOUT}}"
    local container_name="awg-mesh-smoke-${RANDOM}-${RANDOM}"
    docker run --rm \
        --name "${container_name}" \
        --cap-add=NET_ADMIN \
        -v "${WSL_REPO_PATH}:/work" \
        -v "${GOMODCACHE_VOL}:/go/pkg/mod" \
        -v "${GOCACHE_VOL}:/root/.cache/go-build" \
        -w /work \
        "${DOCKER_IMAGE}" \
        bash -c "${cmd}" &
    local pid=$!
    local deadline=$((SECONDS + limit))
    while kill -0 "${pid}" >/dev/null 2>&1; do
        if (( SECONDS >= deadline )); then
            docker rm -f "${container_name}" >/dev/null 2>&1 || true
            wait "${pid}" 2>/dev/null || true
            echo "timeout after ${limit}s: ${container_name}" >&2
            return 124
        fi
        sleep 1
    done
    wait "${pid}"
}

# Build the smoke image once. libpcap-dev pre-baked → per-check invocations
# don't apt-get update on every run (which is what wedged the smoke previously).
ensure_smoke_image() {
    if [ "${SMOKE_REBUILD_IMAGE:-0}" != "1" ] && docker image inspect "${DOCKER_IMAGE}" >/dev/null 2>&1; then
        echo "Reusing existing smoke image: ${DOCKER_IMAGE}"
        return 0
    fi
    if [ "${SMOKE_REBUILD_IMAGE:-0}" = "1" ]; then
        echo "SMOKE_REBUILD_IMAGE=1 — rebuilding smoke image: ${DOCKER_IMAGE}"
    fi
    echo "Building smoke image ${DOCKER_IMAGE} from ${DOCKER_DOCKERFILE}..."
    if ! run_host_with_timeout 600 docker build -t "${DOCKER_IMAGE}" -f "${DOCKER_DOCKERFILE}" "${REPO_ROOT}/tests/simulation"; then
        echo "[FATAL] Smoke image build failed or timed out after 600s. Aborting." >&2
        exit 99
    fi
}

echo "=== F-009 CR-001 foundation smoke ==="
echo "Repo:    ${REPO_ROOT}"
echo "Image:   ${DOCKER_IMAGE}"
echo "Timeout: ${DOCKER_RUN_TIMEOUT}s per check"
echo ""

ensure_smoke_image

# Image bakes libpcap-dev — no apt-get in per-check prelude. Just set -e.
PRELUDE='set -euo pipefail'

# R1: go build clean
if run_in_docker "${PRELUDE}; go build ./... 2>&1" >/tmp/F009-r1.log 2>&1; then
    ok "R1 — go build ./... clean"
else
    bad "R1" "$(tail -5 /tmp/F009-r1.log)"
fi

# R2: go vet clean
if run_in_docker "${PRELUDE}; go vet ./... 2>&1" >/tmp/F009-r2.log 2>&1; then
    ok "R2 — go vet ./... clean"
else
    bad "R2" "$(tail -5 /tmp/F009-r2.log)"
fi

# R3: gofmt clean
if run_in_docker "${PRELUDE}; out=\$(gofmt -l . 2>&1); if [ -n \"\$out\" ]; then echo \"gofmt drift:\"; echo \"\$out\"; exit 1; fi" >/tmp/F009-r3.log 2>&1; then
    ok "R3 — gofmt -l . clean"
else
    bad "R3" "$(tail -10 /tmp/F009-r3.log)"
fi

# R4: tests. Serialize packages inside the Docker smoke container so
# localhost gRPC and NET_ADMIN fixtures do not contend under Docker Desktop.
if run_in_docker "${PRELUDE}; go test -p 1 -count=1 -short ./... 2>&1" >/tmp/F009-r4.log 2>&1; then
    ok "R4 — go test -p 1 -short ./... PASS"
else
    bad "R4" "$(tail -15 /tmp/F009-r4.log)"
fi

# R5: awg-mesh-node --version
expected_node_version="${EXPECTED_NODE_VERSION:-v2.0.0}"
expected_meshctl_version="${expected_node_version}"
if run_in_docker "${PRELUDE}; go build -o /tmp/awg-mesh-node ./cmd/awg-mesh-node && /tmp/awg-mesh-node --version 2>&1" >/tmp/F009-r5.log 2>&1; then
    if grep -q "awg-mesh-node ${expected_node_version}" /tmp/F009-r5.log; then
        ok "R5 — awg-mesh-node --version reports ${expected_node_version}"
    else
        bad "R5" "version output mismatch: $(cat /tmp/F009-r5.log)"
    fi
else
    bad "R5" "build/run failed: $(tail -5 /tmp/F009-r5.log)"
fi

# R6: control-plane starts, implemented role dry-runs validate, and real
# client/clientd reports usage.
r6_failed=0
set +e
run_in_docker "${PRELUDE}; go build -o /tmp/awg-mesh-node ./cmd/awg-mesh-node && /tmp/awg-mesh-node --mode control-plane --listen 127.0.0.1:0 --state-dir /tmp/awg-mesh-cp" 8 >/tmp/F009-r6-control-plane.log 2>&1
r6_status=$?
set -e
if ! grep -q 'control-plane: listening' /tmp/F009-r6-control-plane.log; then
    r6_failed=1
    bad "R6 (--mode control-plane)" "did not reach listening state: $(tail -5 /tmp/F009-r6-control-plane.log)"
elif [ "${r6_status}" -ne 124 ]; then
    r6_failed=1
    bad "R6 (--mode control-plane)" "expected watchdog stop (124), got ${r6_status}: $(tail -5 /tmp/F009-r6-control-plane.log)"
fi
if ! run_in_docker "${PRELUDE}; go build -o /tmp/awg-mesh-node ./cmd/awg-mesh-node && /tmp/awg-mesh-node --mode master --dry-run --name master-01 --overlay-ip 172.21.92.2 2>&1" \
    >/tmp/F009-r6-master.log 2>&1; then
    r6_failed=1
    bad "R6 (--mode master --dry-run)" "exited non-zero: $(tail -3 /tmp/F009-r6-master.log)"
elif ! grep -q 'client=wg-clients:51820/vanilla-wg mesh=wg-mesh:51821/amneziawg' /tmp/F009-r6-master.log; then
    r6_failed=1
    bad "R6 (--mode master --dry-run)" "dual listener plan missing: $(cat /tmp/F009-r6-master.log)"
fi
if ! run_in_docker "${PRELUDE}; go build -o /tmp/awg-mesh-node ./cmd/awg-mesh-node && /tmp/awg-mesh-node --mode egress --dry-run --name egress-01 --overlay-ip 172.21.92.20 --internet-iface eth0 2>&1" \
    >/tmp/F009-r6-egress.log 2>&1; then
    r6_failed=1
    bad "R6 (--mode egress --dry-run)" "exited non-zero: $(tail -3 /tmp/F009-r6-egress.log)"
elif ! grep -q 'nat=awg_mesh:nat_postrouting/oifname eth0 masquerade' /tmp/F009-r6-egress.log; then
    r6_failed=1
    bad "R6 (--mode egress --dry-run)" "NAT plan missing: $(cat /tmp/F009-r6-egress.log)"
fi
if ! run_in_docker "${PRELUDE}; go build -o /tmp/awg-mesh-node ./cmd/awg-mesh-node && /tmp/awg-mesh-node --mode endpoint --dry-run --name egress-01 --overlay-ip 172.21.92.20 --internet-iface eth0 2>&1" \
    >/tmp/F009-r6-endpoint.log 2>&1; then
    r6_failed=1
    bad "R6 (--mode endpoint --dry-run)" "exited non-zero: $(tail -3 /tmp/F009-r6-endpoint.log)"
elif ! grep -q 'warning: --mode endpoint is deprecated' /tmp/F009-r6-endpoint.log || ! grep -q 'egress dry-run node=egress-01' /tmp/F009-r6-endpoint.log; then
    r6_failed=1
    bad "R6 (--mode endpoint --dry-run)" "endpoint alias plan/warning missing: $(cat /tmp/F009-r6-endpoint.log)"
fi
if ! run_in_docker "${PRELUDE}; go build -o /tmp/awg-mesh-node ./cmd/awg-mesh-node && /tmp/awg-mesh-node --mode ingress --dry-run --name ingress-01 --overlay-ip 172.21.92.30 --ingress-public-addr :8443 --ingress-route media.example.com=172.21.92.10:8096 2>&1" \
    >/tmp/F009-r6-ingress.log 2>&1; then
    r6_failed=1
    bad "R6 (--mode ingress --dry-run)" "exited non-zero: $(tail -3 /tmp/F009-r6-ingress.log)"
elif ! grep -q 'ingress dry-run node=ingress-01' /tmp/F009-r6-ingress.log || ! grep -q 'media.example.com->172.21.92.10:8096' /tmp/F009-r6-ingress.log; then
    r6_failed=1
    bad "R6 (--mode ingress --dry-run)" "ingress plan missing: $(cat /tmp/F009-r6-ingress.log)"
fi
if ! run_in_docker "${PRELUDE}; go build -o /tmp/awg-mesh-node ./cmd/awg-mesh-node && /tmp/awg-mesh-node --mode balancer --dry-run --name master-01 --overlay-ip 172.21.92.1 --balancer-egress egress-ru=172.21.92.10:51821,weight=2 --balancer-egress egress-eu=172.21.92.11:51821,weight=1 --balancer-dscp 10=egress-ru 2>&1" \
    >/tmp/F009-r6-balancer.log 2>&1; then
    r6_failed=1
    bad "R6 (--mode balancer --dry-run)" "exited non-zero: $(tail -3 /tmp/F009-r6-balancer.log)"
elif ! grep -q 'balancer dry-run node=master-01' /tmp/F009-r6-balancer.log || ! grep -q 'dscp=10->egress-ru' /tmp/F009-r6-balancer.log; then
    r6_failed=1
    bad "R6 (--mode balancer --dry-run)" "balancer plan missing: $(cat /tmp/F009-r6-balancer.log)"
fi
clientd_out=$(run_in_docker "${PRELUDE}; go build -o /tmp/awg-mesh-node ./cmd/awg-mesh-node 2>/dev/null; set +e; /tmp/awg-mesh-node --mode clientd; echo \"EXIT=\$?\"" 2>&1 || true)
if ! echo "${clientd_out}" | grep -q "EXIT=2"; then
    r6_failed=1
    bad "R6 (--mode clientd)" "expected EXIT=2, got: ${clientd_out}"
elif ! echo "${clientd_out}" | grep -q "missing required flags"; then
    r6_failed=1
    bad "R6 (--mode clientd)" "expected missing required flags usage text, got: ${clientd_out}"
fi
client_out=$(run_in_docker "${PRELUDE}; go build -o /tmp/awg-mesh-node ./cmd/awg-mesh-node 2>/dev/null; set +e; /tmp/awg-mesh-node --mode client; echo \"EXIT=\$?\"" 2>&1 || true)
if ! echo "${client_out}" | grep -q "EXIT=2"; then
    r6_failed=1
    bad "R6 (--mode client)" "expected EXIT=2, got: ${client_out}"
elif ! echo "${client_out}" | grep -q "missing required flags"; then
    r6_failed=1
    bad "R6 (--mode client)" "expected missing required flags usage text, got: ${client_out}"
fi
if [ "${r6_failed}" -eq 0 ]; then
    ok "R6 — control-plane starts; master/egress/ingress/balancer dry-runs validate; client/clientd reports required flags"
fi

# R7: no --mode → exit 2
# Disable set -e inside the inner bash so a deliberate non-zero exit from
# the binary does not abort before the echo captures it.
out=$(run_in_docker "${PRELUDE}; go build -o /tmp/awg-mesh-node ./cmd/awg-mesh-node 2>/dev/null; set +e; /tmp/awg-mesh-node; echo \"EXIT=\$?\"" 2>&1 || true)
if echo "${out}" | grep -q "EXIT=2"; then
    ok "R7 — awg-mesh-node with no --mode exits 2 (usage)"
else
    bad "R7" "expected EXIT=2, got: ${out}"
fi

# R8: invalid mode → exit 2
out=$(run_in_docker "${PRELUDE}; go build -o /tmp/awg-mesh-node ./cmd/awg-mesh-node 2>/dev/null; set +e; /tmp/awg-mesh-node --mode bogus; echo \"EXIT=\$?\"" 2>&1 || true)
if echo "${out}" | grep -q "EXIT=2"; then
    ok "R8 — awg-mesh-node --mode bogus exits 2 (usage)"
else
    bad "R8" "expected EXIT=2, got: ${out}"
fi

# R9: mesh-ctl version
out=$(run_in_docker "${PRELUDE}; go build -o /tmp/mesh-ctl ./cmd/mesh-ctl && /tmp/mesh-ctl version 2>&1" 2>&1 || true)
if echo "${out}" | grep -q "mesh-ctl version ${expected_meshctl_version}"; then
    ok "R9 — mesh-ctl version subcommand reports ${expected_meshctl_version}"
else
    bad "R9" "expected 'mesh-ctl version ${expected_meshctl_version}', got: ${out}"
fi

# R10: pkg/topology unit tests cover v1.x reject + v2.0 accept
if run_in_docker "${PRELUDE}; go test -count=1 -short -run 'TestValidateV2|TestDetectSchemaVersion' ./pkg/topology/... 2>&1" >/tmp/F009-r10.log 2>&1; then
    ok "R10 — topology v1.x rejected, v2.0 accepted (TestValidateV2 + TestDetectSchemaVersion)"
else
    bad "R10" "$(tail -10 /tmp/F009-r10.log)"
fi

# R11: pkg/role validator
if run_in_docker "${PRELUDE}; go test -count=1 -short -run 'TestValidateComposability' ./pkg/role/... 2>&1" >/tmp/F009-r11.log 2>&1; then
    ok "R11 — role composability validator (TestValidateComposability)"
else
    bad "R11" "$(tail -10 /tmp/F009-r11.log)"
fi

# R12: critical-suite runner — must exit 0 with no FAILs (PASS/SKIP mix OK).
# CR-001 ships 3 real tests (composable-roles, single-tun, pluggable-transport)
# + 15 SKIP stubs implemented in subsequent CRs. As later CRs land, the
# PASS count grows; what matters here is "0 FAIL" and exit 0.
out=$(run_in_docker "${PRELUDE}; set +e; bash tests/critical/run-all.sh; echo \"RUNNER_EXIT=\$?\"" 2>&1 || true)
if echo "${out}" | grep -qE 'RUNNER_EXIT=0' && echo "${out}" | grep -qE '0 FAIL'; then
    pass_count=$(echo "${out}" | grep -oE '[0-9]+ PASS' | head -1 | grep -oE '[0-9]+')
    skip_count=$(echo "${out}" | grep -oE '[0-9]+ SKIP' | head -1 | grep -oE '[0-9]+')
    ok "R12 — critical-suite: ${pass_count} PASS, ${skip_count} SKIP, 0 FAIL — exit 0"
else
    bad "R12" "$(echo "${out}" | tail -5)"
fi

echo ""
echo "F-009 CR-001 smoke summary: ${pass} PASS, ${fail} FAIL"
if [ "${fail}" -gt 0 ]; then
    echo "Failed checks: ${fail_names[*]}"
    exit "${fail}"
fi
exit 0
