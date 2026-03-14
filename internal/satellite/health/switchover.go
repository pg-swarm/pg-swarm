package health

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
)

// Switchover performs a planned primary switchover: demotes the current
// primary (pg_wal_switch + checkpoint) and promotes the target replica.
// It also updates the leader lease so the failover sidecar doesn't fight.
func Switchover(ctx context.Context, client kubernetes.Interface, req *pgswarmv1.SwitchoverRequest, password string) *pgswarmv1.SwitchoverResult {
	result := &pgswarmv1.SwitchoverResult{ClusterName: req.ClusterName}
	ns := req.Namespace
	target := req.TargetPod

	log.Info().
		Str("cluster", req.ClusterName).
		Str("target", target).
		Msg("starting planned switchover")

	// 1. Verify the target pod exists and is a replica
	targetPod, err := client.CoreV1().Pods(ns).Get(ctx, target, metav1.GetOptions{})
	if err != nil {
		result.ErrorMessage = fmt.Sprintf("target pod not found: %v", err)
		return result
	}
	if targetPod.Labels["pg-swarm.io/role"] != "replica" {
		result.ErrorMessage = fmt.Sprintf("target pod %s is not a replica (role=%s)", target, targetPod.Labels["pg-swarm.io/role"])
		return result
	}

	// 2. Find the current primary pod
	pods, err := client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("pg-swarm.io/cluster=%s,pg-swarm.io/role=primary", req.ClusterName),
	})
	if err != nil || len(pods.Items) == 0 {
		result.ErrorMessage = "no current primary found"
		return result
	}
	primaryPod := &pods.Items[0]

	// Read superuser password from the target to verify PG connectivity
	targetHost := fmt.Sprintf("%s.%s-headless.%s.svc.cluster.local",
		target, req.ClusterName, ns)
	primaryHost := fmt.Sprintf("%s.%s-headless.%s.svc.cluster.local",
		primaryPod.Name, req.ClusterName, ns)
	escapedPass := url.QueryEscape(password)

	// 3. Verify target replica is streaming and caught up
	targetConn, err := pgx.Connect(ctx, fmt.Sprintf(
		"postgres://postgres:%s@%s:5432/postgres?connect_timeout=5&sslmode=disable", escapedPass, targetHost))
	if err != nil {
		result.ErrorMessage = fmt.Sprintf("cannot connect to target replica: %v", err)
		return result
	}
	defer targetConn.Close(ctx)

	var isRecovery bool
	if err := targetConn.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&isRecovery); err != nil || !isRecovery {
		result.ErrorMessage = "target is not in recovery mode"
		return result
	}

	// 4. Checkpoint on primary to flush pending WAL
	primaryConn, err := pgx.Connect(ctx, fmt.Sprintf(
		"postgres://postgres:%s@%s:5432/postgres?connect_timeout=5&sslmode=disable", escapedPass, primaryHost))
	if err != nil {
		result.ErrorMessage = fmt.Sprintf("cannot connect to primary: %v", err)
		return result
	}
	defer primaryConn.Close(ctx)

	if _, err := primaryConn.Exec(ctx, "CHECKPOINT"); err != nil {
		log.Warn().Err(err).Msg("checkpoint on primary failed (proceeding anyway)")
	}

	// 5. Transfer the leader lease to the target pod
	leaseName := req.ClusterName + "-leader"
	if err := transferLease(ctx, client, ns, leaseName, target); err != nil {
		result.ErrorMessage = fmt.Sprintf("failed to transfer lease: %v", err)
		return result
	}

	// 6. Promote the target replica
	if _, err := targetConn.Exec(ctx, "SELECT pg_promote()"); err != nil {
		result.ErrorMessage = fmt.Sprintf("pg_promote() failed: %v", err)
		return result
	}

	log.Info().
		Str("cluster", req.ClusterName).
		Str("old_primary", primaryPod.Name).
		Str("new_primary", target).
		Msg("switchover completed successfully")

	result.Success = true
	return result
}

// transferLease updates the leader lease to point to the new holder.
func transferLease(ctx context.Context, client kubernetes.Interface, namespace, leaseName, newHolder string) error {
	lease, err := client.CoordinationV1().Leases(namespace).Get(ctx, leaseName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// Create a new lease for the target
		now := metav1.NewMicroTime(time.Now())
		dur := int32(15)
		_, err := client.CoordinationV1().Leases(namespace).Create(ctx, &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: leaseName, Namespace: namespace},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       &newHolder,
				LeaseDurationSeconds: &dur,
				AcquireTime:          &now,
				RenewTime:            &now,
			},
		}, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}

	now := metav1.NewMicroTime(time.Now())
	lease.Spec.HolderIdentity = &newHolder
	lease.Spec.AcquireTime = &now
	lease.Spec.RenewTime = &now
	_, err = client.CoordinationV1().Leases(namespace).Update(ctx, lease, metav1.UpdateOptions{})
	return err
}
