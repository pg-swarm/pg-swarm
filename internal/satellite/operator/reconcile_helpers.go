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

// ensureNamespace creates the namespace if it does not already exist.
func ensureNamespace(ctx context.Context, client kubernetes.Interface, name string) error {
	log.Trace().Str("namespace", name).Msg("ensureNamespace entry")
	if name == "default" {
		return nil
	}
	ns := buildNamespace(name)
	_, err := client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		log.Trace().Str("namespace", name).Msg("ensureNamespace already exists")
		return nil
	}
	return err
}

// buildNamespace constructs a Namespace object with pg-swarm managed-by labels.
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
// This preserves passwords across config updates. If the secret exists but is
// missing keys that the desired secret has, the missing keys are backfilled
// without touching existing ones.
func createOrPreserveSecret(ctx context.Context, client kubernetes.Interface, desired *corev1.Secret) error {
	log.Trace().Str("secret", desired.Name).Msg("createOrPreserveSecret entry")
	existing, err := client.CoreV1().Secrets(desired.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = client.CoreV1().Secrets(desired.Namespace).Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return fmt.Errorf("get secret %s: %w", desired.Name, err)
	}

	// Backfill any keys present in desired.StringData but missing from the
	// existing secret. This handles schema upgrades without overwriting
	// existing passwords.
	needsUpdate := false
	for key, val := range desired.StringData {
		if _, exists := existing.Data[key]; !exists {
			if existing.StringData == nil {
				existing.StringData = map[string]string{}
			}
			existing.StringData[key] = val
			needsUpdate = true
			log.Info().Str("secret", desired.Name).Str("key", key).Msg("backfilling missing secret key")
		}
	}
	if needsUpdate {
		_, err = client.CoreV1().Secrets(desired.Namespace).Update(ctx, existing, metav1.UpdateOptions{})
		return err
	}

	log.Trace().Str("secret", desired.Name).Msg("createOrPreserveSecret: already exists, preserving")
	return nil
}

// createOrUpdateConfigMap creates or updates a ConfigMap to match the desired state.
func createOrUpdateConfigMap(ctx context.Context, client kubernetes.Interface, desired *corev1.ConfigMap) error {
	log.Trace().Str("configmap", desired.Name).Msg("createOrUpdateConfigMap entry")
	existing, err := client.CoreV1().ConfigMaps(desired.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		log.Trace().Str("configmap", desired.Name).Msg("createOrUpdateConfigMap: creating")
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

// createOrUpdateService creates or updates a Service, preserving the immutable ClusterIP.
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

// createOrUpdateStatefulSet creates or updates a StatefulSet, preserving immutable VolumeClaimTemplates.
func createOrUpdateStatefulSet(ctx context.Context, client kubernetes.Interface, desired *appsv1.StatefulSet) error {
	log.Trace().Str("statefulset", desired.Name).Msg("createOrUpdateStatefulSet entry")
	existing, err := client.AppsV1().StatefulSets(desired.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		log.Trace().Str("statefulset", desired.Name).Msg("createOrUpdateStatefulSet: creating")
		_, err = client.AppsV1().StatefulSets(desired.Namespace).Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return fmt.Errorf("get statefulset %s: %w", desired.Name, err)
	}

	// VolumeClaimTemplates are immutable after creation — warn if storage changed
	log.Trace().Str("statefulset", desired.Name).Msg("createOrUpdateStatefulSet: comparing VCTs")
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

// labelPods assigns initial role labels to pods that don't have one yet.
// Pods that already carry a role label are left alone — the failover sidecar
// is the authority on role after initial deployment (it detects pg_is_in_recovery).
// On first creation: ordinal 0 = primary, rest = replica.
func labelPods(ctx context.Context, client kubernetes.Interface, namespace, clusterName string) error {
	log.Trace().Str("cluster", clusterName).Msg("labelPods entry")
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", LabelCluster, clusterName),
	})
	if err != nil {
		return fmt.Errorf("list pods for cluster %s: %w", clusterName, err)
	}

	for i := range pods.Items {
		pod := &pods.Items[i]

		// Skip pods that already have a role label — the failover sidecar manages
		// role labels based on actual PostgreSQL state (pg_is_in_recovery).
		if _, hasRole := pod.Labels[LabelRole]; hasRole {
			continue
		}

		role := RoleReplica
		// Ordinal 0 is the initial primary
		if pod.Name == fmt.Sprintf("%s-0", clusterName) {
			role = RolePrimary
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

// createOrUpdateServiceAccount creates a ServiceAccount if it does not already exist.
func createOrUpdateServiceAccount(ctx context.Context, client kubernetes.Interface, desired *corev1.ServiceAccount) error {
	_, err := client.CoreV1().ServiceAccounts(desired.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = client.CoreV1().ServiceAccounts(desired.Namespace).Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	return err // already exists — no update needed for ServiceAccount
}

// createOrUpdateRole creates or updates an RBAC Role to match the desired rules.
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

// createOrUpdateRoleBinding creates a RoleBinding if it does not already exist.
func createOrUpdateRoleBinding(ctx context.Context, client kubernetes.Interface, desired *rbacv1.RoleBinding) error {
	_, err := client.RbacV1().RoleBindings(desired.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = client.RbacV1().RoleBindings(desired.Namespace).Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	return err // RoleBinding roleRef is immutable — no update needed
}

// createOrUpdatePVC creates a PVC if it doesn't exist, or updates labels only.
// PVC spec (size, storage class) is immutable on bound volumes, so changes are warned but not applied.
func createOrUpdatePVC(ctx context.Context, client kubernetes.Interface, desired *corev1.PersistentVolumeClaim) error {
	existing, err := client.CoreV1().PersistentVolumeClaims(desired.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = client.CoreV1().PersistentVolumeClaims(desired.Namespace).Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return fmt.Errorf("get PVC %s: %w", desired.Name, err)
	}

	// Warn if size differs — PVC size is immutable on bound volumes
	existingSize := existing.Spec.Resources.Requests[corev1.ResourceStorage]
	desiredSize := desired.Spec.Resources.Requests[corev1.ResourceStorage]
	if existingSize.Cmp(desiredSize) != 0 {
		log.Warn().
			Str("pvc", desired.Name).
			Str("existing_size", existingSize.String()).
			Str("desired_size", desiredSize.String()).
			Msg("PVC storage size change detected — PVC size is immutable on bound volumes, change ignored")
	}

	// Update labels only
	existing.Labels = desired.Labels
	_, err = client.CoreV1().PersistentVolumeClaims(desired.Namespace).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

// reconcilePVCFinalizers ensures PVC finalizers match the current DeletionProtection
// setting. VolumeClaimTemplates are immutable after StatefulSet creation, so we
// must patch the actual PVCs to add or remove the finalizer.
func reconcilePVCFinalizers(ctx context.Context, client kubernetes.Interface, namespace, clusterName string, protect bool) error {
	pvcs, err := client.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", LabelCluster, clusterName),
	})
	if err != nil {
		return fmt.Errorf("list PVCs: %w", err)
	}

	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		has := false
		for _, f := range pvc.Finalizers {
			if f == FinalizerPGSwarm {
				has = true
				break
			}
		}

		if protect && !has {
			// Add finalizer
			pvc.Finalizers = append(pvc.Finalizers, FinalizerPGSwarm)
			if _, err := client.CoreV1().PersistentVolumeClaims(namespace).Update(ctx, pvc, metav1.UpdateOptions{}); err != nil {
				log.Warn().Err(err).Str("pvc", pvc.Name).Msg("failed to add finalizer to PVC")
			}
		} else if !protect && has {
			// Remove finalizer
			filtered := make([]string, 0, len(pvc.Finalizers))
			for _, f := range pvc.Finalizers {
				if f != FinalizerPGSwarm {
					filtered = append(filtered, f)
				}
			}
			pvc.Finalizers = filtered
			if _, err := client.CoreV1().PersistentVolumeClaims(namespace).Update(ctx, pvc, metav1.UpdateOptions{}); err != nil {
				log.Warn().Err(err).Str("pvc", pvc.Name).Msg("failed to remove finalizer from PVC")
			}
		}
	}
	return nil
}

// removeFinalizedPVCs strips the pg-swarm finalizer from PVCs belonging to the
// cluster's StatefulSet and deletes them. PVCs follow the naming convention
// <vct-name>-<sts-name>-<ordinal>.
func removeFinalizedPVCs(ctx context.Context, client kubernetes.Interface, namespace, clusterName string) {
	pvcs, err := client.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", LabelCluster, clusterName),
	})
	if err != nil {
		log.Warn().Err(err).Str("cluster", clusterName).Msg("failed to list PVCs for cleanup")
		return
	}

	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		// Remove our finalizer
		filtered := make([]string, 0, len(pvc.Finalizers))
		for _, f := range pvc.Finalizers {
			if f != FinalizerPGSwarm {
				filtered = append(filtered, f)
			}
		}
		if len(filtered) != len(pvc.Finalizers) {
			pvc.Finalizers = filtered
			if _, err := client.CoreV1().PersistentVolumeClaims(namespace).Update(ctx, pvc, metav1.UpdateOptions{}); err != nil {
				log.Warn().Err(err).Str("pvc", pvc.Name).Msg("failed to remove finalizer from PVC")
				continue
			}
		}
		if err := client.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, pvc.Name, metav1.DeleteOptions{}); err != nil {
			log.Warn().Err(err).Str("pvc", pvc.Name).Msg("failed to delete PVC")
		}
	}
}
