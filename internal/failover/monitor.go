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
	leaseDuration = 15 * time.Second

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
		m.handleReplica(ctx)
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
// It creates standby.signal, sets primary_conninfo, and stops PG.
// K8s will restart the container and PG will come up as a streaming standby.
func (m *Monitor) demotePrimary(ctx context.Context) error {
	if m.cfg.RestConfig == nil {
		return fmt.Errorf("rest.Config not set — cannot exec into container")
	}

	// Determine the new primary from the lease holder
	lease, err := m.client.CoordinationV1().Leases(m.cfg.Namespace).Get(ctx, m.leaseName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get lease for demote: %w", err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
		return fmt.Errorf("lease has no holder — cannot determine new primary")
	}
	newPrimary := *lease.Spec.HolderIdentity

	// Build headless service hostname for the new primary
	headlessSvc := fmt.Sprintf("%s-headless", m.cfg.ClusterName)
	primaryHost := fmt.Sprintf("%s.%s.%s.svc.cluster.local", newPrimary, headlessSvc, m.cfg.Namespace)

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
		Str("new_primary", newPrimary).
		Msg("demotion completed — PG will restart as standby")

	return nil
}

// handleReplica labels the pod as replica and checks if failover is needed.
func (m *Monitor) handleReplica(ctx context.Context) {
	m.labelPod(ctx, roleReplica)

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
	log.Info().Msg("promotion successful — now primary")
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
