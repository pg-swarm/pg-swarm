#!/usr/bin/env bash
# run_all.sh — Run all config change management tests in sequence.
#
# Usage:
#   ./tests/config/run_all.sh              # Run all tests
#   ./tests/config/run_all.sh --verbose    # Show all command output (build logs, API responses)
#   ./tests/config/run_all.sh --no-build   # Skip image build (use existing images)
#   ./tests/config/run_all.sh --no-setup   # Skip setup entirely (assume cluster exists)
#   ./tests/config/run_all.sh --no-cleanup # Skip cleanup at end

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# --- Colors ---
RED='\033[0;31m'
GREEN='\033[0;32m'
CYAN='\033[0;36m'
BOLD='\033[1m'
RESET='\033[0m'

SKIP_SETUP=false
SKIP_BUILD=false
SKIP_CLEANUP=false
VERBOSE="${VERBOSE:-false}"
PASS_ARGS=()
for arg in "$@"; do
    case "$arg" in
        --no-setup)   SKIP_SETUP=true ;;
        --no-build)   SKIP_BUILD=true ;;
        --no-cleanup) SKIP_CLEANUP=true ;;
        --verbose|-v) VERBOSE=true; PASS_ARGS+=(--verbose) ;;
    esac
done
export VERBOSE SKIP_BUILD

echo ""
echo -e "${BOLD}${CYAN}╔══════════════════════════════════════════════════════════════════════════════╗${RESET}"
echo -e "${BOLD}${CYAN}║          pg-swarm Config Change Management — Integration Tests               ║${RESET}"
echo -e "${BOLD}${CYAN}╚══════════════════════════════════════════════════════════════════════════════╝${RESET}"
echo ""
echo -e "  Start time: $(date '+%Y-%m-%d %H:%M:%S')"
if [ "$VERBOSE" = true ]; then
    echo -e "  Mode: ${BOLD}verbose${RESET} (showing all command output)"
fi
echo ""

TOTAL_PASS=0
TOTAL_FAIL=0
SUITE_START=$(date +%s)

run_test() {
    local script="$1"
    local name
    name=$(basename "$script" .sh)

    echo -e "${BOLD}▶ Running: ${name}${RESET}"

    local start
    start=$(date +%s)

    if bash "$script" "${PASS_ARGS[@]+"${PASS_ARGS[@]}"}"; then
        local elapsed=$(( $(date +%s) - start ))
        echo -e "  ${GREEN}✓ ${name} completed in ${elapsed}s${RESET}"
        TOTAL_PASS=$((TOTAL_PASS + 1))
    else
        local elapsed=$(( $(date +%s) - start ))
        echo -e "  ${RED}✗ ${name} failed after ${elapsed}s${RESET}"
        TOTAL_FAIL=$((TOTAL_FAIL + 1))

        # On failure, optionally continue or abort
        if [ "${CONTINUE_ON_FAILURE:-false}" != "true" ]; then
            echo -e "\n${RED}Aborting test suite due to failure.${RESET}"
            echo -e "Set CONTINUE_ON_FAILURE=true to continue after failures.\n"
            # Still run cleanup
            if [ "$SKIP_CLEANUP" = false ] && [ -f "$SCRIPT_DIR/99_cleanup.sh" ]; then
                echo -e "${BOLD}▶ Running cleanup despite failure...${RESET}"
                bash "$SCRIPT_DIR/99_cleanup.sh" "${PASS_ARGS[@]+"${PASS_ARGS[@]}"}" || true
            fi
            exit 1
        fi
    fi
    echo ""
}

# --- Run tests ---

if [ "$SKIP_SETUP" = false ]; then
    run_test "$SCRIPT_DIR/01_setup.sh"
fi

run_test "$SCRIPT_DIR/02_profile_unlock.sh"
run_test "$SCRIPT_DIR/03_sequential_restart.sh"
run_test "$SCRIPT_DIR/04_immutable_rejection.sh"
run_test "$SCRIPT_DIR/05_change_classification.sh"
run_test "$SCRIPT_DIR/06_version_history.sh"
run_test "$SCRIPT_DIR/07_full_restart.sh"
run_test "$SCRIPT_DIR/08_cluster_databases.sh"

if [ "$SKIP_CLEANUP" = false ]; then
    run_test "$SCRIPT_DIR/99_cleanup.sh"
fi

# --- Summary ---

SUITE_ELAPSED=$(( $(date +%s) - SUITE_START ))

echo ""
echo -e "${BOLD}╔══════════════════════════════════════════════════════════════════════════════╗${RESET}"
if [ "$TOTAL_FAIL" -eq 0 ]; then
    echo -e "${BOLD}${GREEN}║  All $TOTAL_PASS test suites passed in ${SUITE_ELAPSED}s${RESET}"
else
    echo -e "${BOLD}${RED}║  $TOTAL_FAIL/$((TOTAL_PASS + TOTAL_FAIL)) test suites failed (${SUITE_ELAPSED}s)${RESET}"
fi
echo -e "${BOLD}╚══════════════════════════════════════════════════════════════════════════════╝${RESET}"
echo ""

exit "$TOTAL_FAIL"
