# pg-swarm

Centralized management system for PostgreSQL HA clusters across edge Kubernetes clusters.

## Architecture

- **Central** (`cmd/central`): gRPC server (:9090) + REST API (:8080, 60+ endpoints) + embedded React dashboard + WebSocket hub
- **Satellite** (`cmd/satellite`): Lightweight agent on each edge K8s cluster, sidecar streaming, log capture
- **Sentinel sidecar** (`cmd/sentinel-sidecar`): Per-pod sidecar for leader election, promotion, log watcher, sidecar streaming
- **Protobuf** definitions in `api/proto/v1/`, generated code in `api/gen/v1/`

## Build & Test

```bash
make build             # Compile all binaries (runs proto + dashboard first)
make test              # Unit tests only
make test-integration  # Integration tests against minikube (only when asked)
make manifests         # Regenerate operator testdata YAMLs
make lint              # golangci-lint
make proto             # Regenerate protobuf Go code (requires buf)
make dashboard         # Build React dashboard
```

## Rules

- **Do not run integration tests** (`make test-integration`) unless explicitly asked. They hit a real minikube cluster and take ~50s.
- **Manifests must include TypeMeta**: All K8s resource builders must set `TypeMeta` with `apiVersion` and `kind`. Go typed structs don't populate these automatically.
- **Secrets are create-only**: `createOrPreserveSecret` never overwrites an existing secret — passwords must survive config updates.
- **VolumeClaimTemplates are immutable**: The operator warns on VCT changes but cannot apply them to existing StatefulSets.
- **REST API uses GoFiber v2**, not chi (DESIGN.md is outdated on this).
- **Web dashboard is JSX** (React + Vite), not TypeScript as DESIGN.md states.
- **Satellite identity is stored in a K8s Secret**, not a file on disk.

## Key Paths

- `internal/central/` — Central control plane (server, store, registry, auth)
- `internal/central/server/ws.go` — WebSocket hub for real-time updates
- `internal/central/server/ops_tracker.go` — Active operation tracking
- `internal/satellite/` — Satellite agent (operator, stream connector, registration)
- `internal/satellite/sidecar/` — Sidecar streaming (gRPC server for sentinel sidecars)
- `internal/satellite/logcapture/` — Satellite log capture and forwarding
- `internal/satellite/operator/tombstone.go` — Cluster deletion markers
- `internal/sentinel/` — Failover monitor (leader lease, pg_promote, log watcher, sidecar connector)
- `internal/sentinel/logwatcher.go` — Real-time PG log monitoring (40+ recovery patterns)
- `internal/sentinel/connector.go` — Bidirectional gRPC streaming to satellite
- `internal/shared/models/` — Shared Go types
- `dashboard/` — React SPA + Go embed (10 pages)
- `deploy/docker/` — Dockerfiles + docker-compose
- `deploy/k8s/` — Kubernetes manifests (kustomize)
