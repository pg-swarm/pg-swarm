# Backup Feature Implementation Plan

## Context

pg-swarm manages PostgreSQL HA clusters across edge K8s clusters but has no backup capability. A comprehensive design spec exists at `docs/BACKUP.md` covering physical backups (base + incremental + WAL), logical backups (pg_dump), GCS/SFTP storage, PITR, and a backup sidecar architecture. Proto fields are reserved (7-9, 18) but no code exists. This plan implements the full feature in small, incremental steps — starting from the UI and working backward.

---

## Phase 1: Database Schema + Go Models (Steps 1-3)

### Step 1: Migration — `backup_stores` table
- **New file**: `internal/central/store/migrations/023_backup_stores.sql`
- Schema from BACKUP.md §11.1: `id`, `name` (unique), `description`, `store_type` (gcs/sftp), `config` (JSONB), `credentials` (BYTEA — AES-256-GCM encrypted), timestamps
- **Verify**: `make build`

### Step 2: Migration — `backup_inventory` + `restore_operations` tables
- **New file**: `internal/central/store/migrations/024_backup_inventory.sql`
- Both tables from BACKUP.md §11.2-11.3 in one migration
- Index: `idx_backup_inventory_cluster ON (satellite_id, cluster_name, started_at DESC)`
- **Verify**: `make build`

### Step 3: Go model types
- **Modify**: `internal/shared/models/models.go`
- Add structs: `BackupStore`, `BackupInventory`, `RestoreOperation`
- Add config types: `BackupSpec` (store_id, physical, logical), `PhysicalBackupConfig`, `LogicalBackupConfig`, `BackupRetention`
- Add storage types: `GCSStoreConfig`, `SFTPStoreConfig`
- Add `Backup *BackupSpec` field to `ClusterSpec`
- Add `ValidateBackupStore()`, `ValidateBackupSpec()`
- **Verify**: `make build`, `make test`

---

## Phase 2: Store Layer + REST API for Backup Stores (Steps 4-6)

### Step 4: Store interface + Postgres implementation for backup stores
- **Modify**: `internal/central/store/store.go` — add `CreateBackupStore`, `GetBackupStore`, `ListBackupStores`, `UpdateBackupStore`, `DeleteBackupStore` to interface
- **Modify**: `internal/central/store/postgres.go` — implement all five; follow `StorageTier` CRUD pattern
- Delete checks if any profile references the store (in-use protection)
- **Verify**: `make build`, `make test`

### Step 5: REST API endpoints for backup store CRUD
- **Modify**: `internal/central/server/rest.go`
- Register 5 routes in `setupRoutes()`: `GET/POST /backup-stores`, `GET/PUT/DELETE /backup-stores/:id`
- Handlers: encrypt credentials on create/update via `s.encryptor.Encrypt()`; never return raw credentials — return `credentials_set` map instead; preserve existing credentials on partial update
- Add audit log calls following existing pattern
- **Verify**: `make build`, `make test`

### Step 6: Wire backup stores into WebSocket + DataContext + api.js
- **Modify**: `internal/central/server/ws.go` — add `backupStores` to `fetchState()`
- **Modify**: `dashboard/src/api.js` — add `backupStores()`, `createBackupStore()`, `updateBackupStore()`, `deleteBackupStore()`
- **Modify**: `dashboard/src/context/DataContext.jsx` — add `backupStores` to EMPTY, refresh(), and WS state
- **Verify**: `make build`, `npm run build`

---

## Phase 3: Dashboard UI — Backup Stores + Profile Backup Tab (Steps 7-9)

### Step 7: Admin page — Backup Stores tab
- **Modify**: `dashboard/src/pages/Admin.jsx`
- Add 6th tab: "Backup Stores" (HardDrive icon)
- Follow `StorageTiersTab` pattern: table + inline create/edit form
- Table columns: Name, Type (gcs/sftp badge), Description, Credentials (set/not set badges), Actions
- Form: name, description, store_type selector, dynamic config fields (GCS: bucket + path_prefix; SFTP: host + port + user + base_path), credential password inputs
- Delete with in-use error handling
- **Verify**: `npm run build`

### Step 8: Profile editor — Backup tab
- **Modify**: `dashboard/src/pages/Profiles.jsx`
- Add tab `{ id: 'backup', label: 'Backup' }` to profile editor tabs
- Add `backup: null` to `emptySpec()`
- Tab content: store selector dropdown, Physical section (enable toggle, base_schedule cron, incremental_schedule cron, WAL archive toggle, archive_timeout, retention fields), Logical section (enable toggle, schedule cron, databases input, format selector, retention)
- **Verify**: `npm run build`

### Step 9: Profile save/load roundtrip for backup config
- **Modify**: `internal/central/server/rest.go` — call `ValidateBackupSpec()` in profile create/update handlers
- **Modify**: `dashboard/src/pages/Profiles.jsx` — ensure backup state loads correctly in `startEdit()`
- Validation: if backup is set, store_id required; cron must be valid 5-field; retention counts >= 1
- **Verify**: `make build`, `make test`, `npm run build`; create profile with backup, reload, verify persistence

---

## Phase 4: Proto Messages + Central-to-Satellite Config Flow (Steps 10-12)

### Step 10: Proto definitions
- **New file**: `api/proto/v1/backup.proto` — messages from BACKUP.md §13.1.1: `BackupConfig`, `PhysicalBackupConfig`, `LogicalBackupConfig`, `BackupRetention`, `BackupDestination`, `GCSDestination`, `SFTPDestination`, `BackupStatusReport`, `RestoreCommand`, `RestoreStatusReport`, `BackupTrigger`
- **Modify**: `api/proto/v1/config.proto` — unreserve field 18 on `ClusterConfig` → `BackupConfig backups = 18`; unreserve fields 8-9 on `SatelliteMessage` → `BackupStatusReport`/`RestoreStatusReport`; unreserve field 7 on `CentralMessage` → `RestoreCommand`; add `BackupTrigger` field 20
- Run `make proto`
- **Verify**: `make proto`, `make build`

### Step 11: Extend `buildProtoClusterConfig` for backup
- **Modify**: `internal/central/server/rest.go`
- In `buildProtoClusterConfig()`: if `spec.Backup != nil && spec.Backup.StoreID != nil`, look up the BackupStore, decrypt credentials, build `BackupDestination` proto, map physical/logical/retention config, set `protoConfig.Backups`
- Add helper `buildBackupDestination(store, encryptor)` to map store → proto destination
- Reuse existing `s.encryptor` (already available in RESTServer)
- **Verify**: `make build`, `make test`

### Step 12: Store interface + implementation for inventory and restore operations
- **Modify**: `internal/central/store/store.go` — add `CreateBackupInventory`, `UpdateBackupInventory`, `ListBackupInventory`, `GetBackupInventory`, `CreateRestoreOperation`, `UpdateRestoreOperation`, `ListRestoreOperations`
- **Modify**: `internal/central/store/postgres.go` — implement all
- **Verify**: `make build`, `make test`

---

## Phase 5: Operator Integration + Dashboard Backup/Restore UI (Steps 13-16)

### Step 13: Satellite operator — backup sidecar injection
- **New file**: `internal/satellite/operator/manifest_backup.go` — `backupEnabled()`, naming helpers, `buildBackupCredentialSecret()`, `buildBackupConfigConfigMap()`, `buildBackupSidecar()` container spec
- **Modify**: `internal/satellite/operator/manifest_statefulset.go` — add wal-staging + wal-restore emptyDir volumes and backup sidecar container when `backupEnabled(cfg)`
- **Modify**: `internal/satellite/operator/manifest_secret.go` — add `backup-password` key when backup enabled
- **Modify**: `internal/satellite/operator/manifest_configmap.go` — set `archive_mode=on`, `archive_command`, `archive_timeout`, `summarize_wal=on` when backup enabled
- **Modify**: `internal/satellite/operator/operator.go` — add backup resource creation/deletion in reconcile + HandleDelete
- **Verify**: `make build`, `make test`

### Step 14: gRPC status handlers + REST endpoints for inventory/restore
- **Modify**: `internal/central/server/rest.go` — register routes: `GET /clusters/:id/backups`, `GET /backups/:id`, `POST /clusters/:id/backups/trigger`, `POST /clusters/:id/restore`, `GET /clusters/:id/restores`; implement handlers
- **Modify**: `internal/central/server/grpc.go` — handle `BackupStatusReport` and `RestoreStatusReport` in stream handler; add `PushRestoreCommand()` and `PushBackupTrigger()` to StreamManager
- **Modify**: `dashboard/src/api.js` — add `clusterBackups()`, `triggerBackup()`, `initiateRestore()`, `clusterRestores()`
- **Verify**: `make build`, `make test`

### Step 15: ClusterDetail page — Backups tab
- **Modify**: `dashboard/src/pages/ClusterDetail.jsx`
- Add "Backups" tab alongside Instances/Databases/Events
- Backup inventory table: Type, Status (badge), Started, Duration, Size, Path; sorted newest first
- "Backup Now" button with type dropdown (base/incremental/logical); disabled when backup running
- WAL archiving status indicator
- Empty state when no backups
- **Verify**: `npm run build`

### Step 16: Restore wizard modal
- **Modify**: `dashboard/src/pages/ClusterDetail.jsx`
- 3-step wizard modal: (1) Choose type — logical vs PITR, (2) Configure — target DB or timestamp + mode, (3) Confirm + submit
- In-place PITR shows destructive action warning (red background, explicit confirmation)
- Calls `api.initiateRestore()`, shows progress via WebSocket updates
- "Restore" button in Backups tab header
- **Verify**: `npm run build`

---

## Phase 6: Backup Sidecar Binary (Steps 17-18)

### Step 17: Storage destination interface + implementations
- **New package**: `internal/backup/destination/`
- `destination.go` — `Destination` interface: `Upload`, `Download`, `List`, `Delete`, `Exists`; `New()` factory
- `gcs.go` — GCS implementation (`cloud.google.com/go/storage`)
- `sftp.go` — SFTP implementation (`github.com/pkg/sftp` + `golang.org/x/crypto/ssh`)
- **Verify**: `make build`, unit tests with mock storage

### Step 18: Backup sidecar binary + internal logic
- **New file**: `cmd/backup-sidecar/main.go` — entry point (follow `cmd/failover-sidecar/main.go` pattern)
- **New package**: `internal/backup/` with:
  - `config.go` — env var parsing
  - `sidecar.go` — main lifecycle, role detection via `pg_is_in_recovery()`
  - `scheduler.go` — cron scheduler with health gates (replication lag check)
  - `wal.go` — WAL archiver: watch `/wal-staging`, compress (gzip), upload, delete local
  - `physical.go` — `pg_basebackup` execution (base + incremental)
  - `logical.go` — `pg_dump` execution
  - `retention.go` — periodic cleanup enforcing retention policies
  - `reporter.go` — status reporting (gRPC primary, ConfigMap fallback)
  - `connector.go` — gRPC stream to satellite (receives RestoreCommand/BackupTrigger)
- **Modify**: `Makefile` — add `backup-sidecar` build target
- **New file**: `deploy/docker/Dockerfile.backup-sidecar`
- **Verify**: `make build`, unit tests for scheduler/retention/WAL logic

---

## Key Files Reference

| File | Steps |
|------|-------|
| `internal/central/store/migrations/023_*.sql, 024_*.sql` | 1, 2 |
| `internal/shared/models/models.go` | 3 |
| `internal/central/store/store.go` | 4, 12 |
| `internal/central/store/postgres.go` | 4, 12 |
| `internal/central/server/rest.go` | 5, 9, 11, 14 |
| `internal/central/server/ws.go` | 6 |
| `internal/central/server/grpc.go` | 14 |
| `dashboard/src/api.js` | 6, 14 |
| `dashboard/src/context/DataContext.jsx` | 6 |
| `dashboard/src/pages/Admin.jsx` | 7 |
| `dashboard/src/pages/Profiles.jsx` | 8, 9 |
| `dashboard/src/pages/ClusterDetail.jsx` | 15, 16 |
| `api/proto/v1/backup.proto` (new) | 10 |
| `api/proto/v1/config.proto` | 10 |
| `internal/satellite/operator/manifest_backup.go` (new) | 13 |
| `internal/satellite/operator/manifest_statefulset.go` | 13 |
| `internal/satellite/operator/manifest_configmap.go` | 13 |
| `internal/backup/destination/` (new) | 17 |
| `internal/backup/` (new) | 18 |
| `cmd/backup-sidecar/main.go` (new) | 18 |

## Existing Code to Reuse

- **Encryption**: `internal/central/crypto/crypto.go` — `Encryptor.Encrypt()`/`Decrypt()` for backup store credentials
- **CRUD pattern**: `StorageTier` CRUD in `store.go`/`postgres.go`/`rest.go` — exact pattern for BackupStore
- **Admin tab pattern**: `StorageTiersTab` in `Admin.jsx` — exact pattern for Backup Stores tab
- **Sidecar injection**: Failover sidecar in `manifest_statefulset.go` — conditional container + volumes pattern
- **Secret preservation**: `createOrPreserveSecret` in `reconcile_helpers.go` — backup password persistence
- **gRPC stream handling**: `Connect()` in `grpc.go` — existing oneof dispatch for new message types
- **Proto reserved fields**: Fields 7-9, 18, 20 in `config.proto` — designed for backup

## Verification

After each step: `make build` + `make test` for backend, `npm run build` for frontend. Full end-to-end test after Phase 5: create a backup store in Admin, configure backup in a profile, verify sidecar appears in the pod spec, trigger a backup from ClusterDetail. Full integration test after Phase 6 with a real minikube cluster (`make test-integration`).
