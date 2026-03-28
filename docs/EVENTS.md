# Event-Driven Architecture

## Overview

pg-swarm uses an event-driven architecture where all system behavior is expressed as **events** (facts about what happened) and **commands** (instructions to execute). No component takes autonomous action — every action is the result of a command dispatched in response to an event.

```
┌──────────────────────────────────────────────────────────────┐
│                          CENTRAL                              │
│                                                               │
│  REST API → emits lifecycle events (cluster.create, etc.)     │
│  Event Store ← persists all events for audit + dashboard      │
│  Learning Engine ← tracks outcomes, tunes mappings            │
│  Policy Engine → pushes updated event→command mappings        │
│                                                               │
│          events UP ↑              ↓ events + mapping updates  │
└──────────────────────────────────────────────────────────────┘
                   │                │
            gRPC bidi stream (per satellite)
                   │                │
┌──────────────────────────────────────────────────────────────┐
│                        SATELLITE                              │
│                                                               │
│  Event Bus ← routes events to handlers                        │
│  Event→Command Cache ← maps known events to commands          │
│  State Machine (per cluster) ← derives state from events      │
│  K8s Watcher ← informers emit infrastructure events           │
│  Command Router ← sends commands to correct pod sidecar       │
│                                                               │
│          events UP ↑              ↓ commands DOWN              │
└──────────────────────────────────────────────────────────────┘
          │              │              │
     ┌────┴────┐    ┌────┴────┐    ┌────┴────┐
     │  Pod 0  │    │  Pod 1  │    │  Pod 2  │
     │ Sidecar │    │ Sidecar │    │ Sidecar │
     │         │    │         │    │         │
     │ Events  │    │ Events  │    │ Events  │
     │   UP    │    │   UP    │    │   UP    │
     │         │    │         │    │         │
     │ Execute │    │ Execute │    │ Execute │
     │ Commands│    │ Commands│    │ Commands│
     │  DOWN   │    │  DOWN   │    │  DOWN   │
     └─────────┘    └─────────┘    └─────────┘
```

### Principles

1. **Events are facts.** They describe what happened, never what to do. They are immutable and always persisted.
2. **Commands are instructions.** They tell a sidecar exactly what to execute — a SQL statement, a shell command, or a K8s API call.
3. **Sidecars never decide.** They emit events and execute commands. All decision-making lives in the satellite (fast-path via cached mappings) or central (policy decisions).
4. **Known events are handled locally.** The satellite maintains an event→command cache. When a known event arrives, the mapped commands execute immediately — no round-trip to central.
5. **Unknown events escalate.** Events not in the satellite's cache are forwarded to central. Central decides and can update the satellite's cache for future occurrences.
6. **Everything is an event.** Cluster creation, log pattern matches, pod readiness, failover, backups, config changes — all are events processed through the same framework.

---

## Event Structure

Every event has this shape:

| Field | Type | Description |
|-------|------|-------------|
| `id` | UUID | Unique event identifier, generated at source |
| `type` | string | Dot-separated event type (e.g., `cluster.create`, `log.wal.missing_checkpoint`) |
| `cluster_name` | string | Target cluster (empty for satellite-level events) |
| `namespace` | string | K8s namespace |
| `pod_name` | string | Source pod (empty for cluster-level events) |
| `severity` | string | `info`, `warning`, `error`, `critical` |
| `data` | map | Event-specific key-value pairs |
| `timestamp` | Timestamp | When the event occurred |
| `source` | string | `central`, `satellite`, `sidecar`, `k8s-watch` |
| `operation_id` | string | Correlates multi-step operations (e.g., switchover steps) |
| `payload` | oneof | Optional typed payload (ClusterConfig, SwitchoverRequest, etc.) |

---

## Event Catalog

### 0. Satellite Lifecycle

Events for the satellite agent lifecycle. These are **central-only events** — they don't flow through the satellite's EventBus because the satellite either doesn't exist yet or just disconnected. Central stores them in the event store for audit and dashboard.

| Event Type | Sender | Receiver | Severity | Data | Description |
|---|---|---|---|---|---|
| `satellite.registered` | central | central | info | `satellite_id, hostname, k8s_cluster, region` | New satellite called Register RPC. Awaiting admin approval. |
| `satellite.approved` | central | central | info | `satellite_id, hostname, k8s_cluster` | Admin approved the satellite. Auth token issued. |
| `satellite.connected` | central | central | info | `satellite_id, hostname, k8s_cluster` | Satellite opened bidi gRPC stream. EventBus is now active. |
| `satellite.disconnected` | central | central | warning | `satellite_id, hostname, k8s_cluster` | Satellite stream closed. |

### 1. Cluster Lifecycle

Events for the full cluster CRUD lifecycle. Emitted by central (user-initiated) or satellite (state changes).

| Event Type | Sender | Receiver | Severity | Payload / Data | Description |
|---|---|---|---|---|---|
| `cluster.create` | central | satellite | info | `payload: ClusterConfig` | User created a new cluster. Satellite creates K8s resources. |
| `cluster.created` | satellite | central | info | `state: creating` | K8s resources created, pods starting. |
| `cluster.update` | central | satellite | info | `payload: ClusterConfig` | User updated cluster config. Satellite reconciles. |
| `cluster.updated` | satellite | central | info | `config_version: N` | Config applied successfully. |
| `cluster.delete` | central | satellite | warning | `payload: DeleteCluster` | User deleted cluster. Satellite removes all resources. |
| `cluster.deleted` | satellite | central | info | — | All K8s resources removed, tombstone created. |
| `cluster.pause` | central | satellite | warning | — | User paused cluster. RW service removed. |
| `cluster.paused` | satellite | central | info | — | RW service deleted, cluster is read-only. |
| `cluster.unpause` | central | satellite | info | — | User unpaused cluster. RW service restored. |
| `cluster.unpaused` | satellite | central | info | — | RW service recreated. |
| `cluster.state_changed` | satellite | central | varies | `old_state, new_state, reason` | Cluster state transition (creating→running, running→degraded, etc.). |
| `cluster.config_rejected` | satellite | central | error | `reason, config_version` | Config cannot be applied (e.g., immutable storage change). |

**Cluster States**: `creating` → `running` / `degraded` / `failed` / `paused` / `deleting` / `updating` / `failover` / `promoting` / `recovering` / `deadlocked` / `emergency`

### 2. Instance Lifecycle

Events about individual PostgreSQL instances (pods). Emitted by sidecar on every tick.

| Event Type | Sender | Receiver | Severity | Data | Description |
|---|---|---|---|---|---|
| `instance.pg_up` | sidecar | satellite | info | `role: primary\|replica`, `timeline_id`, `ready: true\|false` | PostgreSQL is running and accepting connections. |
| `instance.pg_down` | sidecar | satellite | error | `error, down_count` | PostgreSQL connection failed. `down_count` is consecutive failures. |
| `instance.role_changed` | sidecar | satellite | warning | `old_role, new_role` | Pod's PG role changed (e.g., promoted from replica to primary). |
| `instance.ready` | sidecar | satellite | info | `containers_ready: true` | Pod is ready (PG running, accepting connections). |
| `instance.not_ready` | sidecar | satellite | warning | `reason` | Pod is not ready (PG starting, recovering, or down). |
| `instance.pgdata_missing` | sidecar | satellite | critical | — | `PG_VERSION` file absent. PGDATA was deleted or corrupted at runtime. |
| `instance.started` | sidecar | satellite | info | `pg_version, timeline_id` | PostgreSQL process started (first successful connection after startup). |
| `instance.stopped` | sidecar | satellite | warning | `exit_code, reason` | PostgreSQL process exited. |

### 3. Health Metrics

Periodic detailed metrics from each instance. Emitted by sidecar every 6th tick (~30s).

| Event Type | Sender | Receiver | Severity | Data | Description |
|---|---|---|---|---|---|
| `health.report` | sidecar | satellite | info | See fields below | Full instance health snapshot. Satellite forwards to central for dashboard. |

**`health.report` data fields:**

| Key | Type | Description |
|-----|------|-------------|
| `role` | string | primary / replica |
| `ready` | bool | PG accepting connections |
| `connections_used` | int | Current connection count |
| `connections_max` | int | max_connections setting |
| `connections_active` | int | state='active' count |
| `disk_used_bytes` | int64 | Sum of pg_database_size() |
| `wal_disk_bytes` | int64 | WAL directory size on disk |
| `replication_lag_bytes` | int64 | WAL byte lag (replicas) |
| `replication_lag_seconds` | float | Time-based lag (replicas) |
| `timeline_id` | int64 | Current timeline |
| `pg_start_time` | timestamp | pg_postmaster_start_time() |
| `wal_receiver_active` | bool | WAL streaming active (replicas) |
| `wal_records` | int64 | From pg_stat_wal |
| `wal_bytes` | int64 | From pg_stat_wal |
| `index_hit_ratio` | float | 0.0–1.0 from pg_statio_user_indexes |
| `txn_commit_ratio` | float | 0.0–1.0 from pg_stat_database |
| `database_stats` | JSON | Per-database sizes and cache hit ratios |
| `table_stats` | JSON | Per-table activity metrics (top 30 per DB) |
| `slow_queries` | JSON | Top 10 by mean execution time |

### 4. Replication

Events about WAL streaming and replication health.

| Event Type | Sender | Receiver | Severity | Data | Description |
|---|---|---|---|---|---|
| `replication.streaming_started` | sidecar | satellite | info | `primary_host` | WAL receiver connected to primary and streaming. |
| `replication.streaming_stopped` | sidecar | satellite | warning | `duration_since_last, replay_lsn` | WAL receiver disconnected. Grace period starts. |
| `replication.lag_high` | sidecar | satellite | warning | `lag_bytes, lag_seconds, threshold` | Replication lag exceeds configured threshold. |
| `replication.slot_invalidated` | sidecar | satellite | critical | `slot_name` | Replication slot fell behind max_slot_wal_keep_size. Permanent WAL gap. |
| `replication.wal_gap` | sidecar | satellite | critical | `required_segment, available` | Required WAL segment has been recycled on primary. |

### 5. Timeline

Events about PostgreSQL timeline management.

| Event Type | Sender | Receiver | Severity | Data | Description |
|---|---|---|---|---|---|
| `timeline.divergence` | sidecar | satellite | critical | `local_tli, primary_tli, replay_lsn` | Local timeline doesn't match primary. Needs rewind or rebuild. |
| `timeline.switch` | sidecar | satellite | info | `old_tli, new_tli` | Timeline changed (e.g., after promotion). |

### 6. Lease

Events about the Kubernetes leader lease used for primary identity.

| Event Type | Sender | Receiver | Severity | Data | Description |
|---|---|---|---|---|---|
| `lease.acquired` | sidecar | satellite | info | `holder` | This pod acquired the leader lease. |
| `lease.renewed` | sidecar | satellite | info | `holder` | This pod renewed the leader lease. |
| `lease.expired` | sidecar | satellite | warning | `old_holder, expired_at` | Leader lease expired (no holder renewed it). |
| `lease.conflict` | sidecar | satellite | critical | `local_role, lease_holder` | Pod is running as primary but lease is held by another pod (split-brain). |

### 7. Log-Based Events

PG log patterns generate specific typed events. Each log rule defines a `pattern` (regex) and the `event_type` to emit when matched. **No action is taken by the sidecar** — only the event is emitted.

#### Log Rule Structure

```yaml
log_rules:
  - name: <rule-name>
    pattern: <regex>
    event_type: <event type to emit>   # e.g., "log.wal.missing_checkpoint"
    severity: critical | error | warning | info
    cooldown: <duration>               # suppress re-fires
    category: <grouping label>
    enabled: true | false
```

#### 7.1 WAL & Checkpoint Events

| Event Type | Pattern | Severity | Category |
|---|---|---|---|
| `log.wal.invalid_record` | `invalid record length at .* expected at least \d+, got 0` | critical | wal |
| `log.wal.missing_checkpoint` | `could not locate a valid checkpoint record` | critical | wal |
| `log.wal.corrupt_read` | `could not read WAL at .* invalid record length` | critical | wal |
| `log.wal.truncated_segment` | `WAL file .* has size \d+, should be \d+` | critical | wal |
| `log.wal.incorrect_prev_link` | `record with incorrect prev-link` | critical | wal |

#### 7.2 Timeline Events

| Event Type | Pattern | Severity | Category |
|---|---|---|---|
| `log.timeline.not_in_history` | `requested starting point .* is not in this server's history` | critical | timeline |
| `log.timeline.ahead_of_flush` | `requested starting point .* is ahead of the WAL flush position` | critical | timeline |
| `log.timeline.not_child` | `requested timeline \d+ is not a child of this server's history` | critical | timeline |
| `log.timeline.incompatible` | `new timeline \d+ is not a child of database system timeline \d+` | critical | timeline |
| `log.timeline.stream_failed` | `could not receive data from WAL stream:.*timeline` | critical | timeline |

#### 7.3 Corruption & Data Integrity Events

| Event Type | Pattern | Severity | Category |
|---|---|---|---|
| `log.corruption.invalid_page` | `invalid page in block \d+ of relation` | critical | corruption |
| `log.corruption.read_failed` | `could not read block \d+ in file` | critical | corruption |
| `log.corruption.disk_full` | `could not write to file.*No space left on device` | critical | corruption |
| `log.corruption.fsync_failed` | `could not fsync file` | critical | corruption |
| `log.corruption.wrong_ownership` | `PANIC:.*data directory .* has wrong ownership` | critical | corruption |
| `log.corruption.write_failed` | `PANIC:.*could not write to log file` | critical | corruption |

#### 7.4 Connection & Resource Events

| Event Type | Pattern | Severity | Category |
|---|---|---|---|
| `log.connection.too_many_clients` | `FATAL:.*sorry, too many clients already` | warning | connection |
| `log.connection.reserved_full` | `FATAL:.*remaining connection slots are reserved` | warning | connection |
| `log.connection.shutting_down` | `FATAL:.*the database system is shutting down` | info | connection |
| `log.connection.starting_up` | `FATAL:.*the database system is starting up` | info | connection |
| `log.resource.shared_memory` | `LOG:.*out of shared memory` | error | resource |
| `log.resource.out_of_memory` | `ERROR:.*out of memory` | error | resource |

#### 7.5 Replication Slot & WAL Retention Events

| Event Type | Pattern | Severity | Category |
|---|---|---|---|
| `log.slot.invalidated` | `replication slot .* has been invalidated` | critical | slot |
| `log.slot.wal_removed` | `requested WAL segment .* has already been removed` | critical | slot |
| `log.slot.not_exist` | `could not start WAL streaming:.*replication slot .* does not exist` | error | slot |
| `log.replication.timeout` | `terminating walsender process due to replication timeout` | warning | replication |
| `log.replication.conflict` | `ERROR:.*canceling statement due to conflict with recovery` | warning | replication |

#### 7.6 Authentication Events

| Event Type | Pattern | Severity | Category |
|---|---|---|---|
| `log.auth.password_failed` | `FATAL:.*password authentication failed for user` | warning | auth |
| `log.auth.no_hba_entry` | `FATAL:.*no pg_hba.conf entry for` | warning | auth |
| `log.auth.ssl_required` | `FATAL:.*SSL connection is required` | warning | auth |

#### 7.7 Recovery & Startup Events

| Event Type | Pattern | Severity | Category |
|---|---|---|---|
| `log.recovery.stale_backup_label` | `FATAL:.*could not open file.*backup_label` | error | recovery |
| `log.recovery.crash_detected` | `FATAL:.*database system was interrupted; last known up at` | warning | recovery |
| `log.recovery.auto_recovery` | `LOG:.*database system was not properly shut down; automatic recovery in progress` | info | recovery |
| `log.recovery.wal_level_minimal` | `WAL was generated with .wal_level=minimal., cannot continue recovering` | critical | recovery |
| `log.recovery.pg_wal_missing` | `FATAL:.*could not open directory.*pg_wal` | critical | recovery |
| `log.recovery.incompatible_version` | `database files are incompatible with server` | critical | recovery |
| `log.recovery.catalog_corruption` | `cache lookup failed for (relation\|type\|function\|operator)` | critical | recovery |
| `log.recovery.tablespace_missing` | `could not open tablespace directory` | critical | recovery |
| `log.recovery.consistent_state` | `LOG:.*consistent recovery state reached` | info | recovery |
| `log.recovery.redo_done` | `LOG:.*redo done at` | info | recovery |

#### 7.8 Streaming Replication Events

| Event Type | Pattern | Severity | Category |
|---|---|---|---|
| `log.streaming.started` | `LOG:.*started streaming WAL from primary` | info | streaming |
| `log.streaming.primary_unreachable` | `FATAL:.*could not connect to the primary server` | error | streaming |
| `log.streaming.terminated` | `LOG:.*replication terminated by primary server` | warning | streaming |
| `log.streaming.max_senders` | `FATAL:.*number of requested standby connections exceeds max_wal_senders` | error | streaming |

#### 7.9 Archive & Backup Events

| Event Type | Pattern | Severity | Category |
|---|---|---|---|
| `log.archive.command_failed` | `LOG:.*archive command failed with exit code` | error | archive |
| `log.archive.slow` | `WARNING:.*archive_command .* took .* seconds` | warning | archive |
| `log.archive.restored` | `LOG:.*restored log file .* from archive` | info | archive |

### 8. Recovery & Failover

Events emitted during recovery operations. These are the *result* events — the commands that trigger these operations are described in the Commands section.

| Event Type | Sender | Receiver | Severity | Data | Description |
|---|---|---|---|---|---|
| `recovery.failover_needed` | satellite | central | critical | `reason, cluster_state, failed_pod` | State machine determined failover is required. |
| `recovery.promote_started` | satellite | central | warning | `target_pod, operation_id` | Promote command sent to target pod. |
| `recovery.promote_completed` | satellite | central | info | `target_pod, success, duration_ms, operation_id` | Promotion finished. |
| `recovery.demote_started` | satellite | central | warning | `target_pod, reason, operation_id` | Demote command sent (split-brain or switchover). |
| `recovery.demote_completed` | satellite | central | info | `target_pod, success, duration_ms, operation_id` | Demotion finished. |
| `recovery.rebuild_started` | satellite | central | warning | `target_pod, reason, operation_id` | Rebuild (basebackup) command sent. |
| `recovery.rebuild_completed` | satellite | central | info | `target_pod, success, duration_ms, operation_id` | Rebuild finished. |
| `recovery.rewind_started` | satellite | central | warning | `target_pod, reason, operation_id` | Rewind command sent. |
| `recovery.rewind_completed` | satellite | central | info | `target_pod, success, duration_ms, operation_id` | Rewind finished. |
| `recovery.fence_started` | satellite | central | warning | `target_pod, reason` | Fence command sent. |
| `recovery.fence_completed` | satellite | central | info | `target_pod, success` | Pod fenced. |
| `recovery.unfence_completed` | satellite | central | info | `target_pod` | Pod unfenced. |
| `recovery.deadlock_detected` | satellite | central | critical | `stuck_pods, duration` | All pods stuck, no primary, no progress. |
| `recovery.emergency_promote` | satellite | central | critical | `target_pod, reason` | Forced promotion to break deadlock. |

### 9. Switchover

Events for planned primary switchover (user-initiated).

| Event Type | Sender | Receiver | Severity | Data | Description |
|---|---|---|---|---|---|
| `switchover.requested` | central | satellite | info | `payload: SwitchoverRequest`, `target_pod, operation_id` | User requested switchover. |
| `switchover.step` | satellite | central | info | `step, step_name, status, target_pod, operation_id` | Progress update for each step. |
| `switchover.completed` | satellite | central | info | `success, operation_id, duration_ms` | Switchover finished successfully. |
| `switchover.failed` | satellite | central | error | `error_message, step, operation_id` | Switchover failed at a step. |
| `switchover.rolled_back` | satellite | central | warning | `step, operation_id` | Switchover rolled back (unfenced primary). |

**Switchover Steps**: `verify_target` → `find_primary` → `check_status` → `fence_primary` → `checkpoint` → `transfer_lease` → `promote_target` → `unfence_old_primary`

### 10. Backup

Events for backup operations (scheduled + ad-hoc).

| Event Type | Sender | Receiver | Severity | Data | Description |
|---|---|---|---|---|---|
| `backup.trigger` | central | satellite | info | `backup_type: base\|incremental\|logical` | User triggered ad-hoc backup. |
| `backup.scheduled` | satellite | satellite | info | `backup_type, schedule` | Cron schedule fired for backup. |
| `backup.started` | satellite | central | info | `backup_type, pod_name` | Backup execution started. |
| `backup.completed` | satellite | central | info | `backup_type, size_bytes, backup_path, pg_version, wal_start_lsn, wal_end_lsn, duration_ms` | Backup completed successfully. |
| `backup.failed` | satellite | central | error | `backup_type, error_message` | Backup failed. |

### 11. Restore

Events for restore operations.

| Event Type | Sender | Receiver | Severity | Data | Description |
|---|---|---|---|---|---|
| `restore.requested` | central | satellite | warning | `restore_type: logical\|pitr`, `restore_mode: in_place\|new_cluster`, `target_time, backup_id` | User initiated restore. |
| `restore.started` | satellite | central | info | `restore_id, restore_type` | Restore execution started. |
| `restore.completed` | satellite | central | info | `restore_id, success, duration_ms` | Restore finished. |
| `restore.failed` | satellite | central | error | `restore_id, error_message` | Restore failed. |

### 12. Database Management

Events for dynamic database creation on clusters.

| Event Type | Sender | Receiver | Severity | Data | Description |
|---|---|---|---|---|---|
| `database.create_requested` | satellite | satellite | info | `db_name, db_user` | New database detected in cluster config. |
| `database.created` | satellite | central | info | `db_name, db_user, pod_name` | Database created successfully on primary. |
| `database.create_failed` | satellite | central | error | `db_name, error_message` | Database creation failed. |

### 13. Reconciliation

Events for periodic drift detection and correction.

| Event Type | Sender | Receiver | Severity | Data | Description |
|---|---|---|---|---|---|
| `reconcile.scheduled` | satellite | satellite | info | `trigger: timer\|watch\|manual` | Periodic reconcile timer fired or K8s watch triggered. |
| `reconcile.drift_detected` | satellite | central | warning | `resource_type, resource_name, field, expected, actual` | Actual K8s state doesn't match desired. |
| `reconcile.corrected` | satellite | central | info | `resource_type, resource_name, field` | Drift corrected. |
| `reconcile.completed` | satellite | satellite | info | `changes_made: N` | Reconcile cycle finished. |

### 14. K8s Infrastructure

Events from Kubernetes informers/watches.

| Event Type | Sender | Receiver | Severity | Data | Description |
|---|---|---|---|---|---|
| `k8s.pod.created` | k8s-watch | satellite | info | `pod_name, phase` | New pod appeared in the namespace. |
| `k8s.pod.ready` | k8s-watch | satellite | info | `pod_name` | Pod transitioned to Ready condition. |
| `k8s.pod.not_ready` | k8s-watch | satellite | warning | `pod_name, reason` | Pod lost Ready condition. |
| `k8s.pod.deleted` | k8s-watch | satellite | warning | `pod_name` | Pod was deleted. |
| `k8s.statefulset.updated` | k8s-watch | satellite | info | `replicas, ready_replicas, current_replicas` | StatefulSet spec or status changed. |

### 15. Configuration

Events for configuration changes that require reload or restart.

| Event Type | Sender | Receiver | Severity | Data | Description |
|---|---|---|---|---|---|
| `config.received` | satellite | central | info | `config_version` | New config received from central. |
| `config.applied` | satellite | central | info | `config_version` | Config applied to K8s resources. |
| `config.rejected` | satellite | central | error | `config_version, reason` | Config rejected (immutable field change, etc.). |
| `config.reload_needed` | satellite | satellite | info | `changed_params` | PG params changed that require pg_reload_conf(). |
| `config.restart_needed` | satellite | satellite | warning | `changed_params` | PG params changed that require full restart (wal_level, etc.). |

---

## Commands

Commands are instructions sent from the satellite to a sidecar. The sidecar executes them and reports success/failure. Commands are **never** generated by the sidecar itself.

### Command Structure

| Field | Type | Description |
|-------|------|-------------|
| `id` | UUID | Unique command identifier |
| `type` | string | Command type (e.g., `sql.promote`, `shell.stop_pg`, `composite.rebuild`) |
| `cluster_name` | string | Target cluster |
| `namespace` | string | K8s namespace |
| `pod_name` | string | Target pod |
| `params` | map | Command-specific parameters |
| `timeout` | duration | Max execution time |
| `operation_id` | string | Correlates with the triggering event |

### SQL Commands

Executed by the sidecar via a direct PostgreSQL connection.

| Command | SQL | Description |
|---------|-----|-------------|
| `sql.promote` | `SELECT pg_promote()` | Promote replica to primary. |
| `sql.checkpoint` | `CHECKPOINT` | Force WAL checkpoint. |
| `sql.fence` | `ALTER SYSTEM SET default_transaction_read_only = on; SELECT pg_reload_conf()` | Block writes on this instance. |
| `sql.unfence` | `ALTER SYSTEM SET default_transaction_read_only = off; SELECT pg_reload_conf()` | Allow writes on this instance. |
| `sql.reload` | `SELECT pg_reload_conf()` | Reload configuration without restart. |
| `sql.create_database` | `CREATE DATABASE {db_name} OWNER {db_user}` | Create a new database. Params: `db_name`, `db_user`, `password`. |
| `sql.terminate_backends` | `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE ...` | Kill client connections. Params: `exclude_replication: bool`. |
| `sql.execute` | `{query}` | Execute arbitrary SQL. Params: `query`. |

### Shell Commands

Executed by the sidecar via K8s exec into the postgres container.

| Command | Shell | Description |
|---------|-------|-------------|
| `shell.stop_pg` | `pg_ctl stop -m fast` | Gracefully stop PostgreSQL. |
| `shell.stop_pg_immediate` | `pg_ctl stop -m immediate` | Immediately stop PostgreSQL. |
| `shell.write_standby_signal` | `touch $PGDATA/standby.signal` | Mark instance for standby mode on next start. |
| `shell.write_basebackup_marker` | `touch $PGDATA/.pg-swarm-needs-basebackup` | Signal wrapper to run pg_basebackup on next start. |
| `shell.remove_markers` | `rm -f $PGDATA/standby.signal $PGDATA/.pg-swarm-needs-basebackup` | Clear recovery markers (deadlock breaker). |
| `shell.remove_backup_label` | `rm -f $PGDATA/backup_label $PGDATA/tablespace_map` | Remove stale backup artifacts. |
| `shell.set_primary_conninfo` | Write `primary_conninfo` to `postgresql.auto.conf` | Point replica at the correct primary. Params: `primary_host`. |
| `shell.execute` | `{command}` | Execute arbitrary shell command. Params: `command`. |

### K8s Commands

Executed by the sidecar via the Kubernetes API (sidecar has RBAC for pods, leases).

| Command | API Call | Description |
|---------|----------|-------------|
| `k8s.label_pod` | `PATCH /api/v1/pods/{pod}` | Set pod labels. Params: `labels` (map). |
| `k8s.delete_lease` | `DELETE /apis/coordination.k8s.io/v1/leases/{name}` | Delete the leader lease. Params: `lease_name`. |

### Composite Commands

Sequences of atomic commands executed in order. If any step fails, the sequence stops and reports the failure.

| Command | Steps | Description |
|---------|-------|-------------|
| `composite.promote` | `sql.checkpoint` → `sql.promote` → `k8s.label_pod(role=primary)` | Full promotion sequence. |
| `composite.demote` | `sql.fence` → `sql.terminate_backends` → `shell.write_standby_signal` → `shell.set_primary_conninfo` → `shell.remove_backup_label` → `shell.stop_pg` → `k8s.label_pod(role=replica)` | Full demotion sequence. |
| `composite.rebuild` | `shell.write_basebackup_marker` → `shell.stop_pg` | Trigger full pg_basebackup on next start. |
| `composite.rewind` | `shell.write_standby_signal` → `shell.set_primary_conninfo` → `shell.stop_pg` | Trigger pg_rewind on next start. |
| `composite.fence` | `sql.fence` → `sql.terminate_backends` | Block writes and kill existing connections. |
| `composite.unfence` | `sql.unfence` | Resume writes. |
| `composite.restart` | `shell.stop_pg` | Graceful restart (wrapper auto-restarts PG). |

---

## Event → Command Cache

The satellite maintains a local cache that maps event types to commands. This enables fast, local decision-making without a central round-trip.

### Cache Structure

```json
{
  "version": 1,
  "updated_at": "2026-03-27T12:00:00Z",
  "mappings": [
    {
      "event_type": "log.wal.missing_checkpoint",
      "conditions": {},
      "commands": ["composite.rebuild"],
      "cooldown": "120s"
    },
    {
      "event_type": "log.timeline.not_in_history",
      "conditions": {},
      "commands": ["composite.rewind"],
      "cooldown": "120s"
    },
    {
      "event_type": "instance.pgdata_missing",
      "conditions": {},
      "commands": ["composite.rebuild"],
      "cooldown": "120s"
    },
    {
      "event_type": "lease.conflict",
      "conditions": {},
      "commands": ["composite.demote"],
      "cooldown": "60s"
    }
  ],
  "state_machine_rules": [
    {
      "event_type": "instance.pg_down",
      "conditions": {
        "pod_role": "primary",
        "consecutive_count": 3,
        "cluster_has_no_primary": true
      },
      "action": "failover",
      "cooldown": "30s"
    },
    {
      "event_type": "lease.expired",
      "conditions": {
        "cluster_has_no_primary": true,
        "consecutive_ticks": 5
      },
      "action": "emergency_promote",
      "cooldown": "30s"
    }
  ]
}
```

### Cache Behavior

| Scenario | Behavior |
|---|---|
| Event type found in `mappings` | Execute mapped commands immediately. No central round-trip. |
| Event type found in `state_machine_rules` | Evaluate conditions against cluster state machine. If met, execute the mapped action (which produces commands). |
| Event type NOT found in cache | Forward event to central. Central decides and may update the cache. |
| Central pushes cache update | Satellite merges the update atomically. New mappings take effect immediately. |

### Default Mappings (shipped with every satellite)

These are the built-in mappings seeded from the log rule catalog:

| Event Type | Commands | Cooldown | Rationale |
|---|---|---|---|
| **WAL & Checkpoint** | | | |
| `log.wal.invalid_record` | `composite.restart` | 60s | Zeroed WAL segment. Restart lets wrapper's WAL cleanup handle it. |
| `log.wal.missing_checkpoint` | `composite.rebuild` | 120s | Checkpoint WAL corrupt. No point retrying. |
| `log.wal.corrupt_read` | `composite.restart` | 60s | Variant during streaming recovery. |
| `log.wal.truncated_segment` | `composite.restart` | 60s | Truncated WAL from disk full or interrupted rewind. |
| `log.wal.incorrect_prev_link` | `composite.restart` | 60s | Stale WAL from diverged timeline. |
| **Timeline** | | | |
| `log.timeline.not_in_history` | `composite.rewind` | 120s | Replica on old timeline. Needs pg_rewind. |
| `log.timeline.ahead_of_flush` | `composite.rewind` | 120s | Replica ahead of new primary. |
| `log.timeline.not_child` | `composite.rewind` | 120s | Timeline fork after promotion. |
| `log.timeline.incompatible` | `composite.rewind` | 120s | Can't follow timeline switch. |
| `log.timeline.stream_failed` | `composite.rewind` | 120s | Streaming failed due to timeline. |
| **Corruption** | | | |
| `log.corruption.invalid_page` | *(escalate to central)* | 300s | Data corruption. Needs investigation. |
| `log.corruption.read_failed` | *(escalate to central)* | 300s | I/O error. Possible disk failure. |
| `log.corruption.disk_full` | *(escalate to central)* | 60s | Disk full. Automated cleanup risky. |
| `log.corruption.fsync_failed` | *(escalate to central)* | 60s | Possible hardware failure. |
| `log.corruption.wrong_ownership` | *(escalate to central)* | — | Permissions issue. |
| **Connection & Resource** | | | |
| `log.connection.*` | *(escalate to central)* | 30s | Informational. No automated fix. |
| `log.resource.*` | *(escalate to central)* | 30s | Needs human attention. |
| **Replication Slot** | | | |
| `log.slot.invalidated` | `composite.rebuild` | 300s | Permanent WAL gap. Full rebuild only fix. |
| `log.slot.wal_removed` | `composite.rebuild` | 300s | WAL recycled. Permanent gap. |
| `log.slot.not_exist` | *(escalate to central)* | 120s | Needs slot recreation or rebuild. |
| **Recovery & Startup** | | | |
| `log.recovery.stale_backup_label` | `composite.restart` | 60s | Stale backup_label blocking startup. |
| `log.recovery.wal_level_minimal` | `composite.rebuild` | 120s | Source had wal_level=minimal. Must rebuild. |
| `log.recovery.pg_wal_missing` | `composite.rebuild` | 120s | WAL directory missing. |
| `log.recovery.incompatible_version` | `composite.rebuild` | 300s | PG major version mismatch. |
| `log.recovery.catalog_corruption` | `composite.rebuild` | 300s | System catalog corruption. |
| `log.recovery.tablespace_missing` | `composite.rebuild` | 120s | Tablespace path mismatch after rewind. |
| `log.recovery.consistent_state` | *(no command — positive signal)* | — | Recovery complete. Reset error counters. |
| `log.recovery.redo_done` | *(no command — positive signal)* | — | WAL replay finished. |
| **Instance** | | | |
| `instance.pgdata_missing` | `composite.rebuild` | 120s | PGDATA deleted at runtime. |
| `lease.conflict` | `composite.demote` | 60s | Split-brain: this pod is primary but doesn't hold the lease. |
| `timeline.divergence` | `composite.rewind` | 120s | Timeline mismatch detected outside of log monitoring. |
| `replication.slot_invalidated` | `composite.rebuild` | 300s | Permanent WAL gap via slot invalidation. |
| `replication.wal_gap` | `composite.rebuild` | 300s | Permanent WAL gap via segment removal. |

### State Machine Rules (compiled into satellite)

These are evaluated by the satellite's cluster state machine, not simple event→command lookups:

| Trigger Event | Conditions | Action | Description |
|---|---|---|---|
| `instance.pg_down` | `role=primary`, `down_count >= 3`, `lease_expired` | **Failover**: select best replica, send `composite.promote` | Primary down. Promote best replica. |
| `instance.pg_down` | `role=primary`, `down_count >= 3`, `lease NOT expired` | **Wait**: suppress, primary might recover | Primary flapping. Give it time. |
| `lease.expired` | `cluster_has_no_primary`, `ticks >= 5` | **Emergency promote**: pick pod with most data, send `shell.remove_markers` + `composite.promote` | Zero-primary deadlock. Force promote. |
| `cluster.state_changed` | `new_state=promoting`, `timeout > 30s` | **Deadlock**: transition to `deadlocked` state | Promote timed out. |
| `cluster.state_changed` | `new_state=recovering`, `all_replicas_ready` | **Healthy**: transition to `running` | All pods recovered after failover. |
| `recovery.promote_completed` | `success=true` | For each non-primary pod: if `timeline_diverged` → `composite.rewind`, elif `wal_gap` → `composite.rebuild`, else wait | Post-promotion cleanup. |

### Best Replica Selection

When failover is needed, the satellite ranks candidate replicas:

1. **Has data** (PG was recently connected) — hard requirement
2. **Highest replay LSN** — closest to old primary's state
3. **Lowest replication lag** — fewest transactions behind
4. **Ready container** — PG is currently running
5. **Not in basebackup loop** — wrapper is not stuck rebuilding

---

## Event Routing

### Sidecar → Satellite

The sidecar sends events to the satellite via the existing `SidecarStreamService.Connect` gRPC bidi stream. Events are added to `SidecarMessage` as a new oneof field.

The sidecar emits events in its `tick()` loop (every 5s):
1. Check PGDATA → `instance.pgdata_missing` if missing
2. Connect to PG → `instance.pg_up` or `instance.pg_down`
3. Compare role to last tick → `instance.role_changed` if different
4. Check WAL receiver → `replication.streaming_started` or `replication.streaming_stopped`
5. Check timeline → `timeline.divergence` if mismatch
6. Check lease → `lease.acquired`, `lease.renewed`, `lease.expired`, or `lease.conflict`
7. Every 6th tick: `health.report` with full metrics

The log watcher emits `log.*` events when patterns match.

The sidecar **never** acts on these events. It waits for commands.

### Satellite → Sidecar

Commands flow from satellite to sidecar via the same bidi stream (`SidecarCommand`). The satellite's event bus processes events, looks up the event→command cache, and dispatches commands to the correct pod via `SidecarStreamManager`.

### Satellite → Central

All events published to the satellite's event bus are forwarded to central via the `SatelliteStreamService.Connect` bidi stream. Central persists them in the `cluster_events` table and pushes to the WebSocket hub for the dashboard.

### Central → Satellite

User-initiated operations arrive as events:
- REST API call → central constructs Event → pushes to satellite via stream
- `cluster.create`, `cluster.update`, `cluster.delete`, `switchover.requested`, `backup.trigger`, `restore.requested`

Central can also push cache updates when the learning engine tunes mappings.

---

## Satellite Operating Modes

| Mode | When | Event Processing | Command Source |
|---|---|---|---|
| **Normal** | Satellite connected to central, sidecars connected | EventBus routes events to handlers, looks up cache, executes commands | Satellite event→command cache + state machine |
| **Disconnected from central** | Central unreachable | Same as normal, but events are queued for central. Cache still works. | Satellite cache (last synced version) |
| **Sidecar fallback** | Satellite unreachable from sidecar for 60s | Sidecar reverts to local failover logic (existing handlePrimary/handleReplica code) | Local sidecar (safety net) |

---

## Learning Engine (Central)

Central observes all events and tracks recovery outcomes:

1. **Outcome tracking**: Every `recovery.*.completed` event is correlated with the triggering event and the command that was executed. Success/failure and duration are recorded.
2. **Rule effectiveness**: Central computes per-mapping success rates. If mapping X consistently fails, central can push an updated mapping to the satellite.
3. **Cross-cluster sharing**: When a new cluster is created, its satellite receives mappings tuned from fleet-wide outcomes.

### Outcome Table

```sql
CREATE TABLE recovery_outcomes (
    id               UUID PRIMARY KEY,
    satellite_id     UUID NOT NULL,
    cluster_name     TEXT NOT NULL,
    trigger_event    TEXT NOT NULL,     -- event type that triggered recovery
    trigger_rule     TEXT DEFAULT '',   -- log rule name (if log-based)
    command          TEXT NOT NULL,     -- command that was executed
    success          BOOLEAN NOT NULL,
    duration_ms      INTEGER NOT NULL,
    operation_id     TEXT NOT NULL,
    created_at       TIMESTAMPTZ DEFAULT NOW()
);
```
