# TODO

## Roadmap

### Database-level pause (planned)

Add a "database pause" feature distinct from the current operator pause. The operator pause stops config reconciliation but leaves PostgreSQL serving traffic. A database pause should gracefully stop the database from serving application traffic while keeping replication alive.

**Implementation plan:**

1. Set read-only mode (`ALTER SYSTEM SET default_transaction_read_only = on` + reload)
2. Set connection limit to 0 (`ALTER DATABASE ... CONNECTION LIMIT 0`) to block new connections
3. Terminate existing client connections (preserve replication/walsender backends)
4. Keep WAL streaming active so replicas stay in sync
5. Resume: reset read-only, restore connection limit, reload

This reuses most of the existing `pgfence` package. Needs a new REST endpoint (`POST /clusters/:id/db-pause` / `POST /clusters/:id/db-resume`) and a corresponding gRPC downstream message to the satellite.

The dashboard should clearly distinguish between:
- **Operator paused** — management plane frozen, database running normally
- **Database paused** — database read-only and draining connections, replication active

### Zero-downtime switchover: TCP proxy sidecar vs PgBouncer (decision pending)

Currently, planned switchovers cause 2-5 seconds of application errors: the old primary is fenced (kills connections), the new primary is promoted, and the RW Service endpoint updates. Apps with retry logic handle this fine, but apps without retry see errors.

Two options to eliminate application errors during planned switchovers:

**Option A: Lightweight TCP proxy sidecar (simpler)**

A single-file Go proxy (~150 lines) that runs as a sidecar in each PG pod:

```
App → RW Service (:5432) → pg-proxy sidecar (:5432) → postgres (:5433)
```

Normal mode: transparent TCP passthrough, zero overhead, no SQL parsing.
Pause mode: accepts new connections but holds them in a waiting list. Resume connects them to the backend.

Control via localhost HTTP: `POST /pause`, `POST /resume`, `POST /drain`.

Switchover flow:
1. Satellite tells proxy: PAUSE — proxy holds new connections
2. Fence old primary, transfer lease, promote replica
3. New primary's proxy: RESUME — held connections go through
4. Old primary's proxy: DRAIN — existing connections get graceful close

What it solves:
- New connections during switchover wait instead of getting "connection refused"
- Idle connections don't see a disconnect/reconnect cycle

What it doesn't solve:
- Existing in-flight queries on the old primary still error (can't replay a half-executed transaction)
- Automatic failover (primary is dead, proxy can't buffer)
- Apps still need basic retry logic for writes during unplanned failures

**Option B: PgBouncer sidecar (full-featured)**

PgBouncer as a sidecar alongside each PG pod:

```
App → RW Service (:6432) → PgBouncer sidecar (:6432) → postgres (:5432)
```

PgBouncer's `PAUSE mydb` command holds client queries without dropping connections. `RESUME` releases queued queries. Apps see a slow query, no errors.

Additional benefits: connection pooling (multiplexes many app connections onto fewer PG backends), query queuing, auth passthrough.

Drawbacks: another process to configure, monitor, keep in sync with PG credentials. More complex to deploy and debug. Overkill if the only goal is switchover buffering.

**Recommendation:**

For most setups, neither is needed — the 2-5 second switchover window is handled by standard app retry logic (every ORM and connection pool does this). Consider adding only if:
- Edge clusters run legacy apps without retry logic
- SLA requires zero visible errors during planned maintenance
- Frequent planned switchovers (weekly recovery drills) make the cumulative error volume significant

If building, start with Option A (TCP proxy) — it's simpler, has no external dependencies, and covers the primary use case. Upgrade to PgBouncer only if connection pooling is also needed.

### Security hardening

- mTLS between central and satellites (central as CA, client certs during approval, cert rotation)
- REST API authentication (token-based or OIDC)
- RBAC for dashboard users (operator vs read-only roles)
- Satellite auth token rotation with zero-downtime handoff
- Audit logging for approval, config changes, switchovers

### Backup and recovery

- ~~Automated base backups to object storage (S3/GCS/MinIO) or PVC~~ Done
- ~~Continuous WAL archiving to S3-compatible backends~~ Done
- ~~Backup catalog with configurable retention policies~~ Done
- Point-in-time recovery (PITR) to a specific timestamp
- Cross-cluster restore (restore backup from one satellite onto another)
- Backup verification (periodic restore-to-temp + pg_checksums)

### Automatic recovery drills

- Scheduled failover drills on a configurable schedule (e.g., weekly)
- RTO measurement: time from primary death to new primary accepting writes
- Replica divergence drills: deliberately create timeline divergence and verify pg_rewind recovery
- Network partition simulation: test split-brain detection and fencing
- Backup restore drills: periodically restore latest backup to temp cluster and validate
- Drill history dashboard page with pass/fail trends and mean RTO

### Deferred code review items

- Connection string injection in `cmd/failover-sidecar/main.go` (password in libpq string — use pgx config struct)
- `CheckApproval` token regeneration race in registry (repeated polling invalidates tokens)
- Non-atomic satellite approve (token set + state update should be transactional)
- `resource.MustParse` panics on malformed config (add validation layer in operator)
- SQL injection in `buildDatabaseSQL` (sanitize user/database names from central)
- `context.Background()` in agent switchover/K8s calls (should use derived context)
- No rollback on partial switchover failure (lease transferred but promotion fails)
