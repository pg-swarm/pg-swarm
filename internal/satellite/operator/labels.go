package operator

import "fmt"

const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelAppName   = "app.kubernetes.io/name"
	LabelCluster   = "pg-swarm.io/cluster"
	LabelRole      = "pg-swarm.io/role"
	LabelProfile   = "pg-swarm.io/profile"

	ManagedByValue = "pg-swarm"
	AppNameValue   = "postgresql"

	RolePrimary = "primary"
	RoleReplica = "replica"
)

// clusterLabels returns the standard labels applied to all resources for a cluster.
// profileName is optional — empty string is omitted.
// labelSelector key-value pairs are flattened into pg-swarm.io/selector-<key> labels.
func clusterLabels(clusterName, profileName string, labelSelector map[string]string) map[string]string {
	labels := map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelAppName:   AppNameValue,
		LabelCluster:   clusterName,
	}
	if profileName != "" {
		labels[LabelProfile] = profileName
	}
	for k, v := range labelSelector {
		labels[fmt.Sprintf("pg-swarm.io/selector-%s", k)] = v
	}
	return labels
}

// resourceName builds a deterministic resource name from cluster name and suffix.
func resourceName(clusterName, suffix string) string {
	return fmt.Sprintf("%s-%s", clusterName, suffix)
}
