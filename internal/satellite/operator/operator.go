package operator

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"k8s.io/client-go/kubernetes"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
)

// Operator materializes ClusterConfig messages as running PostgreSQL HA clusters.
type Operator struct {
	client               kubernetes.Interface
	k8sClusterName       string // used in config-storage ConfigMap naming
	defaultNamespace     string // fallback when ClusterConfig.Namespace is empty
	defaultFailoverImage string // fallback failover sidecar image
	mu                   sync.RWMutex
	desired              map[string]*pgswarmv1.ClusterConfig // key: "ns/cluster"
	applied              map[string]int64                    // key -> last applied config version
}

// New creates a new Operator backed by the given Kubernetes client.
// k8sClusterName is used for config-storage ConfigMap naming (pg-swarm-<k8s>-<pg>).
// defaultNamespace is used when a ClusterConfig has no namespace set.
func New(client kubernetes.Interface, k8sClusterName, defaultNamespace, defaultFailoverImage string) *Operator {
	if defaultNamespace == "" {
		defaultNamespace = "default"
	}
	if defaultFailoverImage == "" {
		defaultFailoverImage = "ghcr.io/pg-swarm/pg-swarm-failover:latest"
	}
	return &Operator{
		client:               client,
		k8sClusterName:       k8sClusterName,
		defaultNamespace:     defaultNamespace,
		defaultFailoverImage: defaultFailoverImage,
		desired:              make(map[string]*pgswarmv1.ClusterConfig),
		applied:              make(map[string]int64),
	}
}

// configStoreName returns the ConfigMap name for storing the received config:
// pg-swarm-<k8sClusterName>-<pgClusterName>
func (o *Operator) configStoreName(pgClusterName string) string {
	return fmt.Sprintf("pg-swarm-%s-%s", o.k8sClusterName, pgClusterName)
}

// clusterKey returns the map key for a cluster: "namespace/name".
func clusterKey(namespace, name string) string {
	return namespace + "/" + name
}

// resolveNamespace returns the config's namespace, falling back to the
// operator's default. It also sets the namespace on the config so downstream
// builders see a concrete value.
func (o *Operator) resolveNamespace(cfg *pgswarmv1.ClusterConfig) {
	if cfg.Namespace == "" {
		cfg.Namespace = o.defaultNamespace
	}
}

// HandleConfig is called when a ClusterConfig is received from central.
// It is idempotent: duplicate versions are skipped.
func (o *Operator) HandleConfig(cfg *pgswarmv1.ClusterConfig) error {
	log.Trace().Str("cluster", cfg.ClusterName).Int64("version", cfg.ConfigVersion).Msg("HandleConfig entry")
	o.resolveNamespace(cfg)
	key := clusterKey(cfg.Namespace, cfg.ClusterName)

	// Remove any tombstone from a previous deletion — cluster is being re-configured
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := deleteTombstone(ctx, o.client, cfg.Namespace, cfg.ClusterName); err != nil {
		log.Warn().Err(err).Str("cluster", key).Msg("failed to delete tombstone on re-config")
	}

	o.mu.RLock()
	appliedVersion := o.applied[key]
	o.mu.RUnlock()

	log.Trace().Str("cluster", key).Int64("applied", appliedVersion).Int64("incoming", cfg.ConfigVersion).Msg("HandleConfig version check")
	if appliedVersion >= cfg.ConfigVersion {
		log.Info().
			Str("cluster", key).
			Int64("version", cfg.ConfigVersion).
			Int64("applied_version", appliedVersion).
			Msg("config version already applied, skipping")
		return nil
	}

	log.Info().
		Str("cluster", key).
		Int64("version", cfg.ConfigVersion).
		Msg("reconciling cluster config")

	log.Trace().Str("cluster", key).Msg("HandleConfig starting reconcile")
	if err := o.reconcile(cfg); err != nil {
		return fmt.Errorf("reconcile %s: %w", key, err)
	}
	log.Trace().Str("cluster", key).Msg("HandleConfig reconcile completed")

	o.mu.Lock()
	o.desired[key] = cfg
	o.applied[key] = cfg.ConfigVersion
	o.mu.Unlock()

	log.Info().
		Str("cluster", key).
		Int64("version", cfg.ConfigVersion).
		Msg("cluster config applied successfully")

	return nil
}

// HandleDelete removes all K8s resources for the given cluster.
func (o *Operator) HandleDelete(del *pgswarmv1.DeleteCluster) error {
	log.Trace().Str("cluster", del.ClusterName).Msg("HandleDelete entry")
	if del.Namespace == "" {
		del.Namespace = o.defaultNamespace
	}
	key := clusterKey(del.Namespace, del.ClusterName)
	log.Info().Str("cluster", key).Msg("deleting cluster resources")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ns := del.Namespace
	name := del.ClusterName
	propagation := metav1.DeletePropagationForeground

	// Delete StatefulSet (cascades to pods)
	log.Trace().Str("resource", "statefulset/"+name).Msg("HandleDelete deleting statefulset")
	err := o.client.AppsV1().StatefulSets(ns).Delete(ctx, name, metav1.DeleteOptions{
		PropagationPolicy: &propagation,
	})
	if err != nil {
		log.Warn().Err(err).Str("resource", "statefulset/"+name).Msg("delete failed")
	}

	// Delete services
	log.Trace().Msg("HandleDelete deleting services")
	for _, suffix := range []string{"headless", "rw", "ro"} {
		svcName := resourceName(name, suffix)
		if err := o.client.CoreV1().Services(ns).Delete(ctx, svcName, metav1.DeleteOptions{}); err != nil {
			log.Warn().Err(err).Str("resource", "service/"+svcName).Msg("delete failed")
		}
	}

	// Delete ConfigMap
	log.Trace().Msg("HandleDelete deleting configmap")
	cmName := resourceName(name, "config")
	if err := o.client.CoreV1().ConfigMaps(ns).Delete(ctx, cmName, metav1.DeleteOptions{}); err != nil {
		log.Warn().Err(err).Str("resource", "configmap/"+cmName).Msg("delete failed")
	}

	// Delete Secret
	log.Trace().Msg("HandleDelete deleting secret")
	secretName := resourceName(name, "secret")
	if err := o.client.CoreV1().Secrets(ns).Delete(ctx, secretName, metav1.DeleteOptions{}); err != nil {
		log.Warn().Err(err).Str("resource", "secret/"+secretName).Msg("delete failed")
	}

	// Delete failover RBAC resources
	log.Trace().Msg("HandleDelete deleting failover RBAC")
	foName := failoverServiceAccountName(name)
	if err := o.client.RbacV1().RoleBindings(ns).Delete(ctx, foName, metav1.DeleteOptions{}); err != nil {
		log.Warn().Err(err).Str("resource", "rolebinding/"+foName).Msg("delete failed")
	}
	if err := o.client.RbacV1().Roles(ns).Delete(ctx, foName, metav1.DeleteOptions{}); err != nil {
		log.Warn().Err(err).Str("resource", "role/"+foName).Msg("delete failed")
	}
	if err := o.client.CoreV1().ServiceAccounts(ns).Delete(ctx, foName, metav1.DeleteOptions{}); err != nil {
		log.Warn().Err(err).Str("resource", "serviceaccount/"+foName).Msg("delete failed")
	}
	// Delete failover leader lease
	leaseName := failoverLeaseName(name)
	if err := o.client.CoordinationV1().Leases(ns).Delete(ctx, leaseName, metav1.DeleteOptions{}); err != nil {
		log.Warn().Err(err).Str("resource", "lease/"+leaseName).Msg("delete failed")
	}

	// Delete cluster-status ConfigMap
	clusterStatusCMName := clusterStatusConfigMapName(name)
	if err := o.client.CoreV1().ConfigMaps(ns).Delete(ctx, clusterStatusCMName, metav1.DeleteOptions{}); err != nil {
		log.Warn().Err(err).Str("resource", "configmap/"+clusterStatusCMName).Msg("delete failed")
	}

	// Delete config-store ConfigMap
	cfgStoreName := o.configStoreName(name)
	if err := o.client.CoreV1().ConfigMaps(ns).Delete(ctx, cfgStoreName, metav1.DeleteOptions{}); err != nil {
		log.Warn().Err(err).Str("resource", "configmap/"+cfgStoreName).Msg("delete failed")
	}

	// Remove finalizers from PVCs and delete them
	removeFinalizedPVCs(ctx, o.client, ns, name)

	// Create tombstone marker so orphan detection knows this was intentional
	o.mu.RLock()
	ver := o.applied[key]
	o.mu.RUnlock()
	if err := createTombstone(ctx, o.client, ns, name, ver); err != nil {
		log.Warn().Err(err).Str("cluster", key).Msg("failed to create tombstone")
	}

	o.mu.Lock()
	delete(o.desired, key)
	delete(o.applied, key)
	o.mu.Unlock()

	log.Info().Str("cluster", key).Msg("cluster resources deleted")
	return nil
}

// ManagedCluster is a snapshot of a cluster managed by this operator.
type ManagedCluster struct {
	ClusterName string
	Namespace   string
	Replicas    int32
	Paused      bool
}

// ManagedClusters returns a snapshot of all clusters the operator is managing.
func (o *Operator) ManagedClusters() []ManagedCluster {
	o.mu.RLock()
	defer o.mu.RUnlock()

	out := make([]ManagedCluster, 0, len(o.desired))
	for _, cfg := range o.desired {
		out = append(out, ManagedCluster{
			ClusterName: cfg.ClusterName,
			Namespace:   cfg.Namespace,
			Replicas:    cfg.Replicas,
			Paused:      cfg.Paused,
		})
	}
	return out
}

// ResolveNamespaceForCluster returns the namespace for a given cluster name,
// falling back to defaultNamespace if the cluster is unknown.
func (o *Operator) ResolveNamespaceForCluster(clusterName, namespace string) string {
	if namespace != "" {
		return namespace
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	for _, cfg := range o.desired {
		if cfg.ClusterName == clusterName {
			return cfg.Namespace
		}
	}
	return o.defaultNamespace
}

// ValidateSidecarToken checks if the given token matches the sidecar-stream-token
// in any managed cluster's secret. This is used by the sidecar gRPC server to
// authenticate incoming sidecar connections.
func (o *Operator) ValidateSidecarToken(token string) bool {
	if token == "" {
		return false
	}
	o.mu.RLock()
	configs := make([]*pgswarmv1.ClusterConfig, 0, len(o.desired))
	for _, cfg := range o.desired {
		configs = append(configs, cfg)
	}
	o.mu.RUnlock()

	for _, cfg := range configs {
		secretName := resourceName(cfg.ClusterName, "secret")
		secret, err := o.client.CoreV1().Secrets(cfg.Namespace).Get(
			context.Background(), secretName, metav1.GetOptions{})
		if err != nil {
			continue
		}
		if string(secret.Data["sidecar-stream-token"]) == token {
			return true
		}
	}
	return false
}

// buildConfigStore creates a ConfigMap that stores the received ClusterConfig
// as JSON for inspection. Named: pg-swarm-<k8sClusterName>-<pgClusterName>.
func (o *Operator) buildConfigStore(cfg *pgswarmv1.ClusterConfig) *corev1.ConfigMap {
	// Serialize proto to JSON (redact passwords for safety)
	cfgCopy := proto.Clone(cfg).(*pgswarmv1.ClusterConfig)
	for _, db := range cfgCopy.Databases {
		db.Password = "***"
	}
	jsonBytes, err := protojson.MarshalOptions{Indent: "  "}.Marshal(cfgCopy)
	if err != nil {
		log.Error().Err(err).Msg("failed to marshal config for storage")
		jsonBytes = []byte("{}")
	}

	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      o.configStoreName(cfg.ClusterName),
			Namespace: cfg.Namespace,
			Labels:    clusterLabels(cfg.ClusterName, cfg.ProfileName, cfg.LabelSelector),
		},
		Data: map[string]string{
			"config.json": string(jsonBytes),
		},
	}
}

// reconcile creates or updates all K8s resources for a cluster to match the desired config.
func (o *Operator) reconcile(cfg *pgswarmv1.ClusterConfig) error {
	log.Trace().Str("cluster", cfg.ClusterName).Str("namespace", cfg.Namespace).Msg("reconcile entry")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// 1. Ensure namespace
	log.Trace().Str("namespace", cfg.Namespace).Msg("reconcile: ensuring namespace")
	if err := ensureNamespace(ctx, o.client, cfg.Namespace); err != nil {
		return fmt.Errorf("ensure namespace %s: %w", cfg.Namespace, err)
	}

	// 2. Store received config as a ConfigMap for inspection
	log.Trace().Msg("reconcile: storing config-store configmap")
	cfgStore := o.buildConfigStore(cfg)
	if err := createOrUpdateConfigMap(ctx, o.client, cfgStore); err != nil {
		return fmt.Errorf("config-store configmap: %w", err)
	}

	// 3. Secret (create if absent, never update to preserve passwords)
	log.Trace().Msg("reconcile: ensuring secret")
	secret := buildSecret(cfg)
	if err := createOrPreserveSecret(ctx, o.client, secret); err != nil {
		return fmt.Errorf("secret: %w", err)
	}

	// 4. ConfigMap (postgresql.conf + pg_hba.conf)
	log.Trace().Msg("reconcile: ensuring configmap")
	cm := buildConfigMap(cfg)
	if err := createOrUpdateConfigMap(ctx, o.client, cm); err != nil {
		return fmt.Errorf("configmap: %w", err)
	}

	// 4b. Recovery rules ConfigMap (for failover sidecar)
	if failoverEnabled(cfg) {
		log.Trace().Msg("reconcile: ensuring recovery-rules configmap")
		rrCM := buildRecoveryRulesConfigMap(cfg)
		if err := createOrUpdateConfigMap(ctx, o.client, rrCM); err != nil {
			return fmt.Errorf("recovery-rules configmap: %w", err)
		}
	}

	// 4c. Cluster status ConfigMap (create-only — health monitor owns updates)
	clusterStatusCM := clusterStatusConfigMapName(cfg.ClusterName)
	if _, err := o.client.CoreV1().ConfigMaps(cfg.Namespace).Get(ctx, clusterStatusCM, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		log.Trace().Msg("reconcile: seeding cluster-status configmap")
		statusCM := buildClusterStatusConfigMap(cfg)
		if _, err := o.client.CoreV1().ConfigMaps(cfg.Namespace).Create(ctx, statusCM, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("cluster-status configmap: %w", err)
		}
	}

	// 5. Services
	log.Trace().Msg("reconcile: ensuring services")
	if err := createOrUpdateService(ctx, o.client, buildHeadlessService(cfg)); err != nil {
		return fmt.Errorf("service headless: %w", err)
	}
	if err := createOrUpdateService(ctx, o.client, buildROService(cfg)); err != nil {
		return fmt.Errorf("service ro: %w", err)
	}
	rwSvcName := resourceName(cfg.ClusterName, "rw")
	if cfg.Paused {
		// Paused: remove the RW service to make the cluster read-only
		if err := o.client.CoreV1().Services(cfg.Namespace).Delete(ctx, rwSvcName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete rw service: %w", err)
		}
		log.Info().Str("cluster", cfg.ClusterName).Msg("cluster paused — RW service removed")
	} else {
		if err := createOrUpdateService(ctx, o.client, buildRWService(cfg)); err != nil {
			return fmt.Errorf("service rw: %w", err)
		}
	}

	// 6. Failover RBAC (ServiceAccount, Role, RoleBinding)
	log.Trace().Bool("failover_enabled", failoverEnabled(cfg)).Msg("reconcile: checking failover RBAC")
	if failoverEnabled(cfg) {
		if err := createOrUpdateServiceAccount(ctx, o.client, buildFailoverServiceAccount(cfg)); err != nil {
			return fmt.Errorf("failover serviceaccount: %w", err)
		}
		if err := createOrUpdateRole(ctx, o.client, buildFailoverRole(cfg)); err != nil {
			return fmt.Errorf("failover role: %w", err)
		}
		if err := createOrUpdateRoleBinding(ctx, o.client, buildFailoverRoleBinding(cfg)); err != nil {
			return fmt.Errorf("failover rolebinding: %w", err)
		}
	}

	// 7. StatefulSet
	log.Trace().Msg("reconcile: ensuring statefulset")
	sts := buildStatefulSet(cfg, secret.Name, o.defaultFailoverImage)
	if err := createOrUpdateStatefulSet(ctx, o.client, sts); err != nil {
		return fmt.Errorf("statefulset: %w", err)
	}

	// 8. Reconcile PVC finalizers (VCTs are immutable, so we patch PVCs directly)
	log.Trace().Bool("deletion_protection", cfg.DeletionProtection).Msg("reconcile: reconciling PVC finalizers")
	if err := reconcilePVCFinalizers(ctx, o.client, cfg.Namespace, cfg.ClusterName, cfg.DeletionProtection); err != nil {
		log.Warn().Err(err).Str("cluster", cfg.ClusterName).Msg("failed to reconcile PVC finalizers")
	}

	// 9. Label pods (best-effort, pods may not exist yet)
	log.Trace().Msg("reconcile: labeling pods")
	if err := labelPods(ctx, o.client, cfg.Namespace, cfg.ClusterName); err != nil {
		log.Warn().Err(err).Str("cluster", cfg.ClusterName).Msg("failed to label pods (will retry on next reconcile)")
	}

	// When failover is disabled, no sidecar will label pods later — we must
	// wait for pods to appear and label them ourselves.
	if !failoverEnabled(cfg) {
		go o.ensurePodLabels(cfg.Namespace, cfg.ClusterName, cfg.Replicas)
	}

	return nil
}

// CheckOrphans scans for StatefulSets managed by pg-swarm that are not in the
// desired map. If a tombstone exists the cluster was intentionally deleted; if
// not, it is logged as an orphan.
func (o *Operator) CheckOrphans(ctx context.Context) {
	nsList, err := o.client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Warn().Err(err).Msg("orphan-check: failed to list namespaces")
		return
	}

	o.mu.RLock()
	desired := make(map[string]struct{}, len(o.desired))
	for k := range o.desired {
		desired[k] = struct{}{}
	}
	o.mu.RUnlock()

	for _, ns := range nsList.Items {
		stsList, err := o.client.AppsV1().StatefulSets(ns.Name).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s", LabelManagedBy, ManagedByValue),
		})
		if err != nil {
			log.Warn().Err(err).Str("namespace", ns.Name).Msg("orphan-check: failed to list statefulsets")
			continue
		}
		for _, sts := range stsList.Items {
			clusterName := sts.Labels[LabelCluster]
			if clusterName == "" {
				continue
			}
			key := clusterKey(ns.Name, clusterName)
			if _, ok := desired[key]; ok {
				continue
			}
			tombstoned, err := hasTombstone(ctx, o.client, ns.Name, clusterName)
			if err != nil {
				log.Warn().Err(err).Str("cluster", key).Msg("orphan-check: failed to check tombstone")
				continue
			}
			if tombstoned {
				log.Info().Str("cluster", key).Msg("orphan-check: cluster has tombstone (intentionally deleted)")
			} else {
				log.Warn().Str("cluster", key).Msg("orphan-check: orphaned cluster detected (no desired config, no tombstone)")
			}
		}
	}
}

// StartOrphanChecker runs CheckOrphans periodically until the context is cancelled.
func (o *Operator) StartOrphanChecker(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			o.CheckOrphans(ctx)
		}
	}
}

// StartDriftReconciler periodically checks managed StatefulSets for replica
// count drift (e.g. manual kubectl scale) and corrects them back to the
// desired count from the ClusterConfig.
func (o *Operator) StartDriftReconciler(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			o.reconcileDrift(ctx)
		}
	}
}

func (o *Operator) reconcileDrift(ctx context.Context) {
	o.mu.RLock()
	snapshot := make(map[string]*pgswarmv1.ClusterConfig, len(o.desired))
	for k, v := range o.desired {
		snapshot[k] = v
	}
	o.mu.RUnlock()

	for key, cfg := range snapshot {
		sts, err := o.client.AppsV1().StatefulSets(cfg.Namespace).Get(ctx, cfg.ClusterName, metav1.GetOptions{})
		if err != nil {
			if !apierrors.IsNotFound(err) {
				log.Warn().Err(err).Str("cluster", key).Msg("drift-check: failed to get statefulset")
			}
			continue
		}
		actual := int32(0)
		if sts.Spec.Replicas != nil {
			actual = *sts.Spec.Replicas
		}
		if actual != cfg.Replicas {
			log.Warn().
				Str("cluster", key).
				Int32("desired", cfg.Replicas).
				Int32("actual", actual).
				Msg("drift-check: replica count mismatch, correcting")
			sts.Spec.Replicas = &cfg.Replicas
			if _, err := o.client.AppsV1().StatefulSets(cfg.Namespace).Update(ctx, sts, metav1.UpdateOptions{}); err != nil {
				log.Error().Err(err).Str("cluster", key).Msg("drift-check: failed to correct replicas")
			} else {
				log.Info().Str("cluster", key).Int32("replicas", cfg.Replicas).Msg("drift-check: replicas corrected")
			}
		}
	}
}

// ensurePodLabels polls until all expected pods have a role label.
// Used when failover is disabled and no sidecar will label them.
func (o *Operator) ensurePodLabels(namespace, clusterName string, replicas int32) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Warn().Str("cluster", clusterName).Msg("timed out waiting to label pods")
			return
		case <-ticker.C:
			if err := labelPods(ctx, o.client, namespace, clusterName); err != nil {
				log.Trace().Err(err).Msg("ensurePodLabels: retry")
				continue
			}
			// Check if all pods are labeled
			pods, err := o.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("%s=%s,%s", LabelCluster, clusterName, LabelRole),
			})
			if err == nil && int32(len(pods.Items)) >= replicas {
				log.Info().Str("cluster", clusterName).Int("labeled", len(pods.Items)).Msg("all pods labeled")
				return
			}
		}
	}
}
