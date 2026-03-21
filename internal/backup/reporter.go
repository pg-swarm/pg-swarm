package backup

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/rs/zerolog/log"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Reporter writes backup status to a ConfigMap for the health monitor to pick up.
type Reporter struct {
	namespace   string
	clusterName string
	client      kubernetes.Interface
}

// NewReporter creates a new backup status reporter.
func NewReporter(namespace, clusterName string) *Reporter {
	r := &Reporter{
		namespace:   namespace,
		clusterName: clusterName,
	}
	// Try to build K8s client
	k8sCfg, err := rest.InClusterConfig()
	if err != nil {
		log.Warn().Err(err).Msg("K8s client unavailable for status reporting")
		return r
	}
	client, err := kubernetes.NewForConfig(k8sCfg)
	if err != nil {
		log.Warn().Err(err).Msg("K8s client creation failed for status reporting")
		return r
	}
	r.client = client
	return r
}

// ReportBackup writes backup status to the cluster's backup-status ConfigMap.
func (r *Reporter) ReportBackup(ctx context.Context, backupType, status string, sizeBytes int64, errMsg string) {
	r.ReportBackupWithHealth(ctx, backupType, status, sizeBytes, errMsg, nil)
}

// ReportBackupWithHealth writes backup status with health context to the ConfigMap.
func (r *Reporter) ReportBackupWithHealth(ctx context.Context, backupType, status string, sizeBytes int64, errMsg string, hs *HealthStatus) {
	if r.client == nil {
		return
	}

	cmName := fmt.Sprintf("%s-backup-status", r.clusterName)
	now := time.Now().UTC().Format(time.RFC3339)
	podName := os.Getenv("POD_NAME")

	data := map[string]string{
		"backup_type":   backupType,
		"status":        status,
		"completed_at":  now,
		"size_bytes":    fmt.Sprintf("%d", sizeBytes),
		"error_message": errMsg,
		"pod_name":      podName,
	}

	if hs != nil {
		data["replication_lag_bytes"] = fmt.Sprintf("%d", hs.ReplicationLagBytes)
		data["wal_receiver_status"] = hs.WalReceiverStatus
		data["health_check_passed"] = fmt.Sprintf("%t", hs.Healthy)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: r.namespace,
		},
		Data: data,
	}

	existing, err := r.client.CoreV1().ConfigMaps(r.namespace).Get(ctx, cmName, metav1.GetOptions{})
	if err != nil {
		// Create new
		if _, err := r.client.CoreV1().ConfigMaps(r.namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			log.Warn().Err(err).Str("configmap", cmName).Msg("failed to create backup status ConfigMap")
		}
		return
	}

	// Update existing
	existing.Data = data
	if _, err := r.client.CoreV1().ConfigMaps(r.namespace).Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		log.Warn().Err(err).Str("configmap", cmName).Msg("failed to update backup status ConfigMap")
	}
}
