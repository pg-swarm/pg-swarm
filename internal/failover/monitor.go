package failover

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	defaultLeaseDuration = 5 * time.Second

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
	PGConnString        string        // e.g. "host=localhost user=postgres password=xxx dbname=postgres"
	RestConfig          *rest.Config
	ReplicationPassword string        // for primary_conninfo when demoting
	PrimaryHost         string        // RW service DNS for reachability check
	PGPassword          string        // superuser password for reachability check
	LeaseDuration       time.Duration // configurable, default 5s
}

// Monitor watches the local PostgreSQL instance and manages leader election.
type Monitor struct {
	cfg        Config
	client     kubernetes.Interface
	leaseName  string
	leaseDur   time.Duration
	fenceFunc  func(ctx context.Context, db pgfence.PGExecer) error // nil = pgfence.FencePrimary
	demoteFunc func(ctx context.Context) error                      // nil = real demotePrimary
	rewindFunc func(ctx context.Context) error                      // nil = real rewindOrReinit

	// walReceiverDownSince tracks when the WAL receiver was first observed as
	// inactive on a replica. Zero means the receiver is active (or we haven't
	// checked yet). After rewindGracePeriod, we attempt pg_rewind / re-basebackup.
	walReceiverDownSince time.Time

	// primaryUnreachableCount tracks consecutive failed reachability checks.
	primaryUnreachableCount int
	// primaryCheckFunc overrides isPrimaryReachable for testing (nil = real check).
	primaryCheckFunc func(ctx context.Context) bool
}

// NewMonitor creates a new failover monitor.
func NewMonitor(cfg Config, client kubernetes.Interface) *Monitor {
	ld := cfg.LeaseDuration
	if ld <= 0 {
		ld = defaultLeaseDuration
	}
	return &Monitor{
		cfg:       cfg,
		client:    client,
		leaseName: fmt.Sprintf("%s-leader", cfg.ClusterName),
		leaseDur:  ld,
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

// tick runs a single iteration of the monitoring loop: connects to PG,
// determines whether the local instance is primary or replica, and delegates
// to the appropriate handler.
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

	pgdata := "/var/lib/postgresql/data/pgdata"
	// Passwords are read from container env vars (REPLICATION_PASSWORD) set in
	// the StatefulSet manifest, avoiding shell injection via interpolated values.
	script := fmt.Sprintf(`set -e
PGDATA="%s"
touch "$PGDATA/standby.signal"
sed -i '/^primary_conninfo/d' "$PGDATA/postgresql.auto.conf" 2>/dev/null || true
echo "primary_conninfo = 'host=%s port=5432 user=repl_user password=$REPLICATION_PASSWORD application_name=%s'" >> "$PGDATA/postgresql.auto.conf"
pg_ctl -D "$PGDATA" stop -m fast`,
		pgdata, primaryHost, m.cfg.PodName)

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

// isPrimaryReachable checks whether the primary is reachable via a fast
// TCP+auth connection attempt. Returns true if a connection succeeds.
func (m *Monitor) isPrimaryReachable(ctx context.Context) bool {
	if m.primaryCheckFunc != nil {
		return m.primaryCheckFunc(ctx)
	}
	if m.cfg.PrimaryHost == "" {
		return true // no primary host configured — skip reachability check
	}
	checkCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	conn, err := pgx.Connect(checkCtx, fmt.Sprintf(
		"host=%s port=5432 user=postgres password=%s dbname=postgres connect_timeout=1",
		m.cfg.PrimaryHost, m.cfg.PGPassword))
	if err != nil {
		return false
	}
	conn.Close(checkCtx)
	return true
}

// handleReplica labels the pod as replica, checks WAL receiver health,
// and initiates failover if the leader lease has expired.
//
// Fast-path: if the primary is reachable, we skip the lease check entirely
// (happy path — no K8s API call needed). If the primary is unreachable for
// 3+ consecutive ticks AND the lease is expired, we promote.
func (m *Monitor) handleReplica(ctx context.Context, conn *pgx.Conn) {
	m.labelPod(ctx, roleReplica)

	// Check if WAL receiver is actively streaming from the primary.
	if conn != nil {
		m.checkWalReceiver(ctx, conn)
	}

	// Fast-path reachability check: if the primary responds, skip lease check.
	if m.isPrimaryReachable(ctx) {
		m.primaryUnreachableCount = 0
		return
	}

	m.primaryUnreachableCount++
	log.Warn().
		Int("count", m.primaryUnreachableCount).
		Msg("primary unreachable")

	if m.primaryUnreachableCount < 3 {
		return // not enough evidence yet
	}

	// Primary has been unreachable for 3+ ticks — check the lease.
	expired, err := m.isLeaseExpired(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("failed to check leader lease")
		return
	}

	if !expired {
		log.Warn().Msg("primary unreachable but lease still valid — possible network partition to this replica")
		return
	}

	log.Info().Msg("leader lease expired and primary unreachable — attempting failover")
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
	m.primaryUnreachableCount = 0
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
	if err != nil {
		log.Warn().Err(err).Msg("failed to check leader lease for WAL recovery decision")
		return
	}
	if expired {
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

// rewindOrReinit sets up standby.signal and primary_conninfo, then stops PG.
// The wrapper loop in the main container detects the exit and calls
// pg_swarm_recover() which handles the actual pg_rewind / re-basebackup
// before restarting PG. This avoids a race between the exec script and the
// wrapper both trying to modify PGDATA concurrently.
func (m *Monitor) rewindOrReinit(ctx context.Context) error {
	if m.cfg.RestConfig == nil {
		return fmt.Errorf("rest.Config not set — cannot exec into container")
	}

	rwSvc := fmt.Sprintf("%s-rw", m.cfg.ClusterName)
	primaryHost := fmt.Sprintf("%s.%s.svc.cluster.local", rwSvc, m.cfg.Namespace)

	pgdata := "/var/lib/postgresql/data/pgdata"

	// Set standby.signal and primary_conninfo so the wrapper's pg_swarm_recover
	// can detect timeline divergence and run pg_rewind. Then stop PG so the
	// wrapper loop wakes up and handles recovery.
	//
	// Passwords are read from container env vars (REPLICATION_PASSWORD) set in
	// the StatefulSet manifest, avoiding shell injection via interpolated values.
	script := fmt.Sprintf(`set -e
PGDATA="%s"
touch "$PGDATA/standby.signal"
sed -i '/^primary_conninfo/d' "$PGDATA/postgresql.auto.conf" 2>/dev/null || true
echo "primary_conninfo = 'host=%s port=5432 user=repl_user password=$REPLICATION_PASSWORD application_name=%s'" >> "$PGDATA/postgresql.auto.conf"
pg_ctl -D "$PGDATA" stop -m fast`,
		pgdata, primaryHost, m.cfg.PodName)

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
		Msg("PG stopped for recovery — wrapper will handle pg_rewind and restart")

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

// createLease creates a new leader lease owned by this pod. Returns false
// if the lease already exists (another pod created it first).
func (m *Monitor) createLease(ctx context.Context) (bool, error) {
	now := metav1.NewMicroTime(time.Now())
	dur := int32(m.leaseDur.Seconds())
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

// leaseExpired returns true if the given lease's renew time plus its duration
// is in the past, or if the lease is missing required fields.
func leaseExpired(lease *coordinationv1.Lease) bool {
	if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		return true
	}
	expiry := lease.Spec.RenewTime.Time.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second)
	return time.Now().After(expiry)
}

