package operator

import (
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
)

func failoverEnabled(cfg *pgswarmv1.ClusterConfig) bool {
	return cfg.Failover != nil && cfg.Failover.Enabled
}

func failoverServiceAccountName(clusterName string) string {
	return resourceName(clusterName, "failover")
}

func failoverLeaseName(clusterName string) string {
	return resourceName(clusterName, "leader")
}

func buildFailoverServiceAccount(cfg *pgswarmv1.ClusterConfig) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      failoverServiceAccountName(cfg.ClusterName),
			Namespace: cfg.Namespace,
			Labels:    clusterLabels(cfg.ClusterName, cfg.ProfileName, cfg.LabelSelector),
		},
	}
}

func buildFailoverRole(cfg *pgswarmv1.ClusterConfig) *rbacv1.Role {
	return &rbacv1.Role{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      failoverServiceAccountName(cfg.ClusterName),
			Namespace: cfg.Namespace,
			Labels:    clusterLabels(cfg.ClusterName, cfg.ProfileName, cfg.LabelSelector),
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"get", "patch"},
			},
			{
				APIGroups: []string{"coordination.k8s.io"},
				Resources: []string{"leases"},
				Verbs:     []string{"get", "create", "update"},
			},
		},
	}
}

func buildFailoverRoleBinding(cfg *pgswarmv1.ClusterConfig) *rbacv1.RoleBinding {
	saName := failoverServiceAccountName(cfg.ClusterName)
	return &rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: cfg.Namespace,
			Labels:    clusterLabels(cfg.ClusterName, cfg.ProfileName, cfg.LabelSelector),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     saName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      saName,
				Namespace: cfg.Namespace,
			},
		},
	}
}
