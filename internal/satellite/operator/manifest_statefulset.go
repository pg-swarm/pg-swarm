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

// buildStatefulSet creates the StatefulSet for the PostgreSQL cluster.
func buildStatefulSet(cfg *pgswarmv1.ClusterConfig, secretName, defaultFailoverImage string, satelliteID ...string) *appsv1.StatefulSet {
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
	}

	// Backup sidecar
	if backupEnabled(cfg) {
		satID := ""
		if len(satelliteID) > 0 {
			satID = satelliteID[0]
		}
		sts.Spec.Template.Spec.Containers = append(
			sts.Spec.Template.Spec.Containers,
			buildBackupSidecar(cfg, secretName, satID),
		)
		// If failover is not enabled but backup is, we still need a service account
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

		// Mount GCP credentials for backup sidecar if using GCS
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
	if backupEnabled(cfg) {
		// Sidecar handles WAL fetch via shared emptyDir volume
		restoreCmd = walRestoreCommand
	} else if archiveEnabled(cfg) && cfg.Archive.Mode == "custom" {
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

	initScript := fmt.Sprintf(`#!/bin/bash
set -e

ORDINAL=${POD_NAME##*-}
PGDATA="%s"
PRIMARY_HOST="%s.%s.svc.cluster.local"

# Idempotent: skip if already initialized
if [ -f "$PGDATA/PG_VERSION" ]; then
    echo "PGDATA already initialized, copying config only"
    cp /etc/pg-config/postgresql.conf "$PGDATA/postgresql.conf"
    cp /etc/pg-config/pg_hba.conf "$PGDATA/pg_hba.conf"%s%s

    # If this is a standby, check for timeline divergence before PG starts.
    # After a failover the new primary moves to timeline N+1. Replicas that
    # were on timeline N have a checkpoint AHEAD of the fork point and PG
    # will refuse to start, causing a CrashLoopBackOff.
    if [ -f "$PGDATA/standby.signal" ]; then
        LOCAL_TLI=$(pg_controldata -D "$PGDATA" 2>/dev/null | grep "Latest checkpoint.s TimeLineID" | awk '{print $NF}')
        echo "Local timeline: ${LOCAL_TLI:-unknown}"

        # Wait for the primary to be reachable (it may be starting up too).
        # Retry for up to 30 seconds — the new primary may still be booting.
        PRIMARY_READY=false
        for i in $(seq 1 6); do
            if pg_isready -h "$PRIMARY_HOST" -U postgres -t 5 >/dev/null 2>&1; then
                PRIMARY_READY=true
                break
            fi
            echo "Waiting for primary ($i/6)..."
        done

        if [ "$PRIMARY_READY" = "true" ]; then
            PRIMARY_TLI=$(PGPASSWORD="$POSTGRES_PASSWORD" psql -h "$PRIMARY_HOST" -U postgres -d postgres -tAc "SELECT timeline_id FROM pg_control_checkpoint()" 2>/dev/null || echo "")

            if [ -n "$PRIMARY_TLI" ] && [ -n "$LOCAL_TLI" ] && [ "$LOCAL_TLI" != "$PRIMARY_TLI" ]; then
                echo "TIMELINE DIVERGENCE: local=$LOCAL_TLI primary=$PRIMARY_TLI — running pg_rewind"

                # pg_rewind requires PG to have shut down cleanly. Run a crash
                # recovery by starting in single-user mode briefly if needed.
                if pg_controldata -D "$PGDATA" 2>/dev/null | grep -q "shut down in recovery"; then
                    echo "Clean shutdown detected, proceeding with pg_rewind"
                elif pg_controldata -D "$PGDATA" 2>/dev/null | grep -q "in production\|in archive recovery"; then
                    echo "Unclean shutdown — running single-user recovery first"
                    postgres --single -D "$PGDATA" -c config_file="$PGDATA/postgresql.conf" </dev/null >/dev/null 2>&1 || true
                fi

                if PGPASSWORD="$POSTGRES_PASSWORD" pg_rewind \
                    -D "$PGDATA" \
                    --source-server="host=$PRIMARY_HOST port=5432 user=postgres password=$POSTGRES_PASSWORD dbname=postgres" \
                    --progress 2>&1; then
                    echo "pg_rewind succeeded"
                    if [ -f "$PGDATA/backup_label" ]; then echo "Removing stale backup_label after pg_rewind"; rm -f "$PGDATA/backup_label"; fi
                    if [ -f "$PGDATA/tablespace_map" ]; then echo "Removing stale tablespace_map after pg_rewind"; rm -f "$PGDATA/tablespace_map"; fi
                    # Clean up stale/pre-allocated WAL segments to prevent "invalid record length"
                    CKPT_WAL=$(pg_controldata -D "$PGDATA" 2>/dev/null | grep "REDO WAL file" | awk '{print $NF}')
                    if [ -n "$CKPT_WAL" ]; then
                        echo "pg-swarm: cleaning stale WAL segments after pg_rewind (keeping $CKPT_WAL)"
                        find "$PGDATA/pg_wal" -maxdepth 1 -type f \
                            ! -name "$CKPT_WAL" ! -name "*.history" ! -name "*.backup" \
                            -delete 2>/dev/null || true
                    fi
                    if [ -n "$CKPT_WAL" ] && [ ! -f "$PGDATA/pg_wal/$CKPT_WAL" ]; then
                        echo "pg-swarm: checkpoint WAL $CKPT_WAL missing after pg_rewind — falling back to pg_basebackup"
                        MARKER="/var/lib/postgresql/data/.pg-swarm-needs-basebackup"
                        touch "$MARKER"
                        find "$PGDATA" -mindepth 1 -delete 2>/dev/null || true
                        PGPASSWORD="$REPLICATION_PASSWORD" pg_basebackup \
                            -h "$PRIMARY_HOST" -U repl_user -D "$PGDATA" -R -Xs -P
                        rm -f "$MARKER"
                        cp /etc/pg-config/postgresql.conf "$PGDATA/postgresql.conf"
                        cp /etc/pg-config/pg_hba.conf "$PGDATA/pg_hba.conf"
                    fi
                else
                    echo "pg_rewind failed — falling back to full re-basebackup"
                    # Mark outside PGDATA (on PVC root) so it survives cleanup
                    # and a failed pg_basebackup retries instead of hitting initdb.
                    MARKER="/var/lib/postgresql/data/.pg-swarm-needs-basebackup"
                    touch "$MARKER"
                    find "$PGDATA" -mindepth 1 -delete 2>/dev/null || true
                    PGPASSWORD="$REPLICATION_PASSWORD" pg_basebackup \
                        -h "$PRIMARY_HOST" -U repl_user -D "$PGDATA" -R -Xs -P
                    rm -f "$MARKER"
                    cp /etc/pg-config/postgresql.conf "$PGDATA/postgresql.conf"
                    cp /etc/pg-config/pg_hba.conf "$PGDATA/pg_hba.conf"
                fi

                # Ensure standby.signal and primary_conninfo point at the RW service
                touch "$PGDATA/standby.signal"
                sed -i '/^primary_conninfo/d' "$PGDATA/postgresql.auto.conf" 2>/dev/null || true
                echo "primary_conninfo = 'host=$PRIMARY_HOST port=5432 user=repl_user password=$REPLICATION_PASSWORD application_name=$POD_NAME'" >> "$PGDATA/postgresql.auto.conf"
                echo "Timeline recovery complete"
            fi
        else
            echo "Primary not reachable — skipping timeline check (PG will retry streaming)"
        fi
    fi

    exit 0
fi

# Check if this pod was a demoted primary that needs re-basebackup (not initdb).
# The marker lives on the PVC root (outside PGDATA) so it survives cleanup.
MARKER="/var/lib/postgresql/data/.pg-swarm-needs-basebackup"
NEEDS_BASEBACKUP=false
if [ -f "$MARKER" ]; then
    echo "Found needs-basebackup marker — previous re-basebackup failed, retrying"
    NEEDS_BASEBACKUP=true
    find "$PGDATA" -mindepth 1 -delete 2>/dev/null || true
    rm -f "$MARKER"
fi

# Clean stale partial data from a previous failed init (no PG_VERSION but dir not empty)
if [ -d "$PGDATA" ] && [ -n "$(ls -A "$PGDATA" 2>/dev/null)" ]; then
    echo "Cleaning stale PGDATA (no PG_VERSION but directory not empty)"
    find "$PGDATA" -mindepth 1 -delete 2>/dev/null || true
fi

if [ "$NEEDS_BASEBACKUP" = "false" ] && [ "$ORDINAL" = "0" ]; then
    echo "Initializing primary (ordinal 0)"
    initdb -D "$PGDATA" --auth-local=trust --auth-host=md5

    # Set superuser password
    pg_ctl -D "$PGDATA" start -w -o "-c listen_addresses='localhost'"
    psql -U postgres -c "ALTER USER postgres PASSWORD '$POSTGRES_PASSWORD';"
    psql -U postgres -c "CREATE ROLE repl_user WITH REPLICATION LOGIN PASSWORD '$REPLICATION_PASSWORD';"
    psql -U postgres -c "CREATE ROLE backup_user WITH REPLICATION LOGIN PASSWORD '$BACKUP_PASSWORD' IN ROLE pg_read_all_data;"
    psql -U postgres -c "CREATE EXTENSION IF NOT EXISTS pg_stat_statements;"
%s
    pg_ctl -D "$PGDATA" stop -w

    # Copy config
    cp /etc/pg-config/postgresql.conf "$PGDATA/postgresql.conf"
    cp /etc/pg-config/pg_hba.conf "$PGDATA/pg_hba.conf"%s%s
else
    echo "Initializing replica (ordinal $ORDINAL)"
    # PRIMARY_HOST is set at the top of the script (RW service follows pg-swarm.io/role=primary).
    until pg_isready -h "$PRIMARY_HOST" -U postgres; do
        echo "Waiting for primary..."
        sleep 2
    done
    PGPASSWORD="$REPLICATION_PASSWORD" pg_basebackup \
        -h "$PRIMARY_HOST" -U repl_user -D "$PGDATA" -R -Xs -P
    cp /etc/pg-config/postgresql.conf "$PGDATA/postgresql.conf"
    cp /etc/pg-config/pg_hba.conf "$PGDATA/pg_hba.conf"%s%s
fi
`, pgDataPath, rwSvc, cfg.Namespace, walSymlinkIdempotentBlock, reinitDatabaseBlock, databaseSQL, primaryArchiveBlock, walSymlinkBlock, replicaRestoreBlock, walSymlinkBlock)

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

	// Mount wal-restore on init container (needed for recovery during init)
	if backupEnabled(cfg) {
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
			Name:      "wal-restore",
			MountPath: "/wal-restore",
		})
	}

	return c
}

// buildMainContainer creates the main postgres container with a restart-loop wrapper script.
func buildMainContainer(cfg *pgswarmv1.ClusterConfig, secretName string) corev1.Container {
	rwSvc := resourceName(cfg.ClusterName, "rw")
	primaryHost := fmt.Sprintf("%s.%s.svc.cluster.local", rwSvc, cfg.Namespace)

	// Wrapper script that keeps the container alive across PG restarts.
	//
	// When the failover sidecar demotes this node (pg_ctl stop) or PG crashes
	// from a timeline mismatch, PG exits but the CONTAINER stays running.
	// The wrapper detects the exit, runs pg_rewind if needed, and restarts PG
	// in-place — no K8s container restart, no restart counter increment.
	//
	// Only a K8s SIGTERM (pod deletion / rolling update) exits the container.
	// We distinguish the two cases via a SHUTTING_DOWN flag set by the trap.
	startupScript := fmt.Sprintf(`#!/bin/bash
PRIMARY_HOST="%s"
ORDINAL=${POD_NAME##*-}

# --- Timeline recovery function ---
pg_swarm_recover() {
    if [ ! -f "$PGDATA/standby.signal" ] || [ ! -f "$PGDATA/PG_VERSION" ]; then
        return
    fi

    LOCAL_TLI=$(pg_controldata -D "$PGDATA" 2>/dev/null | grep "Latest checkpoint.s TimeLineID" | awk '{print $NF}')
    echo "pg-swarm: checking timeline, local=${LOCAL_TLI:-unknown}"

    # Wait for the primary (up to 30s)
    for i in $(seq 1 6); do
        if pg_isready -h "$PRIMARY_HOST" -U postgres -t 5 >/dev/null 2>&1; then
            break
        fi
        echo "pg-swarm: waiting for primary ($i/6)..."
        if [ "$i" = "6" ]; then
            echo "pg-swarm: primary not reachable — will start PG anyway"
            return
        fi
    done

    PRIMARY_TLI=$(PGPASSWORD="$POSTGRES_PASSWORD" psql -h "$PRIMARY_HOST" -U postgres -d postgres -tAc "SELECT timeline_id FROM pg_control_checkpoint()" 2>/dev/null || echo "")

    if [ -z "$PRIMARY_TLI" ] || [ -z "$LOCAL_TLI" ] || [ "$LOCAL_TLI" = "$PRIMARY_TLI" ]; then
        return
    fi

    echo "pg-swarm: TIMELINE DIVERGENCE local=$LOCAL_TLI primary=$PRIMARY_TLI"

    # Ensure clean shutdown state for pg_rewind
    DB_STATE=$(pg_controldata -D "$PGDATA" 2>/dev/null | grep "Database cluster state" | sed 's/.*: *//')
    case "$DB_STATE" in
        "shut down"|"shut down in recovery")
            ;;
        *)
            echo "pg-swarm: unclean state ($DB_STATE) — running single-user recovery"
            postgres --single -D "$PGDATA" -c config_file="$PGDATA/postgresql.conf" </dev/null >/dev/null 2>&1 || true
            ;;
    esac

    if PGPASSWORD="$POSTGRES_PASSWORD" pg_rewind \
        -D "$PGDATA" \
        --source-server="host=$PRIMARY_HOST port=5432 user=postgres password=$POSTGRES_PASSWORD dbname=postgres" \
        --progress 2>&1; then
        echo "pg-swarm: pg_rewind succeeded"
        if [ -f "$PGDATA/backup_label" ]; then echo "pg-swarm: removing stale backup_label after pg_rewind"; rm -f "$PGDATA/backup_label"; fi
        if [ -f "$PGDATA/tablespace_map" ]; then echo "pg-swarm: removing stale tablespace_map after pg_rewind"; rm -f "$PGDATA/tablespace_map"; fi
        # Clean up stale/pre-allocated WAL segments to prevent "invalid record length"
        CKPT_WAL=$(pg_controldata -D "$PGDATA" 2>/dev/null | grep "REDO WAL file" | awk '{print $NF}')
        if [ -n "$CKPT_WAL" ]; then
            echo "pg-swarm: cleaning stale WAL segments after pg_rewind (keeping $CKPT_WAL)"
            find "$PGDATA/pg_wal" -maxdepth 1 -type f \
                ! -name "$CKPT_WAL" ! -name "*.history" ! -name "*.backup" \
                -delete 2>/dev/null || true
        fi
        if [ -n "$CKPT_WAL" ] && [ ! -f "$PGDATA/pg_wal/$CKPT_WAL" ]; then
            echo "pg-swarm: checkpoint WAL $CKPT_WAL missing after pg_rewind — falling back to pg_basebackup"
            if ! pg_isready -h "$PRIMARY_HOST" -U postgres -t 5 >/dev/null 2>&1; then
                echo "pg-swarm: primary not reachable — skipping destructive recovery to preserve data"
                return
            fi
            MARKER="/var/lib/postgresql/data/.pg-swarm-needs-basebackup"
            touch "$MARKER"
            find "$PGDATA" -mindepth 1 -delete 2>/dev/null || true
            if PGPASSWORD="$REPLICATION_PASSWORD" pg_basebackup \
                -h "$PRIMARY_HOST" -U repl_user -D "$PGDATA" -R -Xs -P; then
                rm -f "$MARKER"
                cp /etc/pg-config/postgresql.conf "$PGDATA/postgresql.conf"
                cp /etc/pg-config/pg_hba.conf "$PGDATA/pg_hba.conf"
            else
                echo "pg-swarm: pg_basebackup failed — will retry on next loop iteration"
                return
            fi
        fi
    else
        echo "pg-swarm: pg_rewind failed — checking if primary is available for re-basebackup"
        if ! pg_isready -h "$PRIMARY_HOST" -U postgres -t 5 >/dev/null 2>&1; then
            echo "pg-swarm: primary not reachable — skipping destructive recovery to preserve data"
            return
        fi
        MARKER="/var/lib/postgresql/data/.pg-swarm-needs-basebackup"
        touch "$MARKER"
        find "$PGDATA" -mindepth 1 -delete 2>/dev/null || true
        if PGPASSWORD="$REPLICATION_PASSWORD" pg_basebackup \
            -h "$PRIMARY_HOST" -U repl_user -D "$PGDATA" -R -Xs -P; then
            rm -f "$MARKER"
            cp /etc/pg-config/postgresql.conf "$PGDATA/postgresql.conf"
            cp /etc/pg-config/pg_hba.conf "$PGDATA/pg_hba.conf"
        else
            echo "pg-swarm: pg_basebackup failed — will retry on next loop iteration"
            return
        fi
    fi

    touch "$PGDATA/standby.signal"
    sed -i '/^primary_conninfo/d' "$PGDATA/postgresql.auto.conf" 2>/dev/null || true
    echo "primary_conninfo = 'host=$PRIMARY_HOST port=5432 user=repl_user password=$REPLICATION_PASSWORD application_name=$POD_NAME'" >> "$PGDATA/postgresql.auto.conf"
    echo "pg-swarm: timeline recovery complete"
}

# --- Main loop ---
# SIGTERM from K8s (pod deletion) sets SHUTTING_DOWN and forwards to PG.
# pg_ctl stop from the sidecar sends SIGTERM directly to PG, NOT to us.
SHUTTING_DOWN=false
trap 'SHUTTING_DOWN=true; kill -TERM $PG_PID 2>/dev/null' TERM

while true; do
    # Check and fix timeline divergence before starting PG
    pg_swarm_recover

    # Check if a forced re-basebackup was requested by the sidecar (e.g. WAL gap)
    MARKER="/var/lib/postgresql/data/.pg-swarm-needs-basebackup"
    if [ -f "$MARKER" ]; then
        echo "pg-swarm: forced re-basebackup requested (e.g. WAL gap)"
        find "$PGDATA" -mindepth 1 -delete 2>/dev/null || true
        echo "pg-swarm: waiting for primary to re-basebackup..."
        BASEBACKUP_OK=false
        for i in $(seq 1 60); do
            if pg_isready -h "$PRIMARY_HOST" -U postgres -t 2 >/dev/null 2>&1; then
                if PGPASSWORD="$REPLICATION_PASSWORD" pg_basebackup \
                    -h "$PRIMARY_HOST" -U repl_user -D "$PGDATA" -R -Xs -P; then
                    cp /etc/pg-config/postgresql.conf "$PGDATA/postgresql.conf"
                    cp /etc/pg-config/pg_hba.conf "$PGDATA/pg_hba.conf"
                    rm -f "$MARKER"
                    BASEBACKUP_OK=true
                    break
                fi
            fi
            sleep 5
        done
        if [ "$BASEBACKUP_OK" = "false" ]; then
            echo "pg-swarm: re-basebackup failed — retrying next iteration"
            sleep 5
            continue
        fi
    fi

    # Guard: if PGDATA has files but no PG_VERSION, it is corrupt (e.g. a
    # previous pg_basebackup failed partway through). Clean up and either
    # re-basebackup (replicas) or let docker-entrypoint.sh initdb (primary).
    if [ -d "$PGDATA" ] && [ -n "$(ls -A "$PGDATA" 2>/dev/null)" ] && [ ! -s "$PGDATA/PG_VERSION" ]; then
        echo "pg-swarm: corrupt PGDATA (no PG_VERSION) — cleaning up"
        find "$PGDATA" -mindepth 1 -delete 2>/dev/null || true
        if [ "$ORDINAL" != "0" ]; then
            # Replica: wait for primary and re-basebackup
            echo "pg-swarm: waiting for primary to re-basebackup..."
            BASEBACKUP_OK=false
            for i in $(seq 1 60); do
                if pg_isready -h "$PRIMARY_HOST" -U postgres -t 2 >/dev/null 2>&1; then
                    if PGPASSWORD="$REPLICATION_PASSWORD" pg_basebackup \
                        -h "$PRIMARY_HOST" -U repl_user -D "$PGDATA" -R -Xs -P; then
                        cp /etc/pg-config/postgresql.conf "$PGDATA/postgresql.conf"
                        cp /etc/pg-config/pg_hba.conf "$PGDATA/pg_hba.conf"
                        BASEBACKUP_OK=true
                        break
                    fi
                fi
                sleep 5
            done
            if [ "$BASEBACKUP_OK" = "false" ]; then
                echo "pg-swarm: re-basebackup failed — retrying next iteration"
                sleep 5
                continue
            fi
        fi
        # For ordinal 0 (primary), fall through to docker-entrypoint.sh which
        # will run initdb on the now-empty directory.
    fi

    # Final guard: verify checkpoint WAL exists before starting PG
    if [ -f "$PGDATA/PG_VERSION" ]; then
        CKPT_WAL=$(pg_controldata -D "$PGDATA" 2>/dev/null | grep "REDO WAL file" | awk '{print $NF}')
        if [ -n "$CKPT_WAL" ] && [ ! -f "$PGDATA/pg_wal/$CKPT_WAL" ]; then
            echo "pg-swarm: CRITICAL — checkpoint WAL $CKPT_WAL missing from pg_wal/"
            if [ "$ORDINAL" != "0" ]; then
                echo "pg-swarm: replica — falling back to full re-basebackup"
                find "$PGDATA" -mindepth 1 -delete 2>/dev/null || true
                BASEBACKUP_OK=false
                for i in $(seq 1 60); do
                    if pg_isready -h "$PRIMARY_HOST" -U postgres -t 2 >/dev/null 2>&1; then
                        if PGPASSWORD="$REPLICATION_PASSWORD" pg_basebackup \
                            -h "$PRIMARY_HOST" -U repl_user -D "$PGDATA" -R -Xs -P; then
                            cp /etc/pg-config/postgresql.conf "$PGDATA/postgresql.conf"
                            cp /etc/pg-config/pg_hba.conf "$PGDATA/pg_hba.conf"
                            BASEBACKUP_OK=true
                            break
                        fi
                    fi
                    sleep 5
                done
                if [ "$BASEBACKUP_OK" = "false" ]; then
                    echo "pg-swarm: re-basebackup failed — retrying next iteration"
                    sleep 5
                    continue
                fi
            else
                echo "pg-swarm: primary — attempting pg_resetwal to recover"
                pg_resetwal -f -D "$PGDATA" 2>&1 || true
            fi
        fi
    fi

    # Start PG in the background so we can catch its exit
    docker-entrypoint.sh postgres &
    PG_PID=$!
    wait $PG_PID
    EXIT_CODE=$?

    # K8s sent SIGTERM → exit the container cleanly
    if [ "$SHUTTING_DOWN" = "true" ]; then
        echo "pg-swarm: shutting down"
        exit 0
    fi

    # PG exited on its own (sidecar demotion or crash) → recover and restart
    echo "pg-swarm: postgres exited (code=$EXIT_CODE) — recovering in-place"
    sleep 2
done
`, primaryHost)

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

	// Shared emptyDir mounts for file-based WAL archive/restore
	if backupEnabled(cfg) {
		c.VolumeMounts = append(c.VolumeMounts,
			corev1.VolumeMount{Name: "wal-staging", MountPath: "/wal-staging"},
			corev1.VolumeMount{Name: "wal-restore", MountPath: "/wal-restore"},
		)
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
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: "/var/lib/postgresql/data"},
		},
	}
}
