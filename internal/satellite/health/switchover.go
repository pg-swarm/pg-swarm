package health

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/pg-swarm/pg-swarm/internal/satellite/sidecar"
)

// Switchover performs a planned primary switchover: fences the current
// primary (via sidecar stream), runs a checkpoint, transfers the lease,
// and promotes the target replica (via sidecar stream).
func Switchover(ctx context.Context, client kubernetes.Interface, req *pgswarmv1.SwitchoverRequest, streams *sidecar.SidecarStreamManager) *pgswarmv1.SwitchoverResult {
	log.Trace().Str("cluster", req.ClusterName).Str("target", req.TargetPod).Str("namespace", req.Namespace).Msg("Switchover entry")
	result := &pgswarmv1.SwitchoverResult{ClusterName: req.ClusterName}
	ns := req.Namespace
	target := req.TargetPod

	log.Info().
		Str("cluster", req.ClusterName).
		Str("target", target).
		Msg("starting planned switchover")

	// 1. Verify the target pod exists and is a replica
	log.Trace().Str("target", target).Msg("Switchover: verifying target pod")
	targetPod, err := client.CoreV1().Pods(ns).Get(ctx, target, metav1.GetOptions{})
	if err != nil {
		result.ErrorMessage = fmt.Sprintf("target pod not found: %v", err)
		return result
	}
	if targetPod.Labels["pg-swarm.io/role"] != "replica" {
		result.ErrorMessage = fmt.Sprintf("target pod %s is not a replica (role=%s)", target, targetPod.Labels["pg-swarm.io/role"])
		return result
	}

	log.Trace().Msg("Switchover: target verified as replica")
	// 2. Find the current primary pod
	pods, err := client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("pg-swarm.io/cluster=%s,pg-swarm.io/role=primary", req.ClusterName),
	})
	if err != nil || len(pods.Items) == 0 {
		result.ErrorMessage = "no current primary found"
		return result
	}
	primaryPod := &pods.Items[0]
	log.Trace().Str("primary", primaryPod.Name).Msg("Switchover: current primary found")

	// 3. Verify target is in recovery via sidecar stream
	log.Trace().Msg("Switchover: checking target status via sidecar stream")
	statusCtx, statusCancel := context.WithTimeout(ctx, 10*time.Second)
	defer statusCancel()
	statusResult, err := streams.SendCommandAndWait(statusCtx, ns, target, &pgswarmv1.SidecarCommand{
		Cmd: &pgswarmv1.SidecarCommand_Status{Status: &pgswarmv1.StatusCmd{}},
	})
	if err != nil {
		result.ErrorMessage = fmt.Sprintf("cannot check target status: %v", err)
		return result
	}
	if !statusResult.Success {
		result.ErrorMessage = fmt.Sprintf("target status check failed: %s", statusResult.Error)
		return result
	}
	if !statusResult.InRecovery {
		result.ErrorMessage = "target is not in recovery mode"
		return result
	}

	// 4a. Fence the old primary via sidecar stream
	log.Trace().Msg("Switchover: fencing old primary via sidecar stream")
	fenceCtx, fenceCancel := context.WithTimeout(ctx, 15*time.Second)
	defer fenceCancel()
	fenceResult, err := streams.SendCommandAndWait(fenceCtx, ns, primaryPod.Name, &pgswarmv1.SidecarCommand{
		Cmd: &pgswarmv1.SidecarCommand_Fence{Fence: &pgswarmv1.FenceCmd{DrainTimeoutSeconds: 5}},
	})
	if err != nil {
		result.ErrorMessage = fmt.Sprintf("fencing old primary failed: %v", err)
		return result
	}
	if !fenceResult.Success {
		result.ErrorMessage = fmt.Sprintf("fencing old primary failed: %s", fenceResult.Error)
		return result
	}

	// 4b. Checkpoint on primary via sidecar stream
	log.Trace().Msg("Switchover: running checkpoint on primary via sidecar stream")
	cpCtx, cpCancel := context.WithTimeout(ctx, 30*time.Second)
	defer cpCancel()
	cpResult, err := streams.SendCommandAndWait(cpCtx, ns, primaryPod.Name, &pgswarmv1.SidecarCommand{
		Cmd: &pgswarmv1.SidecarCommand_Checkpoint{Checkpoint: &pgswarmv1.CheckpointCmd{}},
	})
	if err != nil {
		// Rollback: unfence primary
		log.Warn().Err(err).Msg("Switchover: checkpoint failed, unfencing primary")
		unfenceRollback(ctx, streams, ns, primaryPod.Name)
		result.ErrorMessage = fmt.Sprintf("checkpoint on primary failed: %v", err)
		return result
	}
	if !cpResult.Success {
		log.Warn().Str("error", cpResult.Error).Msg("Switchover: checkpoint failed, unfencing primary")
		unfenceRollback(ctx, streams, ns, primaryPod.Name)
		result.ErrorMessage = fmt.Sprintf("checkpoint on primary failed: %s", cpResult.Error)
		return result
	}

	// 5. Transfer the leader lease to the target pod
	leaseName := req.ClusterName + "-leader"
	if err := transferLease(ctx, client, ns, leaseName, target); err != nil {
		// Rollback: unfence primary
		log.Warn().Err(err).Msg("Switchover: lease transfer failed, unfencing primary")
		unfenceRollback(ctx, streams, ns, primaryPod.Name)
		result.ErrorMessage = fmt.Sprintf("failed to transfer lease: %v", err)
		return result
	}

	// 6. Promote the target replica via sidecar stream
	log.Trace().Str("target", target).Msg("Switchover: promoting target via sidecar stream")
	promoteCtx, promoteCancel := context.WithTimeout(ctx, 20*time.Second)
	defer promoteCancel()
	promoteResult, err := streams.SendCommandAndWait(promoteCtx, ns, target, &pgswarmv1.SidecarCommand{
		Cmd: &pgswarmv1.SidecarCommand_Promote{Promote: &pgswarmv1.PromoteCmd{WaitTimeoutSeconds: 15}},
	})
	if err != nil {
		// Point of no return — lease already transferred, do NOT unfence
		log.Error().Err(err).Msg("Switchover: promote failed after lease transfer (point of no return)")
		result.ErrorMessage = fmt.Sprintf("promote failed after lease transfer: %v", err)
		return result
	}
	if !promoteResult.Success {
		log.Error().Str("error", promoteResult.Error).Msg("Switchover: promote failed after lease transfer (point of no return)")
		result.ErrorMessage = fmt.Sprintf("promote failed after lease transfer: %s", promoteResult.Error)
		return result
	}

	// 7. Label pods directly so sidecars and services pick up the new topology
	log.Trace().Msg("Switchover: labeling pods")
	labelPod(ctx, client, ns, target, "primary")
	labelPod(ctx, client, ns, primaryPod.Name, "replica")

	// 8. Renew the lease one more time
	_ = renewLease(ctx, client, ns, leaseName, target)

	log.Info().
		Str("cluster", req.ClusterName).
		Str("old_primary", primaryPod.Name).
		Str("new_primary", target).
		Msg("switchover completed successfully")

	result.Success = true
	return result
}

// unfenceRollback attempts to unfence a primary as part of switchover rollback.
func unfenceRollback(ctx context.Context, streams *sidecar.SidecarStreamManager, ns, podName string) {
	unfenceCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := streams.SendCommandAndWait(unfenceCtx, ns, podName, &pgswarmv1.SidecarCommand{
		Cmd: &pgswarmv1.SidecarCommand_Unfence{Unfence: &pgswarmv1.UnfenceCmd{}},
	})
	if err != nil {
		log.Error().Err(err).Str("pod", podName).Msg("Switchover rollback: unfence failed")
	}
}

// labelPod patches the pg-swarm.io/role label on a pod.
func labelPod(ctx context.Context, client kubernetes.Interface, namespace, podName, role string) {
	patch := map[string]any{
		"metadata": map[string]any{
			"labels": map[string]string{
				"pg-swarm.io/role": role,
			},
		},
	}
	patchBytes, _ := json.Marshal(patch)
	if _, err := client.CoreV1().Pods(namespace).Patch(ctx, podName, types.MergePatchType, patchBytes, metav1.PatchOptions{}); err != nil {
		log.Warn().Err(err).Str("pod", podName).Str("role", role).Msg("failed to label pod during switchover")
	}
}

// renewLease renews the leader lease for the given holder.
func renewLease(ctx context.Context, client kubernetes.Interface, namespace, leaseName, holder string) error {
	lease, err := client.CoordinationV1().Leases(namespace).Get(ctx, leaseName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	now := metav1.NewMicroTime(time.Now())
	lease.Spec.HolderIdentity = &holder
	lease.Spec.RenewTime = &now
	_, err = client.CoordinationV1().Leases(namespace).Update(ctx, lease, metav1.UpdateOptions{})
	return err
}

// transferLease updates the leader lease to point to the new holder.
func transferLease(ctx context.Context, client kubernetes.Interface, namespace, leaseName, newHolder string) error {
	lease, err := client.CoordinationV1().Leases(namespace).Get(ctx, leaseName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// Create a new lease for the target
		now := metav1.NewMicroTime(time.Now())
		dur := int32(5)
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
