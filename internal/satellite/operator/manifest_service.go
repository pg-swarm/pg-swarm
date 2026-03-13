package operator

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
)

func buildHeadlessService(cfg *pgswarmv1.ClusterConfig) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName(cfg.ClusterName, "headless"),
			Namespace: cfg.Namespace,
			Labels:    clusterLabels(cfg.ClusterName),
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector:  clusterLabels(cfg.ClusterName),
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

func buildRWService(cfg *pgswarmv1.ClusterConfig) *corev1.Service {
	sel := clusterLabels(cfg.ClusterName)
	sel[LabelRole] = RolePrimary

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName(cfg.ClusterName, "rw"),
			Namespace: cfg.Namespace,
			Labels:    clusterLabels(cfg.ClusterName),
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

func buildROService(cfg *pgswarmv1.ClusterConfig) *corev1.Service {
	sel := clusterLabels(cfg.ClusterName)
	sel[LabelRole] = RoleReplica

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName(cfg.ClusterName, "ro"),
			Namespace: cfg.Namespace,
			Labels:    clusterLabels(cfg.ClusterName),
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
