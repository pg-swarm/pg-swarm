# Backup and Restore Architecture

## Overview

pg-swarm provides managed backup and restore through **backup profiles** — independent entities that attach/detach from cluster profiles. Each profile supports at most **one physical rule** (base backup + WAL archiving) and **one logical rule** (pg_dump). Each rule targets a single destination. When attached, the satellite operator automatically creates CronJobs and configures WAL archiving. When detached, CronJobs are cleaned up with zero impact on running postgres pods.

## Data Flow

```
BackupProfile ──┐
BackupProfile ──┼─attach──> ClusterProfile ──DeploymentRule──> ClusterConfig
BackupProfile ──┘                                                    │
                                                    gRPC push (repeated BackupConfig)
                                                                  │
                                                           Satellite Operator
                                                   ┌──────────┬──┴──┬──────────┐
                                                   ▼          ▼     ▼          ▼
                                            CronJob:    CronJob:  CronJob:  postgresql.conf:
                                            base-S3     base-GCS  logical   archive_command
                                                   └──────────┬─────┘
                                                              ▼
                                                   BackupStatusReport (gRPC)
                                                              │
                                                       Central Server
                                                              │
                                                  ┌───────────┴───────────┐
                                                  ▼                       ▼
                                          backup_inventory        restore_operations
```

## Why CronJobs (not sidecars)

Backup rules attach and detach dynamically from profiles. This rules out sidecars:

- **CronJob**: attach = create CronJob, detach = delete it. Zero impact on postgres pods.
- **Sidecar**: attach = update StatefulSet = rolling restart of all pods. VolumeClaimTemplates are immutable, so backup volumes can't be added to existing StatefulSets either.

CronJobs also consume zero resources when idle, fail independently of postgres, and pick up credential rotations automatically on the next run.

WAL archiving is the one continuous operation, but postgres handles this natively via `archive_command` in postgresql.conf — no sidecar needed.

## Backup Profile Spec

A backup profile contains four sections:

```json
{
  "physical": {
    "base_schedule": "0 2 * * 0",
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

Both `physical` and `logical` are optional — a rule can do one or both.

## Destination Types

| Type | Use Case | Credentials |
|------|----------|-------------|
| `s3` | AWS S3 or S3-compatible (MinIO) | `access_key_id` + `secret_access_key` |
| `gcs` | Google Cloud Storage | `service_account_json` |
| `sftp` | Remote SFTP server | `password` or `private_key` |
| `local` | PVC on the satellite cluster | None (just `size` + optional `storage_class`) |

Credentials are stored in a K8s Secret (`<cluster>-backup-creds`) on the satellite, created by the operator.

## WAL Archiving

When `physical.wal_archive_enabled` is true, the operator auto-configures `archive_mode = on` and sets `archive_command` based on the destination type:

| Destination | archive_command |
|-------------|-----------------|
| S3 | `pg-swarm-backup wal-push --dest s3 --bucket $BUCKET --prefix $PREFIX %p %f` |
| GCS | `pg-swarm-backup wal-push --dest gcs --bucket $BUCKET --prefix $PREFIX %p %f` |
| SFTP | `pg-swarm-backup wal-push --dest sftp --host $HOST --path $PATH %p %f` |
| Local | `test ! -f /backup-storage/wal/%f && cp %p /backup-storage/wal/%f` |

This reuses the existing custom archive mode infrastructure in `manifest_configmap.go`. The `pg-swarm-backup` binary is installed in the postgres container via an init container.

## Attach / Detach Flow

**Attach** (`POST /api/v1/profiles/:id/attach-backup-profile`):
1. Insert row into `profile_backup_profiles` join table
2. Bump `config_version` on all ClusterConfigs linked via deployment rules
3. Re-push configs to satellites (now includes this rule in `repeated BackupConfig`)
4. Operator creates: per-rule credential Secret, per-rule CronJobs, configures `archive_command` from first WAL-enabled rule

**Detach** (`POST /api/v1/profiles/:id/detach-backup-profile`):
1. Delete row from `profile_backup_profiles` join table
2. Bump `config_version` on all linked ClusterConfigs
3. Re-push configs (rule removed from `repeated BackupConfig`)
4. Operator removes: that rule's CronJobs and credential Secret. If no rules remain, resets `archive_command`

## CronJob Design

- **Image**: `ghcr.io/pg-swarm/pg-swarm-backup:latest` — Alpine with `postgresql17-client`, `aws-cli`, `openssh-client`, `rclone`
- **Env vars**: `PGPASSWORD` from cluster Secret, destination creds from `<cluster>-backup-creds` Secret
- **Upload**: Shell functions selected by `$DEST_TYPE` env var
- **Retention**: Enforced in the same CronJob script after successful backup
- **Status**: CronJob writes completion JSON to a ConfigMap (`<cluster>-backup-status`). The health monitor reads it each tick and sends `BackupStatusReport` via the existing gRPC stream.

## Status Reporting

```
CronJob completion → ConfigMap (<cluster>-backup-status)
                          │
                   Health monitor tick
                          │
                   BackupStatusReport (gRPC stream)
                          │
                   Central: upsert backup_inventory row + create Event
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

## Database Tables

**`backup_profiles`** — Rule definitions (name, config JSONB). Independent of profiles.

**`profile_backup_profiles`** — Join table (profile_id, backup_profile_id). Many-to-many. `ON DELETE CASCADE` both sides.

**`backup_inventory`** — One row per completed (or failed) backup. Tracked per satellite + cluster. Contains backup type, status, size, WAL LSN range, path.

**`restore_operations`** — One row per restore attempt. Tracks type (pitr/logical), target time, status, errors.

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

## Implementation Phases

| Phase | Scope | Key Files |
|-------|-------|-----------|
| 1 | Models + Migrations + Store | `models.go`, `store.go`, `postgres.go`, `011/012/013.sql` |
| 2 | Proto + codegen | `backup.proto`, `config.proto`, `make proto` |
| 3 | REST API + config push | `rest.go` |
| 4 | Operator CronJobs + WAL auto-config | `manifest_backup.go`, `operator.go` |
| 5 | Backup CronJob image | `Dockerfile.backup` |
| 6 | Status reporting (gRPC) | `grpc.go`, `connector.go`, `monitor.go` |
| 7 | Restore flow | `manifest_backup.go`, `operator.go`, `rest.go` |
| 8 | Dashboard UI | `BackupProfiles.jsx`, `Profiles.jsx`, `ClusterDetail.jsx`, `api.js` |
