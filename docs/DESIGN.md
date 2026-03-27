# pg-swarm: Design Document

## 1. Overview

pg-swarm is a centralized management system for PostgreSQL High Availability (HA) clusters deployed across up to 500 edge Kubernetes clusters. A cloud-hosted **Central** control plane handles registration, configuration distribution, and health monitoring. A **Satellite** agent runs on each edge cluster as a lightweight Kubernetes operator — constructing PG cluster manifests from JSON configs, performing health checks, and orchestrating failover. A **Sentinel Sidecar** runs alongside each PG pod, managing leader election via Kubernetes Leases and performing automatic promotion and demotion.

**No CRDs, no external operator frameworks.** pg-swarm builds StatefulSets, Services, ConfigMaps, Secrets, and RBAC resources from scratch.

### Design Goals

| Goal | Approach |
|------|----------|
| Scalable to 500 edge clusters | Bidirectional gRPC streaming; one persistent connection per satellite |
| Minimal edge footprint | Single Go binary per edge cluster; no CRDs or operator frameworks |
| Centralized visibility | All health and events stream to central; REST API + web dashboard for ops |
| Automated failover | Per-pod sidecar with Lease-based leader election; no central round-trip required |
| Split-brain prevention | SQL fencing + K8s exec demotion; old primary converted to standby |
| Planned switchover | Satellite-controlled 9-step orchestration with progress tracking |
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
│   ├── sentinel-sidecar/main.go     # Sentinel sidecar entrypoint
│   └── backup-sidecar/main.go       # Backup sidecar entrypoint
├── api/
│   ├── proto/v1/                    # Protobuf definitions
│   │   ├── common.proto             # Shared enums (SatelliteState, ClusterState, InstanceRole)
│   │   ├── registration.proto       # Register + CheckApproval RPCs
│   │   ├── config.proto             # Bidirectional streaming + ClusterConfig messages
│   │   ├── health.proto             # Health reports, events, database/table/query stats
│   │   └── backup.proto             # BackupConfig, BackupStatusReport, RestoreCommand, destinations
│   └── gen/v1/                      # Generated Go code (buf generate)
├── internal/
│   ├── central/
│   │   ├── server/
│   │   │   ├── grpc.go              # gRPC server, StreamManager, auth interceptors
│   │   │   ├── grpc_health_test.go  # Health report gRPC tests
│   │   │   ├── rest.go              # REST API (GoFiber v2, 60+ endpoints)
│   │   │   ├── ws.go               # WebSocket hub for real-time state push
│   │   │   └── ops_tracker.go      # Active operation tracking with progress updates
│   │   ├── store/
│   │   │   ├── store.go             # Store interface (79 methods)
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
│   │   │   ├── manifest_test.go     # Golden-file manifest tests
│   │   │   └── tombstone.go        # Cluster deletion markers
│   │   ├── sidecar/                 # gRPC server for sentinel sidecar streaming
│   │   │   ├── server.go           # SidecarStreamService gRPC server
│   │   │   └── stream_manager.go   # Sidecar connection lifecycle
│   │   ├── logcapture/             # Satellite log capture and forwarding
│   │   └── health/
│   │       ├── monitor.go           # Health checker + reporter (10s interval)
│   │       ├── monitor_test.go      # Health monitor tests
│   │       └── switchover.go        # Satellite-controlled 9-step switchover
│   ├── failover/
│   │   ├── monitor.go               # Sidecar: Lease election, fencing, demotion
│   │   ├── monitor_test.go          # Split-brain + lease tests
│   │   ├── logwatcher.go            # Real-time PG log monitoring (40+ patterns)
│   │   └── connector.go             # Bidirectional gRPC streaming to satellite
│   ├── backup/                      # Backup sidecar package
│   │   ├── sidecar.go              # Lifecycle, role detection, role switching
│   │   ├── api.go                  # HTTP server (WAL push/fetch, backup/complete, /healthz)
│   │   ├── metadata.go             # SQLite operations (backups.db)
│   │   ├── physical.go             # pg_basebackup (base + incremental)
│   │   ├── logical.go              # pg_dump / pg_dumpall
│   │   ├── scheduler.go            # Cron scheduler for replica backups
│   │   ├── notifier.go             # Replica→primary notification
│   │   ├── reporter.go             # ConfigMap status writer
│   │   ├── retention.go            # Retention policy enforcement
│   │   └── destination/            # Storage backend interface + implementations
│   │       ├── destination.go      # Interface: Upload, Download, List, Delete, Exists
│   │       ├── s3.go, gcs.go, sftp.go, local.go
│   └── shared/
│       ├── models/models.go         # Shared Go types (ClusterState, etc.)
│       └── pgfence/
│           ├── fence.go             # SQL fencing (read-only + kill connections)
│           └── fence_test.go        # Fence/unfence unit tests
├── web/
│   ├── embed.go                     # Go embed for static dashboard assets
│   └── dashboard/                   # React SPA (Vite)
│       └── src/
│           ├── App.jsx              # Router setup (10 routes)
│           ├── main.jsx             # React entry point
│           ├── api.js               # REST API client + helpers
│           ├── index.css            # Global styles (CSS variables, responsive)
│           ├── components/
│           │   ├── Layout.jsx       # Topbar, nav with icons, status pill
│           │   ├── Badge.jsx        # State badges with lucide-react icons
│           │   ├── MiniHeader.jsx   # Compact header for full-page routes
│           │   ├── SwitchoverProgressModal.jsx  # 9-step switchover visualization
│           │   └── EventRulesTab.jsx     # Event rule set editor for Admin
│           ├── context/
│           │   ├── DataContext.jsx   # Global data provider (10s auto-refresh)
│           │   └── ToastContext.jsx  # Toast notification system
│           └── pages/
│               ├── Overview.jsx     # Stat cards, recent activity
│               ├── Satellites.jsx   # Satellite table, approve/reject, labels
│               ├── Profiles.jsx     # Profile editor (6-tab form, PG params)
│               ├── DeploymentRules.jsx  # Rule CRUD, cluster list per rule
│               ├── Clusters.jsx     # Cluster cards, instance table
│               ├── ClusterDetail.jsx  # Full-page cluster view (instances, backups, events)
│               ├── BackupProfiles.jsx  # Backup profile CRUD + destinations
│               ├── Events.jsx       # Event log with severity icons
│               ├── SatelliteLogs.jsx  # Terminal-style log viewer with SSE
│               └── Admin.jsx        # 4 tabs: Storage Tiers, Image Variants, PG Versions, Recovery Rules
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
| `BackupStatusReport` | Backup completion/failure from sidecar | On backup complete |
| `RestoreStatusReport` | Restore completion/failure from sidecar | On restore complete |
| `LogEntry` | Satellite log entry for central buffering | On log emission |
| `SidecarMessage` | Sidecar identity, heartbeat, command results | Via sidecar stream |

**Downstream (Central → Satellite):**

| Message | Purpose | Trigger |
|---------|---------|---------|
| `ClusterConfig` | Full cluster specification to deploy | REST create/update |
| `DeleteCluster` | Remove a cluster | REST delete |
| `HeartbeatAck` | Acknowledge heartbeat | On heartbeat |
| `SwitchoverRequest` | Initiate planned primary switch | REST API |
| `RestoreCommand` | Initiate restore on a cluster | REST API |
| `SetLogLevel` | Change satellite log level remotely | REST API |
| `SidecarCommand` | Fence, checkpoint, promote, unfence, status | Switchover flow |
| `RequestStorageClasses` | Trigger storage class re-discovery | REST API |

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
  BackupConfig backup_config = 17;   // backup sidecar configuration
  string event_rule_set = 18;        // event rule set name
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

### 4.5 SidecarStreamService

Bidirectional streaming RPC between sentinel sidecars and the satellite agent (`internal/satellite/sidecar/`, `internal/sentinel/connector.go`):

```protobuf
service SidecarStreamService {
  rpc Connect(stream SidecarMessage) returns (stream SidecarCommand);
}
```

**Upstream (Sidecar → Satellite):**

| Message | Purpose | Frequency |
|---------|---------|-----------|
| `SidecarIdentity` | Pod name, cluster, namespace on connect | Once on connect |
| `Heartbeat` | Keep-alive | Every 10 seconds |
| `CommandResult` | Result of a dispatched command (success/error, output) | On command completion |

**Downstream (Satellite → Sidecar):**

| Command | Purpose | Trigger |
|---------|---------|---------|
| `fence` | Fence primary (block writes, kill connections) | Switchover step 4 |
| `checkpoint` | Run CHECKPOINT on primary | Switchover step 5 |
| `promote` | Run pg_promote() on target replica | Switchover step 7 |
| `unfence` | Unfence primary (reset read-only) | Rollback/recovery |
| `status` | Query sidecar health and PG state | On demand |

The sidecar maintains a persistent connection with exponential backoff (1s to 30s). The satellite's `StreamManager` maps connected sidecars by pod name for targeted command dispatch during switchover.

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

The `Store` interface defines 79 methods across 13 domains:

| Domain | Methods | Key Details |
|--------|---------|-------------|
| Satellites | 12 | CRUD, token lookup, heartbeat, labels, storage classes, tier mappings, label selector query |
| Cluster Configs | 8 | CRUD, query by satellite/profile, pause/resume |
| Profiles | 7 | CRUD, lock, force delete, touch |
| Deployment Rules | 7 | CRUD, query by profile, query clusters by rule |
| Postgres Versions | 7 | CRUD, set default, query by version+variant |
| Postgres Variants | 3 | List, create, delete |
| Backup Profiles | 9 | CRUD, attach/detach to profiles, list for profile |
| Backup Inventory | 4 | Create, update, list, get |
| Restore Operations | 4 | Create, update, get, list |
| Event Rule Sets | 5 | CRUD |
| Storage Tiers | 6 | CRUD, satellite tier mappings, reassign configs |
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

┌──────────────────┐       ┌──────────────────────┐       ┌──────────────────┐
│ backup_profiles   │       │ profile_backup_       │       │ event_rule_sets  │
├──────────────────┤       │ profiles (junction)   │       │                  │
│ id (PK)          │◄──FK──├──────────────────────┤       ├──────────────────┤
│ name (UQ)        │       │ profile_id ──────────┼──►    │ id (PK)          │
│ config JSONB     │       │ backup_profile_id     │       │ name (UQ)        │
│ created_at       │       └──────────────────────┘       │ rules JSONB      │
│ updated_at       │                                       │ created_at       │
└──────────────────┘                                       │ updated_at       │
                                                           └──────────────────┘
┌──────────────────┐       ┌──────────────────────┐       ┌──────────────────┐
│ storage_tiers     │       │ satellite_tier_       │       │ postgres_        │
├──────────────────┤       │ mappings             │       │ variants         │
│ id (PK)          │◄──FK──├──────────────────────┤       ├──────────────────┤
│ name (UQ)        │       │ satellite_id ────────┼──►    │ id (PK)          │
│ config JSONB     │       │ tier_id              │       │ name (UQ)        │
│ created_at       │       │ storage_class        │       │ image            │
│ updated_at       │       └──────────────────────┘       │ created_at       │
└──────────────────┘                                       └──────────────────┘

┌──────────────────┐       ┌──────────────────┐
│ backup_inventory  │       │ restore_          │
├──────────────────┤       │ operations        │
│ id (PK)          │       ├──────────────────┤
│ satellite_id (FK)│       │ id (PK)          │
│ cluster_name     │       │ cluster_config_id │
│ backup_data JSONB│       │ backup_id         │
│ created_at       │       │ status            │
│ updated_at       │       │ details JSONB     │
└──────────────────┘       │ created_at       │
                           │ updated_at       │
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

63 endpoints under `/api/v1` using GoFiber v2:

| Method | Path | Description |
|--------|------|-------------|
| **WebSocket** | | |
| GET | `/ws` | WebSocket for real-time state push |
| **Satellites** | | |
| GET | `/satellites` | List all satellites with derived state |
| POST | `/satellites/{id}/approve` | Approve pending satellite → returns auth_token |
| POST | `/satellites/{id}/reject` | Reject pending satellite |
| PUT | `/satellites/{id}/labels` | Update satellite labels |
| POST | `/satellites/{id}/refresh-storage-classes` | Trigger storage class discovery |
| PUT | `/satellites/{id}/tier-mappings` | Update satellite storage tier mappings |
| GET | `/satellites/{id}/logs` | Get buffered satellite logs |
| GET | `/satellites/{id}/logs/stream` | SSE stream for real-time satellite logs |
| POST | `/satellites/{id}/log-level` | Change satellite log level remotely |
| **Storage Tiers** | | |
| GET | `/storage-tiers` | List storage tiers |
| POST | `/storage-tiers` | Create storage tier |
| PUT | `/storage-tiers/{id}` | Update storage tier |
| DELETE | `/storage-tiers/{id}` | Delete storage tier |
| **Event Rule Sets** | | |
| GET | `/event-rule-sets` | List event rule sets |
| POST | `/event-rule-sets` | Create event rule set |
| GET | `/event-rule-sets/{id}` | Get event rule set |
| PUT | `/event-rule-sets/{id}` | Update event rule set |
| DELETE | `/event-rule-sets/{id}` | Delete event rule set |
| **Clusters** | | |
| GET | `/clusters` | List all cluster configs |
| POST | `/clusters` | Create cluster config → triggers push |
| GET | `/clusters/{id}` | Get single cluster config |
| PUT | `/clusters/{id}` | Update cluster config → triggers push |
| DELETE | `/clusters/{id}` | Delete cluster config |
| POST | `/clusters/{id}/pause` | Pause cluster (stops reconciliation) |
| POST | `/clusters/{id}/resume` | Resume paused cluster |
| POST | `/clusters/{id}/switchover` | Initiate planned primary switchover |
| GET | `/clusters/{id}/backups` | List backup inventory for cluster |
| POST | `/clusters/{id}/restore` | Initiate restore operation |
| GET | `/clusters/{id}/restores` | List restore operations for cluster |
| **Deployment Rules** | | |
| GET | `/deployment-rules` | List rules |
| POST | `/deployment-rules` | Create rule → auto-creates cluster configs |
| GET | `/deployment-rules/{id}` | Get rule |
| PUT | `/deployment-rules/{id}` | Update rule |
| DELETE | `/deployment-rules/{id}` | Delete rule |
| GET | `/deployment-rules/{id}/clusters` | List clusters created by rule |
| **Profiles** | | |
| GET | `/profiles` | List all profiles |
| POST | `/profiles` | Create profile |
| GET | `/profiles/{id}` | Get profile |
| PUT | `/profiles/{id}` | Update profile |
| DELETE | `/profiles/{id}` | Delete profile |
| GET | `/profiles/{id}/cascade-preview` | Preview cascade delete impact |
| POST | `/profiles/{id}/clone` | Clone profile with new name |
| **Backup Profiles** | | |
| GET | `/backup-profiles` | List backup profiles |
| POST | `/backup-profiles` | Create backup profile |
| GET | `/backup-profiles/{id}` | Get backup profile |
| PUT | `/backup-profiles/{id}` | Update backup profile |
| DELETE | `/backup-profiles/{id}` | Delete backup profile |
| POST | `/profiles/{id}/backup-profiles` | Attach backup profile to profile |
| DELETE | `/profiles/{id}/backup-profiles/{bpId}` | Detach backup profile from profile |
| GET | `/profiles/{id}/backup-profiles` | List backup profiles for profile |
| **Backup Inventory** | | |
| GET | `/backups` | List all backup inventory |
| GET | `/backups/{id}` | Get backup inventory entry |
| **Health & Events** | | |
| GET | `/health` | List all cluster health reports |
| GET | `/events?limit=N` | List recent events (default limit: 100) |
| **Admin** | | |
| GET | `/postgres-versions` | List PG version registry |
| POST | `/postgres-versions` | Add PG version |
| PUT | `/postgres-versions/{id}` | Update PG version |
| DELETE | `/postgres-versions/{id}` | Delete PG version |
| POST | `/postgres-versions/{id}/default` | Set default PG version |
| GET | `/postgres-variants` | List image variants |
| POST | `/postgres-variants` | Create image variant |
| DELETE | `/postgres-variants/{id}` | Delete image variant |

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
 │     └─ Satellite-controlled 9-step orchestration   │
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
| ServiceAccount | `{name}-failover` | Identity for sentinel sidecar |
| Role | `{name}-failover` | pods (get,patch), pods/exec (create), leases (get,create,update) |
| RoleBinding | `{name}-failover` | Binds role to service account |

**Init container logic:**
- Ordinal 0 (primary): runs `initdb`, creates replication user, creates app databases
- Ordinal > 0 (replica): runs `pg_basebackup` from primary with `-R` flag

**StatefulSet containers:**
1. `pg-init` — init container for first-boot setup
2. `postgres` — main PG container with liveness/readiness probes
3. `failover` — sidecar for leader election and automatic failover (when enabled)
4. `backup` — sidecar for WAL archiving, base/incremental/logical backups (when backup profiles attached)

**Volume management:**
- `data` VCT — primary PGDATA volume
- `wal` VCT — separate WAL volume (optional, from `wal_storage` config)
- `config` volume — mounted ConfigMap for postgresql.conf/pg_hba.conf
- `secret` volume — mounted Secret for passwords

**Secrets are create-only** (`createOrPreserveSecret`): passwords are generated once and never overwritten on config updates. This prevents password rotation from breaking running clusters.

**VolumeClaimTemplates are immutable**: the operator warns on VCT changes but cannot apply them to existing StatefulSets (K8s limitation).

**Pod role labels** (`pg-swarm.io/role=primary` / `pg-swarm.io/role=replica`) drive service routing via label selectors. The sentinel sidecar manages these labels.

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

The satellite orchestrates switchover as a 9-step process with progress tracking. The switchover is satellite-controlled — central sends the `SwitchoverRequest`, and the satellite drives the entire flow using sidecar streaming for direct command dispatch:

```
 Switchover (Satellite-controlled, 9 steps)
    │
    ├─ 1. Verify target pod exists and is a replica
    ├─ 2. Discover current primary pod (via sidecar stream)
    ├─ 3. Check replica is streaming and caught up
    ├─ 4. Fence primary with drain (FencePrimaryWithOpts, configurable timeout)
    ├─ 5. CHECKPOINT on primary (flush WAL)
    ├─ 6. Transfer leader lease to target pod
    ├─ 7. pg_promote() on target replica    ◄── POINT OF NO RETURN
    ├─ 8. Label pods (swap primary/replica labels)
    └─ 9. Renew lease under new primary
```

**Progress tracking**: Each step is reported to the ops tracker (`internal/central/server/ops_tracker.go`), which broadcasts updates to dashboard clients via WebSocket. The dashboard renders a `SwitchoverProgressModal` with real-time step visualization and a PONR indicator.

**Rollback**: Steps 1-6 are reversible (unfence primary, restore lease). After step 7 (promote), the switchover cannot be rolled back — the old primary's sentinel sidecar detects the split-brain condition on its next tick and automatically demotes PG to a standby (see Section 8).

---

## 8. Sentinel Sidecar

The sentinel sidecar (`cmd/sentinel-sidecar`) runs as a container alongside each postgres pod in the StatefulSet. It manages leader election using Kubernetes Coordination Leases and handles automatic failover.

### 8.1 Leader Election

Each sidecar contends for a Lease resource (`{cluster}-leader`) in the cluster namespace:

- **Lease duration**: 5 seconds
- **Renewal**: On each tick (every 1 second by default), the current holder renews `renewTime`
- **Acquisition**: If the lease is expired or doesn't exist, a replica can acquire it and promote
- **Optimistic locking**: Uses `resourceVersion` to prevent race conditions

### 8.2 Tick Loop

```
 Sidecar Tick (every 1s)
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
        ├─ Check WAL receiver health
        ├─ Check primary reachability (1s TCP connect to RW service)
        │   ├─ Reachable → reset count, skip lease check (fast path)
        │   └─ Unreachable → increment count
        │       ├─ count < 3 → return (not enough evidence)
        │       └─ count >= 3 → check lease
        │           ├─ Expired → acquire lease → pg_promote() → label "primary"
        │           └─ Not expired → log warning (network partition)
```

### 8.3 SQL Fencing (`internal/shared/pgfence`)

Fencing is a shared package used by both the sentinel sidecar and the switchover handler:

**`FencePrimary(ctx, db)`** — three steps, all attempted even if earlier ones fail:
1. `ALTER SYSTEM SET default_transaction_read_only = on` — block writes
2. `SELECT pg_reload_conf()` — apply immediately
3. `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE backend_type = 'client backend'` — kill existing clients, preserve replication

We intentionally do NOT lower `max_connections` because `ALTER SYSTEM` persists in `postgresql.auto.conf`. If PG restarts after demotion with `max_connections=1`, it fails: `superuser_reserved_connections (3) must be less than max_connections (1)`.

**`UnfencePrimary(ctx, db)`** — reverses fencing:
1. `ALTER SYSTEM RESET default_transaction_read_only`
2. `SELECT pg_reload_conf()`

**`IsFenced(ctx, db)`** — checks live `SHOW default_transaction_read_only`. Nil-safe with panic recovery.

**`FencePrimaryWithOpts(ctx, db, opts)`** — extended fencing for switchover with configurable drain timeout. Allows existing connections to complete in-flight transactions before termination. Used by the satellite-controlled switchover (step 4).

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

The sentinel sidecar requires these Kubernetes permissions:

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
| `HEALTH_CHECK_INTERVAL` | Config | Tick interval in seconds (default: 1) |
| `PRIMARY_HOST` | Config | RW service DNS for direct primary reachability check |

### 8.7 Log Watcher (`internal/sentinel/logwatcher.go`)

Real-time PostgreSQL log monitoring via the Kubernetes log API. The log watcher tails the postgres container's stdout/stderr and matches log lines against recovery patterns.

**Pattern categories (40+ patterns across 9 categories):**

| Category | Examples | Default Actions |
|----------|----------|-----------------|
| Data corruption | checksum failure, invalid page header | rebasebackup |
| OOM | out of memory, kill process | restart |
| WAL issues | WAL segment not found, timeline mismatch | rewind |
| Replication failures | replication terminated, primary connection lost | event |
| Configuration errors | invalid config parameter, could not bind | event |
| Connection issues | too many connections, connection limit exceeded | event |
| Storage | no space left on device, disk full | event |
| Tablespace | tablespace not found, invalid tablespace | event |
| Extension | extension not found, incompatible version | event |

**Action types:**
- `restart` — stop and restart PostgreSQL
- `rewind` — run `pg_rewind` to re-sync with primary
- `rebasebackup` — full re-sync via `pg_basebackup`
- `event` — report event to central (no local action)
- `exec` — run a custom command

**Safety features:** cooldown period between actions (prevents action storms), pattern deduplication (same log line won't trigger twice), action mutex (only one recovery action at a time).

Recovery patterns can be managed centrally via event rule sets and attached to clusters.

### 8.8 Sidecar Streaming (`internal/sentinel/connector.go`)

Bidirectional gRPC streaming between each sentinel sidecar and the satellite agent. This enables the satellite to dispatch commands directly to specific sidecars during switchover and other operations.

**Connection lifecycle:**
1. Sidecar connects to satellite's `SidecarStreamService` (see Section 4.5)
2. Sends `SidecarIdentity` with pod name, cluster name, namespace
3. Maintains heartbeat every 10 seconds
4. Receives commands and returns results

**Reconnection:** Exponential backoff (1s to 30s), automatic reconnection on disconnect.

**Command flow during switchover:**
1. Satellite's switchover handler looks up the sidecar stream by pod name
2. Sends command (e.g., `fence`, `checkpoint`, `promote`) to the target sidecar
3. Sidecar executes the command locally and returns `CommandResult` with success/error and output
4. Satellite proceeds to next switchover step or handles failure

This replaces the previous approach of K8s exec for switchover operations, providing lower latency and typed command/result protocol.

---

## 9. Backup Sidecar

The backup sidecar (`cmd/backup-sidecar`) runs as a container alongside each postgres pod when backup profiles are attached. It detects its role (primary or replica) via `pg_is_in_recovery()` and activates the corresponding responsibilities.

### 9.1 Split-Responsibility Model

```
┌─────────────────────────────────────────────────────┐
│              StatefulSet Pod (Primary)               │
│  ┌──────────┐  ┌──────────────┐  ┌───────────────┐ │
│  │ postgres │  │   failover   │  │    backup      │ │
│  │          │  │   sidecar    │  │    sidecar     │ │
│  │ archive_ │  │              │  │                │ │
│  │ command──┼──┼──────────────┼──►  WAL push API  │ │
│  │          │  │              │  │  backups.db     │ │
│  │          │  │              │  │  retention      │ │
│  └──────────┘  └──────────────┘  └───────────────┘ │
└─────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────┐
│              StatefulSet Pod (Replica)                │
│  ┌──────────┐  ┌──────────────┐  ┌───────────────┐ │
│  │ postgres │  │   failover   │  │    backup      │ │
│  │          │  │   sidecar    │  │    sidecar     │ │
│  │          │  │              │  │                │ │
│  │          │  │              │  │ pg_basebackup  │ │
│  │          │  │              │  │ pg_dump        │ │
│  │          │  │              │  │ scheduler      │ │
│  └──────────┘  └──────────────┘  └───────────────┘ │
└─────────────────────────────────────────────────────┘
```

### 9.2 WAL Archiving

PostgreSQL's `archive_command` and `restore_command` point to the local backup sidecar via HTTP:

```
archive_command = 'curl -sf -X POST -F file=@%p -F name=%f http://localhost:8442/wal/push'
restore_command = 'curl -sf -o %p http://localhost:8442/wal/fetch?name=%f'
```

The sidecar compresses WAL segments (gzip) and uploads them to the configured destination. The `archive_command` blocks until the upload is durable — PG only marks WAL as archived after curl returns 0.

### 9.3 Metadata DB

Each satellite-cluster combination has a SQLite database (`backups.db`) stored at the destination root. The primary sidecar is the single writer. It tracks backup sets, individual backups, WAL segments, and statistics. Chain reconstruction queries enable PITR: find the base backup, chain incrementals, identify the WAL range.

### 9.4 Cross-Pod Communication

The replica sidecar notifies the primary after each backup via:
```
POST http://{cluster}-0.{cluster}-headless.{namespace}.svc.cluster.local:8442/backup/complete
```

If the primary is unreachable (failover in progress), the replica retries with backoff. Backup files are already uploaded — only metadata recording is deferred.

### 9.5 Role Change Handling

The sidecar polls `pg_is_in_recovery()` every 10 seconds. On role change (failover):
1. Stop current responsibilities (scheduler or WAL handler)
2. Switch to new role's responsibilities
3. New primary: take over WAL archiving + metadata
4. New replica: start backup scheduler

### 9.6 Environment Variables

| Variable | Source | Purpose |
|----------|--------|---------|
| `SATELLITE_ID` | Config | Destination folder naming |
| `CLUSTER_NAME` | Config | Destination folder + headless service DNS |
| `POD_NAME` | Downward API | Pod identity |
| `NAMESPACE` | Config | K8s namespace for services |
| `DEST_TYPE` | Config | Destination type (s3, gcs, sftp, local) |
| `BASE_SCHEDULE` | Config | Cron expression for base backups |
| `INCR_SCHEDULE` | Config | Cron expression for incremental backups |
| `LOGICAL_SCHEDULE` | Config | Cron expression for logical backups |
| `RETENTION_SETS` | Config | Number of backup sets to retain |
| `RETENTION_DAYS` | Config | Days of WAL retention |
| `PGUSER` | Config | PostgreSQL user (backup_user) |
| `PGPASSWORD` | Secret | PostgreSQL password |

Destination-specific variables (S3_BUCKET, GCS_BUCKET, SFTP_HOST, etc.) are also injected.

---

## 10. Profiles and Deployment Rules

### 10.1 Profiles

A **Profile** is a reusable cluster template stored as JSONB. It defines the full cluster specification: PostgreSQL version, storage, resources, PG parameters, HBA rules, failover settings, WAL archiving, and application databases.

- Profiles can be **cloned** to create variations
- Profiles can be **locked** after first deployment (immutable — prevents accidental changes to running clusters)
- The profile editor in the dashboard has 6 tabs: General, Volumes, Resources, PostgreSQL (extensive parameter catalog with 8 categories), HBA Rules, and Databases

### 10.2 Deployment Rules

A **Deployment Rule** maps a profile (WHAT) to satellites (WHERE):

```
Rule: "prod-analytics-db"
  Profile: "analytics-ha-3node"
  Label Selector: { region: "us-east", tier: "prod" }
  Namespace: "analytics"
  Cluster Name: "analytics"
```

When a rule is created or a new satellite matches the label selector, the central automatically creates a `cluster_config` for each matching satellite and pushes it via the gRPC stream.

### 10.3 PostgreSQL Version Registry

The admin page manages a registry of available PostgreSQL versions (version, variant: alpine/debian, image tag). The default version is pre-selected when creating new profiles.

---

## 11. Web Dashboard

The dashboard is a React 19 SPA built with Vite and JSX (not TypeScript). It is embedded into the Central binary via Go's `embed.FS` and served alongside the REST API.

### 11.1 Pages

| Page | Route | Purpose |
|------|-------|---------|
| **Overview** | `/` | Stat cards (satellites, clusters, healthy, events) with icons; recent activity table |
| **Satellites** | `/satellites` | Table with approve/reject, label editing, state badges, log viewer link |
| **Profiles** | `/profiles` | Grid of profile cards; 6-tab editor with PG parameter catalog, backup profile attach/detach |
| **Deployment Rules** | `/deployment-rules` | Rule CRUD; expandable cards showing profile summary and created clusters |
| **Clusters** | `/clusters` | Card grid with instance table, disk/WAL breakdown, database sizes, cache hit ratio, slow queries, switchover buttons |
| **Cluster Detail** | `/clusters/:id` | Full-page cluster view with tabs: Instances, Backups, Events (separate route, uses MiniHeader) |
| **Backup Profiles** | `/backup-profiles` | Backup profile CRUD, schedule configuration, destination settings |
| **Events** | `/events` | Event log with severity icons (info, warning, error, critical) |
| **Satellite Logs** | `/satellites/:id/logs` | Terminal-style log viewer with SSE streaming, level filter, remote log level control (uses MiniHeader) |
| **Admin** | `/admin` | 4 tabs: Storage Tiers, Image Variants, PG Versions, Recovery Rules |

### 11.2 Architecture

- **DataContext**: Global provider fetching all data every 10 seconds via REST API
- **ToastContext**: Toast notification system (auto-dismiss after 3.5s)
- **Badge component**: Semantic state badges with lucide-react icons (CheckCircle2, Loader, AlertCircle, Pause, XCircle, etc.)
- **Layout**: Sticky topbar with gradient, icon-enhanced navigation, satellite status pill
- **MiniHeader**: Compact header for full-page routes (SatelliteLogs, ClusterDetail) outside the main Layout
- **WebSocket**: Real-time state push for switchover progress and health updates, with automatic polling fallback
- **Ops Tracker**: Active operation tracking — dashboard subscribes to operation updates via WebSocket
- **SSE**: Server-sent events for satellite log streaming (`/satellites/:id/logs/stream`)

### 11.3 Cluster Detail Page

The Cluster Detail page (`/clusters/:id`) is a full-page route providing deep visibility into a cluster:

- **Instances tab**: Per-pod details — ready state, timeline, connections, replication lag, PG uptime, WAL receiver status, disk/WAL breakdown, database sizes, cache hit ratios, table stats, slow queries
- **Backups tab**: Backup inventory from sidecar reports, restore operations
- **Events tab**: Cluster-specific event log

### 11.4 Switchover Progress Modal

The `SwitchoverProgressModal` component visualizes the 9-step satellite-controlled switchover in real-time:

- Each step shown with status (pending, in-progress, completed, failed)
- Point of no return (PONR) indicator at step 7 (promote)
- Real-time updates via WebSocket from the ops tracker
- Error details if a step fails, with rollback status for pre-PONR failures

### 11.5 Event Rules Editor

The `EventRulesTab` component in the Admin page provides:

- Event rule set CRUD (create, edit, delete)
- Inline rule editing — add/remove patterns, configure actions, set cooldowns
- Pattern sandbox — test regex patterns against sample log lines

### 11.6 Status Indicators

- **PG Status Dot**: Green (ready with uptime tooltip) or blinking red (not ready)
- **Lag Dot**: Green (< 1 min), amber (1-3 min), blinking red (> 3 min)
- **Connection Bar**: Visual bar with color coding (green < 75%, amber 75-90%, red > 90%)

---

## 12. Configuration

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

### 11.3 Sentinel Sidecar

See Section 8.6.

---

## 13. Build & Test

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

## 14. Key Design Decisions

### Why gRPC bidirectional streaming?

Edge clusters may be behind NAT or firewalls. The satellite initiates the connection outward, and the persistent stream allows central to push configs without needing to reach back in. This also provides natural keep-alive semantics via heartbeats.

### Why not use CRDs / operator-sdk?

Minimizing the edge footprint. CRDs require cluster-admin to install, and operator-sdk adds framework weight. By constructing raw manifests (StatefulSet, Service, ConfigMap, Secret), the satellite only needs basic RBAC on a single namespace.

### Why a per-pod sentinel sidecar instead of satellite-driven failover?

The sidecar runs locally to each pod and uses Kubernetes Leases for leader election. This is faster than satellite-driven failover (no cross-pod exec needed), works even if the satellite agent is down, and provides a clean separation of concerns. The satellite handles planned switchovers; the sidecar handles automatic failover.

### Why SQL fencing before demotion?

Demotion involves creating `standby.signal` and restarting PG, which takes several seconds. During that window, the old primary could accept writes. Fencing (`ALTER SYSTEM SET default_transaction_read_only` + reload + kill client connections) provides immediate write protection, closing the split-brain window to near-zero. We avoid lowering `max_connections` because `ALTER SYSTEM` persists in `postgresql.auto.conf` and PG refuses to start if `max_connections` < `superuser_reserved_connections`.

### Why K8s exec for demotion?

The sentinel sidecar runs in a separate container from postgres. Creating `standby.signal` and running `pg_ctl stop` requires filesystem access to PGDATA. The Kubernetes exec API (`remotecommand.NewSPDYExecutor`) allows the sidecar to run commands inside the postgres container without sharing volumes.

### Why SHA-256 token hashing (not bcrypt)?

Auth tokens are high-entropy random strings (256 bits), not user-chosen passwords. SHA-256 is sufficient for pre-image resistance on random tokens, and avoids the latency of bcrypt on every stream reconnection.

### Why JSONB for config and labels?

Cluster configurations vary widely (different PG params, HBA rules, resource profiles). JSONB provides schema flexibility while remaining queryable. Labels on satellites enable flexible targeting without schema changes.

### Why sync cluster config state from health reports?

Without this, a cluster's config state stays stuck at `creating` forever after creation. When the satellite reports health (running/degraded/failed), the central now updates `cluster_configs.state` to match — but skips `paused` and `deleting` states which are user-controlled.

### Why buffered send channels (cap=64)?

Decouples the REST API response time from satellite stream throughput. A slow satellite won't block the admin's API call. If the buffer fills (satellite severely behind), the push fails gracefully rather than blocking.

### Why a backup sidecar instead of CronJobs?

The original CronJob-based model had fundamental issues: WAL and backups could land on different storage systems, WAL continuity was never validated, and ~500 lines of bash were embedded in Go strings. The sidecar model provides:

- **WAL integrity** — the sidecar owns both WAL archiving and backup metadata, ensuring consistency
- **Role-aware** — detects primary/replica via `pg_is_in_recovery()` and switches responsibilities on failover
- **localhost access** — `archive_command` POSTs to `localhost:8442`, no network hop for WAL. `pg_basebackup -h localhost` for backups, no service DNS resolution
- **Pure Go** — all metadata operations in Go with embedded SQLite (`modernc.org/sqlite`), no shell scripts
- **Automatic initial backup** — sidecar triggers a base backup on startup if no existing backup set exists
- **Zero additional K8s resources** — no CronJobs, no backup PVCs, no Jobs to manage

The StatefulSet rolling restart concern from adding a sidecar is acceptable because backup profiles are typically attached once at cluster creation time, not dynamically toggled.

### Why JSX instead of TypeScript?

The dashboard is a relatively straightforward CRUD + monitoring UI. JSX keeps the setup simple with fewer build dependencies. The React 19 + Vite + lucide-react stack provides a modern development experience without TypeScript overhead.

### Why profiles and deployment rules?

Profiles provide reusable, versionable cluster templates. Deployment rules provide declarative "deploy this profile to all satellites matching these labels" semantics. Together they enable fleet-scale management: change a profile, and all clusters using it get updated.
