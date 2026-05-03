critical_run_go_tests_required() {
    local output_file="$1"
    shift
    local run_re="$1"
    shift

    local packages=()
    while [[ "$#" -gt 0 && "$1" != "--" ]]; do
        packages+=("$1")
        shift
    done
    if [[ "$#" -eq 0 ]]; then
        echo "FAIL - critical_run_go_tests_required missing -- separator" >&2
        return 2
    fi
    shift
    if [[ "$#" -eq 0 ]]; then
        echo "FAIL - critical_run_go_tests_required missing required test names after --" >&2
        return 2
    fi

    if ! "$GO" test -count=1 -run "$run_re" -v "${packages[@]}" >"$output_file" 2>&1; then
        cat "$output_file" >&2
        return 1
    fi

    local test_name
    for test_name in "$@"; do
        if ! grep -Eq "^=== RUN[[:space:]]+${test_name}($|/)" "$output_file"; then
            echo "FAIL - required test did not run: ${test_name}" >&2
            cat "$output_file" >&2
            return 1
        fi
        if grep -Eq "^--- SKIP:[[:space:]]+${test_name}([[:space:]]|/|$)" "$output_file"; then
            echo "FAIL - required test was skipped: ${test_name}" >&2
            cat "$output_file" >&2
            return 1
        fi
        if ! grep -Eq "^--- PASS:[[:space:]]+${test_name}([[:space:]]|/|$)" "$output_file"; then
            echo "FAIL - required test did not pass: ${test_name}" >&2
            cat "$output_file" >&2
            return 1
        fi
    done
}
