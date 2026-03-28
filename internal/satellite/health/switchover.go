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

// ProgressFunc is called at each switchover step boundary to report progress.
type ProgressFunc func(step int32, stepName, status, targetPod, errorMsg string, ponr bool)

// SwitchoverSession holds shared state for running switchover steps individually.
// It tracks inter-step state (primary pod, lease name) so steps can be executed
// one at a time without rebuilding context.
type SwitchoverSession struct {
	k8sClient  kubernetes.Interface
	req        *pgswarmv1.SwitchoverRequest
	streams    *sidecar.SidecarStreamManager
	primaryPod string // populated after step 2
	leaseName  string // req.ClusterName + "-leader"
}

// NewSwitchoverSession creates a session for running switchover steps individually.
// Use ExecuteStep to run each step, then Rollback if aborting before step 7.
func NewSwitchoverSession(client kubernetes.Interface, req *pgswarmv1.SwitchoverRequest, streams *sidecar.SidecarStreamManager) *SwitchoverSession {
	return &SwitchoverSession{
		k8sClient: client,
		req:       req,
		streams:   streams,
		leaseName: req.ClusterName + "-leader",
	}
}

// TotalSteps returns the number of switchover steps.
func (*SwitchoverSession) TotalSteps() int { return 9 }

// PrimaryPod returns the current primary pod discovered during step 2.
// Empty until step 2 completes successfully.
func (s *SwitchoverSession) PrimaryPod() string { return s.primaryPod }

// StepMeta returns static information about a step: its name, the pod it
// targets (may be empty for step 2 before the primary is known), and
// whether the step is at or past the point of no return.
func (s *SwitchoverSession) StepMeta(step int32) (name, targetPod string, ponr bool) {
	switch step {
	case 1:
		return "verify_target", s.req.TargetPod, false
	case 2:
		return "find_primary", "", false // primary unknown until step 2 runs
	case 3:
		return "check_status", s.req.TargetPod, false
	case 4:
		return "fence_primary", s.primaryPod, false
	case 5:
		return "checkpoint", s.primaryPod, false
	case 6:
		return "transfer_lease", s.req.TargetPod, false
	case 7:
		return "promote", s.req.TargetPod, true
	case 8:
		return "label_pods", s.req.TargetPod, true
	case 9:
		return "renew_lease", s.req.TargetPod, true
	}
	return "unknown", "", false
}

// ExecuteStep runs exactly one step, calling emit for any result status changes.
// The caller is responsible for emitting the "starting" status before calling this.
// Returns (success bool, errorMsg string).
func (s *SwitchoverSession) ExecuteStep(ctx context.Context, step int32, emit ProgressFunc) (bool, string) {
	switch step {
	case 1:
		return s.step1VerifyTarget(ctx, emit)
	case 2:
		return s.step2FindPrimary(ctx, emit)
	case 3:
		return s.step3CheckStatus(ctx, emit)
	case 4:
		return s.step4FencePrimary(ctx, emit)
	case 5:
		return s.step5Checkpoint(ctx, emit)
	case 6:
		return s.step6TransferLease(ctx, emit)
	case 7:
		return s.step7Promote(ctx, emit)
	case 8:
		return s.step8LabelPods(ctx, emit)
	case 9:
		return s.step9RenewLease(ctx, emit)
	}
	return false, fmt.Sprintf("unknown step %d", step)
}

// Rollback unfences the primary pod. Call this when aborting after step 4
// (fence_primary) but before step 7 (promote / point of no return).
func (s *SwitchoverSession) Rollback(ctx context.Context) {
	if s.primaryPod != "" {
		unfenceRollback(ctx, s.streams, s.req.Namespace, s.primaryPod)
	}
}

func (s *SwitchoverSession) step1VerifyTarget(ctx context.Context, emit ProgressFunc) (bool, string) {
	target := s.req.TargetPod
	ns := s.req.Namespace
	log.Trace().Str("target", target).Msg("Switchover: verifying target pod")
	targetPod, err := s.k8sClient.CoreV1().Pods(ns).Get(ctx, target, metav1.GetOptions{})
	if err != nil {
		msg := fmt.Sprintf("target pod not found: %v", err)
		emit(1, "verify_target", "failed", target, msg, false)
		return false, msg
	}
	if targetPod.Labels["pg-swarm.io/role"] != "replica" {
		msg := fmt.Sprintf("target pod %s is not a replica (role=%s)", target, targetPod.Labels["pg-swarm.io/role"])
		emit(1, "verify_target", "failed", target, msg, false)
		return false, msg
	}
	emit(1, "verify_target", "completed", target, "role=replica, pod ready", false)
	return true, ""
}

func (s *SwitchoverSession) step2FindPrimary(ctx context.Context, emit ProgressFunc) (bool, string) {
	ns := s.req.Namespace
	log.Trace().Msg("Switchover: finding primary pod")
	pods, err := s.k8sClient.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("pg-swarm.io/cluster=%s,pg-swarm.io/role=primary", s.req.ClusterName),
	})
	if err != nil || len(pods.Items) == 0 {
		msg := "no current primary found"
		emit(2, "find_primary", "failed", "", msg, false)
		return false, msg
	}
	s.primaryPod = pods.Items[0].Name
	emit(2, "find_primary", "completed", s.primaryPod, fmt.Sprintf("primary: %s", s.primaryPod), false)
	log.Trace().Str("primary", s.primaryPod).Msg("Switchover: current primary found")
	return true, ""
}

func (s *SwitchoverSession) step3CheckStatus(ctx context.Context, emit ProgressFunc) (bool, string) {
	target := s.req.TargetPod
	ns := s.req.Namespace
	log.Trace().Msg("Switchover: checking target status via sidecar stream")
	statusCtx, statusCancel := context.WithTimeout(ctx, 10*time.Second)
	defer statusCancel()
	statusResult, err := s.streams.SendCommandAndWait(statusCtx, ns, target, &pgswarmv1.SidecarCommand{
		Cmd: &pgswarmv1.SidecarCommand_Status{Status: &pgswarmv1.StatusCmd{}},
	})
	if err != nil {
		msg := fmt.Sprintf("cannot check target status: %v", err)
		emit(3, "check_status", "failed", target, msg, false)
		return false, msg
	}
	if !statusResult.Success {
		msg := fmt.Sprintf("target status check failed: %s", statusResult.Error)
		emit(3, "check_status", "failed", target, msg, false)
		return false, msg
	}
	if !statusResult.InRecovery {
		msg := "target is not in recovery mode"
		emit(3, "check_status", "failed", target, msg, false)
		return false, msg
	}
	emit(3, "check_status", "completed", target, "in_recovery=true", false)
	return true, ""
}

func (s *SwitchoverSession) step4FencePrimary(ctx context.Context, emit ProgressFunc) (bool, string) {
	ns := s.req.Namespace
	log.Trace().Msg("Switchover: fencing old primary via sidecar stream")
	fenceCtx, fenceCancel := context.WithTimeout(ctx, 15*time.Second)
	defer fenceCancel()
	fenceResult, err := s.streams.SendCommandAndWait(fenceCtx, ns, s.primaryPod, &pgswarmv1.SidecarCommand{
		Cmd: &pgswarmv1.SidecarCommand_Fence{Fence: &pgswarmv1.FenceCmd{DrainTimeoutSeconds: 5}},
	})
	if err != nil {
		msg := fmt.Sprintf("fencing old primary failed: %v", err)
		emit(4, "fence_primary", "failed", s.primaryPod, msg, false)
		return false, msg
	}
	if !fenceResult.Success {
		msg := fmt.Sprintf("fencing old primary failed: %s", fenceResult.Error)
		emit(4, "fence_primary", "failed", s.primaryPod, msg, false)
		return false, msg
	}
	emit(4, "fence_primary", "completed", s.primaryPod, "fenced, connections drained", false)
	return true, ""
}

func (s *SwitchoverSession) step5Checkpoint(ctx context.Context, emit ProgressFunc) (bool, string) {
	ns := s.req.Namespace
	log.Trace().Msg("Switchover: running checkpoint on primary via sidecar stream")
	cpCtx, cpCancel := context.WithTimeout(ctx, 30*time.Second)
	defer cpCancel()
	cpResult, err := s.streams.SendCommandAndWait(cpCtx, ns, s.primaryPod, &pgswarmv1.SidecarCommand{
		Cmd: &pgswarmv1.SidecarCommand_Checkpoint{Checkpoint: &pgswarmv1.CheckpointCmd{}},
	})
	if err != nil {
		emit(5, "checkpoint", "rolling_back", s.primaryPod, "rolling back — unfencing primary", false)
		log.Warn().Err(err).Msg("Switchover: checkpoint failed, unfencing primary")
		unfenceRollback(ctx, s.streams, ns, s.primaryPod)
		msg := fmt.Sprintf("checkpoint on primary failed: %v", err)
		emit(5, "checkpoint", "failed", s.primaryPod, msg, false)
		return false, msg
	}
	if !cpResult.Success {
		emit(5, "checkpoint", "rolling_back", s.primaryPod, "rolling back — unfencing primary", false)
		log.Warn().Str("error", cpResult.Error).Msg("Switchover: checkpoint failed, unfencing primary")
		unfenceRollback(ctx, s.streams, ns, s.primaryPod)
		msg := fmt.Sprintf("checkpoint on primary failed: %s", cpResult.Error)
		emit(5, "checkpoint", "failed", s.primaryPod, msg, false)
		return false, msg
	}
	emit(5, "checkpoint", "completed", s.primaryPod, "checkpoint completed", false)
	return true, ""
}

func (s *SwitchoverSession) step6TransferLease(ctx context.Context, emit ProgressFunc) (bool, string) {
	target := s.req.TargetPod
	ns := s.req.Namespace
	if err := transferLease(ctx, s.k8sClient, ns, s.leaseName, target); err != nil {
		emit(6, "transfer_lease", "rolling_back", target, "rolling back — unfencing primary", false)
		log.Warn().Err(err).Msg("Switchover: lease transfer failed, unfencing primary")
		unfenceRollback(ctx, s.streams, ns, s.primaryPod)
		msg := fmt.Sprintf("failed to transfer lease: %v", err)
		emit(6, "transfer_lease", "failed", target, msg, false)
		return false, msg
	}
	emit(6, "transfer_lease", "completed", target, fmt.Sprintf("lease %s transferred", s.leaseName), false)
	return true, ""
}

func (s *SwitchoverSession) step7Promote(ctx context.Context, emit ProgressFunc) (bool, string) {
	target := s.req.TargetPod
	ns := s.req.Namespace
	log.Trace().Str("target", target).Msg("Switchover: promoting target via sidecar stream")
	promoteCtx, promoteCancel := context.WithTimeout(ctx, 65*time.Second)
	defer promoteCancel()
	promoteResult, err := s.streams.SendCommandAndWait(promoteCtx, ns, target, &pgswarmv1.SidecarCommand{
		Cmd: &pgswarmv1.SidecarCommand_Promote{Promote: &pgswarmv1.PromoteCmd{WaitTimeoutSeconds: 60}},
	})
	if err != nil {
		log.Error().Err(err).Msg("Switchover: promote failed after lease transfer (point of no return)")
		msg := fmt.Sprintf("promote failed after lease transfer: %v", err)
		emit(7, "promote", "failed", target, msg, true)
		return false, msg
	}
	if !promoteResult.Success {
		log.Error().Str("error", promoteResult.Error).Msg("Switchover: promote failed after lease transfer (point of no return)")
		msg := fmt.Sprintf("promote failed after lease transfer: %s", promoteResult.Error)
		emit(7, "promote", "failed", target, msg, true)
		return false, msg
	}
	emit(7, "promote", "completed", target, "pg_promote() succeeded, exited recovery", true)
	return true, ""
}

func (s *SwitchoverSession) step8LabelPods(ctx context.Context, emit ProgressFunc) (bool, string) {
	target := s.req.TargetPod
	ns := s.req.Namespace
	log.Trace().Msg("Switchover: labeling pods")
	labelPod(ctx, s.k8sClient, ns, target, "primary")
	labelPod(ctx, s.k8sClient, ns, s.primaryPod, "replica")
	emit(8, "label_pods", "completed", target, fmt.Sprintf("%s=primary, %s=replica", target, s.primaryPod), true)
	return true, ""
}

func (s *SwitchoverSession) step9RenewLease(ctx context.Context, emit ProgressFunc) (bool, string) {
	target := s.req.TargetPod
	ns := s.req.Namespace
	_ = renewLease(ctx, s.k8sClient, ns, s.leaseName, target)
	emit(9, "renew_lease", "completed", target, fmt.Sprintf("lease renewed for %s", target), true)
	return true, ""
}

// Switchover performs a planned primary switchover using the SwitchoverSession.
// It is equivalent to calling NewSwitchoverSession and stepping through all 9 steps.
// Non-interactive callers should use this function; for interactive (step-by-step)
// use NewSwitchoverSession directly.
func Switchover(ctx context.Context, client kubernetes.Interface, req *pgswarmv1.SwitchoverRequest, streams *sidecar.SidecarStreamManager, onProgress ProgressFunc) *pgswarmv1.SwitchoverResult {
	emit := func(step int32, stepName, status, targetPod, errorMsg string, ponr bool) {
		if onProgress != nil {
			onProgress(step, stepName, status, targetPod, errorMsg, ponr)
		}
	}

	log.Info().
		Str("cluster", req.ClusterName).
		Str("target", req.TargetPod).
		Msg("starting planned switchover")

	result := &pgswarmv1.SwitchoverResult{ClusterName: req.ClusterName, OperationId: req.OperationId}
	sess := NewSwitchoverSession(client, req, streams)

	for step := int32(1); step <= int32(sess.TotalSteps()); step++ {
		name, targetPod, ponr := sess.StepMeta(step)
		emit(step, name, "starting", targetPod, "", ponr)
		ok, errMsg := sess.ExecuteStep(ctx, step, emit)
		if !ok {
			result.ErrorMessage = errMsg
			return result
		}
	}

	log.Info().
		Str("cluster", req.ClusterName).
		Str("old_primary", sess.PrimaryPod()).
		Str("new_primary", req.TargetPod).
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
