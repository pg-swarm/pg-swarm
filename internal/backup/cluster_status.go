package backup

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// stalenessThreshold is how old updated_at can be before we allow backup anyway
// (health monitor may be down).
const stalenessThreshold = 5 * time.Minute

// ClusterStatusGate reads the cluster-status ConfigMap to decide if backups are allowed.
type ClusterStatusGate struct {
	namespace   string
	clusterName string
	client      kubernetes.Interface
}

// NewClusterStatusGate creates a new gate. Fails open if K8s client is unavailable.
func NewClusterStatusGate(namespace, clusterName string) *ClusterStatusGate {
	g := &ClusterStatusGate{
		namespace:   namespace,
		clusterName: clusterName,
	}
	k8sCfg, err := rest.InClusterConfig()
	if err != nil {
		log.Warn().Err(err).Msg("K8s client unavailable for cluster status gate (fail-open)")
		return g
	}
	client, err := kubernetes.NewForConfig(k8sCfg)
	if err != nil {
		log.Warn().Err(err).Msg("K8s client creation failed for cluster status gate (fail-open)")
		return g
	}
	g.client = client
	return g
}

// IsBackupAllowed returns true only if the cluster lifecycle_state is RUNNING.
// Falls back to allowing backups if:
// - K8s client is unavailable (fail-open)
// - ConfigMap doesn't exist yet
// - updated_at is stale (>5 minutes old — health monitor may be down)
func (g *ClusterStatusGate) IsBackupAllowed(ctx context.Context) (bool, string) {
	if g.client == nil {
		return true, "K8s client unavailable (fail-open)"
	}

	cmName := g.clusterName + "-cluster-status"
	cm, err := g.client.CoreV1().ConfigMaps(g.namespace).Get(ctx, cmName, metav1.GetOptions{})
	if err != nil {
		return true, fmt.Sprintf("cluster-status ConfigMap not found (fail-open): %v", err)
	}

	state := cm.Data["lifecycle_state"]

	// Staleness fallback: if updated_at is >5 minutes old, allow backup
	if updatedAt, err := time.Parse(time.RFC3339, cm.Data["updated_at"]); err == nil {
		if time.Since(updatedAt) > stalenessThreshold {
			return true, fmt.Sprintf("cluster-status stale (last updated %s, fail-open)", cm.Data["updated_at"])
		}
	}

	if state == "RUNNING" {
		return true, "cluster RUNNING"
	}

	reason := cm.Data["reason"]
	return false, fmt.Sprintf("lifecycle_state=%s: %s", state, reason)
}
