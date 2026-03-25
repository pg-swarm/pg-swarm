#!/usr/bin/env bash
# 02_profile_unlock.sh — Test that profiles can be updated while in use
# and changes are classified correctly.

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/helpers.sh"

header "Test: Profile Unlock — Edit profiles with active clusters"

# 1. Verify profile is in use
LOCKED=$(api_get "/profiles/${PROFILE_ID}" | python3 -c "import json,sys; print(json.load(sys.stdin)['locked'])")
if [ "$LOCKED" = "True" ]; then
    pass "Profile is in-use (locked=true) as expected"
else
    fail "Profile should be in-use but locked=$LOCKED"
fi

# 2. Update profile with a reload-class param (statement_timeout is sighup context)
info "Updating profile: adding statement_timeout=45s..."
CURRENT=$(api_get "/profiles/${PROFILE_ID}")
UPDATED=$(echo "$CURRENT" | python3 -c "
import json, sys
p = json.load(sys.stdin)
if 'pg_params' not in p['config']:
    p['config']['pg_params'] = {}
p['config']['pg_params']['statement_timeout'] = '45s'
print(json.dumps(p))
")
RESULT=$(api_put "/profiles/${PROFILE_ID}" "$UPDATED")

# 3. Verify change_impact returned with reload strategy
STRATEGY=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('change_impact',{}).get('apply_strategy','none'))" 2>/dev/null)
if [ "$STRATEGY" = "reload" ]; then
    pass "Profile updated — apply_strategy=reload (statement_timeout is sighup)"
else
    fail "Expected reload, got: $STRATEGY"
fi

# 4. Verify affected clusters listed
AFFECTED=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['change_impact']['affected_clusters'])" 2>/dev/null)
if echo "$AFFECTED" | grep -q "$CLUSTER_NAME"; then
    pass "Affected cluster '${CLUSTER_NAME}' listed in change_impact"
else
    fail "Cluster ${CLUSTER_NAME} not in affected list" "$AFFECTED"
fi

# 5. Verify reload_changes contains statement_timeout
CHANGES=$(echo "$RESULT" | python3 -c "
import json, sys
changes = json.load(sys.stdin)['change_impact']['reload_changes']
for c in changes:
    print(f\"{c['path']}: {c['old_value']} → {c['new_value']}\")
" 2>/dev/null)
if echo "$CHANGES" | grep -q "statement_timeout"; then
    pass "Change detected: $CHANGES"
else
    fail "statement_timeout not in reload_changes"
fi

summary
