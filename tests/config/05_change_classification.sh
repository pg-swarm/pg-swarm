#!/usr/bin/env bash
# 05_change_classification.sh — Test that different param types are classified correctly.
# Three modes: reload (sighup params), sequential (postmaster params), full_restart (wal_level)

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/helpers.sh"

header "Test: Change Classification — Correct apply strategy for different params"

CURRENT=$(api_get "/profiles/${PROFILE_ID}")

# Helper: update a field and check the apply_strategy without actually applying
check_strategy() {
    local description="$1"
    local jq_transform="$2"
    local expected_strategy="$3"

    local updated
    updated=$(echo "$CURRENT" | python3 -c "$jq_transform")
    local result
    result=$(api_put "/profiles/${PROFILE_ID}" "$updated" 2>/dev/null || true)

    local strategy
    strategy=$(echo "$result" | python3 -c "import json,sys; print(json.load(sys.stdin).get('change_impact',{}).get('apply_strategy','no_change'))" 2>/dev/null || echo "error")

    if [ "$strategy" = "$expected_strategy" ]; then
        pass "$description → $strategy"
    else
        fail "$description: expected $expected_strategy, got $strategy"
    fi

    # Revert the profile back to CURRENT for next test
    api_put "/profiles/${PROFILE_ID}" "$CURRENT" > "$DEVNULL" 2>&1 || true
}

# 1. Sighup param → reload (no restart)
check_strategy "work_mem change (sighup)" "
import json, sys
p = json.load(sys.stdin)
p['config']['pg_params']['work_mem'] = '256MB'
print(json.dumps(p))
" "reload"

# 2. wal_level → full_restart (replication-sensitive)
check_strategy "wal_level change" "
import json, sys
p = json.load(sys.stdin)
p['config']['pg_params']['wal_level'] = 'logical'
print(json.dumps(p))
" "full_restart"

# 3. Resource change → sequential_restart (pod template change)
check_strategy "CPU request change" "
import json, sys
p = json.load(sys.stdin)
p['config']['resources']['cpu_request'] = '200m'
print(json.dumps(p))
" "sequential_restart"

# 4. HBA rules change → reload (pg_reload_conf)
check_strategy "HBA rule change (sighup)" "
import json, sys
p = json.load(sys.stdin)
p['config']['hba_rules'] = ['host all all 0.0.0.0/0 scram-sha-256']
print(json.dumps(p))
" "reload"

# 5. Mixed reload + full_restart → full_restart (highest priority wins)
check_strategy "Mixed: work_mem + wal_level" "
import json, sys
p = json.load(sys.stdin)
p['config']['pg_params']['work_mem'] = '512MB'
p['config']['pg_params']['wal_level'] = 'logical'
print(json.dumps(p))
" "full_restart"

# 6. Postmaster param → sequential_restart (from DB classification)
check_strategy "shared_buffers change (postmaster)" "
import json, sys
p = json.load(sys.stdin)
p['config']['pg_params']['shared_buffers'] = '1GB'
print(json.dumps(p))
" "sequential_restart"

summary
