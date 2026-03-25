# Dashboard Mock Data vs Real Backend — Gap Analysis

**Every REST endpoint the dashboard calls is fully implemented.** All 59 endpoints have real handlers hitting a PostgreSQL store with real SQL. The mock data closely mirrors the actual data shapes. The main gap is that health metrics depend on satellites running against real K8s clusters with PostgreSQL pods.

---

## Fully Wired (backend → store → database → REST → dashboard)

| Dashboard Feature | Endpoints | Backend Status |
|---|---|---|
| **Satellites list** (state, labels, heartbeat) | `GET /satellites`, approve, reject, labels | Real SQL, live heartbeat tracking |
| **Satellite logs** (recent + streaming) | `GET /satellites/:id/logs`, `/logs/stream` | In-memory ring buffer, SSE streaming |
| **Satellite log level** | `POST /satellites/:id/log-level` | Pushes gRPC command to satellite |
| **Cluster configs** (list, CRUD) | `GET /clusters`, create, update, delete | Real SQL, pushes config to satellites via gRPC |
| **Cluster pause/resume** | `POST /clusters/:id/pause\|resume` | Real store update + gRPC push |
| **Cluster switchover** | `POST /clusters/:id/switchover` | Sends gRPC SwitchoverRequest to satellite |
| **Profiles** (CRUD + clone) | All `/profiles` endpoints | Full SQL CRUD, profile locking on cluster attach |
| **Deployment rules** (CRUD + cluster listing) | All `/deployment-rules` endpoints | Full SQL, label-matching evaluation |
| **PostgreSQL versions + variants** | All `/postgres-versions`, `/postgres-variants` | Full SQL CRUD, default version management |
| **Backup profiles** (CRUD + attach/detach) | All `/backup-profiles` endpoints | Full SQL, bumps config versions on attach/detach |
| **Backup inventory** | `GET /clusters/:id/backups`, `GET /backups/:id` | Real SQL, populated by satellite backup CronJobs |
| **Restore operations** | `POST /clusters/:id/restore`, list restores | Real SQL + gRPC RestoreCommand to satellite |
| **Events** | `GET /events` | Real SQL, events created by gRPC health processing |
| **Health** (cluster state + instances) | `GET /health` | Real SQL, data from satellite health monitor |

---

## Health Metrics — What the Satellite Actually Collects

The satellite health monitor (`internal/satellite/health/monitor.go`, 660 lines) queries every PostgreSQL instance directly and collects:

| Metric | Source | Mock equivalent |
|---|---|---|
| Pod name, role, ready | K8s pod labels + PG query | `pod_name`, `role`, `ready` |
| Replication lag (bytes + seconds) | `pg_stat_replication` / `pg_wal_lsn_diff` | `replication_lag_seconds`, `replication_lag_bytes` |
| Connections (used, max, active) | `pg_stat_activity` + `max_connections` | `connections_used`, `connections_max`, `connections_active` |
| Disk usage | `pg_database_size()` sum | `disk_used_bytes` |
| WAL disk size | `pg_ls_waldir()` | `wal_disk_bytes` |
| Index hit ratio | `pg_statio_user_indexes` | `index_hit_ratio` |
| Txn commit ratio | `pg_stat_database` | `txn_commit_ratio` |
| Database stats (top 5) | Per-DB size + cache hit | `database_stats[]` |
| Table stats (top 30) | Tuple counts, scans, sizes | `table_stats[]` |
| Slow queries (top 10) | `pg_stat_statements` | `slow_queries[]` |
| WAL receiver active | `pg_stat_wal_receiver` | `wal_receiver_active` |
| Timeline ID | `pg_control_checkpoint()` | `timeline_id` |
| PG start time | `pg_postmaster_start_time()` | `pg_start_time` |

**All mock fields map 1:1 to real collected data.** The satellite stores this as a JSON blob in `cluster_health.instances`.

---

## Data Flow

```
PostgreSQL pod  →  Satellite health monitor (SQL queries every N seconds)
                   →  gRPC stream (ClusterHealthReport proto)
                      →  Central server (converts proto → JSON, upserts into PostgreSQL)
                         →  REST GET /health (returns stored JSON)
                            →  Dashboard DataContext (polls every 10s)
```

Cluster state (`running`, `degraded`, `failed`, `creating`, `paused`) is derived **by the satellite** based on primary readiness + replica count, then stored as-is by central.

---

## What You Need for Real Data

To see real data instead of mock:

1. **Central server running** with PostgreSQL (`make docker-compose-up` or `go run ./cmd/central`)
2. **At least one satellite** registered and approved (`go run ./cmd/satellite`)
3. **A cluster deployed** via deployment rule — satellite creates StatefulSet — PostgreSQL pods start
4. **Health monitor runs** automatically on the satellite, feeding data back to central

The dashboard at `npm run dev` (without MOCK) proxies to `localhost:8080` and shows the real data.

---

## No Gaps Found

The mock data shapes were intentionally modeled after the real proto/model definitions. There are no dashboard features that call endpoints which don't exist or return different shapes than expected.
