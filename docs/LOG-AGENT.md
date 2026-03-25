# Log-Based Recovery Agent (Satellite Edge)

## Overview

A rule-based log watcher in the failover sidecar that reads PostgreSQL container logs in real-time, matches patterns, and takes automated corrective action. Instead of waiting for PG to crash 3 times (crash-loop breaker) or for the sidecar's periodic health check to fire, the agent reacts on the **first log line** that indicates a problem.

## Where it lives

**The failover sidecar** — not the satellite. It shares the pod, has `pods/exec` RBAC, has `rest.Config`, and runs a monitoring loop already. Latency is milliseconds, not the seconds a satellite gRPC round-trip would add.

## How it reads logs

K8s log API with `Follow: true` on the postgres container:

```go
req := client.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
    Container: "postgres",
    Follow:    true,
    SinceTime: &startTime,
})
stream, _ := req.Stream(ctx)
// scan lines, match rules
```

No shared volume needed. No changes to the postgres container.

## Rule structure

```yaml
log_rules:
  - name: <rule-name>
    pattern: <regex>
    severity: critical | error | warning | info
    action: restart | rewind | rebasebackup | event | exec
    exec_command: <optional, for action=exec>
    cooldown: <duration>
    max_fires: <optional, 0=unlimited>
```

## Complete pattern catalog

### Category 1: WAL & Checkpoint Errors

These are the errors that currently cause crash loops. The log agent would catch them on the first occurrence.

| # | Pattern | Action | Cooldown | Why |
|---|---------|--------|----------|-----|
| 1 | `invalid record length at .* expected at least \d+, got 0` | `restart` | 60s | Zeroed/pre-allocated WAL segment. Restart lets wrapper's WAL cleanup + crash-loop breaker handle it. |
| 2 | `could not locate a valid checkpoint record` | `rebasebackup` | 120s | Checkpoint WAL is missing or corrupt. No point retrying — go straight to basebackup. |
| 3 | `could not read WAL at .* invalid record length` | `restart` | 60s | Variant of #1 during streaming recovery. |
| 4 | `WAL file .* has size \d+, should be \d+` | `restart` | 60s | Truncated WAL segment (disk full during write, interrupted pg_rewind). |
| 5 | `record with incorrect prev-link` | `restart` | 60s | Stale WAL segment with record chain pointers from old diverged timeline. Restart triggers WAL cleanup. |

### Category 2: Timeline & Replication Errors

Currently handled by the sidecar's `hasTimelineDivergence()` on a periodic tick. The log agent catches them instantly.

| # | Pattern | Action | Cooldown | Why |
|---|---------|--------|----------|-----|
| 5 | `requested starting point .* is not in this server's history` | `rewind` | 120s | Replica's LSN is on an old timeline the primary doesn't serve. Needs pg_rewind. |
| 6 | `requested starting point .* is ahead of the WAL flush position` | `rewind` | 120s | Replica received more WAL from old primary than new primary has. Needs pg_rewind. |
| 7 | `requested timeline \d+ is not a child of this server's history` | `rewind` | 120s | Timeline fork — classic post-promotion state. |
| 8 | `new timeline \d+ is not a child of database system timeline \d+` | `rewind` | 120s | PG can't follow the timeline switch. |
| 9 | `could not receive data from WAL stream:.*timeline` | `rewind` | 120s | WAL streaming failed due to timeline mismatch (catch-all). |

### Category 3: Corruption & Data Integrity

These require human attention or a full rebuild. The agent reports them and optionally triggers recovery.

| # | Pattern | Action | Cooldown | Why |
|---|---------|--------|----------|-----|
| 10 | `invalid page in block \d+ of relation` | `event` | 300s | Data page corruption. Don't auto-fix — report for investigation. Might need `SET zero_damaged_pages = on` or PITR restore. |
| 11 | `could not read block \d+ in file` | `event` | 300s | I/O error reading a data file. Could be disk failure. |
| 12 | `could not write to file.*No space left on device` | `event` | 60s | Disk full. Alert immediately. Automated cleanup risky. |
| 13 | `could not fsync file` | `event` | 60s | Disk I/O error. Possible hardware failure. |
| 14 | `PANIC:.*data directory .* has wrong ownership` | `event` | 0 | Permissions issue. Container security context wrong. |
| 15 | `PANIC:.*could not write to log file` | `event` | 0 | Log directory issue. |

### Category 4: Connection & Resource Exhaustion

| # | Pattern | Action | Cooldown | Why |
|---|---------|--------|----------|-----|
| 16 | `FATAL:.*sorry, too many clients already` | `event` | 30s | Connection limit reached. Needs pgbouncer or limit tuning. |
| 17 | `FATAL:.*remaining connection slots are reserved` | `event` | 30s | All non-reserved slots used. |
| 18 | `FATAL:.*the database system is shutting down` | `event` | 10s | PG is stopping. Expected during failover, noise otherwise. |
| 19 | `FATAL:.*the database system is starting up` | `event` | 10s | PG not ready yet. Expected during recovery. |
| 20 | `LOG:.*out of shared memory` | `event` | 60s | shared_buffers or work_mem exhaustion. |
| 21 | `ERROR:.*out of memory` | `event` | 30s | OOM at query level (before kernel OOM killer). |

### Category 5: Replication Slot & WAL Retention

| # | Pattern | Action | Cooldown | Why |
|---|---------|--------|----------|-----|
| 22 | `replication slot .* has been invalidated` | `rebasebackup` | 300s | Slot fell behind max_slot_wal_keep_size. WAL gap is permanent — only fix is full rebuild. |
| 23 | `requested WAL segment .* has already been removed` | `rebasebackup` | 300s | Primary recycled the WAL this replica needs. Permanent gap. |
| 24 | `terminating walsender process due to replication timeout` | `event` | 60s | Replication connection dropped. Usually self-heals. |
| 25 | `could not start WAL streaming:.*replication slot .* does not exist` | `event` | 120s | Slot was dropped. Needs recreation or rebasebackup. |
| 26 | `ERROR:.*canceling statement due to conflict with recovery` | `event` | 30s | Hot standby query conflict. Normal but worth tracking frequency. |

### Category 6: Authentication & Security

| # | Pattern | Action | Cooldown | Why |
|---|---------|--------|----------|-----|
| 27 | `FATAL:.*password authentication failed for user` | `event` | 30s | Wrong password. Could indicate secret rotation issue. |
| 28 | `FATAL:.*no pg_hba.conf entry for` | `event` | 30s | HBA config doesn't allow this connection. |
| 29 | `FATAL:.*SSL connection is required` | `event` | 60s | SSL enforcement mismatch. |

### Category 7: Recovery & Startup

| # | Pattern | Action | Cooldown | Why |
|---|---------|--------|----------|-----|
| 30 | `FATAL:.*could not open file.*backup_label` | `restart` | 60s | Stale backup_label blocking startup. Restart triggers cleanup. |
| 31 | `FATAL:.*database system was interrupted; last known up at` | `event` | 60s | Crash recovery needed. PG handles this itself — just track it. |
| 32 | `LOG:.*database system was not properly shut down; automatic recovery in progress` | `event` | 60s | Normal after unclean stop. Informational. |
| 33 | `WAL was generated with .wal_level=minimal., cannot continue recovering` | `rebasebackup` | 120s | Source had wal_level=minimal. WAL lacks replication info. Only fix is full rebasebackup from a primary with wal_level=replica. |
| 34 | `FATAL:.*could not open directory.*pg_wal` | `rebasebackup` | 120s | WAL directory missing. Unrecoverable without rebuild. |
| 35 | `database files are incompatible with server` | `rebasebackup` | 300s | PG major version changed (e.g. image updated 16→17 without pg_upgrade). Must rebuild from current-version primary. |
| 36 | `cache lookup failed for (relation\|type\|function\|operator)` | `rebasebackup` | 300s | System catalog corruption. No partial fix — needs full rebuild. |
| 37 | `could not open tablespace directory` | `rebasebackup` | 120s | Tablespace path mismatch after pg_rewind from a node with different mounts. |
| 38 | `LOG:.*consistent recovery state reached` | `event` | 0 | Recovery complete. Good signal — reset error counters. |
| 39 | `LOG:.*redo done at` | `event` | 0 | WAL replay finished. Informational. |

### Category 8: Streaming Replication Health

| # | Pattern | Action | Cooldown | Why |
|---|---------|--------|----------|-----|
| 40 | `LOG:.*started streaming WAL from primary` | `event` | 0 | Replication connected. Good signal — clear WAL receiver alerts. |
| 41 | `FATAL:.*could not connect to the primary server` | `event` | 30s | Primary unreachable from replica. Failover sidecar handles this via lease logic. |
| 42 | `LOG:.*replication terminated by primary server` | `event` | 60s | Primary disconnected us. Could be pg_ctl stop, config reload, or network. |
| 43 | `FATAL:.*number of requested standby connections exceeds max_wal_senders` | `event` | 120s | Too many replicas. Need to increase max_wal_senders. |

### Category 9: Archive & Backup

| # | Pattern | Action | Cooldown | Why |
|---|---------|--------|----------|-----|
| 44 | `LOG:.*archive command failed with exit code` | `event` | 60s | WAL archiving broken. Backup gap forming. |
| 45 | `WARNING:.*archive_command .* took .* seconds` | `event` | 300s | Slow archiving. Destination might be throttling. |
| 46 | `LOG:.*restored log file .* from archive` | `event` | 0 | Archive restore working. Good signal during PITR. |

## Actions reference

| Action | What it does | Implemented by |
|---|---|---|
| `restart` | `pg_ctl stop -m fast` — wrapper loop handles recovery on restart | Sidecar exec into postgres container |
| `rewind` | Create `standby.signal` + stop PG — wrapper runs `pg_swarm_recover()` with pg_rewind | Existing `rewindOrReinit()` in `monitor.go` |
| `rebasebackup` | Write `.pg-swarm-needs-basebackup` marker + stop PG — wrapper nukes PGDATA and pg_basebackup | Sidecar exec + marker file |
| `event` | Report to central via gRPC stream — appears in dashboard event log | Existing event reporting |
| `exec` | Run arbitrary command in postgres container via K8s exec | Sidecar exec (same as `execStandbyConversion`) |

## Action mutex

Only one destructive action (`restart`, `rewind`, `rebasebackup`, `exec`) can run at a time. This prevents conflicts like a resync and a rebuild both manipulating PGDATA simultaneously.

**Implementation**: a single `sync.Mutex` in the log watcher (or sidecar-level `actionMu`).

```go
type LogWatcher struct {
    actionMu    sync.Mutex
    actionRunning string          // name of rule currently executing, "" if idle
    cooldowns   map[string]time.Time
    // ...
}
```

**Behavior when a rule fires while another action is in progress**:

| Incoming action | Current action | Result |
|---|---|---|
| `event` | any | **Always runs** — events are non-destructive, skip the mutex |
| `restart` | `restart` | **Drop** — same action already in progress |
| `rebuild` | `restart` | **Queued** — rebuild supersedes restart (higher severity). Runs after current action completes |
| `restart` | `rebuild` | **Drop** — rebuild already covers restart |
| `resync` | `rebuild` | **Drop** — rebuild is more thorough than resync |
| `rebuild` | `resync` | **Queued** — rebuild supersedes resync |
| any destructive | same | **Drop** — duplicate |

**Severity ordering**: `rebuild > resync > restart > exec`. A higher-severity action queued during a lower-severity one will run next. A lower-severity action is dropped if a higher one is already running or queued.

**At most one queued action** — if a third rule fires while an action is running and one is already queued, the higher-severity one wins, the other is dropped. This bounds the queue to 1 and prevents action storms.

**Logging**: every drop/queue/supersede decision is logged and emitted as an event:
```
pg-swarm: rule 'slot-invalidated' fired (rebuild) — queued behind running action 'timeline-not-in-history' (resync)
pg-swarm: rule 'wal-read-error' fired (restart) — dropped, rebuild already queued
```

## Cooldown & deduplication

- Each rule has a `cooldown` duration. After firing, the rule is suppressed for that duration.
- A fired rule emits an **event** to central regardless of action type (for dashboard visibility).
- Rules with `action: event` are report-only — no PG disruption, no mutex needed.
- Rules with destructive actions (`restart`, `rewind`, `rebasebackup`) have longer cooldowns to prevent action storms.
- Cooldown is checked **before** the mutex — a rule within its cooldown window never contends for the lock.

## What this replaces vs complements

| Current mechanism | Log agent role |
|---|---|
| Wrapper crash-loop breaker (3 crashes) | **Complement**: agent catches errors on first occurrence; crash-loop breaker is the last-resort safety net |
| Sidecar `checkWalReceiver()` (periodic) | **Replace for timeline errors**: agent catches log errors instantly instead of waiting for next tick + grace period |
| Sidecar `hasTimelineDivergence()` | **Complement**: the sidecar check still runs for cases where PG doesn't log (e.g., silent streaming stall) |
| Wrapper WAL cleanup | **Complement**: WAL cleanup prevents errors; agent catches them if cleanup missed a case |
| Wrapper final guard | **Complement**: guard catches missing files before PG starts; agent catches corruption PG finds at runtime |

## Central management

### How rules flow

```
Dashboard UI → Central REST API → Store (SQLite) → ClusterConfig protobuf
    → gRPC stream → Satellite → Operator → Sidecar (env/ConfigMap)
```

Rules are a **profile-level setting** with optional per-cluster overrides, following the same pattern as `pg_params`, `hba_rules`, and backup schedules.

### Protobuf schema

Add to the existing `ClusterConfig` message in `api/proto/v1/config.proto`:

```protobuf
message LogRule {
  string name = 1;
  string pattern = 2;           // regex matched against each PG log line
  string severity = 3;          // critical, error, warning, info
  string action = 4;            // restart, rewind, rebasebackup, event, exec
  string exec_command = 5;      // only used when action=exec
  int32  cooldown_seconds = 6;  // suppress re-fires for this duration
  bool   enabled = 7;           // toggle without deleting
}

// In ClusterConfig:
message ClusterConfig {
  // ... existing fields ...
  repeated LogRule log_rules = N;  // next available field number
}
```

### Where rules are stored

Central's SQLite store, in the **cluster_profiles** table (JSON column or normalized table). Same as `pg_params` — profile-level defaults that get merged into `ClusterConfig` at push time.

```
profiles.log_rules (JSON)       ← default rules for all clusters under this profile
clusters.log_rule_overrides     ← per-cluster additions/disables (optional)
```

The merge logic: start with profile rules, then apply cluster overrides (add new rules, disable existing by name, override cooldowns/actions).

### Built-in rules vs custom rules

Two tiers:

1. **Built-in rules** (rules 1-41 from the catalog above) — shipped as defaults in every new profile. Users can disable but not delete them. Central seeds these on profile creation. They get version-stamped so upgrades can add new built-in rules without overwriting user customizations.

2. **Custom rules** — user-defined via the dashboard. Full control over pattern, action, cooldown. Central validates the regex before saving (compile with Go's `regexp.Compile` on the API handler).

### Dashboard UI

**Profile settings page** → new "Log Rules" tab:

| Enabled | Name | Pattern | Severity | Action | Cooldown | |
|---|---|---|---|---|---|---|
| ✓ | stale-wal-recovery | `invalid record length at .* expected at least \d+, got 0` | critical | restart | 60s | Edit / Disable |
| ✓ | timeline-mismatch | `requested starting point .* is not in this server's history` | critical | rewind | 120s | Edit / Disable |
| ✗ | custom-deadlock | `deadlock detected` | warning | event | 30s | Edit / Delete |
| | | | | | | **+ Add Rule** |

- Toggle switch to enable/disable
- Edit opens inline form with pattern, action dropdown, cooldown input
- "Test Pattern" button — paste a sample log line, see if it matches
- Built-in rules show a lock icon (can disable but not delete)
- Changes push immediately via the existing config reconciliation loop

### REST API endpoints

```
GET    /api/v1/profiles/:id/log-rules          List rules for profile
PUT    /api/v1/profiles/:id/log-rules          Replace all rules
POST   /api/v1/profiles/:id/log-rules/validate Validate a regex pattern
GET    /api/v1/clusters/:id/log-rules          Effective rules (profile + overrides merged)
PUT    /api/v1/clusters/:id/log-rules          Set cluster-level overrides
```

### How the sidecar receives rules

The operator serializes `log_rules` from `ClusterConfig` into the failover sidecar's environment as a JSON-encoded env var:

```go
{Name: "LOG_RULES", Value: logRulesJSON}
```

Or, for large rule sets, as a ConfigMap key mounted into the sidecar. The sidecar watches the mounted file for changes (inotify or periodic stat) and hot-reloads rules without restart.

The sidecar compiles regexps on startup/reload, validates them, and logs any invalid patterns (falling back to the last valid set).

### Audit trail

Every rule fire produces an event sent to central:

```json
{
  "type": "log_rule_fired",
  "rule_name": "stale-wal-recovery",
  "matched_line": "PANIC: invalid record length at 0/C0000A0...",
  "action_taken": "restart",
  "pod_name": "dev-1",
  "cluster_name": "dev",
  "timestamp": "2026-03-19T14:23:01Z"
}
```

Visible in the dashboard's Events tab for the cluster. Enables tracking which rules fire most often, whether actions succeeded, and when to tune cooldowns.

## Implementation priority

**Phase 1** (high impact, low effort): Rules 1-9 (WAL/checkpoint/timeline errors) with hardcoded rules in the sidecar. No central management yet — just a JSON env var with the default rules. This is the exact set of errors causing the instability we've been debugging all session.

**Phase 2** (central management): Protobuf schema, REST API, profile storage, dashboard rule editor. Makes rules configurable without code changes.

**Phase 3** (observability): Rules 10-15, 16-21, 29-34. Corruption, resource exhaustion, recovery events. Mostly `event` actions for dashboard visibility.

**Phase 4** (completeness): Rules 22-28, 35-41. Replication health, auth, archive. Polish for production readiness.
