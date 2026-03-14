package failover

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/pg-swarm/pg-swarm/internal/shared/pgfence"
)

func TestHandlePrimary_SplitBrain_FencesDemotesAndLabelsReplica(t *testing.T) {
	otherPod := "pg-cluster-1"
	myPod := "pg-cluster-0"
	ns := "default"

	now := metav1.NewMicroTime(time.Now())
	dur := int32(15)
	client := fake.NewSimpleClientset(
		&coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: "mycluster-leader", Namespace: ns},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       &otherPod,
				LeaseDurationSeconds: &dur,
				AcquireTime:          &now,
				RenewTime:            &now,
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: myPod, Namespace: ns},
		},
	)

	var fenceCalled atomic.Bool
	var demoteCalled atomic.Bool
	mon := &Monitor{
		cfg: Config{
			PodName:     myPod,
			Namespace:   ns,
			ClusterName: "mycluster",
		},
		client:    client,
		leaseName: "mycluster-leader",
		fenceFunc: func(_ context.Context, _ pgfence.PGExecer) error {
			fenceCalled.Store(true)
			return nil
		},
		demoteFunc: func(_ context.Context) error {
			demoteCalled.Store(true)
			return nil
		},
	}

	mon.handlePrimary(context.Background(), nil)

	if !fenceCalled.Load() {
		t.Fatal("expected fenceFunc to be called on split-brain")
	}
	if !demoteCalled.Load() {
		t.Fatal("expected demoteFunc to be called on split-brain")
	}

	pod, err := client.CoreV1().Pods(ns).Get(context.Background(), myPod, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if pod.Labels[labelRole] != roleReplica {
		t.Fatalf("expected role=%s, got %s", roleReplica, pod.Labels[labelRole])
	}
}

func TestHandlePrimary_LeaseAcquired_LabelsAsPrimary(t *testing.T) {
	myPod := "pg-cluster-0"
	ns := "default"

	now := metav1.NewMicroTime(time.Now())
	dur := int32(15)
	client := fake.NewSimpleClientset(
		&coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: "mycluster-leader", Namespace: ns},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       &myPod,
				LeaseDurationSeconds: &dur,
				AcquireTime:          &now,
				RenewTime:            &now,
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: myPod, Namespace: ns},
		},
	)

	mon := &Monitor{
		cfg: Config{
			PodName:     myPod,
			Namespace:   ns,
			ClusterName: "mycluster",
		},
		client:    client,
		leaseName: "mycluster-leader",
	}

	// IsFenced calls QueryRow on nil conn which returns false (the recover
	// inside IsFenced handles this). No unfence needed.
	mon.handlePrimary(context.Background(), nil)

	pod, err := client.CoreV1().Pods(ns).Get(context.Background(), myPod, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if pod.Labels[labelRole] != rolePrimary {
		t.Fatalf("expected role=%s, got %s", rolePrimary, pod.Labels[labelRole])
	}
}

func TestHandlePrimary_LeaseError_FencesButDoesNotDemote(t *testing.T) {
	myPod := "pg-cluster-0"
	ns := "default"

	client := fake.NewSimpleClientset(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: myPod, Namespace: ns},
		},
	)

	// Make lease Get return an error (simulates K8s API unreachable)
	client.PrependReactor("get", "leases", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, fmt.Errorf("connection refused")
	})

	var fenceCalled atomic.Bool
	var demoteCalled atomic.Bool
	mon := &Monitor{
		cfg: Config{
			PodName:     myPod,
			Namespace:   ns,
			ClusterName: "mycluster",
		},
		client:    client,
		leaseName: "mycluster-leader",
		fenceFunc: func(_ context.Context, _ pgfence.PGExecer) error {
			fenceCalled.Store(true)
			return nil
		},
		demoteFunc: func(_ context.Context) error {
			demoteCalled.Store(true)
			return nil
		},
	}

	mon.handlePrimary(context.Background(), nil)

	if !fenceCalled.Load() {
		t.Fatal("expected fenceFunc to be called when lease operation fails")
	}
	if demoteCalled.Load() {
		t.Fatal("demoteFunc should NOT be called on lease errors — can't determine new primary")
	}
}
