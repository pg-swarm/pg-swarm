package failover

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
	corev1 "k8s.io/api/core/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/pg-swarm/pg-swarm/internal/shared/pgfence"
)

const (
	leaseDuration = 15 * time.Second

	// rewindGracePeriod is how long we wait with WAL receiver down before
	// attempting pg_rewind / re-basebackup. This gives PG time to reconnect
	// on its own after a normal primary switchover.
	rewindGracePeriod = 30 * time.Second

	labelRole    = "pg-swarm.io/role"
	rolePrimary  = "primary"
	roleReplica  = "replica"
)

// Config holds the failover monitor configuration.
type Config struct {
	PodName             string
	Namespace           string
	ClusterName         string
	Interval            time.Duration
	PGConnString        string // e.g. "host=localhost user=postgres password=xxx dbname=postgres"
	RestConfig          *rest.Config
	ReplicationPassword string // for primary_conninfo when demoting
}

// Monitor watches the local PostgreSQL instance and manages leader election.
type Monitor struct {
	cfg        Config
	client     kubernetes.Interface
	leaseName  string
	fenceFunc  func(ctx context.Context, db pgfence.PGExecer) error // nil = pgfence.FencePrimary
	demoteFunc func(ctx context.Context) error                      // nil = real demotePrimary
	rewindFunc func(ctx context.Context) error                      // nil = real rewindOrReinit

	// walReceiverDownSince tracks when the WAL receiver was first observed as
	// inactive on a replica. Zero means the receiver is active (or we haven't
	// checked yet). After rewindGracePeriod, we attempt pg_rewind / re-basebackup.
	walReceiverDownSince time.Time
}

// NewMonitor creates a new failover monitor.
func NewMonitor(cfg Config, client kubernetes.Interface) *Monitor {
	return &Monitor{
		cfg:       cfg,
		client:    client,
		leaseName: fmt.Sprintf("%s-leader", cfg.ClusterName),
	}
}

// Run starts the monitoring loop. It blocks until the context is cancelled.
func (m *Monitor) Run(ctx context.Context) error {
	log.Info().
		Str("pod", m.cfg.PodName).
		Str("cluster", m.cfg.ClusterName).
		Dur("interval", m.cfg.Interval).
		Msg("failover monitor starting")

	ticker := time.NewTicker(m.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.tick(ctx)
		}
	}
}

func (m *Monitor) tick(ctx context.Context) {
	conn, err := pgx.Connect(ctx, m.cfg.PGConnString)
	if err != nil {
		log.Warn().Err(err).Msg("cannot connect to local PostgreSQL")
		return
	}
	defer conn.Close(ctx)

	var isInRecovery bool
	if err := conn.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&isInRecovery); err != nil {
		log.Warn().Err(err).Msg("pg_is_in_recovery() failed")
		return
	}

	if !isInRecovery {
		m.handlePrimary(ctx, conn)
	} else {
		m.handleReplica(ctx, conn)
	}
}

// handlePrimary renews the leader lease and labels the pod.
// If another pod holds the lease (split-brain) or the lease cannot be
// verified, PG is fenced to prevent writes.
func (m *Monitor) handlePrimary(ctx context.Context, conn *pgx.Conn) {
	acquired, err := m.acquireOrRenew(ctx)
	if err != nil {
		// Cannot verify lease ownership — fence as a safety measure.
		// A primary that can't reach the K8s API must not keep accepting writes
		// because another pod may have already acquired the lease and promoted.
		log.Error().Err(err).Msg("lease operation failed — fencing as precaution")
		m.doFence(ctx, conn)
		return
	}

	if acquired {
		// We hold the lease. Check if PG is still fenced from a previous
		// split-brain (the ALTER SYSTEM setting persists in postgresql.auto.conf
		// across sidecar restarts). Unfence if so.
		if pgfence.IsFenced(ctx, conn) {
			log.Info().Msg("legitimate primary is fenced — unfencing")
			if err := pgfence.UnfencePrimary(ctx, conn); err != nil {
				log.Error().Err(err).Msg("failed to unfence primary")
			}
		}
		m.labelPod(ctx, rolePrimary)
	} else {
		// Split-brain: PG thinks it's primary but lease says otherwise.
		// Fence first (blocks writes immediately), then demote (converts to standby).
		log.Error().
			Str("pod", m.cfg.PodName).
			Msg("SPLIT-BRAIN: running as PG primary but another pod holds the leader lease — fencing, demoting, and labeling as replica")
		m.doFence(ctx, conn)
		m.doDemote(ctx)
		m.labelPod(ctx, roleReplica)
	}
}

// doFence calls the fence function (real or injected for tests).
func (m *Monitor) doFence(ctx context.Context, conn *pgx.Conn) {
	fence := m.fenceFunc
	if fence == nil {
		fence = pgfence.FencePrimary
	}
	if err := fence(ctx, conn); err != nil {
		log.Error().Err(err).Msg("fencing failed")
	}
}

// doDemote calls the demote function (real or injected for tests).
func (m *Monitor) doDemote(ctx context.Context) {
	demote := m.demoteFunc
	if demote == nil {
		demote = m.demotePrimary
	}
	if err := demote(ctx); err != nil {
		log.Error().Err(err).Msg("demotion failed — PG is fenced but still running as primary; will retry next tick")
	}
}

// demotePrimary uses K8s exec to convert the local PG instance to a standby.
// It creates standby.signal, sets primary_conninfo pointing at the RW service,
// and stops PG. K8s will restart the container and PG comes up as a standby.
//
// Using the RW service (which selects pg-swarm.io/role=primary) instead of a
// specific pod hostname means the standby always streams from whoever is the
// current primary, even after subsequent failovers.
func (m *Monitor) demotePrimary(ctx context.Context) error {
	if m.cfg.RestConfig == nil {
		return fmt.Errorf("rest.Config not set — cannot exec into container")
	}

	// Point primary_conninfo at the RW service — it follows the primary label.
	rwSvc := fmt.Sprintf("%s-rw", m.cfg.ClusterName)
	primaryHost := fmt.Sprintf("%s.%s.svc.cluster.local", rwSvc, m.cfg.Namespace)

	replPassword := m.cfg.ReplicationPassword

	pgdata := "/var/lib/postgresql/data/pgdata"
	script := fmt.Sprintf(`set -e
PGDATA="%s"
touch "$PGDATA/standby.signal"
sed -i '/^primary_conninfo/d' "$PGDATA/postgresql.auto.conf" 2>/dev/null || true
echo "primary_conninfo = 'host=%s port=5432 user=repl_user password=%s application_name=%s'" >> "$PGDATA/postgresql.auto.conf"
pg_ctl -D "$PGDATA" stop -m fast`,
		pgdata, primaryHost, replPassword, m.cfg.PodName)

	req := m.client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(m.cfg.PodName).
		Namespace(m.cfg.Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "postgres",
			Command:   []string{"bash", "-c", script},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(m.cfg.RestConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("create SPDY executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return fmt.Errorf("exec demote script: %w (stderr: %s)", err, stderr.String())
	}

	log.Info().
		Str("pod", m.cfg.PodName).
		Str("primary_host", primaryHost).
		Msg("demotion completed — PG will restart as standby")

	return nil
}

// handleReplica labels the pod as replica, checks WAL receiver health,
// and initiates failover if the leader lease has expired.
func (m *Monitor) handleReplica(ctx context.Context, conn *pgx.Conn) {
	m.labelPod(ctx, roleReplica)

	// Check if WAL receiver is actively streaming from the primary.
	m.checkWalReceiver(ctx, conn)

	expired, err := m.isLeaseExpired(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("failed to check leader lease")
		return
	}

	if !expired {
		return
	}

	log.Info().Msg("leader lease expired — attempting failover")
	acquired, err := m.acquireOrRenew(ctx)
	if err != nil {
		log.Error().Err(err).Msg("failed to acquire lease for failover")
		return
	}
	if !acquired {
		log.Info().Msg("another replica acquired the lease first")
		return
	}

	log.Info().Msg("lease acquired — promoting to primary")
	if err := m.promote(ctx); err != nil {
		log.Error().Err(err).Msg("pg_promote() failed")
		return
	}

	m.labelPod(ctx, rolePrimary)
	m.walReceiverDownSince = time.Time{} // reset on promotion
	log.Info().Msg("promotion successful — now primary")
}

// checkWalReceiver monitors whether this replica is actively streaming WAL.
// If the WAL receiver has been down for longer than rewindGracePeriod and a
// valid primary exists (lease not expired), we attempt pg_rewind to re-sync
// with the new timeline, falling back to a full re-basebackup.
//
// If the replica's timeline doesn't match the primary's (fatal divergence),
// we skip the grace period and trigger recovery immediately.
func (m *Monitor) checkWalReceiver(ctx context.Context, conn *pgx.Conn) {
	var active bool
	err := conn.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM pg_stat_wal_receiver WHERE status = 'streaming')").Scan(&active)
	if err != nil {
		log.Warn().Err(err).Msg("failed to check pg_stat_wal_receiver")
		return
	}

	if active {
		if !m.walReceiverDownSince.IsZero() {
			log.Info().Msg("WAL receiver reconnected — replication restored")
		}
		m.walReceiverDownSince = time.Time{}
		return
	}

	// WAL receiver is not streaming. Check if there's a valid primary first —
	// if the lease is expired, failover logic below will handle it.
	expired, err := m.isLeaseExpired(ctx)
	if err != nil || expired {
		return
	}

	// Check for timeline divergence — this is fatal and will never self-heal.
	// The replica's timeline won't match the new primary's after a promotion.
	if m.hasTimelineDivergence(ctx, conn) {
		log.Warn().Msg("timeline divergence detected — triggering immediate recovery")
		m.doRewind(ctx)
		m.walReceiverDownSince = time.Time{}
		return
	}

	// No timeline divergence — use grace period for transient issues.
	now := time.Now()
	if m.walReceiverDownSince.IsZero() {
		m.walReceiverDownSince = now
		log.Warn().Msg("WAL receiver not streaming — starting grace period")
		return
	}

	downFor := now.Sub(m.walReceiverDownSince)
	if downFor < rewindGracePeriod {
		log.Warn().
			Dur("down_for", downFor).
			Dur("grace_period", rewindGracePeriod).
			Msg("WAL receiver still down — waiting before recovery")
		return
	}

	log.Warn().
		Dur("down_for", downFor).
		Msg("WAL receiver down beyond grace period — attempting pg_rewind / re-basebackup")

	m.doRewind(ctx)
	m.walReceiverDownSince = time.Time{} // reset so we wait again if it fails
}

// hasTimelineDivergence checks if this replica's timeline differs from the
// primary's. After a promotion, the new primary moves to timeline N+1 while
// other replicas are still on timeline N. PG logs "requested timeline X is
// not a child of this server's history" and will never recover on its own.
func (m *Monitor) hasTimelineDivergence(ctx context.Context, conn *pgx.Conn) bool {
	// Get our current timeline from pg_control_checkpoint().
	var localTimeline int64
	err := conn.QueryRow(ctx,
		"SELECT timeline_id FROM pg_control_checkpoint()").Scan(&localTimeline)
	if err != nil {
		log.Debug().Err(err).Msg("failed to get local timeline")
		return false
	}

	// Get the primary's timeline from the lease holder.
	// We can't query the primary directly from the sidecar, but we can check
	// if the WAL receiver's last known timeline (from its conninfo) differs.
	// A simpler heuristic: if WAL receiver is absent/not streaming AND our
	// timeline differs from what pg_stat_wal_receiver last reported, we've diverged.
	var receiverTimeline *int64
	err = conn.QueryRow(ctx,
		"SELECT received_tli FROM pg_stat_wal_receiver LIMIT 1").Scan(&receiverTimeline)
	if err != nil || receiverTimeline == nil {
		// No WAL receiver row at all — PG may have never connected to the new primary.
		// Check if there are multiple timeline history files as a signal of promotion.
		var historyCount int64
		err = conn.QueryRow(ctx,
			"SELECT count(*) FROM pg_ls_waldir() WHERE name LIKE '%.history'").Scan(&historyCount)
		if err != nil {
			log.Debug().Err(err).Msg("failed to check timeline history files")
			return false
		}
		// If we have fewer history files than our timeline - 1, we're missing
		// the new timeline's history file (it was never streamed to us).
		// Timeline 1 has 0 history files, timeline 2 has 1, etc.
		expectedHistories := localTimeline - 1
		if historyCount < expectedHistories {
			log.Warn().
				Int64("local_timeline", localTimeline).
				Int64("history_files", historyCount).
				Int64("expected", expectedHistories).
				Msg("missing timeline history files — divergence likely")
			return true
		}
		return false
	}

	if *receiverTimeline != localTimeline {
		log.Warn().
			Int64("local_timeline", localTimeline).
			Int64("receiver_timeline", *receiverTimeline).
			Msg("timeline mismatch between local and receiver")
		return true
	}

	return false
}

// doRewind calls the rewind function (real or injected for tests).
func (m *Monitor) doRewind(ctx context.Context) {
	rewind := m.rewindFunc
	if rewind == nil {
		rewind = m.rewindOrReinit
	}
	if err := rewind(ctx); err != nil {
		log.Error().Err(err).Msg("rewind/re-basebackup failed — will retry after grace period")
	}
}

// rewindOrReinit uses K8s exec to run pg_rewind against the current primary.
// If pg_rewind fails (e.g. diverged too far), falls back to a full re-basebackup.
// In both cases PG is stopped and K8s restarts the container.
func (m *Monitor) rewindOrReinit(ctx context.Context) error {
	if m.cfg.RestConfig == nil {
		return fmt.Errorf("rest.Config not set — cannot exec into container")
	}

	rwSvc := fmt.Sprintf("%s-rw", m.cfg.ClusterName)
	primaryHost := fmt.Sprintf("%s.%s.svc.cluster.local", rwSvc, m.cfg.Namespace)
	replPassword := m.cfg.ReplicationPassword

	// Extract the postgres superuser password from PGConnString for pg_rewind.
	// PGConnString format: "host=localhost ... password=XXX ..."
	pgPassword := extractPassword(m.cfg.PGConnString)

	pgdata := "/var/lib/postgresql/data/pgdata"

	// Try pg_rewind first — it's fast and preserves most of PGDATA.
	// Falls back to wiping PGDATA and running pg_basebackup if rewind fails.
	// NOTE: no `set -e` — we need the fallback to run if pg_rewind fails.
	// pg_rewind uses the postgres superuser because it needs pg_read_binary_file(),
	// pg_ls_dir(), and pg_stat_file() which require superuser privileges.
	script := fmt.Sprintf(`
PGDATA="%s"
PRIMARY_HOST="%s"
REPL_PASSWORD="%s"
PG_PASSWORD="%s"
POD_NAME="%s"

echo "Stopping PostgreSQL for recovery..."
pg_ctl -D "$PGDATA" stop -m fast -w 2>/dev/null || true

echo "Attempting pg_rewind..."
if PGPASSWORD="$PG_PASSWORD" pg_rewind \
    -D "$PGDATA" \
    --source-server="host=$PRIMARY_HOST port=5432 user=postgres password=$PG_PASSWORD dbname=postgres" \
    --progress 2>&1; then
    echo "pg_rewind succeeded"
else
    echo "pg_rewind failed — falling back to full re-basebackup"

    rm -rf "$PGDATA"/*

    PGPASSWORD="$REPL_PASSWORD" pg_basebackup \
        -h "$PRIMARY_HOST" -U repl_user -D "$PGDATA" -R -Xs -P

    if [ $? -ne 0 ]; then
        echo "FATAL: re-basebackup also failed"
        exit 1
    fi

    # Restore config files from mounted configmap
    cp /etc/pg-config/postgresql.conf "$PGDATA/postgresql.conf" 2>/dev/null || true
    cp /etc/pg-config/pg_hba.conf "$PGDATA/pg_hba.conf" 2>/dev/null || true
fi

# Ensure standby.signal and primary_conninfo are set correctly
touch "$PGDATA/standby.signal"
sed -i '/^primary_conninfo/d' "$PGDATA/postgresql.auto.conf" 2>/dev/null || true
echo "primary_conninfo = 'host=$PRIMARY_HOST port=5432 user=repl_user password=$REPL_PASSWORD application_name=$POD_NAME'" >> "$PGDATA/postgresql.auto.conf"

echo "Recovery complete — PG will restart as standby"
# Do NOT start PG here — K8s will restart the container
`, pgdata, primaryHost, replPassword, pgPassword, m.cfg.PodName)

	req := m.client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(m.cfg.PodName).
		Namespace(m.cfg.Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "postgres",
			Command:   []string{"bash", "-c", script},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(m.cfg.RestConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("create SPDY executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return fmt.Errorf("exec rewind script: %w (stdout: %s, stderr: %s)", err, stdout.String(), stderr.String())
	}

	log.Info().
		Str("pod", m.cfg.PodName).
		Str("primary_host", primaryHost).
		Str("output", stdout.String()).
		Msg("rewind/re-basebackup completed — PG will restart as standby")

	return nil
}

// promote calls pg_promote() on the local PostgreSQL instance.
func (m *Monitor) promote(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, m.cfg.PGConnString)
	if err != nil {
		return fmt.Errorf("connect for promote: %w", err)
	}
	defer conn.Close(ctx)

	_, err = conn.Exec(ctx, "SELECT pg_promote()")
	return err
}

// labelPod patches the pod's pg-swarm.io/role label.
func (m *Monitor) labelPod(ctx context.Context, role string) {
	patch := map[string]any{
		"metadata": map[string]any{
			"labels": map[string]string{
				labelRole: role,
			},
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		log.Error().Err(err).Msg("failed to marshal label patch")
		return
	}

	_, err = m.client.CoreV1().Pods(m.cfg.Namespace).Patch(
		ctx, m.cfg.PodName, types.MergePatchType, patchBytes, metav1.PatchOptions{},
	)
	if err != nil {
		log.Warn().Err(err).Str("role", role).Msg("failed to patch pod label")
	}
}

// acquireOrRenew attempts to acquire or renew the leader lease.
// Returns true if this pod now holds the lease.
func (m *Monitor) acquireOrRenew(ctx context.Context) (bool, error) {
	lease, err := m.client.CoordinationV1().Leases(m.cfg.Namespace).Get(ctx, m.leaseName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return m.createLease(ctx)
	}
	if err != nil {
		return false, fmt.Errorf("get lease: %w", err)
	}

	// We hold it — renew
	if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity == m.cfg.PodName {
		now := metav1.NewMicroTime(time.Now())
		lease.Spec.RenewTime = &now
		_, err = m.client.CoordinationV1().Leases(m.cfg.Namespace).Update(ctx, lease, metav1.UpdateOptions{})
		if err != nil {
			return false, fmt.Errorf("renew lease: %w", err)
		}
		return true, nil
	}

	// Someone else holds it — check if expired
	if !leaseExpired(lease) {
		return false, nil
	}

	// Expired — try to take over (optimistic locking via resourceVersion)
	now := metav1.NewMicroTime(time.Now())
	lease.Spec.HolderIdentity = &m.cfg.PodName
	lease.Spec.AcquireTime = &now
	lease.Spec.RenewTime = &now
	_, err = m.client.CoordinationV1().Leases(m.cfg.Namespace).Update(ctx, lease, metav1.UpdateOptions{})
	if apierrors.IsConflict(err) {
		return false, nil // another pod won the race
	}
	if err != nil {
		return false, fmt.Errorf("acquire expired lease: %w", err)
	}
	return true, nil
}

func (m *Monitor) createLease(ctx context.Context) (bool, error) {
	now := metav1.NewMicroTime(time.Now())
	dur := int32(leaseDuration.Seconds())
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.leaseName,
			Namespace: m.cfg.Namespace,
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &m.cfg.PodName,
			LeaseDurationSeconds: &dur,
			AcquireTime:          &now,
			RenewTime:            &now,
		},
	}
	_, err := m.client.CoordinationV1().Leases(m.cfg.Namespace).Create(ctx, lease, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("create lease: %w", err)
	}
	return true, nil
}

// isLeaseExpired checks if the leader lease has expired (or doesn't exist).
func (m *Monitor) isLeaseExpired(ctx context.Context) (bool, error) {
	lease, err := m.client.CoordinationV1().Leases(m.cfg.Namespace).Get(ctx, m.leaseName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil // no lease = expired
	}
	if err != nil {
		return false, fmt.Errorf("get lease: %w", err)
	}
	return leaseExpired(lease), nil
}

func leaseExpired(lease *coordinationv1.Lease) bool {
	if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		return true
	}
	expiry := lease.Spec.RenewTime.Time.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second)
	return time.Now().After(expiry)
}

// extractPassword parses "password=XXX" from a libpq-style connection string.
func extractPassword(connStr string) string {
	for _, part := range strings.Fields(connStr) {
		if strings.HasPrefix(part, "password=") {
			return strings.TrimPrefix(part, "password=")
		}
	}
	return ""
}
