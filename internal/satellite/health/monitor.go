package health

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"google.golang.org/protobuf/types/known/timestamppb"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/pg-swarm/pg-swarm/internal/satellite/operator"
)

// Monitor periodically checks the health of managed PostgreSQL clusters.
// clusterStartGrace is how long a cluster is allowed to be not-fully-ready
// before it transitions from CREATING to FAILED.
const clusterStartGrace = 10 * time.Minute

// Monitor periodically checks the health of all managed PostgreSQL clusters.
type Monitor struct {
	client       kubernetes.Interface
	operator     *operator.Operator
	onHealth     func(*pgswarmv1.ClusterHealthReport)
	onEvent      func(*pgswarmv1.EventReport)
	onBackup     func(*pgswarmv1.BackupStatusReport)
	interval     time.Duration
	lastStates   map[string]pgswarmv1.ClusterState
	mu           sync.Mutex
	firstSeen    map[string]time.Time // tracks when each cluster was first observed
	lastBackupCM map[string]string    // tracks last-seen backup status ConfigMap resourceVersion
}

// New creates a new health Monitor.
func New(client kubernetes.Interface, op *operator.Operator, interval time.Duration) *Monitor {
	return &Monitor{
		client:       client,
		operator:     op,
		interval:     interval,
		lastStates:   make(map[string]pgswarmv1.ClusterState),
		firstSeen:    make(map[string]time.Time),
		lastBackupCM: make(map[string]string),
	}
}

// SetOnHealth sets the callback for health reports.
func (m *Monitor) SetOnHealth(fn func(*pgswarmv1.ClusterHealthReport)) {
	m.onHealth = fn
}

// SetOnEvent sets the callback for event reports.
func (m *Monitor) SetOnEvent(fn func(*pgswarmv1.EventReport)) {
	m.onEvent = fn
}

// SetOnBackup sets the callback for backup status reports.
func (m *Monitor) SetOnBackup(fn func(*pgswarmv1.BackupStatusReport)) {
	m.onBackup = fn
}

// Run starts the health check loop. It blocks until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) {
	log.Trace().Dur("interval", m.interval).Msg("health monitor starting")
	// Initial delay to let clusters start
	select {
	case <-time.After(10 * time.Second):
	case <-ctx.Done():
		return
	}

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	m.checkAll(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			log.Trace().Msg("health monitor tick")
			m.checkAll(ctx)
		}
	}
}

func (m *Monitor) checkAll(ctx context.Context) {
	clusters := m.operator.ManagedClusters()
	log.Trace().Int("cluster_count", len(clusters)).Msg("checkAll starting")

	// Check all clusters in parallel so a broken cluster doesn't block
	// health reporting for healthy ones.
	type result struct {
		mc     operator.ManagedCluster
		report *pgswarmv1.ClusterHealthReport
	}
	results := make([]result, len(clusters))
	var wg sync.WaitGroup
	for i, mc := range clusters {
		wg.Add(1)
		go func(idx int, mc operator.ManagedCluster) {
			defer wg.Done()
			results[idx] = result{mc: mc, report: m.checkCluster(ctx, mc)}
		}(i, mc)
	}
	wg.Wait()

	for _, r := range results {
		if r.report == nil {
			continue
		}

		if m.onHealth != nil {
			m.onHealth(r.report)
		}

		// Detect state transitions and emit events
		key := r.mc.Namespace + "/" + r.mc.ClusterName
		prev, existed := m.lastStates[key]
		if existed && prev != r.report.State {
			if m.onEvent != nil {
				m.onEvent(&pgswarmv1.EventReport{
					ClusterName: r.mc.ClusterName,
					Severity:    severityForTransition(r.report.State),
					Message:     fmt.Sprintf("cluster state changed: %s -> %s", prev, r.report.State),
					Source:      "health-monitor",
					Timestamp:   timestamppb.Now(),
				})
			}
		}
		// Trigger pending base backups when cluster first becomes RUNNING
		if r.report.State == pgswarmv1.ClusterState_CLUSTER_STATE_RUNNING &&
			(!existed || prev != pgswarmv1.ClusterState_CLUSTER_STATE_RUNNING) {
			m.operator.TriggerPendingBackups(ctx, r.mc.Namespace, r.mc.ClusterName)
		}
		m.lastStates[key] = r.report.State
	}

	// Check for backup status ConfigMap updates
	if m.onBackup != nil {
		m.checkBackupStatuses(ctx, clusters)
	}
}

// checkBackupStatuses looks for backup-status ConfigMaps and reports new completions.
func (m *Monitor) checkBackupStatuses(ctx context.Context, clusters []operator.ManagedCluster) {
	for _, mc := range clusters {
		cmName := mc.ClusterName + "-backup-status"
		cm, err := m.client.CoreV1().ConfigMaps(mc.Namespace).Get(ctx, cmName, metav1.GetOptions{})
		if err != nil {
			continue // no backup status ConfigMap = no backup running
		}

		key := mc.Namespace + "/" + cmName
		if cm.ResourceVersion == m.lastBackupCM[key] {
			continue // no change since last check
		}
		m.lastBackupCM[key] = cm.ResourceVersion

		report := &pgswarmv1.BackupStatusReport{
			ClusterName:  mc.ClusterName,
			Namespace:    mc.Namespace,
			BackupType:   cm.Data["backup_type"],
			Status:       cm.Data["status"],
			BackupPath:   cm.Data["backup_path"],
			ErrorMessage: cm.Data["error_message"],
		}

		if t, err := time.Parse(time.RFC3339, cm.Data["started_at"]); err == nil {
			report.StartedAt = timestamppb.New(t)
		}
		if t, err := time.Parse(time.RFC3339, cm.Data["completed_at"]); err == nil {
			report.CompletedAt = timestamppb.New(t)
		}

		m.onBackup(report)
	}
}

func (m *Monitor) checkCluster(ctx context.Context, mc operator.ManagedCluster) *pgswarmv1.ClusterHealthReport {
	clusterKey := mc.Namespace + "/" + mc.ClusterName
	log.Trace().Str("cluster", clusterKey).Msg("checkCluster start")

	// Track when we first observed this cluster for the startup grace period.
	m.mu.Lock()
	if _, ok := m.firstSeen[clusterKey]; !ok {
		m.firstSeen[clusterKey] = time.Now()
	}
	clusterAge := time.Since(m.firstSeen[clusterKey])
	m.mu.Unlock()

	checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	pods, err := m.client.CoreV1().Pods(mc.Namespace).List(checkCtx, metav1.ListOptions{
		LabelSelector: operator.LabelCluster + "=" + mc.ClusterName + ",!" + operator.LabelBackupType,
	})
	if err != nil {
		log.Warn().Err(err).Str("cluster", mc.ClusterName).Msg("failed to list pods for health check")
		return nil
	}

	log.Trace().Str("cluster", clusterKey).Int("pod_count", len(pods.Items)).Msg("checkCluster pods found")
	if len(pods.Items) == 0 {
		return nil
	}

	password := m.readSuperuserPassword(checkCtx, mc.Namespace, mc.ClusterName)

	// Check all instances in parallel
	instances := make([]*pgswarmv1.InstanceHealth, len(pods.Items))
	var wg sync.WaitGroup
	for i := range pods.Items {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			instances[idx] = m.checkInstance(checkCtx, &pods.Items[idx], mc.Namespace, mc.ClusterName, password)
		}(i)
	}
	wg.Wait()

	state := DeriveClusterState(instances, mc.Replicas, clusterAge)
	log.Trace().Str("cluster", clusterKey).Str("state", state.String()).Msg("checkCluster derived state")
	if mc.Paused {
		state = pgswarmv1.ClusterState_CLUSTER_STATE_PAUSED
	}

	return &pgswarmv1.ClusterHealthReport{
		ClusterName: mc.ClusterName,
		State:       state,
		Instances:   instances,
		Timestamp:   timestamppb.Now(),
	}
}

func (m *Monitor) readSuperuserPassword(ctx context.Context, namespace, clusterName string) string {
	secretName := clusterName + "-secret"
	secret, err := m.client.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		log.Warn().Err(err).Str("secret", secretName).Msg("failed to read cluster secret")
		return ""
	}
	return string(secret.Data["superuser-password"])
}

func (m *Monitor) checkInstance(ctx context.Context, pod *corev1.Pod, namespace, clusterName, password string) *pgswarmv1.InstanceHealth {
	ih := &pgswarmv1.InstanceHealth{
		PodName: pod.Name,
		Ready:   isPodReady(pod),
	}

	if password == "" {
		ih.ErrorMessage = "superuser password unavailable"
		return ih
	}
	if pod.Status.PodIP == "" {
		ih.ErrorMessage = "pod has no IP"
		return ih
	}

	host := fmt.Sprintf("%s.%s-headless.%s.svc.cluster.local", pod.Name, clusterName, namespace)
	// URL-encode password to handle special characters
	connStr := fmt.Sprintf("postgres://postgres:%s@%s:5432/postgres?connect_timeout=5&sslmode=disable",
		url.QueryEscape(password), host)

	connCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	conn, err := pgx.Connect(connCtx, connStr)
	if err != nil {
		ih.Ready = false
		ih.ErrorMessage = fmt.Sprintf("connection failed: %v", err)
		return ih
	}
	defer conn.Close(ctx)

	// 1. Role detection
	var isInRecovery bool
	if err := conn.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&isInRecovery); err != nil {
		ih.Ready = false
		ih.ErrorMessage = fmt.Sprintf("pg_is_in_recovery failed: %v", err)
		return ih
	}

	// 2. Connection count + max_connections
	var connsUsed int32
	var connsMax int32
	row := conn.QueryRow(ctx,
		"SELECT (SELECT count(*)::int FROM pg_stat_activity), current_setting('max_connections')::int")
	if err := row.Scan(&connsUsed, &connsMax); err != nil {
		log.Debug().Err(err).Str("pod", pod.Name).Msg("failed to read connection stats")
	} else {
		ih.ConnectionsUsed = connsUsed
		ih.ConnectionsMax = connsMax
	}

	// 3. Disk usage (sum of all database sizes)
	var diskUsed int64
	if err := conn.QueryRow(ctx, "SELECT COALESCE(sum(pg_database_size(datname)), 0)::bigint FROM pg_database").Scan(&diskUsed); err != nil {
		log.Debug().Err(err).Str("pod", pod.Name).Msg("failed to read disk usage")
	} else {
		ih.DiskUsedBytes = diskUsed
	}

	// 4. PG start time (uptime)
	var pgStart time.Time
	if err := conn.QueryRow(ctx, "SELECT pg_postmaster_start_time()").Scan(&pgStart); err != nil {
		log.Debug().Err(err).Str("pod", pod.Name).Msg("failed to read pg start time")
	} else {
		ih.PgStartTime = timestamppb.New(pgStart)
	}

	// 5. Timeline ID
	var timelineID int64
	if err := conn.QueryRow(ctx, "SELECT timeline_id::bigint FROM pg_control_checkpoint()").Scan(&timelineID); err != nil {
		log.Debug().Err(err).Str("pod", pod.Name).Msg("failed to read timeline")
	} else {
		ih.TimelineId = timelineID
	}

	// 6. Role-specific checks
	if isInRecovery {
		ih.Role = pgswarmv1.InstanceRole_INSTANCE_ROLE_REPLICA
		m.checkReplica(ctx, conn, ih)
	} else {
		ih.Role = pgswarmv1.InstanceRole_INSTANCE_ROLE_PRIMARY
		m.checkPrimary(ctx, conn, ih)
	}

	// 7. WAL statistics (pg_stat_wal)
	m.collectWalStats(ctx, conn, ih)

	// 8. WAL on-disk size
	m.collectWalDiskSize(ctx, conn, ih)

	// 9. Index hit ratio (from pg_statio_user_indexes)
	m.collectIndexHitRatio(ctx, conn, ih)

	// 10. Transaction commit ratio (from pg_stat_database)
	m.collectTxnCommitRatio(ctx, conn, ih)

	// 11. Active connections (from pg_stat_activity)
	m.collectActiveConnections(ctx, conn, ih)

	// 12. Per-database sizes and table statistics
	m.collectDatabaseStats(ctx, conn, ih, host, password)

	// 13. Slow queries (pg_stat_statements — requires extension)
	m.collectSlowQueries(ctx, conn, ih)

	return ih
}

// checkPrimary gathers primary-specific health metrics.
func (m *Monitor) checkPrimary(ctx context.Context, conn *pgx.Conn, ih *pgswarmv1.InstanceHealth) {
	// Max replication lag across all standbys
	var lag int64
	err := conn.QueryRow(ctx,
		"SELECT COALESCE(MAX(pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn)), 0)::bigint FROM pg_stat_replication",
	).Scan(&lag)
	if err != nil {
		log.Debug().Err(err).Str("pod", ih.PodName).Msg("failed to read primary replication lag")
	} else {
		ih.ReplicationLagBytes = lag
	}
}

// checkReplica gathers replica-specific health metrics.
func (m *Monitor) checkReplica(ctx context.Context, conn *pgx.Conn, ih *pgswarmv1.InstanceHealth) {
	// Byte lag
	var lag int64
	err := conn.QueryRow(ctx,
		"SELECT COALESCE(pg_wal_lsn_diff(pg_last_wal_receive_lsn(), pg_last_wal_replay_lsn()), 0)::bigint",
	).Scan(&lag)
	if err != nil {
		log.Debug().Err(err).Str("pod", ih.PodName).Msg("failed to read replica byte lag")
	} else {
		ih.ReplicationLagBytes = lag
	}

	// Time-based lag (seconds since last replayed transaction)
	var lastReplay *time.Time
	if err := conn.QueryRow(ctx, "SELECT pg_last_xact_replay_timestamp()").Scan(&lastReplay); err != nil {
		log.Debug().Err(err).Str("pod", ih.PodName).Msg("failed to read replay timestamp")
	} else if lastReplay != nil {
		ih.ReplicationLagSeconds = time.Since(*lastReplay).Seconds()
	}

	// WAL receiver streaming status
	var walStatus *string
	if err := conn.QueryRow(ctx, "SELECT status FROM pg_stat_wal_receiver LIMIT 1").Scan(&walStatus); err != nil {
		// No rows means no WAL receiver (disconnected from primary)
		ih.WalReceiverActive = false
	} else {
		ih.WalReceiverActive = walStatus != nil && *walStatus == "streaming"
	}
}

// collectWalStats reads WAL write statistics from pg_stat_wal.
func (m *Monitor) collectWalStats(ctx context.Context, conn *pgx.Conn, ih *pgswarmv1.InstanceHealth) {
	err := conn.QueryRow(ctx,
		"SELECT COALESCE(wal_records, 0)::bigint, COALESCE(wal_bytes, 0)::bigint, COALESCE(wal_buffers_full, 0)::bigint FROM pg_stat_wal",
	).Scan(&ih.WalRecords, &ih.WalBytes, &ih.WalBuffersFull)
	if err != nil {
		log.Debug().Err(err).Str("pod", ih.PodName).Msg("failed to read WAL stats")
	}
}

// collectWalDiskSize reads the total WAL directory size from pg_ls_waldir().
func (m *Monitor) collectWalDiskSize(ctx context.Context, conn *pgx.Conn, ih *pgswarmv1.InstanceHealth) {
	var walSize int64
	if err := conn.QueryRow(ctx, "SELECT COALESCE(SUM(size), 0)::bigint FROM pg_ls_waldir()").Scan(&walSize); err != nil {
		log.Debug().Err(err).Str("pod", ih.PodName).Msg("failed to read WAL disk size")
	} else {
		ih.WalDiskBytes = walSize
	}
}

// collectIndexHitRatio reads the index hit ratio from pg_statio_user_indexes.
// A value below 0.95 indicates indexes are being read from disk frequently.
func (m *Monitor) collectIndexHitRatio(ctx context.Context, conn *pgx.Conn, ih *pgswarmv1.InstanceHealth) {
	var ratio float64
	err := conn.QueryRow(ctx,
		"SELECT COALESCE(SUM(idx_blks_hit)::float / NULLIF(SUM(idx_blks_hit) + SUM(idx_blks_read), 0), 0) FROM pg_statio_user_indexes",
	).Scan(&ratio)
	if err != nil {
		log.Debug().Err(err).Str("pod", ih.PodName).Msg("failed to read index hit ratio")
	} else {
		ih.IndexHitRatio = ratio
	}
}

// collectTxnCommitRatio reads the transaction commit ratio from pg_stat_database.
// A low ratio may indicate application errors, deadlocks, or excessive rollbacks.
func (m *Monitor) collectTxnCommitRatio(ctx context.Context, conn *pgx.Conn, ih *pgswarmv1.InstanceHealth) {
	var ratio float64
	err := conn.QueryRow(ctx,
		"SELECT COALESCE(xact_commit::float / NULLIF(xact_commit + xact_rollback, 0), 0) FROM pg_stat_database WHERE datname = current_database()",
	).Scan(&ratio)
	if err != nil {
		log.Debug().Err(err).Str("pod", ih.PodName).Msg("failed to read txn commit ratio")
	} else {
		ih.TxnCommitRatio = ratio
	}
}

// collectActiveConnections counts connections with state='active' from pg_stat_activity.
func (m *Monitor) collectActiveConnections(ctx context.Context, conn *pgx.Conn, ih *pgswarmv1.InstanceHealth) {
	var active int32
	err := conn.QueryRow(ctx,
		"SELECT COUNT(*)::int FROM pg_stat_activity WHERE state = 'active'",
	).Scan(&active)
	if err != nil {
		log.Debug().Err(err).Str("pod", ih.PodName).Msg("failed to read active connections")
	} else {
		ih.ConnectionsActive = active
	}
}

// collectDatabaseStats collects per-database sizes, cache hit ratios, and table statistics.
func (m *Monitor) collectDatabaseStats(ctx context.Context, conn *pgx.Conn, ih *pgswarmv1.InstanceHealth, host, password string) {
	rows, err := conn.Query(ctx,
		"SELECT datname, pg_database_size(datname)::bigint FROM pg_database WHERE NOT datistemplate ORDER BY pg_database_size(datname) DESC")
	if err != nil {
		log.Debug().Err(err).Str("pod", ih.PodName).Msg("failed to read database sizes")
		return
	}
	defer rows.Close()

	for rows.Next() {
		ds := &pgswarmv1.DatabaseStat{}
		if err := rows.Scan(&ds.DatabaseName, &ds.SizeBytes); err != nil {
			log.Debug().Err(err).Str("pod", ih.PodName).Msg("failed to scan database stat row")
			continue
		}
		ih.DatabaseStats = append(ih.DatabaseStats, ds)
	}
	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Str("pod", ih.PodName).Msg("error iterating database stat rows")
	}

	// Collect table stats + cache hit ratio from each database (limit to 5)
	limit := 5
	if len(ih.DatabaseStats) < limit {
		limit = len(ih.DatabaseStats)
	}
	for _, ds := range ih.DatabaseStats[:limit] {
		m.collectTablesForDB(ctx, ih, ds, host, password)
	}
}

// collectTablesForDB opens a connection to a specific database, collects table stats
// and the database-level cache hit ratio.
func (m *Monitor) collectTablesForDB(ctx context.Context, ih *pgswarmv1.InstanceHealth, ds *pgswarmv1.DatabaseStat, host, password string) {
	connStr := fmt.Sprintf("postgres://postgres:%s@%s:5432/%s?connect_timeout=3&sslmode=disable",
		url.QueryEscape(password), host, url.PathEscape(ds.DatabaseName))

	connCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	conn, err := pgx.Connect(connCtx, connStr)
	if err != nil {
		log.Debug().Err(err).Str("pod", ih.PodName).Str("db", ds.DatabaseName).Msg("failed to connect for table stats")
		return
	}
	defer conn.Close(ctx)

	// Cache hit ratio from pg_statio_user_tables
	var hits, reads int64
	if err := conn.QueryRow(ctx, `
		SELECT COALESCE(SUM(heap_blks_hit + idx_blks_hit + toast_blks_hit + tidx_blks_hit), 0)::bigint,
		       COALESCE(SUM(heap_blks_read + idx_blks_read + toast_blks_read + tidx_blks_read), 0)::bigint
		FROM pg_statio_user_tables`).Scan(&hits, &reads); err != nil {
		log.Debug().Err(err).Str("pod", ih.PodName).Str("db", ds.DatabaseName).Msg("failed to read cache hit ratio")
	} else if hits+reads > 0 {
		ds.CacheHitRatio = float64(hits) / float64(hits+reads)
	}

	// Table stats
	rows, err := conn.Query(ctx, `
		SELECT schemaname, relname,
			n_live_tup::bigint, n_dead_tup::bigint,
			seq_scan::bigint, COALESCE(idx_scan, 0)::bigint,
			n_tup_ins::bigint, n_tup_upd::bigint, n_tup_del::bigint,
			COALESCE(to_char(last_vacuum, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), ''),
			COALESCE(to_char(last_autovacuum, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), ''),
			pg_total_relation_size(relid)::bigint
		FROM pg_stat_user_tables
		ORDER BY pg_total_relation_size(relid) DESC
		LIMIT 30`)
	if err != nil {
		log.Debug().Err(err).Str("pod", ih.PodName).Str("db", ds.DatabaseName).Msg("failed to read table stats")
		return
	}
	defer rows.Close()

	for rows.Next() {
		ts := &pgswarmv1.TableStat{DatabaseName: ds.DatabaseName}
		if err := rows.Scan(
			&ts.SchemaName, &ts.TableName,
			&ts.LiveTuples, &ts.DeadTuples,
			&ts.SeqScan, &ts.IdxScan,
			&ts.NTupIns, &ts.NTupUpd, &ts.NTupDel,
			&ts.LastVacuum, &ts.LastAutovacuum,
			&ts.TableSizeBytes,
		); err != nil {
			log.Debug().Err(err).Str("pod", ih.PodName).Str("db", ds.DatabaseName).Msg("failed to scan table stat row")
			continue
		}
		ih.TableStats = append(ih.TableStats, ts)
	}
	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Str("pod", ih.PodName).Str("db", ds.DatabaseName).Msg("error iterating table stat rows")
	}
}

// collectSlowQueries reads top slow queries from pg_stat_statements (if available).
func (m *Monitor) collectSlowQueries(ctx context.Context, conn *pgx.Conn, ih *pgswarmv1.InstanceHealth) {
	rows, err := conn.Query(ctx, `
		SELECT s.query, d.datname,
			s.calls::bigint,
			s.total_exec_time,
			s.mean_exec_time,
			s.max_exec_time,
			s.rows::bigint
		FROM pg_stat_statements s
		JOIN pg_database d ON s.dbid = d.oid
		WHERE s.query NOT LIKE 'SHOW%'
		  AND s.query NOT LIKE '%pg_stat%'
		  AND s.query NOT LIKE '%pg_database%'
		  AND s.query NOT LIKE '%pg_ls_waldir%'
		  AND s.calls > 0
		ORDER BY s.mean_exec_time DESC
		LIMIT 10`)
	if err != nil {
		// pg_stat_statements extension may not be installed — not an error
		log.Debug().Err(err).Str("pod", ih.PodName).Msg("pg_stat_statements not available (extension may not be loaded)")
		return
	}
	defer rows.Close()

	for rows.Next() {
		sq := &pgswarmv1.SlowQuery{}
		if err := rows.Scan(
			&sq.Query, &sq.DatabaseName,
			&sq.Calls,
			&sq.TotalExecTimeMs, &sq.MeanExecTimeMs, &sq.MaxExecTimeMs,
			&sq.Rows,
		); err != nil {
			log.Debug().Err(err).Str("pod", ih.PodName).Msg("failed to scan slow query row")
			continue
		}
		ih.SlowQueries = append(ih.SlowQueries, sq)
	}
	if err := rows.Err(); err != nil {
		log.Debug().Err(err).Str("pod", ih.PodName).Msg("error iterating slow query rows")
	}
}

// DeriveClusterState determines the overall cluster state from instance health.
// The age parameter indicates how long the cluster has been observed. Clusters
// that are not yet fully running within clusterStartGrace (10 minutes) report
// CREATING instead of FAILED, giving pods time to pull images and initialise.
func DeriveClusterState(instances []*pgswarmv1.InstanceHealth, expectedReplicas int32, age time.Duration) pgswarmv1.ClusterState {
	if len(instances) == 0 {
		if age < clusterStartGrace {
			return pgswarmv1.ClusterState_CLUSTER_STATE_CREATING
		}
		return pgswarmv1.ClusterState_CLUSTER_STATE_FAILED
	}

	var primaryReady bool
	var readyCount int32

	for _, inst := range instances {
		if inst.Role == pgswarmv1.InstanceRole_INSTANCE_ROLE_PRIMARY && inst.Ready {
			primaryReady = true
		}
		if inst.Ready {
			readyCount++
		}
	}

	if !primaryReady {
		if age < clusterStartGrace {
			return pgswarmv1.ClusterState_CLUSTER_STATE_CREATING
		}
		return pgswarmv1.ClusterState_CLUSTER_STATE_FAILED
	}

	if readyCount >= expectedReplicas && int32(len(instances)) >= expectedReplicas {
		return pgswarmv1.ClusterState_CLUSTER_STATE_RUNNING
	}

	return pgswarmv1.ClusterState_CLUSTER_STATE_DEGRADED
}

func isPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func severityForTransition(newState pgswarmv1.ClusterState) string {
	switch newState {
	case pgswarmv1.ClusterState_CLUSTER_STATE_RUNNING:
		return "info"
	case pgswarmv1.ClusterState_CLUSTER_STATE_DEGRADED,
		pgswarmv1.ClusterState_CLUSTER_STATE_PAUSED:
		return "warning"
	case pgswarmv1.ClusterState_CLUSTER_STATE_FAILED:
		return "error"
	default:
		return "info"
	}
}
