package operator

import (
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
)

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

func archiveEnabled(cfg *pgswarmv1.ClusterConfig) bool {
	return cfg.Archive != nil && cfg.Archive.Mode != ""
}

func walStorageEnabled(cfg *pgswarmv1.ClusterConfig) bool {
	return cfg.WalStorage != nil && cfg.WalStorage.Size != ""
}

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

func buildStatefulSet(cfg *pgswarmv1.ClusterConfig, secretName, defaultFailoverImage string) *appsv1.StatefulSet {
	labels := clusterLabels(cfg.ClusterName, cfg.ProfileName, cfg.LabelSelector)
	headlessSvc := resourceName(cfg.ClusterName, "headless")
	replicas := cfg.Replicas

	sts := &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.ClusterName,
			Namespace: cfg.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: headlessSvc,
			Replicas:    &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
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

	// PVC archive mode: add wal-archive VolumeClaimTemplate
	if archiveEnabled(cfg) && cfg.Archive.Mode == "pvc" && cfg.Archive.ArchiveStorage != nil {
		sts.Spec.VolumeClaimTemplates = append(sts.Spec.VolumeClaimTemplates, buildWalArchiveVCT(cfg))
	}

	// Failover sidecar
	if failoverEnabled(cfg) {
		sts.Spec.Template.Spec.Containers = append(
			sts.Spec.Template.Spec.Containers,
			buildFailoverSidecar(cfg, secretName, defaultFailoverImage),
		)
		sts.Spec.Template.Spec.ServiceAccountName = failoverServiceAccountName(cfg.ClusterName)
	}

	return sts
}

func buildWalArchiveVCT(cfg *pgswarmv1.ClusterConfig) corev1.PersistentVolumeClaim {
	objMeta := metav1.ObjectMeta{
		Name:   "wal-archive",
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
					corev1.ResourceStorage: resource.MustParse(cfg.Archive.ArchiveStorage.Size),
				},
			},
		},
	}
	if cfg.Archive.ArchiveStorage.StorageClass != "" {
		pvc.Spec.StorageClassName = &cfg.Archive.ArchiveStorage.StorageClass
	}
	return pvc
}

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

func buildInitContainer(cfg *pgswarmv1.ClusterConfig, secretName string) corev1.Container {
	headlessSvc := resourceName(cfg.ClusterName, "headless")

	// Build archive-specific script blocks
	var primaryArchiveBlock, replicaRestoreBlock string
	if archiveEnabled(cfg) && cfg.Archive.Mode == "pvc" {
		primaryArchiveBlock = `
    # Ensure wal-archive directory is ready
    mkdir -p /wal-archive
    chown postgres:postgres /wal-archive`
	}

	// Determine restore_command for replicas
	restoreCmd := ""
	if archiveEnabled(cfg) {
		switch cfg.Archive.Mode {
		case "pvc":
			restoreCmd = "cp /wal-archive/%f %p"
		case "custom":
			restoreCmd = cfg.Archive.RestoreCommand
		}
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

	initScript := fmt.Sprintf(`#!/bin/bash
set -e

ORDINAL=${POD_NAME##*-}
PGDATA="%s"

# Idempotent: skip if already initialized
if [ -f "$PGDATA/PG_VERSION" ]; then
    echo "PGDATA already initialized, copying config only"
    cp /etc/pg-config/postgresql.conf "$PGDATA/postgresql.conf"
    cp /etc/pg-config/pg_hba.conf "$PGDATA/pg_hba.conf"%s%s
    exit 0
fi

if [ "$ORDINAL" = "0" ]; then
    echo "Initializing primary (ordinal 0)"
    initdb -D "$PGDATA" --auth-local=trust --auth-host=md5

    # Set superuser password
    pg_ctl -D "$PGDATA" start -w -o "-c listen_addresses='localhost'"
    psql -U postgres -c "ALTER USER postgres PASSWORD '$POSTGRES_PASSWORD';"
    psql -U postgres -c "CREATE ROLE repl_user WITH REPLICATION LOGIN PASSWORD '$REPLICATION_PASSWORD';"
%s
    pg_ctl -D "$PGDATA" stop -w

    # Copy config
    cp /etc/pg-config/postgresql.conf "$PGDATA/postgresql.conf"
    cp /etc/pg-config/pg_hba.conf "$PGDATA/pg_hba.conf"%s%s
else
    echo "Initializing replica (ordinal $ORDINAL)"
    PRIMARY_HOST="%s-0.%s.%s.svc.cluster.local"
    until pg_isready -h "$PRIMARY_HOST" -U postgres; do
        echo "Waiting for primary..."
        sleep 2
    done
    PGPASSWORD="$REPLICATION_PASSWORD" pg_basebackup \
        -h "$PRIMARY_HOST" -U repl_user -D "$PGDATA" -R -Xs -P
    cp /etc/pg-config/postgresql.conf "$PGDATA/postgresql.conf"
    cp /etc/pg-config/pg_hba.conf "$PGDATA/pg_hba.conf"%s%s
fi
`, pgDataPath, walSymlinkIdempotentBlock, reinitDatabaseBlock, databaseSQL, primaryArchiveBlock, walSymlinkBlock, cfg.ClusterName, headlessSvc, cfg.Namespace, replicaRestoreBlock, walSymlinkBlock)

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

	// Mount wal-archive volume on init container for PVC mode
	if archiveEnabled(cfg) && cfg.Archive.Mode == "pvc" {
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
			Name:      "wal-archive",
			MountPath: "/wal-archive",
		})
	}

	return c
}

func buildMainContainer(cfg *pgswarmv1.ClusterConfig, secretName string) corev1.Container {
	c := corev1.Container{
		Name:  "postgres",
		Image: cfg.Postgres.Image,
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
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: "/var/lib/postgresql/data"},
			{Name: "config", MountPath: "/etc/pg-config", ReadOnly: true},
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"pg_isready", "-U", "postgres"},
				},
			},
			InitialDelaySeconds: 30,
			PeriodSeconds:       10,
			TimeoutSeconds:      5,
			FailureThreshold:    6,
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

	// PVC archive mode: mount wal-archive volume
	if archiveEnabled(cfg) && cfg.Archive.Mode == "pvc" {
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
			Name:      "wal-archive",
			MountPath: "/wal-archive",
		})
	}

	// Custom archive mode with credentials: mount secret as env vars
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
	}

	return c
}

func buildFailoverSidecar(cfg *pgswarmv1.ClusterConfig, secretName, defaultFailoverImage string) corev1.Container {
	image := cfg.Failover.SidecarImage
	if image == "" {
		image = defaultFailoverImage
	}
	interval := cfg.Failover.HealthCheckIntervalSeconds
	if interval <= 0 {
		interval = 5
	}

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
	}
}
