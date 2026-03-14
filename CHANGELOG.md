# Changelog

## Unreleased

### Automatic failover recovery

- Added timeline divergence detection and automatic recovery (pg_rewind with pg_basebackup fallback) so replicas re-sync after a primary promotion without manual intervention.
- Postgres container now runs inside a supervisor loop that catches exits from sidecar-initiated demotion or timeline crashes, recovers in-place, and restarts PG — eliminating Kubernetes container restart counts.
- pg_rewind uses the postgres superuser to avoid permission errors with `pg_read_binary_file()`.

### PostgreSQL configuration

- Added `shared_preload_libraries = 'pg_stat_statements'` and `pg_stat_statements.track = all` to mandatory params, with `CREATE EXTENSION` in the primary init script.
- Added `recovery_target_timeline = 'latest'` and `wal_keep_size = '512MB'` to mandatory params for reliable timeline following after failover.

### Failover sidecar

- WAL receiver health monitoring: the sidecar checks `pg_stat_wal_receiver` each tick and triggers recovery if streaming is down beyond a 30-second grace period.
- Timeline divergence detection via `pg_control_checkpoint()` and WAL history file checks — skips the grace period for fatal divergence.
- Rewind/re-basebackup via K8s exec with injectable `rewindFunc` for testing.

### Documentation

- Added project README with architecture overview, module descriptions, current state, quick start guide, and roadmap.
- Added CHANGELOG.md.

---

## 2026-03-14

### SQL fencing and split-brain prevention

- Added `pgfence` package (`internal/shared/pgfence/`): `FencePrimary()` blocks writes via `ALTER SYSTEM SET default_transaction_read_only`, reloads config, and kills client backends. `UnfencePrimary()` reverses it. `IsFenced()` checks state.
- Failover monitor now detects split-brain (PG running as primary but another pod holds the lease), fences immediately, then demotes via K8s exec (creates `standby.signal`, sets `primary_conninfo`, stops PG).
- Legitimate primaries that were previously fenced are automatically unfenced when they reacquire the lease.

### Health monitoring system

- New `internal/satellite/health/` package with per-cluster health collection on a 10-second loop.
- Metrics collected: `pg_isready`, replication lag (bytes + seconds), connection count vs `max_connections`, disk usage, WAL on-disk size, timeline ID, PG start time, WAL receiver status, WAL statistics (`pg_stat_wal`).
- Per-database stats: database sizes, cache hit ratio from `pg_statio_user_tables`, collected by connecting to each user database individually.
- Per-table stats: live/dead tuples, seq/idx scans, inserts/updates/deletes, last vacuum, table size.
- Slow queries from `pg_stat_statements` (top 10 by mean execution time, gracefully skipped if extension not loaded).
- Cluster state derivation: all instances ready = RUNNING, otherwise DEGRADED.

### Planned switchover

- Central-initiated switchover via `POST /clusters/:id/switchover`: verifies target pod exists and is a caught-up replica, runs CHECKPOINT on the current primary, fences old primary, transfers leader lease, promotes target via `pg_promote()`.
- Old primary's failover sidecar detects the lease change and auto-demotes on next tick.

### Cluster pause/resume

- New `POST /clusters/:id/pause` and `POST /clusters/:id/resume` endpoints.
- Paused clusters skip state sync from health reports (user-controlled state preserved).

### PostgreSQL version registry

- New `postgres_versions` table and REST endpoints (`GET/POST/PUT/DELETE /postgres-versions`, `POST /postgres-versions/:id/default`).
- Default version pre-selected when creating new profiles.

### Satellite storage class discovery

- Satellites report available storage classes. `POST /satellites/:id/refresh-storage-classes` triggers re-discovery.

### Dashboard enhancements

- Clusters page: instance table with role badges, ready/WAL status dots, connection bars, disk usage breakdown, switchover buttons, pause/resume controls, recent cluster events.
- Admin page for PostgreSQL version registry management.
- Events page with severity icons (info, warning, error, critical).
- Improved Badge component with semantic icons (CheckCircle2, Loader, AlertCircle, Pause, XCircle).
- Enhanced Layout with gradient topbar, icon navigation, satellite status pill.

### Kubernetes deployment

- Reorganized `deploy/k8s/` into kustomize base + overlay structure (central and satellite separated).
- Added minikube-specific overlays with patches.

### Failover monitor testing

- Added `internal/failover/monitor_test.go`: split-brain detection, lease acquisition, lease error handling.
- Added `internal/satellite/health/monitor_test.go`.

---

## 2026-02-08

### Deployment rules with label selectors

- Replaced deployment groups with deployment rules. Each rule maps a profile to satellites via label selectors (`map[string]string`).
- When a rule is created or a new satellite matches the selector, cluster configs are auto-created and pushed.
- Satellite labels stored in JSONB, editable via `PUT /satellites/:id/labels`.
- K8s resource labels now include `pg-swarm.io/profile` and `pg-swarm.io/selector-<key>` flattened from deployment rule selectors.

### Cluster profiles

- Profiles are reusable cluster templates stored as JSONB (PG version, storage, resources, PG params, HBA rules, failover, WAL archiving, databases).
- REST endpoints: `GET/POST/PUT/DELETE /profiles`, `POST /profiles/:id/clone`, `POST /profiles/:id/lock`.
- Profiles lock after first deployment to prevent accidental changes to running clusters.

### WAL storage support

- Added `wal_storage` field to ClusterConfig proto for separate WAL volumes.
- Operator creates a dedicated VolumeClaimTemplate and symlinks `pg_wal` to the separate volume in the init container.

### Dashboard pages

- Profiles page: 6-tab editor (General, Volumes, Resources, PostgreSQL params, HBA Rules, Databases) with profile cloning and locking.
- Deployment Rules page: rule CRUD with expandable cards showing profile summary and matched clusters.
