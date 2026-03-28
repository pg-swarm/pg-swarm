package operator

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
)

//go:embed scripts/pg-init.sh
var pgInitScript string

//go:embed scripts/pg-wrapper.sh
var pgWrapperScript string

//go:embed scripts/pg-wrapper-standalone.sh
var pgWrapperStandaloneScript string

const pgDataPath = "/var/lib/postgresql/data/pgdata"

// podSecurityContext returns the PodSecurityContext for all PG pods.
// Alpine-based images use UID/GID 70; Debian-based images use UID/GID 999.
func podSecurityContext(image string) *corev1.PodSecurityContext {
	id := int64(70)
	if !strings.Contains(image, "alpine") {
		id = 999
	}
	return &corev1.PodSecurityContext{
		RunAsUser:  &id,
		RunAsGroup: &id,
		FSGroup:    &id,
	}
}

// archiveEnabled returns true if WAL archiving is configured.
func archiveEnabled(cfg *pgswarmv1.ClusterConfig) bool {
	return cfg.Archive != nil && cfg.Archive.Mode != ""
}

// walStorageEnabled returns true if a separate WAL storage volume is configured.
func walStorageEnabled(cfg *pgswarmv1.ClusterConfig) bool {
	return cfg.WalStorage != nil && cfg.WalStorage.Size != ""
}

// buildWalVCT creates the PersistentVolumeClaim template for the dedicated WAL volume.
func buildWalVCT(cfg *pgswarmv1.ClusterConfig) corev1.PersistentVolumeClaim {
	objMeta := metav1.ObjectMeta{
		Name:   "wal",
		Labels: clusterLabels(cfg.ClusterName, cfg.ProfileName, cfg.LabelSelector),
	}
	if cfg.DeletionProtection {
		objMeta.Finalizers = []string{FinalizerPGSwarm}
	}
	pvc := corev1.PersistentVolumeClaim{
		ObjectMeta: objMeta,
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(cfg.WalStorage.Size),
				},
			},
		},
	}
	if cfg.WalStorage.StorageClass != "" {
		pvc.Spec.StorageClassName = &cfg.WalStorage.StorageClass
	}
	return pvc
}

// clusterStatusConfigMapName returns the ConfigMap name for cluster lifecycle status.
func clusterStatusConfigMapName(clusterName string) string {
	return clusterName + "-cluster-status"
}

// buildClusterStatusConfigMap creates the initial cluster-status ConfigMap.
// The health monitor owns updates; the operator only seeds it on first creation.
func buildClusterStatusConfigMap(cfg *pgswarmv1.ClusterConfig) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterStatusConfigMapName(cfg.ClusterName),
			Namespace: cfg.Namespace,
			Labels:    clusterLabels(cfg.ClusterName, cfg.ProfileName, cfg.LabelSelector),
		},
		Data: map[string]string{
			"lifecycle_state": "PROVISIONING",
			"reason":          "initial seed by operator",
		},
	}
}

// buildStatefulSet creates the StatefulSet for the PostgreSQL cluster.
func buildStatefulSet(cfg *pgswarmv1.ClusterConfig, secretName, defaultSentinelImage string) *appsv1.StatefulSet {
	selLabels := selectorLabels(cfg.ClusterName)
	allLabels := clusterLabels(cfg.ClusterName, cfg.ProfileName, cfg.LabelSelector)
	headlessSvc := resourceName(cfg.ClusterName, "headless")
	replicas := cfg.Replicas

	sts := &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.ClusterName,
			Namespace: cfg.Namespace,
			Labels:    allLabels,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: headlessSvc,
			Replicas:    &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: selLabels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: selLabels,
					Annotations: map[string]string{
						"pg-swarm.io/config-hash": configHash(cfg),
					},
				},
				Spec: corev1.PodSpec{
					SecurityContext: podSecurityContext(cfg.Postgres.Image),
					InitContainers: []corev1.Container{
						buildInitContainer(cfg, secretName),
					},
					Containers: []corev1.Container{
						buildMainContainer(cfg, secretName),
					},
					Volumes: []corev1.Volume{
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: resourceName(cfg.ClusterName, "config"),
									},
								},
							},
						},
						{
							Name: "secret",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: secretName,
								},
							},
						},
					},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				buildVCT(cfg),
			},
		},
	}

	// Separate WAL volume
	if walStorageEnabled(cfg) {
		sts.Spec.VolumeClaimTemplates = append(sts.Spec.VolumeClaimTemplates, buildWalVCT(cfg))
	}

	// Sentinel sidecar
	if sentinelEnabled(cfg) {
		sts.Spec.Template.Spec.Containers = append(
			sts.Spec.Template.Spec.Containers,
			buildSentinelSidecar(cfg, secretName, defaultSentinelImage),
		)
		sts.Spec.Template.Spec.ServiceAccountName = sentinelServiceAccountName(cfg.ClusterName)
		// Recovery rules ConfigMap volume
		sts.Spec.Template.Spec.Volumes = append(sts.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: "recovery-rules",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: recoveryRulesConfigMapName(cfg.ClusterName),
					},
				},
			},
		})
	}

	// Mount custom archive credentials as a volume
	if archiveEnabled(cfg) && cfg.Archive.Mode == "custom" && cfg.Archive.CredentialsSecret != nil {
		optional := true
		sts.Spec.Template.Spec.Volumes = append(sts.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: "archive-creds",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: cfg.Archive.CredentialsSecret.Name,
					Optional:   &optional,
				},
			},
		})
	}

	return sts
}

// buildVCT creates the PersistentVolumeClaim template for the main data volume.
func buildVCT(cfg *pgswarmv1.ClusterConfig) corev1.PersistentVolumeClaim {
	objMeta := metav1.ObjectMeta{
		Name:   "data",
		Labels: clusterLabels(cfg.ClusterName, cfg.ProfileName, cfg.LabelSelector),
	}
	if cfg.DeletionProtection {
		objMeta.Finalizers = []string{FinalizerPGSwarm}
	}
	pvc := corev1.PersistentVolumeClaim{
		ObjectMeta: objMeta,
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(cfg.Storage.Size),
				},
			},
		},
	}
	if cfg.Storage.StorageClass != "" {
		pvc.Spec.StorageClassName = &cfg.Storage.StorageClass
	}
	return pvc
}

// buildInitContainer creates the init container that bootstraps PG data and handles replication setup.
func buildInitContainer(cfg *pgswarmv1.ClusterConfig, secretName string) corev1.Container {
	rwSvc := resourceName(cfg.ClusterName, "rw")

	// Build archive-specific script blocks
	var primaryArchiveBlock, replicaRestoreBlock string

	// Determine restore_command for replicas
	restoreCmd := ""
	if archiveEnabled(cfg) && cfg.Archive.Mode == "custom" {
		restoreCmd = cfg.Archive.RestoreCommand
	}
	if restoreCmd != "" {
		replicaRestoreBlock = fmt.Sprintf(`
    # Inject restore_command for archive recovery
    echo "restore_command = '%s'" >> "$PGDATA/postgresql.auto.conf"`, restoreCmd)
	}

	// WAL symlink script block (used after initdb/pg_basebackup and in re-init path)
	walSymlinkBlock := ""
	walSymlinkIdempotentBlock := ""
	if walStorageEnabled(cfg) {
		walSymlinkBlock = `
    # Move WAL to separate volume and symlink
    if [ -d "/var/lib/postgresql/wal" ]; then
        mv "$PGDATA/pg_wal"/* /var/lib/postgresql/wal/ 2>/dev/null || true
        rm -rf "$PGDATA/pg_wal"
        ln -s /var/lib/postgresql/wal "$PGDATA/pg_wal"
    fi`
		walSymlinkIdempotentBlock = `
    if [ -d "/var/lib/postgresql/wal" ] && [ ! -L "$PGDATA/pg_wal" ]; then
        mv "$PGDATA/pg_wal"/* /var/lib/postgresql/wal/ 2>/dev/null || true
        rm -rf "$PGDATA/pg_wal"
        ln -s /var/lib/postgresql/wal "$PGDATA/pg_wal"
    fi`
	}

	initScript := strings.NewReplacer(
		"{{PGDATA}}", pgDataPath,
		"{{RW_SVC}}", rwSvc,
		"{{NAMESPACE}}", cfg.Namespace,
		"{{WAL_SYMLINK_IDEMPOTENT}}", walSymlinkIdempotentBlock,
		"{{PRIMARY_ARCHIVE}}", primaryArchiveBlock,
		"{{WAL_SYMLINK}}", walSymlinkBlock,
		"{{REPLICA_RESTORE}}", replicaRestoreBlock,
	).Replace(pgInitScript)

	c := corev1.Container{
		Name:    "pg-init",
		Image:   cfg.Postgres.Image,
		Command: []string{"bash", "-c", initScript},
		Env: []corev1.EnvVar{
			{
				Name: "POD_NAME",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
				},
			},
			{Name: "PGDATA", Value: pgDataPath},
			{
				Name: "POSTGRES_PASSWORD",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
						Key:                  "superuser-password",
					},
				},
			},
			{
				Name: "REPLICATION_PASSWORD",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
						Key:                  "replication-password",
					},
				},
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: "/var/lib/postgresql/data"},
			{Name: "config", MountPath: "/etc/pg-config"},
		},
	}

	// NOTE: Cluster-level database passwords are NOT injected as env vars.
	// Databases are created via sidecar CreateDatabaseCmd which carries the
	// password directly in the proto message. No init container involvement.

	// Mount WAL volume on init container
	if walStorageEnabled(cfg) {
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
			Name:      "wal",
			MountPath: "/var/lib/postgresql/wal",
		})
	}

	return c
}

// buildMainContainer creates the main postgres container with a restart-loop wrapper script.
// When sentinel is enabled, uses a slim wrapper that delegates recovery decisions to the
// sidecar. When disabled, uses a standalone wrapper with full self-contained recovery logic.
func buildMainContainer(cfg *pgswarmv1.ClusterConfig, secretName string) corev1.Container {
	rwSvc := resourceName(cfg.ClusterName, "rw")
	primaryHost := fmt.Sprintf("%s.%s.svc.cluster.local", rwSvc, cfg.Namespace)

	wrapperScript := pgWrapperScript
	if !sentinelEnabled(cfg) {
		wrapperScript = pgWrapperStandaloneScript
	}

	startupScript := strings.NewReplacer(
		"{{PRIMARY_HOST}}", primaryHost,
	).Replace(wrapperScript)

	c := corev1.Container{
		Name:    "postgres",
		Image:   cfg.Postgres.Image,
		Command: []string{"bash", "-c", startupScript},
		Ports: []corev1.ContainerPort{
			{Name: "postgres", ContainerPort: 5432, Protocol: corev1.ProtocolTCP},
		},
		Env: []corev1.EnvVar{
			{Name: "PGDATA", Value: pgDataPath},
			{
				Name: "POSTGRES_PASSWORD",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
						Key:                  "superuser-password",
					},
				},
			},
			{
				Name: "REPLICATION_PASSWORD",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
						Key:                  "replication-password",
					},
				},
			},
			{
				Name:      "POD_NAME",
				ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}},
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: "/var/lib/postgresql/data"},
			{Name: "config", MountPath: "/etc/pg-config", ReadOnly: true},
		},
		StartupProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"pg_isready", "-U", "postgres"},
				},
			},
			PeriodSeconds:    10,
			TimeoutSeconds:   5,
			FailureThreshold: 60,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"pg_isready", "-U", "postgres"},
				},
			},
			PeriodSeconds:    10,
			TimeoutSeconds:   5,
			FailureThreshold: 6,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"pg_isready", "-U", "postgres"},
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       5,
			TimeoutSeconds:      3,
			FailureThreshold:    3,
		},
	}

	if cfg.Resources != nil {
		c.Resources = corev1.ResourceRequirements{}
		if cfg.Resources.CpuRequest != "" || cfg.Resources.MemoryRequest != "" {
			c.Resources.Requests = corev1.ResourceList{}
			if cfg.Resources.CpuRequest != "" {
				c.Resources.Requests[corev1.ResourceCPU] = resource.MustParse(cfg.Resources.CpuRequest)
			}
			if cfg.Resources.MemoryRequest != "" {
				c.Resources.Requests[corev1.ResourceMemory] = resource.MustParse(cfg.Resources.MemoryRequest)
			}
		}
		if cfg.Resources.CpuLimit != "" || cfg.Resources.MemoryLimit != "" {
			c.Resources.Limits = corev1.ResourceList{}
			if cfg.Resources.CpuLimit != "" {
				c.Resources.Limits[corev1.ResourceCPU] = resource.MustParse(cfg.Resources.CpuLimit)
			}
			if cfg.Resources.MemoryLimit != "" {
				c.Resources.Limits[corev1.ResourceMemory] = resource.MustParse(cfg.Resources.MemoryLimit)
			}
		}
	}

	// Mount WAL volume on main container
	if walStorageEnabled(cfg) {
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
			Name:      "wal",
			MountPath: "/var/lib/postgresql/wal",
		})
	}

	// Custom archive mode with credentials: mount secret as env vars AND as a volume
	// (so file-based credentials like GOOGLE_APPLICATION_CREDENTIALS can be used).
	if archiveEnabled(cfg) && cfg.Archive.Mode == "custom" && cfg.Archive.CredentialsSecret != nil {
		optional := true
		c.EnvFrom = append(c.EnvFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: cfg.Archive.CredentialsSecret.Name,
				},
				Optional: &optional,
			},
		})
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
			Name:      "archive-creds",
			MountPath: "/etc/archive-creds",
			ReadOnly:  true,
		})
		c.Env = append(c.Env, corev1.EnvVar{
			Name:  "GOOGLE_APPLICATION_CREDENTIALS",
			Value: "/etc/archive-creds/service-account.json", // Expected key in the secret
		})
	}

	return c
}

// buildSentinelSidecar creates the sentinel sidecar container for leader election and promotion.
func buildSentinelSidecar(cfg *pgswarmv1.ClusterConfig, secretName, defaultSentinelImage string) corev1.Container {
	image := cfg.Sentinel.SidecarImage
	if image == "" {
		image = defaultSentinelImage
	}
	interval := cfg.Sentinel.HealthCheckIntervalSeconds
	if interval <= 0 {
		interval = 1
	}

	primaryHostDNS := fmt.Sprintf("%s.%s.svc.cluster.local", resourceName(cfg.ClusterName, "rw"), cfg.Namespace)

	return corev1.Container{
		Name:            "sentinel",
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Env: []corev1.EnvVar{
			{
				Name: "POD_NAME",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
				},
			},
			{
				Name: "POD_NAMESPACE",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
				},
			},
			{Name: "CLUSTER_NAME", Value: cfg.ClusterName},
			{Name: "HEALTH_CHECK_INTERVAL", Value: fmt.Sprintf("%d", interval)},
			{Name: "PRIMARY_HOST", Value: primaryHostDNS},
			{
				Name: "POSTGRES_PASSWORD",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
						Key:                  "superuser-password",
					},
				},
			},
			{
				Name: "REPLICATION_PASSWORD",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
						Key:                  "replication-password",
					},
				},
			},
			{
				Name:  "SATELLITE_ADDR",
				Value: "pg-swarm-satellite.pgswarm-system.svc.cluster.local:9091",
			},
			{
				Name: "SIDECAR_STREAM_TOKEN",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
						Key:                  "sidecar-stream-token",
					},
				},
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: "/var/lib/postgresql/data"},
			{Name: "recovery-rules", MountPath: "/etc/recovery-rules", ReadOnly: true},
		},
	}
}

// configHash computes a short hash of the config-affecting fields in a ClusterConfig.
// This is used as a pod template annotation so that K8s triggers a rolling restart
// when the ConfigMap content changes (K8s doesn't track ConfigMap data changes natively).
func configHash(cfg *pgswarmv1.ClusterConfig) string {
	h := sha256.New()
	// Include all fields that affect postgresql.conf and pg_hba.conf
	keys := make([]string, 0, len(cfg.PgParams))
	for k := range cfg.PgParams {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%s\n", k, cfg.PgParams[k])
	}
	for _, rule := range cfg.HbaRules {
		fmt.Fprintf(h, "hba:%s\n", rule)
	}
	if cfg.Archive != nil {
		fmt.Fprintf(h, "archive:%s:%s:%d\n", cfg.Archive.Mode, cfg.Archive.ArchiveCommand, cfg.Archive.ArchiveTimeoutSeconds)
	}
	if cfg.Resources != nil {
		fmt.Fprintf(h, "res:%s:%s:%s:%s\n", cfg.Resources.CpuRequest, cfg.Resources.CpuLimit, cfg.Resources.MemoryRequest, cfg.Resources.MemoryLimit)
	}
	if cfg.Postgres != nil {
		fmt.Fprintf(h, "pg:%s:%s\n", cfg.Postgres.Version, cfg.Postgres.Image)
	}
	if cfg.Sentinel != nil {
		fmt.Fprintf(h, "fo:%v:%s\n", cfg.Sentinel.Enabled, cfg.Sentinel.SidecarImage)
	}
	// NOTE: ClusterDatabases are intentionally NOT included in the hash.
	// Database changes are handled via sidecar SQL commands + pg_reload_conf()
	// without requiring a pod restart.
	// NOTE: ConfigVersion is intentionally NOT included. The hash should only
	// change when actual config content changes, not on every version bump.
	// Scale-only changes should not trigger a rolling restart.
	return hex.EncodeToString(h.Sum(nil))[:16]
}
