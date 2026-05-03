#!/usr/bin/env bash
# tests/critical/run-all.sh — runner for the F-009 v2.0 critical test suite.
#
# Per AGENTS.md release gate (rule #10): every release MUST pass the critical
# test suite on a real or staged dev stand before tag + publish. This runner
# iterates every *.sh in tests/critical/ except itself, collects PASS/SKIP/FAIL
# counts, and exits non-zero if any FAIL occurs. In strict release mode it also
# exits non-zero if any test reports SKIP.
#
# Exit codes:
#   0  — no FAILs (all PASS, or PASS+SKIP mix)
#   1  — at least one FAIL
#   2  — runner error (missing tests/, unreadable file, etc.)
set -euo pipefail

readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly TESTS_DIR="${REPO_ROOT}/tests/critical"
readonly RUNNER_SELF="$(basename "${BASH_SOURCE[0]}")"

strict=0
for arg in "$@"; do
    case "${arg}" in
        --strict)
            strict=1
            ;;
        *)
            echo "[run-all] unknown argument: ${arg}" >&2
            exit 2
            ;;
    esac
done

if [[ "${CRITICAL_STRICT:-0}" == "1" ]]; then
    strict=1
fi

runner_mode="developer"
if [[ "${strict}" -eq 1 ]]; then
    runner_mode="release"
fi

if [[ ! -d "${TESTS_DIR}" ]]; then
    echo "[run-all] tests directory missing: ${TESTS_DIR}" >&2
    exit 2
fi

pass=0
skip=0
fail=0
fail_names=()
skip_names=()

for test_path in "${TESTS_DIR}"/*.sh; do
    [[ -f "${test_path}" ]] || continue
    test_name="$(basename "${test_path}")"
    [[ "${test_name}" == "${RUNNER_SELF}" ]] && continue

    set +e
    output="$(CRITICAL_RUNNER_MODE="${runner_mode}" bash "${test_path}" 2>&1)"
    exit_code=$?
    set -e

    if [[ ${exit_code} -eq 0 ]]; then
        if printf '%s' "${output}" | grep -q '^SKIP'; then
            ((++skip))
            skip_names+=("${test_name}")
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
        printf '\n'
    fi
done

echo ""
echo "Critical-suite summary: ${pass} PASS, ${skip} SKIP, ${fail} FAIL"
if [[ ${fail} -gt 0 ]]; then
    echo "Failed tests: ${fail_names[*]}"
    exit 1
fi
if [[ "${strict}" -eq 1 && ${skip} -gt 0 ]]; then
    echo "Strict mode rejects skipped tests: ${skip_names[*]}"
    exit 1
fi
exit 0
