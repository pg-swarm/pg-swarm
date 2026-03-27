# Failover State Machine

Complete reference for the pg-swarm failover system: every component state, marker file, signal, label, lease operation, and failure scenario.

## Components

Each PostgreSQL pod has three concurrent actors:

| Component | Container | Role |
|-----------|-----------|------|
| **Wrapper** (`pg-wrapper.sh`) | `postgres` | Keeps container alive, starts/restarts PG, runs recovery (rewind, basebackup) |
| **Sidecar** (`monitor.go`) | `failover-sidecar` | Manages leader lease, detects failures, promotes replicas, labels pods |
| **Log Watcher** (`logwatcher.go`) | `failover-sidecar` | Matches PG log patterns, triggers recovery actions |

They coordinate through **marker files** on a shared volume, **K8s labels**, and a **K8s Lease**.

---

## Marker Files

All markers live on the **PVC volume root** (`/var/lib/postgresql/data/`), outside `$PGDATA` (`/var/lib/postgresql/data/pgdata/`), so they survive PGDATA deletion.

| File | Written By | Read By | Deleted By | Effect |
|------|-----------|---------|-----------|--------|
| `.pg-swarm-needs-basebackup` | Sidecar (PGDATA deletion, doReBasebackup), LogWatcher (rebasebackup rules), Wrapper (recovery fallbacks) | Wrapper (main loop, line 195) | Wrapper (on successful basebackup), Sidecar (deadlock breaker) | Wrapper enters `pg_swarm_rebasebackup` loop: deletes PGDATA, retries `pg_basebackup` from primary up to 12 times (60s) |
| `.pg-swarm-initialized` | Wrapper (after first successful PG start) | Wrapper (PGDATA guards) | Wrapper (before re-basebackup on primary PGDATA loss) | Distinguishes first boot from runtime PGDATA loss. If present + PGDATA gone = runtime loss, don't initdb |
| `pgdata/standby.signal` | Sidecar (execStandbyConversion for demote/rewind), Wrapper (pg_swarm_recover), `pg_basebackup -R` | Wrapper (timeline recovery, WAL level check), PostgreSQL (startup mode) | Sidecar (deadlock breaker), PostgreSQL (after pg_promote) | PG starts in recovery/standby mode. Removed by PG after successful promotion. |
| `pgdata/PG_VERSION` | PostgreSQL (initdb, pg_basebackup) | Sidecar (tick, PGDATA deletion check), Wrapper (corrupt PGDATA guard) | N/A (deleted with PGDATA) | Indicates PGDATA is initialized. Absence = PGDATA corrupt or deleted |

### Marker Interaction Problems

**Problem 1: Marker written but reader already past the check**
- The wrapper's `pg_swarm_rebasebackup` runs a 60s retry loop. If the sidecar removes the marker during the loop, the wrapper doesn't notice until the function returns and the main loop re-checks.
- **Mitigation**: The retry loop checks for marker existence on each iteration. If removed, aborts immediately.

**Problem 2: Multiple writers**
- Both the sidecar (PGDATA deletion detector) and the logwatcher (rebasebackup rule) can write the marker.
- Both the sidecar (deadlock breaker) and the wrapper (successful basebackup) can delete it.
- **Contract**: Writing is idempotent (touch/overwrite). Deletion is a signal to abort recovery.

**Problem 3: Marker on the wrong pod**
- After a promotion + timeline change, replicas detect divergence → sidecar calls `doReBasebackup` → writes marker on the replica → wrapper enters basebackup loop.
- If the new primary ALSO gets a marker (from logwatcher matching a log pattern during promotion transition), all pods are stuck.
- **Mitigation**: Deadlock breaker (see below).

---

## Labels

| Label | Values | Set By | When |
|-------|--------|--------|------|
| `pg-swarm.io/role` | `primary`, `replica` | Sidecar: `labelPod()`, `labelRemotePod()`, `clearPrimaryLabels()` | On lease acquired (primary), on demotion (replica), on PG down (replica, via wasPrimary), on promotion (clear others → set self), on deadlock break (primary) |
| `pg-swarm.io/cluster` | `<clusterName>` | K8s manifest (operator) | Pod creation (immutable) |

### Label Timing

The RW service selector is `pg-swarm.io/role=primary`. Traffic routing changes when labels change.

| Event | Label Change | Delay | Impact |
|-------|-------------|-------|--------|
| PG crash on primary | `primary → replica` (wasPrimary self-clear) | ≤ 5s (one tick) | RW service loses endpoint |
| Replica promotes | `replica → primary` (after pg_promote + clearPrimaryLabels) | ~100ms | RW service gains endpoint |
| Split-brain detected | `primary → replica` (fence + demote) | ≤ 5s (one tick) | Old primary stops serving |
| Deadlock breaker | `replica → primary` (force) | After 25s (5 ticks) | RW service gains endpoint |

---

## Lease

**Name**: `{clusterName}-leader`
**TTL**: 5 seconds (configurable via `HEALTH_CHECK_INTERVAL`)
**Holder**: Pod name of the current primary

| Operation | Who | When | Effect |
|-----------|-----|------|--------|
| **Renew** | Primary sidecar (`handlePrimary → acquireOrRenew`) | Every tick (5s) while PG is healthy | Keeps lease alive |
| **NOT renewed** | Primary sidecar | When PG is down (connection failure) | Lease expires after 5s → replicas can acquire |
| **NOT renewed** | Primary sidecar | When crash-loop detected (3+ fast crashes) | Intentional yield for failover |
| **Acquire** | Replica sidecar (`handleReplica → acquireOrRenew`) | After 3 ticks of primary unreachable + lease expired | Optimistic lock via resourceVersion |
| **Acquire** | Any sidecar (deadlock breaker) | After 5 ticks of zero primaries | Emergency recovery |

### Lease Expiry Logic

```
expired = (now > lease.RenewTime + lease.LeaseDurationSeconds)
```

The lease holder identity persists even after expiry. `acquireOrRenew` checks:
1. If `HolderIdentity == self` → **renew** (update RenewTime) → return true
2. If `HolderIdentity != self` AND **not expired** → return false (someone else holds it)
3. If `HolderIdentity != self` AND **expired** → **acquire** (update HolderIdentity + RenewTime) → return true

**Critical**: Step 1 renews even if the lease was expired. This means a pod that was down briefly can reclaim its own expired lease before another pod acquires it. This is a race condition.

---

## Sidecar State Machine

### State Variables

| Variable | Type | Initial | Meaning |
|----------|------|---------|---------|
| `wasConnected` | bool | false | Has ever successfully connected to local PG |
| `wasPrimary` | bool | false | Last successful connection was to a primary (not in recovery) |
| `localPGDownCount` | int | 0 | Consecutive ticks where local PG is unreachable |
| `consecutiveHealthyTicks` | int | 0 | Ticks since PG came back after crash-loop |
| `primaryUnreachableCount` | int | 0 | Consecutive ticks where primary RW service is unreachable (replicas only) |
| `zeroPrimaryCount` | int | 0 | Consecutive ticks with zero ready primaries in cluster |
| `walReceiverDownSince` | time | zero | When WAL receiver first became inactive |

### tick() Decision Tree

```
tick()
 │
 ├─ PGDATA DELETION CHECK (wasConnected + PG_VERSION missing)
 │   → write basebackup marker
 │   → if wasPrimary: label self replica
 │   → RETURN
 │
 ├─ CONNECT TO LOCAL PG
 │   ├─ ERROR 57P03 (starting up) → RETURN
 │   │
 │   ├─ ERROR (PG down)
 │   │   → localPGDownCount++
 │   │   → if wasPrimary: label self replica
 │   │   → ZERO-PRIMARY DEADLOCK CHECK
 │   │   │   if countReadyPrimaries() == 0:
 │   │   │     zeroPrimaryCount++
 │   │   │     if >= 5 AND (lease expired OR held by self):
 │   │   │       acquire lease
 │   │   │       REMOVE basebackup marker
 │   │   │       REMOVE standby.signal
 │   │   │       label self primary
 │   │   │   else:
 │   │   │     zeroPrimaryCount = 0
 │   │   → RETURN
 │   │
 │   └─ SUCCESS
 │       → wasConnected = true
 │       → wasPrimary = !pg_is_in_recovery()
 │       → if PRIMARY: handlePrimary()
 │       → if REPLICA: handleReplica()
```

### handlePrimary() Decision Tree

```
handlePrimary()
 │
 ├─ zeroPrimaryCount = 0
 ├─ consecutiveHealthyTicks++
 │
 ├─ CRASH-LOOP CHECK
 │   if downCount >= 3 AND healthyTicks < 3:
 │     → RETURN (skip lease renewal → lease expires → replica promotes)
 │
 ├─ ACQUIRE OR RENEW LEASE
 │   ├─ ERROR → fence PG (safety) → RETURN
 │   ├─ ACQUIRED (we hold lease)
 │   │   → unfence if fenced
 │   │   → label self primary
 │   └─ NOT ACQUIRED (someone else holds it)
 │       → SPLIT-BRAIN
 │       → fence PG (block writes)
 │       → demote (standby.signal + stop PG)
 │       → label self replica
```

### handleReplica() Decision Tree

```
handleReplica()
 │
 ├─ label self replica
 │
 ├─ ZERO-PRIMARY CHECK (PG is running as replica)
 │   if countReadyPrimaries() == 0:
 │     zeroPrimaryCount++
 │     if >= 5:
 │       acquire lease → pg_promote() → label self primary
 │       RETURN
 │   else:
 │     zeroPrimaryCount = 0
 │
 ├─ CHECK WAL RECEIVER
 │   → checkWalReceiver() (see below)
 │
 ├─ PRIMARY REACHABLE?
 │   ├─ YES → primaryUnreachableCount = 0 → RETURN
 │   └─ NO → primaryUnreachableCount++
 │
 ├─ if unreachableCount < 3 → RETURN
 │
 ├─ LEASE EXPIRED?
 │   ├─ NO → RETURN (possible network partition)
 │   └─ YES → attempt failover
 │
 └─ FAILOVER
     → acquire lease
     → pg_promote()
     → clearPrimaryLabels() (all other pods → replica)
     → label self primary
```

### checkWalReceiver() Decision Tree

```
checkWalReceiver()
 │
 ├─ WAL receiver streaming? → reset timer → RETURN
 │
 ├─ Lease expired? → RETURN (failover will handle)
 │
 ├─ TIMELINE DIVERGENCE?
 │   ├─ YES + primary reachable → doRewind()
 │   ├─ YES + primary unreachable → RETURN (skip destructive)
 │   └─ NO → continue
 │
 ├─ GRACE PERIOD (30s)
 │   if first detection → start timer → RETURN
 │   if replay LSN advancing → reset timer → RETURN
 │   if < 30s → RETURN
 │
 ├─ PRIMARY REACHABLE?
 │   └─ NO → RETURN (skip destructive)
 │
 └─ RECOVERY
     if WAL gap → doReBasebackup() (marker + stop PG)
     else → doRewind() (standby.signal + stop PG)
```

---

## Wrapper State Machine

### Main Loop Decision Tree

```
while true:
 │
 ├─ CRASH-LOOP BREAKER (3+ fast crashes)
 │   ├─ replica → pg_basebackup
 │   └─ primary → pg_resetwal
 │
 ├─ TIMELINE RECOVERY (pg_swarm_recover)
 │   if standby.signal + PG_VERSION exist:
 │     compare timelines → pg_rewind or pg_basebackup
 │
 ├─ BASEBACKUP MARKER CHECK
 │   if .pg-swarm-needs-basebackup exists:
 │     → pg_swarm_rebasebackup() (12 retries, 60s)
 │     → aborts if sidecar removes marker mid-loop
 │     → continue
 │
 ├─ PRIMARY EMPTY PGDATA + SENTINEL
 │   if ordinal==0 AND PGDATA empty AND sentinel exists:
 │     → sleep 30s (yield for failover)
 │     → remove sentinel
 │     → pg_basebackup from new primary
 │     → continue
 │
 ├─ CORRUPT PGDATA (no PG_VERSION)
 │   ├─ replica → pg_basebackup
 │   ├─ primary + sentinel → sleep 30s → pg_basebackup
 │   └─ primary + no sentinel → clean up → initdb (first boot)
 │
 ├─ MISSING WAL SEGMENTS
 │   ├─ replica → pg_basebackup
 │   └─ primary → pg_resetwal
 │
 ├─ WAL_LEVEL=MINIMAL (replica only)
 │   → pg_basebackup
 │
 ├─ START PG
 │   docker-entrypoint.sh postgres
 │   write sentinel if first start
 │   wait for PG exit
 │
 ├─ K8S SHUTDOWN? → exit 0
 │
 ├─ FATAL ERROR SCAN (replica only)
 │   grep exit logs for unrecoverable patterns
 │   → pg_basebackup if matched
 │
 └─ CRASH TRACKING
     if PG ran < 30s → crash_count++
     else → crash_count = 0
     sleep 2
```

---

## Failure Scenarios — Complete Trace

### Scenario 1: Primary Pod Killed

```
T=0s:   Pod killed by K8s (or kubectl delete)
        Wrapper: receives SIGTERM → SHUTTING_DOWN=true → exit
        Sidecar: container terminated
        Lease: NOT renewed (sidecar gone)

T=5s:   Lease expires (last renewal was at T≤0)

T=5-15s: Replica sidecars: isPrimaryReachable() fails
         primaryUnreachableCount increments (1, 2, 3)

T=15s:  Replica sidecar: unreachableCount=3, lease expired
        → acquireOrRenew() → acquired
        → pg_promote()
        → clearPrimaryLabels() (old pod doesn't exist)
        → labelPod(primary)
        → NEW PRIMARY SERVING

T=15-30s: StatefulSet recreates the pod
          Wrapper: PGDATA intact on PVC → starts PG
          Sidecar: pg_is_in_recovery()=false → handlePrimary
          → acquireOrRenew() → NOT acquired (new primary holds lease)
          → SPLIT-BRAIN detected → fence + demote + label replica

T=30-60s: Wrapper: PG stopped by demote → timeline recovery
          → pg_rewind from new primary → restart as replica
```

**Markers written**: None
**Labels changed**: New primary → `primary`. Old pod (recreated) → `replica` via split-brain handler.
**Risk**: ~100ms split-brain label window during clearPrimaryLabels. Old pod is dead so harmless.

### Scenario 2: PGDATA Deleted on Primary

```
T=0s:   PGDATA deleted (rm -rf pgdata/*)
        PG crashes (files gone)

T=2s:   Wrapper: detects PG exit
        → loops back to top of main loop
        → BASEBACKUP MARKER? No (not yet written)
        → PRIMARY EMPTY PGDATA + SENTINEL? YES
        → "yielding lease for failover"
        → sleep 30s

T=5s:   Sidecar tick: can't connect to PG
        → localPGDownCount++
        → wasPrimary=true → labelPod(replica)
        → zeroPrimaryCount++ (0 ready primaries)
        Lease: NOT renewed → starts expiring

T=10s:  Lease expires

T=10-20s: Replica sidecars: isPrimaryReachable() fails
          primaryUnreachableCount increments

T=20s:  Replica sidecar: unreachableCount=3, lease expired
        → acquireOrRenew() → acquired
        → pg_promote()
        → clearPrimaryLabels()
        → labelPod(primary)
        → NEW PRIMARY SERVING

T=20-25s: OTHER replicas: WAL receiver down, detect timeline divergence
          → checkWalReceiver() → hasTimelineDivergence()=true
          → primary reachable? Checking new primary... YES
          → doRewind() → execStandbyConversion()
          → standby.signal + stop PG
          → wrapper: pg_swarm_recover → pg_rewind from new primary

T=32s:  Original primary wrapper: wakes from sleep 30s
        → removes sentinel
        → pg_swarm_rebasebackup() from new primary
        → if new primary is up: SUCCESS → starts as replica
        → if new primary is NOT up: RETRIES for 60s
```

**Critical path**: Between T=20 and T=25, the OTHER replicas may trigger `doReBasebackup` instead of `doRewind` if `hasWalGap()=true`. This writes the basebackup marker on them. If the new primary ALSO crashes during this window (e.g., logwatcher fires a rule), ALL pods end up with markers → DEADLOCK.

**Deadlock recovery**: Sidecar deadlock breaker at T=25s (5 ticks of zero ready primaries) → removes markers + standby.signal → labels self primary → wrapper starts PG → other pods recover.

### Scenario 3: PGDATA Deleted + Replica Crashes During Promotion

```
T=0s:   PGDATA deleted on primary
T=20s:  Replica A promotes (acquires lease)
T=21s:  Replica A's PG crashes during promotion (e.g., OOM, bug)
        → wasPrimary=true (was briefly primary) → labels self replica
        → Lease NOT renewed → expires at T=26s

T=22s:  Replica B: still streaming from old primary → WAL receiver down
        → checkWalReceiver → timeline divergence → doReBasebackup()
        → MARKER WRITTEN on Replica B → wrapper enters basebackup loop

T=23s:  Replica A: wrapper restarts PG
        → LogWatcher: may match timeline errors → rebasebackup rule fires
        → MARKER WRITTEN on Replica A → wrapper enters basebackup loop

T=25s:  Original primary: still in sleep/basebackup loop
        → MARKER WRITTEN (by PGDATA deletion detector at T=5s)

T=25s:  ALL PODS: marker set, PG down, basebackup loop, no RW endpoints
        → DEADLOCK

T=50s:  Sidecar deadlock breaker (25s since all pods stuck):
        zeroPrimaryCount=5
        → Lease expired → acquire
        → REMOVE marker + standby.signal
        → Label self primary
        → Wrapper: no marker → starts PG → primary up
        → Other pods: basebackup succeeds → recover as replicas
```

### Scenario 4: Network Partition (Primary Isolated)

```
T=0s:   Primary's network partitioned from replicas (but K8s API accessible)

T=0-15s: Primary sidecar: handlePrimary → acquireOrRenew → SUCCEEDS
         (K8s API accessible, lease renewed)
         Replicas: isPrimaryReachable() FAILS (TCP check fails)
         primaryUnreachableCount increments

T=15s:  Replicas: unreachableCount=3, but lease NOT expired
        (primary is still renewing)
        → "primary unreachable but lease still valid"
        → NO FAILOVER (correct — primary is alive, just partitioned from replicas)

T=∞:    Partition heals → replicas reconnect → WAL streaming resumes
```

**Key**: The lease prevents false failover during network partitions. Only when the primary can't renew (K8s API unreachable or PG down) does the lease expire.

### Scenario 5: Primary Fenced (K8s API Unreachable from Primary)

```
T=0s:   Primary can't reach K8s API (but PG and network to replicas work)

T=5s:   Primary sidecar: acquireOrRenew() → ERROR
        → doFence(conn) → ALTER SYSTEM SET default_transaction_read_only = on
        → PG accepts connections but rejects writes
        → Lease NOT renewed → expires

T=10s:  Lease expires

T=15s:  Replica: unreachableCount=3 (TCP to primary SUCCEEDS but...)
        Wait — isPrimaryReachable does TCP check. If network to primary works,
        this returns true → primaryUnreachableCount resets → NO FAILOVER

        The primary is fenced (read-only) but replicas see it as reachable.
        Writes fail but reads work. This is a degraded state.

T=∞:    K8s API connectivity restores
        → Primary: acquireOrRenew() succeeds → unfence → normal operation
```

**Gap**: Fenced primary is a degraded state with no automatic recovery other than K8s API restoration. Replicas don't know the primary is fenced.

### Scenario 6: Two Failures Overlap — Primary PGDATA Deleted + One Replica Also Crashes

```
T=0s:   PGDATA deleted on primary
T=1s:   Replica B crashes (unrelated — OOM, disk issue)

T=5s:   Primary sidecar: PG down → label replica → lease not renewed
T=5s:   Replica B sidecar: PG down → NOT wasPrimary → no label change
        (Replica B was a replica, so wasPrimary=false. Label stays replica.)

T=10s:  Lease expires. Only Replica A is healthy.

T=20s:  Replica A: unreachableCount=3, lease expired
        → acquireOrRenew → promote → clearPrimaryLabels → label primary
        → NEW PRIMARY (Replica A)

T=25s:  Replica B's wrapper: restarts PG
        → PG comes up as replica → streams from Replica A
        OR: timeline divergence → pg_rewind → stream from Replica A

T=32s:  Primary wrapper: sleep done → basebackup from Replica A → replica

Result: Replica A is primary. Others recover. Single-node primary for ~25s.
```

### Scenario 7: Cascading Timeline Divergence After Promotion

```
T=0s:   Primary dies
T=20s:  Replica A promotes (timeline N → N+1)
T=21s:  Replica B: WAL receiver down → checkWalReceiver
        → hasTimelineDivergence()? Checking...
        → localTimeline=N, primary timeline=N+1 → YES
        → isPrimaryReachable()? Replica A is up → YES
        → doRewind() → standby.signal + stop PG
        → Wrapper: pg_swarm_recover → pg_rewind from Replica A → restart

T=21s:  Replica C: same as Replica B → doRewind() → recover

T=22s:  Replica B: if pg_rewind fails AND hasWalGap()=true
        → doReBasebackup() → MARKER WRITTEN → wrapper basebackup loop
        → pg_basebackup from Replica A → success → recover

Result: All replicas converge to Replica A's timeline. Some via rewind, some via basebackup.
Risk: If Replica A crashes between T=20 and T=22 while others are mid-recovery → Scenario 3 (deadlock).
```

---

## Deadlock Breaker — Detailed Mechanism

### Trigger Conditions

The deadlock breaker fires when:
1. Local PG is **unreachable** (connection failure in `tick()`)
2. `countClusterPrimaries()` returns **0** (no pod with `role=primary` AND containers Ready)
3. This has persisted for **5 consecutive ticks** (25s)
4. The lease is **expired** OR **held by this pod**

### Actions

1. **Acquire lease** via `acquireOrRenew()` — prevents multiple pods from doing this simultaneously
2. **Remove `.pg-swarm-needs-basebackup`** — breaks the wrapper's basebackup retry loop
3. **Remove `standby.signal`** — PG will start as primary, not replica
4. **Label self `primary`** — RW service gets an endpoint

### Recovery After Deadlock Break

```
Winning pod:
  → Wrapper: no marker, no standby.signal
  → docker-entrypoint.sh starts PG
  → If PGDATA exists: normal primary start
  → If PGDATA empty: initdb → fresh primary (DATA LOSS if no other pod has data)
  → Sidecar: connects → handlePrimary → renews lease

Other pods:
  → Wrapper: still in basebackup loop, but...
  → Sidecar: next tick sees countClusterPrimaries()=1 → zeroPrimaryCount resets
  → Wrapper: on next retry, pg_isready to RW service SUCCEEDS
  → pg_basebackup succeeds → starts as replica
```

### Data Loss Risk

If ALL pods lost PGDATA (not just the primary), the deadlock breaker starts a pod as primary with empty data. This is the correct last-resort behavior — an empty running cluster is better than a permanently stuck one. The operator should be alerted.

---

## Wrapper ↔ Sidecar Contract

### Who Decides What

| Decision | Owner | Mechanism |
|----------|-------|-----------|
| Start/stop PG | **Wrapper** | Main loop, pg_ctl |
| Which role (primary/replica) | **Sidecar** | standby.signal presence, lease |
| When to rebuild PGDATA | **Sidecar** (via marker) or **Wrapper** (crash-loop, WAL checks) | `.pg-swarm-needs-basebackup` marker |
| When to promote | **Sidecar** | `pg_promote()` SQL, lease acquisition |
| When to demote | **Sidecar** | `execStandbyConversion()` (creates standby.signal + stops PG) |
| When to rewind | **Sidecar** (via standby.signal) or **Wrapper** (timeline recovery) | `standby.signal` + `pg_swarm_recover()` |
| Breaking deadlock | **Sidecar** (removes markers) | Deadlock breaker removes marker + standby.signal |

### Invariants

1. **The wrapper never makes role decisions.** It starts PG in whatever mode the files dictate (standby.signal → replica, no signal → primary). The sidecar controls the files.

2. **The sidecar never starts PG.** It writes/removes marker files and stops PG (via exec). The wrapper loop detects the exit and handles recovery.

3. **The marker is a one-way signal.** Writing it means "rebuild PGDATA." Removing it means "abort rebuild, start normally." Only the sidecar's deadlock breaker removes it externally.

4. **The lease is the source of truth for primary identity.** Labels follow the lease, not the other way around. If labels disagree with the lease, the lease wins.

5. **Only one destructive action at a time.** The logwatcher's action mutex ensures restart/rewind/rebasebackup don't overlap. Higher severity supersedes lower.

---

## Timing Constants

| Constant | Default | Where | Effect |
|----------|---------|-------|--------|
| Sidecar tick interval | 5s | `HEALTH_CHECK_INTERVAL` env | Polling frequency for all checks |
| Lease TTL | 5s | `LeaseDurationSeconds` | Time before lease expires without renewal |
| Primary unreachable threshold | 3 ticks (15s) | `handleReplica` | Ticks before attempting failover |
| Crash-loop threshold | 3 crashes | `crashLoopThreshold` | Fast crashes before yielding lease |
| Stable-up threshold | 3 ticks | `stableUpThreshold` | Healthy ticks to resume lease renewal after crash-loop |
| WAL receiver grace period | 30s | `rewindGracePeriod` | Seconds before triggering rewind on WAL receiver down |
| Zero-primary threshold | 5 ticks (25s) | Deadlock breaker | Ticks before emergency promotion |
| Basebackup retries | 12 (60s) | `pg_swarm_rebasebackup` | Max attempts before giving up |
| Wrapper yield sleep | 30s | Primary PGDATA loss guard | Time to wait before attempting basebackup |
| Wrapper crash sleep | 2s | Main loop | Pause between PG restarts |
| LogWatcher rule poll | 15s | `watchRuleFile` | ConfigMap change detection interval |

---

## Known Gaps

1. **Fenced primary with reachable network**: Replicas see the primary as reachable (TCP check succeeds) but writes fail. No automatic resolution other than K8s API recovery.

2. **Lease race on recovery**: A pod that was down briefly can reclaim its own expired lease via `acquireOrRenew` (step 1: identity matches → renew). This can race with a replica trying to acquire the same expired lease.

3. **LogWatcher on new primary**: After promotion, log patterns from the transition (timeline messages, WAL errors) can match rebasebackup rules and trigger marker creation on the new primary itself.

4. **No health-based promotion decision**: The deadlock breaker picks whichever pod acquires the lease first. It doesn't prefer pods with the most recent data or least replication lag.

5. **Wrapper's 30s yield is a fixed sleep**: The wrapper sleeps for 30s regardless of whether a replica has already promoted. Could be shorter with a check loop.
