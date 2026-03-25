# Configuration Change Management

## 1. Overview

pg-swarm currently locks cluster profiles once any cluster references them, preventing all configuration changes. This document specifies the requirements for allowing configuration changes to active clusters with appropriate safety mechanisms.

### Goals

1. **Allow changes to configurations once clusters are created** — remove the blanket profile lock.
2. **For safe changes, do a sequential restart** — restart pods one at a time (replicas first, then switchover primary). Cluster stays available throughout.
3. **For replication-sensitive changes, do a full cluster restart** — scale the StatefulSet to 0, update the config, then scale back up. This avoids cluster members having different configurations simultaneously.

### Design Principle

All configuration changes result in a pod restart — there is no `pg_reload_conf()` pathway. This keeps the system simple and predictable. The only decision is **how** the restart happens: sequential (zero downtime) or full shutdown (brief outage).

---

## 2. Change Classification

Every configuration change falls into one of three categories.

### 2.1 Sequential Restart (cluster stays available)

Changes where a brief mismatch between pods is harmless. Pods are restarted one at a time — each picks up the new ConfigMap on startup.

**Covers:**
- Most `pg_params` changes: `work_mem`, `maintenance_work_mem`, `shared_buffers`, `max_connections`, `effective_cache_size`, `checkpoint_timeout`, `log_*` parameters, `autovacuum_*`, `statement_timeout`, etc.
- `hba_rules` changes
- `archive` setting changes (`archive_command`, `archive_timeout`)
- `resources` changes (CPU/memory requests and limits)
- `failover` sidecar configuration changes
- `databases` changes (new databases created on primary during init)
- `deletion_protection` toggle
- `replicas` increase (scale-up only — new pods join, no restart of existing)

### 2.2 Full Cluster Restart (brief outage)

Changes where even a temporary mismatch between primary and replica causes replication failures or data integrity issues. The cluster is fully stopped before any config is applied.

**Covers:**
- `wal_level` — replicas cannot stream from a primary with a different WAL level
- `max_wal_senders` — reducing below active sender count breaks replication
- `max_replication_slots` — reducing below active slot count breaks replication
- `postgres.version` / `postgres.image` — different PG versions cannot replicate
- `replicas` decrease (scale-down) — must coordinate which pods to remove
- `archive_mode` off → on — requires postmaster restart and replicas must match

```go
var fullRestartRequired = map[string]bool{
    "wal_level":            true,
    "max_wal_senders":      true,
    "max_replication_slots": true,
}
```

Pod-level changes (`postgres.image`, `postgres.version`) and replica scale-down also trigger full restart.

### 2.3 Immutable (blocked)

These cannot be changed after the cluster is created due to Kubernetes constraints.

| Field | Reason |
|-------|--------|
| `storage.size` | VolumeClaimTemplates are immutable in StatefulSets |
| `storage.storage_class` | VolumeClaimTemplates are immutable |
| `wal_storage.size` | VolumeClaimTemplates are immutable |
| `wal_storage.storage_class` | VolumeClaimTemplates are immutable |

The API and dashboard must **reject** these changes with a clear error message. The current behavior of silently ignoring VCT changes is replaced with an explicit error.

---

## 3. Sequential Restart Flow

For changes classified as safe for sequential restart, no custom orchestration is needed. The operator updates the ConfigMap and StatefulSet pod template — Kubernetes handles the rolling restart automatically, and the existing failover sidecar handles primary promotion.

### 3.1 Flow

```
1. User submits config change
2. Central classifies changes, confirms with user
3. Central updates ClusterConfig (bumps config_version)
4. Central pushes new config to satellite via gRPC
5. Satellite operator runs normal reconciliation:
   a. Updates ConfigMap (postgresql.conf / pg_hba.conf)
   b. Updates StatefulSet pod template (resources, image, etc.)
6. Kubernetes detects pod template change and performs rolling update:
   a. Deletes pods one at a time (highest ordinal first)
   b. Waits for replacement pod to become Ready before proceeding
   c. Each new pod picks up the updated ConfigMap on startup
7. When the primary pod is restarted:
   a. Failover sidecar on a replica detects primary is gone
   b. Acquires leader lease and promotes itself (existing failover mechanism)
   c. Primary pod restarts as a replica and re-joins the cluster
8. Central receives health update, creates Event, pushes WebSocket update
```

No new operator code is required for sequential restart — it is the existing `createOrUpdateConfigMap()` + `createOrUpdateStatefulSet()` flow that already triggers a K8s rolling update.

### 3.2 Why Sequential is Safe for Most Parameters

During the rolling restart, some pods briefly have the new config while others have the old. This is safe because:

- **`shared_buffers` mismatch**: Each pod manages its own buffer pool. No cross-pod impact.
- **`max_connections` mismatch**: Each pod has its own connection limit. Clients connect to individual pods.
- **`work_mem` mismatch**: Per-session setting. Different pods having different values is functionally identical to different sessions having different `SET work_mem`.
- **HBA rules mismatch**: Each pod evaluates its own `pg_hba.conf` independently.
- **Resource limits**: Kubernetes applies CPU/memory limits per-pod independently.

The only dangerous parameters are those where primary and replica **must agree** for replication to work (section 2.2).

### 3.3 Single-Node Clusters

For clusters with `replicas = 1` (primary only, no replicas):
- K8s deletes and recreates the single pod.
- This causes a brief outage (no replica available for failover).
- The pod restarts with the new config and resumes as primary.

---

## 4. Full Cluster Restart Flow

For replication-sensitive changes (section 2.2), all pods are stopped before any config is applied.

### 4.1 Flow

```
1. User submits config change
2. Central classifies changes, warns user about downtime
3. User confirms
4. Central updates ClusterConfig (bumps config_version)
5. Central pushes new config to satellite via gRPC with restart flag
6. Satellite operator executes:
   a. Fence the primary (prevent new writes, drain active transactions)
   b. Scale StatefulSet replicas to 0
   c. Wait for all pods to terminate
   d. Update ConfigMap (postgresql.conf / pg_hba.conf)
   e. Update StatefulSet pod template (resource/image changes if any)
   f. Scale StatefulSet replicas back to original count
   g. Wait for all pods to become Ready
   h. Verify primary is accepting writes and replicas are streaming
7. Operator reports completion to central
8. Central creates Event, pushes WebSocket update
```

### 4.2 Why Not Rolling for These Parameters

When `wal_level` changes from `replica` to `logical` (or vice versa):
- A replica streaming from a primary with a different `wal_level` may fail to apply WAL records.
- The replica could enter an unrecoverable state requiring a full re-basebackup.

When `postgres.version` changes (e.g., 17 → 18):
- Different PG major versions have incompatible on-disk formats.
- Replication between different major versions is not supported.

These make even a brief mismatch dangerous.

### 4.3 Mixed Changes

If a single config update contains both sequential-safe and full-restart changes:
- The entire change is treated as **full cluster restart**.
- The confirmation dialog shows both categories, making clear that the full restart is caused by the replication-sensitive parameters.

---

## 5. Profile Unlocking

### 5.1 Current Behavior

`UpdateProfile()` in the store layer checks if the profile has any `deployment_rules` or `cluster_configs`. If either exists, the update is rejected: `"profile X is currently in use and cannot be edited"`.

### 5.2 New Behavior

Remove the blanket lock. Allow profile updates even when clusters reference the profile, with these guardrails:

1. **Immutable field validation**: If the update changes immutable fields (storage size/class) AND active clusters exist, reject with a specific error listing the immutable fields.
2. **Change classification**: Compute the diff between old and new config. Classify each changed field as sequential-safe, full-restart, or immutable.
3. **Return change impact**: The API response includes the classification and affected cluster list so the dashboard can present the appropriate confirmation dialog.
4. **Cascade to clusters**: After the user confirms, bump `config_version` on all clusters using this profile and push the new config to their satellites (existing `rePushClustersForProfile()` logic).

### 5.3 Profile Delete Behavior

Profile deletion remains blocked when clusters reference the profile. Deletion is destructive and would leave clusters without a config source. The existing cascade-delete flow (with confirmation) remains unchanged.

---

## 6. Configuration Versioning & Rollback

### 6.1 Version History

Every configuration change creates a new version record in central. Each version stores the **complete configuration snapshot** (not a diff), so any version can be restored independently.

**Database schema:**

```sql
CREATE TABLE config_versions (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    profile_id     UUID NOT NULL REFERENCES cluster_profiles(id) ON DELETE CASCADE,
    version        INT NOT NULL,
    config         JSONB NOT NULL,              -- full ClusterSpec snapshot
    change_summary TEXT DEFAULT '',              -- human-readable description of what changed
    apply_status   TEXT DEFAULT 'pending',       -- pending, applied, failed, reverted
    created_by     TEXT DEFAULT '',              -- user or "system"
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_config_versions_profile_version
    ON config_versions (profile_id, version);
```

### 6.2 Version Creation

A new version is created whenever:
- A profile config is updated via the REST API.
- A revert is performed (the reverted-to config becomes the new latest version).

The version number is a monotonically increasing integer per profile, starting at 1.

```
Profile "production":
  v1: initial creation (replicas=3, shared_buffers=256MB)
  v2: updated shared_buffers to 512MB
  v3: added HBA rule for analytics team
  v4: reverted to v2 (shared_buffers=512MB, no analytics HBA rule)
```

Note: v4 is a **new version** with the same config as v2. The history is append-only — reverting does not delete versions.

### 6.3 Rollback Flow

```
1. User views version history for a profile (list of versions with timestamps and summaries)
2. User selects a version to revert to
3. System computes diff between current config and the selected version
4. System presents the standard change confirmation dialog (sequential vs full restart)
5. User confirms
6. System creates a new version (current version + 1) with the reverted config
7. Apply follows the normal restart flow (sequential or full)
```

Rollback is treated as a regular config change — the only special part is that the new config comes from a historical version rather than user input.

### 6.4 REST API

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/v1/profiles/:id/versions` | List all versions (newest first) |
| `GET` | `/api/v1/profiles/:id/versions/:version` | Get a specific version's full config |
| `POST` | `/api/v1/profiles/:id/revert` | Revert to a specific version |

**List versions response:**
```json
[
  {
    "version": 4,
    "change_summary": "Reverted to version 2",
    "apply_status": "applied",
    "created_by": "admin",
    "created_at": "2026-03-24T15:30:00Z"
  },
  {
    "version": 3,
    "change_summary": "Added HBA rule for analytics team",
    "apply_status": "applied",
    "created_by": "admin",
    "created_at": "2026-03-24T14:00:00Z"
  }
]
```

**Revert request:**
```json
{
  "target_version": 2
}
```

The response follows the same `change_impact` pattern as a normal profile update — the user must confirm via `/apply` if active clusters are affected.

### 6.5 Dashboard UI

- **Profile detail page**: New "Version History" tab showing a timeline of all versions.
- Each version entry shows: version number, timestamp, change summary, apply status.
- **Diff view**: Clicking a version shows a side-by-side diff against the current config.
- **Revert button**: On each historical version, a "Revert to this version" button triggers the rollback flow with the standard confirmation dialog.

---

## 7. Change Detection

### 7.1 Diff Computation

When a config update is submitted, the system computes a structured diff:

```go
type ConfigDiff struct {
    SequentialChanges []ParamChange  // Safe for sequential restart
    FullRestartChanges []ParamChange // Require full cluster restart
    ImmutableErrors   []ParamChange  // Blocked changes
    ScaleUp           *int32         // Replica increase (nil if unchanged)
    ScaleDown         *int32         // Replica decrease (nil if unchanged)
}

type ParamChange struct {
    Path     string // e.g., "pg_params.shared_buffers", "resources.cpu_request"
    OldValue string
    NewValue string
}
```

### 7.2 Classification Logic

```
For each changed field:
  pg_params key in fullRestartRequired set → FullRestartChanges
  pg_params key NOT in set              → SequentialChanges
  hba_rules                             → SequentialChanges
  archive.*                             → SequentialChanges (except archive_mode off→on → FullRestart)
  resources.*                           → SequentialChanges
  postgres.version / postgres.image     → FullRestartChanges
  failover.*                            → SequentialChanges
  databases                             → SequentialChanges
  deletion_protection                   → SequentialChanges (no restart, PVC-level only)
  storage.* / wal_storage.*             → ImmutableErrors
  replicas increase                     → ScaleUp (no restart)
  replicas decrease                     → ScaleDown (full restart — must choose which pods to remove)
```

### 7.3 Apply Strategy

```
If ImmutableErrors is non-empty → reject the entire update
If FullRestartChanges is non-empty OR ScaleDown → full cluster restart
If only SequentialChanges → sequential restart
If only ScaleUp → scale StatefulSet directly (no restart)
If only deletion_protection → patch PVCs directly (no restart)
```

---

## 8. REST API Changes

### 8.1 Profile Update (Modified)

`PUT /api/v1/profiles/:id`

**Current**: Rejects if profile has active clusters.
**New**: Accepts the update. If active clusters exist, the response includes a `change_impact`:

```json
{
  "id": "...",
  "name": "production",
  "config": { ... },
  "change_impact": {
    "affected_clusters": ["orders-db", "payments-db"],
    "sequential_changes": [
      {"path": "pg_params.work_mem", "old": "64MB", "new": "128MB"}
    ],
    "full_restart_changes": [],
    "immutable_errors": [],
    "apply_strategy": "sequential_restart",
    "requires_confirmation": true
  }
}
```

If `immutable_errors` is non-empty, the update is rejected with `400 Bad Request`.

If no active clusters reference the profile, `change_impact` is null — the profile is saved immediately with no restart needed.

### 8.2 Apply Confirmation

`POST /api/v1/profiles/:id/apply`

After reviewing the `change_impact`, the user confirms. This triggers the config push and restart.

```json
{ "confirmed": true }
```

Response:
```json
{
  "operation_id": "...",
  "apply_strategy": "sequential_restart",
  "affected_clusters": ["orders-db", "payments-db"],
  "status": "in_progress"
}
```

### 8.3 Cluster Config Update (Modified)

`PUT /api/v1/clusters/:id` — same pattern: returns `change_impact`, requires `/apply`.

`POST /api/v1/clusters/:id/apply`

### 8.4 Operation Status

`GET /api/v1/operations/:id`

Returns step-by-step progress. For sequential restart: per-pod status. For full restart: fence → scale down → update → scale up → verify.

---

## 9. Operator Changes

### 9.1 Config Change Detection

The satellite operator, upon receiving a new `ClusterConfig`:

1. Compare new config against the last applied config.
2. Classify the changes using the logic in section 7.
3. Choose apply strategy:
   - **Sequential restart**: Normal reconciliation — update ConfigMap + StatefulSet template. K8s rolling update + failover sidecar handle the rest. No new code needed.
   - **Full restart**: Fence → scale to 0 → update ConfigMap + StatefulSet → scale back. Requires explicit orchestration.
   - **Scale only**: Update StatefulSet replicas directly.
   - **PVC only**: Patch PVC finalizers.

### 9.2 Sequential Restart (No Custom Code)

For sequential restart, the operator runs its existing reconciliation:

```go
// Existing flow — no changes needed
createOrUpdateConfigMap(ctx, o.client, buildConfigMap(cfg))
createOrUpdateStatefulSet(ctx, o.client, buildStatefulSet(cfg, ...))
// K8s rolling update kicks in automatically when pod template changes.
// Failover sidecar handles promotion when primary pod is restarted.
```

This is the same code path the operator already uses. The only new behavior is that it is now allowed to run on config changes (previously blocked by profile locking).

### 9.3 Full Restart Orchestration

```go
func (o *Operator) fullRestart(ctx context.Context, cfg *pgswarmv1.ClusterConfig) error {
    // 1. Fence primary
    o.sidecarManager.SendFenceCmd(primaryPod)

    // 2. Scale to 0
    o.scaleStatefulSet(ctx, cfg, 0)
    o.waitForPodsTerminated(ctx, cfg)

    // 3. Update ConfigMap and StatefulSet
    createOrUpdateConfigMap(ctx, o.client, buildConfigMap(cfg))
    createOrUpdateStatefulSet(ctx, o.client, buildStatefulSet(cfg, ...))

    // 4. Scale back
    o.scaleStatefulSet(ctx, cfg, cfg.Replicas)
    o.waitForPodsReady(ctx, cfg)

    // 5. Verify
    o.verifyClusterHealth(ctx, cfg)

    return nil
}
```

---

## 10. Dashboard UX

### 10.1 Profile Edit Form

- Remove the "locked" state. All fields are editable when a profile has active clusters.
- Immutable fields (storage size/class) are shown as **disabled** with a tooltip: "Cannot be changed after cluster creation."

### 10.2 Change Confirmation Dialog

When the user saves a profile with active clusters, a confirmation dialog appears:

**Sequential restart (no downtime):**
```
┌─────────────────────────────────────────────────────────────┐
│  Apply Configuration Changes                                │
│                                                             │
│  Affected clusters: orders-db, payments-db                  │
│                                                             │
│  Changes:                                                   │
│    • shared_buffers: 256MB → 512MB                          │
│    • work_mem: 64MB → 128MB                                 │
│                                                             │
│  Pods will be restarted one at a time.                      │
│  The cluster remains available throughout.                  │
│                                                             │
│               [ Cancel ]    [ Apply Changes ]               │
└─────────────────────────────────────────────────────────────┘
```

**Full cluster restart (downtime):**
```
┌─────────────────────────────────────────────────────────────┐
│  ⚠️  Apply Configuration Changes                            │
│                                                             │
│  Affected clusters: orders-db, payments-db                  │
│                                                             │
│  Changes:                                                   │
│    • wal_level: replica → logical                           │
│    • work_mem: 64MB → 128MB                                 │
│                                                             │
│  ⚠️ Changing wal_level requires a FULL CLUSTER RESTART.     │
│  All connections will be dropped. The cluster will be       │
│  unavailable during the restart.                            │
│                                                             │
│               [ Cancel ]    [ Apply Changes ]               │
└─────────────────────────────────────────────────────────────┘
```

### 10.3 Operation Progress

After confirmation, the dashboard shows a progress view (via WebSocket):

**Sequential restart:**
- Pod `orders-db-2`: Restarting → Ready
- Pod `orders-db-1`: Restarting → Ready
- Switchover: `orders-db-0` → `orders-db-1` (promoting)
- Pod `orders-db-0`: Restarting → Ready
- Complete

**Full restart:**
- Fencing primary...
- Scaling down (3 → 0)...
- Updating configuration...
- Scaling up (0 → 3)...
- Verifying cluster health...
- Complete

---

## 11. Edge Cases

### 11.1 Cluster Not Connected

If a satellite is disconnected when apply is triggered:
- The config is stored with the new `config_version`.
- When the satellite reconnects, it receives the latest config and applies it.
- The operator determines the restart strategy at apply time.

### 11.2 Apply During Switchover

If a config apply is triggered while a switchover is in progress:
- Rejected with: "Cannot apply config changes during an active operation."

### 11.3 Sequential Restart Failure

If a pod fails to become Ready during sequential restart:
- The operation pauses at the failing pod.
- Remaining pods still have the old config.
- Reported as **partially applied** — user can retry or roll back.
- The primary was not yet restarted (replicas go first), so the cluster is still serving traffic.

### 11.4 Full Restart Failure

If the cluster fails to come back after a full restart:
- Pods may be in CrashLoopBackOff due to invalid config.
- Reported as **failed** with step details.
- User submits a corrective config change; operator re-attempts reconciliation.

### 11.5 Concurrent Config Changes

Only one config change operation can be active per cluster at a time. A second change during an in-progress operation is rejected with: "A configuration change is already in progress for this cluster."

---

## 12. Summary

| Scenario | Restart Mode | Downtime |
|----------|-------------|----------|
| Change `shared_buffers`, `work_mem`, `max_connections` | Sequential | None |
| Change HBA rules, `log_*` params, `autovacuum_*` | Sequential | None |
| Change CPU/memory resources | Sequential | None |
| Change `failover` config | Sequential | None |
| Change `archive_command`, `archive_timeout` | Sequential | None |
| Change `wal_level`, `max_wal_senders` | Full | Brief outage |
| Change PostgreSQL version/image | Full | Brief outage |
| Scale down replicas | Full | Brief outage |
| Scale up replicas | Scale only | None |
| Change `deletion_protection` | PVC patch | None |
| Change storage size/class | **Rejected** | N/A |
| Mixed safe + replication-sensitive | Full | Brief outage |

---

## 13. Detailed Design

This section specifies the concrete implementation: store layer changes, REST API modifications, change detection logic, operator integration, protobuf additions, database migration, and dashboard updates. All designs follow existing codebase patterns.

### 13.1 Store Layer Changes (`internal/central/store/`)

#### 13.1.1 Remove Profile Lock from UpdateProfile

**File:** `postgres.go` — `UpdateProfile()` (currently lines 740-769)

Remove the `inUse` check that blocks updates when clusters reference the profile:

```go
// REMOVE this block:
var inUse bool
err := s.pool.QueryRow(ctx, `
    SELECT EXISTS(SELECT 1 FROM deployment_rules WHERE profile_id = $1) OR
           EXISTS(SELECT 1 FROM cluster_configs WHERE profile_id = $1)`, p.ID).Scan(&inUse)
if inUse {
    return fmt.Errorf("profile %s is currently in use and cannot be edited", p.ID)
}
```

The update proceeds unconditionally. Immutable field validation moves to the REST handler (section 13.2).

`DeleteProfile()` keeps its lock check — deletion while in use is still blocked.

#### 13.1.2 Config Versions Store Methods

Add to `store.go` interface:

```go
// Config Versions
CreateConfigVersion(ctx context.Context, v *models.ConfigVersion) error
ListConfigVersions(ctx context.Context, profileID uuid.UUID) ([]*models.ConfigVersion, error)
GetConfigVersion(ctx context.Context, profileID uuid.UUID, version int) (*models.ConfigVersion, error)
```

Implementation in `postgres.go`:

```go
func (s *PostgresStore) CreateConfigVersion(ctx context.Context, v *models.ConfigVersion) error {
    // SELECT COALESCE(MAX(version), 0) + 1 FROM config_versions WHERE profile_id = $1
    // INSERT with computed version number
}

func (s *PostgresStore) ListConfigVersions(ctx context.Context, profileID uuid.UUID) ([]*models.ConfigVersion, error) {
    // SELECT ... ORDER BY version DESC
}

func (s *PostgresStore) GetConfigVersion(ctx context.Context, profileID uuid.UUID, version int) (*models.ConfigVersion, error) {
    // SELECT ... WHERE profile_id = $1 AND version = $2
}
```

#### 13.1.3 Config Version Model (`internal/shared/models/models.go`)

```go
type ConfigVersion struct {
    ID            uuid.UUID       `json:"id" db:"id"`
    ProfileID     uuid.UUID       `json:"profile_id" db:"profile_id"`
    Version       int             `json:"version" db:"version"`
    Config        json.RawMessage `json:"config" db:"config"`
    ChangeSummary string          `json:"change_summary" db:"change_summary"`
    ApplyStatus   string          `json:"apply_status" db:"apply_status"`
    CreatedBy     string          `json:"created_by" db:"created_by"`
    CreatedAt     time.Time       `json:"created_at" db:"created_at"`
}
```

### 13.2 REST API Changes (`internal/central/server/rest.go`)

#### 13.2.1 Modified `updateProfile()` Handler

The handler is restructured into two phases: preview (save + classify) and apply (push to satellites).

```go
func (s *RESTServer) updateProfile(c *fiber.Ctx) error {
    id, _ := uuid.Parse(c.Params("id"))

    // 1. Parse and validate new config (same as before)
    var profile models.ClusterProfile
    if err := c.BodyParser(&profile); err != nil { ... }
    spec, err := profile.ParseSpec()
    // ValidateArchiveSpec, ValidateDatabases, validatePostgresImage...

    // 2. Load current config for diff
    existing, err := s.store.GetProfile(c.Context(), id)
    oldSpec, _ := existing.ParseSpec()

    // 3. Compute diff and classify
    diff := classifyChanges(oldSpec, spec)

    // 4. Check immutable fields if active clusters exist
    clusters, _ := s.store.GetClusterConfigsByProfile(c.Context(), id)
    if len(clusters) > 0 && len(diff.ImmutableErrors) > 0 {
        return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
            "error": "immutable fields cannot be changed",
            "immutable_errors": diff.ImmutableErrors,
        })
    }

    // 5. Save profile (no lock check)
    profile.ID = id
    if err := s.store.UpdateProfile(c.Context(), &profile); err != nil { ... }

    // 6. Create config version
    s.store.CreateConfigVersion(c.Context(), &models.ConfigVersion{
        ProfileID:     id,
        Config:        profile.Config,
        ChangeSummary: diff.Summary(),
        ApplyStatus:   "pending",
    })

    // 7. Return with change_impact if clusters are affected
    if len(clusters) > 0 {
        return c.JSON(fiber.Map{
            "profile":       profile,
            "change_impact": buildChangeImpact(diff, clusters),
        })
    }
    return c.JSON(profile)
}
```

#### 13.2.2 New `applyProfile()` Handler

```go
func (s *RESTServer) applyProfile(c *fiber.Ctx) error {
    id, _ := uuid.Parse(c.Params("id"))

    var body struct {
        Confirmed bool `json:"confirmed"`
    }
    if err := c.BodyParser(&body); err != nil || !body.Confirmed {
        return fiber.NewError(fiber.StatusBadRequest, "confirmation required")
    }

    // Check no active operation on any affected cluster
    clusters, _ := s.store.GetClusterConfigsByProfile(c.Context(), id)
    for _, cfg := range clusters {
        if s.opsTracker.HasActiveOp(cfg.Name) {
            return fiber.NewError(fiber.StatusConflict,
                fmt.Sprintf("cluster %s has an active operation", cfg.Name))
        }
    }

    // Push to all clusters (bumps config_version, pushes to satellites)
    s.rePushClustersForProfile(c.Context(), id)

    return c.JSON(fiber.Map{
        "status":            "in_progress",
        "affected_clusters": clusterNames(clusters),
    })
}
```

#### 13.2.3 New Version History Handlers

```go
// Route registration:
api.Get("/profiles/:id/versions", s.listProfileVersions)
api.Get("/profiles/:id/versions/:version", s.getProfileVersion)
api.Post("/profiles/:id/revert", s.revertProfile)

func (s *RESTServer) listProfileVersions(c *fiber.Ctx) error {
    id, _ := uuid.Parse(c.Params("id"))
    versions, err := s.store.ListConfigVersions(c.Context(), id)
    return c.JSON(versions)
}

func (s *RESTServer) getProfileVersion(c *fiber.Ctx) error {
    id, _ := uuid.Parse(c.Params("id"))
    version, _ := strconv.Atoi(c.Params("version"))
    v, err := s.store.GetConfigVersion(c.Context(), id, version)
    return c.JSON(v)
}

func (s *RESTServer) revertProfile(c *fiber.Ctx) error {
    id, _ := uuid.Parse(c.Params("id"))
    var body struct {
        TargetVersion int `json:"target_version"`
    }
    c.BodyParser(&body)

    // Load target version's config
    v, err := s.store.GetConfigVersion(c.Context(), id, body.TargetVersion)

    // Load current profile
    existing, _ := s.store.GetProfile(c.Context(), id)

    // Compute diff (revert = current → target version's config)
    oldSpec, _ := existing.ParseSpec()
    var newSpec models.ClusterSpec
    json.Unmarshal(v.Config, &newSpec)
    diff := classifyChanges(oldSpec, &newSpec)

    // Update profile with reverted config
    existing.Config = v.Config
    s.store.UpdateProfile(c.Context(), existing)

    // Create new version entry
    s.store.CreateConfigVersion(c.Context(), &models.ConfigVersion{
        ProfileID:     id,
        Config:        v.Config,
        ChangeSummary: fmt.Sprintf("Reverted to version %d", body.TargetVersion),
        ApplyStatus:   "pending",
    })

    // Return change_impact (same as updateProfile)
    clusters, _ := s.store.GetClusterConfigsByProfile(c.Context(), id)
    return c.JSON(fiber.Map{
        "profile":       existing,
        "change_impact": buildChangeImpact(diff, clusters),
    })
}
```

#### 13.2.4 Route Registration

```go
// Existing
api.Put("/profiles/:id", s.updateProfile)

// New
api.Post("/profiles/:id/apply", s.applyProfile)
api.Get("/profiles/:id/versions", s.listProfileVersions)
api.Get("/profiles/:id/versions/:version", s.getProfileVersion)
api.Post("/profiles/:id/revert", s.revertProfile)

// Cluster-level apply
api.Post("/clusters/:id/apply", s.applyCluster)
```

### 13.3 Change Classification Engine (`internal/central/server/config_diff.go` — new file)

```go
package server

import "github.com/pg-swarm/pg-swarm/internal/shared/models"

// Postmaster-context parameters that require full restart.
var fullRestartParams = map[string]bool{
    "shared_buffers":              true,
    "max_connections":             true,
    "wal_level":                   true,
    "max_wal_senders":             true,
    "max_replication_slots":       true,
    "max_worker_processes":        true,
    "max_parallel_workers":        true,
    "shared_preload_libraries":    true,
    "max_locks_per_transaction":   true,
    "max_prepared_transactions":   true,
    "archive_mode":                true,
    "hot_standby":                 true,
    "wal_log_hints":               true,
    "huge_pages":                  true,
    "ssl":                         true,
    "track_commit_timestamp":      true,
    "superuser_reserved_connections": true,
}

type ConfigDiff struct {
    SequentialChanges  []ParamChange
    FullRestartChanges []ParamChange
    ImmutableErrors    []ParamChange
    ScaleUp            *int32
    ScaleDown          *int32
}

type ParamChange struct {
    Path     string `json:"path"`
    OldValue string `json:"old_value"`
    NewValue string `json:"new_value"`
}

// classifyChanges compares old and new ClusterSpec and classifies each difference.
func classifyChanges(old, new *models.ClusterSpec) *ConfigDiff {
    diff := &ConfigDiff{}

    // Compare pg_params
    allKeys := mergeKeys(old.PgParams, new.PgParams)
    for _, key := range allKeys {
        oldVal, newVal := old.PgParams[key], new.PgParams[key]
        if oldVal == newVal { continue }
        change := ParamChange{
            Path: "pg_params." + key, OldValue: oldVal, NewValue: newVal,
        }
        if fullRestartParams[key] {
            diff.FullRestartChanges = append(diff.FullRestartChanges, change)
        } else {
            diff.SequentialChanges = append(diff.SequentialChanges, change)
        }
    }

    // Compare storage (immutable)
    if old.Storage.Size != new.Storage.Size {
        diff.ImmutableErrors = append(diff.ImmutableErrors, ParamChange{
            Path: "storage.size", OldValue: old.Storage.Size, NewValue: new.Storage.Size,
        })
    }
    // ... same for storage_class, wal_storage

    // Compare resources (sequential)
    // Compare postgres version/image (full restart)
    // Compare hba_rules (sequential)
    // Compare replicas (scale up/down)
    // Compare failover, archive, databases, deletion_protection (sequential)

    return diff
}

// ApplyStrategy returns the restart mode needed for this diff.
func (d *ConfigDiff) ApplyStrategy() string {
    if len(d.ImmutableErrors) > 0 { return "rejected" }
    if len(d.FullRestartChanges) > 0 { return "full_restart" }
    if d.ScaleDown != nil { return "full_restart" }
    if len(d.SequentialChanges) > 0 { return "sequential_restart" }
    if d.ScaleUp != nil { return "scale_only" }
    return "no_change"
}

// Summary returns a human-readable description of all changes.
func (d *ConfigDiff) Summary() string { ... }
```

#### 13.3.1 Change Impact Response Builder

```go
func buildChangeImpact(diff *ConfigDiff, clusters []*models.ClusterConfig) fiber.Map {
    names := make([]string, len(clusters))
    for i, c := range clusters { names[i] = c.Name }

    return fiber.Map{
        "affected_clusters":    names,
        "sequential_changes":   diff.SequentialChanges,
        "full_restart_changes": diff.FullRestartChanges,
        "immutable_errors":     diff.ImmutableErrors,
        "apply_strategy":       diff.ApplyStrategy(),
        "requires_confirmation": diff.ApplyStrategy() != "no_change",
    }
}
```

### 13.4 Operator Changes (`internal/satellite/operator/`)

#### 13.4.1 Full Restart Detection (`operator.go`)

The operator needs to detect when a full restart is required vs normal reconciliation. The diff classification runs on the satellite side by comparing the incoming config against the last applied config.

```go
func (o *Operator) HandleConfig(cfg *pgswarmv1.ClusterConfig) error {
    key := cfg.Namespace + "/" + cfg.ClusterName

    o.mu.Lock()
    appliedVersion := o.applied[key]
    previousConfig := o.desired[key]  // store previous config for diffing
    o.mu.Unlock()

    if appliedVersion >= cfg.ConfigVersion {
        return nil  // idempotent
    }

    // Determine if full restart is needed
    if previousConfig != nil && requiresFullRestart(previousConfig, cfg) {
        return o.fullRestart(ctx, cfg)
    }

    // Normal reconciliation (triggers K8s rolling update if pod template changed)
    return o.reconcile(cfg)
}
```

#### 13.4.2 Full Restart Check

```go
// requiresFullRestart compares old and new ClusterConfig proto messages
// and returns true if any replication-sensitive parameter changed.
func requiresFullRestart(old, new *pgswarmv1.ClusterConfig) bool {
    // Check postgres version/image change
    if old.Postgres.Version != new.Postgres.Version ||
       old.Postgres.Image != new.Postgres.Image {
        return true
    }

    // Check scale down
    if new.Replicas < old.Replicas {
        return true
    }

    // Check replication-sensitive pg_params
    for _, key := range []string{"wal_level", "max_wal_senders", "max_replication_slots"} {
        if old.PgParams[key] != new.PgParams[key] {
            return true
        }
    }

    return false
}
```

#### 13.4.3 Full Restart Orchestration

```go
func (o *Operator) fullRestart(ctx context.Context, cfg *pgswarmv1.ClusterConfig) error {
    log.Info().Str("cluster", cfg.ClusterName).Msg("full cluster restart required")

    // 1. Fence primary (prevent new writes)
    if failoverEnabled(cfg) {
        pods, _ := listClusterPods(ctx, o.client, cfg.Namespace, cfg.ClusterName)
        for _, pod := range pods {
            if pod.Labels[LabelRole] == RolePrimary {
                o.sidecarManager.SendCommand(pod.Name, &pgswarmv1.SidecarCommand{
                    Cmd: &pgswarmv1.SidecarCommand_Fence{Fence: &pgswarmv1.FenceCmd{
                        DrainTimeoutSeconds: 5,
                    }},
                })
                break
            }
        }
    }

    // 2. Scale to 0
    sts, err := o.client.AppsV1().StatefulSets(cfg.Namespace).Get(ctx, cfg.ClusterName, metav1.GetOptions{})
    if err != nil { return err }
    zero := int32(0)
    sts.Spec.Replicas = &zero
    o.client.AppsV1().StatefulSets(cfg.Namespace).Update(ctx, sts, metav1.UpdateOptions{})

    // 3. Wait for all pods to terminate
    waitForPods(ctx, o.client, cfg.Namespace, cfg.ClusterName, 0, 5*time.Minute)

    // 4. Normal reconcile (updates ConfigMap, StatefulSet template, sets replicas back)
    if err := o.reconcile(cfg); err != nil {
        return fmt.Errorf("reconcile after scale-down: %w", err)
    }

    // 5. Wait for pods to become Ready
    waitForPods(ctx, o.client, cfg.Namespace, cfg.ClusterName, cfg.Replicas, 10*time.Minute)

    log.Info().Str("cluster", cfg.ClusterName).Msg("full cluster restart completed")
    return nil
}
```

#### 13.4.4 Immutable Field Guard in Reconcile

In `createOrUpdateStatefulSet()` (`reconcile_helpers.go`), replace the current silent warning with an error log + skip pattern:

```go
// Current: log.Warn and silently ignore
// New: log.Error with explicit message
for i, desired := range desiredVCTs {
    if i < len(existingVCTs) {
        if desired.Spec.Resources.Requests[corev1.ResourceStorage] !=
           existingVCTs[i].Spec.Resources.Requests[corev1.ResourceStorage] {
            log.Error().
                Str("cluster", existing.Name).
                Str("vct", desired.Name).
                Msg("storage size change rejected — VolumeClaimTemplates are immutable in K8s")
        }
    }
}
```

### 13.5 Protobuf Changes (`api/proto/v1/config.proto`)

No new proto messages are needed for config changes. The existing `ClusterConfig` message with `config_version` field is sufficient:

- Sequential restart: operator receives new config, runs normal reconciliation, K8s rolling update handles restart.
- Full restart: operator receives new config, detects replication-sensitive changes, runs full restart sequence.

The apply strategy decision is made on the satellite side by comparing incoming config with previously applied config.

### 13.6 Database Migration (`internal/central/store/migrations/018_config_versions.sql`)

```sql
-- Config version history for profiles.
-- Each version stores the complete ClusterSpec snapshot.
CREATE TABLE config_versions (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    profile_id     UUID NOT NULL REFERENCES cluster_profiles(id) ON DELETE CASCADE,
    version        INT NOT NULL,
    config         JSONB NOT NULL,
    change_summary TEXT NOT NULL DEFAULT '',
    apply_status   TEXT NOT NULL DEFAULT 'pending'
                   CHECK (apply_status IN ('pending', 'applied', 'failed', 'reverted')),
    created_by     TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_config_versions_profile_version
    ON config_versions (profile_id, version);

-- Remove the locked column from cluster_profiles (no longer used).
ALTER TABLE cluster_profiles DROP COLUMN IF EXISTS locked;
```

### 13.7 Dashboard Changes (`web/dashboard/src/pages/Profiles.jsx`)

#### 13.7.1 Remove Lock State

Remove all locking UI logic:
- Remove the `locked` badge (`In Use (Locked)` / `Editable`).
- Replace the View/Edit button toggle with always showing **Edit**.
- Remove `cascadePreview` calls on edit attempts.

#### 13.7.2 Immutable Field Handling

When editing a profile that has active clusters:
- Storage size/class fields are rendered as **disabled** with tooltip: "Cannot be changed after cluster creation."
- Detected via: `GET /api/v1/profiles/:id` response includes cluster count (or separate check).

#### 13.7.3 Change Confirmation Dialog

On save, if the response includes `change_impact`:

```jsx
function ChangeConfirmDialog({ impact, onConfirm, onCancel }) {
    const isFullRestart = impact.apply_strategy === 'full_restart';
    return (
        <Modal>
            <h3>{isFullRestart ? '⚠️ Full Cluster Restart Required' : 'Apply Configuration Changes'}</h3>
            <p>Affected clusters: {impact.affected_clusters.join(', ')}</p>

            {impact.sequential_changes.length > 0 && (
                <section>
                    <h4>Changes (applied via rolling restart):</h4>
                    {impact.sequential_changes.map(c =>
                        <div>{c.path}: {c.old_value} → {c.new_value}</div>
                    )}
                </section>
            )}

            {impact.full_restart_changes.length > 0 && (
                <section>
                    <h4>⚠️ Replication-sensitive changes (full shutdown):</h4>
                    {impact.full_restart_changes.map(c =>
                        <div>{c.path}: {c.old_value} → {c.new_value}</div>
                    )}
                    <p className="warning">All connections will be dropped during restart.</p>
                </section>
            )}

            <div className="actions">
                <button onClick={onCancel}>Cancel</button>
                <button className={isFullRestart ? 'btn-danger' : 'btn-primary'}
                        onClick={() => onConfirm()}>
                    Apply Changes
                </button>
            </div>
        </Modal>
    );
}
```

On confirm, call `POST /api/v1/profiles/:id/apply`.

#### 13.7.4 Version History Tab

New tab in the profile detail/edit view:

```jsx
function VersionHistoryTab({ profileId }) {
    const [versions, setVersions] = useState([]);

    useEffect(() => {
        api.profileVersions(profileId).then(setVersions);
    }, [profileId]);

    return (
        <table>
            <thead>
                <tr><th>Version</th><th>Summary</th><th>Status</th><th>Date</th><th></th></tr>
            </thead>
            <tbody>
                {versions.map(v => (
                    <tr key={v.version}>
                        <td>v{v.version}</td>
                        <td>{v.change_summary}</td>
                        <td><StatusBadge status={v.apply_status} /></td>
                        <td>{formatDate(v.created_at)}</td>
                        <td>
                            <button onClick={() => handleRevert(v.version)}>
                                Revert to this version
                            </button>
                        </td>
                    </tr>
                ))}
            </tbody>
        </table>
    );
}
```

#### 13.7.5 API Client Additions (`web/dashboard/src/api.js`)

```js
// Config versions
profileVersions: (id) => get(`/profiles/${id}/versions`),
profileVersion: (id, version) => get(`/profiles/${id}/versions/${version}`),
revertProfile: (id, targetVersion) => post(`/profiles/${id}/revert`, { target_version: targetVersion }),
applyProfile: (id) => post(`/profiles/${id}/apply`, { confirmed: true }),
applyCluster: (id) => post(`/clusters/${id}/apply`, { confirmed: true }),
```

### 13.8 Files Modified Summary

| File | Change |
|------|--------|
| `internal/central/store/postgres.go` | Remove lock check from `UpdateProfile()`; add `CreateConfigVersion`, `ListConfigVersions`, `GetConfigVersion` |
| `internal/central/store/store.go` | Add ConfigVersion interface methods |
| `internal/shared/models/models.go` | Add `ConfigVersion` struct |
| `internal/central/server/rest.go` | Modify `updateProfile()`; add `applyProfile`, version history handlers, route registration |
| `internal/central/server/config_diff.go` | **New file** — `classifyChanges()`, `ConfigDiff`, `ParamChange`, `fullRestartParams` registry |
| `internal/satellite/operator/operator.go` | Add `requiresFullRestart()` check in `HandleConfig()`; add `fullRestart()` method |
| `internal/satellite/operator/reconcile_helpers.go` | Change VCT warning from silent to explicit error log |
| `internal/central/store/migrations/018_config_versions.sql` | **New file** — `config_versions` table, drop `locked` column |
| `api/proto/v1/config.proto` | No changes needed |
| `web/dashboard/src/pages/Profiles.jsx` | Remove locking UI; add confirmation dialog, version history tab |
| `web/dashboard/src/api.js` | Add version/apply/revert API functions |
| `web/dashboard/mock/data.js` | Add mock config versions |
| `web/dashboard/mock/plugin.js` | Add mock version/apply/revert routes |

---

## 14. Implementation Phases

### Phase 1: Foundation — Models, Migration, Store Layer

**Goal:** Database schema, Go types, and store CRUD — no behavior change yet.

**Steps:**
1. Add `ConfigVersion` struct to `internal/shared/models/models.go`.
2. Create migration `internal/central/store/migrations/018_config_versions.sql`:
   - `config_versions` table.
   - Drop `locked` column from `cluster_profiles`.
3. Add store interface methods to `internal/central/store/store.go`:
   - `CreateConfigVersion`, `ListConfigVersions`, `GetConfigVersion`.
4. Implement in `internal/central/store/postgres.go`:
   - Config version CRUD with auto-incrementing version number.

**Verification:** `make build && make test` — all existing tests pass, no behavior change.

---

### Phase 2: Change Classification Engine

**Goal:** Diff computation and classification logic, isolated and testable.

**Steps:**
1. Create `internal/central/server/config_diff.go`:
   - `fullRestartParams` registry (hardcoded set of postmaster-context parameters).
   - `ConfigDiff` and `ParamChange` types.
   - `classifyChanges(old, new *models.ClusterSpec) *ConfigDiff` function.
   - `ApplyStrategy()` method.
   - `Summary()` method for human-readable change descriptions.
   - `buildChangeImpact()` helper for REST response.
2. Write unit tests in `internal/central/server/config_diff_test.go`:
   - Test each category: dynamic params, restart params, immutable fields, scale up/down, mixed changes.
   - Test `ApplyStrategy()` returns correct mode for each combination.

**Verification:** `make test` — new tests pass. No behavior change to running system.

---

### Phase 3: Profile Unlocking & Version History (Central)

**Goal:** Remove profile lock, add version history, expose via REST API.

**Steps:**
1. Modify `UpdateProfile()` in `postgres.go`: remove the `inUse` check.
2. Modify `updateProfile()` handler in `rest.go`:
   - Load old config before saving for diff computation.
   - After save, create a `ConfigVersion` record.
   - If active clusters exist, compute `change_impact` and include in response.
   - Add immutable field validation (reject if immutable fields changed and clusters exist).
3. Add new handlers:
   - `applyProfile()` — confirmation endpoint, calls `rePushClustersForProfile()`.
   - `listProfileVersions()`, `getProfileVersion()`, `revertProfile()`.
4. Register new routes in `setupRoutes()`.
5. Apply same pattern to `updateClusterConfig()` — add `change_impact` and `applyCluster()`.

**Verification:**
- `make build && make test`.
- Manual test: update a profile with active clusters → verify `change_impact` in response.
- Manual test: call `/apply` → verify config pushed to satellite.
- Manual test: list versions → verify history.
- Manual test: revert to older version → verify config restored.

---

### Phase 4: Operator — Full Restart Orchestration

**Goal:** Satellite operator detects replication-sensitive changes and performs full cluster restart.

**Steps:**
1. In `operator.go` `HandleConfig()`:
   - Store previous config in `o.desired` map before overwriting.
   - Add `requiresFullRestart(old, new)` check.
   - If full restart needed, call `o.fullRestart(ctx, cfg)` instead of `o.reconcile(cfg)`.
2. Implement `fullRestart()`:
   - Fence primary via sidecar command (if failover enabled).
   - Scale StatefulSet to 0, wait for termination.
   - Run normal `reconcile()` (updates ConfigMap, StatefulSet template, sets replicas back).
   - Wait for pods to become Ready.
3. In `reconcile_helpers.go`:
   - Upgrade VCT mismatch warning from `log.Warn` to `log.Error` with explicit message.

**Verification:**
- `make build && make test`.
- Integration test (if asked): change `wal_level` on a profile → verify cluster does full restart (scale to 0 → back up).
- Integration test: change `work_mem` → verify normal rolling restart (no scale-down).

---

### Phase 5: Dashboard — Unlock Profiles & Confirmation Dialog

**Goal:** Users can edit profiles with active clusters, see change impact, and confirm.

**Steps:**
1. **Remove locking UI** from `Profiles.jsx`:
   - Remove `In Use (Locked)` / `Editable` badges.
   - Always show **Edit** button (never View-only).
   - Remove `cascadePreview` calls on edit attempts.
2. **Disable immutable fields**: when editing a profile with active clusters, disable storage size/class inputs with tooltip.
3. **Add confirmation dialog** (`ChangeConfirmDialog` component):
   - Triggered when `updateProfile` response includes `change_impact`.
   - Shows sequential vs full restart changes.
   - Full restart shows warning styling.
   - On confirm, calls `POST /profiles/:id/apply`.
4. **Add API client functions** in `api.js`:
   - `applyProfile()`, `profileVersions()`, `profileVersion()`, `revertProfile()`.

**Verification:** Manual test in dashboard:
- Edit a profile with active clusters → see confirmation dialog.
- Dynamic-only change → dialog shows "rolling restart, no downtime".
- Restart-required change → dialog shows warning with red styling.
- Confirm → clusters restart.

---

### Phase 6: Dashboard — Version History & Revert

**Goal:** Users can view config history and revert to previous versions.

**Steps:**
1. Add **Version History tab** to profile detail/edit view.
2. Implement version list (table with version, summary, status, date, revert button).
3. Implement **diff view**: clicking a version shows side-by-side comparison with current config.
4. Implement **revert flow**: revert button → calls API → shows confirmation dialog (same as phase 5) → applies.
5. Add mock data and routes for development:
   - `mock/data.js` — sample config versions.
   - `mock/plugin.js` — mock version/apply/revert endpoints.

**Verification:** Manual test:
- View version history → see list of past configs.
- Click revert → see confirmation dialog with diff → confirm → profile reverts.

---

### Phase Summary

| Phase | Scope | Dependencies | Risk |
|-------|-------|-------------|------|
| 1 | Models, migration, store | None | Low — additive only |
| 2 | Classification engine | None | Low — pure logic, unit testable |
| 3 | REST API changes | Phases 1, 2 | Medium — changes profile update behavior |
| 4 | Operator full restart | None (satellite-side) | Medium — new orchestration path |
| 5 | Dashboard unlock + confirm | Phase 3 | Low — UI only |
| 6 | Dashboard version history | Phases 3, 5 | Low — UI only |

Phases 1-2 can be done in parallel. Phase 4 is independent of phases 3/5/6 (satellite-side). Phases 5-6 depend on phase 3 (central API must be ready).
