#!/usr/bin/env bash
# tests/critical/run-all.sh — runner for the F-009 v2.0 critical test suite.
#
# Per AGENTS.md release gate (rule #10): every release MUST pass the critical
# test suite on a real or staged dev stand before tag + publish. This runner
# iterates every *.sh in tests/critical/ except itself, collects PASS/SKIP/FAIL
# counts, and exits non-zero if any FAIL occurs.
#
# CR-001 ships skeleton stubs that all SKIP. Subsequent CRs (CR-002..CR-014)
# implement individual tests; the runner gates v2.0.0 release readiness once
# all 18 tests are green.
#
# Exit codes:
#   0  — no FAILs (all PASS, or PASS+SKIP mix)
#   1  — at least one FAIL
#   2  — runner error (missing tests/, unreadable file, etc.)
set -euo pipefail

readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly TESTS_DIR="${REPO_ROOT}/tests/critical"
readonly RUNNER_SELF="$(basename "${BASH_SOURCE[0]}")"

if [[ ! -d "${TESTS_DIR}" ]]; then
    echo "[run-all] tests directory missing: ${TESTS_DIR}" >&2
    exit 2
fi

pass=0
skip=0
fail=0
fail_names=()

for test_path in "${TESTS_DIR}"/*.sh; do
    [[ -f "${test_path}" ]] || continue
    test_name="$(basename "${test_path}")"
    [[ "${test_name}" == "${RUNNER_SELF}" ]] && continue

    set +e
    output="$(bash "${test_path}" 2>&1)"
    exit_code=$?
    set -e

    if [[ ${exit_code} -eq 0 ]]; then
        if printf '%s' "${output}" | grep -q '^SKIP'; then
            ((++skip))
            printf '[SKIP] %-40s — %s\n' "${test_name}" "$(printf '%s' "${output}" | head -1)"
        else
            ((++pass))
            printf '[PASS] %-40s\n' "${test_name}"
        fi
    else
        ((++fail))
        fail_names+=("${test_name}")
        printf '[FAIL] %-40s (exit %d)\n' "${test_name}" "${exit_code}"
        printf '%s' "${output}" | sed 's/^/         /'
    fi
done

echo ""
echo "Critical-suite summary: ${pass} PASS, ${skip} SKIP, ${fail} FAIL"
if [[ ${fail} -gt 0 ]]; then
    echo "Failed tests: ${fail_names[*]}"
    exit 1
fi
exit 0
