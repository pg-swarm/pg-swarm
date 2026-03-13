package operator

import (
	"context"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/rs/zerolog/log"
)

func ensureNamespace(ctx context.Context, client kubernetes.Interface, name string) error {
	if name == "default" {
		return nil
	}
	ns := buildNamespace(name)
	_, err := client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func buildNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				LabelManagedBy: ManagedByValue,
			},
		},
	}
}

// createOrPreserveSecret creates the secret only if it doesn't already exist.
// This preserves passwords across config updates.
func createOrPreserveSecret(ctx context.Context, client kubernetes.Interface, desired *corev1.Secret) error {
	_, err := client.CoreV1().Secrets(desired.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if err == nil {
		// Secret exists — preserve it
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get secret %s: %w", desired.Name, err)
	}
	_, err = client.CoreV1().Secrets(desired.Namespace).Create(ctx, desired, metav1.CreateOptions{})
	return err
}

func createOrUpdateConfigMap(ctx context.Context, client kubernetes.Interface, desired *corev1.ConfigMap) error {
	existing, err := client.CoreV1().ConfigMaps(desired.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = client.CoreV1().ConfigMaps(desired.Namespace).Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return fmt.Errorf("get configmap %s: %w", desired.Name, err)
	}

	existing.Data = desired.Data
	existing.Labels = desired.Labels
	_, err = client.CoreV1().ConfigMaps(desired.Namespace).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

func createOrUpdateService(ctx context.Context, client kubernetes.Interface, desired *corev1.Service) error {
	existing, err := client.CoreV1().Services(desired.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = client.CoreV1().Services(desired.Namespace).Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return fmt.Errorf("get service %s: %w", desired.Name, err)
	}

	// Preserve ClusterIP on update (immutable field)
	desired.Spec.ClusterIP = existing.Spec.ClusterIP
	desired.ObjectMeta.ResourceVersion = existing.ObjectMeta.ResourceVersion
	_, err = client.CoreV1().Services(desired.Namespace).Update(ctx, desired, metav1.UpdateOptions{})
	return err
}

func createOrUpdateStatefulSet(ctx context.Context, client kubernetes.Interface, desired *appsv1.StatefulSet) error {
	existing, err := client.AppsV1().StatefulSets(desired.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = client.AppsV1().StatefulSets(desired.Namespace).Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return fmt.Errorf("get statefulset %s: %w", desired.Name, err)
	}

	// VolumeClaimTemplates are immutable after creation — warn if storage changed
	for i, desiredVCT := range desired.Spec.VolumeClaimTemplates {
		if i >= len(existing.Spec.VolumeClaimTemplates) {
			log.Warn().
				Str("statefulset", desired.Name).
				Str("vct", desiredVCT.Name).
				Msg("new VolumeClaimTemplate detected — VCTs are immutable after creation, new VCT ignored")
			continue
		}
		existingSize := existing.Spec.VolumeClaimTemplates[i].Spec.Resources.Requests[corev1.ResourceStorage]
		desiredSize := desiredVCT.Spec.Resources.Requests[corev1.ResourceStorage]
		if existingSize.Cmp(desiredSize) != 0 {
			log.Warn().
				Str("statefulset", desired.Name).
				Str("vct", desiredVCT.Name).
				Str("existing_size", existingSize.String()).
				Str("desired_size", desiredSize.String()).
				Msg("VolumeClaimTemplate storage size change detected — VCTs are immutable after creation, change ignored")
		}
	}

	// Update replicas and pod template (safe to update)
	existing.Spec.Replicas = desired.Spec.Replicas
	existing.Spec.Template = desired.Spec.Template
	_, err = client.AppsV1().StatefulSets(desired.Namespace).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

// labelPods labels pods based on ordinal: 0=primary, rest=replica.
// Pods that don't exist yet are silently skipped.
func labelPods(ctx context.Context, client kubernetes.Interface, namespace, clusterName string) error {
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", LabelCluster, clusterName),
	})
	if err != nil {
		return fmt.Errorf("list pods for cluster %s: %w", clusterName, err)
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		role := RoleReplica
		// Ordinal 0 is always the initial primary
		if pod.Name == fmt.Sprintf("%s-0", clusterName) {
			role = RolePrimary
		}

		if pod.Labels[LabelRole] == role {
			continue // already labeled correctly
		}

		patch := map[string]interface{}{
			"metadata": map[string]interface{}{
				"labels": map[string]string{
					LabelRole: role,
				},
			},
		}
		patchBytes, err := json.Marshal(patch)
		if err != nil {
			return fmt.Errorf("marshal patch: %w", err)
		}
		_, err = client.CoreV1().Pods(namespace).Patch(ctx, pod.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
		if err != nil {
			log.Warn().Err(err).Str("pod", pod.Name).Msg("failed to label pod (may not be running yet)")
		}
	}

	return nil
}

func createOrUpdateServiceAccount(ctx context.Context, client kubernetes.Interface, desired *corev1.ServiceAccount) error {
	_, err := client.CoreV1().ServiceAccounts(desired.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = client.CoreV1().ServiceAccounts(desired.Namespace).Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	return err // already exists — no update needed for ServiceAccount
}

func createOrUpdateRole(ctx context.Context, client kubernetes.Interface, desired *rbacv1.Role) error {
	existing, err := client.RbacV1().Roles(desired.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = client.RbacV1().Roles(desired.Namespace).Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return fmt.Errorf("get role %s: %w", desired.Name, err)
	}
	existing.Rules = desired.Rules
	existing.Labels = desired.Labels
	_, err = client.RbacV1().Roles(desired.Namespace).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

func createOrUpdateRoleBinding(ctx context.Context, client kubernetes.Interface, desired *rbacv1.RoleBinding) error {
	_, err := client.RbacV1().RoleBindings(desired.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = client.RbacV1().RoleBindings(desired.Namespace).Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	return err // RoleBinding roleRef is immutable — no update needed
}
