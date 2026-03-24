# Backup & Restore Requirements

## 1. Overview

pg-swarm supports two forms of backup and two forms of restore for PostgreSQL clusters managed across edge Kubernetes clusters.

| Dimension | Options |
|-----------|---------|
| **Backup types** | Physical (base + incremental + WAL) and Logical (`pg_dump`) |
| **Storage backends** | GCS (Google Cloud Storage) and SFTP |
| **Restore modes** | Logical restore (select a backup) and PITR (select a timestamp) |

All scheduled backups run on **replica** pods to avoid I/O load on the primary. WAL archiving runs on the **primary** (required by PostgreSQL). A backup sidecar container is injected into each PG pod by the satellite operator when backup is configured.

---

## 2. Backup Types

### 2.1 Physical Backup

Physical backups capture the raw PostgreSQL data directory and WAL stream, enabling point-in-time recovery.

#### 2.1.1 Base Backup

- Full cluster snapshot via `pg_basebackup --checkpoint=fast --wal-method=none`.
- Runs on a **replica** pod (connects to primary via streaming replication).
- Output is compressed (gzip) and uploaded to the configured storage backend.
- Scheduling: 5-field cron expression (e.g., `0 2 * * 0` for weekly at 2 AM).
- Each base backup is recorded in the central backup inventory with start/end LSN.

#### 2.1.2 Incremental Backup

- Delta since the last base backup via `pg_basebackup --incremental=<manifest>` (PostgreSQL 17+).
- Runs on a **replica** pod.
- Requires `summarize_wal = on` in `postgresql.conf` (set by the operator when incremental backups are configured).
- Falls back to a full base backup if no valid manifest exists.
- Scheduling: separate cron expression (e.g., `0 2 * * 1-6` for nightly on non-base days).
- Stored relative to its parent base backup in the storage path hierarchy.

#### 2.1.3 WAL Archiving

- Continuous archiving of WAL segments from the **primary** pod.
- PostgreSQL `archive_command` writes completed WAL segments to a shared staging directory (`/wal-staging` emptyDir volume).
- The backup sidecar watches the staging directory, compresses each segment (gzip), uploads to storage, then deletes the local copy.
- WAL archiving is **required** for PITR and must be enabled whenever physical backups are configured.
- Configurable archive timeout (seconds) — PostgreSQL `archive_timeout` forces a WAL switch after this period of inactivity.

### 2.2 Logical Backup

Logical backups capture database contents as SQL statements or archive format, enabling selective database-level restore.

- Uses `pg_dump` for specific databases or `pg_dumpall` for all databases.
- Runs on a **replica** pod.
- Output formats:
  - **custom** (`.dump`) — compressed, supports `pg_restore` with selective restore. Default.
  - **plain** (`.sql`) — plain SQL text, restored via `psql`.
  - **directory** — parallel dump to a directory, supports `pg_restore` with parallel jobs.
- Databases to back up: explicit list, or empty = all databases.
- Scheduling: 5-field cron expression.
- Each logical backup is recorded in the central inventory with database name(s), format, and size.

### 2.3 On-Demand Backup

In addition to scheduled backups, users can trigger a backup immediately from the dashboard or REST API.

- Supported types: base, incremental, or logical (user selects).
- The request flows: REST API → central → satellite (gRPC) → backup sidecar (gRPC stream command).
- The sidecar executes the backup immediately, bypassing the cron scheduler but still applying health gates for physical backups.
- On-demand backups are recorded in the backup inventory with the same status reporting as scheduled backups.
- Only one backup runs at a time — if a scheduled or on-demand backup is already in progress, the request is rejected with an error.

---

## 3. Storage Backends

All storage backends use a consistent path hierarchy:

```
<base_path>/<satellite_name>-<namespace>-<cluster_name>/
  wal/                          # WAL segments (gzipped)
    000000010000000000000001.gz
    000000010000000000000002.gz
  base/                         # Base backups
    20260324T020000/
      base.tar.gz
      manifest.json
  incremental/                  # Incremental backups
    20260325T020000/
      incremental.tar.gz
      manifest.json
  logical/                      # Logical backups
    20260324T030000/
      appdb.dump
      analytics.dump
```

### 3.1 GCS (Google Cloud Storage)

| Field | Description |
|-------|-------------|
| `bucket` | GCS bucket name (required) |
| `path_prefix` | Optional prefix within the bucket |
| **Credentials** | |
| `service_account_json` | Service account key JSON (encrypted at rest) |

Authentication: service account key injected as env var into the sidecar.

### 3.2 SFTP

| Field | Description |
|-------|-------------|
| `host` | SFTP server hostname or IP (required) |
| `port` | SFTP port (default: 22) |
| `user` | SFTP username (required) |
| `base_path` | Remote directory path (required) |
| **Credentials** | |
| `password` | Password authentication (encrypted at rest) |
| `private_key` | SSH private key authentication (encrypted at rest) |

If both `password` and `private_key` are provided, private key takes precedence.

---

## 4. Restore Operations

### 4.1 Logical Restore

Restores a logical backup (pg_dump output) to the cluster.

**Workflow:**
1. User selects a logical backup from the cluster's backup inventory.
2. User specifies the target database (same name or a different database).
3. Central sends a restore command to the satellite via gRPC.
4. The backup sidecar downloads the backup from storage.
5. Runs `pg_restore` (for custom/directory format) or `psql` (for plain SQL) against the primary.
6. Status reported back to central (pending → running → completed/failed).

**Constraints:**
- Target database must exist (the restore does not create it).
- For custom/directory format, `--clean --if-exists` is used by default.
- Runs on the **primary** pod (or connects to primary from replica).

### 4.2 PITR (Point-in-Time Recovery)

Restores the cluster to a specific point in time using a base backup + WAL replay.

**Workflow:**
1. User selects a target timestamp via the dashboard date-time picker.
2. System validates: the timestamp must fall within the range covered by available base backups and archived WAL segments.
3. System identifies the nearest preceding base backup.
4. User chooses restore mode: **in-place** or **new cluster**.

#### 4.2.1 In-Place Restore

1. Cluster is paused (operator stops reconciliation).
2. All pods are scaled to 0.
3. PGDATA is wiped on the target pod.
4. Base backup is downloaded and extracted.
5. `recovery.signal` is created with `recovery_target_time` and `restore_command` pointing to the sidecar's WAL restore path.
6. Pod is started — PostgreSQL replays WAL to the target time.
7. Once recovery completes, remaining replicas are re-basebackupped from the new primary.
8. Cluster is unpaused.

**Warning:** This is a destructive operation. All data after the target timestamp is lost.

#### 4.2.2 New Cluster Restore

1. A new StatefulSet is created (e.g., `<cluster>-restored-<timestamp>`).
2. Init container downloads and extracts the base backup.
3. Recovery configuration replays WAL to the target time.
4. New cluster starts as an independent standalone instance.
5. User verifies data, then manually switches traffic or promotes the restored cluster.

**Constraints:**
- Requires continuous WAL archiving to be enabled.
- PITR precision depends on WAL segment availability and `archive_timeout` setting.
- The earliest recoverable point is the start LSN of the oldest available base backup.
- The latest recoverable point is the most recent archived WAL segment.

---

## 5. Architecture

### 5.1 Backup Sidecar

The backup sidecar is a container injected into each PostgreSQL pod by the satellite operator when backup is configured for the cluster.

```
Pod: <cluster>-N
├── postgres          (main PG container)
├── failover-sidecar  (optional, leader election + promotion)
└── backup-sidecar    (backup scheduling, WAL archiving, restore agent)
    Volumes:
    ├── /var/lib/postgresql/data  (shared PGDATA, from PVC)
    ├── /wal-staging              (emptyDir: PG archive_command → sidecar)
    └── /wal-restore              (emptyDir: sidecar → PG restore_command)
```

**Role-aware behavior:**

| Pod role | Responsibilities |
|----------|-----------------|
| Primary | WAL archiving (watch `/wal-staging`, compress, upload) |
| Replica | Scheduled base, incremental, and logical backups |

The sidecar detects its role by querying `pg_is_in_recovery()` every 10 seconds and activates/deactivates services accordingly (handles failover role transitions).

**Health gates:**
- Before running a scheduled backup, the sidecar checks replication lag via `pg_stat_wal_receiver`.
- If lag exceeds a configurable threshold, the backup is **skipped** (not queued) and reported as `skipped`.
- Logical backups skip the health check (they query local data).

**Internal scheduler:**
- Cron-based scheduler running inside the sidecar process.
- No Kubernetes CronJobs — the sidecar manages all scheduling.
- Supports immediate trigger via gRPC command from satellite (for on-demand backups initiated from dashboard).

### 5.2 Backup Stores (Central)

Backup stores are admin-managed, reusable storage destinations created and managed centrally.

- Stored in the `backup_stores` table.
- Non-secret config (bucket, host, path) stored as plaintext JSONB.
- Credentials encrypted at rest using AES-256-GCM with the `ENCRYPTION_KEY` env var.
- Referenced by backup configuration in cluster profiles (via `store_id`).
- The REST API **never** returns raw credentials — only a `credentials_set` map indicating which fields are populated (e.g., `{"service_account_json": true}`).

### 5.3 Config Flow

```
Admin creates backup store (Central REST API)
    ↓
Admin configures backup in cluster profile (store_id + schedules + retention)
    ↓
Profile attached to clusters via deployment rules
    ↓
Central pushes ClusterConfig (with backup config) to satellite via gRPC
    ↓
Satellite operator reconciles:
  - Injects backup sidecar container into StatefulSet
  - Creates credential Secret from decrypted store credentials
  - Creates config ConfigMap (schedules, retention, store type)
  - Creates RBAC (ServiceAccount, Role, RoleBinding)
    ↓
Backup sidecar reads config, starts scheduler, reports status
```

### 5.4 WAL Staging Volumes

Two shared emptyDir volumes enable WAL flow between PostgreSQL and the backup sidecar:

**`/wal-staging`** (archive path — PG to sidecar):
- `archive_command = 'cp %p /wal-staging/%f'`
- Sidecar watches with 1-second poll interval.
- On new file: compress (gzip) → upload to storage → delete local.
- If upload fails: file remains, PG will retry `archive_command` on next checkpoint.

**`/wal-restore`** (restore path — sidecar to PG):
- `restore_command = 'cp /wal-restore/%f %p'`
- During PITR recovery, the sidecar pre-fetches WAL segments from storage.
- Downloads ahead of PG's requests to minimize recovery stalls.
- 500ms poll interval for pre-fetch loop.

---

## 6. Scheduling & Retention

### 6.1 Scheduling

All schedules use standard 5-field cron expressions: `minute hour day-of-month month day-of-week`.

| Backup type | Schedule field | Example |
|-------------|---------------|---------|
| Base backup | `physical.base_schedule` | `0 2 * * 0` (weekly Sunday 2 AM) |
| Incremental backup | `physical.incremental_schedule` | `0 2 * * 1-6` (nightly except Sunday) |
| Logical backup | `logical.schedule` | `0 3 * * *` (daily 3 AM) |

Schedules are evaluated in the sidecar's local time (container timezone, typically UTC).

### 6.2 Retention

| Policy | Field | Description |
|--------|-------|-------------|
| Base backups | `retention.base_backup_count` | Max number of base backups to keep (minimum: 1) |
| Incremental backups | `retention.incremental_backup_count` | Max incrementals per base backup (0 = unlimited) |
| WAL segments | `retention.wal_retention_days` | Days to retain WAL segments (minimum: 1 when WAL archiving enabled) |
| Logical backups | `retention.logical_backup_count` | Max number of logical backups to keep (minimum: 1) |

The retention worker runs periodically in the sidecar:
1. Lists backups in storage, ordered by age.
2. Deletes backups exceeding the configured count/age.
3. When a base backup is deleted, its associated incrementals are also deleted.
4. WAL segments older than `wal_retention_days` are deleted, but never deletes WAL segments still needed by the oldest retained base backup.

---

## 7. Security

### 7.1 Credential Encryption

| Layer | Mechanism |
|-------|-----------|
| Central DB | AES-256-GCM encryption (nonce ‖ ciphertext ‖ tag stored as BYTEA) |
| Central → Satellite | gRPC with mTLS |
| Satellite → Pod | Kubernetes Secret (RBAC-protected, etcd encryption-at-rest recommended) |
| Sidecar | Reads credentials from Secret-mounted env vars |

The `ENCRYPTION_KEY` environment variable must be a 32-byte hex-encoded string (64 hex characters). It is required to create or read backup stores with credentials.

### 7.2 PostgreSQL Roles

The operator creates a `backup_user` role during cluster initialization:

```sql
CREATE ROLE backup_user WITH REPLICATION LOGIN PASSWORD '<generated>'
  IN ROLE pg_read_all_data;
```

- `REPLICATION`: required for `pg_basebackup` and WAL streaming.
- `pg_read_all_data`: required for `pg_dump` on all databases.
- Password stored in the cluster Secret, injected as `BACKUP_PASSWORD` env var.

### 7.3 pg_hba.conf

The operator adds an HBA rule for the backup user:

```
host replication backup_user all scram-sha-256
```

---

## 8. Status & Monitoring

### 8.1 Backup Inventory

Every backup operation (scheduled or on-demand) creates a record in the central `backup_inventory` table:

| Field | Description |
|-------|-------------|
| `id` | UUID |
| `satellite_id` | Satellite that ran the backup |
| `cluster_name` | Cluster name |
| `backup_type` | `base`, `incremental`, `logical` |
| `status` | `pending`, `running`, `completed`, `failed`, `skipped` |
| `started_at` | Backup start timestamp |
| `completed_at` | Backup completion timestamp |
| `size_bytes` | Total backup size |
| `backup_path` | Full path in storage backend |
| `pg_version` | PostgreSQL version |
| `wal_start_lsn` | Start LSN (physical backups only) |
| `wal_end_lsn` | End LSN (physical backups only) |
| `databases` | Database names (logical backups only) |
| `error_message` | Error details (failed backups only) |

### 8.2 Reporting Flow

```
Primary path (gRPC):
  Backup sidecar
    → sends BackupStatusReport via gRPC stream to satellite
    → satellite forwards to central via gRPC stream
    → central upserts backup_inventory record
    → central creates Event (backup started/completed/failed)
    → central pushes update via WebSocket to dashboard

Fallback path (ConfigMap, used when gRPC stream is disconnected):
  Backup sidecar
    → writes status to ConfigMap (<cluster>-backup-status)
    → satellite health monitor reads ConfigMap (every 10s)
    → satellite sends BackupStatusReport via gRPC stream to central
```

### 8.3 Restore Operations

Restore operations are tracked in the `restore_operations` table:

| Field | Description |
|-------|-------------|
| `id` | UUID |
| `satellite_id` | Target satellite |
| `cluster_name` | Target cluster |
| `backup_id` | Source backup (for logical restore) |
| `restore_type` | `logical`, `pitr` |
| `target_time` | Target timestamp (PITR only) |
| `target_database` | Target database (logical restore only) |
| `restore_mode` | `in_place`, `new_cluster` (PITR only) |
| `status` | `pending`, `running`, `completed`, `failed` |
| `error_message` | Error details |
| `started_at` | Operation start |
| `completed_at` | Operation completion |

---

## 9. REST API

### 9.1 Backup Stores

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/v1/backup-stores` | List all stores (credentials masked) |
| `POST` | `/api/v1/backup-stores` | Create a store |
| `GET` | `/api/v1/backup-stores/:id` | Get a store (credentials masked) |
| `PUT` | `/api/v1/backup-stores/:id` | Update a store |
| `DELETE` | `/api/v1/backup-stores/:id` | Delete a store |

**Create/Update request body:**
```json
{
  "name": "production-gcs",
  "description": "GCS bucket for production backups",
  "store_type": "gcs",
  "config": { "bucket": "pg-backups-prod", "path_prefix": "clusters" },
  "credentials": { "service_account_json": "{ ... }" }
}
```

**Response (credentials never returned):**
```json
{
  "id": "...",
  "name": "production-gcs",
  "store_type": "gcs",
  "config": { "bucket": "pg-backups-prod", "path_prefix": "clusters" },
  "credentials_set": { "service_account_json": true },
  "created_at": "...",
  "updated_at": "..."
}
```

### 9.2 Backup Inventory

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/v1/clusters/:id/backups` | List backups for a cluster |
| `GET` | `/api/v1/backups/:id` | Get backup details |
| `POST` | `/api/v1/clusters/:id/backups/trigger` | Trigger an on-demand backup |

**On-demand backup request:**
```json
{
  "backup_type": "base"
}
```
Valid types: `"base"`, `"incremental"`, `"logical"`. Returns `201` with a backup inventory record in `"running"` status.

### 9.3 Restore Operations

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/api/v1/clusters/:id/restore` | Initiate a restore |
| `GET` | `/api/v1/clusters/:id/restores` | List restore operations |

**Logical restore request:**
```json
{
  "backup_id": "<uuid>",
  "restore_type": "logical",
  "target_database": "appdb"
}
```

**PITR restore request:**
```json
{
  "restore_type": "pitr",
  "target_time": "2026-03-24T14:30:00Z",
  "restore_mode": "in_place"
}
```

---

## 10. Dashboard UI

### 10.1 Admin Page — Backup Stores Tab

- Table listing all backup stores (name, type, description, credential status, created date).
- Create/Edit modal:
  - Store type selector (GCS / SFTP).
  - Dynamic config fields based on type.
  - Credential fields (password inputs, never pre-filled with actual values).
  - Credential status indicators (set / not set).
- Delete with confirmation (fails if referenced by profiles).

### 10.2 Profiles Page — Backup Tab

- Store selector dropdown (references existing backup stores).
- Physical backup section:
  - Enable/disable toggle.
  - Base backup schedule (cron input with helper text).
  - Incremental backup schedule (optional).
  - WAL archiving toggle (auto-enabled when physical backups enabled).
  - Archive timeout (seconds).
  - Retention fields (base count, incremental count, WAL days).
- Logical backup section:
  - Enable/disable toggle.
  - Schedule (cron input).
  - Databases list (comma-separated, empty = all).
  - Format selector (custom / plain / directory).
  - Retention field (backup count).

### 10.3 Cluster Detail Page — Backups Tab

- Backup inventory table:
  - Columns: type (base/incremental/logical), status (badge), started, duration, size, path.
  - Sort by started date (newest first).
  - Status badges: running (blue), completed (green), failed (red), skipped (gray).
- **Restore** button opens the restore wizard.
- **Backup Now** button with dropdown to select type (base / incremental / logical). Triggers on-demand backup. Disabled while a backup is already running.
- WAL archiving status indicator (healthy / lagging / disabled).

### 10.4 Restore Wizard (Modal)

- Step 1: Choose restore type:
  - **Logical restore** — shows list of completed logical backups to select from.
  - **PITR** — shows date-time picker with valid range indicator.
- Step 2 (Logical): Select target database, confirm.
- Step 2 (PITR): Select restore mode (in-place / new cluster), confirm.
  - In-place shows a destructive action warning.
  - New cluster shows the name of the cluster that will be created.
- Step 3: Confirmation with operation summary, submit.
- Progress view: status updates streamed via WebSocket.

---

## 11. Database Schema

### 11.1 backup_stores

```sql
CREATE TABLE backup_stores (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT UNIQUE NOT NULL,
    description TEXT DEFAULT '',
    store_type  TEXT NOT NULL CHECK (store_type IN ('gcs', 'sftp')),
    config      JSONB NOT NULL DEFAULT '{}',
    credentials BYTEA DEFAULT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

### 11.2 backup_inventory

```sql
CREATE TABLE backup_inventory (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    satellite_id      UUID NOT NULL REFERENCES satellites(id),
    cluster_name      TEXT NOT NULL,
    backup_type       TEXT NOT NULL CHECK (backup_type IN ('base', 'incremental', 'logical')),
    status            TEXT NOT NULL CHECK (status IN ('pending', 'running', 'completed', 'failed', 'skipped')),
    started_at        TIMESTAMPTZ NOT NULL,
    completed_at      TIMESTAMPTZ,
    size_bytes        BIGINT DEFAULT 0,
    backup_path       TEXT DEFAULT '',
    pg_version        TEXT DEFAULT '',
    wal_start_lsn     TEXT DEFAULT '',
    wal_end_lsn       TEXT DEFAULT '',
    databases         TEXT[] DEFAULT '{}',
    error_message     TEXT DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_backup_inventory_cluster
    ON backup_inventory (satellite_id, cluster_name, started_at DESC);
```

### 11.3 restore_operations

```sql
CREATE TABLE restore_operations (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    satellite_id    UUID NOT NULL REFERENCES satellites(id),
    cluster_name    TEXT NOT NULL,
    backup_id       UUID REFERENCES backup_inventory(id),
    restore_type    TEXT NOT NULL CHECK (restore_type IN ('logical', 'pitr')),
    restore_mode    TEXT CHECK (restore_mode IN ('in_place', 'new_cluster')),
    target_time     TIMESTAMPTZ,
    target_database TEXT DEFAULT '',
    status          TEXT NOT NULL CHECK (status IN ('pending', 'running', 'completed', 'failed')),
    error_message   TEXT DEFAULT '',
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

---

## 12. Protobuf Messages

The following messages are added to `api/proto/v1/backup.proto`:

- `BackupConfig` — per-cluster backup configuration (store reference, physical config, logical config, retention).
- `PhysicalBackupConfig` — base schedule, incremental schedule, WAL archiving toggle, archive timeout.
- `LogicalBackupConfig` — schedule, databases, format.
- `BackupRetention` — base count, incremental count, WAL days, logical count.
- `BackupDestination` — union of GCS and SFTP destination configs with credentials.
- `GCSDestination` — bucket, path prefix, service account JSON.
- `SFTPDestination` — host, port, user, base path, password, private key.
- `BackupStatusReport` — sent satellite → central (backup type, status, size, path, LSN range).
- `RestoreCommand` — sent central → satellite (restore type, mode, target time, backup path).
- `RestoreStatusReport` — sent satellite → central (restore ID, status, error).
- `BackupTrigger` — sent central → satellite (on-demand backup request with type).

These are carried inside the existing `ClusterConfig`, `SatelliteMessage`, and `CentralMessage` envelopes. See section 13.1 for full protobuf definitions.

---

## 13. Detailed Design

This section specifies the concrete implementation: protobuf definitions, Go types, package structure, sidecar internals, operator integration, and central server wiring. All designs follow existing codebase patterns.

### 13.1 Protobuf Definitions

#### 13.1.1 `api/proto/v1/backup.proto` (new file)

```protobuf
syntax = "proto3";
package pgswarm.v1;
option go_package = "github.com/pg-swarm/pg-swarm/api/gen/v1";

import "google/protobuf/timestamp.proto";

// BackupConfig is the per-cluster backup configuration pushed from central to satellite.
// Carried in ClusterConfig.backups (field 18, currently reserved — reuse).
message BackupConfig {
  string   store_id  = 1;   // references a BackupStore in central DB
  string   base_path = 2;   // "<satellite_name>-<namespace>-<cluster_name>"
  PhysicalBackupConfig physical  = 3;
  LogicalBackupConfig  logical   = 4;
  BackupRetention      retention = 5;
  BackupDestination    destination = 6;  // resolved credentials from store
}

message PhysicalBackupConfig {
  string base_schedule        = 1;  // 5-field cron
  string incremental_schedule = 2;  // 5-field cron (optional)
  bool   wal_archive_enabled  = 3;
  int32  archive_timeout_seconds = 4;  // PG archive_timeout
}

message LogicalBackupConfig {
  string          schedule  = 1;  // 5-field cron
  repeated string databases = 2;  // empty = all
  string          format    = 3;  // "custom", "plain", "directory"
}

message BackupRetention {
  int32 base_backup_count        = 1;
  int32 incremental_backup_count = 2;
  int32 wal_retention_days       = 3;
  int32 logical_backup_count     = 4;
}

// BackupDestination carries resolved store config + decrypted credentials.
message BackupDestination {
  string          type = 1;  // "gcs" or "sftp"
  GCSDestination  gcs  = 2;
  SFTPDestination sftp = 3;
}

message GCSDestination {
  string bucket              = 1;
  string path_prefix         = 2;
  string service_account_json = 3;  // decrypted credential
}

message SFTPDestination {
  string host        = 1;
  int32  port        = 2;
  string user        = 3;
  string base_path   = 4;
  string password    = 5;  // decrypted credential
  string private_key = 6;  // decrypted credential
}

// BackupStatusReport is sent satellite → central via gRPC stream.
message BackupStatusReport {
  string cluster_name   = 1;
  string namespace      = 2;
  string backup_type    = 3;  // "base", "incremental", "logical"
  string status         = 4;  // "running", "completed", "failed", "skipped"
  int64  size_bytes     = 5;
  string backup_path    = 6;
  string pg_version     = 7;
  string wal_start_lsn  = 8;
  string wal_end_lsn    = 9;
  string error_message  = 10;
  google.protobuf.Timestamp started_at   = 11;
  google.protobuf.Timestamp completed_at = 12;
}

// RestoreCommand is sent central → satellite via gRPC stream.
message RestoreCommand {
  string cluster_name    = 1;
  string namespace       = 2;
  string restore_id      = 3;  // UUID of the restore_operations row
  string restore_type    = 4;  // "logical", "pitr"
  string restore_mode    = 5;  // "in_place", "new_cluster" (PITR only)
  string backup_id       = 6;  // source backup UUID (logical only)
  string backup_path     = 7;  // storage path to backup
  string target_database = 8;  // logical restore target
  google.protobuf.Timestamp target_time = 9;  // PITR target
}

// RestoreStatusReport is sent satellite → central via gRPC stream.
message RestoreStatusReport {
  string restore_id    = 1;
  string cluster_name  = 2;
  string namespace     = 3;
  string status        = 4;  // "running", "completed", "failed"
  string error_message = 5;
}

// BackupTrigger is sent central → satellite to request an on-demand backup.
message BackupTrigger {
  string cluster_name = 1;
  string namespace    = 2;
  string backup_type  = 3;  // "base", "incremental", "logical"
}
```

#### 13.1.2 Integration into `config.proto`

```protobuf
// In ClusterConfig — reuse reserved field 18:
repeated BackupConfig backups = 18;

// In SatelliteMessage oneof — reuse reserved fields 8, 9:
BackupStatusReport  backup_status  = 8;
RestoreStatusReport restore_status = 9;

// In CentralMessage oneof — reuse reserved field 7 and add new field:
RestoreCommand restore_command = 7;
BackupTrigger  backup_trigger  = 20;  // on-demand backup request
```

### 13.2 Go Model Types

#### 13.2.1 `internal/shared/models/models.go` additions

```go
// Added to ClusterSpec:
type ClusterSpec struct {
    // ... existing fields ...
    Backup             *BackupSpec       `json:"backup,omitempty"` // nil = no backup
}

// Backup configuration attached to a profile/cluster.
type BackupSpec struct {
    StoreID  *uuid.UUID            `json:"store_id,omitempty"`
    Physical *PhysicalBackupConfig `json:"physical,omitempty"`
    Logical  *LogicalBackupConfig  `json:"logical,omitempty"`
}

type PhysicalBackupConfig struct {
    BaseSchedule        string            `json:"base_schedule"`
    IncrementalSchedule string            `json:"incremental_schedule,omitempty"`
    WalArchiveEnabled   bool              `json:"wal_archive_enabled"`
    ArchiveTimeoutSecs  int32             `json:"archive_timeout_seconds,omitempty"`
    Retention           PhysicalRetention `json:"retention"`
}

type PhysicalRetention struct {
    BaseBackupCount        int `json:"base_backup_count"`
    IncrementalBackupCount int `json:"incremental_backup_count,omitempty"`
    WalRetentionDays       int `json:"wal_retention_days"`
}

type LogicalBackupConfig struct {
    Schedule  string           `json:"schedule"`
    Databases []string         `json:"databases,omitempty"`
    Format    string           `json:"format,omitempty"` // "custom"|"plain"|"directory"
    Retention LogicalRetention `json:"retention"`
}

type LogicalRetention struct {
    BackupCount int `json:"backup_count"`
}

// Backup store (central DB entity).
type BackupStore struct {
    ID             uuid.UUID       `json:"id"`
    Name           string          `json:"name"`
    Description    string          `json:"description"`
    StoreType      string          `json:"store_type"` // "gcs"|"sftp"
    Config         json.RawMessage `json:"config"`
    Credentials    []byte          `json:"-"`
    CredentialsSet map[string]bool `json:"credentials_set,omitempty"`
    CreatedAt      time.Time       `json:"created_at"`
    UpdatedAt      time.Time       `json:"updated_at"`
}

// Store config types (plaintext JSONB).
type GCSStoreConfig  struct {
    Bucket     string `json:"bucket"`
    PathPrefix string `json:"path_prefix,omitempty"`
}
type SFTPStoreConfig struct {
    Host     string `json:"host"`
    Port     int    `json:"port,omitempty"`
    User     string `json:"user"`
    BasePath string `json:"base_path"`
}

// Store credential types (encrypted BYTEA).
type GCSStoreCredentials  struct {
    ServiceAccountJSON string `json:"service_account_json,omitempty"`
}
type SFTPStoreCredentials struct {
    Password   string `json:"password,omitempty"`
    PrivateKey string `json:"private_key,omitempty"`
}

// Backup inventory record (central DB).
type BackupInventory struct {
    ID           uuid.UUID  `json:"id" db:"id"`
    SatelliteID  uuid.UUID  `json:"satellite_id" db:"satellite_id"`
    ClusterName  string     `json:"cluster_name" db:"cluster_name"`
    BackupType   string     `json:"backup_type" db:"backup_type"`
    Status       string     `json:"status" db:"status"`
    StartedAt    time.Time  `json:"started_at" db:"started_at"`
    CompletedAt  *time.Time `json:"completed_at,omitempty" db:"completed_at"`
    SizeBytes    int64      `json:"size_bytes" db:"size_bytes"`
    BackupPath   string     `json:"backup_path" db:"backup_path"`
    PgVersion    string     `json:"pg_version" db:"pg_version"`
    WalStartLSN  string     `json:"wal_start_lsn,omitempty" db:"wal_start_lsn"`
    WalEndLSN    string     `json:"wal_end_lsn,omitempty" db:"wal_end_lsn"`
    Databases    []string   `json:"databases,omitempty" db:"databases"`
    ErrorMessage string     `json:"error_message,omitempty" db:"error_message"`
    CreatedAt    time.Time  `json:"created_at" db:"created_at"`
}

// Restore operation record (central DB).
type RestoreOperation struct {
    ID             uuid.UUID  `json:"id" db:"id"`
    SatelliteID    uuid.UUID  `json:"satellite_id" db:"satellite_id"`
    ClusterName    string     `json:"cluster_name" db:"cluster_name"`
    BackupID       *uuid.UUID `json:"backup_id,omitempty" db:"backup_id"`
    RestoreType    string     `json:"restore_type" db:"restore_type"`
    RestoreMode    string     `json:"restore_mode,omitempty" db:"restore_mode"`
    TargetTime     *time.Time `json:"target_time,omitempty" db:"target_time"`
    TargetDatabase string     `json:"target_database,omitempty" db:"target_database"`
    Status         string     `json:"status" db:"status"`
    ErrorMessage   string     `json:"error_message,omitempty" db:"error_message"`
    StartedAt      *time.Time `json:"started_at,omitempty" db:"started_at"`
    CompletedAt    *time.Time `json:"completed_at,omitempty" db:"completed_at"`
    CreatedAt      time.Time  `json:"created_at" db:"created_at"`
}
```

#### 13.2.2 Validation Functions

```go
// ValidateBackupSpec validates the backup configuration.
// nil is valid (no backup configured).
func ValidateBackupSpec(b *BackupSpec) error

// ValidateBackupStore validates a backup store's required fields.
func ValidateBackupStore(store *BackupStore) error

// ComputeCredentialsSet returns which credential fields are non-empty
// without exposing the actual values.
func ComputeCredentialsSet(storeType string, decryptedCreds []byte) map[string]bool
```

Validation rules:
- `BackupSpec`: `store_id` required when physical or logical is set; cron expressions must be 5-field; retention counts >= 1; `wal_retention_days` >= 1 when WAL archiving enabled.
- `BackupStore`: `name` required; `store_type` must be `"gcs"` or `"sftp"`; type-specific config validation (e.g., GCS requires `bucket`, SFTP requires `host` and `base_path`).

### 13.3 Storage Destination Interface

#### 13.3.1 `internal/backup/destination/destination.go`

```go
package destination

import (
    "context"
    "io"
)

// Destination abstracts a remote storage backend for backup files.
type Destination interface {
    // Upload writes data from reader to the given remote path.
    Upload(ctx context.Context, remotePath string, reader io.Reader) error

    // Download returns a reader for the given remote path.
    Download(ctx context.Context, remotePath string) (io.ReadCloser, error)

    // List returns all file paths under the given prefix.
    List(ctx context.Context, prefix string) ([]string, error)

    // Delete removes a file at the given remote path.
    Delete(ctx context.Context, remotePath string) error

    // Exists checks whether a file exists at the given remote path.
    Exists(ctx context.Context, remotePath string) (bool, error)
}

// New creates a Destination from store type, config JSON, and decrypted credentials.
func New(storeType string, config []byte, credentials []byte) (Destination, error)
```

#### 13.3.2 Implementations

| File | Backend | Dependencies |
|------|---------|-------------|
| `gcs.go` | Google Cloud Storage | `cloud.google.com/go/storage` |
| `sftp.go` | SFTP | `github.com/pkg/sftp` + `golang.org/x/crypto/ssh` |

Each implementation parses the config/credentials JSON in its constructor and maintains a connection pool or client instance.

### 13.4 Backup Sidecar Binary

#### 13.4.1 `cmd/backup-sidecar/main.go`

Follows the same pattern as `cmd/failover-sidecar/main.go`: read env vars, construct config, run.

```go
func main() {
    // Read environment variables
    cfg := backup.Config{
        PodName:       os.Getenv("POD_NAME"),
        Namespace:     os.Getenv("POD_NAMESPACE"),
        ClusterName:   os.Getenv("CLUSTER_NAME"),
        PGPassword:    os.Getenv("POSTGRES_PASSWORD"),
        BackupPassword:    os.Getenv("BACKUP_PASSWORD"),
        PrimaryHost:       os.Getenv("PRIMARY_HOST"),
        SatelliteAddr:     os.Getenv("SATELLITE_ADDR"),
        SidecarStreamToken: os.Getenv("SIDECAR_STREAM_TOKEN"),
        StoreType:         os.Getenv("BACKUP_STORE_TYPE"),
        StoreConfig:      os.Getenv("BACKUP_STORE_CONFIG"),
        StoreCredentials: os.Getenv("BACKUP_STORE_CREDENTIALS"),
        BasePath:         os.Getenv("BACKUP_BASE_PATH"),
        // Schedules and retention from config ConfigMap (mounted as env)
        BaseSchedule:        os.Getenv("BACKUP_BASE_SCHEDULE"),
        IncrementalSchedule: os.Getenv("BACKUP_INCREMENTAL_SCHEDULE"),
        LogicalSchedule:     os.Getenv("BACKUP_LOGICAL_SCHEDULE"),
        LogicalDatabases:    os.Getenv("BACKUP_LOGICAL_DATABASES"),
        LogicalFormat:       os.Getenv("BACKUP_LOGICAL_FORMAT"),
        WalArchiveEnabled:   os.Getenv("BACKUP_WAL_ARCHIVE_ENABLED") == "true",
        ArchiveTimeout:      parseIntEnv("BACKUP_ARCHIVE_TIMEOUT", 300),
        RetentionBase:       parseIntEnv("BACKUP_RETENTION_BASE", 2),
        RetentionIncremental: parseIntEnv("BACKUP_RETENTION_INCREMENTAL", 0),
        RetentionWalDays:    parseIntEnv("BACKUP_RETENTION_WAL_DAYS", 7),
        RetentionLogical:    parseIntEnv("BACKUP_RETENTION_LOGICAL", 5),
    }

    ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
    defer cancel()

    sidecar, err := backup.NewSidecar(cfg)
    // ...
    if err := sidecar.Run(ctx); err != nil && ctx.Err() == nil {
        log.Fatal().Err(err).Msg("backup sidecar exited with error")
    }
}
```

#### 13.4.2 Environment Variable Contract

| Variable | Source | Description |
|----------|--------|-------------|
| `POD_NAME` | fieldRef `metadata.name` | Pod identity |
| `POD_NAMESPACE` | fieldRef `metadata.namespace` | Namespace |
| `CLUSTER_NAME` | ConfigMap | Cluster name |
| `PRIMARY_HOST` | Computed | `<cluster>-0.<cluster>.<ns>.svc.cluster.local` |
| `SATELLITE_ADDR` | Hardcoded | Satellite sidecar gRPC server address |
| `SIDECAR_STREAM_TOKEN` | Secret `sidecar-stream-token` | Auth token for sidecar gRPC stream |
| `POSTGRES_PASSWORD` | Secret `superuser-password` | PG superuser password |
| `BACKUP_PASSWORD` | Secret `backup-password` | `backup_user` password |
| `BACKUP_STORE_TYPE` | ConfigMap | `"gcs"` or `"sftp"` |
| `BACKUP_STORE_CONFIG` | ConfigMap | Store config JSON |
| `BACKUP_STORE_CREDENTIALS` | Secret | Decrypted credentials JSON |
| `BACKUP_BASE_PATH` | ConfigMap | Storage path prefix |
| `BACKUP_BASE_SCHEDULE` | ConfigMap | Base backup cron |
| `BACKUP_INCREMENTAL_SCHEDULE` | ConfigMap | Incremental backup cron |
| `BACKUP_LOGICAL_SCHEDULE` | ConfigMap | Logical backup cron |
| `BACKUP_LOGICAL_DATABASES` | ConfigMap | Comma-separated DB list |
| `BACKUP_LOGICAL_FORMAT` | ConfigMap | `"custom"`, `"plain"`, `"directory"` |
| `BACKUP_WAL_ARCHIVE_ENABLED` | ConfigMap | `"true"` or `"false"` |
| `BACKUP_ARCHIVE_TIMEOUT` | ConfigMap | Seconds |
| `BACKUP_RETENTION_*` | ConfigMap | Retention policy values |

### 13.5 Sidecar Internals (`internal/backup/`)

#### 13.5.1 Package Structure

```
internal/backup/
├── sidecar.go          # Sidecar struct, lifecycle, role detection
├── config.go           # Config struct, parsing
├── scheduler.go        # Cron scheduler, health gates
├── wal.go              # WAL archiver (staging watcher + upload)
├── physical.go         # Base and incremental backup execution
├── logical.go          # pg_dump execution
├── retention.go        # Cleanup worker
├── reporter.go         # ConfigMap status writer
├── connector.go        # gRPC stream to satellite (restore commands, on-demand triggers)
└── destination/
    ├── destination.go  # Interface + factory
    ├── gcs.go          # GCS implementation
    └── sftp.go         # SFTP implementation
```

#### 13.5.2 Sidecar Lifecycle (`sidecar.go`)

```go
type Sidecar struct {
    cfg       Config
    dest      destination.Destination
    scheduler *Scheduler
    archiver  *WALArchiver
    retention *RetentionWorker
    reporter  *Reporter
    connector *Connector
    role      string  // "primary" or "replica"
    mu        sync.Mutex
}

func NewSidecar(cfg Config) (*Sidecar, error)
func (s *Sidecar) Run(ctx context.Context) error
```

**Run loop:**
1. Initialize storage destination from config.
2. Connect to satellite sidecar gRPC server (same pattern as failover sidecar connector).
3. Enter role detection loop (every 10 seconds):
   - Query `SELECT pg_is_in_recovery()` via local PG connection.
   - If role changed: deactivate old services, activate new.
4. **Primary activation**: start WAL archiver (watch `/wal-staging`).
5. **Replica activation**: start scheduler (base, incremental, logical), start retention worker.
6. Listen for `SidecarCommand` messages from satellite (restore commands, on-demand backup triggers).
7. On context cancellation: gracefully stop all services.

#### 13.5.3 Scheduler (`scheduler.go`)

```go
type Scheduler struct {
    dest       destination.Destination
    baseCron   string
    incrCron   string
    logicalCron string
    // ...
}

func (s *Scheduler) Run(ctx context.Context)
```

**Scheduling logic:**
- Evaluate cron expressions every 60 seconds against current time.
- Before executing a physical backup, check replication health:
  - Query `pg_stat_wal_receiver` for `last_msg_receipt_time`.
  - If lag exceeds configurable threshold (default: 60 seconds), skip and report `status=skipped`.
- Logical backups skip health check.
- Only one backup runs at a time (mutex-protected).

#### 13.5.4 WAL Archiver (`wal.go`)

```go
type WALArchiver struct {
    dest      destination.Destination
    basePath  string
    stagingDir string  // "/wal-staging"
}

func (w *WALArchiver) Run(ctx context.Context) error
```

**Watch loop (1-second poll):**
1. `os.ReadDir("/wal-staging")` — list files.
2. For each file:
   a. Open and gzip-compress into memory buffer.
   b. Upload to `<basePath>/wal/<filename>.gz`.
   c. On success: `os.Remove()` the local file.
   d. On failure: log warning, leave file for retry on next poll.

#### 13.5.5 Physical Backup (`physical.go`)

```go
func (s *Sidecar) runBaseBackup(ctx context.Context) error
func (s *Sidecar) runIncrementalBackup(ctx context.Context) error
```

Note: `basePath` in all operations below refers to the full resolved path `<store_base_path>/<satellite_name>-<namespace>-<cluster_name>` (set in `BackupConfig.base_path` by central).

**Base backup:**
1. Generate timestamp directory: `base/20260324T020000/`.
2. Execute: `pg_basebackup -h <primary> -U backup_user -D - --checkpoint=fast --wal-method=none -Ft -z`
3. Stream stdout directly to `dest.Upload(ctx, "<basePath>/base/<ts>/base.tar.gz", stdout)`.
4. Save manifest: `pg_basebackup` produces `backup_manifest` — upload as `manifest.json`.
5. Report status via reporter.

**Incremental backup:**
1. Download latest base manifest from storage.
2. Execute: `pg_basebackup -h <primary> -U backup_user -D - --incremental=<manifest_path> -Ft -z`
3. Upload to `<basePath>/incremental/<ts>/incremental.tar.gz`.
4. If manifest missing or invalid: fall back to full base backup.

#### 13.5.6 Logical Backup (`logical.go`)

```go
func (s *Sidecar) runLogicalBackup(ctx context.Context) error
```

1. Determine database list: if configured, use it; otherwise query `SELECT datname FROM pg_database WHERE datistemplate = false AND datname != 'postgres'`.
2. For each database:
   - Execute: `pg_dump -h localhost -U backup_user -Fc <dbname>` (format varies by config).
   - Stream to `dest.Upload(ctx, "<basePath>/logical/<ts>/<dbname>.dump", stdout)`.
3. Report status with database list.

#### 13.5.7 Retention Worker (`retention.go`)

```go
type RetentionWorker struct {
    dest      destination.Destination
    basePath  string
    retention Config  // retention counts/days
}

func (r *RetentionWorker) Run(ctx context.Context) // runs every hour
```

**Logic:**
1. List `<basePath>/base/` — sort by timestamp descending.
2. Keep newest `base_backup_count`, delete the rest (including their incrementals).
3. List `<basePath>/incremental/` per base — keep newest `incremental_backup_count` per base.
4. List `<basePath>/wal/` — delete segments older than `wal_retention_days`, but never delete WAL needed by the oldest retained base (by comparing LSN).
5. List `<basePath>/logical/` — keep newest `logical_backup_count`, delete the rest.

#### 13.5.8 Reporter (`reporter.go`)

```go
type Reporter struct {
    connector *Connector  // sends status via gRPC stream
    client    kubernetes.Interface  // writes to ConfigMap as fallback
    namespace   string
    clusterName string
}

func (r *Reporter) ReportStatus(ctx context.Context, report BackupStatus) error
```

Reports backup status through two channels:
1. **Primary**: sends `BackupStatusReport` via the gRPC stream connector to the satellite, which forwards to central.
2. **Fallback**: writes a JSON-encoded status to ConfigMap `<cluster>-backup-status` for the satellite health monitor to pick up (handles cases where the gRPC stream is temporarily disconnected).

#### 13.5.9 gRPC Connector (`connector.go`)

Connects to the satellite's sidecar gRPC server (`SidecarStreamService`) using the same bidirectional streaming pattern as the failover sidecar connector (`internal/failover/connector.go`).

```go
type Connector struct {
    satelliteAddr string
    token         string
    identity      *pgswarmv1.SidecarIdentity
    onRestore     func(*pgswarmv1.RestoreCommand)
    onTrigger     func(backupType string) // on-demand trigger from dashboard
}

func NewConnector(addr, token string, identity *pgswarmv1.SidecarIdentity) *Connector
func (c *Connector) Run(ctx context.Context) error  // reconnect loop with backoff
func (c *Connector) SendStatus(report *pgswarmv1.BackupStatusReport) // non-blocking
```

**Behavior:**
- Authenticates with `SIDECAR_STREAM_TOKEN` from cluster Secret.
- Sends `SidecarIdentity` on connect (with `sidecar_type: "backup"`).
- Receives `SidecarCommand` messages — dispatches restore commands and on-demand backup triggers.
- Sends backup status reports upstream to satellite (which forwards to central).
- Reconnects with exponential backoff on disconnect.

### 13.6 Operator Manifest Building

#### 13.6.1 `internal/satellite/operator/manifest_backup.go` (new file)

```go
package operator

// backupEnabled returns true if the cluster config has any backup rules.
func backupEnabled(cfg *pgswarmv1.ClusterConfig) bool {
    return len(cfg.Backups) > 0
}

// Naming helpers
func backupServiceAccountName(clusterName string) string  // resourceName(cluster, "backup")
func backupCredentialSecretName(clusterName string) string // resourceName(cluster, "backup-creds")
func backupConfigConfigMapName(clusterName string) string  // resourceName(cluster, "backup-config")
func backupStatusConfigMapName(clusterName string) string  // resourceName(cluster, "backup-status")

// RBAC builders — same pattern as buildFailoverServiceAccount/Role/RoleBinding.
// NOTE: When both failover AND backup are enabled, there is only one ServiceAccount
// per pod (K8s constraint). The backup sidecar runs under the failover SA, and the
// failover Role gets additional configmap permissions appended. A separate backup SA
// is only created when failover is disabled but backup is enabled.
func buildBackupServiceAccount(cfg *pgswarmv1.ClusterConfig) *corev1.ServiceAccount
func buildBackupRole(cfg *pgswarmv1.ClusterConfig) *rbacv1.Role
func buildBackupRoleBinding(cfg *pgswarmv1.ClusterConfig) *rbacv1.RoleBinding

// ensureBackupRBAC handles the SA coexistence logic:
//   - If failover enabled: append configmap rules to failover Role, skip separate backup SA.
//   - If failover disabled: create dedicated backup SA, Role, RoleBinding.
func ensureBackupRBAC(ctx context.Context, client kubernetes.Interface, cfg *pgswarmv1.ClusterConfig) error

// Backup Role permissions:
//   - pods: get (for role detection)
//   - configmaps: get, create, update, patch (for status reporting)

// Credential Secret — contains decrypted store credentials from central
func buildBackupCredentialSecret(cfg *pgswarmv1.ClusterConfig, backup *pgswarmv1.BackupConfig) *corev1.Secret

// Config ConfigMap — contains schedules, retention, store type, base path
func buildBackupConfigConfigMap(cfg *pgswarmv1.ClusterConfig) *corev1.ConfigMap

// Sidecar container builder
func buildBackupSidecar(cfg *pgswarmv1.ClusterConfig, secretName string) corev1.Container
```

#### 13.6.2 Sidecar Container Spec

```go
func buildBackupSidecar(cfg *pgswarmv1.ClusterConfig, secretName string) corev1.Container {
    backup := cfg.Backups[0]  // one backup config per cluster
    return corev1.Container{
        Name:            "backup",
        Image:           defaultBackupSidecarImage,
        ImagePullPolicy: corev1.PullIfNotPresent,
        Env: []corev1.EnvVar{
            {Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{
                FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
            {Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{
                FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
            {Name: "CLUSTER_NAME", Value: cfg.ClusterName},
            {Name: "PRIMARY_HOST", Value: primaryHostDNS(cfg)},
            // gRPC connection to satellite sidecar server (same as failover sidecar)
            {Name: "SATELLITE_ADDR", Value: "pg-swarm-satellite.pgswarm-system.svc.cluster.local:9091"},
            {Name: "SIDECAR_STREAM_TOKEN", ValueFrom: secretKeyRef(secretName, "sidecar-stream-token")},
            // Passwords from cluster secret
            {Name: "POSTGRES_PASSWORD", ValueFrom: secretKeyRef(secretName, "superuser-password")},
            {Name: "BACKUP_PASSWORD", ValueFrom: secretKeyRef(secretName, "backup-password")},
            // Store credentials from backup credential secret
            {Name: "BACKUP_STORE_CREDENTIALS", ValueFrom: secretKeyRef(
                backupCredentialSecretName(cfg.ClusterName), "credentials")},
            // Config from backup config ConfigMap
            envFromConfigMap(backupConfigConfigMapName(cfg.ClusterName), "BACKUP_STORE_TYPE"),
            envFromConfigMap(backupConfigConfigMapName(cfg.ClusterName), "BACKUP_STORE_CONFIG"),
            envFromConfigMap(backupConfigConfigMapName(cfg.ClusterName), "BACKUP_BASE_PATH"),
            // ... all BACKUP_* schedule/retention env vars from ConfigMap
        },
        VolumeMounts: []corev1.VolumeMount{
            {Name: "data", MountPath: "/var/lib/postgresql/data"},
            {Name: "wal-staging", MountPath: "/wal-staging"},
            {Name: "wal-restore", MountPath: "/wal-restore"},
        },
    }
}
```

#### 13.6.3 Volume Injection

When backup is enabled, `buildStatefulSet()` adds two emptyDir volumes:

```go
if backupEnabled(cfg) {
    sts.Spec.Template.Spec.Volumes = append(sts.Spec.Template.Spec.Volumes,
        corev1.Volume{Name: "wal-staging", VolumeSource: corev1.VolumeSource{
            EmptyDir: &corev1.EmptyDirVolumeSource{}}},
        corev1.Volume{Name: "wal-restore", VolumeSource: corev1.VolumeSource{
            EmptyDir: &corev1.EmptyDirVolumeSource{}}},
    )
    sts.Spec.Template.Spec.Containers = append(sts.Spec.Template.Spec.Containers,
        buildBackupSidecar(cfg, secretName))
    // Add wal-staging and wal-restore mounts to the postgres container too
}
```

The main postgres container also gets volume mounts for `/wal-staging` and `/wal-restore` so that `archive_command` and `restore_command` can write/read WAL files.

#### 13.6.4 Reconciliation Integration (`operator.go`)

Added as step 10 in `reconcile()`, after pod labeling:

```go
// 10. Backup sidecar RBAC, credentials, and config
if backupEnabled(cfg) {
    if err := ensureBackupRBAC(ctx, o.client, cfg); err != nil {
        return fmt.Errorf("backup RBAC: %w", err)
    }
    for _, backup := range cfg.Backups {
        secret := buildBackupCredentialSecret(cfg, backup)
        if secret != nil {
            if err := createOrPreserveSecret(ctx, o.client, secret); err != nil {
                return fmt.Errorf("backup credential secret: %w", err)
            }
        }
    }
    if err := createOrUpdateConfigMap(ctx, o.client, buildBackupConfigConfigMap(cfg)); err != nil {
        return fmt.Errorf("backup config configmap: %w", err)
    }
} else {
    cleanupBackupResources(ctx, o.client, cfg.Namespace, cfg.ClusterName)
}
```

#### 13.6.5 PostgreSQL Configuration

When backup is enabled, `buildPostgresConf()` in `manifest_configmap.go` adds:

```
archive_mode = on
archive_command = 'cp %p /wal-staging/%f'
archive_timeout = <configured_seconds>
```

When incremental backups are configured:

```
summarize_wal = on
```

The `buildHbaConf()` function adds:

```
host replication backup_user all scram-sha-256
```

The `buildSecret()` function adds `"backup-password": randomPassword(24)` to the cluster secret.

The `pg-init.sh` script creates the `backup_user` role:

```bash
psql -U postgres -c "CREATE ROLE backup_user WITH REPLICATION LOGIN PASSWORD '$BACKUP_PASSWORD' IN ROLE pg_read_all_data;"
```

### 13.7 Central Server Integration

#### 13.7.1 Store Interface Additions (`internal/central/store/store.go`)

```go
// Backup Stores
CreateBackupStore(ctx context.Context, store *models.BackupStore) error
GetBackupStore(ctx context.Context, id uuid.UUID) (*models.BackupStore, error)
ListBackupStores(ctx context.Context) ([]*models.BackupStore, error)
UpdateBackupStore(ctx context.Context, store *models.BackupStore) error
DeleteBackupStore(ctx context.Context, id uuid.UUID) error

// Backup Inventory
CreateBackupInventory(ctx context.Context, inv *models.BackupInventory) error
UpdateBackupInventory(ctx context.Context, inv *models.BackupInventory) error
ListBackupInventory(ctx context.Context, satelliteID uuid.UUID, clusterName string) ([]*models.BackupInventory, error)
GetBackupInventory(ctx context.Context, id uuid.UUID) (*models.BackupInventory, error)

// Restore Operations
CreateRestoreOperation(ctx context.Context, op *models.RestoreOperation) error
UpdateRestoreOperation(ctx context.Context, op *models.RestoreOperation) error
GetRestoreOperation(ctx context.Context, id uuid.UUID) (*models.RestoreOperation, error)
ListRestoreOperations(ctx context.Context, satelliteID uuid.UUID, clusterName string) ([]*models.RestoreOperation, error)
```

#### 13.7.2 REST Handlers (`internal/central/server/rest.go`)

Route registration:

```go
// Backup Stores
api.Get("/backup-stores", s.listBackupStores)
api.Post("/backup-stores", s.createBackupStore)
api.Get("/backup-stores/:id", s.getBackupStore)
api.Put("/backup-stores/:id", s.updateBackupStore)
api.Delete("/backup-stores/:id", s.deleteBackupStore)

// Backup Inventory, On-Demand Trigger & Restore
api.Get("/clusters/:id/backups", s.listClusterBackups)
api.Get("/backups/:id", s.getBackup)
api.Post("/clusters/:id/backups/trigger", s.triggerBackup)
api.Post("/clusters/:id/restore", s.initiateRestore)
api.Get("/clusters/:id/restores", s.listClusterRestores)
```

Key handler pattern for `createBackupStore`:
1. Parse body (name, description, store_type, config, credentials).
2. Validate with `models.ValidateBackupStore()`.
3. Encrypt credentials with `s.encryptor.Encrypt()`.
4. Store via `s.store.CreateBackupStore()`.
5. Return with `computeStoreCredentialsSet()` (never raw credentials).

Key handler pattern for `triggerBackup`:
1. Parse body (`backup_type`: `"base"`, `"incremental"`, or `"logical"`).
2. Look up cluster config and validate backup is enabled.
3. Create `BackupInventory` record with status `"pending"`.
4. Push `BackupTrigger` via `s.streams.PushBackupTrigger()` to the satellite.
5. Return `201` with the inventory record. Sidecar updates status to `"running"` when it starts.

Key handler pattern for `initiateRestore`:
1. Parse body (backup_id or target_time, restore_type, restore_mode).
2. Validate backup exists (for logical) or WAL coverage (for PITR).
3. Create `RestoreOperation` record in DB.
4. Push `RestoreCommand` via `s.streams.PushRestoreCommand()`.
5. Return operation with status `"pending"`.

#### 13.7.3 Proto Config Builder (`rest.go` — `buildProtoConfig`)

Inside the existing `buildProtoConfig()` function that assembles `*pgswarmv1.ClusterConfig`:

```go
if spec.Backup != nil && spec.Backup.StoreID != nil {
    store, err := st.GetBackupStore(ctx, *spec.Backup.StoreID)
    if err != nil { /* warn and skip */ }
    backupCfg := &pgswarmv1.BackupConfig{
        StoreId:     spec.Backup.StoreID.String(),
        BasePath:    fmt.Sprintf("%s-%s-%s", satelliteName, cfg.Namespace, cfg.Name),
        Destination: buildBackupDestination(store, enc),
    }
    // Map physical and logical config...
    protoConfig.Backups = append(protoConfig.Backups, backupCfg)
}
```

`buildBackupDestination()` decrypts store credentials and maps to proto `BackupDestination`.

#### 13.7.4 gRPC Message Handling (`internal/central/server/grpc.go`)

In the `Connect()` stream handler's message dispatch switch:

```go
case *pgswarmv1.SatelliteMessage_BackupStatus:
    s.handleBackupStatusReport(ctx, satID, payload.BackupStatus)

case *pgswarmv1.SatelliteMessage_RestoreStatus:
    s.handleRestoreStatusReport(ctx, payload.RestoreStatus)
```

`handleBackupStatusReport`: upserts `BackupInventory` record, creates `Event`, notifies WebSocket.

`handleRestoreStatusReport`: updates `RestoreOperation` status, creates `Event`, notifies WebSocket.

`PushRestoreCommand` on `StreamManager`: wraps `RestoreCommand` in `CentralMessage` and sends to the satellite's stream channel.

`PushBackupTrigger` on `StreamManager`: wraps `BackupTrigger` in `CentralMessage` and sends to the satellite's stream channel. The satellite forwards it to the backup sidecar via the sidecar gRPC stream.

#### 13.7.5 WebSocket (`internal/central/server/ws.go`)

In `fetchState()`, add backup stores to the state snapshot:

```go
if v, err := s.ListBackupStores(ctx); err == nil {
    state["backupStores"] = v
}
```

### 13.8 Satellite Integration

#### 13.8.1 Health Monitor (`internal/satellite/health/monitor.go`)

Add a `checkBackupStatuses()` method that:
1. For each managed cluster with backup enabled, reads ConfigMap `<cluster>-backup-status`.
2. Parses the JSON status and builds a `pgswarmv1.BackupStatusReport`.
3. Calls the `onBackup` callback to send it upstream.

Add `SetOnBackup(fn func(*pgswarmv1.BackupStatusReport))` following the existing `SetOnHealth`/`SetOnEvent` pattern.

Exclude backup pods from health checks by filtering on a `pg-swarm.io/backup-type` label in the pod list selector.

#### 13.8.2 Stream Connector (`internal/satellite/stream/connector.go`)

Add `OnRestoreCommand func(*pgswarmv1.RestoreCommand)` and `OnBackupTrigger func(*pgswarmv1.BackupTrigger)` fields.

In the message dispatch switch:

```go
case *pgswarmv1.CentralMessage_RestoreCommand:
    if c.OnRestoreCommand != nil {
        c.OnRestoreCommand(payload.RestoreCommand)
    }

case *pgswarmv1.CentralMessage_BackupTrigger:
    if c.OnBackupTrigger != nil {
        c.OnBackupTrigger(payload.BackupTrigger)
    }
```

#### 13.8.3 Agent Wiring (`internal/satellite/agent/agent.go`)

Wire the callbacks:

```go
// Restore command from central → forward to backup sidecar via sidecar stream
a.connector.OnRestoreCommand = func(cmd *pgswarmv1.RestoreCommand) {
    log.Info().Str("cluster", cmd.ClusterName).Str("type", cmd.RestoreType).Msg("restore command received")
    a.sidecarManager.ForwardRestoreCommand(cmd)
}

// On-demand backup trigger from central → forward to backup sidecar via sidecar stream
a.connector.OnBackupTrigger = func(trigger *pgswarmv1.BackupTrigger) {
    log.Info().Str("cluster", trigger.ClusterName).Str("type", trigger.BackupType).Msg("backup trigger received")
    a.sidecarManager.ForwardBackupTrigger(trigger)
}

// Backup status from health monitor → send upstream
mon.SetOnBackup(func(report *pgswarmv1.BackupStatusReport) {
    a.connector.SendMessage(&pgswarmv1.SatelliteMessage{
        Payload: &pgswarmv1.SatelliteMessage_BackupStatus{BackupStatus: report},
    })
})
```
