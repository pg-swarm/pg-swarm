# Cluster Recovery Agent

Centralized, cluster-level state reconciliation combined with a learning engine. Replaces the current distributed decision-making (each sidecar decides independently) with a single brain in central that has a global view of every pod.

## Why

The current failover system has each pod's sidecar making local decisions based on partial information:
- The sidecar knows its own PG state but not other pods'
- The wrapper knows about marker files but not about the lease
- The logwatcher fires rules but doesn't know if the cluster can handle the action
- Compound failures (PGDATA deleted + replica crash) cause cascading deadlocks because no component sees the full picture

The fix: **events flow UP to central, decisions flow DOWN to pods.**

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                          CENTRAL                             │
│                                                              │
│  ┌───────────────────────────────────────────────────────┐  │
│  │              Cluster State Machine                     │  │
│  │                                                        │  │
│  │  For each cluster:                                     │  │
│  │    1. OBSERVE: aggregate all pod events into a         │  │
│  │       single cluster-level state snapshot              │  │
│  │    2. ANALYZE: classify the situation                  │  │
│  │       (normal, degraded, failover, deadlock)           │  │
│  │    3. DECIDE: compute the target state                 │  │
│  │       (which pod should be primary, who rebuilds)      │  │
│  │    4. ACT: push specific commands to each pod          │  │
│  │    5. LEARN: track outcomes, tune future decisions     │  │
│  │                                                        │  │
│  └───────────────────────────────────────────────────────┘  │
│                                                              │
│  ┌──────────────┐  ┌──────────────┐  ┌────────────────┐    │
│  │ Event Store   │  │ Outcome Store│  │ Rule Optimizer  │    │
│  │ (per-cluster  │  │ (did the     │  │ (tune rules     │    │
│  │  event log)   │  │  action      │  │  based on       │    │
│  │               │  │  work?)      │  │  outcomes)      │    │
│  └──────────────┘  └──────────────┘  └────────────────┘    │
│                                                              │
│            existing gRPC bidi stream                         │
│       events UP ↑                    ↓ commands DOWN         │
└─────────────────────────────────────────────────────────────┘
                    │                    │
                    │   per satellite    │
                    │                    │
┌─────────────────────────────────────────────────────────────┐
│                        SATELLITE                             │
│                                                              │
│  Routes commands to the correct pod's sidecar                │
│                                                              │
└─────────────────────────────────────────────────────────────┘
          │              │              │
     ┌────┴────┐    ┌────┴────┐    ┌────┴────┐
     │  Pod 0  │    │  Pod 1  │    │  Pod 2  │
     │         │    │         │    │         │
     │ Sidecar │    │ Sidecar │    │ Sidecar │
     │ (events │    │ (events │    │ (events │
     │  UP,    │    │  UP,    │    │  UP,    │
     │  exec   │    │  exec   │    │  exec   │
     │  DOWN)  │    │  DOWN)  │    │  DOWN)  │
     │         │    │         │    │         │
     │ Wrapper │    │ Wrapper │    │ Wrapper │
     │ (start  │    │ (start  │    │ (start  │
     │  PG)    │    │  PG)    │    │  PG)    │
     └─────────┘    └─────────┘    └─────────┘
```

## Events (UP to Central)

Events are **cluster-level**, not per-pod. Each event includes the pod identity so central can build a complete picture.

### Event Types

| Event | Source | Payload | Meaning |
|-------|--------|---------|---------|
| `pod_state` | Sidecar | pod, pg_running, pg_role, pg_ready, replication_lag | Periodic heartbeat: what PG is doing on this pod |
| `pg_down` | Sidecar | pod, error, down_count | PG connection failed |
| `pgdata_missing` | Sidecar | pod | PG_VERSION file absent (PGDATA deleted/corrupt) |
| `wal_receiver_down` | Sidecar | pod, duration, replay_lsn | WAL streaming stopped |
| `timeline_divergence` | Sidecar | pod, local_tli, primary_tli | Timeline mismatch detected |
| `log_match` | LogWatcher | pod, rule_name, pattern, line | Log pattern matched a recovery rule |
| `recovery_started` | Wrapper/Sidecar | pod, action (rewind/basebackup/restart) | Recovery action initiated |
| `recovery_completed` | Wrapper/Sidecar | pod, action, success, duration | Recovery action finished |
| `lease_acquired` | Sidecar | pod | This pod now holds the leader lease |
| `lease_expired` | Sidecar | pod, old_holder | Leader lease expired |
| `marker_written` | Sidecar/LogWatcher | pod, marker_name, reason | A marker file was created |

### Proto Definition

```protobuf
message ClusterEvent {
    string cluster_name = 1;
    string namespace     = 2;
    string pod_name      = 3;
    string event_type    = 4;   // from table above
    string severity      = 5;   // info, warning, error, critical
    map<string, string> data = 6;  // event-specific key-value pairs
    google.protobuf.Timestamp timestamp = 7;
}
```

Added to `SatelliteMessage` as a new oneof field. Replaces the current scattered health reports, event reports, and switchover progress with a unified event stream.

## Commands (DOWN to Pods)

Central computes the target state and pushes **specific, per-pod commands**. The sidecar executes them without second-guessing.

### Command Types

| Command | Target | Effect | When Central Sends It |
|---------|--------|--------|----------------------|
| `promote` | Sidecar | pg_promote() + label primary | Central decides this pod should be primary |
| `demote` | Sidecar | fence + standby.signal + stop PG | Central decides this pod should be replica |
| `rebuild` | Sidecar | write basebackup marker + stop PG | Central decides this pod needs fresh PGDATA |
| `rewind` | Sidecar | standby.signal + stop PG | Central decides this pod needs timeline sync |
| `fence` | Sidecar | SET default_transaction_read_only = on | Central decides this pod should stop writes |
| `unfence` | Sidecar | SET default_transaction_read_only = off | Central decides this pod can resume writes |
| `wait` | Sidecar | No-op, suppress local decisions | Central is handling the situation, pod should not act independently |
| `label` | Sidecar | Set pg-swarm.io/role label | Central controls routing |
| `remove_marker` | Sidecar | Delete basebackup marker + standby.signal | Central breaks a deadlock |

### Proto Definition

```protobuf
message RecoveryCommand {
    string cluster_name = 1;
    string namespace     = 2;
    string pod_name      = 3;  // target pod
    string command        = 4;  // from table above
    string reason         = 5;  // human-readable explanation
    string operation_id   = 6;  // for tracking outcomes
    map<string, string> params = 7;  // command-specific parameters
}
```

Added to `CentralMessage` (central → satellite) or routed via the satellite's sidecar stream.

## Cluster State Machine (Central)

Central maintains a state machine per cluster. On every event, it recomputes the desired state and emits commands.

### Cluster States

| State | Meaning | Transitions |
|-------|---------|-------------|
| `healthy` | 1 primary + N replicas, all streaming, all ready | → `degraded` (pod unhealthy), → `failover` (primary down) |
| `degraded` | Primary up but some replicas unhealthy | → `healthy` (replicas recover), → `failover` (primary also fails) |
| `failover` | Primary down, deciding which replica to promote | → `promoting` (decision made) |
| `promoting` | Sent promote command, waiting for confirmation | → `healthy` (promote succeeded), → `deadlocked` (promote failed + all replicas stuck) |
| `recovering` | Primary changed, replicas rebuilding/rewinding | → `healthy` (all replicas recovered) |
| `deadlocked` | No working primary, all pods stuck | → `emergency` (deadlock breaker fires) |
| `emergency` | Forcing a pod to start as primary | → `recovering` (emergency primary up) |

### State Transition Logic

```
On event(pod_state):
  Update cluster snapshot

  if snapshot.ready_primaries == 0:
    if state == healthy or degraded:
      → failover
      Pick best replica (most recent LSN, least lag, ready)
      Send: promote(best_replica)
      Send: wait(all_other_pods)  // suppress local decisions
      → promoting

  if state == promoting AND event.type == "recovery_completed" AND success:
    → recovering
    For each non-primary pod:
      if timeline_diverged: send rewind(pod)
      elif wal_gap: send rebuild(pod)
      else: send wait(pod)  // will auto-reconnect

  if state == promoting AND timeout(30s):
    → deadlocked
    Send: remove_marker(best_available_pod)
    Send: promote(best_available_pod)
    → emergency

  if state == recovering AND all_replicas_ready:
    → healthy

  if state == deadlocked AND timeout(30s):
    → emergency
    Pick pod with most data (if known), or any pod
    Send: remove_marker(pod) + promote(pod)
```

### Best Replica Selection

When choosing which replica to promote, central ranks candidates by:

1. **Has data** (PG was recently connected) — hard requirement
2. **Most recent replay LSN** — closest to the old primary's state
3. **Lowest replication lag** — fewest transactions behind
4. **Ready container** — PG is currently running
5. **Not in basebackup loop** — wrapper is not stuck

This information is available from the `pod_state` events that each sidecar streams up.

## Sidecar Changes

With central making decisions, the sidecar simplifies dramatically:

### Current Sidecar (Complex)

```
tick():
  detect PGDATA deletion → write marker, label
  connect to PG → handlePrimary or handleReplica
    handlePrimary: lease renewal, crash-loop detection, split-brain
    handleReplica: WAL receiver, timeline divergence, reachability, promotion
  deadlock breaker: zero-primary detection
```

### New Sidecar (Simple)

```
tick():
  collect local state (PG up/down, role, lag, PGDATA present)
  send pod_state event to satellite → central

  if pending_command from central:
    execute command (promote, demote, rebuild, rewind, fence, wait)
    send recovery_completed event

  if no command received for 60s AND zero primaries:
    fallback: local deadlock breaker (safety net if central is down)
```

The sidecar becomes an **event emitter + command executor**. All decision logic moves to central. The local deadlock breaker stays as a safety net for when central is unreachable.

### Backward Compatibility

The transition can be gradual:
1. **Phase 0** (current): Sidecar makes all decisions locally. Central is passive.
2. **Phase 1**: Sidecar sends events to central. Central logs them and computes recommendations but doesn't push commands. Dashboard shows "what central would have done."
3. **Phase 2**: Central pushes commands for failover decisions only. Sidecar's local failover logic is disabled when central is connected. Falls back to local logic on disconnect.
4. **Phase 3**: Central manages all recovery (rebuild, rewind, timeline). Sidecar is purely an executor.

## Learning Engine

The learning engine from the earlier AGENT.md design integrates naturally:

### Outcome Tracking

Every command central sends has an `operation_id`. The sidecar reports back with `recovery_completed(operation_id, success, duration)`. Central tracks:

```sql
CREATE TABLE recovery_outcomes (
    id              UUID PRIMARY KEY,
    cluster_name    TEXT NOT NULL,
    satellite_id    UUID NOT NULL,
    pod_name        TEXT NOT NULL,
    command         TEXT NOT NULL,
    reason          TEXT NOT NULL,
    rule_name       TEXT,           -- if triggered by a log rule
    operation_id    TEXT NOT NULL,
    cluster_state   TEXT NOT NULL,  -- cluster state when command was sent
    success         BOOL,
    resolution_ms   INT,
    created_at      TIMESTAMPTZ DEFAULT now()
);
```

### Rule Tuning

Central correlates log_match events with outcomes:
- Rule X fires → central sends rebuild → took 120s → success. Record.
- Same rule, different cluster → central sends rewind instead → took 15s → success. Faster.
- Next time Rule X fires → central prefers rewind over rebuild.

### Cross-Cluster Learning

When a new cluster is created:
- Central queries outcomes for clusters with the same profile/PG version
- Pre-loads the state machine with known-good response strategies
- Skips the "trial and error" phase that other clusters went through

## Proto Changes Summary

### New Messages

```protobuf
// In backup.proto or a new agent.proto:

message ClusterEvent {
    string cluster_name = 1;
    string namespace     = 2;
    string pod_name      = 3;
    string event_type    = 4;
    string severity      = 5;
    map<string, string> data = 6;
    google.protobuf.Timestamp timestamp = 7;
}

message RecoveryCommand {
    string cluster_name = 1;
    string namespace     = 2;
    string pod_name      = 3;
    string command        = 4;
    string reason         = 5;
    string operation_id   = 6;
    map<string, string> params = 7;
}
```

### Integration into Existing Streams

```protobuf
// SatelliteMessage (satellite → central)
oneof payload {
    // ... existing fields ...
    ClusterEvent cluster_event = 13;  // reserved for agent
}

// CentralMessage (central → satellite)
oneof payload {
    // ... existing fields ...
    RecoveryCommand recovery_command = 21;  // reserved for agent
}
```

## Implementation Phases

### Phase 1: Event Streaming (foundation)

- Add `ClusterEvent` proto message
- Sidecar: emit events for all state changes (already happening via health reports — formalize it)
- Central: store events in a new `cluster_events` table
- Dashboard: "Cluster Timeline" view showing events across all pods

**No behavior change.** Pure observability.

### Phase 2: Centralized Failover

- Add `RecoveryCommand` proto message
- Central: implement cluster state machine for failover only (promote/demote)
- Sidecar: accept promote/demote commands from central
- Sidecar: disable local failover logic when central is connected
- Safety net: fall back to local logic if no central heartbeat for 60s

**Replaces**: handleReplica's promotion path, handlePrimary's split-brain detection.

### Phase 3: Centralized Recovery

- Central: extend state machine for rebuild/rewind decisions
- Central: "best replica" selection using LSN data from events
- Sidecar: accept rebuild/rewind/remove_marker commands
- Deadlock detection and resolution in central (not per-pod)

**Replaces**: checkWalReceiver, doRewind, doReBasebackup, deadlock breaker.

### Phase 4: Learning

- Outcome tracking (operation_id → success/failure/duration)
- Rule effectiveness analysis
- Cross-cluster strategy sharing
- Anomaly detection from event patterns

**New capability**: Central gets smarter over time.

## File Impact

| File | Phase | Change |
|------|-------|--------|
| `api/proto/v1/agent.proto` | 1 | New: ClusterEvent, RecoveryCommand messages |
| `api/proto/v1/config.proto` | 1 | Add event/command fields to SatelliteMessage/CentralMessage |
| `internal/central/server/grpc.go` | 1 | Handle ClusterEvent in stream handler |
| `internal/central/store/` | 1 | New: cluster_events table + store methods |
| `internal/central/agent/` | 2 | New: cluster state machine, command dispatcher |
| `internal/failover/monitor.go` | 2 | Accept commands from satellite stream, disable local failover when managed |
| `internal/satellite/stream/connector.go` | 2 | Route RecoveryCommand to correct pod's sidecar |
| `internal/central/agent/learner.go` | 4 | New: outcome tracking, rule optimization |
| `dashboard/src/pages/ClusterDetail.jsx` | 1 | New: Cluster Timeline tab |

## Local Rule Cache

The sidecar maintains a **local cache of event-based rules** pushed down from central. This enables autonomous decision-making when central is unreachable.

### How It Works

```
Central                              Sidecar
  │                                    │
  │  ClusterConfig push (includes      │
  │  recovery rules + event rules)     │
  ├───────────────────────────────────→│
  │                                    ├─ Write to local cache
  │                                    │  /etc/recovery-rules/rules.json (ConfigMap)
  │                                    │  + event-rules.json (new, from stream)
  │                                    │
  │  Rule update (central learned      │
  │  a better strategy)                │
  ├───────────────────────────────────→│
  │                                    ├─ Update local cache
  │                                    │
  │         ╳ network failure ╳        │
  │                                    │
  │                                    ├─ Central unreachable for 60s
  │                                    ├─ Switch to LOCAL MODE
  │                                    ├─ Use cached rules for all decisions
  │                                    ├─ Log: "operating autonomously"
  │                                    │
  │         ╳ network restored ╳       │
  │                                    │
  │  Reconnect                         │
  ├←──────────────────────────────────→│
  │                                    ├─ Switch back to MANAGED MODE
  │                                    ├─ Upload events that occurred offline
  │                                    ├─ Central re-syncs full cluster state
```

### Rule Cache Structure

```json
{
  "version": 42,
  "updated_at": "2026-03-26T12:00:00Z",
  "mode": "managed",

  "log_rules": [
    {"name": "checkpoint-missing", "pattern": "...", "action": "rebasebackup", ...}
  ],

  "event_rules": [
    {
      "name": "primary-down-failover",
      "trigger": "pg_down",
      "conditions": {
        "pod_role": "primary",
        "consecutive_ticks": 3,
        "lease_expired": true
      },
      "action": "promote_best_replica",
      "priority": 1
    },
    {
      "name": "zero-primary-deadlock",
      "trigger": "zero_ready_primaries",
      "conditions": {
        "consecutive_ticks": 5,
        "all_pods_pg_down": true
      },
      "action": "force_promote_self",
      "priority": 0
    },
    {
      "name": "timeline-divergence-rewind",
      "trigger": "timeline_divergence",
      "conditions": {
        "primary_reachable": true
      },
      "action": "rewind",
      "priority": 2
    }
  ],

  "timeouts": {
    "primary_unreachable_threshold": 3,
    "zero_primary_threshold": 5,
    "wal_receiver_grace_seconds": 30,
    "central_disconnect_fallback_seconds": 60
  }
}
```

### Three Operating Modes

| Mode | When | Decision Maker | Rule Source |
|------|------|---------------|------------|
| **Managed** | Central connected, heartbeat recent | Central | Central pushes commands |
| **Autonomous** | Central disconnected > 60s | Local sidecar | Cached rules from last sync |
| **Degraded** | No cached rules + no central | Local sidecar | Hardcoded defaults (current behavior) |

In **managed mode**, the sidecar suppresses local decision-making and only executes commands from central. It still emits events and monitors local PG state.

In **autonomous mode**, the sidecar uses the cached event rules to make local decisions. These rules encode the same logic that central would apply, but with only local visibility. The rules include the learnings from central's analysis — so a sidecar operating autonomously uses strategies proven across the fleet, not just hardcoded defaults.

In **degraded mode** (fresh sidecar, never connected to central), the sidecar falls back to the current hardcoded logic. This is the worst case and matches today's behavior.

### Cache Sync Protocol

1. **Initial sync**: When the satellite connects to central, the full rule set is pushed as part of the cluster config (extends the existing `ClusterConfig.recovery_rules` field).
2. **Incremental updates**: When central's learning engine updates a rule, it pushes a `RuleCacheUpdate` message down the stream. The sidecar applies it atomically.
3. **Version tracking**: Each cache has a version number. On reconnect, the sidecar sends its cache version. Central sends a diff if outdated, or "up to date" if current.
4. **Persistence**: The cache is written to the data volume (`/var/lib/postgresql/data/.pg-swarm-rules-cache.json`) so it survives container restarts.

### Proto Addition

```protobuf
message RuleCacheUpdate {
    int64  version   = 1;
    string rules_json = 2;  // full cache JSON
    bool   full_sync  = 3;  // true = replace all, false = merge
}
```

Added to `CentralMessage`:
```protobuf
oneof payload {
    // ... existing fields ...
    RecoveryCommand recovery_command = 21;
    RuleCacheUpdate rule_cache_update = 22;
}
```

## Design Principles

1. **Central decides, sidecar executes — when connected.** When disconnected, the sidecar uses cached rules that encode central's learnings. The system is never dumber than the last time it talked to central.

2. **Events are cluster-level.** Central sees all pods' events simultaneously and correlates them. "Pod 0 PG down" + "Pod 1 WAL receiver down" + "Pod 2 timeline divergence" = "Primary failure, need coordinated failover."

3. **Graceful degradation, not cliff edges.** Managed → autonomous (cached rules) → degraded (hardcoded defaults). Each step is less optimal but still functional. No mode transition causes a failure.

4. **One reconciliation loop when possible.** Central's state machine runs once per cluster, not once per pod. Decisions are consistent and coordinated. Autonomous mode is the fallback, not the design.

5. **Outcome-driven.** Every action has a measurable result. The system tracks what works and prefers strategies with higher success rates. These learnings are baked into the rule cache so autonomous sidecars benefit from fleet-wide experience.
