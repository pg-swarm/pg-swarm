package failover

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const (
	leaseDuration = 15 * time.Second

	labelRole    = "pg-swarm.io/role"
	rolePrimary  = "primary"
	roleReplica  = "replica"
)

// Config holds the failover monitor configuration.
type Config struct {
	PodName      string
	Namespace    string
	ClusterName  string
	Interval     time.Duration
	PGConnString string // e.g. "host=localhost user=postgres password=xxx dbname=postgres"
}

// Monitor watches the local PostgreSQL instance and manages leader election.
type Monitor struct {
	cfg       Config
	client    kubernetes.Interface
	leaseName string
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
		m.handlePrimary(ctx)
	} else {
		m.handleReplica(ctx)
	}
}

// handlePrimary renews the leader lease and labels the pod.
// If another pod holds the lease (split-brain), labels self as replica.
func (m *Monitor) handlePrimary(ctx context.Context) {
	acquired, err := m.acquireOrRenew(ctx)
	if err != nil {
		log.Error().Err(err).Msg("lease operation failed")
		return
	}

	if acquired {
		m.labelPod(ctx, rolePrimary)
	} else {
		// Split-brain: PG thinks it's primary but lease says otherwise
		log.Error().
			Str("pod", m.cfg.PodName).
			Msg("SPLIT-BRAIN: running as PG primary but another pod holds the leader lease — labeling as replica")
		m.labelPod(ctx, roleReplica)
	}
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
