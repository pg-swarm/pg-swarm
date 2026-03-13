package operator

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"k8s.io/client-go/kubernetes"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
)

// Operator materializes ClusterConfig messages as running PostgreSQL HA clusters.
type Operator struct {
	client           kubernetes.Interface
	k8sClusterName   string // used in config-storage ConfigMap naming
	defaultNamespace string // fallback when ClusterConfig.Namespace is empty
	mu               sync.RWMutex
	desired          map[string]*pgswarmv1.ClusterConfig // key: "ns/cluster"
	applied          map[string]int64                    // key -> last applied config version
}

// New creates a new Operator backed by the given Kubernetes client.
// k8sClusterName is used for config-storage ConfigMap naming (pg-swarm-<k8s>-<pg>).
// defaultNamespace is used when a ClusterConfig has no namespace set.
func New(client kubernetes.Interface, k8sClusterName, defaultNamespace string) *Operator {
	if defaultNamespace == "" {
		defaultNamespace = "default"
	}
	return &Operator{
		client:           client,
		k8sClusterName:   k8sClusterName,
		defaultNamespace: defaultNamespace,
		desired:          make(map[string]*pgswarmv1.ClusterConfig),
		applied:          make(map[string]int64),
	}
}

// configStoreName returns the ConfigMap name for storing the received config:
// pg-swarm-<k8sClusterName>-<pgClusterName>
func (o *Operator) configStoreName(pgClusterName string) string {
	return fmt.Sprintf("pg-swarm-%s-%s", o.k8sClusterName, pgClusterName)
}

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
	o.resolveNamespace(cfg)
	key := clusterKey(cfg.Namespace, cfg.ClusterName)

	o.mu.RLock()
	appliedVersion := o.applied[key]
	o.mu.RUnlock()

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

	if err := o.reconcile(cfg); err != nil {
		return fmt.Errorf("reconcile %s: %w", key, err)
	}

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
	err := o.client.AppsV1().StatefulSets(ns).Delete(ctx, name, metav1.DeleteOptions{
		PropagationPolicy: &propagation,
	})
	if err != nil {
		log.Warn().Err(err).Str("resource", "statefulset/"+name).Msg("delete failed")
	}

	// Delete services
	for _, suffix := range []string{"headless", "rw", "ro"} {
		svcName := resourceName(name, suffix)
		if err := o.client.CoreV1().Services(ns).Delete(ctx, svcName, metav1.DeleteOptions{}); err != nil {
			log.Warn().Err(err).Str("resource", "service/"+svcName).Msg("delete failed")
		}
	}

	// Delete ConfigMap
	cmName := resourceName(name, "config")
	if err := o.client.CoreV1().ConfigMaps(ns).Delete(ctx, cmName, metav1.DeleteOptions{}); err != nil {
		log.Warn().Err(err).Str("resource", "configmap/"+cmName).Msg("delete failed")
	}

	// Delete Secret
	secretName := resourceName(name, "secret")
	if err := o.client.CoreV1().Secrets(ns).Delete(ctx, secretName, metav1.DeleteOptions{}); err != nil {
		log.Warn().Err(err).Str("resource", "secret/"+secretName).Msg("delete failed")
	}

	// Delete failover RBAC resources
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

	// Delete config-store ConfigMap
	cfgStoreName := o.configStoreName(name)
	if err := o.client.CoreV1().ConfigMaps(ns).Delete(ctx, cfgStoreName, metav1.DeleteOptions{}); err != nil {
		log.Warn().Err(err).Str("resource", "configmap/"+cfgStoreName).Msg("delete failed")
	}

	o.mu.Lock()
	delete(o.desired, key)
	delete(o.applied, key)
	o.mu.Unlock()

	log.Info().Str("cluster", key).Msg("cluster resources deleted")
	return nil
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
		ObjectMeta: metav1.ObjectMeta{
			Name:      o.configStoreName(cfg.ClusterName),
			Namespace: cfg.Namespace,
			Labels:    clusterLabels(cfg.ClusterName),
		},
		Data: map[string]string{
			"config.json": string(jsonBytes),
		},
	}
}

func (o *Operator) reconcile(cfg *pgswarmv1.ClusterConfig) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// 1. Ensure namespace
	if err := ensureNamespace(ctx, o.client, cfg.Namespace); err != nil {
		return fmt.Errorf("ensure namespace %s: %w", cfg.Namespace, err)
	}

	// 2. Store received config as a ConfigMap for inspection
	cfgStore := o.buildConfigStore(cfg)
	if err := createOrUpdateConfigMap(ctx, o.client, cfgStore); err != nil {
		return fmt.Errorf("config-store configmap: %w", err)
	}

	// 3. Secret (create if absent, never update to preserve passwords)
	secret := buildSecret(cfg)
	if err := createOrPreserveSecret(ctx, o.client, secret); err != nil {
		return fmt.Errorf("secret: %w", err)
	}

	// 4. ConfigMap (postgresql.conf + pg_hba.conf)
	cm := buildConfigMap(cfg)
	if err := createOrUpdateConfigMap(ctx, o.client, cm); err != nil {
		return fmt.Errorf("configmap: %w", err)
	}

	// 5. Services
	for _, svc := range []*corev1.Service{
		buildHeadlessService(cfg),
		buildRWService(cfg),
		buildROService(cfg),
	} {
		if err := createOrUpdateService(ctx, o.client, svc); err != nil {
			return fmt.Errorf("service %s: %w", svc.Name, err)
		}
	}

	// 6. Failover RBAC (ServiceAccount, Role, RoleBinding)
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
	sts := buildStatefulSet(cfg, secret.Name)
	if err := createOrUpdateStatefulSet(ctx, o.client, sts); err != nil {
		return fmt.Errorf("statefulset: %w", err)
	}

	// 8. Label pods (best-effort, pods may not exist yet)
	if err := labelPods(ctx, o.client, cfg.Namespace, cfg.ClusterName); err != nil {
		log.Warn().Err(err).Str("cluster", cfg.ClusterName).Msg("failed to label pods (will retry on next reconcile)")
	}

	return nil
}
