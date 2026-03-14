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
type Monitor struct {
	client     kubernetes.Interface
	operator   *operator.Operator
	onHealth   func(*pgswarmv1.ClusterHealthReport)
	onEvent    func(*pgswarmv1.EventReport)
	interval   time.Duration
	lastStates map[string]pgswarmv1.ClusterState
}

// New creates a new health Monitor.
func New(client kubernetes.Interface, op *operator.Operator, interval time.Duration) *Monitor {
	return &Monitor{
		client:     client,
		operator:   op,
		interval:   interval,
		lastStates: make(map[string]pgswarmv1.ClusterState),
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

// Run starts the health check loop. It blocks until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) {
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
			m.checkAll(ctx)
		}
	}
}

func (m *Monitor) checkAll(ctx context.Context) {
	clusters := m.operator.ManagedClusters()
	for _, mc := range clusters {
		report := m.checkCluster(ctx, mc)
		if report == nil {
			continue
		}

		if m.onHealth != nil {
			m.onHealth(report)
		}

		// Detect state transitions and emit events
		key := mc.Namespace + "/" + mc.ClusterName
		prev, existed := m.lastStates[key]
		if existed && prev != report.State {
			if m.onEvent != nil {
				m.onEvent(&pgswarmv1.EventReport{
					ClusterName: mc.ClusterName,
					Severity:    severityForTransition(report.State),
					Message:     fmt.Sprintf("cluster state changed: %s -> %s", prev, report.State),
					Source:      "health-monitor",
					Timestamp:   timestamppb.Now(),
				})
			}
		}
		m.lastStates[key] = report.State
	}
}

func (m *Monitor) checkCluster(ctx context.Context, mc operator.ManagedCluster) *pgswarmv1.ClusterHealthReport {
	checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	pods, err := m.client.CoreV1().Pods(mc.Namespace).List(checkCtx, metav1.ListOptions{
		LabelSelector: "pg-swarm.io/cluster=" + mc.ClusterName,
	})
	if err != nil {
		log.Warn().Err(err).Str("cluster", mc.ClusterName).Msg("failed to list pods for health check")
		return nil
	}

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

	state := DeriveClusterState(instances, mc.Replicas)

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

// DeriveClusterState determines the overall cluster state from instance health.
func DeriveClusterState(instances []*pgswarmv1.InstanceHealth, expectedReplicas int32) pgswarmv1.ClusterState {
	if len(instances) == 0 {
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
	case pgswarmv1.ClusterState_CLUSTER_STATE_DEGRADED:
		return "warning"
	case pgswarmv1.ClusterState_CLUSTER_STATE_FAILED:
		return "error"
	default:
		return "info"
	}
}
