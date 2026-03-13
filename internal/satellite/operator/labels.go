package operator

import "fmt"

const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelAppName   = "app.kubernetes.io/name"
	LabelCluster   = "pg-swarm.io/cluster"
	LabelRole      = "pg-swarm.io/role"

	ManagedByValue = "pg-swarm"
	AppNameValue   = "postgresql"

	RolePrimary = "primary"
	RoleReplica = "replica"
)

// clusterLabels returns the standard labels applied to all resources for a cluster.
func clusterLabels(clusterName string) map[string]string {
	return map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelAppName:   AppNameValue,
		LabelCluster:   clusterName,
	}
}

// resourceName builds a deterministic resource name from cluster name and suffix.
func resourceName(clusterName, suffix string) string {
	return fmt.Sprintf("%s-%s", clusterName, suffix)
}
