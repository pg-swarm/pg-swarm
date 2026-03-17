# pg-swarm: Boot & Failover Sequence of Operations

This document describes the exact sequence of operations during initial boot,
replica boot, and failover promotion. It covers every container, shared volume,
shell command, and inter-component interaction.

---

## Pod Anatomy

Each PostgreSQL pod in the StatefulSet contains:

| Container       | Image                            | Condition                    | Purpose                                      |
|-----------------|----------------------------------|------------------------------|----------------------------------------------|
| `pg-init`       | `postgres:17-alpine`             | Always                       | Init container — bootstraps PGDATA           |
| `postgres`      | `postgres:17-alpine`             | Always                       | Main container — PG with restart-loop wrapper |
| `failover`      | `pg-swarm-failover:latest`       | Only if `Failover.Enabled`   | Sidecar — leader lease + promotion           |
| `backup`        | `pg-swarm-backup-sidecar:latest` | Only if `Backups` configured | Sidecar — WAL archiving + backups            |

### Role Labeling

Pod role labels (`pg-swarm.io/role = primary|replica`) control which K8s service
routes traffic to a pod. There are **two actors** that set these labels:

1. **Operator (`labelPods`)** — runs during every reconcile. Assigns initial labels
   to pods that have no role label yet: ordinal 0 = `primary`, others = `replica`.
   Never overwrites an existing label.

2. **Failover sidecar** (if present) — takes authority after initial labeling.
   Queries `pg_is_in_recovery()` on every tick and patches the pod label to
   match the actual PostgreSQL state. Handles promotion and demotion.

Without the failover sidecar, the operator's initial labeling is permanent.
There is **no automatic failover** — manual intervention is required.

### Shared Volumes

| Volume         | Type       | Condition        | Mount Path (postgres)       | Mount Path (backup) | Purpose                                           |
|----------------|------------|------------------|-----------------------------|----------------------|----------------------------------------------------|
| `data`         | PVC        | Always           | `/var/lib/postgresql/data`  | —                    | PGDATA (persistent)                                |
| `config`       | ConfigMap  | Always           | `/etc/pg-config` (RO)       | —                    | `postgresql.conf` + `pg_hba.conf`                  |
| `secret`       | Secret     | Always           | (env vars only)             | (env vars only)      | Passwords for superuser, replication, backup        |
| `wal`          | PVC        | If `wal_storage` | `/var/lib/postgresql/wal`   | —                    | Separate WAL volume                                |
| `wal-staging`  | emptyDir   | If `Backups`     | `/wal-staging`              | `/wal-staging`       | PG writes WAL here via `archive_command`; sidecar picks up, compresses, uploads, deletes |
| `wal-restore`  | emptyDir   | If `Backups`     | `/wal-restore`              | `/wal-restore`       | PG requests WAL via `.request` file; sidecar downloads and places the segment here       |

### Key Commands in postgresql.conf

```
archive_command = 'cp %p /wal-staging/%f'
restore_command = 'test -f /wal-restore/%f && cp /wal-restore/%f %p && exit 0; echo %f > /wal-restore/.request; for i in 1 2 ... 30; do sleep 1; test -f /wal-restore/%f && cp /wal-restore/%f %p && rm -f /wal-restore/%f && exit 0; test -f /wal-restore/.error && rm -f /wal-restore/.error && exit 1; done; exit 1'
```

These use only `cp`, `test`, `echo`, `sleep`, `rm` — all available in every postgres image. No `curl` needed.

---

## Scenario 1: Initial Boot (Primary — Ordinal 0)

### Phase 1: Init Container (`pg-init`)

The init container runs to completion before any other container starts.

```
t=0  Init container starts
     ├── Compute: ORDINAL = ${POD_NAME##*-}  →  "0"
     ├── Set: PGDATA = /var/lib/postgresql/data/pgdata
     ├── Set: PRIMARY_HOST = <cluster>-rw.<namespace>.svc.cluster.local
     │
     ├── Check: does $PGDATA/PG_VERSION exist?
     │   └── NO (first boot)
     │
     ├── Check: ORDINAL == "0"?
     │   └── YES → Initialize as primary
     │
     ├── Run: initdb -D $PGDATA --auth-local=trust --auth-host=md5
     │
     ├── Run: pg_ctl -D $PGDATA start -w -o "-c listen_addresses='localhost'"
     │   (starts PG temporarily, listening only on localhost)
     │
     ├── Run SQL:
     │   ├── ALTER USER postgres PASSWORD '$POSTGRES_PASSWORD'
     │   ├── CREATE ROLE repl_user WITH REPLICATION LOGIN PASSWORD '$REPLICATION_PASSWORD'
     │   ├── CREATE ROLE backup_user WITH REPLICATION LOGIN PASSWORD '$BACKUP_PASSWORD' IN ROLE pg_read_all_data
     │   ├── CREATE EXTENSION IF NOT EXISTS pg_stat_statements
     │   └── (if databases configured) CREATE ROLE <user> ... CREATE DATABASE <db> ...
     │
     ├── Run: pg_ctl -D $PGDATA stop -w
     │
     ├── Copy configs:
     │   ├── cp /etc/pg-config/postgresql.conf $PGDATA/postgresql.conf
     │   └── cp /etc/pg-config/pg_hba.conf $PGDATA/pg_hba.conf
     │
     ├── (if wal_storage) Move pg_wal to /var/lib/postgresql/wal + symlink
     │
     └── exit 0
```

### Phase 2: Main Container (`postgres`) — Restart-Loop Wrapper

The main container runs a bash wrapper that keeps PG alive across restarts.

```
t=1  Main container starts
     ├── Define pg_swarm_recover() function (timeline divergence recovery)
     ├── Set trap: SIGTERM → set SHUTTING_DOWN=true, forward to PG
     │
     ├── Enter main loop:
     │   ├── Call pg_swarm_recover()
     │   │   └── No standby.signal → skip (this is a primary)
     │   │
     │   ├── Check for corrupt PGDATA (files but no PG_VERSION) → skip (clean)
     │   │
     │   ├── Run: docker-entrypoint.sh postgres &
     │   │   (official postgres entrypoint, starts PG in background)
     │   │   PG_PID=$!
     │   │
     │   └── wait $PG_PID
     │       (blocks until PG exits or SIGTERM received)
     │
     ├── PG starts accepting connections on :5432
     │
     ├── Liveness probe begins (initial delay 30s):
     │   └── pg_isready -U postgres  (every 10s, fail after 6)
     │
     └── Readiness probe begins (initial delay 5s):
         └── pg_isready -U postgres  (every 5s, fail after 3)
```

### Phase 3: Role Labeling

The operator's `labelPods()` runs during reconciliation (before or shortly after
pod creation). It patches pods that have no `pg-swarm.io/role` label:

```
Operator reconcile:
     ├── List pods with label pg-swarm.io/cluster=<cluster>
     ├── For each pod without a role label:
     │   ├── If pod name == "<cluster>-0" → patch role = "primary"
     │   └── Otherwise → patch role = "replica"
     │
     └── This is best-effort; retried on next reconcile if pod isn't ready yet
```

After this, RW service routes to ordinal 0 and RO service routes to the rest.

### Phase 4: Failover Sidecar (`failover`) — ONLY IF `Failover.Enabled`

If the failover sidecar is not configured, skip this phase. The operator's
initial label is permanent, and there is no automatic failover.

When present, the failover sidecar starts in parallel with the main container
and takes over role labeling authority from the operator.

```
t=1  Failover sidecar starts
     │
     ├── Read config from env: CLUSTER_NAME, POD_NAME, POD_NAMESPACE, PRIMARY_HOST
     ├── Initialize K8s client (in-cluster config)
     │
     ├── Enter tick loop (every HEALTH_CHECK_INTERVAL seconds, default 1s):
     │
     │   Tick 1 (t≈2):
     │   ├── Connect to local PG (localhost:5432)
     │   │   └── May fail (PG still starting) → increment localPGDownCount, continue
     │   │
     │   Tick N (t≈5, PG ready):
     │   ├── Connect to local PG
     │   ├── Query: SELECT pg_is_in_recovery()
     │   │   └── Returns FALSE → this is a PRIMARY
     │   │
     │   ├── PRIMARY path:
     │   │   ├── Check crash-loop detection:
     │   │   │   └── localPGDownCount < 3 → proceed normally
     │   │   │
     │   │   ├── Try acquire/renew leader Lease in K8s:
     │   │   │   ├── Lease name: <cluster>-leader
     │   │   │   ├── Lease duration: 5s (default)
     │   │   │   ├── If lease doesn't exist → CREATE it, holder = this pod
     │   │   │   └── If lease exists and holder = this pod → RENEW (update renewTime)
     │   │   │
     │   │   ├── Lease acquired successfully:
     │   │   │   ├── Check if PG is fenced → unfence if so
     │   │   │   └── Patch pod label: pg-swarm.io/role = "primary"
     │   │   │       (RW service now routes traffic to this pod)
     │   │   │
     │   │   └── Continue ticking every 1s to renew lease
```

### Phase 5: Backup Sidecar (`backup`) — ONLY IF `Backups` Configured

If the backup sidecar is not configured, skip this phase. WAL archiving and
the shared `wal-staging`/`wal-restore` volumes are not present.

When present, starts in parallel. Waits for PG to become reachable.

```
t=1  Backup sidecar starts
     │
     ├── Read config from env: CLUSTER_NAME, DEST_TYPE, schedules, retention, etc.
     ├── Initialize destination (S3/GCS/SFTP/local)
     │
     ├── Detect role (retry up to 60s):
     │   ├── Connect to localhost:5432 as backup_user
     │   ├── Query: SELECT pg_is_in_recovery()
     │   │   └── Returns FALSE → RolePrimary
     │
     ├── Activate PRIMARY responsibilities:
     │   │
     │   ├── Download backups.db from destination (or create new)
     │   │   (SQLite metadata: backup sets, WAL segments, backup records)
     │   │
     │   ├── Ensure active backup set exists
     │   │   └── If none → create initial backup set
     │   │
     │   ├── Start HTTP API server on :8442
     │   │   ├── GET  /healthz          → {"status":"ok","role":"primary"}
     │   │   ├── POST /wal/push         → legacy HTTP WAL push (backward compat)
     │   │   ├── GET  /wal/fetch        → legacy HTTP WAL fetch (backward compat)
     │   │   └── POST /backup/complete  → receives backup notifications from replicas
     │   │
     │   ├── Start WatchWALStaging goroutine:
     │   │   └── Every 1s: scan /wal-staging/
     │   │       for each file (not dir, not dot-prefixed):
     │   │         1. gzip compress + upload to <dest>/<sat>-<cluster>/wal/<name>.gz
     │   │         2. Record in backups.db (WAL segment metadata)
     │   │         3. Delete local file from /wal-staging/
     │   │
     │   ├── Start WatchWALRestore goroutine:
     │   │   └── Every 500ms: check /wal-restore/.request
     │   │       if file exists:
     │   │         1. Read requested WAL name
     │   │         2. Download <dest>/<sat>-<cluster>/wal/<name>.gz
     │   │         3. Decompress to /wal-restore/<name>
     │   │         4. Delete /wal-restore/.request
     │   │         (on error: write /wal-restore/.error)
     │   │
     │   ├── Start retention worker (periodic cleanup of old backup sets)
     │   │
     │   └── Start reporter (writes status to K8s ConfigMap <cluster>-backup-status)
     │
     ├── Start role-change watcher:
     │   └── Every 10s: re-query pg_is_in_recovery()
     │       if role changed → deactivate old, activate new
     │
     └── Block until context cancelled
```

### Steady State (Primary)

```
STEADY STATE — Primary pod fully operational:

  PostgreSQL (:5432)
     ├── Accepting read/write connections via RW service
     ├── (if backup) WAL archiving active:
     │   └── archive_command = 'cp %p /wal-staging/%f'
     │       PG writes completed WAL segments to /wal-staging/
     └── Streaming replication to connected replicas

  Failover sidecar (if Failover.Enabled)
     └── Every 1s: renew leader Lease in K8s API, patch label role=primary

  Backup sidecar (if Backups configured)
     ├── WatchWALStaging: every 1s, pick up WAL from /wal-staging/
     │   → compress → upload to S3/GCS → record in backups.db → delete local
     ├── WatchWALRestore: every 500ms, check /wal-restore/.request
     │   → download + decompress WAL on demand (for recovery scenarios)
     └── /backup/complete endpoint: waiting for notifications from replicas

  K8s Services
     ├── <cluster>-rw   → selector: {cluster, role=primary} → THIS POD
     ├── <cluster>-ro   → selector: {cluster, role=replica} → replica pods
     └── <cluster>-headless → all pods (for DNS: pod-0, pod-1, ...)
```

---

## Scenario 2: Replica Boot (Ordinal 1+)

Assume the primary (ordinal 0) is already running and healthy.

### Phase 1: Init Container (`pg-init`)

```
t=0  Init container starts
     ├── Compute: ORDINAL = ${POD_NAME##*-}  →  "1" (or higher)
     ├── Set: PGDATA = /var/lib/postgresql/data/pgdata
     ├── Set: PRIMARY_HOST = <cluster>-rw.<namespace>.svc.cluster.local
     │
     ├── Check: does $PGDATA/PG_VERSION exist?
     │   └── NO (first boot)
     │
     ├── Check: needs-basebackup marker?
     │   └── NO
     │
     ├── ORDINAL != "0" OR NEEDS_BASEBACKUP → Initialize as replica
     │
     ├── Wait for primary:
     │   └── Loop: pg_isready -h $PRIMARY_HOST -U postgres
     │       (retries every 2s until primary is reachable via RW service)
     │
     ├── Run: PGPASSWORD=$REPLICATION_PASSWORD pg_basebackup \
     │       -h $PRIMARY_HOST -U repl_user -D $PGDATA -R -Xs -P
     │
     │   Flags:
     │     -R  → Create standby.signal + write primary_conninfo to postgresql.auto.conf
     │     -Xs → Stream WAL during backup (ensures consistency)
     │     -P  → Show progress
     │
     │   Result: $PGDATA is a full copy of the primary's data directory
     │           $PGDATA/standby.signal exists (marks this as a standby)
     │           $PGDATA/postgresql.auto.conf contains:
     │             primary_conninfo = 'host=<rw-svc> port=5432 user=repl_user password=...'
     │
     ├── Copy configs (override what pg_basebackup copied):
     │   ├── cp /etc/pg-config/postgresql.conf $PGDATA/postgresql.conf
     │   └── cp /etc/pg-config/pg_hba.conf $PGDATA/pg_hba.conf
     │
     ├── (if wal_storage) Set up WAL symlink
     │
     ├── Inject restore_command for archive recovery:
     │   └── echo "restore_command = '<wal-restore-script>'" >> $PGDATA/postgresql.auto.conf
     │       (the file-based script using /wal-restore/.request)
     │
     └── exit 0
```

### Phase 2: Main Container (`postgres`)

```
t=1  Main container starts
     ├── Enter restart-loop wrapper
     │
     ├── Call pg_swarm_recover():
     │   ├── standby.signal exists → check timeline
     │   ├── Get local timeline: pg_controldata → Latest checkpoint's TimeLineID
     │   ├── Wait for primary (up to 30s, 6 retries of 5s each)
     │   ├── Get primary timeline: psql → SELECT timeline_id FROM pg_control_checkpoint()
     │   ├── Compare: local == primary? → YES (both on timeline 1, fresh basebackup)
     │   └── No divergence → skip recovery
     │
     ├── Run: docker-entrypoint.sh postgres &
     │   PG starts in recovery mode (standby.signal present):
     │     1. Reads postgresql.auto.conf → connects to primary via primary_conninfo
     │     2. Starts streaming replication (WAL receiver)
     │     3. If any WAL gaps, uses restore_command to fetch from archive
     │
     └── wait $PG_PID (PG runs as standby, accepting read-only queries)
```

### Phase 3: Role Labeling

Same as primary: the operator's `labelPods()` patches this pod with
`pg-swarm.io/role = replica` (since ordinal != 0). The RO service begins
routing read traffic here.

### Phase 4: Failover Sidecar — ONLY IF `Failover.Enabled`

Without the failover sidecar, the replica runs PG in standby mode with
streaming replication but there is no monitoring for primary failure and
no automatic promotion.

When present:

```
t=1  Failover sidecar starts
     │
     ├── Tick loop begins:
     │
     │   Tick N (PG ready):
     │   ├── Connect to local PG
     │   ├── Query: SELECT pg_is_in_recovery()
     │   │   └── Returns TRUE → this is a REPLICA
     │   │
     │   ├── REPLICA path:
     │   │   ├── Patch pod label: pg-swarm.io/role = "replica"
     │   │   │   (confirms operator's initial label)
     │   │   │
     │   │   ├── Check WAL receiver status:
     │   │   │   └── Query: SELECT status FROM pg_stat_wal_receiver
     │   │   │       └── "streaming" → healthy, replication active
     │   │   │
     │   │   ├── Fast-path reachability check:
     │   │   │   └── TCP connect to PRIMARY_HOST:5432 + pg_isready
     │   │   │       └── Succeeds in <1s → primary is alive, skip lease check
     │   │   │
     │   │   └── Continue ticking (monitoring for primary failure)
```

### Phase 5: Backup Sidecar — ONLY IF `Backups` Configured

```
t=1  Backup sidecar starts
     │
     ├── Detect role:
     │   └── pg_is_in_recovery() → TRUE → RoleReplica
     │
     ├── Activate REPLICA responsibilities:
     │   │
     │   ├── Start HTTP API on :8442 (/healthz only)
     │   │
     │   ├── Start reporter
     │   │
     │   ├── Start notifier:
     │   │   └── Discovers primary address:
     │   │       <cluster>-0.<cluster>-headless.<namespace>.svc.cluster.local:8442
     │   │
     │   └── Start scheduler:
     │       │
     │       ├── Parse cron schedules from env:
     │       │   ├── BASE_SCHEDULE  = "0 2 * * *"   (daily at 2am)
     │       │   ├── INCR_SCHEDULE  = "0 */6 * * *" (every 6 hours)
     │       │   └── LOGICAL_SCHEDULE = "0 3 * * *"  (daily at 3am)
     │       │
     │       ├── Run initial base backup immediately (if BASE_SCHEDULE set):
     │       │   │
     │       │   ├── RunBaseBackup():
     │       │   │   ├── Create temp dir
     │       │   │   ├── Run: pg_basebackup -h localhost -U backup_user \
     │       │   │   │       -D <tmpdir> -Ft -z -Xs -P
     │       │   │   │   (-Ft=tar, -z=gzip, -Xs=stream WAL, -P=progress)
     │       │   │   ├── Upload base.tar.gz to destination:
     │       │   │   │   <sat>-<cluster>/base/<timestamp>.tar.gz
     │       │   │   ├── Upload backup_manifest (for incremental backups)
     │       │   │   └── Cleanup temp dir
     │       │   │
     │       │   ├── Notify primary:
     │       │   │   └── HTTP POST to primary:8442/backup/complete
     │       │   │       Body: {id, type:"base", filename, size_bytes, wal_start_lsn, ...}
     │       │   │       Retry: 5 attempts with exponential backoff (1s, 4s, 9s, 16s, 25s)
     │       │   │
     │       │   └── Report status to K8s ConfigMap
     │       │
     │       └── Enter cron tick loop:
     │           └── Every minute: check if any schedule matches → run backup
```

### Phase 5: Primary Receives Backup Notification

```
Primary's backup sidecar receives POST /backup/complete:
     │
     ├── Decode BackupCompleteRequest JSON
     │
     ├── If type == "base" (new base backup):
     │   ├── Seal current backup set in backups.db
     │   └── Create new backup set (with PG version + WAL start LSN)
     │
     ├── Get active backup set ID
     │
     ├── Insert backup record into backups.db:
     │   └── {id, set_id, type, filename, size, wal_range, status:"completed"}
     │
     ├── Insert backup stats (duration, throughput)
     │
     ├── Upload updated backups.db to destination
     │
     ├── If base backup → trigger retention worker:
     │   └── Delete old backup sets exceeding RETENTION_SETS count
     │       Delete WAL segments older than RETENTION_DAYS
     │
     └── Report status to K8s ConfigMap
```

### Steady State (Primary + Replica)

```
PRIMARY POD (ordinal 0):
  ┌──────────────────────────────────────────────────────────────┐
  │  postgres container                                          │
  │  ├── PG running as primary, accepting writes on :5432        │
  │  ├── Streaming WAL to connected replicas                     │
  │  ├── (if backup) archive_command: cp %p /wal-staging/%f      │
  │  │   └── completed WAL segments land in /wal-staging/        │
  │  └── Probes: pg_isready every 5-10s                          │
  ├──────────────────────────────────────────────────────────────┤
  │  failover sidecar  (only if Failover.Enabled)                │
  │  └── Every 1s: renew Lease, patch label role=primary         │
  ├──────────────────────────────────────────────────────────────┤
  │  backup sidecar  (only if Backups configured)                │
  │  ├── WatchWALStaging: /wal-staging/ → compress → upload → rm │
  │  ├── WatchWALRestore: /wal-restore/.request → download       │
  │  ├── HTTP :8442 /backup/complete → record metadata           │
  │  └── Retention worker: periodic cleanup                      │
  └──────────────────────────────────────────────────────────────┘
       ▲                          ▲
       │ /wal-staging/ (emptyDir) │ /wal-restore/ (emptyDir)
       │ PG writes → sidecar reads│ sidecar writes → PG reads
       ▼                          ▼
       (only present when backup sidecar is configured)

REPLICA POD (ordinal 1):
  ┌──────────────────────────────────────────────────────────────┐
  │  postgres container                                          │
  │  ├── PG running as hot standby, read-only queries on :5432   │
  │  ├── WAL receiver: streaming from primary via primary_conninfo│
  │  ├── (if backup) restore_command: request/poll /wal-restore/ │
  │  │   └── used when WAL gaps exist (archive recovery)         │
  │  └── Probes: pg_isready every 5-10s                          │
  ├──────────────────────────────────────────────────────────────┤
  │  failover sidecar  (only if Failover.Enabled)                │
  │  ├── Every 1s: check primary reachability (fast-path TCP)    │
  │  ├── Label: role=replica                                     │
  │  └── If primary unreachable 3+ ticks → check lease → promote │
  ├──────────────────────────────────────────────────────────────┤
  │  backup sidecar  (only if Backups configured)                │
  │  ├── WatchWALRestore: /wal-restore/.request → download       │
  │  │   └── needed for timeline history files after failover    │
  │  ├── Scheduler: fires pg_basebackup on cron schedule         │
  │  ├── Notifier: POST results to primary:8442/backup/complete  │
  │  └── Reporter: writes status to K8s ConfigMap                │
  └──────────────────────────────────────────────────────────────┘

K8s SERVICES (always present):
  <cluster>-rw       → selector {role=primary}  → routes to ordinal 0
  <cluster>-ro       → selector {role=replica}  → routes to ordinal 1+
  <cluster>-headless → all pods                 → DNS: pod-0, pod-1, ...
```

---

## Scenario 3: Failover — Replica Promotes to Primary

**Requires `Failover.Enabled`.** Without the failover sidecar, there is no
automatic promotion. The primary stays down until manually recovered.

Assume: primary (ordinal 0) crashes or becomes unreachable.
Both pods have the failover sidecar injected.

### Phase 1: Primary Failure Detection

```
t=0   Primary pod's PG crashes (or pod is deleted, or node goes down)

      PRIMARY'S FAILOVER SIDECAR (if still running):
      ├── Tick: connect to local PG → FAIL
      ├── localPGDownCount = 1, consecutiveHealthyTicks = 0
      │
      ├── Tick: connect to local PG → FAIL
      ├── localPGDownCount = 2
      │
      ├── Tick: connect to local PG → FAIL
      ├── localPGDownCount = 3  (≥ crashLoopThreshold)
      │
      └── STOP RENEWING LEASE
          (lease will expire in ≤5s since last renewal)
```

### Phase 2: Replica Detects Failure and Promotes

```
t≈3   REPLICA'S FAILOVER SIDECAR detects primary failure:
      │
      ├── Tick: query local PG → pg_is_in_recovery() = TRUE (still replica)
      │
      ├── REPLICA path:
      │   ├── Fast-path check: TCP connect to PRIMARY_HOST:5432
      │   │   └── FAIL (primary unreachable)
      │   ├── primaryUnreachableCount = 1
      │
      ├── Tick: fast-path check → FAIL
      │   primaryUnreachableCount = 2
      │
      ├── Tick: fast-path check → FAIL
      │   primaryUnreachableCount = 3  (≥ threshold)
      │
      │   NOW CHECK LEADER LEASE:
      │   ├── Read Lease from K8s API
      │   ├── Check: lease.Spec.RenewTime + LeaseDurationSeconds < now?
      │   │   └── YES → lease expired (primary stopped renewing ≥5s ago)
      │   │
      │   ├── Try to acquire lease:
      │   │   └── Update Lease: holder = this pod's name, renewTime = now
      │   │       (K8s atomic update — only one pod can win)
      │   │
      │   ├── LEASE ACQUIRED → THIS POD IS THE NEW PRIMARY
      │   │
      │   ├── PROMOTE POSTGRESQL:
      │   │   ├── Connect to local PG
      │   │   ├── Execute: SELECT pg_promote(true, 60)
      │   │   │   (true = wait for promotion, 60s timeout)
      │   │   ├── PG exits recovery mode
      │   │   ├── PG advances to timeline N+1
      │   │   └── PG starts accepting writes
      │   │
      │   └── Patch pod label: pg-swarm.io/role = "primary"
      │       ├── RW service now routes to THIS pod
      │       └── RO service stops routing to this pod

t≈8   NEW PRIMARY is fully operational
      Application connections via RW service now reach the promoted replica
```

### Phase 3: Backup Sidecar Role Switch

```
t≈10  BACKUP SIDECAR on promoted pod detects role change:
      │
      ├── Role-change watcher (every 10s):
      │   ├── Query: pg_is_in_recovery()
      │   │   └── Returns FALSE → RolePrimary (was RoleReplica)
      │   │
      │   ├── ROLE CHANGE DETECTED: replica → primary
      │   │
      │   ├── Deactivate replica responsibilities:
      │   │   ├── Stop scheduler (no more pg_basebackup jobs)
      │   │   ├── Stop API server
      │   │   └── Stop notifier
      │   │
      │   └── Activate primary responsibilities:
      │       ├── Download/create backups.db
      │       ├── Ensure active backup set exists
      │       ├── Start API server (:8442)
      │       ├── Start WatchWALStaging goroutine
      │       │   └── PG's archive_command (cp %p /wal-staging/%f) now works
      │       │       sidecar picks up, compresses, uploads
      │       ├── Start WatchWALRestore goroutine
      │       ├── Start retention worker
      │       └── Start reporter
```

### Phase 4: Old Primary Recovers (If Pod Restarts)

```
t≈30  Old primary pod restarts (K8s recreates it or PG restarts in-place)

      CASE A: Main container restart-loop detects the exit:
      ├── PG exited (code != 0, SHUTTING_DOWN = false)
      ├── Log: "postgres exited (code=X) — recovering in-place"
      ├── sleep 2
      │
      ├── Call pg_swarm_recover():
      │   ├── standby.signal exists? → may or may not
      │   │   If no standby.signal (was primary):
      │   │   └── Skip timeline check (handled by failover sidecar)
      │   │
      │   If failover sidecar already demoted this node:
      │   ├── standby.signal was created by demotion
      │   ├── Get local timeline: pg_controldata → timeline 1
      │   ├── Wait for new primary (via RW service DNS, now points to promoted replica)
      │   ├── Get primary timeline: → timeline 2
      │   ├── TIMELINE DIVERGENCE: local=1, primary=2
      │   │
      │   ├── Try pg_rewind:
      │   │   ├── Ensure clean shutdown (single-user recovery if needed)
      │   │   ├── pg_rewind -D $PGDATA \
      │   │   │     --source-server="host=<rw-svc> user=postgres ..."
      │   │   └── If succeeds: fast-forward data to match new primary
      │   │
      │   ├── If pg_rewind fails:
      │   │   ├── Check primary reachable (don't destroy data if isolated)
      │   │   ├── Full re-basebackup from new primary
      │   │   └── Copy configs
      │   │
      │   ├── Create standby.signal
      │   ├── Set primary_conninfo → new primary (via RW service)
      │   └── Log: "timeline recovery complete"
      │
      ├── Restart PG: docker-entrypoint.sh postgres &
      │   PG starts as standby, replicates from the new primary
      │
      └── PG is now a functioning replica

      CASE B: Pod fully deleted and recreated by StatefulSet:
      ├── Init container runs
      ├── PGDATA/PG_VERSION exists (data PVC preserved)
      ├── Idempotent path:
      │   ├── Copy configs
      │   ├── Detect timeline divergence
      │   ├── pg_rewind or re-basebackup
      │   └── Set up as standby
      └── Main container starts PG as replica

      FAILOVER SIDECAR on old primary (present since Failover.Enabled):
      ├── Tick: pg_is_in_recovery() → TRUE (now a replica)
      ├── Patch label: role=replica
      │   (RO service now routes to this pod too)
      │
      ├── Previously held lease but another pod now holds it
      │   └── Does NOT try to reclaim — respects current lease holder
      │
      └── Enters replica monitoring mode

      BACKUP SIDECAR on old primary (if Backups configured):
      ├── Detect role: RoleReplica
      ├── Activate replica responsibilities:
      │   ├── Start scheduler (backups now run here too)
      │   ├── Start notifier (notify new primary)
      │   └── Start reporter
```

### Phase 5: Split-Brain Prevention

If the old primary's PG somehow comes back as primary while the lease is held by another pod:

```
      OLD PRIMARY'S FAILOVER SIDECAR:
      ├── Tick: pg_is_in_recovery() → FALSE (PG thinks it's primary)
      │
      ├── PRIMARY path:
      │   ├── Try to renew lease
      │   │   └── FAIL: lease held by different pod (the promoted replica)
      │   │
      │   ├── SPLIT-BRAIN DETECTED
      │   │
      │   ├── FENCE immediately:
      │   │   └── pgfence: block all write operations at PG level
      │   │       (prevents any writes from going through)
      │   │
      │   ├── DEMOTE:
      │   │   ├── Create $PGDATA/standby.signal
      │   │   ├── Set primary_conninfo to RW service (new primary)
      │   │   ├── Remove primary_conninfo duplicates
      │   │   └── pg_ctl -D $PGDATA stop -m fast
      │   │       (stop PG — main container wrapper will restart it as standby)
      │   │
      │   └── Patch label: role=replica
      │
      └── Main container wrapper detects PG exit → pg_swarm_recover → restart as standby
```

---

## Timing Summary

| Event | Condition | Approximate Time |
|-------|-----------|-----------------|
| Primary init container completes | Always | ~5-10s |
| Primary PG accepting connections | Always | ~15-20s |
| Operator labels pod (role=primary) | Always | During reconcile |
| Failover sidecar acquires lease | Failover.Enabled | ~20s |
| Backup sidecar WAL watcher active | Backups configured | ~25s (waits for PG) |
| Replica init container (pg_basebackup) | Always | ~20-60s (data size) |
| Replica PG streaming from primary | Always | ~30-70s |
| Replica labeled and serving reads | Always | ~35-75s |
| **Failover detection** (crash → lease expiry) | Failover.Enabled | **~5-8s** |
| **Promotion** (lease → pg_promote → writes) | Failover.Enabled | **~1-3s** |
| **Total failover** (crash → new primary) | Failover.Enabled | **~8-15s** |
| Old primary recovery (pg_rewind) | After failover | ~10-30s |
| Old primary recovery (re-basebackup) | After failover | ~30-300s (data size) |
| Backup sidecar role switch | Backups configured | ≤10s (next role check) |

---

## WAL Archive/Restore Data Flow

```
ARCHIVE (primary writes WAL):

  PostgreSQL                /wal-staging/          Backup Sidecar           S3/GCS
  ─────────                 ─────────────          ──────────────           ──────
  WAL segment               emptyDir               WatchWALStaging
  complete                  (shared vol)           (polls every 1s)
      │                         │                       │
      ├── cp %p /wal-staging/%f │                       │
      │   ───────────────────►  │                       │
      │                         │  read file            │
      │                         │  ◄────────────────    │
      │                         │                       │
      │                         │  gzip + upload        │
      │                         │  ────────────────────►│
      │                         │                       │
      │                         │  record in backups.db │
      │                         │                       │
      │                         │  rm local file        │
      │                         │  ◄────────────────    │
      │                         │  (deleted)            │


RESTORE (replica or primary recovery needs WAL):

  PostgreSQL                /wal-restore/          Backup Sidecar           S3/GCS
  ─────────                 ─────────────          ──────────────           ──────
  restore_command           emptyDir               WatchWALRestore
  triggered                 (shared vol)           (polls every 500ms)
      │                         │                       │
      ├── test -f %f?           │                       │
      │   └── NO                │                       │
      │                         │                       │
      ├── echo %f > .request    │                       │
      │   ───────────────────►  │                       │
      │                         │  read .request        │
      │                         │  ◄────────────────    │
      │                         │                       │
      │                         │  download .gz         │
      │                         │  ◄───────────────────│
      │                         │                       │
      │                         │  decompress to %f     │
      │                         │  ────────────────►    │
      │                         │                       │
      │                         │  rm .request          │
      │                         │  ◄────────────────    │
      │                         │                       │
      ├── poll: test -f %f?     │                       │
      │   └── YES               │                       │
      │                         │                       │
      ├── cp %f %p              │                       │
      │   ◄─────────────────    │                       │
      │                         │                       │
      ├── rm %f                 │                       │
      └── exit 0                │                       │
```
