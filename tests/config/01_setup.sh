#!/usr/bin/env bash
# 01_setup.sh — Deploy pg-swarm, approve satellite, create test cluster.
# Precondition: minikube running, no pg-swarm deployments.

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/helpers.sh"

header "Setup: Deploy pg-swarm and create test cluster"

# 1. Build images (skip with --no-build or SKIP_BUILD=true)
if [ "${SKIP_BUILD:-false}" = true ]; then
    info "Skipping image build (--no-build)"
else
    info "Building images for minikube..."
    make -C "$SCRIPT_DIR/../.." minikube-build-all > "$DEVNULL" 2>&1
    pass "Images built"
fi

# 2. Deploy
info "Deploying central + satellite..."
make -C "$SCRIPT_DIR/../.." k8s-deploy-all > "$DEVNULL" 2>&1
pass "Deployed to minikube"

# 3. Wait for pods
info "Waiting for central and satellite pods..."
sleep 10
if wait_for_pods_ready pgswarm-system "app=pg-swarm-central" 1 60 && \
   wait_for_pods_ready pgswarm-system "app=pg-swarm-satellite" 1 60; then
    pass "Central and satellite pods ready"
else
    fail "Pods not ready within timeout"
    summary; exit 1
fi

# 4. Port forward
pkill -f "port-forward.*8080" 2>/dev/null || true
sleep 1
kubectl port-forward svc/pg-swarm-central-http -n pgswarm-system 8080:8080 > "$DEVNULL" 2>&1 &
sleep 3

# 5. Wait for satellite to register, then approve
info "Waiting for satellite to register..."
SAT_ID=""
for _attempt in $(seq 1 30); do
    SAT_ID=$(api_get "/satellites" 2>/dev/null | python3 -c "
import json, sys
sats = json.load(sys.stdin) or []
if sats:
    print(sats[0]['id'])
" 2>/dev/null || true)
    if [ -n "$SAT_ID" ]; then
        break
    fi
    sleep 2
done
if [ -z "$SAT_ID" ]; then
    fail "Satellite did not register within 60 seconds"
    summary; exit 1
fi
info "Approving satellite..."
RESULT=$(api_post "/satellites/${SAT_ID}/approve" '{"name":"minikube"}')
if echo "$RESULT" | python3 -c "import json,sys; d=json.load(sys.stdin); assert 'auth_token' in d" 2>/dev/null; then
    pass "Satellite approved (id: ${SAT_ID})"
else
    fail "Satellite approval failed" "$RESULT"
    summary; exit 1
fi

# 6. Create test cluster
info "Creating test cluster from dev profile..."
PROFILE_CONFIG=$(api_get "/profiles/${PROFILE_ID}" | python3 -c "import json,sys; print(json.dumps(json.load(sys.stdin)['config']))")
RESULT=$(api_post "/clusters" "{
    \"name\": \"${CLUSTER_NAME}\",
    \"namespace\": \"${NAMESPACE}\",
    \"satellite_id\": \"${SAT_ID}\",
    \"profile_id\": \"${PROFILE_ID}\",
    \"config\": ${PROFILE_CONFIG}
}")
CLUSTER_ID=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['id'])" 2>/dev/null)
if [ -n "$CLUSTER_ID" ]; then
    pass "Cluster created (id: ${CLUSTER_ID})"
else
    fail "Cluster creation failed" "$RESULT"
    summary; exit 1
fi

# 7. Wait for cluster pods
info "Waiting for cluster pods (this may take up to 2 minutes)..."
EXPECTED_REPLICAS=$(echo "$PROFILE_CONFIG" | python3 -c "import json,sys; print(json.load(sys.stdin).get('replicas', 1))")
if wait_for_pods_ready "$NAMESPACE" "pg-swarm.io/cluster=${CLUSTER_NAME}" "$EXPECTED_REPLICAS" 180; then
    PODS=$(kubectl get pods -n "$NAMESPACE" --no-headers 2>/dev/null | wc -l | tr -d ' ')
    pass "All $PODS cluster pods ready"
else
    fail "Cluster pods not ready within timeout"
    kubectl get pods -n "$NAMESPACE" 2>&1
    summary; exit 1
fi

# Save state for other tests
echo "$SAT_ID" > /tmp/pgswarm-test-sat-id
echo "$CLUSTER_ID" > /tmp/pgswarm-test-cluster-id

summary
