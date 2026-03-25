package operator

import (
	"context"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// tombstoneName returns the ConfigMap name for a deletion tombstone.
func tombstoneName(clusterName string) string {
	return "pg-swarm-tombstone-" + clusterName
}

// createTombstone creates a ConfigMap tombstone marker indicating the cluster
// was intentionally deleted. This prevents the orphan detector from flagging
// resources left behind during asynchronous cleanup.
func createTombstone(ctx context.Context, client kubernetes.Interface, namespace, clusterName string, configVersion int64) error {
	cm := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      tombstoneName(clusterName),
			Namespace: namespace,
			Labels: map[string]string{
				LabelManagedBy: ManagedByValue,
				LabelTombstone: "true",
				LabelCluster:   clusterName,
			},
		},
		Data: map[string]string{
			"deleted-at":     time.Now().UTC().Format(time.RFC3339),
			"config-version": strconv.FormatInt(configVersion, 10),
		},
	}

	_, err := client.CoreV1().ConfigMaps(namespace).Create(ctx, cm, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("create tombstone for %s/%s: %w", namespace, clusterName, err)
	}
	return nil
}

// deleteTombstone removes a tombstone ConfigMap if it exists. This is called
// when a cluster is re-configured, indicating it is no longer deleted.
func deleteTombstone(ctx context.Context, client kubernetes.Interface, namespace, clusterName string) error {
	err := client.CoreV1().ConfigMaps(namespace).Delete(ctx, tombstoneName(clusterName), metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// hasTombstone checks whether a tombstone ConfigMap exists for the given cluster.
func hasTombstone(ctx context.Context, client kubernetes.Interface, namespace, clusterName string) (bool, error) {
	_, err := client.CoreV1().ConfigMaps(namespace).Get(ctx, tombstoneName(clusterName), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
