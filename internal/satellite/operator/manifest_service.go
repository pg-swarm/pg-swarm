package operator

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
)

// buildHeadlessService creates the headless Service used by the StatefulSet for pod DNS.
func buildHeadlessService(cfg *pgswarmv1.ClusterConfig) *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName(cfg.ClusterName, "headless"),
			Namespace: cfg.Namespace,
			Labels:    clusterLabels(cfg.ClusterName, cfg.ProfileName, cfg.LabelSelector),
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector:  selectorLabels(cfg.ClusterName),
			Ports: []corev1.ServicePort{
				{
					Name:       "postgres",
					Port:       5432,
					TargetPort: intstr.FromInt32(5432),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

// buildRWService creates the read-write Service that routes to the primary pod.
func buildRWService(cfg *pgswarmv1.ClusterConfig) *corev1.Service {
	sel := selectorLabels(cfg.ClusterName)
	sel[LabelRole] = RolePrimary

	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName(cfg.ClusterName, "rw"),
			Namespace: cfg.Namespace,
			Labels:    clusterLabels(cfg.ClusterName, cfg.ProfileName, cfg.LabelSelector),
		},
		Spec: corev1.ServiceSpec{
			Selector: sel,
			Ports: []corev1.ServicePort{
				{
					Name:       "postgres",
					Port:       5432,
					TargetPort: intstr.FromInt32(5432),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

// buildROService creates the read-only Service that routes to replica pods.
func buildROService(cfg *pgswarmv1.ClusterConfig) *corev1.Service {
	sel := selectorLabels(cfg.ClusterName)
	sel[LabelRole] = RoleReplica

	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName(cfg.ClusterName, "ro"),
			Namespace: cfg.Namespace,
			Labels:    clusterLabels(cfg.ClusterName, cfg.ProfileName, cfg.LabelSelector),
		},
		Spec: corev1.ServiceSpec{
			Selector: sel,
			Ports: []corev1.ServicePort{
				{
					Name:       "postgres",
					Port:       5432,
					TargetPort: intstr.FromInt32(5432),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}
