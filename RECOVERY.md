# Cluster Failure Conditions & Recovery Logic

## Three recovery layers

| Layer | Runs when | Location | Responsibility |
|-------|-----------|----------|----------------|
| **Init container** | Pod (re)created | `manifest_statefulset.go` `buildInitContainer` | Bootstrap or repair PGDATA before PG starts |
| **Wrapper loop** | Continuously in postgres container | `manifest_statefulset.go` `buildMainContainer` | Detect PG exit, run `pg_swarm_recover()`, restart PG in-place |
| **Failover sidecar** | Continuously in failover container | `monitor.go` | Lease management, split-brain detection, promotion, demotion, WAL receiver monitoring |

---

## A. PG Process Failures

### A1. PG crashes on primary, recovers within 5s
- **Condition**: PG process exits but wrapper restarts it quickly. Sidecar ticks fail (`pgx.Connect` fails) but lease hasn't expired yet.
- **Recovery**: Wrapper detects PG exit → `pg_swarm_recover()` (no-op: no `standby.signal`) → restarts PG via `docker-entrypoint.sh`. Sidecar resumes renewing lease on next successful tick. No failover.

### A2. PG crashes on primary, down > 5s
- **Condition**: Wrapper restarts PG but it takes > 5s (or keeps crashing). Lease expires.
- **Recovery**:
  1. Replica sidecars detect primary is unreachable via direct reachability check (TCP connect to RW service) and expired lease → one wins via `acquireOrRenew` (optimistic locking on resourceVersion) → calls `pg_promote()` → labels self `primary`
  2. RW service switches to new primary
  3. When original primary's PG finally starts, sidecar runs `handlePrimary` → lease held by other pod → **fence** (set `default_transaction_read_only=on`, kill client backends) → **demote** (create `standby.signal`, set `primary_conninfo`, `pg_ctl stop`)
  4. Wrapper detects PG exit → `pg_swarm_recover()` → checks timeline → `pg_rewind` if diverged → restarts PG as standby

### A3. PG crashes on replica
- **Condition**: Replica PG exits. No lease implications.
- **Recovery**: Wrapper detects exit → `pg_swarm_recover()` → checks timeline (standby.signal present) → if timelines match, just restarts PG. If diverged, `pg_rewind` → restart.
- **Health**: `DeriveClusterState` → `DEGRADED` while replica is down.

### A4. PG hangs on primary (process alive, not responding)
- **Condition**: PG is alive but not accepting connections. `pg_isready` fails.
- **Recovery**:
  1. Sidecar: `pgx.Connect` fails each tick → lease not renewed → expires after 5s
  2. Replica promotes (same as A2)
  3. Liveness probe fails after `initialDelaySeconds(30) + failureThreshold(6) * periodSeconds(10)` = **90s** → K8s kills container
  4. Wrapper exits (SIGTERM) → pod container restarts
  5. Init container runs → idempotent path → PG starts as primary (no standby.signal)
  6. Sidecar detects split-brain → fence → demote → wrapper restarts as standby
- **Gap**: 90s before K8s kills the hung PG. During this window, failover has already happened (at 5s), but the old primary pod is still "Running" in K8s. The sidecar can't connect so it can't fence. Writes are blocked only by the RW service switching away. **The old primary is unfenced for up to 90s** — any client connecting directly to the pod (not via service) could write.

### A5. PG OOM-killed
- **Condition**: Kernel kills PG process (or container). Container exit code 137.
- **Recovery**: Same as A2 if primary, A3 if replica. K8s may restart the container (CrashLoopBackOff) which triggers init container → same paths.

---

## B. Pod-Level Failures

### B1. Primary pod deleted (kubectl delete, eviction)
- **Condition**: Pod receives SIGTERM. Wrapper trap fires → sets `SHUTTING_DOWN=true` → forwards SIGTERM to PG → PG does fast shutdown → container exits. PVC persists.
- **Recovery**:
  1. Lease expires (5s) → replica promotes
  2. StatefulSet controller recreates pod (same ordinal, same PVC)
  3. Init container: `PG_VERSION` exists, no `standby.signal` → copies config → exits (PG starts as primary)
  4. Sidecar: `pg_is_in_recovery()=false` → `handlePrimary` → lease held by new primary → fence → demote
  5. Wrapper restarts as standby

### B2. Replica pod deleted
- **Condition**: Same SIGTERM/shutdown flow. PVC persists.
- **Recovery**:
  1. StatefulSet recreates pod
  2. Init container: `PG_VERSION` exists, `standby.signal` exists → timeline check
     - Timelines match → PG starts as standby, reconnects to primary
     - Timelines diverge (failover happened while this pod was gone) → `pg_rewind` or `pg_basebackup`
  3. Health: `DEGRADED` → `RUNNING`

### B3. Pod evicted (resource pressure)
- **Condition**: Same as B1/B2. K8s sends SIGTERM with grace period.
- **Recovery**: Identical to B1 (primary) or B2 (replica).

---

## C. Node Failures

### C1. Node running primary goes down
- **Condition**: Node becomes NotReady. K8s default taint timeout is **5 minutes** before pods are evicted.
- **Recovery**:
  1. Lease expires after 5s → replica promotes → RW service switches
  2. After 5 min: K8s evicts pods → StatefulSet recreates on another node → same PVC (if storage supports multi-attach or pod is rescheduled to same zone)
  3. New pod init container: same as B1 steps 3-5
- **Gap**: If the PVC is zone-local and no node is available in that zone, the pod stays Pending indefinitely. Cluster runs degraded with the promoted replica as primary.

### C2. Node running replica goes down
- **Condition**: Same 5-minute taint timeout.
- **Recovery**: Same as B2, just delayed by 5 minutes. Cluster stays `DEGRADED` until pod is rescheduled.

### C3. Network partition — primary can't reach K8s API
- **Condition**: Primary PG is healthy, but sidecar can't reach K8s API to renew lease.
- **Recovery**:
  1. Sidecar: `acquireOrRenew` returns error → `handlePrimary` fences as precaution (line 123-129)
  2. Lease expires → replica promotes
  3. When partition heals: sidecar reconnects → sees lease held by other pod → fence already in place → demote
- **Note**: Fencing on API unreachability is the safe choice — better to block writes than risk split-brain.

### C4. Network partition — pods can't reach each other but can reach API
- **Condition**: Primary renews lease successfully. Replicas can't stream WAL from primary. WAL receiver goes down.
- **Recovery**:
  1. Replicas: `checkWalReceiver` → WAL receiver not streaming → lease NOT expired (primary is renewing) → start grace period (30s)
  2. `hasTimelineDivergence` → false (no promotion happened, same timeline) → wait for grace period
  3. After 30s grace: `doRewind` → sets standby.signal → stops PG → wrapper does `pg_swarm_recover` → timelines match → just restarts PG → WAL receiver reconnects if partition heals
- **Gap**: If partition persists, replicas keep cycling through stop → recover → start → WAL receiver fails → grace period → stop. This is a loop. The replicas will keep restarting every ~32s (30s grace + 2s sleep). Not ideal but not harmful.

### C5. Network partition — primary isolated from both API and replicas
- **Condition**: Primary can't reach anything. Replicas and API are fine.
- **Recovery**:
  1. Primary sidecar: lease renewal fails → fences immediately (line 123-129)
  2. Replicas: lease expires → one promotes
  3. When partition heals: primary sidecar sees lease held by other → already fenced → demote → wrapper restarts as standby

---

## D. Split-Brain Conditions

### D1. Two pods think they're primary
- **Condition**: Pod-A was primary, pod-B got promoted. Pod-A's PG recovers/restarts before sidecar detects the change.
- **Recovery**: Sidecar on pod-A runs `handlePrimary` → `acquireOrRenew` returns false (pod-B holds lease) → fence → demote → wrapper restarts as standby.
- **Window**: Between PG starting as primary and sidecar's next tick (up to `healthCheckInterval`, default 1s). During this window, both PG instances accept writes.
- **Mitigation**: Fencing (`default_transaction_read_only=on`) is the first action. Demotion follows. The RW service already points at pod-B, so only direct pod connections hit pod-A.

### D2. Lease renewal fails but PG is healthy
- **Condition**: K8s API hiccup. Primary PG is fine but lease can't be verified.
- **Recovery**: `handlePrimary` catches error → fences as precaution (blocks writes). Next tick: if API is back, renews lease → unfences (line 136-141). If API stays down, stays fenced.
- **Note**: This causes a brief write outage even though PG is healthy. This is the correct tradeoff — safety over availability.

### D3. Stale lease holder identity (pod was replaced but lease still has old name)
- **Condition**: Pod-0 dies, is recreated with same name. Lease still shows pod-0 as holder. Sidecar sees it's the holder → renews.
- **Recovery**: This is actually correct behavior. Same pod name = same identity. The lease is validly held. No issue.

---

## E. Replication & Timeline Issues

### E1. WAL receiver disconnects temporarily (< 30s)
- **Condition**: Brief network blip. WAL receiver reconnects on its own.
- **Recovery**: `checkWalReceiver` → starts grace period → within 30s, WAL receiver reconnects → `active=true` → grace period reset. No action taken.

### E2. WAL receiver down > 30s, no timeline divergence
- **Condition**: Network issue persists but no failover happened (same timeline).
- **Recovery**:
  1. `checkWalReceiver` → grace period expires → `doRewind`
  2. Sidecar exec: create standby.signal, set conninfo, stop PG
  3. Wrapper: `pg_swarm_recover` → timelines match → just restart PG
  4. PG reconnects WAL receiver to primary
- **Note**: This is mostly a no-op recovery (stop and restart) since timelines match. It resets the connection.

### E3. Timeline divergence after promotion
- **Condition**: New primary is on timeline N+1. Replicas still on timeline N. WAL receiver can't stream (incompatible timeline).
- **Recovery**:
  1. `checkWalReceiver` → `hasTimelineDivergence` → true → immediate `doRewind` (skip grace period)
  2. Sidecar sets standby.signal, stops PG
  3. Wrapper: `pg_swarm_recover` → detects timeline mismatch → `pg_rewind` against new primary → restart as standby on timeline N+1

### E4. pg_rewind fails (in wrapper)
- **Condition**: PGDATA has diverged too far, or wal_log_hints/checksums not enabled, or primary unreachable.
- **Recovery**: Wrapper's `pg_swarm_recover` falls back to `rm -rf "$PGDATA"/*` → `pg_basebackup` from primary → copy config → restart.
- **Gap**: If `pg_basebackup` also fails, `PGDATA` is wiped (no `PG_VERSION`). Wrapper starts `docker-entrypoint.sh postgres` on empty PGDATA. The official PG docker image's entrypoint will run `initdb` if PGDATA is empty. On a replica (non-0 ordinal) this creates a standalone instance — **silent data divergence**. On next pod restart, init container sees no `PG_VERSION` → fresh init path → ordinal 0 does `initdb`, non-0 does `pg_basebackup`. But between the wrapper's failed recovery and pod restart, the pod runs a blank standalone PG.

### E5. pg_rewind fails (in init container)
- **Condition**: Same as E4 but during pod startup.
- **Recovery**: Init container falls back to `pg_basebackup`. If that also fails:
  1. Marker `/var/lib/postgresql/data/.pg-swarm-needs-basebackup` is written BEFORE wiping PGDATA
  2. `pg_basebackup` fails → init container exits non-zero → K8s restarts pod
  3. Next init: marker present → clean PGDATA → retry `pg_basebackup`
  4. Retries until primary is reachable and basebackup succeeds
- **Note**: The marker is on the PVC root (outside PGDATA) so it survives the PGDATA wipe.

### E6. Replica falls far behind (large WAL gap)
- **Condition**: Replica was down for a long time, WAL segments have been recycled on primary.
- **Recovery**: PG tries to stream → gets "requested WAL segment has already been removed" → WAL receiver disconnects.
  1. Sidecar: `checkWalReceiver` → timeline may or may not diverge
  2. If archive is configured: `restore_command` can fetch old WAL segments → PG catches up
  3. If no archive: `pg_rewind` won't help (it's a WAL gap, not divergence) → `pg_basebackup` is needed
- **Gap**: The sidecar's `hasTimelineDivergence` may return false (same timeline, just behind). The grace period triggers `doRewind` → wrapper does `pg_swarm_recover` → timelines match → just restarts PG → same problem. **This loops without resolution.** The sidecar doesn't detect "WAL gap" as distinct from "transient disconnection".

---

## F. Storage Failures

### F1. PVC data corruption
- **Condition**: Filesystem corruption on the PVC. PG can't start or crashes immediately.
- **Recovery**: Wrapper loops (PG crashes → recover → crash → ...). Eventually:
  - Primary: lease expires → replica promotes → sidecar demotes when PG briefly starts
  - Replica: stays crashed, cluster `DEGRADED`
- **Resolution**: Manual intervention required — delete PVC, let StatefulSet create new one. Init container will `initdb` (ordinal 0) or `pg_basebackup` (replicas).

### F2. All PVCs deleted, pods running
- **Condition**: PVCs deleted via `kubectl`. Pods still running with PG using in-memory data (PVC unmount is lazy).
- **Recovery**: When pods restart (for any reason), init container sees empty PGDATA → fresh init path. Full data loss.
- **Prevention**: `DeletionProtection` finalizer (`pg-swarm.io/protection`) blocks PVC deletion.

### F3. All PVCs deleted after pods deleted
- **Condition**: Complete wipe.
- **Recovery**: StatefulSet creates new PVCs (empty). Init container: ordinal 0 → `initdb`, others → `pg_basebackup`. Fresh cluster. **Full data loss.**

### F4. Storage class unavailable
- **Condition**: Can't provision new PVCs (StorageClass deleted, provisioner down).
- **Recovery**: Pod stays Pending. No automatic recovery. Manual intervention.

---

## G. Sidecar Failures

### G1. Sidecar container crashes
- **Condition**: Sidecar exits. K8s restarts it. PG keeps running (separate container).
- **Recovery**: K8s restarts sidecar. On startup, sidecar ticks immediately. Detects current PG state and resumes.
- **Gap during restart**: Lease not renewed. If restart takes > 5s (e.g., CrashLoopBackOff backoff), lease expires → replica promotes → two primaries until sidecar restarts and handles split-brain.

### G2. Sidecar can't connect to local PG
- **Condition**: PG is starting up, or sidecar starts before PG.
- **Recovery**: `tick()` → `pgx.Connect` fails → logs warning → skips tick → retries next interval. No action taken. This is expected during startup.

### G3. Sidecar can't exec into postgres container
- **Condition**: `rest.Config` unavailable, RBAC denied, or API server unreachable.
- **Recovery**: `demotePrimary` or `rewindOrReinit` return error → logged → retry next tick.
- **Impact on demote**: PG stays as primary but fenced (writes blocked). Retries each tick until exec succeeds.
- **Impact on rewind**: PG stays running with stale WAL. Retries after grace period reset.

---

## H. Init Container Edge Cases

### H1. Primary unreachable during init (standby timeline check)
- **Condition**: Init container starts, `standby.signal` exists, primary not ready yet.
- **Recovery**: Waits up to 30s (`pg_isready` loop, 6 tries * 5s). If primary still not ready → skips timeline check → PG starts with existing data → sidecar handles any issues at runtime.

### H2. Primary unreachable during fresh replica init
- **Condition**: Non-0 ordinal, empty PVC, primary not ready.
- **Recovery**: `until pg_isready` loop → waits indefinitely until primary is reachable → then `pg_basebackup`. Pod stays in Init state.

### H3. Needs-basebackup marker but primary unreachable
- **Condition**: Previous `pg_basebackup` failed, marker exists, primary still down.
- **Recovery**: Marker forces replica path → `pg_basebackup` fails → init exits non-zero → K8s restarts → retry loop.

### H4. Former primary (ordinal 0) has standby.signal after failover
- **Condition**: Pod-0 was demoted. On restart, init container sees `PG_VERSION` + `standby.signal` → timeline check.
- **Recovery**: If primary is reachable, checks timeline → rewind if needed → starts as standby. Sidecar eventually detects no lease (or lease held by other) → stays replica. Correct behavior.

### H5. All pods restart, pod-0 was a replica (standby.signal present)
- **Condition**: All pods deleted. Pod-0 starts first. Has `standby.signal` from previous demotion.
- **Recovery**:
  1. Init container: `PG_VERSION` + `standby.signal` → timeline check → primary not reachable (no pods up yet) → skips check → PG starts as standby
  2. Sidecar: `pg_is_in_recovery()=true` → `handleReplica` → lease expired/missing → `acquireOrRenew` → creates lease → `pg_promote()` → labels primary
  3. Pod-0 is now primary. Other pods init against it.
- **Note**: Self-correcting but involves a promotion cycle on startup.

---

## I. Cluster-Wide / Operational

### I1. Scale up (add replicas)
- **Condition**: `replicas` increased in config. New pods created by StatefulSet.
- **Recovery**: Not a failure. New pods run init container: empty PVC → `pg_basebackup` from primary.

### I2. Scale down (remove replicas)
- **Condition**: `replicas` decreased. K8s terminates highest-ordinal pods.
- **Recovery**: If removed pod was the lease holder (unlikely but possible after failover), lease expires → remaining replica promotes.
- **Note**: PVCs are NOT deleted by K8s on scale-down. They persist and can be reattached if scaled back up.

### I3. Config update (rolling restart)
- **Condition**: ConfigMap changes. Pods restarted one at a time.
- **Recovery**: Init container runs → idempotent path → copies new config → PG starts. No failover unless restart takes > 5s.
- **Gap**: If the PRIMARY pod is restarted and takes > 5s, a failover happens. On restart, the old primary enters split-brain path → fence → demote → becomes replica. **The primary role has permanently moved.** This is correct but may surprise operators.

### I4. Cluster first creation
- **Condition**: No pods, no PVCs, no lease.
- **Recovery**: StatefulSet creates pods. Pod-0: empty PVC → `initdb` → creates users/databases. Pod-1+: `pg_basebackup` from pod-0 via RW service. Pod-0 sidecar: no lease → creates lease → labels primary.

---

## J. Compound / Cascading Failures

### J1. Primary crashes, promoted replica also crashes
- **Condition**: Pod-A (primary) crashes. Pod-B promotes. Pod-B also crashes before Pod-A recovers.
- **Recovery**: Pod-B's lease expires (5s). Pod-C (if exists) promotes. If only 2 pods: lease expires, Pod-A recovers, sidecar creates new lease → becomes primary. Pod-B recovers → sidecar sees lease held by Pod-A → demotes.

### J2. Failover during pg_rewind
- **Condition**: Replica is doing `pg_rewind` (via wrapper). During rewind, the primary it's rewinding against fails.
- **Recovery**: `pg_rewind` fails (primary unreachable) → wrapper falls back to `pg_basebackup` → also fails → PGDATA wiped → `docker-entrypoint.sh` on empty PGDATA → PG does initdb → standalone instance.
- **Gap**: Same as E4. Silent standalone instance until pod restart.

### J3. All sidecars crash simultaneously
- **Condition**: Bug in sidecar, all crash at once. K8s restarts them.
- **Recovery**: During gap, no lease renewals. If gap > 5s, when sidecars restart: all replicas see expired lease → one wins → promotes. But old primary is still running. Split-brain detected on next tick → fence → demote. Self-correcting.

---

## Summary of Gaps

| # | Condition | Impact | Severity |
|---|-----------|--------|----------|
| 1 | **E4/J2: pg_basebackup fails in wrapper** | PGDATA wiped, `docker-entrypoint.sh` runs initdb on replica → silent standalone instance until pod restart | High |
| 2 | **E6: WAL gap (not divergence)** | Replica can't stream, sidecar keeps restarting in loop, never does pg_basebackup because timelines match | Medium |
| 3 | **A4: PG hang, unfenced for up to 90s** | Old primary accepts direct-pod writes for 90s after failover (RW service already switched, but direct connections are exposed) | Medium |
| 4 | **C4: Network partition loop** | Replicas cycle stop/restart every ~32s during partition. Functional but noisy, burns restart budget | Low |
| 5 | **I3: Rolling restart causes unintended failover** | Primary restart > 5s triggers permanent role swap. Expected by design but operationally surprising | Low |
