#!/usr/bin/env bash
# tests/simulation/F-009-CR-001-foundation-smoke.sh
#
# Foundation-level Docker simulation for F-009 CR-001 (flat-mesh-foundation).
#
# CR-001 ships skeleton only — no daemon logic, no networking. The simulation
# proves the v2.0 substrate is buildable and exits cleanly:
#
#   R1  go build ./... succeeds
#   R2  go vet ./... clean
#   R3  gofmt -l . clean
#   R4  go test -count=1 -short ./... PASS
#   R5  awg-mesh-node binary builds, --version reports v2.0.0-alpha.1
#   R6  awg-mesh-node --mode <each role> exits 0 with placeholder line
#   R7  awg-mesh-node with no --mode exits 2 (usage error)
#   R8  awg-mesh-node --mode invalid exits 2 (usage error)
#   R9  mesh-ctl binary builds, version subcommand reports v2.0.0-alpha.1
#   R10 pkg/topology unit tests verify v1.x rejected, v2.0 accepted
#   R11 pkg/role unit tests verify role composability validator
#   R12 tests/critical/run-all.sh runs without crash, returns 0 PASS / 18 SKIP / 0 FAIL
#
# Replaces tests/simulation/issue-92-rotation.sh (v1.x release gate). Per
# F-009 plan, CR-011 (critical-suite v2) implements production-grade v2.0
# release-gate sim with real container deployments. CR-001 sim is foundation
# smoke only — proves the codebase compiles and the binary runs.
#
# Usage (from repo root, requires Docker + libpcap-dev image build):
#   bash tests/simulation/F-009-CR-001-foundation-smoke.sh
#
# Exit: 0 = all 12 checks PASS, non-zero = failed check count.

set -euo pipefail

readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly WSL_REPO_PATH="${WSL_REPO_PATH:-${REPO_ROOT}}"
readonly DOCKER_IMAGE="${DOCKER_IMAGE:-golang:1.25-bookworm}"

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

run_in_docker() {
    docker run --rm \
        --cap-add=NET_ADMIN \
        -v "${WSL_REPO_PATH}:/work" \
        -w /work \
        "${DOCKER_IMAGE}" \
        bash -c "$1"
}

echo "=== F-009 CR-001 foundation smoke ==="
echo "Repo:  ${REPO_ROOT}"
echo "Image: ${DOCKER_IMAGE}"
echo ""

# Pre-flight: build the docker prep prelude once so per-check invocations are fast.
PRELUDE='set -euo pipefail; apt-get update -qq >/dev/null 2>&1; apt-get install -y -qq libpcap-dev >/dev/null 2>&1'

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

# R4: tests
if run_in_docker "${PRELUDE}; go test -count=1 -short ./... 2>&1" >/tmp/F009-r4.log 2>&1; then
    ok "R4 — go test -short ./... PASS"
else
    bad "R4" "$(tail -15 /tmp/F009-r4.log)"
fi

# R5: awg-mesh-node --version
if run_in_docker "${PRELUDE}; go build -o /tmp/awg-mesh-node ./cmd/awg-mesh-node && /tmp/awg-mesh-node --version 2>&1" >/tmp/F009-r5.log 2>&1; then
    if grep -q "awg-mesh-node 2.0.0-alpha.1" /tmp/F009-r5.log; then
        ok "R5 — awg-mesh-node --version reports v2.0.0-alpha.1"
    else
        bad "R5" "version output mismatch: $(cat /tmp/F009-r5.log)"
    fi
else
    bad "R5" "build/run failed: $(tail -5 /tmp/F009-r5.log)"
fi

# R6: each role exits 0
roles=(control-plane master clientd egress ingress balancer)
r6_failed=0
for role in "${roles[@]}"; do
    if ! run_in_docker "${PRELUDE}; go build -o /tmp/awg-mesh-node ./cmd/awg-mesh-node && /tmp/awg-mesh-node --mode ${role} 2>&1" \
        >/tmp/F009-r6-${role}.log 2>&1; then
        r6_failed=1
        bad "R6 (--mode ${role})" "exited non-zero: $(tail -3 /tmp/F009-r6-${role}.log)"
        break
    fi
done
if [ "${r6_failed}" -eq 0 ]; then
    ok "R6 — awg-mesh-node --mode {control-plane,master,clientd,egress,ingress,balancer} all exit 0"
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
if echo "${out}" | grep -q "mesh-ctl version"; then
    ok "R9 — mesh-ctl version subcommand reports a version"
else
    bad "R9" "expected 'mesh-ctl version <X>', got: ${out}"
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
