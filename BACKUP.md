# Backup and Restore Architecture

## Overview

pg-swarm provides managed backup and restore through two entities:

- **Backup Stores** — centrally managed storage destinations (S3, GCS, SFTP, local) with encrypted-at-rest credentials. Created in the Admin console. Multiple backup profiles can share a single store.
- **Backup Profiles** — define what to back up (physical, logical, or both), when (cron schedules), and how long to keep it (retention). Each profile references exactly one backup store.

Backup profiles attach to cluster profiles, which deploy to satellites via deployment rules. The satellite operator injects a **backup sidecar** container into each StatefulSet pod. The sidecar detects its role (primary vs replica) and activates the appropriate responsibilities. No CronJobs are used.

---

## Backup Stores

A backup store is a reusable storage destination managed centrally via the Admin console.

### Store Types

| Type | Use Case | Config Fields | Credential Fields |
|------|----------|---------------|-------------------|
| `s3` | AWS S3 or S3-compatible (MinIO) | bucket, region, endpoint, force_path_style | access_key_id, secret_access_key |
| `gcs` | Google Cloud Storage | bucket | service_account_json |
| `sftp` | Remote SFTP server | host, port, user, base_path | password, private_key |
| `local` | Local PersistentVolumeClaim | size, storage_class | — |

### Credential Encryption

Store credentials are encrypted at rest in the central PostgreSQL database using **AES-256-GCM**:

- The central server reads a 32-byte encryption key from the `ENCRYPTION_KEY` environment variable (hex-encoded, 64 characters)
- On create/update, credential fields are marshaled to JSON and encrypted as a single BYTEA blob (`nonce || ciphertext || tag`)
- Non-secret config (bucket, region, host, etc.) is stored as plaintext JSONB for visibility
- The REST API never returns raw credential values — only a `credentials_set` map indicating which fields are populated (e.g., `{"access_key_id": true, "secret_access_key": true}`)

### Credential Flow to Satellites

When a cluster config is pushed to a satellite:
1. Central decrypts the store credentials from the database
2. Credentials are embedded in the protobuf `BackupStoreConfig` message
3. The satellite operator creates a K8s Secret (`<cluster>-backup-creds-<id>`) containing the credentials
4. The backup sidecar reads credentials from the Secret via environment variables

### REST API

```
GET    /api/v1/backup-stores           List all stores (credentials masked)
POST   /api/v1/backup-stores           Create store (encrypts credentials)
GET    /api/v1/backup-stores/:id       Get store (credentials masked)
PUT    /api/v1/backup-stores/:id       Update store (re-encrypts credentials)
DELETE /api/v1/backup-stores/:id       Delete store (fails if profiles reference it)
```

### Database Schema

```sql
CREATE TABLE backup_stores (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT UNIQUE NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    store_type  TEXT NOT NULL,              -- 's3', 'gcs', 'sftp', 'local'
    config      JSONB NOT NULL DEFAULT '{}', -- non-secret connection params
    credentials BYTEA NOT NULL DEFAULT '',   -- AES-256-GCM encrypted secrets
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

---

## Backup Profiles

A backup profile defines schedules, retention, and references a backup store. Profiles attach to cluster profiles for deployment.

### Profile Spec

```json
{
  "physical": {
    "base_schedule": "0 4 * * *",
    "incremental_schedule": "0 * * * *",
    "wal_archive_enabled": true,
    "archive_timeout_seconds": 60
  },
  "logical": {
    "schedule": "0 2 * * *",
    "databases": [],
    "format": "custom"
  },
  "backup_store_id": "uuid-of-the-backup-store",
  "retention": {
    "base_backup_count": 7,
    "incremental_backup_count": 23,
    "wal_retention_days": 14,
    "logical_backup_count": 30
  }
}
```

Note: `destination` has been replaced by `backup_store_id`. The destination configuration lives entirely in the referenced backup store.

### Constraints

- A cluster profile can have at most **one physical** and **one logical** backup profile attached
- A profile must reference a valid backup store
- Physical profiles with WAL archiving validate that WAL retention days cover the base backup schedule span

### REST API

```
GET    /api/v1/backup-profiles                      List all backup profiles
POST   /api/v1/backup-profiles                      Create a backup profile
GET    /api/v1/backup-profiles/:id                  Get a backup profile
PUT    /api/v1/backup-profiles/:id                  Update a backup profile
DELETE /api/v1/backup-profiles/:id                  Delete a backup profile
POST   /api/v1/profiles/:id/attach-backup-profile   Attach profile to cluster profile
POST   /api/v1/profiles/:id/detach-backup-profile   Detach profile from cluster profile
GET    /api/v1/profiles/:id/backup-profiles         List attached backup profiles
```

### Attach / Detach Flow

**Attach** (`POST /api/v1/profiles/:id/attach-backup-profile`):
1. Insert row into `profile_backup_profiles` join table
2. Bump `config_version` on all ClusterConfigs linked via deployment rules
3. Re-push configs to satellites (now includes `BackupConfig` with store details)
4. Operator reconciles: credential Secret, backup RBAC, backup sidecar injected into StatefulSet, `archive_command` set

**Detach** (`POST /api/v1/profiles/:id/detach-backup-profile`):
1. Delete row from `profile_backup_profiles` join table
2. Bump `config_version` on all linked ClusterConfigs
3. Re-push configs (backup config removed)
4. Operator reconciles: sidecar removed, `archive_command` reset, credentials cleaned up

---

## Data Flow

```
BackupStore ◄─── references ──── BackupProfile ──attach──> ClusterProfile
                                                                  │
                                                   DeploymentRule (label selector)
                                                                  │
                                                           ClusterConfig
                                                    (includes BackupStoreConfig)
                                                                  │
                                                       gRPC push to satellite
                                                                  │
                                                        Satellite Operator
                                                                  │
                                               Injects backup sidecar into StatefulSet
                                               Creates credential K8s Secret
                                               Sets archive_command in postgresql.conf
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

---

## Destination Folder Structure

Every satellite-cluster combination gets a standardized folder in the backup store:

```
<backup-store-root>/
├── edge-nyc-prod-pg/                       # <satellite-name>-<cluster-name>
│   ├── backups.db                          # Portable SQLite metadata copy
│   ├── base/
│   │   ├── 20260317T040000Z.tar.gz         # Gzipped base backup
│   │   └── 20260317T040000Z_manifest.gz    # pg_basebackup manifest
│   ├── inc/
│   │   ├── 20260317T050000Z.tar.gz         # Gzipped incremental
│   │   └── 20260317T050000Z_manifest.gz
│   ├── wal/
│   │   ├── 000000010000000000000001.gz     # Gzipped WAL segments
│   │   └── 000000010000000000000002.gz
│   └── logical/
│       └── 20260317T020000Z_mydb.sql.gz    # Gzipped pg_dump output
├── edge-lon-prod-pg/
│   ├── backups.db
│   └── ...
```

**Key conventions:**
- Folder prefix: `<satellite-name>-<cluster-name>` (using satellite hostname, not UUID)
- All files are gzipped
- Subfolder names: `base/`, `inc/`, `wal/`, `logical/`
- `backups.db` is a portable SQLite copy synced after each operation — enables import into a different swarm

### Dual Metadata

- **Central DB** (`backup_inventory` table) is authoritative for the dashboard and API
- **`backups.db`** in the store folder is a portable copy for disaster recovery and future import
- Both are updated after each backup/restore operation

---

## Split-Responsibility Model

The backup sidecar detects its role by querying `pg_is_in_recovery()` and activates the appropriate responsibilities:

### Primary sidecar responsibilities:
1. **WAL archiving** — HTTP endpoint receives WAL from `archive_command`, compresses, uploads to `wal/`
2. **Metadata DB** — single writer to `backups.db` (SQLite), ingests backup completion notifications from replica
3. **WAL fetch** — serves `restore_command` requests (downloads WAL from destination for recovery)
4. **Retention** — deletes expired backups/WAL/logical dumps and cascades metadata

### Replica sidecar responsibilities:
1. **Base backups** — `pg_basebackup -h localhost` on schedule, uploads to `base/`
2. **Incremental backups** — `pg_basebackup --incremental` on schedule, uploads to `inc/` (PG 17+, with standby WAL fallback)
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

---

## Sidecar Startup Store Check

On startup, before activating role-specific responsibilities, the backup sidecar checks the store for existing backups:

```
startup
  ├── destination init (from store config)
  ├── checkExistingBackups()
  │     ├── Check if <prefix>/backups.db exists in store
  │     ├── If yes: download, query available restore points
  │     │     ├── Latest base backup timestamp
  │     │     ├── Oldest base backup timestamp
  │     │     ├── WAL range (earliest to latest LSN)
  │     │     └── List of base backups with timestamps
  │     └── Report to central via BackupStatusReport (store_check variant)
  ├── detectRole()
  └── activateRole()
```

This enables:
- **Initial cluster setup**: If the store has existing backups for this satellite+cluster, central can offer PITR restore before the cluster starts serving traffic
- **Cluster recreation**: If a cluster is deleted and recreated pointing at the same store, it can restore from the previous backups

---

## Restore

### PITR (Physical)

Available from two entry points:
1. **Cluster Detail > Backups tab**: "Restore PITR" button on each completed base backup row, with timestamp picker constrained between backup time and next backup (or now)
2. **Initial cluster setup**: If sidecar reports existing backups on startup, dashboard offers restore option

Flow:
1. User picks a base backup + target time in the dashboard
2. `POST /api/v1/clusters/:id/restore` → Central creates `RestoreOperation` (pending)
3. Central sends `RestoreCommand` via gRPC to satellite (includes `BackupStoreConfig` with decrypted credentials)
4. Satellite creates a K8s Job that:
   - Scales StatefulSet to 0
   - Downloads base backup from store
   - Extracts to data volume
   - Sets `recovery_target_time` + `restore_command` in `postgresql.auto.conf`
   - Creates `recovery.signal`
   - Scales StatefulSet back up
5. Satellite sends `RestoreStatusReport` via gRPC
6. Central updates `restore_operations` table

### Logical

1. User picks a logical backup in the Cluster Detail > Backups tab
2. Satellite creates a Job running `pg_restore` against the primary
3. Reports completion via `RestoreStatusReport`

### REST API

```
GET    /api/v1/clusters/:id/backups    List cluster backup inventory
GET    /api/v1/backups/:id             Get single backup record
POST   /api/v1/clusters/:id/restore    Initiate a restore (PITR or logical)
GET    /api/v1/clusters/:id/restores   List cluster restore operations
```

---

## archive_command / restore_command

When backup profiles are attached, the operator configures PostgreSQL to use the sidecar:

```
archive_command = 'curl -sf -X POST -F file=@%p -F name=%f http://localhost:8442/wal/push'
restore_command = 'curl -sf -o %p http://localhost:8442/wal/fetch?name=%f'
```

- `archive_command` POSTs WAL to the local backup sidecar. Blocks until durable upload. PG only marks WAL as archived after curl returns 0.
- `restore_command` fetches WAL from the sidecar, which downloads from the store.

---

## Sidecar HTTP API (:8442)

| Method | Path | Role | Purpose |
|--------|------|------|---------|
| POST | `/wal/push` | Primary | Receives WAL from archive_command, compresses, uploads |
| GET | `/wal/fetch?name=` | Primary | Serves WAL for restore_command |
| POST | `/backup/complete` | Primary | Receives backup metadata from replica |
| GET | `/healthz` | Both | Health check |

---

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
    subfolder       TEXT NOT NULL,             -- base/ | inc/ | logical/
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

---

## Protobuf

The `BackupConfig` message in `api/proto/v1/backup.proto`:

```protobuf
message BackupConfig {
    PhysicalBackupConfig physical = 1;
    LogicalBackupConfig logical = 2;
    reserved 3;                        // was: BackupDestination destination
    BackupRetention retention = 4;
    string backup_image = 5;
    string backup_profile_id = 6;
    BackupStoreConfig store = 7;       // replaces destination
}

message BackupStoreConfig {
    string store_type = 1;             // s3, gcs, sftp, local
    string store_id = 2;               // UUID of the backup store
    S3Destination s3 = 3;              // connection + credentials
    GCSDestination gcs = 4;
    SFTPDestination sftp = 5;
    LocalDestination local = 6;
    string base_path = 7;             // "<satellite-name>-<cluster-name>"
}
```

The `base_path` field is computed by central when building the proto message: `fmt.Sprintf("%s-%s", satelliteName, clusterName)`.

---

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

---

## Sidecar Container

- **Image**: `ghcr.io/pg-swarm/pg-swarm-backup-sidecar:latest`
- **Base**: `postgres:17` (includes pg_basebackup, pg_dump, psql) + aws-cli, openssh-client
- **Port**: 8442
- **Entry point**: Go binary (`backup-sidecar`), long-running daemon
- **Metadata**: Pure Go SQLite (`modernc.org/sqlite`), no external sqlite3 binary needed

---

## Package Structure

```
cmd/backup-sidecar/
└── main.go                     # Entry point

internal/backup/
├── sidecar.go                  # Main lifecycle: init, store check, role detection, run loop
├── scheduler.go                # Cron scheduler for base/incremental/logical
├── physical.go                 # pg_basebackup execution (base + incremental + standby fallback)
├── logical.go                  # pg_dump / pg_dumpall execution
├── wal.go                      # WAL push/fetch
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

internal/central/
├── crypto/
│   ├── crypto.go               # AES-256-GCM encrypt/decrypt for store credentials
│   └── crypto_test.go
```

---

## Backup Sequence

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
  ├── destination init (from store config via env vars)
  ├── checkExistingBackups()           — reports available restore points to central
  ├── go WatchWALRestore()             — MUST start before detectRole()
  │     polls /wal-restore/.request every 500ms
  │     downloads+decompresses WAL from store → /wal-restore/<name>
  │     (needed so restore_command doesn't deadlock PG during recovery)
  └── detectRole()                     — retries pg_is_in_recovery() for up to 60s
```

**If primary:**
```
activatePrimary()
  ├── download or create backups.db (SQLite metadata) from store
  ├── ensure active backup set exists
  ├── go WatchWALStaging()   — polls /wal-staging/ every 1s
  │     compress + upload WAL → store/<prefix>/wal/<name>.gz
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
        ├── baseTicker  → RunBaseBackup()      → store/<prefix>/base/
        ├── incrTicker  → RunIncrBackup()      → store/<prefix>/inc/
        └── logicTicker → RunLogicalBackup()   → store/<prefix>/logical/
```

**Role-change watcher** (every 10s): calls `pg_is_in_recovery()` → if role changed, `deactivate()` → `activateRole()` with new role. This is how the sidecar transitions after a failover (replica → primary).

---

### Key shared volumes (emptyDir, pod-scoped)

| Volume | Direction | Who writes | Who reads |
|---|---|---|---|
| `/wal-staging` | PG → sidecar | PG `archive_command` | `WatchWALStaging` |
| `/wal-restore` | sidecar → PG | `WatchWALRestore` | PG `restore_command` |

---

## Implementation Plan

### Phase 1: Encryption Utilities
- New `internal/central/crypto/` package — AES-256-GCM encrypt/decrypt
- Key from `ENCRYPTION_KEY` env var, passed to RESTServer

### Phase 2: Backup Stores
- Migration: `backup_stores` table
- Models: `BackupStore`, `BackupStoreConfig`, `BackupStoreCredentials`
- Store interface: 5 CRUD methods
- REST API: 5 endpoints
- Dashboard: `BackupStoresTab` in Admin page

### Phase 3: Backup Profile Migration
- Migration: add `backup_store_id` to `backup_profiles`, migrate seed profiles, strip `destination`
- Models: replace `Destination` with `BackupStoreID` in `BackupProfileSpec`
- REST: resolve store when building proto configs
- Dashboard: replace destination tab with store selector

### Phase 4: Proto Updates
- Replace `BackupDestination` with `BackupStoreConfig` in `BackupConfig`
- Add `BackupStoreConfig` message with `base_path` field
- Run `make proto`

### Phase 5: Sidecar Updates
- Add `SatelliteName` to config, update `destPrefix()` to use name instead of UUID
- Add `checkExistingBackups()` on startup
- Verify subfolder paths: `wal/`, `base/`, `inc/`, `logical/`

### Phase 6: Operator Updates
- Uncomment sidecar injection in `manifest_statefulset.go`
- Update `manifest_backup.go` to use `backup.Store` instead of `backup.Destination`
- Re-enable backup RBAC and credential secret reconciliation

### Phase 7: Dashboard PITR UI
- ClusterDetail > Backups tab: "Restore PITR" button per base backup
- Restore modal with timestamp picker and confirmation
- Initial cluster setup: restore prompt if store has existing backups

### Phase 8: Documentation
- Update CLAUDE.md, README.md, DESIGN.md, CHANGELOG.md
- Add `ENCRYPTION_KEY` to configuration tables
