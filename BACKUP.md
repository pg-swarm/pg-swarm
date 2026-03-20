# Backup and Restore Architecture

## Overview

pg-swarm provides managed backup and restore through **backup profiles** — independent entities that attach/detach from cluster profiles. Each profile supports at most **one physical rule** (base + incremental + WAL archiving) and **one logical rule** (pg_dump). Each rule targets a single destination.

Backups are handled by a **pure sidecar model** — a `backup` container injected into every StatefulSet pod when backup profiles are attached. The sidecar detects its role (primary vs replica) and activates the appropriate responsibilities. No CronJobs are used.

## Data Flow

```
BackupProfile ──┐
BackupProfile ──┼─attach──> ClusterProfile ──DeploymentRule──> ClusterConfig
BackupProfile ──┘                                                    │
                                                    gRPC push (repeated BackupConfig)
                                                                  │
                                                           Satellite Operator
                                                                  │
                                                     Injects backup sidecar into
                                                     StatefulSet + sets archive_command
                                                                  │
                                     ┌────────────────────────────┤
                                     ▼                            ▼
                              Primary Pod                   Replica Pod
                          ┌──────────────────┐        ┌──────────────────┐
                          │ postgres         │        │ postgres         │
                          │ failover sidecar │        │ failover sidecar │
                          │ backup sidecar   │        │ backup sidecar   │
                          │   - WAL push API │        │   - pg_basebackup│
                          │   - backups.db   │        │   - pg_dump      │
                          │   - retention    │        │   - scheduler    │
                          └──────────────────┘        └──────────────────┘
                                     │                            │
                                     │    POST /backup/complete   │
                                     │◄───────────────────────────┘
                                     │
                                     ▼
                            BackupStatusReport (via ConfigMap → health monitor → gRPC)
                                     │
                              Central Server
                                     │
                         ┌───────────┴───────────┐
                         ▼                       ▼
                 backup_inventory        restore_operations
```

## Split-Responsibility Model

The backup sidecar detects its role by querying `pg_is_in_recovery()` and activates the appropriate responsibilities:

### Primary sidecar responsibilities:
1. **WAL archiving** — HTTP endpoint receives WAL from `archive_command`, compresses, uploads to `wal/`
2. **Metadata DB** — single writer to `backups.db` (SQLite), ingests backup completion notifications from replica
3. **WAL fetch** — serves `restore_command` requests (downloads WAL from destination for recovery)
4. **Retention** — deletes expired backups/WAL/logical dumps and cascades metadata

### Replica sidecar responsibilities:
1. **Base backups** — `pg_basebackup -h localhost` on schedule, uploads to `base/`
2. **Incremental backups** — `pg_basebackup --incremental` on schedule, uploads to `incremental/` (with standby WAL fallback)
3. **Logical backups** — `pg_dump`/`pg_dumpall` on schedule, uploads to `logical/`
4. **Notify primary** — after each backup, POST metadata to primary sidecar's HTTP API

### Role discovery and failover:
- Each sidecar checks `SELECT pg_is_in_recovery()` on startup and every 10 seconds
- **Primary** (`pg_is_in_recovery() = false`): activates WAL archiving + metadata + retention
- **Replica** (`pg_is_in_recovery() = true`): activates backup scheduler
- On failover (role change detected): sidecar switches responsibilities automatically

### Single-replica or standalone (replicas=1):
- The single pod is the primary — sidecar handles everything: WAL + backups + metadata
- No cross-pod communication needed

## Destination Folder Structure

Every satellite-cluster combination gets a dedicated folder:

```
backup-destination/
├── sat1-c1/                                    # {satelliteID}-{clusterName}
│   ├── backups.db                              # SQLite metadata (chain reconstructor)
│   ├── base/
│   │   ├── 20260317T000000Z.tar.gz
│   │   └── 20260317T000000Z_manifest.gz
│   ├── incremental/
│   │   ├── 20260317T060000Z.tar.gz
│   │   └── 20260317T060000Z_manifest.gz
│   ├── wal/
│   │   ├── 000000010000000000000001.gz
│   │   └── 000000010000000000000002.gz
│   └── logical/
│       └── 20260317T030000Z_mydb.sql.gz
└── sat2-c1/
    ├── backups.db
    └── ...
```

## archive_command / restore_command

When backup profiles are attached, the operator configures PostgreSQL to use the sidecar:

```
archive_command = 'curl -sf -X POST -F file=@%p -F name=%f http://localhost:8442/wal/push'
restore_command = 'curl -sf -o %p http://localhost:8442/wal/fetch?name=%f'
```

- `archive_command` POSTs WAL to the local backup sidecar. Blocks until durable upload. PG only marks WAL as archived after curl returns 0.
- `restore_command` fetches WAL from the sidecar, which downloads from the destination.

## Sidecar HTTP API (:8442)

| Method | Path | Role | Purpose |
|--------|------|------|---------|
| POST | `/wal/push` | Primary | Receives WAL from archive_command, compresses, uploads |
| GET | `/wal/fetch?name=` | Primary | Serves WAL for restore_command |
| POST | `/backup/complete` | Primary | Receives backup metadata from replica |
| GET | `/healthz` | Both | Health check |

## SQLite Metadata Schema (`backups.db`)

```sql
CREATE TABLE backup_sets (
    id              TEXT PRIMARY KEY,
    started_at      TEXT NOT NULL,
    sealed_at       TEXT,
    status          TEXT DEFAULT 'active',    -- active | sealed
    pg_version      TEXT,
    wal_start_lsn   TEXT,
    wal_end_lsn     TEXT
);

CREATE TABLE backups (
    id              TEXT PRIMARY KEY,
    set_id          TEXT NOT NULL REFERENCES backup_sets(id) ON DELETE CASCADE,
    type            TEXT NOT NULL,             -- base | incremental | logical
    filename        TEXT NOT NULL,
    subfolder       TEXT NOT NULL,
    started_at      TEXT NOT NULL,
    completed_at    TEXT,
    size_bytes      INTEGER DEFAULT 0,
    parent_id       TEXT,
    wal_start_lsn   TEXT,
    wal_end_lsn     TEXT,
    status          TEXT DEFAULT 'running',    -- running | completed | failed
    error           TEXT DEFAULT '',
    database_name   TEXT
);

CREATE TABLE wal_segments (
    name            TEXT PRIMARY KEY,
    set_id          TEXT NOT NULL REFERENCES backup_sets(id) ON DELETE CASCADE,
    archived_at     TEXT NOT NULL,
    size_bytes      INTEGER DEFAULT 0,
    timeline        INTEGER NOT NULL,
    lsn_start       TEXT,
    lsn_end         TEXT
);

CREATE TABLE backup_stats (
    backup_id       TEXT PRIMARY KEY REFERENCES backups(id) ON DELETE CASCADE,
    duration_secs   REAL,
    throughput_mbps  REAL,
    tables_count    INTEGER,
    db_size_bytes   INTEGER,
    extra_json      TEXT
);
```

Retention cascade: `DELETE FROM backup_sets WHERE id=?` cascades to all `backups`, `wal_segments`, and `backup_stats` rows for that set.

## Backup Profile Spec

```json
{
  "physical": {
    "base_schedule": "0 2 * * 0",
    "incremental_schedule": "0 */6 * * *",
    "wal_archive_enabled": true,
    "archive_timeout_seconds": 60
  },
  "logical": {
    "schedule": "0 2 * * *",
    "databases": [],
    "format": "custom"
  },
  "destination": {
    "type": "s3",
    "s3": { "bucket": "backups", "region": "us-east-1", "endpoint": "", "path_prefix": "pg-swarm/" }
  },
  "retention": {
    "base_backup_count": 7,
    "wal_retention_days": 14,
    "logical_backup_count": 30
  }
}
```

## Destination Types

| Type | Use Case | Credentials |
|------|----------|-------------|
| `s3` | AWS S3 or S3-compatible (MinIO) | `access_key_id` + `secret_access_key` |
| `gcs` | Google Cloud Storage | `service_account_json` |
| `sftp` | Remote SFTP server | `password` |
| `local` | Local filesystem path | None |

Credentials are stored in a K8s Secret (`<cluster>-backup-creds-<id>`) on the satellite, created by the operator.

## Attach / Detach Flow

**Attach** (`POST /api/v1/profiles/:id/attach-backup-profile`):
1. Insert row into `profile_backup_profiles` join table
2. Bump `config_version` on all ClusterConfigs linked via deployment rules
3. Re-push configs to satellites (now includes this rule in `repeated BackupConfig`)
4. Operator reconciles: per-rule credential Secret, backup RBAC, backup sidecar injected into StatefulSet, `archive_command` set to sidecar endpoint

**Detach** (`POST /api/v1/profiles/:id/detach-backup-profile`):
1. Delete row from `profile_backup_profiles` join table
2. Bump `config_version` on all linked ClusterConfigs
3. Re-push configs (rule removed from `repeated BackupConfig`)
4. Operator reconciles: backup sidecar removed from StatefulSet, `archive_command` reset, backup RBAC and credentials cleaned up

## Status Reporting

```
Backup sidecar → ConfigMap (<cluster>-backup-status)
                       │
                Health monitor tick (10s)
                       │
                BackupStatusReport (gRPC stream)
                       │
                Central: upsert backup_inventory row + create Event
```

## Sidecar Container

- **Image**: `ghcr.io/pg-swarm/pg-swarm-backup-sidecar:latest`
- **Base**: `postgres:17` (includes pg_basebackup, pg_dump, psql) + aws-cli, openssh-client
- **Port**: 8442
- **Entry point**: Go binary (`backup-sidecar`), long-running daemon
- **Metadata**: Pure Go SQLite (`modernc.org/sqlite`), no external sqlite3 binary needed

## Package Structure

```
cmd/backup-sidecar/
└── main.go                     # Entry point

internal/backup/
├── sidecar.go                  # Main lifecycle: init, role detection, run loop, shutdown
├── scheduler.go                # Cron scheduler for base/incremental/logical
├── physical.go                 # pg_basebackup execution (base + incremental + standby fallback)
├── logical.go                  # pg_dump / pg_dumpall execution
├── wal.go                      # WAL push/fetch documentation
├── metadata.go                 # SQLite operations (all tables)
├── retention.go                # Delete expired sets, cascade metadata, vacuum
├── reporter.go                 # Status reporting to satellite (ConfigMap)
├── notifier.go                 # Replica→primary notification client
├── api.go                      # HTTP server (WAL push/fetch, backup/complete, /healthz)
└── destination/
    ├── destination.go          # Interface: Upload, Download, List, Delete, Exists
    ├── s3.go
    ├── gcs.go
    ├── sftp.go
    └── local.go
```

## Restore

### PITR (Physical)

1. User picks a base backup + target time in the dashboard
2. `POST /api/v1/clusters/:id/restore` → Central creates `RestoreOperation` (pending)
3. Central sends `RestoreCommand` via gRPC to satellite
4. Satellite creates a K8s Job that:
   - Scales StatefulSet to 0
   - Downloads base backup from destination
   - Extracts to data volume
   - Sets `recovery_target_time` + `restore_command` in `postgresql.auto.conf`
   - Creates `recovery.signal`
   - Scales StatefulSet back up
5. Satellite sends `RestoreStatusReport` via gRPC

### Logical

1. User picks a logical backup in the dashboard
2. Satellite creates a Job running `pg_restore` against the primary
3. Reports completion via `RestoreStatusReport`

## REST API

```
GET    /api/v1/backup-profiles                      List all backup profiles
POST   /api/v1/backup-profiles                      Create a backup profile
GET    /api/v1/backup-profiles/:id                  Get a backup profile
PUT    /api/v1/backup-profiles/:id                  Update a backup profile
DELETE /api/v1/backup-profiles/:id                  Delete a backup profile
POST   /api/v1/profiles/:id/attach-backup-profile   Attach rule to profile
POST   /api/v1/profiles/:id/detach-backup-profile   Detach rule from profile
GET    /api/v1/clusters/:id/backups              List cluster backup inventory
GET    /api/v1/backups/:id                       Get single backup record
POST   /api/v1/clusters/:id/restore              Initiate a restore
GET    /api/v1/clusters/:id/restores             List cluster restore operations
```

---

## Backup sequence

### 1. Init container (`pg-init`)

**Primary (ordinal 0) — first boot:**
- `initdb` → starts PG locally → creates `repl_user`, `backup_user`, app databases → stops PG
- Copies ConfigMap `postgresql.conf` / `pg_hba.conf` into PGDATA
- `archive_command` is set to `cp %p /wal-staging/%f` (staging emptyDir shared with backup sidecar)

**Replica (ordinal > 0) — first boot:**
- Waits for primary, runs `pg_basebackup -R -Xs`
- Copies ConfigMap into PGDATA
- Injects `restore_command` into `postgresql.auto.conf` — a shell script that writes the WAL name to `/wal-restore/.request` and polls until the sidecar places the file at `/wal-restore/<name>`

**Any pod — re-init / restart (PGDATA already exists):**
- Copies config only
- If `standby.signal` present → checks for timeline divergence → runs `pg_rewind` or falls back to `pg_basebackup`
- Writes `standby.signal` + `primary_conninfo` pointing to the RW service

---

### 2. Wrapper script (main container loop)

Before PG starts each iteration:
- `pg_swarm_recover()`: detects timeline divergence → `pg_rewind` → WAL cleanup (REDO segment + checkpoint record segment) → falls back to `pg_basebackup` if rewind fails
- Checks `.pg-swarm-needs-basebackup` marker (set by failover sidecar or backup sidecar)
- Final guard: verifies checkpoint WAL segment exists before handing off to `docker-entrypoint.sh`

Then PG starts. On exit → loop repeats.

---

### 3. Backup sidecar (`Run()`)

Starts concurrently with PG in the same pod.

```
startup
  ├── destination.NewFromEnv()         — S3/GCS/SFTP/local
  ├── go WatchWALRestore()             — MUST start before detectRole()
  │     polls /wal-restore/.request every 500ms
  │     downloads+decompresses WAL from dest → /wal-restore/<name>
  │     (needed so restore_command doesn't deadlock PG during recovery)
  └── detectRole()                     — retries pg_is_in_recovery() for up to 60s
```

**If primary:**
```
activatePrimary()
  ├── download or create backups.db (SQLite metadata) from dest
  ├── ensure active backup set exists
  ├── go WatchWALStaging()   — polls /wal-staging/ every 1s
  │     compress + upload WAL → dest/wal/<name>.gz
  │     record segment in metadata
  │     delete local copy
  ├── go api.Start(:8442)    — /backup/complete, /healthz, legacy push/fetch
  └── NewRetentionWorker()   — prunes old backup sets
```

**If replica:**
```
activateReplica()
  ├── go api.Start(:8442)    — /healthz only
  ├── NewNotifier()          — reaches primary sidecar HTTP API
  └── go scheduler.Run()
        ├── immediate: RunBaseBackup() (if BASE_SCHEDULE configured)
        ├── baseTicker  → RunBaseBackup()      (pg_basebackup to dest)
        ├── incrTicker  → RunIncrBackup()      (PG 17+ incremental)
        └── logicTicker → RunLogicalBackup()   (pg_dump per database)
```

**Role-change watcher** (every 10s): calls `pg_is_in_recovery()` → if role changed, `deactivate()` → `activateRole()` with new role. This is how the sidecar transitions after a failover (replica → primary).

---

### Key shared volumes (emptyDir, pod-scoped)

| Volume | Direction | Who writes | Who reads |
|---|---|---|---|
| `/wal-staging` | PG → sidecar | PG `archive_command` | `WatchWALStaging` |
| `/wal-restore` | sidecar → PG | `WatchWALRestore` | PG `restore_command` |
