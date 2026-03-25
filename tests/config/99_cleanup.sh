#!/usr/bin/env bash
# 08_cleanup.sh — Delete test cluster and clean up resources.

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/helpers.sh"

header "Cleanup: Remove test cluster and resources"

# 1. Delete the test cluster via API
CLUSTER_ID=$(cat /tmp/pgswarm-test-cluster-id 2>/dev/null || echo "")
if [ -n "$CLUSTER_ID" ]; then
    info "Deleting cluster ${CLUSTER_ID}..."
    curl -sf -X DELETE "${API_BASE}/clusters/${CLUSTER_ID}" > "$DEVNULL" 2>&1 || true
    pass "Cluster delete requested"
else
    info "No cluster ID found, skipping API delete"
fi

# 2. Wait for pods to terminate
info "Waiting for test namespace pods to terminate..."
for i in $(seq 1 30); do
    POD_COUNT=$(kubectl get pods -n "$NAMESPACE" --no-headers 2>/dev/null | wc -l | tr -d ' ')
    if [ "$POD_COUNT" = "0" ]; then
        pass "All test pods terminated"
        break
    fi
    sleep 2
done

# 3. Kill port-forward
pkill -f "port-forward.*8080" 2>/dev/null || true
pass "Port-forward stopped"

# 4. Clean up temp files
rm -f /tmp/pgswarm-test-sat-id /tmp/pgswarm-test-cluster-id
pass "Temp files cleaned"

summary
