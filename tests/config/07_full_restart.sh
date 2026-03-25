#!/usr/bin/env bash
# 07_full_restart.sh — Test that a wal_level change triggers full cluster restart.
# wal_level is the canonical replication-sensitive parameter: a mismatch between
# primary and replica breaks WAL streaming, so all pods must be stopped before
# the change is applied.

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/helpers.sh"

header "Test: Full Cluster Restart — wal_level change (replica → logical)"

# Wait for all pods to be ready from previous tests
info "Waiting for cluster to be stable..."
wait_for_pods_ready "$NAMESPACE" "pg-swarm.io/cluster=${CLUSTER_NAME}" 3 120
sleep 5

# 1. Record current state
info "Current pod state:"
kubectl get pods -n "$NAMESPACE" -l "pg-swarm.io/cluster=${CLUSTER_NAME}" --no-headers 2>/dev/null

OLD_WAL=$(kubectl exec "${CLUSTER_NAME}-0" -n "$NAMESPACE" -c postgres -- psql -U postgres -t -c "SHOW wal_level;" 2>/dev/null | tr -d ' ')
info "Current wal_level: $OLD_WAL"

# 2. Change wal_level from replica to logical
info "Updating profile: wal_level → logical (requires full restart)..."
CURRENT=$(api_get "/profiles/${PROFILE_ID}")
UPDATED=$(echo "$CURRENT" | python3 -c "
import json, sys
p = json.load(sys.stdin)
p['config']['pg_params']['wal_level'] = 'logical'
print(json.dumps(p))
")
RESULT=$(api_put "/profiles/${PROFILE_ID}" "$UPDATED")

STRATEGY=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('change_impact',{}).get('apply_strategy','none'))" 2>/dev/null)
if [ "$STRATEGY" = "full_restart" ]; then
    pass "Change classified as full_restart"
else
    fail "Expected full_restart, got $STRATEGY"
fi

FULL_CHANGES=$(echo "$RESULT" | python3 -c "
import json, sys
changes = json.load(sys.stdin).get('change_impact',{}).get('full_restart_changes',[])
for c in changes:
    print(f\"  {c['path']}: {c['old_value']} → {c['new_value']}\")
" 2>/dev/null)
info "Full restart changes:
$FULL_CHANGES"

# 3. Apply via per-cluster apply
CLUSTER_ID=$(api_get "/clusters" | python3 -c "
import json, sys
for c in json.load(sys.stdin):
    if c['name'] == '${CLUSTER_NAME}':
        print(c['id'])
        break
" 2>/dev/null)
info "Applying to cluster $CLUSTER_ID (this will do a full cluster restart)..."
APPLY_RESULT=$(api_post "/clusters/${CLUSTER_ID}/apply" '{"confirmed": true}')
info "Apply result: $APPLY_RESULT"

# Verify the cluster config in DB now has wal_level
CLUSTER_WAL=$(api_get "/clusters" | python3 -c "
import json, sys
for c in json.load(sys.stdin):
    if c['name'] == '${CLUSTER_NAME}':
        print(c['config'].get('pg_params',{}).get('wal_level','NOT SET'))
        break
" 2>/dev/null)
info "Cluster config in DB has wal_level=$CLUSTER_WAL"

# 4. Watch for scale-down — poll fast to catch the brief 0-pod window,
#    but also accept satellite log evidence as alternative proof.
info "Watching for full restart evidence..."
SEEN_ZERO=false
for i in $(seq 1 120); do
    POD_COUNT=$(kubectl get pods -n "$NAMESPACE" -l "pg-swarm.io/cluster=${CLUSTER_NAME}" --no-headers 2>/dev/null | wc -l | tr -d ' ')
    if [ "$POD_COUNT" = "0" ]; then
        SEEN_ZERO=true
        pass "Cluster scaled to 0 pods (full shutdown observed)"
        break
    fi
    sleep 0.5
done
if [ "$SEEN_ZERO" = false ]; then
    RESTART_LOG=$(kubectl logs deployment/pg-swarm-satellite -n pgswarm-system 2>/dev/null | grep "full restart: scaling to 0" | tail -1)
    if [ -n "$RESTART_LOG" ]; then
        pass "Full restart confirmed via satellite logs (0-pod window too brief to observe)"
    else
        fail "No evidence of full restart (neither 0-pod observation nor satellite log)"
        kubectl get pods -n "$NAMESPACE" --no-headers 2>/dev/null
    fi
fi

# 5. Wait for pods to come back
info "Waiting for 3 pods to come back up..."
if wait_for_pods_ready "$NAMESPACE" "pg-swarm.io/cluster=${CLUSTER_NAME}" 3 300; then
    pass "All pods back and ready"
else
    fail "Pods did not come back within timeout"
    kubectl get pods -n "$NAMESPACE" --no-headers 2>/dev/null
    summary; exit 1
fi

info "Pod state after full restart:"
kubectl get pods -n "$NAMESPACE" -l "pg-swarm.io/cluster=${CLUSTER_NAME}" --no-headers 2>/dev/null

# 6. Wait for wal_level to propagate to ConfigMap (with retries)
info "Waiting for wal_level=logical to appear in ConfigMap..."
WAL_OK=false
for _retry in $(seq 1 30); do
    CM_WAL=$(kubectl get configmap "${CLUSTER_NAME}-config" -n "$NAMESPACE" -o jsonpath='{.data.postgresql\.conf}' 2>/dev/null | grep "wal_level" || echo "")
    if echo "$CM_WAL" | grep -q "logical"; then
        WAL_OK=true
        break
    fi
    # If not yet propagated, re-push by calling apply again
    if [ "$_retry" = "5" ] || [ "$_retry" = "15" ]; then
        info "Config not propagated yet, re-applying..."
        api_post "/clusters/${CLUSTER_ID}/apply" '{"confirmed": true}' > "$DEVNULL" 2>&1 || true
    fi
    sleep 2
done
if [ "$WAL_OK" = true ]; then
    pass "ConfigMap has wal_level=logical"
else
    info "ConfigMap wal_level: $CM_WAL"
    info "Proto pgParams: $(kubectl get configmap "pg-swarm-minikube-${CLUSTER_NAME}" -n "$NAMESPACE" -o jsonpath='{.data.config\.json}' 2>/dev/null | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('pgParams',{}))" 2>/dev/null || echo 'N/A')"
    info "Satellite versions: $(kubectl logs deployment/pg-swarm-satellite -n pgswarm-system 2>/dev/null | grep 'config applied' | tail -3)"
    fail "wal_level=logical not in ConfigMap after 60s"
fi

# Wait for pods to restart with new config
wait_for_pods_ready "$NAMESPACE" "pg-swarm.io/cluster=${CLUSTER_NAME}" 3 120

# 7. Verify wal_level changed in PostgreSQL
sleep 10
NEW_WAL=$(kubectl exec "${CLUSTER_NAME}-0" -n "$NAMESPACE" -c postgres -- psql -U postgres -t -c "SHOW wal_level;" 2>/dev/null | tr -d ' ')
if [ "$NEW_WAL" = "logical" ]; then
    pass "PostgreSQL confirms: wal_level=$NEW_WAL (was $OLD_WAL)"
else
    fail "PostgreSQL has wal_level=$NEW_WAL (expected logical)"
fi

summary
