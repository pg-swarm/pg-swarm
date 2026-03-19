# pg-swarm

Centralized management system for PostgreSQL High Availability clusters deployed across up to 500 edge Kubernetes clusters.

A cloud-hosted **Central** control plane handles satellite registration, configuration distribution, and fleet-wide health monitoring. A lightweight **Satellite** agent runs on each edge cluster as a Kubernetes operator — constructing PG cluster manifests from JSON configs, performing health checks, and reporting metrics. A **Failover Sidecar** runs alongside each PG pod, managing leader election via Kubernetes Leases and performing automatic promotion, fencing, and demotion — with no central round-trip required.

No dependency on CloudNativePG (CNPG) or any external operator framework. pg-swarm builds StatefulSets, Services, ConfigMaps, Secrets, and RBAC resources from scratch.

> For detailed architecture, protocol definitions, and design decisions, see [DESIGN.md](DESIGN.md).

---

## Objective

Provide a single pane of glass for managing hundreds of PostgreSQL HA clusters at the edge, where:

- Each edge cluster is behind NAT or firewalls and cannot be reached directly
- Clusters must self-heal (automatic failover) without depending on the central control plane
- Configuration changes must propagate reliably from central to all matching satellites
- Operators need deep observability into every PG instance — replication lag, connections, WAL stats, slow queries, per-database cache hit ratios — from a single dashboard

---

## Architecture

```
                    +---------------------------------------------+
                    |           Central Control Plane              |
                    |                                              |
                    |   gRPC Server (:9090)   REST API (:8080)     |
                    |   StreamManager         Web Dashboard        |
                    |              PostgreSQL (metadata)           |
                    +------------------+---------------------------+
                                       | gRPC bidirectional streams
                 +---------------------+---------------------+
                 |                     |                     |
          +------+--------+    +------+--------+    +-------+-------+
          |  Satellite A  |    |  Satellite B  |    |  Satellite N  |
          |               |    |               |    |               |
          | Stream        |    | Stream        |    | Stream        |
          | Connector     |    | Connector     |    | Connector     |
          | Operator      |    | Operator      |    | Operator      |
          | Health Monitor|    | Health Monitor|    | Health Monitor|
          |               |    |               |    |               |
          | Per PG Pod:    |    |               |    |               |
          | +--+ +--+ +--+|   |               |    |               |
          | |FS| |BS| |PG||   |               |    |               |
          | +--+ +--+ +--+|   |               |    |               |
          |  K8s Edge      |   |  K8s Edge     |    |  K8s Edge     |
          +----------------+   +---------------+    +---------------+
                 FS = Failover Sidecar, BS = Backup Sidecar, PG = postgres
```

---

## Modules

### Central Control Plane (`cmd/central`)

The central server hosts three interfaces on a single binary:

| Interface | Port | Purpose |
|-----------|------|---------|
| gRPC Server | 9090 | Bidirectional streaming with satellites (config push, health ingestion, events) |
| REST API (GoFiber v2) | 8080 | 30+ endpoints for satellites, clusters, profiles, deployment rules, health, events, PG versions |
| Web Dashboard | 8080 | Embedded React 19 SPA served alongside the REST API |

**Key internal packages:**

- **`internal/central/server/`** — gRPC handlers (StreamManager, auth interceptors) and REST API routes
- **`internal/central/store/`** — `Store` interface with 40+ methods, PostgreSQL implementation (pgxpool), embedded SQL migration runner
- **`internal/central/registry/`** — Satellite registration and approval workflow
- **`internal/central/auth/`** — Token generation (32 bytes, `crypto/rand`) and SHA-256 hash verification

### Satellite Agent (`cmd/satellite`)

A single Go binary deployed per edge Kubernetes cluster. Maintains a persistent gRPC connection to central with automatic reconnection (exponential backoff, 1s to 30s).

- **`internal/satellite/agent/`** — Lifecycle management: identity persistence (K8s Secret), registration, approval polling
- **`internal/satellite/stream/`** — Persistent bidirectional gRPC stream with heartbeats (10s), message dispatch
- **`internal/satellite/operator/`** — Kubernetes operator that reconciles `ClusterConfig` messages into native K8s resources:

  | Resource | Name Pattern | Purpose |
  |----------|-------------|---------|
  | StatefulSet | `{name}` | N postgres pods with init + main + sidecar containers |
  | Headless Service | `{name}-headless` | Stable pod DNS for replication |
  | RW Service | `{name}-rw` | Routes to primary via `pg-swarm.io/role=primary` selector |
  | RO Service | `{name}-ro` | Routes to replicas via `pg-swarm.io/role=replica` selector |
  | ConfigMap | `{name}-config` | postgresql.conf + pg_hba.conf |
  | Secret | `{name}-secret` | Superuser, replication, and app DB passwords (create-only) |
  | ServiceAccount + Role + RoleBinding | `{name}-failover` | RBAC for the failover sidecar |

- **`internal/satellite/health/`** — 10-second collection loop per cluster: `pg_isready`, replication lag (bytes + seconds), connections, disk usage, WAL stats, per-database sizes and cache hit ratios, table stats, slow queries (`pg_stat_statements`)
- **`internal/satellite/health/switchover.go`** — Planned switchover orchestration (central-initiated): verify target, checkpoint, fence old primary, transfer lease, promote

### Failover Sidecar (`cmd/failover-sidecar`)

Runs as a container alongside each postgres pod in the StatefulSet. Operates autonomously — no dependency on the satellite or central for automatic failover.

- **`internal/failover/monitor.go`** — Tick loop (1s default):
  - **Primary path**: Acquire/renew Kubernetes Coordination Lease, label pod, detect split-brain (fence + demote)
  - **Replica path**: Label pod, monitor WAL receiver health, check primary reachability (TCP connect to RW service), detect timeline divergence, trigger `pg_rewind` / re-basebackup for recovery, attempt promotion if leader lease expires
- **`internal/shared/pgfence/`** — SQL fencing (`ALTER SYSTEM SET default_transaction_read_only`, reload, kill client connections) and unfencing. Idempotent and shared between sidecar and switchover handler.

### Backup Sidecar (`cmd/backup-sidecar`)

Runs as a container alongside each postgres pod when backup profiles are attached. Detects its role (primary vs replica) via `pg_is_in_recovery()` and activates the appropriate responsibilities.

- **`internal/backup/`** — Backup sidecar package:
  - **Primary**: WAL archiving (HTTP API on :8442 receives WAL from `archive_command`), SQLite metadata DB (`backups.db`), retention
  - **Replica**: Scheduled base backups (`pg_basebackup`), incremental backups (`pg_basebackup --incremental`), logical backups (`pg_dump`/`pg_dumpall`)
  - **Role switching**: On failover detection, automatically switches responsibilities
  - **Destinations**: S3, GCS, SFTP, local filesystem via `internal/backup/destination/` interface
  - **Status reporting**: Writes to `{cluster}-backup-status` ConfigMap for the health monitor

> For detailed backup architecture, see [BACKUP.md](BACKUP.md).

### Profiles and Deployment Rules

- **Profiles** — Reusable cluster templates (PG version, storage, resources, PG params, HBA rules, failover, WAL archiving, databases). Can be cloned and locked (immutable after deployment).
- **Deployment Rules** — Map a profile (WHAT) to satellites by label selector (WHERE). When a rule is created or a new satellite matches, cluster configs are auto-created and pushed.

### Web Dashboard (`web/dashboard`)

React 19 SPA built with Vite and JSX, embedded into the central binary via Go's `embed.FS`.

| Page | Purpose |
|------|---------|
| Overview | Stat cards, recent activity |
| Satellites | Approve/reject, label editing, state badges |
| Profiles | 6-tab editor (General, Volumes, Resources, PostgreSQL params, HBA Rules, Databases) |
| Deployment Rules | Rule CRUD with expandable cluster lists |
| Clusters | Instance table, disk/WAL breakdown, database sizes, cache hit ratios, slow queries, switchover |
| Events | Event log with severity filtering |
| Admin | PostgreSQL version registry |

### Protobuf (`api/proto/v1/`)

| File | Contents |
|------|----------|
| `common.proto` | Shared enums: `SatelliteState`, `ClusterState`, `InstanceRole` |
| `registration.proto` | `Register` + `CheckApproval` unary RPCs |
| `config.proto` | `SatelliteStreamService.Connect` bidirectional streaming, `ClusterConfig`, `SwitchoverRequest` |
| `health.proto` | `ClusterHealthReport`, `InstanceHealth` with WAL stats, table stats, slow queries, database stats |

---

## Current State

### What works

- Full satellite lifecycle: registration, approval, identity persistence, persistent gRPC streaming with reconnection
- Config push from central to satellites via bidirectional gRPC stream
- Kubernetes operator builds complete PG clusters from JSON configs (StatefulSet, Services, ConfigMap, Secret, RBAC)
- Automatic failover via per-pod sidecar: leader lease election, split-brain detection with SQL fencing, demotion via K8s exec
- Automatic replica recovery after failover: timeline divergence detection, `pg_rewind` with `pg_basebackup` fallback
- Planned switchover (central-initiated): checkpoint, fence, lease transfer, promote
- Rich health monitoring: replication lag, connections, disk, WAL stats, per-database cache hit ratios, table stats, slow queries (`pg_stat_statements`)
- Backup sidecar: role-aware (primary/replica), WAL archiving via HTTP, scheduled base/incremental/logical backups, SQLite metadata, retention
- Profiles and deployment rules for fleet-scale management
- Web dashboard with 7 pages and 10-second auto-refresh
- Docker Compose for local development
- Kubernetes deployment via Kustomize (base + minikube overlay)

### What's being stabilized

- End-to-end failover under various failure scenarios (network partitions, simultaneous pod failures)
- Replica recovery timing after timeline divergence

---

## Quick Start

### Prerequisites

- Go 1.26+
- [buf](https://buf.build/) (protobuf generation)
- Node.js 18+ (dashboard build)
- Docker and Docker Compose (local dev)
- minikube (integration testing)

### Local Development (Docker Compose)

```bash
cd deploy/docker
docker-compose up -d
```

This starts PostgreSQL (metadata store), the central server (gRPC :9090, REST :8080), and a satellite agent. Open `http://localhost:8080` for the dashboard.

### Build from Source

```bash
make build        # Compile all three binaries (runs proto + dashboard first)
make test         # Unit tests
make lint         # golangci-lint
```

### Kubernetes (minikube)

```bash
make minikube-build-all        # Build images into minikube's Docker
make k8s-deploy-all            # Deploy central + satellite via Kustomize
make k8s-status                # Show all pgswarm-system resources
```

### Key Make Targets

| Target | Description |
|--------|-------------|
| `make build` | Compile central, satellite, failover-sidecar, backup-sidecar binaries |
| `make test` | Run unit tests |
| `make test-integration` | Integration tests against minikube (requires real cluster) |
| `make manifests` | Regenerate operator golden-file test YAMLs |
| `make proto` | Regenerate protobuf Go code |
| `make dashboard` | Build React dashboard |
| `make docker-build-all` | Build all Docker images |
| `make docker-push-all` | Push images to registry |
| `make k8s-deploy-all` | Deploy to Kubernetes via Kustomize |

---

## Configuration

### Central

| Environment Variable | Default | Description |
|---------------------|---------|-------------|
| `PG_HOST`, `PG_PORT`, `PG_USER`, `PG_PASSWORD`, `PG_DATABASE` | localhost defaults | PostgreSQL metadata store connection |
| `GRPC_ADDR` | `:9090` | gRPC listen address |
| `HTTP_ADDR` | `:8080` | REST API + dashboard listen address |

### Satellite

| Environment Variable | Default | Description |
|---------------------|---------|-------------|
| `CENTRAL_ADDR` | `localhost:9090` | Central gRPC address |
| `K8S_CLUSTER_NAME` | *(required)* | Kubernetes cluster identifier |
| `REGION` | `""` | Geographic region label |
| `DEFAULT_FAILOVER_IMAGE` | `ghcr.io/pg-swarm/pg-swarm-failover:latest` | Failover sidecar image |

### Failover Sidecar

| Environment Variable | Source | Description |
|---------------------|--------|-------------|
| `POD_NAME` | Downward API | Pod identity for lease acquisition |
| `POD_NAMESPACE` | Downward API | Namespace for lease and pod operations |
| `CLUSTER_NAME` | Config | Derives lease name (`{cluster}-leader`) |
| `POSTGRES_PASSWORD` | Secret | Connect to local PG |
| `REPLICATION_PASSWORD` | Secret | Set `primary_conninfo` on demotion/recovery |
| `HEALTH_CHECK_INTERVAL` | Config | Tick interval in seconds (default: 1) |
| `PRIMARY_HOST` | Config | RW service DNS for direct primary reachability check |

---

## Tech Stack

| Layer | Technology |
|-------|-----------|
| Language | Go 1.26 |
| Communication | gRPC with bidirectional streaming (protobuf v3) |
| Central database | PostgreSQL via pgx/v5 (pgxpool) |
| REST API | GoFiber v2 |
| Logging | zerolog |
| Web dashboard | React 19 + Vite + JSX, lucide-react icons |
| Kubernetes client | client-go v0.35 |
| Build | buf (protobuf), Make, Docker |
| Deployment | Docker Compose, Kustomize |

---

## Project Structure

```
pg-swarm/
├── cmd/
│   ├── central/main.go              # Central control plane
│   ├── satellite/main.go            # Satellite agent
│   ├── failover-sidecar/main.go     # Failover sidecar
│   └── backup-sidecar/main.go       # Backup sidecar
├── api/
│   ├── proto/v1/                    # Protobuf definitions
│   └── gen/v1/                      # Generated Go code
├── internal/
│   ├── central/
│   │   ├── server/                  # gRPC + REST handlers
│   │   ├── store/                   # Store interface + PostgreSQL impl + migrations
│   │   ├── registry/                # Registration + approval
│   │   └── auth/                    # Token generation + hashing
│   ├── satellite/
│   │   ├── agent/                   # Agent lifecycle + registration
│   │   ├── stream/                  # gRPC stream connector
│   │   ├── operator/                # K8s manifest builders + reconcile
│   │   └── health/                  # Health monitor + switchover
│   ├── failover/                    # Leader election, fencing, demotion, recovery
│   ├── backup/                      # Backup sidecar (WAL archiving, backups, metadata, retention)
│   │   └── destination/             # Storage backends (S3, GCS, SFTP, local)
│   └── shared/
│       ├── models/                  # Shared Go types
│       └── pgfence/                 # SQL fencing utilities
├── web/
│   ├── embed.go                     # Go embed for static assets
│   └── dashboard/                   # React SPA (Vite + JSX)
├── deploy/
│   ├── docker/                      # Dockerfiles + docker-compose
│   └── k8s/                         # Kustomize manifests (base + minikube overlay)
├── DESIGN.md                        # Detailed architecture document
└── Makefile
```

---

## Roadmap

### Security Hardening

- **mTLS between central and satellites** — Central acts as a certificate authority, issuing client certificates during the satellite approval flow. Satellites present client certs on `Connect`. Certificate rotation via stream renegotiation. Eliminates reliance on bearer tokens for stream authentication.
- **REST API authentication** — Add token-based or OIDC authentication to the REST API and dashboard. Currently the API is unauthenticated.
- **RBAC for dashboard users** — Role-based access control for operators vs. read-only viewers.
- **Secret encryption at rest** — Encrypt sensitive fields (tokens, passwords) in the central PostgreSQL database beyond hash storage.
- **Satellite identity rotation** — Periodic rotation of satellite auth tokens with zero-downtime handoff.
- **Audit logging** — Record who approved satellites, changed profiles, triggered switchovers, and when.

### Backup and Recovery

- ~~**Automated base backups**~~ Done — backup sidecar runs scheduled `pg_basebackup` on the replica
- ~~**Continuous WAL archiving**~~ Done — sidecar receives WAL via `archive_command` and uploads to S3/GCS/SFTP/local
- ~~**Incremental backups**~~ Done — `pg_basebackup --incremental` with manifest chaining and standby WAL fallback
- ~~**Logical backups**~~ Done — scheduled `pg_dump`/`pg_dumpall` with gzip compression
- ~~**Backup metadata**~~ Done — SQLite database (`backups.db`) at each destination with chain reconstruction queries
- ~~**Retention**~~ Done — configurable retention by set count, automatic cascade delete of files + metadata
- **Point-in-time recovery (PITR)** — Central-initiated PITR to a specific timestamp: provision a new cluster from backup + WAL replay (restore Job exists, full PITR flow to be completed).
- **Cross-cluster restore** — Restore a backup from one satellite's cluster onto a different satellite.
- **Backup verification** — Periodic restore-to-temp and `pg_checksums` validation to ensure backups are usable.

### Automatic Recovery Drills

- **Scheduled failover drills** — Central-initiated automatic failover exercises on a configurable schedule (e.g., weekly). Kill the primary, verify promotion, verify replica re-sync, report pass/fail.
- **Recovery time measurement** — Track and report RTO (recovery time objective) for each drill: time from primary death to new primary accepting writes.
- **Replica divergence drills** — Deliberately create timeline divergence scenarios and verify that `pg_rewind` / re-basebackup recovery succeeds automatically.
- **Network partition simulation** — Test split-brain detection and fencing under simulated network partitions between pods.
- **Backup restore drills** — Periodically restore the latest backup to a temporary cluster, run validation queries, and report integrity.
- **Drill history and reporting** — Dashboard page showing drill results over time, mean RTO trends, and failure analysis.
- **Alerting on drill failures** — Webhook or email notifications when a recovery drill fails, indicating degraded HA capability.

---

## License

Licensed under the [Apache License, Version 2.0](LICENSE).
