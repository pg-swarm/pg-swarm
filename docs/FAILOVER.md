# Failover Recovery Scenarios

This document describes every failure scenario the pg-swarm failover system handles, how recovery works, and the invariants that protect against data loss.

## Architecture

Each PostgreSQL pod runs two processes:

- **pg-wrapper.sh** — Container entrypoint. Keeps the container alive across PG crashes. Runs `initdb` (primary) or `pg_basebackup` (replica) on first boot, detects crash loops, performs timeline recovery, and restarts PG in-place.
- **sentinel-sidecar** — Kubernetes sidecar container. Manages the leader lease, detects primary failure, promotes replicas, and runs the log watcher for pattern-based recovery.

These two components coordinate via marker files on the data volume:

| File | Location | Purpose |
|------|----------|---------|
| `.pg-swarm-initialized` | `/var/lib/postgresql/data/` (volume root) | Sentinel: marks that this pod completed first boot. Distinguishes first boot from runtime PGDATA loss. |
| `.pg-swarm-needs-basebackup` | `/var/lib/postgresql/data/` (volume root) | Marker: tells the wrapper to run `pg_basebackup` instead of starting PG normally. Written by the sidecar or wrapper when PGDATA is unrecoverable. |
| `standby.signal` | `$PGDATA` | Standard PostgreSQL file indicating this instance should start as a standby. |

All marker files live on the **volume root** (`/var/lib/postgresql/data/`), NOT inside `$PGDATA` (`/var/lib/postgresql/data/pgdata`). This ensures they survive PGDATA deletion but are cleaned when the PVC is recreated.

## Sentinel File: `.pg-swarm-initialized`

The sentinel solves a critical race condition: when PGDATA is deleted on the primary at runtime, the wrapper must NOT reinitialize via `initdb` (which would create an empty primary and prevent failover). Instead, it must yield the lease and let a replica with real data promote.

**Lifecycle:**
- **Written** after the first successful PG startup (PG accepts connections).
- **Read** by the wrapper before deciding whether to `initdb` or yield.
- **Cleared** only when the PVC is deleted and recreated (new volume = new pod identity).

**Decision tree for ordinal 0 (primary) with missing PG_VERSION:**

```
PG_VERSION missing?
  +-- Sentinel exists?
  |     +-- YES: PGDATA lost at runtime -> YIELD (write marker, sleep, let replica promote)
  |     +-- NO:  First boot -> initdb (normal)
  +-- Sentinel missing, PGDATA empty?
        +-- initdb (first boot)
```

## Recovery Scenarios

### Legend

- **Ordinal 0** = primary pod, **Ordinal >0** = replica pod
- **Sentinel** = `.pg-swarm-initialized` exists on volume root
- **PG_VERSION** = `$PGDATA/PG_VERSION` file exists (indicates initialized PGDATA)
- **Lease** = Kubernetes Lease object that determines which pod is the active primary

### Normal Operations

| # | Scenario | Ordinal | Sentinel | PG_VERSION | Recovery | Notes |
|---|----------|---------|----------|------------|----------|-------|
| 1 | First boot, fresh PVC | 0 | No | No | `docker-entrypoint.sh` runs `initdb`, PG starts, sentinel written | Normal primary initialization |
| 2 | First boot, fresh PVC | >0 | No | No | `pg_basebackup` from primary, PG starts as replica, sentinel written | Normal replica initialization |
| 3 | PG crash, PGDATA intact | 0 | Yes | Yes | Wrapper restarts PG, sidecar renews lease | Fast recovery, no data loss |
| 4 | PG crash, PGDATA intact | >0 | Yes | Yes | Wrapper restarts PG as replica | Fast recovery, no data loss |

### PGDATA Deleted at Runtime

| # | Scenario | Ordinal | Sentinel | PG_VERSION | Recovery | Notes |
|---|----------|---------|----------|------------|----------|-------|
| 5 | PGDATA deleted while running | 0 | Yes | No | **Sentinel detects** PG_VERSION absence within one tick (~5s), writes basebackup marker, stops renewing lease. Lease expires, replica promotes. Wrapper picks up marker and re-basebackups from new primary. Standalone wrapper (no sentinel) sleeps 30s instead. | **The critical fix.** Without the sentinel, wrapper would `initdb` and create an empty primary. |
| 6 | PGDATA deleted while running | >0 | Yes | No | `pg_basebackup` from primary (existing behavior) | Replica rebuilds from current primary |
| 7 | PGDATA deleted while pod was stopped | 0 | Yes | No | Same as #5: yields for failover | Safe choice: don't bring up an empty primary when a replica has real data |

### PVC Recreated (Pod + PVC Deleted, Then Pod Rescheduled)

| # | Scenario | Ordinal | Sentinel | PG_VERSION | Recovery | Notes |
|---|----------|---------|----------|------------|----------|-------|
| 8 | PVC recreated | 0 | No | No | `initdb` runs (first boot). Sidecar detects another pod holds the lease (promoted replica). **Fences PG, demotes to replica.** Wrapper runs `pg_rewind` or `pg_basebackup` from new primary. | Brief empty primary (~5s) but sidecar corrects within one tick. No data loss. |
| 9 | PVC recreated | >0 | No | No | `pg_basebackup` from primary | Normal replica rebuild |

### PGDATA Corrupted (Partial Delete, Disk Error)

| # | Scenario | Ordinal | Sentinel | PG_VERSION | Recovery | Notes |
|---|----------|---------|----------|------------|----------|-------|
| 10 | No PG_VERSION, files exist | 0 | Yes | No | Yields for failover (same as #5) | Treats corruption same as deletion for safety |
| 11 | No PG_VERSION, files exist | >0 | Yes | No | `pg_basebackup` from primary | Existing behavior |
| 12 | No PG_VERSION, files exist, first boot | 0 | No | No | Cleans up + `initdb` | Normal first-boot recovery from partial init |

### WAL and Timeline Issues

| # | Scenario | Ordinal | Sentinel | PG_VERSION | Recovery | Notes |
|---|----------|---------|----------|------------|----------|-------|
| 13 | Timeline divergence | >0 | -- | Yes | `pg_rewind` from primary, falls back to `pg_basebackup` if rewind fails | Detected by wrapper pre-start check and sidecar `checkWalReceiver()` |
| 14 | WAL gap / slot invalidated | >0 | -- | Yes | `pg_basebackup` via logwatcher `rebasebackup` rule | Logwatcher fires on `replication slot has been invalidated` or `WAL segment already removed` |
| 15 | Checkpoint WAL missing | 0 | -- | Yes | Sentinel-enabled: sentinel yields lease, replica promotes. Standalone: `pg_resetwal -f` (last resort). | `pg_resetwal` only available in standalone wrapper (sentinel disabled). |
| 16 | Checkpoint WAL missing | >0 | -- | Yes | `pg_basebackup` from primary | Replica rebuilds cleanly |

### Crash Loops

| # | Scenario | Ordinal | Sentinel | PG_VERSION | Recovery | Notes |
|---|----------|---------|----------|------------|----------|-------|
| 17 | 3+ fast crashes (<30s each) | 0 | -- | Yes | Sentinel-enabled: sentinel yields lease, replica promotes. Standalone: `pg_resetwal -f`. | `pg_resetwal` only in standalone wrapper. |
| 18 | 3+ fast crashes (<30s each) | >0 | -- | Yes | `pg_basebackup` from primary | Full rebuild |
| 19 | 3+ fast crashes on primary, sentinel stops renewing lease | 0 | -- | Yes | Sentinel skips lease renewal after `crashLoopThreshold` (3). Lease expires. Replica promotes. Old primary demotes and rebuilds via `pg_rewind` or `pg_basebackup`. | Sentinel's `handlePrimary()` detects crash loop and yields. |

### Failover (Primary Down)

| # | Scenario | Ordinal | Sentinel | PG_VERSION | Recovery | Notes |
|---|----------|---------|----------|------------|----------|-------|
| 20 | Primary pod killed | >0 | -- | Yes | Sidecar detects primary unreachable for 3 ticks (15s). Checks lease expiration. Acquires lease and promotes via `pg_promote()`. | Standard failover path. |
| 21 | Primary network partition | >0 | -- | Yes | Same as #20 but primary may still be running. After partition heals, old primary detects lease held by another pod, fences itself, demotes to replica. | Split-brain protection via lease. |
| 22 | Planned switchover | >0 | -- | Yes | Central sends `SwitchoverRequest`. Satellite fences primary, checkpoints, promotes target replica, unfences. | Zero-downtime with connection draining. |

## Constraints and Invariants

1. **Never `initdb` on a previously initialized primary.** The sentinel file prevents the wrapper from creating an empty primary when PGDATA is lost at runtime. An empty primary would silently replace all cluster data with nothing.

2. **Lease is the single source of truth for primary identity.** The Kubernetes Lease object (5s TTL) determines which pod is the active primary. All other signals (reachability, WAL receiver, log patterns) are evidence used to decide when to stop renewing or attempt acquisition.

3. **Marker files live outside `$PGDATA`.** Both `.pg-swarm-initialized` and `.pg-swarm-needs-basebackup` are stored on the volume root, not inside the PostgreSQL data directory. This ensures they survive PGDATA deletion but are cleaned on PVC recreation.

4. **The wrapper is faster than the sidecar.** The wrapper's main loop detects PG exit immediately and restarts within seconds. The sidecar's tick runs every 5s. The sentinel closes this race window by preventing the wrapper from making destructive decisions (like `initdb`) that the sidecar hasn't validated.

5. **Replicas always rebuild via `pg_basebackup`.** When a replica's PGDATA is unrecoverable, the wrapper runs `pg_basebackup` from the primary. This is safe because the replica has no unique data — all its data came from the primary.

6. **Primary crash loops yield the lease.** After 3 consecutive fast crashes, the sidecar stops renewing the lease (`crashLoopThreshold`). This allows a healthy replica to promote rather than keeping a broken primary.

7. **Split-brain is prevented by label ordering.** During promotion, the new primary **removes the `role=primary` label from ALL other cluster pods first**, then promotes, then labels itself as primary. This guarantees at most one pod has the primary label at any time. The brief "no primary" window (service has zero endpoints) is safe — clients get connection refused and retry. A "two primaries" window would be catastrophic. If a pod later discovers another pod holds the lease (e.g. after a network partition heals), it also fences its local PG and demotes.

8. **Log-based event rules are additive.** The logwatcher matches PG log lines against rules from a ConfigMap and triggers actions (`restart`, `rewind`, `rebasebackup`, `event`). Rules are severity-ordered with mutex protection — only one destructive action runs at a time.

## Timing

| Event | Typical Duration |
|-------|-----------------|
| Sidecar tick interval | 5s (configurable via `HEALTH_CHECK_INTERVAL`) |
| Lease TTL | 5s |
| Primary unreachable threshold | 3 ticks (15s) |
| WAL receiver grace period | 30s before triggering rewind |
| Wrapper crash-loop threshold | 3 fast crashes (<30s each) |
| Wrapper yield sleep (PGDATA loss) | 30s (ensures lease expires) |
| Total failover time (primary killed) | ~20-25s (15s detection + lease expiry + promotion) |
| Total failover time (PGDATA deleted) | ~35-40s (wrapper yield + lease expiry + promotion) |
