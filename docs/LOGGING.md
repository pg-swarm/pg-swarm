# Logging Strategy

Comprehensive logging, tracing, and audit strategy for pg-swarm.

## Current State

pg-swarm uses **zerolog** (v1.34.0) with 453 structured log statements across 22 files. The satellite already has a log capture pipeline (StreamHook -> gRPC stream -> central LogBuffer -> SSE fan-out) and dynamic log level management via `LOG_LEVEL` env var and REST API.

### What exists

- Structured zerolog logging in all three binaries (central, satellite, sidecar)
- Context-enriched sub-loggers with satellite_id, cluster name, pod name
- Log capture hook (`internal/satellite/logcapture/hook.go`) streams satellite logs to central
- Log buffer with per-satellite ring buffer (1000 entries) and SSE fan-out
- Dynamic log level: `POST /api/v1/satellites/:id/log-level` changes satellite level at runtime
- Component tagging on sentinel-sidecar (`Str("component", "sentinel-sidecar")`)

### What is missing

1. **No correlation/request IDs** -- no way to follow a request through the system
2. **No HTTP request logging** -- 60+ REST endpoints have zero request/response visibility
3. **No gRPC request logging** -- interceptors only handle auth, not observability
4. **No audit trail** -- mutations (create cluster, approve satellite, trigger switchover) are not tracked
5. **No cross-service trace propagation** -- a switchover initiated from the dashboard cannot be traced through central -> satellite -> sidecar
6. **Central lacks LOG_LEVEL support** -- only satellite respects the `LOG_LEVEL` env var
7. **Inconsistent component tagging** -- only sentinel-sidecar tags its logger

---

## Architecture Overview

```
User (Dashboard/API)
  |
  v
Central Server
  |-- REST API (GoFiber v2, :8080)  ---> 60+ endpoints
  |-- gRPC Server (:9090)           ---> Registration, SatelliteStream, SidecarStream
  |-- WebSocket Hub                 ---> Real-time state push to dashboard
  |-- Log Buffer                    ---> Per-satellite log ring buffer + SSE
  |
  v  (bidirectional gRPC stream)
Satellite Agent (per edge K8s cluster)
  |-- Operator          ---> K8s resource management
  |-- Health Monitor    ---> PostgreSQL health polling
  |-- Stream Connector  ---> Persistent connection to central
  |-- Sidecar Server    ---> gRPC for sentinel sidecars
  |-- Log Capture Hook  ---> Intercepts zerolog events, streams to central
  |
  v  (bidirectional gRPC stream)
Sentinel Sidecar (per PostgreSQL pod)
  |-- Monitor           ---> Leader election, health checks
  |-- Log Watcher       ---> PostgreSQL log pattern matching
  |-- Connector         ---> Stream to satellite for remote commands
```

Every arrow is a tracing boundary. A single user action (e.g., switchover) crosses all three tiers.

---

## Phase 1: Foundation

### 1.1 Request ID Package (`internal/shared/reqid`)

A shared package for request/correlation ID management, importable by all three binaries.

```go
package reqid

import (
    "context"
    "github.com/google/uuid"
    "github.com/rs/zerolog"
    "github.com/rs/zerolog/log"
)

type requestIDKey struct{}

// NewID generates a new UUID-based request ID.
func NewID() string {
    return uuid.NewString()
}

// WithRequestID stores a request ID in the context.
func WithRequestID(ctx context.Context, id string) context.Context {
    return context.WithValue(ctx, requestIDKey{}, id)
}

// FromContext extracts the request ID from context. Returns "" if absent.
func FromContext(ctx context.Context) string {
    if id, ok := ctx.Value(requestIDKey{}).(string); ok {
        return id
    }
    return ""
}

// Logger returns a zerolog sub-logger enriched with the request ID from context.
func Logger(ctx context.Context) zerolog.Logger {
    id := FromContext(ctx)
    if id == "" {
        return log.Logger
    }
    return log.With().Str("request_id", id).Logger()
}
```

This follows the existing `satelliteIDKey{}` pattern at `internal/central/server/grpc.go:638-649`.

### 1.2 Log Level Package (`internal/shared/loglevel`)

Extract `SetGlobalLevel()` from `internal/satellite/logcapture/level.go` into a shared package so both central and satellite can use it.

```go
package loglevel

import (
    "fmt"
    "strings"
    "github.com/rs/zerolog"
)

// SetGlobalLevel parses a level string and sets zerolog's global level.
// Valid levels: "trace", "debug", "info", "warn", "error".
func SetGlobalLevel(levelStr string) (zerolog.Level, error) {
    level, err := zerolog.ParseLevel(strings.ToLower(levelStr))
    if err != nil {
        return zerolog.InfoLevel, fmt.Errorf("invalid log level %q: %w", levelStr, err)
    }
    zerolog.SetGlobalLevel(level)
    return level, nil
}
```

Update `internal/satellite/logcapture/level.go` to delegate to the shared package.

### 1.3 Component Tagging and LOG_LEVEL for Central

Add to all three binaries' logger initialization:

| Binary | Component tag | LOG_LEVEL support |
|--------|--------------|-------------------|
| Central (`cmd/central/main.go`) | `"central"` | Add (currently missing) |
| Satellite (`cmd/satellite/main.go`) | `"satellite"` | Already exists |
| Sentinel Sidecar (`cmd/sentinel-sidecar/main.go`) | `"sentinel-sidecar"` | Add |

Central example:
```go
log.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).
    With().Timestamp().Str("component", "central").Logger()

if lvl := os.Getenv("LOG_LEVEL"); lvl != "" {
    if _, err := loglevel.SetGlobalLevel(lvl); err != nil {
        log.Warn().Str("level", lvl).Msg("invalid LOG_LEVEL, defaulting to info")
    } else {
        log.Info().Str("level", lvl).Msg("log level set from LOG_LEVEL env var")
    }
}
```

### 1.4 HTTP Request Logging Middleware

A GoFiber middleware (`internal/central/server/middleware.go`) registered in `setupRoutes()` before any API route groups.

**What it logs per request:**

| Field | Source | Example |
|-------|--------|---------|
| `request_id` | `X-Request-ID` header or generated UUID | `"550e8400-e29b-..."` |
| `method` | `c.Method()` | `"POST"` |
| `path` | `c.Path()` | `"/api/v1/clusters"` |
| `status` | `c.Response().StatusCode()` | `201` |
| `latency` | `time.Since(start)` | `"12.3ms"` |
| `client_ip` | `c.IP()` | `"10.0.1.5"` |
| `body_size` | Content-Length (mutations only) | `1024` |

**Log level rules:**

| Condition | Level | Rationale |
|-----------|-------|-----------|
| GET requests (2xx) | Debug | Reads are high-volume, low-interest |
| POST/PUT/DELETE (2xx) | Info | Mutations are always significant |
| 4xx responses | Warn | Client errors may indicate misconfiguration |
| 5xx responses | Error | Server errors require attention |

**Skip list** (suppressed from request logging):

| Path | Reason |
|------|--------|
| Non-`/api` paths | SPA catch-all returning index.html |
| `/api/v1/ws` | WebSocket upgrades produce noise |
| `/api/v1/health` | Health check polling (high frequency) |
| `/api/v1/satellites/:id/logs/stream` | SSE stream (long-lived connection) |

**Request ID propagation:**

1. Check incoming `X-Request-ID` header (from load balancers or upstream services)
2. If absent, generate via `reqid.NewID()`
3. Set `X-Request-ID` on the response
4. Store in `c.Locals("request_id")` for handler access

**Helper functions:**

```go
// requestIDFromFiber extracts the request ID stored by the middleware.
func requestIDFromFiber(c *fiber.Ctx) string {
    if id, ok := c.Locals("request_id").(string); ok {
        return id
    }
    return ""
}

// loggerFromFiber returns a request-scoped sub-logger with request ID.
func loggerFromFiber(c *fiber.Ctx) zerolog.Logger {
    id := requestIDFromFiber(c)
    if id == "" {
        return log.Logger
    }
    return log.With().Str("request_id", id).Logger()
}
```

**Sample output:**
```
12:34:56 INF http request request_id=550e8400-e29b-41d4-a716-446655440000 method=POST path=/api/v1/clusters status=201 latency=12.3ms client_ip=10.0.1.5
```

### 1.5 Error Handler Enhancement

The existing `fiberErrorHandler` at `rest.go:2218` is enhanced to:
1. Include `request_id` in the JSON error response: `{"error": "...", "request_id": "..."}`
2. Log the error with request ID context

---

## Phase 2: gRPC Observability

### 2.1 gRPC Logging Interceptors

Switch from single interceptors to chained interceptors, separating logging from auth:

```go
grpc.ChainUnaryInterceptor(
    srv.unaryLoggingInterceptor,    // logging (runs first)
    srv.unaryAuthInterceptor,       // auth (runs second)
),
grpc.ChainStreamInterceptor(
    srv.streamLoggingInterceptor,
    srv.streamAuthInterceptor,
),
```

**Unary logging interceptor:**

| Field | Source | Example |
|-------|--------|---------|
| `request_id` | `x-request-id` metadata or generated | `"550e8400-..."` |
| `grpc_method` | `info.FullMethod` | `"/pgswarm.v1.RegistrationService/Register"` |
| `grpc_code` | `status.Code(err)` | `"OK"` |
| `duration` | `time.Since(start)` | `"5.2ms"` |
| `satellite_id` | From context (post-auth) | `"uuid-of-satellite"` |

Log levels:
- Successful calls: `Debug`
- Registration RPCs: `Info` (lifecycle events)
- `InvalidArgument`, `NotFound`: `Warn`
- `Internal`, `Unavailable`: `Error`

**Stream logging interceptor:**

Logs stream lifecycle events:
- **Stream open**: `Info` level with satellite_id, method
- **Stream close**: `Info` level with satellite_id, duration, close reason

This formalizes the existing ad-hoc connect/disconnect logs with consistent structured fields.

### 2.2 Sidecar Server Interceptor

Same pattern applied to the satellite's sidecar gRPC server (`internal/satellite/sidecar/server.go`) for sidecar connection lifecycle logging.

---

## Phase 3: Audit Logging

### 3.1 Approach: Structured Log Entries

Audit events are emitted as structured zerolog entries with the marker `Str("audit", "true")`, rather than stored in a separate database table.

**Rationale:**
- Zero migration cost, no new Store methods or REST endpoints required
- Immediately filterable in any log aggregation system (Loki, Elasticsearch, CloudWatch)
- The system already has an `events` table for operational events (cluster state changes); audit events serve a different audience (operators/security)
- Upgrade path: add a `zerolog.Hook` to write to a DB table when database-backed audit is needed

**Filtering examples:**
```bash
# Local grep
grep '"audit":"true"' /var/log/pg-swarm-central.log

# Loki (Grafana)
{job="pg-swarm-central"} | json | audit="true"

# Elasticsearch
{"query": {"term": {"audit": "true"}}}
```

### 3.2 Audit Log Format

```go
func auditLog(c *fiber.Ctx, action, resourceType, resourceID, resourceName, detail string) {
    log.Info().
        Str("audit", "true").
        Str("action", action).
        Str("request_id", requestIDFromFiber(c)).
        Str("resource_type", resourceType).
        Str("resource_id", resourceID).
        Str("resource_name", resourceName).
        Str("client_ip", c.IP()).
        Str("detail", detail).
        Msg("audit")
}
```

**Sample output:**
```
12:34:56 INF audit audit=true action=cluster.switchover request_id=550e8400-... resource_type=cluster resource_id=a1b2c3d4-... resource_name=prod-pg-01 client_ip=10.0.1.5 detail=target_pod=pg-cluster-1
```

### 3.3 Audit-Worthy Endpoints

#### Critical (operational impact)

| Endpoint | Action | What to log |
|----------|--------|-------------|
| `POST /satellites/:id/approve` | `satellite.approve` | Satellite hostname, K8s cluster |
| `POST /satellites/:id/reject` | `satellite.reject` | Satellite hostname |
| `POST /clusters/:id/switchover` | `cluster.switchover` | Cluster name, target pod |
| `POST /clusters/:id/pause` | `cluster.pause` | Cluster name |
| `POST /clusters/:id/resume` | `cluster.resume` | Cluster name |
| `DELETE /clusters/:id` | `cluster.delete` | Cluster name, satellite |
| `POST /clusters/:id/apply` | `cluster.apply` | Cluster name, config version |

#### Important (configuration changes)

| Endpoint | Action |
|----------|--------|
| `POST /clusters` | `cluster.create` |
| `PUT /clusters/:id` | `cluster.update` |
| `POST /profiles` | `profile.create` |
| `PUT /profiles/:id` | `profile.update` |
| `DELETE /profiles/:id` | `profile.delete` |
| `POST /profiles/:id/clone` | `profile.clone` |
| `POST /profiles/:id/apply` | `profile.apply` |
| `POST /profiles/:id/revert` | `profile.revert` |
| `POST /deployment-rules` | `deployment_rule.create` |
| `PUT /deployment-rules/:id` | `deployment_rule.update` |
| `DELETE /deployment-rules/:id` | `deployment_rule.delete` |
| `PUT /satellites/:id/labels` | `satellite.update_labels` |

#### Administrative

| Endpoint | Action |
|----------|--------|
| `POST/PUT/DELETE /storage-tiers/*` | `storage_tier.create/update/delete` |
| `POST/PUT/DELETE /postgres-versions/*` | `pg_version.create/update/delete` |
| `POST /postgres-versions/:id/default` | `pg_version.set_default` |
| `POST/DELETE /postgres-variants/*` | `pg_variant.create/delete` |
| `POST/PUT/DELETE /event-rule-sets/*` | `event_rule_set.create/update/delete` |
| `POST/DELETE /pg-param-classifications/*` | `pg_param.upsert/delete` |
| `POST /clusters/:id/databases` | `cluster_database.create` |
| `PUT /clusters/:id/databases/:dbid` | `cluster_database.update` |
| `DELETE /clusters/:id/databases/:dbid` | `cluster_database.delete` |
| `POST /satellites/:id/log-level` | `satellite.set_log_level` |

### 3.4 Future: Database-Backed Audit Table

When a persistent, queryable audit trail is needed:

```sql
CREATE TABLE audit_log (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    action        TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id   TEXT,
    resource_name TEXT,
    client_ip     TEXT,
    request_id    TEXT,
    detail        TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_audit_log_created_at ON audit_log (created_at DESC);
CREATE INDEX idx_audit_log_resource ON audit_log (resource_type, resource_id);
```

Add `CreateAuditEntry(ctx, entry)` to the `Store` interface and call from the `auditLog` helper alongside the structured log entry.

---

## Phase 4: Cross-Service Trace Propagation

### 4.1 The Problem

A user clicks "Switchover" in the dashboard. This triggers:
1. REST `POST /clusters/:id/switchover` (central)
2. `StreamManager.PushSwitchover()` sends `CentralMessage` to satellite via gRPC stream
3. Satellite's `OnSwitchover` callback runs switchover steps
4. Satellite sends `SidecarCommand` (fence, promote, unfence) to sentinel sidecar
5. Satellite sends `SwitchoverProgress` and `SwitchoverResult` back to central
6. Central broadcasts progress to dashboard via WebSocket

Today, each tier logs independently with no shared identifier linking these events.

### 4.2 Proto-Level Trace ID

Add `trace_id` to the protobuf message wrappers (not gRPC metadata, because the stream is long-lived and metadata is set once at connection time):

```protobuf
message CentralMessage {
  oneof payload { ... }
  string trace_id = 8;  // propagated from originating REST request
}

message SatelliteMessage {
  oneof payload { ... }
  string trace_id = 13;  // echoed from CentralMessage or originated by satellite
}

// In SidecarCommand:
string trace_id = N;  // propagated from satellite
```

### 4.3 Propagation Flow

```
Dashboard
  | POST /clusters/:id/switchover
  v
Central REST handler
  | request_id = "abc-123" (from middleware)
  | auditLog(action="cluster.switchover", request_id="abc-123")
  v
StreamManager.PushSwitchover(req, traceID="abc-123")
  | CentralMessage { switchover: {...}, trace_id: "abc-123" }
  v
Satellite stream.Connector receives CentralMessage
  | Extracts trace_id from CentralMessage
  | Sets in context: reqid.WithRequestID(ctx, "abc-123")
  | Calls OnSwitchover(req) with enriched context
  v
Satellite operator runs switchover steps
  | All logs include: Str("trace_id", "abc-123")
  | SidecarCommand { ..., trace_id: "abc-123" }
  v
Sentinel sidecar executes fence/promote/unfence
  | All logs include: Str("trace_id", "abc-123")
  | Returns CommandResult
  v
Satellite sends back to central:
  | SatelliteMessage { switchover_progress: {...}, trace_id: "abc-123" }
  | SatelliteMessage { switchover_result: {...}, trace_id: "abc-123" }
  v
Central gRPC handler
  | Logs progress with trace_id="abc-123"
  | WebSocket broadcast includes trace_id="abc-123"
  v
Dashboard receives progress with trace_id for correlation
```

### 4.4 Querying a Full Trace

```bash
# Find all log entries for a specific operation across all components
grep '"abc-123"' /var/log/pg-swarm-*.log

# Loki query across all components
{job=~"pg-swarm-.*"} | json | request_id="abc-123" or trace_id="abc-123"
```

---

## Log Level Guidelines

### Level Definitions

| Level | When to Use | Production Default |
|-------|-------------|-------------------|
| **Trace** | Step-by-step execution flow, function entry/exit | Off |
| **Debug** | Routine operational data, individual data points | Off |
| **Info** | Significant lifecycle events, state changes, mutations, audit entries | On |
| **Warn** | Recoverable issues, degraded operation, non-blocking failures | On |
| **Error** | Failures requiring attention, unrecoverable errors in a request | On |
| **Fatal** | Process cannot continue (startup only) | On |

### Level Usage Examples

**Trace:**
- `"handleConfig entry"`, `"heartbeat sending"`, `"reconcile loop tick"`
- Step-by-step switchover progress within the satellite
- Individual SQL query execution in store methods

**Debug:**
- `"health report processed"`, `"heartbeat ack received"`
- GET request logging (high-volume reads)
- WebSocket client connected/disconnected
- Config diff details during reconciliation

**Info:**
- `"satellite connected"`, `"satellite disconnected"`
- `"cluster config created"`, `"switchover completed"`
- All audit log entries
- POST/PUT/DELETE request logging
- Log level changes, component startup/shutdown

**Warn:**
- `"send channel full, dropping message"`
- `"config ack received with error"` (satellite reporting a problem)
- 4xx HTTP responses
- Invalid input from external systems
- Stream reconnection attempts

**Error:**
- `"failed to connect to database"` (during operation, not startup)
- Store operation failures returning 500
- gRPC send errors on established streams
- 5xx HTTP responses

**Fatal:**
- `"failed to connect to database"` (at startup)
- Missing required environment variables
- Invalid encryption key

### Configuration

Set via environment variable on any binary:

```bash
# All binaries
LOG_LEVEL=debug  # trace, debug, info, warn, error

# Satellite only: dynamic change via REST API
POST /api/v1/satellites/:id/log-level
{"level": "debug"}
```

---

## Design Decisions

### 1. Structured logs for audit (not separate DB table)

Audit events are zerolog entries with `"audit":"true"`, not rows in an `audit_log` table.

**Why:** Zero migration cost, immediately available, filterable by any log aggregation tool. The `events` table already tracks operational events for the dashboard; audit events serve operators/security.

**Upgrade path:** Add a `zerolog.Hook` that writes to both the log output and a DB table when database-backed audit becomes necessary.

### 2. Request ID in `c.Locals` (not `c.UserContext`)

GoFiber v2's `c.Context()` returns `*fasthttp.RequestCtx`, not a standard `context.Context`. Using `c.Locals("request_id")` is the idiomatic GoFiber approach and avoids subtle context-wrapping issues.

### 3. Proto-level `trace_id` (not gRPC metadata)

The central-satellite stream (`SatelliteStreamService.Connect`) is a long-lived bidirectional connection. gRPC metadata is set once at stream establishment, making it unsuitable for per-message tracing. Proto-level fields on `CentralMessage`/`SatelliteMessage` carry the trace_id per message.

This is consistent with the existing `SwitchoverRequest.operation_id` and `SidecarCommand.request_id` patterns already in the protobuf definitions.

### 4. No OpenTelemetry (yet)

The proposed approach achieves ~80% of distributed tracing value at ~20% of the complexity. Full OTel integration (spans, baggage, exporters, collector) is a separate initiative. The `trace_id` field provides a clear upgrade path: replace UUID strings with OTel trace IDs when the infrastructure is ready.

### 5. No request/response body logging

REST request and response bodies are not logged because:
- Bodies may contain passwords (`ClusterDatabase.password`, encrypted secrets)
- Large JSON configs would bloat log storage
- The audit log captures the semantic action and resource identity, which is more useful than raw payloads

---

## Implementation Files

### Phase 1 (Foundation)

| File | Change |
|------|--------|
| `internal/shared/reqid/reqid.go` | NEW - Request ID context package |
| `internal/shared/loglevel/loglevel.go` | NEW - Shared log level management |
| `internal/central/server/middleware.go` | NEW - HTTP request logging + audit helpers |
| `cmd/central/main.go` | MODIFY - LOG_LEVEL + component tag |
| `cmd/satellite/main.go` | MODIFY - Component tag |
| `cmd/sentinel-sidecar/main.go` | MODIFY - LOG_LEVEL support |
| `internal/satellite/logcapture/level.go` | MODIFY - Delegate to shared loglevel |
| `internal/central/server/rest.go` | MODIFY - Register middleware in setupRoutes |

### Phase 2 (gRPC Observability)

| File | Change |
|------|--------|
| `internal/central/server/grpc.go` | MODIFY - Chain logging interceptors, request ID propagation |
| `internal/satellite/sidecar/server.go` | MODIFY - Add logging interceptor |

### Phase 3 (Audit Logging)

| File | Change |
|------|--------|
| `internal/central/server/middleware.go` | MODIFY - Add auditLog helper |
| `internal/central/server/rest.go` | MODIFY - Add ~25 auditLog calls in mutation handlers |

### Phase 4 (Cross-Service Tracing)

| File | Change |
|------|--------|
| `api/proto/v1/config.proto` | MODIFY - Add trace_id fields |
| `api/gen/v1/*.pb.go` | REGENERATE via `make proto` |
| `internal/central/server/grpc.go` | MODIFY - Populate trace_id on outbound messages |
| `internal/central/server/rest.go` | MODIFY - Pass request_id to stream push calls |
| `internal/central/server/stream_manager.go` | MODIFY - Accept trace_id in Push methods |
| `internal/satellite/stream/connector.go` | MODIFY - Extract trace_id, propagate to callbacks |
| `internal/satellite/sidecar/stream_manager.go` | MODIFY - Include trace_id in SidecarCommand |

---

## Verification

### Phase 1
```bash
make build && make test && make lint
LOG_LEVEL=debug ./bin/central  # verify debug logs appear
curl -v http://localhost:8080/api/v1/clusters  # check X-Request-ID header + request log
```

### Phase 2
```bash
# Connect a satellite, verify gRPC logs:
# INF grpc stream opened satellite_id=... method=/pgswarm.v1.SatelliteStreamService/Connect
# INF grpc stream closed satellite_id=... duration=5m32s
```

### Phase 3
```bash
# Create a cluster, verify audit entry:
curl -X POST http://localhost:8080/api/v1/clusters -d '...'
# INF audit audit=true action=cluster.create request_id=... resource_type=cluster ...
grep '"audit":"true"' /dev/stderr  # only mutation events
```

### Phase 4
```bash
# Trigger switchover, verify trace_id appears across all tiers:
curl -X POST http://localhost:8080/api/v1/clusters/:id/switchover -d '{"target_pod":"pg-1"}'
# Central:   INF audit action=cluster.switchover request_id=abc-123 ...
# Satellite: INF switchover started trace_id=abc-123 ...
# Sidecar:   INF fence command executed trace_id=abc-123 ...
# Central:   INF switchover complete trace_id=abc-123 ...
```
