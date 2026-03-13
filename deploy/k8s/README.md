# pg-swarm Kubernetes Deployment

Manifests for deploying pg-swarm to a Kubernetes cluster. Tested on minikube. All resources live in the `pgswarm-system` namespace.

## File Structure

```
deploy/k8s/
├── kustomization.yaml          # Kustomize entry point — applies everything in order
├── namespace.yaml              # pgswarm-system namespace
├── postgres/
│   ├── secret.yaml             # DB credentials (username, password, database)
│   ├── statefulset.yaml        # postgres:17-alpine, single replica, 2Gi PVC
│   └── service.yaml            # ClusterIP service + headless service
├── central/
│   ├── configmap.yaml          # GRPC_ADDR, HTTP_ADDR
│   ├── deployment.yaml         # Central deployment + ServiceAccount
│   │                           # init-container waits for postgres readiness
│   └── service.yaml            # Two NodePort services (30080 HTTP, 30090 gRPC)
└── satellite/
    ├── rbac.yaml               # ServiceAccount + ClusterRole + ClusterRoleBinding
    ├── configmap.yaml          # CENTRAL_ADDR, K8S_CLUSTER_NAME, REGION
    └── deployment.yaml         # Satellite deployment + 10Mi PVC for identity
```

## Prerequisites

- [minikube](https://minikube.sigs.k8s.io/) ≥ 1.32
- `kubectl` configured to point at your minikube cluster
- `make` (for Makefile targets)
- Docker (for building images)

## Quick Start

### 1. Start minikube

```bash
minikube start
```

### 2. Build images into minikube's Docker daemon

This loads the images directly into minikube without needing a registry:

```bash
make minikube-build
```

This runs `eval $(minikube docker-env)` and builds both images tagged as `ghcr.io/pg-swarm/pg-swarm-central:latest` and `ghcr.io/pg-swarm/pg-swarm-satellite:latest`.

> `imagePullPolicy: IfNotPresent` is set on both deployments so Kubernetes uses the locally built images rather than trying to pull from a registry.

### 3. Deploy everything

```bash
make k8s-deploy
# or
kubectl apply -k deploy/k8s/
```

This applies all manifests in dependency order via Kustomize.

### 4. Watch rollout

```bash
make k8s-status
# or
kubectl get all -n pgswarm-system
```

Wait until all pods show `Running`:

```
NAME                                    READY   STATUS    RESTARTS
pod/pg-swarm-central-6d8f9b7c4-xk2nt   1/1     Running   0
pod/pg-swarm-satellite-7b4d6c9f-lm8pq  1/1     Running   0
pod/postgres-0                          1/1     Running   0
```

### 5. Access the REST API

```bash
# Get the NodePort URL from minikube
minikube service pg-swarm-central-http -n pgswarm-system --url
# → http://192.168.49.2:30080

# List satellites
curl http://$(minikube ip):30080/api/v1/satellites
```

### 6. Approve the satellite

The satellite registers on first boot and waits for manual approval:

```bash
# Check satellite logs
kubectl logs -n pgswarm-system -l app=pg-swarm-satellite -f

# Get the satellite ID
SATELLITE_ID=$(curl -s http://$(minikube ip):30080/api/v1/satellites | \
  kubectl run -i --rm --restart=Never jq --image=ghcr.io/jqlang/jq:latest -- -r '.[0].id' 2>/dev/null)

# Or just grab it directly
curl -s http://$(minikube ip):30080/api/v1/satellites | python3 -c \
  "import sys,json; print(json.load(sys.stdin)[0]['id'])"

# Approve
curl -X POST http://$(minikube ip):30080/api/v1/satellites/${SATELLITE_ID}/approve
```

After approval the satellite logs will show:

```
approved by central
satellite agent started
connected to central stream
```

## Resource Summary

### Namespace

All resources are created in `pgswarm-system`.

### PostgreSQL

| Resource | Detail |
|----------|--------|
| Kind | `StatefulSet` (1 replica) |
| Image | `postgres:17-alpine` |
| Storage | 2Gi `PersistentVolumeClaim` via `volumeClaimTemplates` |
| Credentials | `postgres-credentials` Secret (`pgswarm/pgswarm`) |
| Internal DNS | `postgres.pgswarm-system.svc.cluster.local:5432` |
| Readiness | `pg_isready -U pgswarm` (5s interval) |

### Central

| Resource | Detail |
|----------|--------|
| Kind | `Deployment` (1 replica) |
| Image | `ghcr.io/pg-swarm/pg-swarm-central:latest` |
| Init container | Waits for `pg_isready` before starting |
| gRPC service | `NodePort 30090` → container port `9090` |
| HTTP service | `NodePort 30080` → container port `8080` |
| Config | `central-config` ConfigMap + `postgres-credentials` Secret |

`DATABASE_URL` is assembled at pod startup from the `postgres-credentials` secret fields, so credentials are never hardcoded in ConfigMaps.

### Satellite

| Resource | Detail |
|----------|--------|
| Kind | `Deployment` (1 replica) |
| Image | `ghcr.io/pg-swarm/pg-swarm-satellite:latest` |
| Identity storage | 10Mi `PersistentVolumeClaim` mounted at `/var/lib/pg-swarm` |
| HOSTNAME | Injected from `spec.nodeName` (the K8s node name) |
| Config | `satellite-config` ConfigMap |
| Service account | `pg-swarm-satellite` with `ClusterRole` (see RBAC below) |

### RBAC

The satellite `ClusterRole` grants permissions needed for current and upcoming phases:

| API Group | Resources | Verbs | Phase |
|-----------|-----------|-------|-------|
| `apps` | `statefulsets` | full | Phase 3 — PG cluster management |
| `` (core) | `services` | full | Phase 3 — headless/rw/ro services |
| `` (core) | `configmaps` | full | Phase 3 — postgresql.conf |
| `` (core) | `secrets` | full | Phase 3 — superuser/replication passwords |
| `` (core) | `pods` | get, list, watch | Phase 4 — health checks |
| `` (core) | `pods/exec` | create | Phase 5 — `pg_ctl promote` |
| `` (core) | `persistentvolumeclaims` | get, list, watch, create, delete | Phase 3 — storage |
| `` (core) | `events` | create, patch | Phase 5 — event reporting |

## Customisation

### Change the cluster name

Edit `deploy/k8s/satellite/configmap.yaml`:

```yaml
data:
  K8S_CLUSTER_NAME: "my-edge-cluster-name"
```

### Change DB credentials

Edit `deploy/k8s/postgres/secret.yaml`. The `central/deployment.yaml` reads credentials from the same secret, so only one file needs updating:

```yaml
stringData:
  username: myuser
  password: mysecretpassword
  database: pgswarm
```

### Use a different image tag

```bash
# Edit the image field in central/deployment.yaml and satellite/deployment.yaml
# Or use a Kustomize image override:
kubectl kustomize deploy/k8s/ | \
  sed 's|ghcr.io/pg-swarm/pg-swarm-central:latest|myregistry.io/pg-swarm-central:v0.2.0|g' | \
  kubectl apply -f -
```

### Point satellite at an external central

If the satellite runs on a different cluster, set `CENTRAL_ADDR` in `satellite/configmap.yaml` to the external address of central:

```yaml
data:
  CENTRAL_ADDR: "central.example.com:9090"
```

## Tear Down

```bash
make k8s-delete
# or
kubectl delete -k deploy/k8s/
```

This removes all resources including PersistentVolumeClaims. To retain data, delete individual resources manually instead.

## Troubleshooting

**Central pod stuck in `Init:0/1`**

The init container is waiting for postgres. Check postgres pod status:
```bash
kubectl describe pod -n pgswarm-system postgres-0
kubectl logs -n pgswarm-system postgres-0
```

**Satellite stuck in `waiting for approval`**

Expected behaviour — the satellite needs a manual approval via the REST API (see step 6 above).

**ImagePullBackOff**

Images were not loaded into minikube. Re-run:
```bash
make minikube-build
```

**`pg_isready` fails in init container**

Postgres may still be initialising. The init container retries every 2 seconds indefinitely. Give it 30–60 seconds on first boot when the PVC is being provisioned.
