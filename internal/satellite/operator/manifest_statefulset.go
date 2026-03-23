package operator

import (
	_ "embed"
	"fmt"
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
// satelliteInfo is optional: [0] = satelliteID, [1] = satelliteName (for backup sidecar).
func buildStatefulSet(cfg *pgswarmv1.ClusterConfig, secretName, defaultFailoverImage string, satelliteInfo ...string) *appsv1.StatefulSet {
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

	// Failover sidecar
	if failoverEnabled(cfg) {
		sts.Spec.Template.Spec.Containers = append(
			sts.Spec.Template.Spec.Containers,
			buildFailoverSidecar(cfg, secretName, defaultFailoverImage),
		)
		sts.Spec.Template.Spec.ServiceAccountName = failoverServiceAccountName(cfg.ClusterName)
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

	// Backup sidecar
	if backupEnabled(cfg) {
		satID := ""
		satName := ""
		if len(satelliteInfo) > 0 {
			satID = satelliteInfo[0]
		}
		if len(satelliteInfo) > 1 {
			satName = satelliteInfo[1]
		}
		sts.Spec.Template.Spec.Containers = append(
			sts.Spec.Template.Spec.Containers,
			buildBackupSidecar(cfg, secretName, satID, satName),
		)
		if !failoverEnabled(cfg) {
			sts.Spec.Template.Spec.ServiceAccountName = backupServiceAccountName(cfg.ClusterName)
		}
	}

	// Shared emptyDir volumes for WAL staging (archive) and WAL restore (fetch).
	if backupEnabled(cfg) {
		sts.Spec.Template.Spec.Volumes = append(sts.Spec.Template.Spec.Volumes,
			corev1.Volume{
				Name:         "wal-staging",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			},
			corev1.Volume{
				Name:         "wal-restore",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			},
		)

		if cfg.Backups[0].Destination != nil && cfg.Backups[0].Destination.Type == "gcs" {
			ruleShort := ruleShortID(cfg.Backups[0].BackupProfileId)
			sts.Spec.Template.Spec.Volumes = append(sts.Spec.Template.Spec.Volumes, corev1.Volume{
				Name: "gcp-creds",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: backupCredentialSecretName(cfg.ClusterName, ruleShort),
						Items: []corev1.KeyToPath{
							{Key: "service-account-json", Path: "service-account.json"},
						},
					},
				},
			})
		}
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

// buildDatabaseSQL generates SQL to create users and databases from the config.
func buildDatabaseSQL(databases []*pgswarmv1.DatabaseSpec) string {
	if len(databases) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n    # Create application databases and users")
	for _, db := range databases {
		// Use password from the secret env var (password-<user>)
		envVar := fmt.Sprintf("DB_PASSWORD_%s", strings.ToUpper(db.User))
		sb.WriteString(fmt.Sprintf(`
    psql -U postgres -c "DO \$\$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='%s') THEN CREATE ROLE %s WITH LOGIN PASSWORD '$%s'; END IF; END \$\$;"
    psql -U postgres -c "SELECT 'CREATE DATABASE %s OWNER %s' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = '%s')" --no-align -t | psql -U postgres`,
			db.User, db.User, envVar,
			db.Name, db.User, db.Name))
	}
	return sb.String()
}

// buildInitContainer creates the init container that bootstraps PG data and handles replication setup.
func buildInitContainer(cfg *pgswarmv1.ClusterConfig, secretName string) corev1.Container {
	rwSvc := resourceName(cfg.ClusterName, "rw")

	// Build archive-specific script blocks
	var primaryArchiveBlock, replicaRestoreBlock string

	// Determine restore_command for replicas
	restoreCmd := ""
	// TODO: Re-enable when backup sidecar is restored.
	// if backupEnabled(cfg) {
	// 	restoreCmd = walRestoreCommand
	// } else
	if archiveEnabled(cfg) && cfg.Archive.Mode == "custom" {
		restoreCmd = cfg.Archive.RestoreCommand
	}
	if restoreCmd != "" {
		replicaRestoreBlock = fmt.Sprintf(`
    # Inject restore_command for archive recovery
    echo "restore_command = '%s'" >> "$PGDATA/postgresql.auto.conf"`, restoreCmd)
	}

	databaseSQL := buildDatabaseSQL(cfg.Databases)

	// Block for creating databases on an already-initialised primary (config updates / pod restarts).
	// Only runs on ordinal 0 when databases are defined and standby.signal is absent.
	reinitDatabaseBlock := ""
	if len(cfg.Databases) > 0 {
		reinitDatabaseBlock = fmt.Sprintf(`
    if [ "$ORDINAL" = "0" ] && [ ! -f "$PGDATA/standby.signal" ]; then
        echo "Ensuring application databases exist"
        pg_ctl -D "$PGDATA" start -w -o "-c listen_addresses='localhost'"
%s
        pg_ctl -D "$PGDATA" stop -w
    fi`, databaseSQL)
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
		"{{REINIT_DATABASE}}", reinitDatabaseBlock,
		"{{DATABASE_SQL}}", databaseSQL,
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
			{
				Name: "BACKUP_PASSWORD",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
						Key:                  "backup-password",
					},
				},
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: "/var/lib/postgresql/data"},
			{Name: "config", MountPath: "/etc/pg-config"},
		},
	}

	// Add env vars for database user passwords
	for _, db := range cfg.Databases {
		c.Env = append(c.Env, corev1.EnvVar{
			Name: fmt.Sprintf("DB_PASSWORD_%s", strings.ToUpper(db.User)),
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
					Key:                  fmt.Sprintf("password-%s", db.User),
				},
			},
		})
	}

	// Mount WAL volume on init container
	if walStorageEnabled(cfg) {
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
			Name:      "wal",
			MountPath: "/var/lib/postgresql/wal",
		})
	}

	// TODO: Re-enable when backup sidecar is restored.
	// // Mount wal-restore on init container (needed for recovery during init)
	// if backupEnabled(cfg) {
	// 	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
	// 		Name:      "wal-restore",
	// 		MountPath: "/wal-restore",
	// 	})
	// }

	return c
}

// buildMainContainer creates the main postgres container with a restart-loop wrapper script.
func buildMainContainer(cfg *pgswarmv1.ClusterConfig, secretName string) corev1.Container {
	rwSvc := resourceName(cfg.ClusterName, "rw")
	primaryHost := fmt.Sprintf("%s.%s.svc.cluster.local", rwSvc, cfg.Namespace)

	startupScript := strings.NewReplacer(
		"{{PRIMARY_HOST}}", primaryHost,
	).Replace(pgWrapperScript)

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

	// TODO: Re-enable when backup sidecar is restored.
	// // Shared emptyDir mounts for file-based WAL archive/restore
	// if backupEnabled(cfg) {
	// 	c.VolumeMounts = append(c.VolumeMounts,
	// 		corev1.VolumeMount{Name: "wal-staging", MountPath: "/wal-staging"},
	// 		corev1.VolumeMount{Name: "wal-restore", MountPath: "/wal-restore"},
	// 	)
	// }

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

// buildFailoverSidecar creates the failover sidecar container for leader election and promotion.
func buildFailoverSidecar(cfg *pgswarmv1.ClusterConfig, secretName, defaultFailoverImage string) corev1.Container {
	image := cfg.Failover.SidecarImage
	if image == "" {
		image = defaultFailoverImage
	}
	interval := cfg.Failover.HealthCheckIntervalSeconds
	if interval <= 0 {
		interval = 1
	}

	primaryHostDNS := fmt.Sprintf("%s.%s.svc.cluster.local", resourceName(cfg.ClusterName, "rw"), cfg.Namespace)

	return corev1.Container{
		Name:            "failover",
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
