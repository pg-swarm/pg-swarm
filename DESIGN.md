# pg-swarm: Design Document

## 1. Overview

pg-swarm is a centralized management system for PostgreSQL High Availability (HA) clusters deployed across up to 500 edge Kubernetes clusters. A cloud-hosted **Central** control plane handles registration, configuration distribution, and health monitoring. A **Satellite** agent runs on each edge cluster as a lightweight Kubernetes operator — constructing PG cluster manifests from JSON configs, performing health checks, and orchestrating failover by switching service selectors.

**No dependency on CloudNativePG (CNPG).** pg-swarm builds StatefulSets, Services, ConfigMaps, and Secrets from scratch.

### Design Goals

| Goal | Approach |
|------|----------|
| Scalable to 500 edge clusters | Bidirectional gRPC streaming; one persistent connection per satellite |
| Minimal edge footprint | Single Go binary per edge cluster; no CRDs or operator frameworks |
| Centralized visibility | All health and events stream to central; REST API + web console for ops |
| Automated failover | Satellite-local detection and promotion; no central round-trip required |
| Secure by default | Token-based auth with SHA-256 hashing; mTLS planned for Phase 7 |

### Tech Stack

- **Language**: Go
- **Communication**: gRPC with bidirectional streaming (protobuf v3)
- **Central database**: PostgreSQL (via pgx/v5)
- **REST API**: GoFiber v2
- **Logging**: zerolog
- **Web console**: React + TypeScript (Phase 6)
- **Build**: buf (protobuf), Make, Docker

---

## 2. Architecture

```
                    ┌─────────────────────────────────────────────┐
                    │              Central Control Plane          │
                    │                                             │
                    │  ┌──────────┐  ┌──────────┐  ┌──────────┐   │
                    │  │ gRPC     │  │ REST API │  │ Web      │   │
                    │  │ Server   │  │ (chi)    │  │ Console  │   │
                    │  │ :9090    │  │ :8080    │  │ (React)  │   │
                    │  └────┬─────┘  └────┬─────┘  └──────────┘   │
                    │       │             │                       │
                    │  ┌────┴─────────────┴─────┐                 │
                    │  │     StreamManager       │                │
                    │  │  map[UUID]*SatStream    │                │
                    │  └────────────┬────────────┘                │
                    │               │                             │
                    │  ┌────────────┴────────────┐                │
                    │  │      PostgreSQL          │               │
                    │  │  satellites, configs,    │               │
                    │  │  health, events, groups  │               │
                    │  └─────────────────────────┘                │
                    └──────────┬──────────────────────────────────┘
                               │ gRPC bidi streams
            ┌──────────────────┼──────────────────┐
            │                  │                  │
     ┌──────┴────-──┐   ┌──────┴────-──┐   ┌──────┴────-──┐
     │ Satellite A  │   │ Satellite B  │   │ Satellite N  │
     │              │   │              │   │              │
     │ ┌──────────┐ │   │ ┌──────────┐ │   │ ┌──────────┐ │
     │ │ Stream   │ │   │ │ Stream   │ │   │ │ Stream   │ │
     │ │ Connector│ │   │ │ Connector│ │   │ │ Connector│ │
     │ ├──────────┤ │   │ ├──────────┤ │   │ ├──────────┤ │
     │ │ Operator │ │   │ │ Operator │ │   │ │ Operator │ │
     │ ├──────────┤ │   │ ├──────────┤ │   │ ├──────────┤ │
     │ │ Health   │ │   │ │ Health   │ │   │ │ Health   │ │
     │ ├──────────┤ │   │ ├──────────┤ │   │ ├──────────┤ │
     │ │ Failover │ │   │ │ Failover │ │   │ │ Failover │ │
     │ └──────────┘ │   │ └──────────┘ │   │ └──────────┘ │
     │   K8s Edge   │   │   K8s Edge   │   │   K8s Edge   │
     └──────────────┘   └──────────────┘   └──────────────┘
```

---

## 3. Project Structure

```
pg-swarm/
├── cmd/
│   ├── central/main.go              # Central control plane entrypoint
│   └── satellite/main.go            # Satellite agent entrypoint
├── api/
│   ├── proto/v1/                    # Protobuf definitions
│   │   ├── common.proto             # Shared enums (SatelliteState, ClusterState, InstanceRole)
│   │   ├── registration.proto       # Register + CheckApproval RPCs
│   │   ├── config.proto             # Bidirectional streaming + ClusterConfig messages
│   │   └── health.proto             # Health report + event messages
│   └── gen/v1/                      # Generated Go code (buf generate)
├── internal/
│   ├── central/
│   │   ├── server/
│   │   │   ├── grpc.go              # gRPC server, StreamManager, auth interceptors
│   │   │   └── rest.go              # REST API (chi router, 13 endpoints)
│   │   ├── store/
│   │   │   ├── store.go             # Store interface (25 methods)
│   │   │   ├── postgres.go          # PostgreSQL implementation (pgxpool)
│   │   │   ├── migrate.go           # Embedded SQL migration runner
│   │   │   └── migrations/          # SQL migration files
│   │   ├── registry/registry.go     # Registration + approval logic
│   │   ├── auth/tokens.go           # Token generation/hashing (SHA-256)
│   │   ├── configmgr/manager.go     # Config lifecycle + push (Phase 2+)
│   │   ├── groups/groups.go         # Edge group management (Phase 2+)
│   │   └── monitor/aggregator.go    # Health aggregation (Phase 4)
│   ├── satellite/
│   │   ├── agent/
│   │   │   ├── agent.go             # Main agent lifecycle
│   │   │   └── registration.go      # Register with central + approval polling
│   │   ├── stream/connector.go      # Persistent gRPC stream + reconnect
│   │   ├── operator/                # K8s operator (Phase 3)
│   │   ├── health/                  # Health checker + reporter (Phase 4)
│   │   └── failover/                # Detection, promotion, switching (Phase 5)
│   └── shared/models/models.go      # Shared Go types
├── pkg/
│   ├── pgutil/                      # pg_isready, replication queries (Phase 3+)
│   └── k8sutil/                     # K8s client helpers (Phase 3+)
├── web/                             # React + TypeScript console (Phase 6)
├── deploy/
│   ├── helm/pg-swarm-central/       # Helm chart (Phase 8)
│   ├── helm/pg-swarm-satellite/     # Helm chart (Phase 8)
│   └── docker/                      # Dockerfiles (Phase 8)
├── buf.yaml                         # Buf module config
├── buf.gen.yaml                     # Buf code generation config
├── go.mod
└── Makefile
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
| `ClusterHealthReport` | Per-cluster health with instance details | Every 10 seconds (Phase 4) |
| `EventReport` | Significant events (failover, errors) | On occurrence |
| `ConfigAck` | Acknowledge config receipt with success/error | On config push |

**Downstream (Central → Satellite):**

| Message | Purpose | Trigger |
|---------|---------|---------|
| `ClusterConfig` | Full cluster specification to deploy | REST create/update |
| `DeleteCluster` | Remove a cluster | REST delete |
| `HeartbeatAck` | Acknowledge heartbeat | On heartbeat |

### 4.3 ClusterConfig (Core Data Contract)

The `ClusterConfig` message defines everything the satellite needs to construct a PG cluster:

```protobuf
message ClusterConfig {
  string cluster_name = 1;
  string namespace = 2;
  int32 replicas = 3;
  PostgresSpec postgres = 4;     // version, image
  StorageSpec storage = 5;       // size, storage_class
  ResourceSpec resources = 6;    // cpu/mem requests and limits
  map<string, string> pg_params = 7;  // postgresql.conf overrides
  repeated string hba_rules = 8;      // pg_hba.conf rules
  int64 config_version = 9;           // monotonically increasing
}
```

Config versions are auto-incremented by the central store on each update, enabling satellites to detect stale or duplicate pushes.

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

### 5.4 Future: mTLS (Phase 7)

- Central acts as CA, issuing client certificates during approval
- Satellite presents client cert on Connect
- Certificate rotation via stream renegotiation

---

## 6. Central Components

### 6.1 Store Layer

The `Store` interface defines 25 methods across 5 domains:

| Domain | Methods | Key Details |
|--------|---------|-------------|
| Satellites | 7 | CRUD, token lookup, heartbeat update |
| Groups | 4 | CRUD, satellite assignment |
| Cluster Configs | 7 | CRUD, query by satellite or group |
| Health | 3 | Upsert (ON CONFLICT DO UPDATE), query |
| Events | 3 | Create, list with limit, filter by cluster |

**PostgreSQL implementation details:**
- Connection pooling via `pgxpool.Pool`
- JSONB columns for flexible data: `labels`, `config`, `instances`
- Parameterized queries throughout (no SQL injection risk)
- `ON CONFLICT (satellite_id, cluster_name) DO UPDATE` for health upserts
- Automatic `config_version + 1` increment on config updates
- Compile-time interface satisfaction check: `var _ Store = (*PostgresStore)(nil)`

### 6.2 Database Schema

```sql
┌─────────────┐       ┌──────────────────┐       ┌──────────────┐
│ edge_groups  │       │   satellites      │       │   events      │
├─────────────┤       ├──────────────────┤       ├──────────────┤
│ id (PK)     │◄──FK──│ group_id          │   ┌──│ satellite_id  │
│ name (UQ)   │       │ id (PK)          │   │  │ cluster_name  │
│ description │       │ hostname          │   │  │ severity      │
│ labels JSONB│       │ k8s_cluster_name  │   │  │ message       │
│ created_at  │       │ region            │   │  │ source        │
│ updated_at  │       │ labels JSONB      │   │  │ created_at    │
└─────────────┘       │ state             │   │  └──────────────┘
                      │ auth_token_hash   │   │
                      │ temp_token_hash   │   │
                      │ last_heartbeat    │   │  ┌──────────────────┐
                      │ created_at        │   │  │ cluster_health    │
                      │ updated_at        │   │  ├──────────────────┤
                      └─────┬────────────┘   ├──│ satellite_id (PK) │
                            │                │  │ cluster_name (PK) │
                            │FK              │  │ state             │
                      ┌─────┴────────────┐   │  │ instances JSONB   │
                      │ cluster_configs   │   │  │ updated_at        │
                      ├──────────────────┤   │  └──────────────────┘
                      │ id (PK)          │   │
                      │ name             │   │
                      │ namespace        │   │
                      │ satellite_id ────┼───┘
                      │ group_id ────────┼──► edge_groups
                      │ config JSONB     │
                      │ config_version   │
                      │ state            │
                      │ UQ(name,sat_id)  │
                      └──────────────────┘
```

**Indexes:**
- `idx_satellites_state` — fast filtering by connection state
- `idx_cluster_configs_satellite` — look up configs for a satellite
- `idx_cluster_configs_group` — look up configs for a group
- `idx_events_satellite_cluster` — filter events by satellite+cluster
- `idx_events_created` — most recent events first

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

The 64-message buffer prevents slow satellites from blocking the REST API. If the buffer is full, PushConfig returns an error (logged, not propagated to the API caller).

### 6.5 REST API

All endpoints are under `/api/v1` using chi router:

| Method | Path | Description |
|--------|------|-------------|
| GET | `/satellites` | List all satellites with state |
| POST | `/satellites/{id}/approve` | Approve pending satellite → returns auth_token |
| POST | `/satellites/{id}/reject` | Reject pending satellite |
| GET | `/clusters` | List all cluster configs |
| POST | `/clusters` | Create cluster config → triggers push |
| GET | `/clusters/{id}` | Get single cluster config |
| PUT | `/clusters/{id}` | Update cluster config → triggers push |
| DELETE | `/clusters/{id}` | Delete cluster config |
| GET | `/groups` | List edge groups |
| POST | `/groups` | Create edge group |
| POST | `/groups/{id}/satellites/{satId}` | Assign satellite to group |
| GET | `/health` | List all cluster health reports |
| GET | `/events?limit=N` | List recent events (default limit: 100) |

---

## 7. Satellite Components

### 7.1 Agent Lifecycle

```
 ┌──────────────────────────────────────────────────┐
 │                  Satellite Agent                   │
 │                                                    │
 │  1. Load identity from disk                        │
 │     └─ Not found? Register + poll for approval     │
 │        └─ Save identity.json on approval           │
 │                                                    │
 │  2. Connect persistent gRPC stream                 │
 │     └─ Exponential backoff on disconnect           │
 │                                                    │
 │  3. Operator loop (Phase 3)                        │
 │     └─ Receive configs → reconcile K8s resources   │
 │                                                    │
 │  4. Health checker (Phase 4)                       │
 │     └─ Every 10s: pg_isready + replication lag     │
 │                                                    │
 │  5. Failover manager (Phase 5)                     │
 │     └─ Detect primary down → promote → switch      │
 └──────────────────────────────────────────────────┘
```

### 7.2 Identity Persistence

The satellite stores its identity at `{DataDir}/identity.json`:

```json
{
  "satellite_id": "550e8400-e29b-41d4-a716-446655440000",
  "auth_token": "a1b2c3d4..."
}
```

- Directory created with `0700` permissions
- File written with `0600` permissions
- On restart, the satellite loads this file and skips registration
- If the file is missing or corrupt, re-registration is triggered

### 7.3 Registration Flow

1. Satellite calls `Register` RPC with hostname, K8s cluster name, region, and labels
2. Central creates a satellite record in `pending` state, returns `satellite_id` + `temp_token`
3. Satellite polls `CheckApproval` every 5 seconds with `satellite_id` + `temp_token`
4. Admin approves via REST API: `POST /api/v1/satellites/{id}/approve`
5. Next `CheckApproval` poll returns `approved=true` + `auth_token`
6. Satellite saves identity to disk and proceeds to stream connection

### 7.4 Stream Connector

The stream connector maintains a persistent bidirectional gRPC connection with automatic reconnection:

**Reconnection strategy:**
- Initial backoff: 1 second
- Exponential increase: backoff × 2 each failure
- Maximum backoff: 30 seconds
- Backoff resets implicitly on successful connection

**Connection setup:**
1. Dial central with `authorization` token in gRPC metadata
2. Call `Connect` RPC to establish bidirectional stream
3. Start heartbeat goroutine (sends every 10 seconds)
4. Enter read loop to process incoming messages

**Message dispatch:**
- `ClusterConfig` → `OnConfig` callback (operator reconcile in Phase 3)
- `DeleteCluster` → `OnDelete` callback (operator delete in Phase 3)
- `HeartbeatAck` → debug log

### 7.5 PG Cluster Manifest Construction (Phase 3)

From a `ClusterConfig`, the operator builds these Kubernetes resources:

| Resource | Name Pattern | Purpose |
|----------|-------------|---------|
| StatefulSet | `{name}` | N postgres pods with init + main containers |
| Headless Service | `{name}-headless` | Stable pod DNS for replication |
| RW Service | `{name}-rw` | Routes to primary (`role=primary` selector) |
| RO Service | `{name}-ro` | Routes to replicas (`role=replica` selector) |
| ConfigMap | `{name}-config` | postgresql.conf + pg_hba.conf |
| Secret | `{name}-secret` | Superuser + replication passwords |

**Init container logic:**
- Ordinal 0 (primary): runs `initdb`
- Ordinal > 0 (replica): runs `pg_basebackup` from primary

**Pod role labels** (`role=primary` / `role=replica`) are managed by the satellite and drive service routing via label selectors.

### 7.6 Failover Logic (Phase 5)

```
 Health Checker
    │
    ├─ 3 consecutive pg_isready failures on primary
    │
    ▼
 Failover Manager
    │
    ├─ 1. Select best replica (lowest lag, pg_ready=true)
    ├─ 2. Exec pg_ctl promote on chosen replica
    ├─ 3. Wait for pg_is_in_recovery() = false
    ├─ 4. Patch pod labels:
    │      new primary → role=primary
    │      old primary → role=failed
    ├─ 5. RW service auto-routes to new primary (selector)
    ├─ 6. Reconfigure remaining replicas:
    │      update primary_conninfo → new primary
    └─ 7. Report events to central via stream
```

**Key design choice:** Failover is satellite-local. No round-trip to central is needed, minimizing promotion time. Central is notified after the fact via event reports.

---

## 8. Configuration

### 8.1 Central

| Environment Variable | Default | Description |
|---------------------|---------|-------------|
| `DATABASE_URL` | `postgres://postgres:postgres@localhost:5432/pgswarm?sslmode=disable` | PostgreSQL connection string |
| `GRPC_ADDR` | `:9090` | gRPC listen address |
| `HTTP_ADDR` | `:8080` | REST API listen address |

### 8.2 Satellite

| Environment Variable | Default | Required | Description |
|---------------------|---------|----------|-------------|
| `CENTRAL_ADDR` | `localhost:9090` | No | Central gRPC address |
| `HOSTNAME` | OS hostname | No | Satellite hostname |
| `K8S_CLUSTER_NAME` | — | **Yes** | Kubernetes cluster identifier |
| `REGION` | `""` | No | Geographic region label |
| `DATA_DIR` | `/var/lib/pg-swarm` | No | Persistent storage for identity |

---

## 9. Build Phases

### Phase 1: Foundation (Current)
- Go module, protobuf definitions, code generation
- Shared models, PostgreSQL store with migrations
- RegistrationService gRPC server + satellite registration client
- REST API (satellites, clusters, groups, health, events)
- **Verify**: Satellite registers, admin approves via curl, satellite gets auth token

### Phase 2: Streaming
- Bidirectional `Connect` stream (StreamManager fully operational)
- Satellite stream connector with heartbeats and reconnection
- Config push: REST creates config → StreamManager pushes to satellite
- ConfigAck flow
- **Verify**: Create config via REST, satellite logs receipt

### Phase 3: PG Operator
- Manifest builders (StatefulSet, Services, ConfigMap, Secrets)
- Init container script for primary/replica bootstrap
- Operator reconcile loop (create/update/delete K8s resources)
- **Verify**: Push config → PG StatefulSet on kind/k3d with streaming replication

### Phase 4: Health Monitoring
- Health checker (pg_isready + replication lag via pgx)
- Reporter streams health to central
- Central aggregator stores health, REST endpoints serve it
- **Verify**: Health data visible via REST, replication lag tracked

### Phase 5: Failover
- Primary failure detection (3 consecutive failures)
- Replica promotion + service selector switching + replica reconfiguration
- Event reporting
- **Verify**: Kill primary pod → automatic promotion, services re-route, central shows events

### Phase 6: Web Console
- React + TypeScript (Vite)
- Pages: Dashboard, Satellites, Groups, Clusters, Events
- REST API client, polling for live updates
- **Verify**: Full management workflow through browser

### Phase 7: Security Hardening
- mTLS with CA, RBAC, web console auth (OIDC), audit logging

### Phase 8: Packaging
- Dockerfiles (multi-stage builds)
- Helm charts (central + satellite)
- CI pipeline

---

## 10. Key Design Decisions

### Why gRPC bidirectional streaming?

Edge clusters may be behind NAT or firewalls. The satellite initiates the connection outward, and the persistent stream allows central to push configs without needing to reach back in. This also provides natural keep-alive semantics via heartbeats.

### Why not use CRDs / operator-sdk?

Minimizing the edge footprint. CRDs require cluster-admin to install, and operator-sdk adds framework weight. By constructing raw manifests (StatefulSet, Service, ConfigMap, Secret), the satellite only needs basic RBAC on a single namespace.

### Why satellite-local failover?

Network partitions between edge and central are expected. If failover depended on central, a WAN outage would leave PG clusters unable to recover. The satellite detects and promotes locally, then reports to central when connectivity is restored.

### Why SHA-256 token hashing (not bcrypt)?

Auth tokens are high-entropy random strings (256 bits), not user-chosen passwords. SHA-256 is sufficient for pre-image resistance on random tokens, and avoids the latency of bcrypt on every stream reconnection.

### Why JSONB for config and labels?

Cluster configurations vary widely (different PG params, HBA rules, resource profiles). JSONB provides schema flexibility while remaining queryable. Labels on satellites and groups enable flexible targeting without schema changes.

### Why buffered send channels (cap=64)?

Decouples the REST API response time from satellite stream throughput. A slow satellite won't block the admin's API call. If the buffer fills (satellite severely behind), the push fails gracefully rather than blocking