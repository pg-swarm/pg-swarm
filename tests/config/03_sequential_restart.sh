#!/usr/bin/env bash
# 03_sequential_restart.sh — Test per-cluster apply with a reload-class change.
# statement_timeout is sighup context → applied via pg_reload_conf, no pod restart.

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/helpers.sh"

header "Test: Per-Cluster Apply — Reload without restart"

# 1. Record current pod ages
info "Current pod state before apply:"
kubectl get pods -n "$NAMESPACE" -l "pg-swarm.io/cluster=${CLUSTER_NAME}" --no-headers 2>/dev/null

# 2. Get cluster ID
CLUSTER_ID=$(api_get "/clusters" | python3 -c "
import json, sys
for c in json.load(sys.stdin):
    if c['name'] == '${CLUSTER_NAME}':
        print(c['id'])
        break
" 2>/dev/null)
info "Cluster ID: $CLUSTER_ID"

# 3. Apply the profile change to this cluster
info "Calling POST /clusters/:id/apply..."
RESULT=$(api_post "/clusters/${CLUSTER_ID}/apply" '{"confirmed": true}')
STATUS=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('status',''))" 2>/dev/null)
if [ "$STATUS" = "in_progress" ]; then
    pass "Apply accepted — status=in_progress"
else
    fail "Apply failed" "$RESULT"
    summary; exit 1
fi

# 4. Wait for config to propagate (ConfigMap update + pg_reload_conf)
info "Waiting for config to propagate (15s)..."
sleep 15

# 5. Verify ConfigMap updated
CM_VALUE=$(kubectl get configmap "${CLUSTER_NAME}-config" -n "$NAMESPACE" -o jsonpath='{.data.postgresql\.conf}' 2>/dev/null | grep "statement_timeout" || echo "NOT_FOUND")
if echo "$CM_VALUE" | grep -q "45s"; then
    pass "ConfigMap updated: $CM_VALUE"
else
    fail "ConfigMap not updated with statement_timeout=45s" "$CM_VALUE"
fi

# 6. Wait for all pods to be fully Ready
EXPECTED=$(kubectl get statefulset "${CLUSTER_NAME}" -n "$NAMESPACE" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo 3)
wait_for_pods_ready "$NAMESPACE" "pg-swarm.io/cluster=${CLUSTER_NAME}" "$EXPECTED" 120

# 7. Verify PostgreSQL has the new value
PG_VALUE=$(kubectl exec "${CLUSTER_NAME}-0" -n "$NAMESPACE" -c postgres -- psql -U postgres -t -c "SHOW statement_timeout;" 2>/dev/null | tr -d ' ')
if [ "$PG_VALUE" = "45s" ]; then
    pass "PostgreSQL confirms: statement_timeout=$PG_VALUE"
else
    fail "PostgreSQL has unexpected value: $PG_VALUE (expected 45s)"
fi

info "Pod state after apply:"
kubectl get pods -n "$NAMESPACE" -l "pg-swarm.io/cluster=${CLUSTER_NAME}" --no-headers 2>/dev/null

summary
