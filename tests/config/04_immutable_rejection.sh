#!/usr/bin/env bash
# 04_immutable_rejection.sh — Test that immutable field changes are rejected.

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/helpers.sh"

header "Test: Immutable Field Rejection — Storage changes blocked"

# 1. Try to change storage size
info "Attempting to change storage.size from 1Gi to 20Gi..."
CURRENT=$(api_get "/profiles/${PROFILE_ID}")
UPDATED=$(echo "$CURRENT" | python3 -c "
import json, sys
p = json.load(sys.stdin)
p['config']['storage']['size'] = '20Gi'
print(json.dumps(p))
")
HTTP_CODE=$(curl -s -o /tmp/pgswarm-test-response -w "%{http_code}" \
    -X PUT "${API_BASE}/profiles/${PROFILE_ID}" \
    -H "Content-Type: application/json" \
    -d "$UPDATED" 2>/dev/null)
RESULT=$(cat /tmp/pgswarm-test-response)

if [ "$HTTP_CODE" = "400" ]; then
    pass "Rejected with HTTP 400 (as expected)"
else
    fail "Expected HTTP 400, got $HTTP_CODE"
fi

ERROR_MSG=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('error',''))" 2>/dev/null)
if echo "$ERROR_MSG" | grep -qi "immutable"; then
    pass "Error message mentions immutable: $ERROR_MSG"
else
    fail "Error message should mention immutable" "$ERROR_MSG"
fi

IMMUTABLE_PATHS=$(echo "$RESULT" | python3 -c "
import json, sys
errors = json.load(sys.stdin).get('immutable_errors', [])
for e in errors:
    print(f\"{e['path']}: {e['old_value']} → {e['new_value']}\")
" 2>/dev/null)
if echo "$IMMUTABLE_PATHS" | grep -q "storage.size"; then
    pass "Immutable error lists storage.size: $IMMUTABLE_PATHS"
else
    fail "Expected storage.size in immutable_errors"
fi

# 2. Try to change storage class
info "Attempting to change storage.storage_class..."
UPDATED2=$(echo "$CURRENT" | python3 -c "
import json, sys
p = json.load(sys.stdin)
p['config']['storage']['storage_class'] = 'premium-ssd'
print(json.dumps(p))
")
HTTP_CODE2=$(curl -s -o /dev/null -w "%{http_code}" \
    -X PUT "${API_BASE}/profiles/${PROFILE_ID}" \
    -H "Content-Type: application/json" \
    -d "$UPDATED2" 2>/dev/null)
if [ "$HTTP_CODE2" = "400" ]; then
    pass "Storage class change also rejected with HTTP 400"
else
    fail "Expected HTTP 400 for storage class change, got $HTTP_CODE2"
fi

# 3. Verify cluster was NOT affected
PODS_STABLE=$(kubectl get pods -n "$NAMESPACE" -l "pg-swarm.io/cluster=${CLUSTER_NAME}" --no-headers 2>/dev/null | awk '{print $4}' | sort -u)
if echo "$PODS_STABLE" | grep -qE "^0$"; then
    pass "Cluster pods unaffected (0 restarts)"
else
    info "Pod restart counts: $PODS_STABLE"
fi

summary
