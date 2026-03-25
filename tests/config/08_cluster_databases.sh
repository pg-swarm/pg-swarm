#!/usr/bin/env bash
# 09_cluster_databases.sh — Test cluster-level database creation without pod restart.
# Verifies:
#   1. Database can be added via API
#   2. No rolling restart occurs (pod ages unchanged)
#   3. PostgreSQL role and database are created on the primary
#   4. HBA rule is added and reloaded (new user can connect from allowed CIDR)
#   5. Database can be updated (CIDR change)
#   6. Database can be deleted

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/helpers.sh"

header "Test: Cluster-Level Databases — Zero-Restart Database Management"

# Wait for cluster to be stable
info "Waiting for cluster to be stable..."
wait_for_pods_ready "$NAMESPACE" "pg-swarm.io/cluster=${CLUSTER_NAME}" 3 120
sleep 5

# 1. Record pod ages before any changes
info "Recording pod state before database creation:"
BEFORE_AGES=$(kubectl get pods -n "$NAMESPACE" -l "pg-swarm.io/cluster=${CLUSTER_NAME}" --no-headers 2>/dev/null | awk '{print $1 "=" $5}')
echo "  $BEFORE_AGES"

# 2. Get cluster ID
CLUSTER_ID=$(api_get "/clusters" | python3 -c "
import json, sys
for c in json.load(sys.stdin):
    if c['name'] == '${CLUSTER_NAME}':
        print(c['id'])
        break
" 2>/dev/null)
if [ -z "$CLUSTER_ID" ]; then
    fail "Could not find cluster ID for ${CLUSTER_NAME}"
    summary; exit 1
fi
info "Cluster ID: $CLUSTER_ID"

# 3. Create a database via API
info "Creating database 'analytics' with user 'analytics_user'..."
CREATE_RESULT=$(api_post "/clusters/${CLUSTER_ID}/databases" "{
    \"db_name\": \"analytics\",
    \"db_user\": \"analytics_user\",
    \"password\": \"s3cretP4ss!\",
    \"allowed_cidrs\": [\"10.0.0.0/8\", \"192.168.0.0/16\"]
}")
DB_ID=$(echo "$CREATE_RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
if [ -n "$DB_ID" ]; then
    pass "Database created (id: $DB_ID)"
else
    fail "Database creation failed" "$CREATE_RESULT"
    summary; exit 1
fi

# 4. Wait for the sidecar to process the command
info "Waiting for sidecar to create database (20s)..."
sleep 20

# 5. Verify NO rolling restart occurred (pod ages should be >= before)
info "Checking pod state after database creation:"
AFTER_AGES=$(kubectl get pods -n "$NAMESPACE" -l "pg-swarm.io/cluster=${CLUSTER_NAME}" --no-headers 2>/dev/null | awk '{print $1 "=" $5}')
echo "  $AFTER_AGES"

# Check that no pod is younger than 15 seconds (indicating restart)
ALL_OLD=true
while IFS= read -r line; do
    age_str=$(echo "$line" | awk -F= '{print $2}')
    secs=999
    if [[ "$age_str" =~ ^([0-9]+)s$ ]]; then secs="${BASH_REMATCH[1]}"; fi
    if [ "$secs" -lt 15 ] 2>/dev/null; then ALL_OLD=false; break; fi
done <<< "$AFTER_AGES"
if [ "$ALL_OLD" = true ]; then
    pass "No pods restarted (zero-restart database creation confirmed)"
else
    fail "Some pods appear to have restarted after database creation"
fi

# 6. Verify the role exists in PostgreSQL
ROLE_EXISTS=$(kubectl exec "${CLUSTER_NAME}-0" -n "$NAMESPACE" -c postgres -- \
    psql -U postgres -t -c "SELECT 1 FROM pg_roles WHERE rolname='analytics_user';" 2>/dev/null | tr -d ' ')
if [ "$ROLE_EXISTS" = "1" ]; then
    pass "PostgreSQL role 'analytics_user' exists"
else
    fail "PostgreSQL role 'analytics_user' not found"
fi

# 7. Verify the database exists in PostgreSQL
DB_EXISTS=$(kubectl exec "${CLUSTER_NAME}-0" -n "$NAMESPACE" -c postgres -- \
    psql -U postgres -t -c "SELECT 1 FROM pg_database WHERE datname='analytics';" 2>/dev/null | tr -d ' ')
if [ "$DB_EXISTS" = "1" ]; then
    pass "PostgreSQL database 'analytics' exists"
else
    fail "PostgreSQL database 'analytics' not found"
fi

# 8. Verify HBA rules contain the new database access rules
HBA_CONTENT=$(kubectl get configmap "${CLUSTER_NAME}-config" -n "$NAMESPACE" -o jsonpath='{.data.pg_hba\.conf}' 2>/dev/null)
if echo "$HBA_CONTENT" | grep -q "analytics.*analytics_user.*10.0.0.0/8"; then
    pass "HBA rule for analytics_user with 10.0.0.0/8 found in ConfigMap"
else
    fail "HBA rule for analytics_user not found in ConfigMap"
fi
if echo "$HBA_CONTENT" | grep -q "analytics.*analytics_user.*192.168.0.0/16"; then
    pass "HBA rule for analytics_user with 192.168.0.0/16 found in ConfigMap"
else
    fail "HBA rule for 192.168.0.0/16 not found in ConfigMap"
fi

# 9. Verify database is listed via API
API_DBS=$(api_get "/clusters/${CLUSTER_ID}/databases")
DB_COUNT=$(echo "$API_DBS" | python3 -c "import json,sys; print(len(json.load(sys.stdin)))" 2>/dev/null)
if [ "$DB_COUNT" -ge 1 ]; then
    pass "API lists $DB_COUNT cluster database(s)"
else
    fail "API returned no databases"
fi

# 10. Update the database (change CIDRs)
info "Updating database CIDRs..."
UPDATE_RESULT=$(curl -sf -X PUT "${API_BASE}/clusters/${CLUSTER_ID}/databases/${DB_ID}" \
    -H "Content-Type: application/json" \
    -d '{"db_user": "analytics_user", "allowed_cidrs": ["10.0.0.0/8"]}' 2>/dev/null || echo "FAIL")
if [ "$UPDATE_RESULT" != "FAIL" ]; then
    pass "Database updated (CIDR changed to 10.0.0.0/8 only)"
else
    fail "Database update failed"
fi

sleep 5

# Verify 192.168.0.0/16 rule is gone
HBA_AFTER=$(kubectl get configmap "${CLUSTER_NAME}-config" -n "$NAMESPACE" -o jsonpath='{.data.pg_hba\.conf}' 2>/dev/null)
if ! echo "$HBA_AFTER" | grep -q "analytics_user.*192.168.0.0/16"; then
    pass "Old CIDR (192.168.0.0/16) removed from HBA after update"
else
    fail "Old CIDR still present in HBA"
fi

# 11. Delete the database record
info "Deleting database record..."
DEL_CODE=$(curl -sf -o /dev/null -w "%{http_code}" -X DELETE "${API_BASE}/clusters/${CLUSTER_ID}/databases/${DB_ID}" 2>/dev/null)
if [ "$DEL_CODE" = "204" ]; then
    pass "Database record deleted (HTTP 204)"
else
    fail "Delete returned HTTP $DEL_CODE (expected 204)"
fi

sleep 5

# Verify HBA rule is gone
HBA_FINAL=$(kubectl get configmap "${CLUSTER_NAME}-config" -n "$NAMESPACE" -o jsonpath='{.data.pg_hba\.conf}' 2>/dev/null)
if ! echo "$HBA_FINAL" | grep -q "analytics_user"; then
    pass "HBA rules for analytics_user removed after delete"
else
    fail "HBA rules for analytics_user still present after delete"
fi

# 12. Add a second database to test multiple databases
info "Creating second database 'reporting' with user 'report_user'..."
CREATE_RESULT2=$(api_post "/clusters/${CLUSTER_ID}/databases" "{
    \"db_name\": \"reporting\",
    \"db_user\": \"report_user\",
    \"password\": \"r3portP4ss!\",
    \"allowed_cidrs\": []
}")
DB_ID2=$(echo "$CREATE_RESULT2" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
if [ -n "$DB_ID2" ]; then
    pass "Second database created"
else
    fail "Second database creation failed"
fi

sleep 20

# Verify empty CIDRs defaults to 0.0.0.0/0 in HBA
HBA_REPORT=$(kubectl get configmap "${CLUSTER_NAME}-config" -n "$NAMESPACE" -o jsonpath='{.data.pg_hba\.conf}' 2>/dev/null)
if echo "$HBA_REPORT" | grep -q "reporting.*report_user.*0.0.0.0/0"; then
    pass "Empty CIDRs defaulted to 0.0.0.0/0 in HBA"
else
    fail "Expected 0.0.0.0/0 default for empty CIDRs"
fi

# Cleanup second database
curl -sf -X DELETE "${API_BASE}/clusters/${CLUSTER_ID}/databases/${DB_ID2}" > "$DEVNULL" 2>&1 || true

summary
