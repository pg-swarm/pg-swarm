#!/usr/bin/env bash
# Common helpers for config change management tests.
# Source this file from each test script.

set -euo pipefail

# --- Verbosity ---
# Set VERBOSE=true or pass --verbose to any script to see all command output.
VERBOSE="${VERBOSE:-false}"
for _arg in "$@"; do
    if [ "$_arg" = "--verbose" ] || [ "$_arg" = "-v" ]; then
        VERBOSE=true
    fi
done

# Redirect target: /dev/null in quiet mode, /dev/stderr in verbose mode.
if [ "$VERBOSE" = true ]; then
    DEVNULL=/dev/stderr
else
    DEVNULL=/dev/null
fi

# --- Colors ---
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
RESET='\033[0m'

# --- Config ---
API_BASE="${API_BASE:-http://localhost:8080/api/v1}"
PROFILE_ID="${PROFILE_ID:-c0000000-0000-0000-0000-000000000001}"
NAMESPACE="${NAMESPACE:-test}"
CLUSTER_NAME="${CLUSTER_NAME:-test-db}"

# --- Counters ---
TESTS_RUN=0
TESTS_PASSED=0
TESTS_FAILED=0

# --- Output helpers ---
header() {
    echo ""
    echo -e "${BOLD}${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}"
    echo -e "${BOLD}${CYAN}  $1${RESET}"
    echo -e "${BOLD}${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}"
}

pass() {
    TESTS_RUN=$((TESTS_RUN + 1))
    TESTS_PASSED=$((TESTS_PASSED + 1))
    echo -e "  ${GREEN}PASS${RESET}  $1"
}

fail() {
    TESTS_RUN=$((TESTS_RUN + 1))
    TESTS_FAILED=$((TESTS_FAILED + 1))
    echo -e "  ${RED}FAIL${RESET}  $1"
    if [ -n "${2:-}" ]; then
        echo -e "        ${RED}$2${RESET}"
    fi
}

info() {
    echo -e "  ${YELLOW}INFO${RESET}  $1"
}

summary() {
    echo ""
    echo -e "${BOLD}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}"
    if [ "$TESTS_FAILED" -eq 0 ]; then
        echo -e "${BOLD}${GREEN}  All $TESTS_RUN tests passed${RESET}"
    else
        echo -e "${BOLD}${RED}  $TESTS_FAILED/$TESTS_RUN tests failed${RESET}"
    fi
    echo -e "${BOLD}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}"
    echo ""
    return "$TESTS_FAILED"
}

# --- API helpers ---
api_get() {
    curl -sf "${API_BASE}$1" 2>/dev/null
}

api_post() {
    curl -sf -X POST "${API_BASE}$1" -H "Content-Type: application/json" -d "$2" 2>/dev/null
}

api_put() {
    curl -sf -X PUT "${API_BASE}$1" -H "Content-Type: application/json" -d "$2" 2>/dev/null
}

# --- K8s helpers ---
wait_for_pods_ready() {
    local ns="$1" label="$2" expected="$3" timeout="${4:-120}"
    local elapsed=0
    while [ "$elapsed" -lt "$timeout" ]; do
        # Count pods where all containers are ready (READY column like "2/2", "1/1")
        local ready=0
        while IFS= read -r line; do
            local ready_col
            ready_col=$(echo "$line" | awk '{print $2}')
            # Check if format is "N/N" and both numbers match (all containers ready)
            local actual total
            actual=$(echo "$ready_col" | cut -d/ -f1)
            total=$(echo "$ready_col" | cut -d/ -f2)
            if [ "$actual" = "$total" ] && [ "$actual" -gt 0 ] 2>/dev/null; then
                ready=$((ready + 1))
            fi
        done < <(kubectl get pods -n "$ns" -l "$label" --no-headers 2>/dev/null | grep "Running")
        if [ "$ready" -ge "$expected" ]; then
            return 0
        fi
        sleep 2
        elapsed=$((elapsed + 2))
    done
    return 1
}

wait_for_pods_restarted() {
    # Wait until all pods have age < threshold seconds
    local ns="$1" cluster="$2" max_age="${3:-60}" timeout="${4:-180}"
    local elapsed=0
    while [ "$elapsed" -lt "$timeout" ]; do
        local all_young=true
        while IFS= read -r line; do
            local age_str
            age_str=$(echo "$line" | awk '{print $5}')
            # Parse age like "30s", "1m30s", "2m"
            local secs=999
            if [[ "$age_str" =~ ^([0-9]+)s$ ]]; then
                secs="${BASH_REMATCH[1]}"
            elif [[ "$age_str" =~ ^([0-9]+)m$ ]]; then
                secs=$(( ${BASH_REMATCH[1]} * 60 ))
            elif [[ "$age_str" =~ ^([0-9]+)m([0-9]+)s$ ]]; then
                secs=$(( ${BASH_REMATCH[1]} * 60 + ${BASH_REMATCH[2]} ))
            fi
            if [ "$secs" -ge "$max_age" ]; then
                all_young=false
                break
            fi
        done < <(kubectl get pods -n "$ns" -l "pg-swarm.io/cluster=$cluster" --no-headers 2>/dev/null)
        if [ "$all_young" = true ]; then
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    return 1
}
