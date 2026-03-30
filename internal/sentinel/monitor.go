package sentinel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rs/zerolog/log"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/pg-swarm/pg-swarm/internal/sentinel/backup"
	"github.com/pg-swarm/pg-swarm/internal/shared/pgfence"
)

const (
	defaultLeaseDuration = 5 * time.Second

	// rewindGracePeriod is how long we wait with WAL receiver down before
	// attempting pg_rewind / re-basebackup. This gives PG time to reconnect
	// on its own after a normal primary switchover.
	rewindGracePeriod = 30 * time.Second

	// crashLoopThreshold is how many consecutive local PG connection failures
	// before we consider the primary to be crash-looping and stop renewing the
	// leader lease so that a secondary can take over.
	crashLoopThreshold = 3

	// stableUpThreshold is how many consecutive healthy ticks a crash-looping
	// primary must achieve before we consider it recovered and resume lease
	// renewal. This prevents a single transient startup from resetting the
	// crash-loop detection.
	stableUpThreshold = 3

	labelRole   = "pg-swarm.io/role"
	rolePrimary = "primary"
	roleReplica = "replica"
)

// Config holds the sentinel monitor configuration.
type Config struct {
	PodName             string
	Namespace           string
	ClusterName         string
	Interval            time.Duration
	PGConnString        string // e.g. "host=localhost user=postgres password=xxx dbname=postgres"
	RestConfig          *rest.Config
	ReplicationPassword string        // for primary_conninfo when demoting
	PrimaryHost         string        // RW service DNS for reachability check
	PGPassword          string        // superuser password for reachability check
	LeaseDuration       time.Duration // configurable, default 5s
	RecoveryRulesPath   string        // path to mounted recovery rules JSON file
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
	lastReplayLSN        string

	// primaryUnreachableCount tracks consecutive failed reachability checks.
	primaryUnreachableCount int
	// primaryCheckFunc overrides isPrimaryReachable for testing (nil = real check).
	primaryCheckFunc func(ctx context.Context) bool

	// localPGDownCount tracks consecutive failures to connect to the local PG
	// instance. Used to detect a crash-looping primary so we can stop renewing
	// the leader lease and allow a secondary to take over.
	localPGDownCount int
	// consecutiveHealthyTicks tracks how many ticks in a row the local PG has
	// been reachable as primary after a crash-loop. We require stableUpThreshold
	// consecutive healthy ticks before resuming lease renewal.
	consecutiveHealthyTicks int

	// wasConnected is set true after the first successful postgres connection.
	// Used to distinguish "PGDATA deleted at runtime" from "postgres hasn't started yet".
	wasConnected bool

	// wasPrimary tracks whether the last successful PG connection was to a
	// primary (not in recovery). When PG goes down and wasPrimary is true,
	// we immediately clear the role=primary label so the RW service stops
	// routing to this pod — even before the replica detects the failure.
	wasPrimary bool

	// zeroPrimaryCount tracks consecutive ticks where no pod in the cluster
	// has role=primary. After 5 ticks (~25s), triggers emergency promotion.
	zeroPrimaryCount int

	// eventEmitter sends log-rule detection events to the satellite.
	// Set via SetEventEmitter before Run.
	eventEmitter EventEmitter

	// backupManager coordinates backup/restore operations.
	// Set via SetBackupManager before Run.
	backupManager *backup.Manager
}

// NewMonitor creates a new sentinel monitor.
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

// SetEventEmitter sets the emitter used by the logwatcher to send detection
// events to the satellite. Must be called before Run.
func (m *Monitor) SetEventEmitter(e EventEmitter) {
	m.eventEmitter = e
}

// SetBackupManager sets the backup manager. Must be called before Run.
func (m *Monitor) SetBackupManager(bm *backup.Manager) {
	m.backupManager = bm
}

// Run starts the monitoring loop. It blocks until the context is cancelled.
func (m *Monitor) Run(ctx context.Context) error {
	log.Info().
		Str("pod", m.cfg.PodName).
		Str("cluster", m.cfg.ClusterName).
		Dur("interval", m.cfg.Interval).
		Msg("sentinel monitor starting")

	// Start log watcher for recovery rules (non-blocking)
	if m.cfg.RecoveryRulesPath != "" {
		lw := NewLogWatcher(m.client, m.eventEmitter, m.cfg.RecoveryRulesPath, m.cfg.PodName, m.cfg.Namespace, m.cfg.ClusterName)
		go lw.Run(ctx)
	}

	// Start backup manager (non-blocking)
	if m.backupManager != nil {
		go m.backupManager.Run(ctx)
	}

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

const (
	pgDataDir              = "/var/lib/postgresql/data/pgdata"
	pgVersionFile          = pgDataDir + "/PG_VERSION"
	pgStandbySignal        = pgDataDir + "/standby.signal"
	pgSwarmNeedsBasebackup = "/var/lib/postgresql/data/.pg-swarm-needs-basebackup"
)

// tick runs a single iteration of the monitoring loop: connects to PG,
// determines whether the local instance is primary or replica, and delegates
// to the appropriate handler.
func (m *Monitor) tick(ctx context.Context) {
	// Proactively detect PGDATA deletion. If we were connected before (postgres
	// was healthy) but PG_VERSION is now absent, PGDATA was wiped at runtime.
	// Write the re-basebackup marker and stop renewing the lease so a replica
	// promotes immediately. The wrapper will re-basebackup from the new primary.
	if m.wasConnected {
		if _, err := os.Stat(pgVersionFile); os.IsNotExist(err) {
			log.Error().Msg("PGDATA is gone while postgres was running — yielding lease for failover; pod will re-basebackup from new primary")
			_ = os.WriteFile(pgSwarmNeedsBasebackup, nil, 0644)
			if m.wasPrimary {
				m.labelPod(ctx, roleReplica)
			}
			m.wasConnected = false
			m.wasPrimary = false
			return
		}
	}

	conn, err := pgx.Connect(ctx, m.cfg.PGConnString)
	if err != nil {
		// 57P03 = cannot_connect_now: PG is still replaying WAL after startup.
		// This is expected on replicas and should not be treated as a failure.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "57P03" {
			log.Debug().Msg("postgres is starting up, waiting for WAL replay to complete")

			// Even during WAL replay, check for zero-primary deadlock.
			// A standby stuck waiting for WAL from a nonexistent primary
			// will stay in 57P03 forever — we must break the deadlock.
			if count := m.countClusterPrimaries(ctx); count == 0 {
				m.zeroPrimaryCount++
				if m.zeroPrimaryCount >= 5 {
					expired, _ := m.isLeaseExpired(ctx)
					if expired || m.leaseHeldBySelf(ctx) {
						if _, err := m.acquireOrRenew(ctx); err == nil {
							log.Error().Int("ticks", m.zeroPrimaryCount).
								Msg("EMERGENCY: zero primaries, PG stuck in WAL replay — forcing primary")
							m.execInContainer(ctx, "pg_ctl stop -m immediate -D "+pgDataDir)
							os.Remove(pgSwarmNeedsBasebackup)
							os.Remove(pgStandbySignal)
							m.labelPod(ctx, rolePrimary)
							m.zeroPrimaryCount = 0
							m.wasConnected = false // suppress PGDATA detector re-writing marker
						}
					}
				}
			} else {
				m.zeroPrimaryCount = 0
			}
			return
		}
		m.localPGDownCount++
		m.consecutiveHealthyTicks = 0
		log.Warn().Err(err).Int("down_count", m.localPGDownCount).Msg("cannot connect to local PostgreSQL")

		// If we were the primary, immediately clear the primary label so the
		// RW service stops routing traffic to this pod. This is critical:
		// without it, the pod keeps role=primary for 15-20s until a replica
		// promotes and relabels us — during which a restarted PG could accept
		// divergent writes.
		if m.wasPrimary {
			log.Warn().Msg("primary PG is down — clearing primary label to prevent split-brain")
			m.labelPod(ctx, roleReplica)
		}

		// Zero-primary deadlock breaker: runs even when local PG is down.
		// If ALL pods are stuck in basebackup loops with no primary, the
		// sidecar that holds (or acquires) the lease removes the markers
		// so the wrapper can start PG as primary instead of looping.
		if count := m.countClusterPrimaries(ctx); count == 0 {
			m.zeroPrimaryCount++
			if m.zeroPrimaryCount >= 5 {
				expired, _ := m.isLeaseExpired(ctx)
				if expired || m.leaseHeldBySelf(ctx) {
					if _, err := m.acquireOrRenew(ctx); err == nil {
						log.Error().Int("ticks", m.zeroPrimaryCount).
							Msg("EMERGENCY: zero primaries and PG down — removing markers to force primary startup")
						os.Remove(pgSwarmNeedsBasebackup)
						os.Remove(pgStandbySignal)
						m.labelPod(ctx, rolePrimary)
						m.zeroPrimaryCount = 0
						m.wasConnected = false // suppress PGDATA detector re-writing marker
					}
				}
			}
		} else {
			m.zeroPrimaryCount = 0
		}
		return
	}
	defer conn.Close(ctx)

	var isInRecovery bool
	if err := conn.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&isInRecovery); err != nil {
		m.localPGDownCount++
		m.consecutiveHealthyTicks = 0
		log.Warn().Err(err).Msg("pg_is_in_recovery() failed")
		return
	}

	m.wasConnected = true
	m.wasPrimary = !isInRecovery

	// Notify backup manager of current role
	if m.backupManager != nil {
		m.backupManager.SetRole(!isInRecovery)
	}

	if !isInRecovery {
		m.handlePrimary(ctx, conn)
	} else {
		// Replicas reset crash-loop tracking — it only applies to primaries.
		m.localPGDownCount = 0
		m.consecutiveHealthyTicks = 0
		m.handleReplica(ctx, conn)
	}
}

// handlePrimary renews the leader lease and labels the pod.
// If another pod holds the lease (split-brain) or the lease cannot be
// verified, PG is fenced to prevent writes.
func (m *Monitor) handlePrimary(ctx context.Context, conn *pgx.Conn) {
	m.consecutiveHealthyTicks++
	m.zeroPrimaryCount = 0 // we are a primary, so at least one exists

	// Crash-loop detection: if PG was recently down multiple times, don't
	// renew the lease until it has been stable for stableUpThreshold ticks.
	// This allows the lease to expire so a secondary can promote.
	if m.localPGDownCount >= crashLoopThreshold && m.consecutiveHealthyTicks < stableUpThreshold {
		log.Error().
			Int("down_count", m.localPGDownCount).
			Int("healthy_ticks", m.consecutiveHealthyTicks).
			Msg("primary is crash-looping — skipping lease renewal to allow failover")
		return
	}
	if m.consecutiveHealthyTicks >= stableUpThreshold {
		m.localPGDownCount = 0
	}

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
// It retries up to 3 times with 500ms between attempts.
func (m *Monitor) doFence(ctx context.Context, conn *pgx.Conn) {
	fence := m.fenceFunc
	if fence == nil {
		fence = pgfence.FencePrimary
	}
	const maxRetries = 3
	var err error
	for i := range maxRetries {
		if err = fence(ctx, conn); err == nil {
			return
		}
		log.Warn().Err(err).Int("attempt", i+1).Msg("fence attempt failed")
		if i < maxRetries-1 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	log.Error().Err(err).Msg("fencing failed after retries")
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

// execStandbyConversion uses K8s exec to convert the local PG instance to a
// standby. It creates standby.signal, sets primary_conninfo pointing at the RW
// service (which follows pg-swarm.io/role=primary), and stops PG so K8s
// restarts it as a standby.
//
// Used by both demotePrimary (split-brain recovery) and rewindOrReinit
// (timeline divergence recovery) — the shell steps are identical.
//
// Passwords are read from the container's REPLICATION_PASSWORD env var (set in
// the StatefulSet manifest), avoiding shell injection via Go-interpolated values.
func (m *Monitor) execStandbyConversion(ctx context.Context, successMsg string) error {
	if m.cfg.RestConfig == nil {
		return fmt.Errorf("rest.Config not set — cannot exec into container")
	}

	rwSvc := fmt.Sprintf("%s-rw", m.cfg.ClusterName)
	primaryHost := fmt.Sprintf("%s.%s.svc.cluster.local", rwSvc, m.cfg.Namespace)

	pgdata := "/var/lib/postgresql/data/pgdata"
	script := fmt.Sprintf(`set -e
PGDATA="%s"
touch "$PGDATA/standby.signal"
sed -i '/^primary_conninfo/d' "$PGDATA/postgresql.auto.conf" 2>/dev/null || true
sed -i '/^default_transaction_read_only/d' "$PGDATA/postgresql.auto.conf" 2>/dev/null || true
echo "primary_conninfo = 'host=%s port=5432 user=repl_user password=$REPLICATION_PASSWORD application_name=%s'" >> "$PGDATA/postgresql.auto.conf"
if [ -f "$PGDATA/backup_label" ]; then echo "pg-swarm: removing stale backup_label"; rm -f "$PGDATA/backup_label"; fi
if [ -f "$PGDATA/tablespace_map" ]; then echo "pg-swarm: removing stale tablespace_map"; rm -f "$PGDATA/tablespace_map"; fi
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
		return fmt.Errorf("exec standby conversion: %w (stdout: %s, stderr: %s)", err, stdout.String(), stderr.String())
	}

	log.Info().
		Str("pod", m.cfg.PodName).
		Str("primary_host", primaryHost).
		Msg(successMsg)

	return nil
}

// demotePrimary uses K8s exec to convert the local PG instance to a standby.
func (m *Monitor) demotePrimary(ctx context.Context) error {
	return m.execStandbyConversion(ctx, "demotion completed — PG will restart as standby")
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

	// Zero-primary safety net: if no pod in the cluster has role=primary
	// for 5+ consecutive ticks, force-promote to break the deadlock.
	if count := m.countClusterPrimaries(ctx); count == 0 {
		m.zeroPrimaryCount++
		if m.zeroPrimaryCount >= 5 {
			log.Error().Int("ticks", m.zeroPrimaryCount).
				Msg("EMERGENCY: zero primaries for 5+ ticks — attempting force promotion")
			if acquired, _ := m.acquireOrRenew(ctx); acquired {
				if err := m.promote(ctx); err == nil {
					m.labelPod(ctx, rolePrimary)
					m.zeroPrimaryCount = 0
					m.primaryUnreachableCount = 0
					log.Info().Msg("emergency promotion successful")
					return
				}
				log.Error().Msg("emergency pg_promote() failed — will retry next tick")
			}
		}
	} else {
		m.zeroPrimaryCount = 0
	}

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
		log.Error().Err(err).Msg("pg_promote() failed — removing standby.signal for retry")
		// Remove standby.signal so PG doesn't restart as a standby that
		// waits forever for WAL from a nonexistent primary. On the next
		// wrapper restart, PG will come up as primary.
		os.Remove(pgStandbySignal)
		return
	}

	// Clear the primary label from all other pods AFTER promote succeeds,
	// then immediately label ourselves. The old primary is already unreachable
	// (3 ticks + expired lease confirmed), so the brief ~100ms window where
	// both pods have the label is harmless — no traffic reaches the old pod.
	// Doing this after promote avoids a deadlock where all pods become
	// replicas if promote fails.
	m.clearPrimaryLabels(ctx)
	m.labelPod(ctx, rolePrimary)

	m.primaryUnreachableCount = 0
	m.zeroPrimaryCount = 0
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
func (m *Monitor) checkWalReceiver(ctx context.Context, conn pgfence.PGExecer) {
	var active bool
	err := conn.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM pg_stat_wal_receiver WHERE status = 'streaming')").Scan(&active)
	if err != nil {
		log.Warn().Err(err).Msg("failed to check pg_stat_wal_receiver")
		return
	}

	var replayLSN string
	if err := conn.QueryRow(ctx, "SELECT pg_last_wal_replay_lsn()").Scan(&replayLSN); err != nil {
		replayLSN = ""
	}

	if active {
		if !m.walReceiverDownSince.IsZero() {
			log.Info().Msg("WAL receiver reconnected — replication restored")
		}
		m.walReceiverDownSince = time.Time{}
		m.lastReplayLSN = replayLSN
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
		if !m.isPrimaryReachable(ctx) {
			log.Warn().Msg("timeline divergence detected but primary unreachable — skipping destructive recovery")
			return
		}
		log.Warn().Msg("timeline divergence detected — triggering immediate recovery")
		m.doRewind(ctx)
		m.walReceiverDownSince = time.Time{}
		m.lastReplayLSN = ""
		return
	}

	// No timeline divergence — use grace period for transient issues.
	now := time.Now()
	if m.walReceiverDownSince.IsZero() {
		m.walReceiverDownSince = now
		m.lastReplayLSN = replayLSN
		log.Warn().Msg("WAL receiver not streaming — starting grace period")
		return
	}

	// If replay LSN advanced while WAL receiver is down, archive recovery is making progress!
	// Reset the timer so we don't interrupt it.
	if replayLSN != "" && replayLSN != m.lastReplayLSN {
		log.Debug().Msg("WAL receiver down but replay LSN advancing (archive recovery active) — resetting grace period")
		m.walReceiverDownSince = now
		m.lastReplayLSN = replayLSN
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

	if !m.isPrimaryReachable(ctx) {
		log.Warn().
			Dur("down_for", downFor).
			Msg("WAL receiver down beyond grace period but primary unreachable — skipping destructive recovery")
		return
	}

	log.Warn().
		Dur("down_for", downFor).
		Msg("WAL receiver down beyond grace period — attempting pg_rewind / re-basebackup")

	if m.hasWalGap(ctx, conn, replayLSN) {
		log.Warn().Msg("WAL gap detected — forcing full re-basebackup")
		m.doReBasebackup(ctx)
	} else {
		m.doRewind(ctx)
	}
	m.walReceiverDownSince = time.Time{} // reset so we wait again if it fails
	m.lastReplayLSN = ""
}

// hasTimelineDivergence checks if this replica's timeline differs from the
// primary's. After a promotion, the new primary moves to timeline N+1 while
// other replicas are still on timeline N. PG logs "requested timeline X is
// not a child of this server's history" and will never recover on its own.
func (m *Monitor) hasTimelineDivergence(ctx context.Context, conn pgfence.PGExecer) bool {
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

// hasWalGap checks if this replica's required WAL segment is missing from the primary.
func (m *Monitor) hasWalGap(ctx context.Context, conn pgfence.PGExecer, replayLSN string) bool {
	if replayLSN == "" {
		return false
	}

	primaryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	pConn, err := pgx.Connect(primaryCtx, fmt.Sprintf(
		"host=%s port=5432 user=postgres password=%s dbname=postgres connect_timeout=2",
		m.cfg.PrimaryHost, m.cfg.PGPassword))
	if err != nil {
		return false
	}
	defer pConn.Close(primaryCtx)

	var walFile string
	err = pConn.QueryRow(primaryCtx, "SELECT pg_walfile_name($1)", replayLSN).Scan(&walFile)
	if err != nil {
		return false
	}

	var exists bool
	err = pConn.QueryRow(primaryCtx, "SELECT EXISTS(SELECT 1 FROM pg_ls_waldir() WHERE name = $1)", walFile).Scan(&exists)
	if err != nil {
		return false
	}

	// If the file exists on primary, no gap (replica can stream it).
	return !exists
}

// doReBasebackup creates the needs-basebackup marker and stops PG so the wrapper handles it.
func (m *Monitor) doReBasebackup(ctx context.Context) {
	if m.cfg.RestConfig == nil {
		log.Error().Msg("rest.Config not set — cannot exec into container for basebackup")
		return
	}

	pgdata := "/var/lib/postgresql/data/pgdata"
	marker := "/var/lib/postgresql/data/.pg-swarm-needs-basebackup"
	script := fmt.Sprintf(`set -e
touch "%s"
pg_ctl -D "%s" stop -m fast`, marker, pgdata)

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
		log.Error().Err(err).Msg("failed to create exec for basebackup")
		return
	}

	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		log.Error().Err(err).Str("stdout", stdout.String()).Str("stderr", stderr.String()).Msg("failed to exec basebackup script")
		return
	}

	log.Info().Str("pod", m.cfg.PodName).Msg("PG stopped for WAL gap recovery — wrapper will handle re-basebackup")
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
// before restarting PG.
func (m *Monitor) rewindOrReinit(ctx context.Context) error {
	return m.execStandbyConversion(ctx, "PG stopped for recovery — wrapper will handle pg_rewind and restart")
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

// clearPrimaryLabels removes the primary role label from ALL pods in this
// cluster except this pod. Called before promotion to guarantee there is never
// more than one pod with role=primary. Creates a brief "no primary" window
// (service returns no endpoints) which is safe — clients retry connections.
// countClusterPrimaries returns how many pods in this cluster have role=primary
// AND are actually running with all containers ready. A pod stuck in a
// basebackup loop has role=primary label but isn't serving — it shouldn't count.
func (m *Monitor) countClusterPrimaries(ctx context.Context) int {
	selector := fmt.Sprintf("pg-swarm.io/cluster=%s,pg-swarm.io/role=%s", m.cfg.ClusterName, rolePrimary)
	pods, err := m.client.CoreV1().Pods(m.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return -1 // unknown, don't trigger emergency
	}
	ready := 0
	for _, pod := range pods.Items {
		if pod.Status.Phase != "Running" {
			continue
		}
		allReady := true
		for _, cs := range pod.Status.ContainerStatuses {
			if !cs.Ready {
				allReady = false
				break
			}
		}
		if allReady {
			ready++
		}
	}
	return ready
}

func (m *Monitor) clearPrimaryLabels(ctx context.Context) {
	labelSelector := fmt.Sprintf("pg-swarm.io/cluster=%s,pg-swarm.io/role=%s", m.cfg.ClusterName, rolePrimary)
	pods, err := m.client.CoreV1().Pods(m.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		log.Warn().Err(err).Msg("failed to list primary pods for label cleanup")
		return
	}
	for _, pod := range pods.Items {
		if pod.Name == m.cfg.PodName {
			continue // don't relabel ourselves
		}
		log.Info().Str("pod", pod.Name).Msg("clearing primary label from old primary")
		m.labelRemotePod(ctx, pod.Name, roleReplica)
	}
}

// labelRemotePod patches the role label on another pod in the same namespace.
// Used during promotion to immediately relabel the old primary as replica,
// eliminating the split-brain window where both pods carry the primary label.
func (m *Monitor) labelRemotePod(ctx context.Context, podName, role string) {
	patch := map[string]any{
		"metadata": map[string]any{
			"labels": map[string]string{
				labelRole: role,
			},
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		log.Error().Err(err).Msg("failed to marshal remote label patch")
		return
	}

	_, err = m.client.CoreV1().Pods(m.cfg.Namespace).Patch(
		ctx, podName, types.MergePatchType, patchBytes, metav1.PatchOptions{},
	)
	if err != nil {
		log.Warn().Err(err).Str("pod", podName).Str("role", role).Msg("failed to relabel remote pod")
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
// leaseHeldBySelf returns true if this pod currently holds the leader lease.
func (m *Monitor) leaseHeldBySelf(ctx context.Context) bool {
	lease, err := m.client.CoordinationV1().Leases(m.cfg.Namespace).Get(ctx, m.leaseName, metav1.GetOptions{})
	if err != nil {
		return false
	}
	return lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity == m.cfg.PodName
}

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
