#!/usr/bin/env bash
# mikrotik-version-matrix.sh — runtime dispatcher for mikrotik-chr-e2e.sh across
# nftables-capable CHR versions defined in MIKROTIK-VERSION-COMPAT.md.
#
# Runs sequentially (single /dev/kvm lane locally; CI parallelizes
# across runners). Aggregates per-version PASS/FAIL into a final matrix
# table. Exits 0 only when every version passes.
#
# RouterOS 7.16/7.20 are generator syntax pivots, not current runtime
# data-plane targets. They are covered by pkg/mikrotik tests unless a separate
# import-only CHR gate is added.
#
# Usage:
#   bash tests/simulation/mikrotik-version-matrix.sh
#   CHR_VERSIONS="7.21.4" bash ...                  # override runtime list
#
# Exit codes: 0 all PASS / N number of FAILed versions / 2 env failure.
set -euo pipefail

readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly E2E_SCRIPT="${REPO_ROOT}/simulation/mikrotik-chr-e2e.sh"
readonly BUILD_BASELINE="${REPO_ROOT}/simulation/lib/build-chr-baseline.sh"
readonly DEFAULT_VERSIONS="7.21.4"
readonly CHR_VERSIONS="${CHR_VERSIONS:-${DEFAULT_VERSIONS}}"
readonly BASELINE_READY_LABEL="awg-mesh.chr-container-enabled"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RESET='\033[0m'

if [[ ! -x "${E2E_SCRIPT}" ]]; then
    chmod +x "${E2E_SCRIPT}" 2>/dev/null || true
fi
if [[ ! -x "${BUILD_BASELINE}" ]]; then
    chmod +x "${BUILD_BASELINE}" 2>/dev/null || true
fi

declare -A RESULTS

echo "=== mikrotik-version-matrix: ${CHR_VERSIONS} ==="
echo ""

is_runtime_supported() {
    local version="${1}"
    local major minor
    major="$(printf '%s' "${version}" | cut -d. -f1)"
    minor="$(printf '%s' "${version}" | cut -d. -f2)"

    [[ "${major}" == "7" ]] || return 1
    [[ "${minor}" =~ ^[0-9]+$ ]] || return 1
    [[ "${minor}" -ge 21 ]] || return 1
    [[ "${minor}" -ne 22 ]] || return 1
    return 0
}

baseline_ready() {
    local version="${1}"
    local image="awg-mesh-chr-baseline:${version}"
    local label

    label="$(docker image inspect "${image}" --format '{{ index .Config.Labels "awg-mesh.chr-container-enabled" }}' 2>/dev/null || true)"
    [[ "${label}" == "true" ]]
}

for VER in ${CHR_VERSIONS}; do
    echo "──────────────────────────────────────────────────────────────────"
    echo " ${VER}"
    echo "──────────────────────────────────────────────────────────────────"

    if ! is_runtime_supported "${VER}"; then
        echo -e "[matrix] ${RED}${VER} is not a supported runtime target${RESET}"
        echo "[matrix] pre-7.21 RouterOS versions are syntax targets only; 7.22.x is blocked by the documented ip-rule regression"
        RESULTS[${VER}]="UNSUPPORTED_RUNTIME"
        continue
    fi

    # Step 1: ensure baseline image exists and is proven container-ready.
    if ! baseline_ready "${VER}"; then
        echo "[matrix] baseline missing or missing ${BASELINE_READY_LABEL}=true — building..."
        if CHR_VERSION="${VER}" bash "${BUILD_BASELINE}"; then
            echo "[matrix] baseline ${VER} built"
        else
            echo -e "[matrix] ${RED}baseline build FAILED${RESET} for ${VER} — skipping E2E"
            RESULTS[${VER}]="BASELINE_FAIL"
            continue
        fi
    fi

    # Step 2: run E2E
    if CHR_VERSION="${VER}" TARGET_ROS_VERSION="${VER}" bash "${E2E_SCRIPT}"; then
        RESULTS[${VER}]="PASS"
    else
        RESULTS[${VER}]="FAIL"
    fi
    echo ""
done

# ---------------------------------------------------------------------------
# Final matrix
# ---------------------------------------------------------------------------
echo ""
echo "=================================================================="
echo " mikrotik-version-matrix: SUMMARY"
echo "=================================================================="
FAIL_COUNT=0
for VER in ${CHR_VERSIONS}; do
    R="${RESULTS[${VER}]:-NOT_RUN}"
    case "${R}" in
        PASS)            echo -e "  ${VER}   ${GREEN}${R}${RESET}" ;;
        FAIL)            echo -e "  ${VER}   ${RED}${R}${RESET}";          (( FAIL_COUNT++ )) ;;
        BASELINE_FAIL)   echo -e "  ${VER}   ${YELLOW}${R}${RESET} (baseline build broken — likely CHR ${VER} first-boot SSH bug, see CR-002 NFR-5)"; (( FAIL_COUNT++ )) ;;
        UNSUPPORTED_RUNTIME) echo -e "  ${VER}   ${RED}${R}${RESET}";       (( FAIL_COUNT++ )) ;;
        *)               echo -e "  ${VER}   ${YELLOW}${R}${RESET}";       (( FAIL_COUNT++ )) ;;
    esac
done
echo "=================================================================="
echo ""

exit "${FAIL_COUNT}"
