# pg-swarm: Design Document

## 1. Overview

pg-swarm is a centralized management system for PostgreSQL High Availability (HA) clusters deployed across up to 500 edge Kubernetes clusters. A cloud-hosted **Central** control plane handles registration, configuration distribution, and health monitoring. A **Satellite** agent runs on each edge cluster as a lightweight Kubernetes operator — constructing PG cluster manifests from JSON configs, performing health checks, and orchestrating failover. A **Failover Sidecar** runs alongside each PG pod, managing leader election via Kubernetes Leases and performing automatic promotion and demotion.

**No dependency on CloudNativePG (CNPG).** pg-swarm builds StatefulSets, Services, ConfigMaps, Secrets, and RBAC resources from scratch.

### Design Goals

| Goal | Approach |
|------|----------|
| Scalable to 500 edge clusters | Bidirectional gRPC streaming; one persistent connection per satellite |
| Minimal edge footprint | Single Go binary per edge cluster; no CRDs or operator frameworks |
| Centralized visibility | All health and events stream to central; REST API + web dashboard for ops |
| Automated failover | Per-pod sidecar with Lease-based leader election; no central round-trip required |
| Split-brain prevention | SQL fencing + K8s exec demotion; old primary converted to standby |
| Planned switchover | Central-initiated primary switch with fencing and WAL catch-up |
| Secure by default | Token-based auth with SHA-256 hashing; mTLS planned for Phase 7 |

### Tech Stack

- **Language**: Go
- **Communication**: gRPC with bidirectional streaming (protobuf v3)
- **Central database**: PostgreSQL (via pgx/v5)
- **REST API**: GoFiber v2
- **Logging**: zerolog
- **Web dashboard**: React 19 + JSX (Vite), lucide-react icons
- **Build**: buf (protobuf), Make, Docker

---

## 2. Architecture

```
                    ┌─────────────────────────────────────────────┐
                    │              Central Control Plane          │
                    │                                             │
                    │  ┌──────────┐  ┌──────────┐  ┌──────────┐  │
                    │  │ gRPC     │  │ REST API │  │ Web      │  │
                    │  │ Server   │  │ (Fiber)  │  │ Dashboard│  │
                    │  │ :9090    │  │ :8080    │  │ (React)  │  │
                    │  └────┬─────┘  └────┬─────┘  └──────────┘  │
                    │       │             │                       │
                    │  ┌────┴─────────────┴─────┐                │
                    │  │     StreamManager       │                │
                    │  │  map[UUID]*SatStream    │                │
                    │  └────────────┬────────────┘                │
                    │               │                             │
                    │  ┌────────────┴────────────┐                │
                    │  │      PostgreSQL          │               │
                    │  │  satellites, configs,    │               │
                    │  │  profiles, rules,        │               │
                    │  │  health, events          │               │
                    │  └─────────────────────────┘                │
                    └──────────┬──────────────────────────────────┘
                               │ gRPC bidi streams
            ┌──────────────────┼──────────────────┐
            │                  │                  │
     ┌──────┴──────────┐ ┌────┴────────────┐ ┌───┴─────────────┐
     │  Satellite A    │ │  Satellite B    │ │  Satellite N    │
     │                 │ │                 │ │                 │
     │ ┌─────────────┐ │ │ ┌─────────────┐ │ │ ┌─────────────┐ │
     │ │ Stream      │ │ │ │ Stream      │ │ │ │ Stream      │ │
     │ │ Connector   │ │ │ │ Connector   │ │ │ │ Connector   │ │
     │ ├─────────────┤ │ │ ├─────────────┤ │ │ ├─────────────┤ │
     │ │ Operator    │ │ │ │ Operator    │ │ │ │ Operator    │ │
     │ ├─────────────┤ │ │ ├─────────────┤ │ │ ├─────────────┤ │
     │ │ Health      │ │ │ │ Health      │ │ │ │ Health      │ │
     │ │ Monitor     │ │ │ │ Monitor     │ │ │ │ Monitor     │ │
     │ └─────────────┘ │ │ └─────────────┘ │ │ └─────────────┘ │
     │                 │ │                 │ │                 │
     │  Per PG Pod:    │ │                 │ │                 │
     │ ┌─────────────┐ │ │                 │ │                 │
     │ │ Failover    │ │ │                 │ │                 │
     │ │ Sidecar     │ │ │                 │ │                 │
     │ │ (per-pod)   │ │ │                 │ │                 │
     │ └─────────────┘ │ │                 │ │                 │
     │   K8s Edge      │ │   K8s Edge      │ │   K8s Edge      │
     └─────────────────┘ └─────────────────┘ └─────────────────┘
```

---

## 3. Project Structure

```
pg-swarm/
├── cmd/
│   ├── central/main.go              # Central control plane entrypoint
│   ├── satellite/main.go            # Satellite agent entrypoint
│   └── failover-sidecar/main.go     # Failover sidecar entrypoint
├── api/
│   ├── proto/v1/                    # Protobuf definitions
│   │   ├── common.proto             # Shared enums (SatelliteState, ClusterState, InstanceRole)
│   │   ├── registration.proto       # Register + CheckApproval RPCs
│   │   ├── config.proto             # Bidirectional streaming + ClusterConfig messages
│   │   └── health.proto             # Health reports, events, database/table/query stats
│   └── gen/v1/                      # Generated Go code (buf generate)
├── internal/
│   ├── central/
│   │   ├── server/
│   │   │   ├── grpc.go              # gRPC server, StreamManager, auth interceptors
│   │   │   ├── grpc_health_test.go  # Health report gRPC tests
│   │   │   └── rest.go              # REST API (GoFiber v2, 30+ endpoints)
│   │   ├── store/
│   │   │   ├── store.go             # Store interface (40+ methods)
│   │   │   ├── postgres.go          # PostgreSQL implementation (pgxpool)
│   │   │   ├── migrate.go           # Embedded SQL migration runner
│   │   │   └── migrations/          # SQL migration files
│   │   ├── registry/registry.go     # Registration + approval logic
│   │   └── auth/tokens.go           # Token generation/hashing (SHA-256)
│   ├── satellite/
│   │   ├── agent/
│   │   │   ├── agent.go             # Main agent lifecycle
│   │   │   └── registration.go      # Register with central + approval polling
│   │   ├── stream/connector.go      # Persistent gRPC stream + reconnect
│   │   ├── operator/                # K8s operator — manifest builders + reconcile
│   │   │   ├── operator.go          # Main reconcile loop
│   │   │   ├── reconcile_helpers.go  # Create/update/delete K8s resources
│   │   │   ├── labels.go            # Label management (cluster, profile, role)
│   │   │   ├── manifest_statefulset.go  # StatefulSet + init/main/sidecar containers
│   │   │   ├── manifest_service.go  # Headless, RW, RO services
│   │   │   ├── manifest_configmap.go  # postgresql.conf + pg_hba.conf
│   │   │   ├── manifest_secret.go   # Password generation + create-only semantics
│   │   │   ├── manifest_rbac.go     # ServiceAccount, Role, RoleBinding for failover
│   │   │   └── manifest_test.go     # Golden-file manifest tests
│   │   └── health/
│   │       ├── monitor.go           # Health checker + reporter (10s interval)
│   │       ├── monitor_test.go      # Health monitor tests
│   │       └── switchover.go        # Planned switchover orchestration
│   ├── failover/
│   │   ├── monitor.go               # Sidecar: Lease election, fencing, demotion
│   │   └── monitor_test.go          # Split-brain + lease tests
│   └── shared/
│       ├── models/models.go         # Shared Go types (ClusterState, etc.)
│       └── pgfence/
│           ├── fence.go             # SQL fencing (read-only + kill connections)
│           └── fence_test.go        # Fence/unfence unit tests
├── web/
│   ├── embed.go                     # Go embed for static dashboard assets
│   └── dashboard/                   # React SPA (Vite)
│       └── src/
│           ├── App.jsx              # Router setup (7 routes)
│           ├── main.jsx             # React entry point
│           ├── api.js               # REST API client + helpers
│           ├── index.css            # Global styles (CSS variables, responsive)
│           ├── components/
│           │   ├── Layout.jsx       # Topbar, nav with icons, status pill
│           │   └── Badge.jsx        # State badges with lucide-react icons
│           ├── context/
│           │   ├── DataContext.jsx   # Global data provider (10s auto-refresh)
│           │   └── ToastContext.jsx  # Toast notification system
│           └── pages/
│               ├── Overview.jsx     # Stat cards, recent activity
│               ├── Satellites.jsx   # Satellite table, approve/reject, labels
│               ├── Profiles.jsx     # Profile editor (6-tab form, PG params)
│               ├── DeploymentRules.jsx  # Rule CRUD, cluster list per rule
│               ├── Clusters.jsx     # Cluster cards, instance table, detail modal
│               ├── Events.jsx       # Event log with severity icons
│               └── Admin.jsx        # PostgreSQL version registry
├── deploy/
│   ├── docker/                      # Dockerfiles + docker-compose
│   └── k8s/                         # Kubernetes manifests (kustomize)
├── buf.yaml / buf.gen.yaml          # Buf protobuf config
├── go.mod
├── Makefile
├── CLAUDE.md                        # AI assistant instructions
└── DESIGN.md                        # This file
```

---

## 4. Communication Protocol

### 4.1 gRPC Services

Two gRPC services define the satellite-to-central communication:

**RegistrationService** — unary RPCs, unauthenticated (server-TLS only):

```protobuf
service RegistrationService {
  rpc Register(RegisterRequest) returns (RegisterResponse);
  rpc CheckApproval(CheckApprovalRequest) returns (CheckApprovalResponse);
}
```

**SatelliteStreamService** — bidirectional streaming, authenticated:

```protobuf
service SatelliteStreamService {
  rpc Connect(stream SatelliteMessage) returns (stream CentralMessage);
}
```

### 4.2 Message Flow

**Upstream (Satellite → Central):**

| Message | Purpose | Frequency |
|---------|---------|-----------|
| `Heartbeat` | Keep-alive with timestamp | Every 10 seconds |
| `ClusterHealthReport` | Per-cluster health with instance details | Every 10 seconds |
| `EventReport` | Significant events (failover, errors) | On occurrence |
| `ConfigAck` | Acknowledge config receipt with success/error | On config push |

**Downstream (Central → Satellite):**

| Message | Purpose | Trigger |
|---------|---------|---------|
| `ClusterConfig` | Full cluster specification to deploy | REST create/update |
| `DeleteCluster` | Remove a cluster | REST delete |
| `HeartbeatAck` | Acknowledge heartbeat | On heartbeat |
| `SwitchoverRequest` | Initiate planned primary switch | REST API |

### 4.3 ClusterConfig (Core Data Contract)

The `ClusterConfig` message defines everything the satellite needs to construct a PG cluster:

```protobuf
message ClusterConfig {
  string cluster_name = 1;
  string namespace = 2;
  int32 replicas = 3;
  PostgresSpec postgres = 4;     // version, image, registry
  StorageSpec storage = 5;       // size, storage_class
  ResourceSpec resources = 6;    // cpu/mem requests and limits
  map<string, string> pg_params = 7;  // postgresql.conf overrides
  repeated HbaRule hba_rules = 8;     // pg_hba.conf rules (structured)
  int64 config_version = 9;           // monotonically increasing
  FailoverSpec failover = 10;         // enabled, sidecar_image, interval
  WalStorageSpec wal_storage = 11;    // separate WAL volume
  ArchiveSpec archive = 12;           // WAL archiving (pvc or custom)
  bool deletion_protection = 13;      // PVC finalizers
  repeated DatabaseSpec databases = 14; // application databases
  string profile_name = 15;          // originating profile name
  map<string, string> label_selector = 16; // satellite label matching
}
```

Config versions are auto-incremented by the central store on each update, enabling satellites to detect stale or duplicate pushes.

### 4.4 Health Report (Rich Observability)

Each instance health report includes comprehensive PostgreSQL metrics:

```protobuf
message InstanceHealth {
  string pod_name = 1;
  InstanceRole role = 2;              // primary, replica
  bool ready = 3;                     // pg_isready
  int64 replication_lag_bytes = 4;    // WAL byte lag
  double replication_lag_seconds = 6; // time-based lag
  int32 connections_used = 7;
  int32 connections_max = 8;
  int64 disk_used_bytes = 9;          // sum of pg_database_size()
  int64 timeline_id = 10;
  Timestamp pg_start_time = 11;       // pg_postmaster_start_time()
  bool wal_receiver_active = 12;      // streaming status

  // WAL statistics (pg_stat_wal)
  int64 wal_records = 13;
  int64 wal_bytes = 14;
  int64 wal_buffers_full = 15;

  // Per-table statistics (pg_stat_user_tables, per-database)
  repeated TableStat table_stats = 16;

  // WAL directory on-disk size (pg_ls_waldir)
  int64 wal_disk_bytes = 17;

  // Per-database sizes + cache hit ratio
  repeated DatabaseStat database_stats = 18;

  // Slow queries (pg_stat_statements, top 10 by mean_exec_time)
  repeated SlowQuery slow_queries = 19;
}
```

The satellite health monitor connects to each user database individually to collect per-database table stats and cache hit ratios from `pg_statio_user_tables`. Slow queries require `pg_stat_statements` extension; the monitor gracefully skips this if the extension is not loaded.

---

## 5. Security Model

### 5.1 Authentication Flow

```
 Satellite                          Central                           Admin
    │                                  │                                │
    │──── Register(hostname,...) ─────>│                                │
    │<─── satellite_id + temp_token ───│                                │
    │                                  │                                │
    │                                  │<─ POST /satellites/{id}/approve|
    │                                  │──> auth_token (returned) ─────>│
    │                                  │                                │
    │── CheckApproval(id, temp) ──────>│                                │
    │<── approved=true, auth_token ────│                                │
    │                                  │                                │
    │── Connect(auth: auth_token) ────>│                                │
    │<════════ bidi stream ═══════════>│                                │
```

### 5.2 Token Security

- **Generation**: 32 bytes from `crypto/rand`, hex-encoded (64 characters)
- **Storage**: Only SHA-256 hashes stored in database (`auth_token_hash`, `temp_token_hash`)
- **Validation**: Hash the presented token and compare with stored hash
- **Transport**: Token passed in gRPC `authorization` metadata header

### 5.3 gRPC Auth Interceptors

Two interceptors protect the gRPC server:

1. **Unary interceptor**: Allows `Register` and `CheckApproval` RPCs without authentication. All other unary RPCs (if added) would require auth.

2. **Stream interceptor**: Extracts the `authorization` token from gRPC metadata, hashes it, looks up the satellite via `GetSatelliteByToken`, and injects the satellite ID into the stream context using a `wrappedServerStream`.

### 5.4 Identity Persistence

The satellite stores its identity in a Kubernetes Secret (`pg-swarm-identity` in the satellite's namespace), not on disk. This survives pod restarts and rescheduling.

### 5.5 Future: mTLS (Phase 7)

- Central acts as CA, issuing client certificates during approval
- Satellite presents client cert on Connect
- Certificate rotation via stream renegotiation

---

## 6. Central Components

### 6.1 Store Layer

The `Store` interface defines 40+ methods across 7 domains:

| Domain | Methods | Key Details |
|--------|---------|-------------|
| Satellites | 10 | CRUD, token lookup, heartbeat, labels, storage classes, label selector query |
| Cluster Configs | 7 | CRUD, query by satellite, pause/resume |
| Profiles | 6 | CRUD, lock (immutable after deployment) |
| Deployment Rules | 7 | CRUD, query by profile, query clusters by rule |
| Postgres Versions | 7 | CRUD, set default, query by version+variant |
| Health | 4 | Upsert health, update config state from health, query |
| Events | 3 | Create, list with limit, filter by cluster |

**PostgreSQL implementation details:**
- Connection pooling via `pgxpool.Pool`
- JSONB columns for flexible data: `labels`, `config`, `instances`, `storage_classes`
- Parameterized queries throughout (no SQL injection risk)
- `ON CONFLICT (satellite_id, cluster_name) DO UPDATE` for health upserts
- Automatic `config_version + 1` increment on config updates
- Health report syncs cluster config state (`creating` → `running`) — skips `paused`/`deleting` states
- Compile-time interface satisfaction check: `var _ Store = (*PostgresStore)(nil)`

### 6.2 Database Schema

```sql
┌──────────────────┐       ┌──────────────────┐       ┌──────────────┐
│   satellites      │       │ cluster_configs   │       │   events      │
├──────────────────┤       ├──────────────────┤       ├──────────────┤
│ id (PK)          │◄──FK──│ satellite_id      │   ┌──│ satellite_id  │
│ hostname          │       │ id (PK)          │   │  │ cluster_name  │
│ k8s_cluster_name  │       │ name             │   │  │ severity      │
│ region            │       │ namespace        │   │  │ message       │
│ labels JSONB      │       │ profile_id ──────┼──►│  │ source        │
│ storage_classes   │       │ deployment_rule_id│   │  │ created_at    │
│ state             │       │ config JSONB     │   │  └──────────────┘
│ auth_token_hash   │       │ config_version   │   │
│ temp_token_hash   │       │ state            │   │  ┌──────────────────┐
│ last_heartbeat    │       │ paused           │   │  │ cluster_health    │
│ created_at        │       │ UQ(name,sat_id)  │   │  ├──────────────────┤
│ updated_at        │       └──────────────────┘   ├──│ satellite_id (PK) │
└──────────────────┘                               │  │ cluster_name (PK) │
                                                   │  │ state             │
┌──────────────────┐       ┌──────────────────┐   │  │ instances JSONB   │
│ cluster_profiles  │       │ deployment_rules  │   │  │ updated_at        │
├──────────────────┤       ├──────────────────┤   │  └──────────────────┘
│ id (PK)          │◄──FK──│ profile_id        │   │
│ name (UQ)        │       │ id (PK)          │   │  ┌──────────────────┐
│ description      │       │ name             │   │  │ postgres_versions │
│ config JSONB     │       │ label_selector   │   │  ├──────────────────┤
│ locked           │       │ namespace        │   │  │ id (PK)          │
│ created_at       │       │ cluster_name     │   │  │ version          │
│ updated_at       │       │ UQ(name)         │   │  │ variant          │
└──────────────────┘       └──────────────────┘   │  │ image_tag        │
                                                      │ is_default       │
                                                      │ UQ(version,var)  │
                                                      └──────────────────┘
```

### 6.3 Migration System

Migrations use Go's `embed.FS` to bundle SQL files into the binary:

1. Creates a `schema_migrations` table on first run
2. Reads `migrations/*.sql` files, sorted alphabetically
3. Checks which migrations have already been applied
4. Runs each new migration in a transaction (rollback on error)
5. Records the migration version after successful execution

### 6.4 StreamManager

The `StreamManager` maintains a thread-safe map of all connected satellite streams:

```go
type StreamManager struct {
    mu      sync.RWMutex
    streams map[uuid.UUID]*SatelliteStream
}

type SatelliteStream struct {
    SatelliteID uuid.UUID
    SendCh      chan *CentralMessage  // buffered, cap=64
    Cancel      context.CancelFunc
}
```

**Config push flow:**
1. Admin creates/updates config via REST API
2. REST handler calls `StreamManager.PushConfig(satelliteID, config)`
3. PushConfig sends the proto message to the satellite's `SendCh`
4. The Connect handler's write loop reads from `SendCh` and calls `stream.Send()`
5. Satellite receives the config and processes it

**Health report processing:**
1. Satellite sends `ClusterHealthReport` via stream
2. gRPC handler converts proto to model and calls `UpsertClusterHealth`
3. After upsert, calls `UpdateClusterConfigState` to sync config state (e.g. `creating` → `running`)
4. Skips state update if config is `paused` or `deleting` (user-controlled states)

The 64-message buffer prevents slow satellites from blocking the REST API. If the buffer is full, PushConfig returns an error (logged, not propagated to the API caller).

### 6.5 REST API

All endpoints are under `/api/v1` using GoFiber v2:

| Method | Path | Description |
|--------|------|-------------|
| **Satellites** | | |
| GET | `/satellites` | List all satellites with derived state |
| POST | `/satellites/{id}/approve` | Approve pending satellite → returns auth_token |
| POST | `/satellites/{id}/reject` | Reject pending satellite |
| PUT | `/satellites/{id}/labels` | Update satellite labels |
| POST | `/satellites/{id}/refresh-storage-classes` | Trigger storage class discovery |
| **Clusters** | | |
| GET | `/clusters` | List all cluster configs |
| POST | `/clusters` | Create cluster config → triggers push |
| GET | `/clusters/{id}` | Get single cluster config |
| PUT | `/clusters/{id}` | Update cluster config → triggers push |
| DELETE | `/clusters/{id}` | Delete cluster config |
| POST | `/clusters/{id}/pause` | Pause cluster (stops reconciliation) |
| POST | `/clusters/{id}/resume` | Resume paused cluster |
| POST | `/clusters/{id}/switchover` | Initiate planned primary switchover |
| **Profiles** | | |
| GET | `/profiles` | List all profiles |
| POST | `/profiles` | Create profile |
| GET | `/profiles/{id}` | Get profile |
| PUT | `/profiles/{id}` | Update profile |
| DELETE | `/profiles/{id}` | Delete profile |
| POST | `/profiles/{id}/clone` | Clone profile with new name |
| POST | `/profiles/{id}/lock` | Lock profile (immutable) |
| **Deployment Rules** | | |
| GET | `/deployment-rules` | List rules |
| POST | `/deployment-rules` | Create rule → auto-creates cluster configs |
| GET | `/deployment-rules/{id}` | Get rule |
| PUT | `/deployment-rules/{id}` | Update rule |
| DELETE | `/deployment-rules/{id}` | Delete rule |
| GET | `/deployment-rules/{id}/clusters` | List clusters created by rule |
| **Health & Events** | | |
| GET | `/health` | List all cluster health reports |
| GET | `/events?limit=N` | List recent events (default limit: 100) |
| **Admin** | | |
| GET | `/postgres-versions` | List PG version registry |
| POST | `/postgres-versions` | Add PG version |
| PUT | `/postgres-versions/{id}` | Update PG version |
| DELETE | `/postgres-versions/{id}` | Delete PG version |
| POST | `/postgres-versions/{id}/default` | Set default PG version |

---

## 7. Satellite Components

### 7.1 Agent Lifecycle

```
 ┌──────────────────────────────────────────────────┐
 │                  Satellite Agent                   │
 │                                                    │
 │  1. Load identity from K8s Secret                  │
 │     └─ Not found? Register + poll for approval     │
 │        └─ Save identity Secret on approval         │
 │                                                    │
 │  2. Connect persistent gRPC stream                 │
 │     └─ Exponential backoff on disconnect           │
 │                                                    │
 │  3. Operator loop                                  │
 │     └─ Receive configs → reconcile K8s resources   │
 │                                                    │
 │  4. Health monitor                                 │
 │     └─ Every 10s: PG metrics + per-DB stats        │
 │                                                    │
 │  5. Switchover handler                             │
 │     └─ Central-initiated planned primary switch    │
 └──────────────────────────────────────────────────┘
```

### 7.2 Registration Flow

1. Satellite calls `Register` RPC with hostname, K8s cluster name, region, and labels
2. Central creates a satellite record in `pending` state, returns `satellite_id` + `temp_token`
3. Satellite polls `CheckApproval` every 5 seconds with `satellite_id` + `temp_token`
4. Admin approves via REST API: `POST /api/v1/satellites/{id}/approve`
5. Next `CheckApproval` poll returns `approved=true` + `auth_token`
6. Satellite saves identity to K8s Secret and proceeds to stream connection

### 7.3 Stream Connector

The stream connector maintains a persistent bidirectional gRPC connection with automatic reconnection:

**Reconnection strategy:**
- Initial backoff: 1 second
- Exponential increase: backoff x 2 each failure
- Maximum backoff: 30 seconds
- Backoff resets implicitly on successful connection

**Connection setup:**
1. Dial central with `authorization` token in gRPC metadata
2. Call `Connect` RPC to establish bidirectional stream
3. Start heartbeat goroutine (sends every 10 seconds)
4. Enter read loop to process incoming messages

**Message dispatch:**
- `ClusterConfig` → Operator reconcile
- `DeleteCluster` → Operator delete
- `HeartbeatAck` → debug log
- `SwitchoverRequest` → Switchover handler

### 7.4 PG Cluster Manifest Construction

From a `ClusterConfig`, the operator builds these Kubernetes resources:

| Resource | Name Pattern | Purpose |
|----------|-------------|---------|
| StatefulSet | `{name}` | N postgres pods with init + main + sidecar containers |
| Headless Service | `{name}-headless` | Stable pod DNS for replication |
| RW Service | `{name}-rw` | Routes to primary (`role=primary` selector) |
| RO Service | `{name}-ro` | Routes to replicas (`role=replica` selector) |
| ConfigMap | `{name}-config` | postgresql.conf + pg_hba.conf |
| Secret | `{name}-secret` | Superuser + replication + app DB passwords |
| ConfigMap | `{name}-store` | Central connection info for health reporting |
| ServiceAccount | `{name}-failover` | Identity for failover sidecar |
| Role | `{name}-failover` | pods (get,patch), pods/exec (create), leases (get,create,update) |
| RoleBinding | `{name}-failover` | Binds role to service account |

**Init container logic:**
- Ordinal 0 (primary): runs `initdb`, creates replication user, creates app databases
- Ordinal > 0 (replica): runs `pg_basebackup` from primary with `-R` flag

**StatefulSet containers:**
1. `pg-init` — init container for first-boot setup
2. `postgres` — main PG container with liveness/readiness probes
3. `failover` — sidecar for leader election and automatic failover (when enabled)

**Volume management:**
- `data` VCT — primary PGDATA volume
- `wal` VCT — separate WAL volume (optional, from `wal_storage` config)
- `wal-archive` VCT — WAL archive volume (optional, for PVC archive mode)
- `config` volume — mounted ConfigMap for postgresql.conf/pg_hba.conf
- `secret` volume — mounted Secret for passwords

**Secrets are create-only** (`createOrPreserveSecret`): passwords are generated once and never overwritten on config updates. This prevents password rotation from breaking running clusters.

**VolumeClaimTemplates are immutable**: the operator warns on VCT changes but cannot apply them to existing StatefulSets (K8s limitation).

**Pod role labels** (`pg-swarm.io/role=primary` / `pg-swarm.io/role=replica`) drive service routing via label selectors. The failover sidecar manages these labels.

### 7.5 Health Monitor

The satellite health monitor runs a 10-second collection loop per cluster:

```
 Health Monitor (10s tick)
    │
    ├─ pg_isready check
    ├─ pg_is_in_recovery() → determine role
    ├─ Replication lag (bytes + seconds)
    ├─ Connection count vs max_connections
    ├─ Disk usage: sum(pg_database_size())
    ├─ WAL on-disk size: sum(pg_ls_waldir())
    ├─ Timeline ID
    ├─ PG start time
    ├─ WAL receiver status (replicas)
    ├─ WAL statistics (pg_stat_wal)
    ├─ Per-database:
    │   ├─ Database sizes
    │   ├─ Cache hit ratio (pg_statio_user_tables)
    │   └─ Table stats (connect to each DB individually)
    ├─ Slow queries (pg_stat_statements, top 10)
    └─ Derive cluster state → stream to central
```

**Cluster state derivation:**
- All replicas ready AND primary ready → `RUNNING`
- Otherwise → `DEGRADED`

**Per-database collection**: The monitor connects to each user database separately (up to 5) because `pg_stat_user_tables` only shows tables in the current database.

**Slow queries**: Requires `pg_stat_statements` extension. The monitor gracefully skips this if the extension is not available. Internal health-check queries are filtered out.

### 7.6 Planned Switchover

The satellite handles central-initiated switchover requests:

```
 Switchover (Central → Satellite)
    │
    ├─ 1. Verify target pod exists and is a replica
    ├─ 2. Find current primary pod
    ├─ 3. Verify target is streaming and caught up
    ├─ 4. CHECKPOINT on primary (flush WAL)
    ├─ 4b. Fence old primary (block writes + kill connections)
    ├─ 5. Transfer leader lease to target pod
    ├─ 6. pg_promote() on target replica
    └─ 7. Report success to central
```

After switchover, the old primary's failover sidecar detects the split-brain condition on its next tick and automatically demotes PG to a standby (see Section 8).

---

## 8. Failover Sidecar

The failover sidecar (`cmd/failover-sidecar`) runs as a container alongside each postgres pod in the StatefulSet. It manages leader election using Kubernetes Coordination Leases and handles automatic failover.

### 8.1 Leader Election

Each sidecar contends for a Lease resource (`{cluster}-leader`) in the cluster namespace:

- **Lease duration**: 15 seconds
- **Renewal**: On each tick (every 5 seconds by default), the current holder renews `renewTime`
- **Acquisition**: If the lease is expired or doesn't exist, a replica can acquire it and promote
- **Optimistic locking**: Uses `resourceVersion` to prevent race conditions

### 8.2 Tick Loop

```
 Sidecar Tick (every 5s)
    │
    ├─ Connect to local PG (localhost:5432)
    ├─ SELECT pg_is_in_recovery()
    │
    ├─ If PRIMARY (not in recovery):
    │   ├─ Acquire/renew lease
    │   ├─ If lease acquired → label pod as "primary"
    │   │   └─ If PG was fenced → unfence (ALTER SYSTEM RESET)
    │   ├─ If another pod holds lease → SPLIT-BRAIN:
    │   │   ├─ 1. Fence: ALTER SYSTEM SET read_only + reload + kill clients
    │   │   ├─ 2. Demote: K8s exec into postgres container:
    │   │   │     - Create standby.signal
    │   │   │     - Set primary_conninfo → new primary
    │   │   │     - pg_ctl stop -m fast (K8s restarts → PG comes up as standby)
    │   │   └─ 3. Label pod as "replica"
    │   └─ If lease error → fence only (can't determine new primary)
    │
    └─ If REPLICA (in recovery):
        ├─ Label pod as "replica"
        ├─ Check if leader lease expired
        └─ If expired → acquire lease → pg_promote() → label as "primary"
```

### 8.3 SQL Fencing (`internal/shared/pgfence`)

Fencing is a shared package used by both the failover sidecar and the switchover handler:

**`FencePrimary(ctx, db)`** — three steps, all attempted even if earlier ones fail:
1. `ALTER SYSTEM SET default_transaction_read_only = on` — block writes
2. `SELECT pg_reload_conf()` — apply immediately
3. `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE backend_type = 'client backend'` — kill existing clients, preserve replication

We intentionally do NOT lower `max_connections` because `ALTER SYSTEM` persists in `postgresql.auto.conf`. If PG restarts after demotion with `max_connections=1`, it fails: `superuser_reserved_connections (3) must be less than max_connections (1)`.

**`UnfencePrimary(ctx, db)`** — reverses fencing:
1. `ALTER SYSTEM RESET default_transaction_read_only`
2. `SELECT pg_reload_conf()`

**`IsFenced(ctx, db)`** — checks live `SHOW default_transaction_read_only`. Nil-safe with panic recovery.

Fencing is **idempotent** — double-fencing from both switchover and sidecar is harmless. The `ALTER SYSTEM` settings persist in `postgresql.auto.conf` across restarts, ensuring PG stays read-only until explicitly unfenced.

### 8.4 Demotion via K8s Exec

The sidecar runs in a separate container and cannot access the postgres container's filesystem directly. To demote the old primary, it uses the Kubernetes exec API (`remotecommand.NewSPDYExecutor`) to run a script inside the `postgres` container:

```bash
# Create standby signal file
touch "$PGDATA/standby.signal"

# Set primary_conninfo pointing to the new primary
echo "primary_conninfo = 'host=<new-primary> port=5432 user=repl_user ...'" \
  >> "$PGDATA/postgresql.auto.conf"

# Stop PG — K8s will restart the container and PG will come up as standby
pg_ctl -D "$PGDATA" stop -m fast
```

This requires the `pods/exec` permission in the failover Role.

### 8.5 RBAC

The failover sidecar requires these Kubernetes permissions:

| Resource | Verbs | Purpose |
|----------|-------|---------|
| `pods` | `get`, `patch` | Read pod state, patch role labels |
| `pods/exec` | `create` | Execute demote script in postgres container |
| `leases` | `get`, `create`, `update` | Leader election via Coordination API |

### 8.6 Environment Variables

| Variable | Source | Purpose |
|----------|--------|---------|
| `POD_NAME` | Downward API | Current pod identity |
| `POD_NAMESPACE` | Downward API | Lease/pod namespace |
| `CLUSTER_NAME` | Config | Lease name derivation |
| `POSTGRES_PASSWORD` | Secret | Connect to local PG |
| `REPLICATION_PASSWORD` | Secret | Set primary_conninfo on demotion |
| `HEALTH_CHECK_INTERVAL` | Config | Tick interval in seconds (default: 5) |

---

## 9. Profiles and Deployment Rules

### 9.1 Profiles

A **Profile** is a reusable cluster template stored as JSONB. It defines the full cluster specification: PostgreSQL version, storage, resources, PG parameters, HBA rules, failover settings, WAL archiving, and application databases.

- Profiles can be **cloned** to create variations
- Profiles can be **locked** after first deployment (immutable — prevents accidental changes to running clusters)
- The profile editor in the dashboard has 6 tabs: General, Volumes, Resources, PostgreSQL (extensive parameter catalog with 8 categories), HBA Rules, and Databases

### 9.2 Deployment Rules

A **Deployment Rule** maps a profile (WHAT) to satellites (WHERE):

```
Rule: "prod-analytics-db"
  Profile: "analytics-ha-3node"
  Label Selector: { region: "us-east", tier: "prod" }
  Namespace: "analytics"
  Cluster Name: "analytics"
```

When a rule is created or a new satellite matches the label selector, the central automatically creates a `cluster_config` for each matching satellite and pushes it via the gRPC stream.

### 9.3 PostgreSQL Version Registry

The admin page manages a registry of available PostgreSQL versions (version, variant: alpine/debian, image tag). The default version is pre-selected when creating new profiles.

---

## 10. Web Dashboard

The dashboard is a React 19 SPA built with Vite and JSX (not TypeScript). It is embedded into the Central binary via Go's `embed.FS` and served alongside the REST API.

### 10.1 Pages

| Page | Purpose |
|------|---------|
| **Overview** | Stat cards (satellites, clusters, healthy, events) with icons; recent activity table |
| **Satellites** | Table with approve/reject, label editing, state badges |
| **Profiles** | Grid of profile cards; 6-tab editor with PG parameter catalog |
| **Deployment Rules** | Rule CRUD; expandable cards showing profile summary and created clusters |
| **Clusters** | Card grid with instance table, disk/WAL breakdown, database sizes, cache hit ratio, slow queries, switchover buttons |
| **Events** | Event log with severity icons (info, warning, error, critical) |
| **Admin** | PostgreSQL version registry management |

### 10.2 Architecture

- **DataContext**: Global provider fetching all data every 10 seconds via REST API
- **ToastContext**: Toast notification system (auto-dismiss after 3.5s)
- **Badge component**: Semantic state badges with lucide-react icons (CheckCircle2, Loader, AlertCircle, Pause, XCircle, etc.)
- **Layout**: Sticky topbar with gradient, icon-enhanced navigation, satellite status pill

### 10.3 Cluster Detail Modal

The instance detail modal provides deep visibility into each PG pod:

- **Instance Overview**: Ready state, timeline, connections, replication lag, PG uptime, WAL receiver status
- **Disk Usage**: Data vs WAL bar chart with percentages against volume capacity
- **WAL Statistics**: Records, bytes written, buffers full
- **Databases**: Size, percentage of data, cache hit ratio (color-coded: green >= 99%, amber >= 95%, red < 95%). Clickable drill-down to table stats.
- **Table Stats**: Live/dead tuples, seq/idx scans, inserts/updates/deletes, last vacuum
- **Slow Queries**: Query text, database, calls, avg/max/total time, rows (from `pg_stat_statements`)

### 10.4 Status Indicators

- **PG Status Dot**: Green (ready with uptime tooltip) or blinking red (not ready)
- **Lag Dot**: Green (< 1 min), amber (1-3 min), blinking red (> 3 min)
- **Connection Bar**: Visual bar with color coding (green < 75%, amber 75-90%, red > 90%)

---

## 11. Configuration

### 11.1 Central

| Environment Variable | Default | Description |
|---------------------|---------|-------------|
| `DATABASE_URL` | `postgres://postgres:postgres@localhost:5432/pgswarm?sslmode=disable` | PostgreSQL connection string |
| `GRPC_ADDR` | `:9090` | gRPC listen address |
| `HTTP_ADDR` | `:8080` | REST API + dashboard listen address |

### 11.2 Satellite

| Environment Variable | Default | Required | Description |
|---------------------|---------|----------|-------------|
| `CENTRAL_ADDR` | `localhost:9090` | No | Central gRPC address |
| `HOSTNAME` | OS hostname | No | Satellite hostname |
| `K8S_CLUSTER_NAME` | — | **Yes** | Kubernetes cluster identifier |
| `REGION` | `""` | No | Geographic region label |

### 11.3 Failover Sidecar

See Section 8.6.

---

## 12. Build & Test

```bash
make build             # Compile all binaries (runs proto + dashboard first)
make test              # Unit tests only
make test-integration  # Integration tests against minikube (requires real cluster)
make manifests         # Regenerate operator testdata YAMLs
make lint              # golangci-lint
make proto             # Regenerate protobuf Go code (requires buf)
make dashboard         # Build React dashboard
```

---

## 13. Key Design Decisions

### Why gRPC bidirectional streaming?

Edge clusters may be behind NAT or firewalls. The satellite initiates the connection outward, and the persistent stream allows central to push configs without needing to reach back in. This also provides natural keep-alive semantics via heartbeats.

### Why not use CRDs / operator-sdk?

Minimizing the edge footprint. CRDs require cluster-admin to install, and operator-sdk adds framework weight. By constructing raw manifests (StatefulSet, Service, ConfigMap, Secret), the satellite only needs basic RBAC on a single namespace.

### Why a per-pod failover sidecar instead of satellite-driven failover?

The sidecar runs locally to each pod and uses Kubernetes Leases for leader election. This is faster than satellite-driven failover (no cross-pod exec needed), works even if the satellite agent is down, and provides a clean separation of concerns. The satellite handles planned switchovers; the sidecar handles automatic failover.

### Why SQL fencing before demotion?

Demotion involves creating `standby.signal` and restarting PG, which takes several seconds. During that window, the old primary could accept writes. Fencing (`ALTER SYSTEM SET default_transaction_read_only` + reload + kill client connections) provides immediate write protection, closing the split-brain window to near-zero. We avoid lowering `max_connections` because `ALTER SYSTEM` persists in `postgresql.auto.conf` and PG refuses to start if `max_connections` < `superuser_reserved_connections`.

### Why K8s exec for demotion?

The failover sidecar runs in a separate container from postgres. Creating `standby.signal` and running `pg_ctl stop` requires filesystem access to PGDATA. The Kubernetes exec API (`remotecommand.NewSPDYExecutor`) allows the sidecar to run commands inside the postgres container without sharing volumes.

### Why SHA-256 token hashing (not bcrypt)?

Auth tokens are high-entropy random strings (256 bits), not user-chosen passwords. SHA-256 is sufficient for pre-image resistance on random tokens, and avoids the latency of bcrypt on every stream reconnection.

### Why JSONB for config and labels?

Cluster configurations vary widely (different PG params, HBA rules, resource profiles). JSONB provides schema flexibility while remaining queryable. Labels on satellites enable flexible targeting without schema changes.

### Why sync cluster config state from health reports?

Without this, a cluster's config state stays stuck at `creating` forever after creation. When the satellite reports health (running/degraded/failed), the central now updates `cluster_configs.state` to match — but skips `paused` and `deleting` states which are user-controlled.

### Why buffered send channels (cap=64)?

Decouples the REST API response time from satellite stream throughput. A slow satellite won't block the admin's API call. If the buffer fills (satellite severely behind), the push fails gracefully rather than blocking.

### Why JSX instead of TypeScript?

The dashboard is a relatively straightforward CRUD + monitoring UI. JSX keeps the setup simple with fewer build dependencies. The React 19 + Vite + lucide-react stack provides a modern development experience without TypeScript overhead.

### Why profiles and deployment rules?

Profiles provide reusable, versionable cluster templates. Deployment rules provide declarative "deploy this profile to all satellites matching these labels" semantics. Together they enable fleet-scale management: change a profile, and all clusters using it get updated.
