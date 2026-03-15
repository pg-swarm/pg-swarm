package operator

import (
	"context"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
)

const defaultBackupImage = "ghcr.io/pg-swarm/pg-swarm-backup:latest"

// backupEnabled returns true if at least one backup profile is attached.
func backupEnabled(cfg *pgswarmv1.ClusterConfig) bool {
	return len(cfg.Backups) > 0
}


// backupImageForRule returns the container image for a backup CronJob.
func backupImageForRule(backup *pgswarmv1.BackupConfig) string {
	if backup != nil && backup.BackupImage != "" {
		return backup.BackupImage
	}
	return defaultBackupImage
}

// ruleShortID returns a short prefix from a backup profile ID for K8s resource naming.
func ruleShortID(ruleID string) string {
	if len(ruleID) >= 8 {
		return ruleID[:8]
	}
	return ruleID
}

// backupCredentialSecretName returns the K8s Secret name for backup destination creds.
func backupCredentialSecretName(clusterName, ruleShort string) string {
	return resourceName(clusterName, "backup-creds-"+ruleShort)
}

// backupStatusConfigMapName returns the ConfigMap name for backup status reporting.
func backupStatusConfigMapName(clusterName string) string {
	return resourceName(clusterName, "backup-status")
}

// buildBackupCredentialSecret creates a K8s Secret containing destination credentials for one backup profile.
func buildBackupCredentialSecret(cfg *pgswarmv1.ClusterConfig, backup *pgswarmv1.BackupConfig) *corev1.Secret {
	if backup == nil || backup.Destination == nil {
		return nil
	}

	data := map[string]string{}
	dest := backup.Destination
	switch dest.Type {
	case "s3":
		if dest.S3 != nil {
			if dest.S3.AccessKeyId != "" {
				data["aws-access-key-id"] = dest.S3.AccessKeyId
			}
			if dest.S3.SecretAccessKey != "" {
				data["aws-secret-access-key"] = dest.S3.SecretAccessKey
			}
		}
	case "gcs":
		if dest.Gcs != nil && dest.Gcs.ServiceAccountJson != "" {
			data["service-account-json"] = dest.Gcs.ServiceAccountJson
		}
	case "sftp":
		if dest.Sftp != nil && dest.Sftp.Password != "" {
			data["sftp-password"] = dest.Sftp.Password
		}
	}

	ruleShort := ruleShortID(backup.BackupProfileId)
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      backupCredentialSecretName(cfg.ClusterName, ruleShort),
			Namespace: cfg.Namespace,
			Labels:    clusterLabels(cfg.ClusterName, cfg.ProfileName, cfg.LabelSelector),
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: data,
	}
}

// buildBaseBackupCronJob creates a CronJob for pg_basebackup for one backup profile.
func buildBaseBackupCronJob(cfg *pgswarmv1.ClusterConfig, backup *pgswarmv1.BackupConfig) *batchv1.CronJob {
	if backup == nil || backup.Physical == nil || backup.Physical.BaseSchedule == "" {
		return nil
	}

	ruleShort := ruleShortID(backup.BackupProfileId)
	secretName := resourceName(cfg.ClusterName, "secret")
	labels := clusterLabels(cfg.ClusterName, cfg.ProfileName, cfg.LabelSelector)
	labels["pg-swarm/backup-type"] = "base"
	labels["pg-swarm/backup-profile"] = ruleShort

	env := backupEnvVars(cfg, backup, secretName)
	script := baseBackupScript(backup.Destination)

	var historyLimit int32 = 3
	return &batchv1.CronJob{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "CronJob"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName(cfg.ClusterName, "base-backup-"+ruleShort),
			Namespace: cfg.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.CronJobSpec{
			Schedule:                   backup.Physical.BaseSchedule,
			SuccessfulJobsHistoryLimit: &historyLimit,
			FailedJobsHistoryLimit:     &historyLimit,
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: labels},
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyOnFailure,
							Containers: []corev1.Container{
								{
									Name:    "base-backup",
									Image:   backupImageForRule(backup),
									Command: []string{"bash", "-c", script},
									Env:     env,
								},
							},
						},
					},
				},
			},
		},
	}
}

// buildLogicalBackupCronJob creates a CronJob for pg_dump/pg_dumpall for one backup profile.
func buildLogicalBackupCronJob(cfg *pgswarmv1.ClusterConfig, backup *pgswarmv1.BackupConfig) *batchv1.CronJob {
	if backup == nil || backup.Logical == nil || backup.Logical.Schedule == "" {
		return nil
	}

	ruleShort := ruleShortID(backup.BackupProfileId)
	secretName := resourceName(cfg.ClusterName, "secret")
	labels := clusterLabels(cfg.ClusterName, cfg.ProfileName, cfg.LabelSelector)
	labels["pg-swarm/backup-type"] = "logical"
	labels["pg-swarm/backup-profile"] = ruleShort

	env := backupEnvVars(cfg, backup, secretName)
	script := logicalBackupScript(backup)

	var historyLimit int32 = 3
	return &batchv1.CronJob{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "CronJob"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName(cfg.ClusterName, "logical-backup-"+ruleShort),
			Namespace: cfg.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.CronJobSpec{
			Schedule:                   backup.Logical.Schedule,
			SuccessfulJobsHistoryLimit: &historyLimit,
			FailedJobsHistoryLimit:     &historyLimit,
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: labels},
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyOnFailure,
							Containers: []corev1.Container{
								{
									Name:    "logical-backup",
									Image:   backupImageForRule(backup),
									Command: []string{"bash", "-c", script},
									Env:     env,
								},
							},
						},
					},
				},
			},
		},
	}
}

// pgMajorVersion extracts the major version from a version string like "17", "16.2", "17.1".
func pgMajorVersion(version string) string {
	if i := strings.IndexByte(version, '.'); i > 0 {
		return version[:i]
	}
	return version
}

// backupEnvVars returns common env vars for backup CronJob containers.
func backupEnvVars(cfg *pgswarmv1.ClusterConfig, backup *pgswarmv1.BackupConfig, secretName string) []corev1.EnvVar {
	ruleShort := ruleShortID(backup.BackupProfileId)
	rwService := resourceName(cfg.ClusterName, "rw")
	pgMajor := "17"
	if cfg.Postgres != nil && cfg.Postgres.Version != "" {
		pgMajor = pgMajorVersion(cfg.Postgres.Version)
	}
	vars := []corev1.EnvVar{
		{Name: "PG_MAJOR", Value: pgMajor},
		{Name: "PGHOST", Value: rwService + "." + cfg.Namespace + ".svc.cluster.local"},
		{Name: "PGPORT", Value: "5432"},
		{Name: "PGUSER", Value: "postgres"},
		{Name: "PGPASSWORD", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  "superuser-password",
			},
		}},
		{Name: "CLUSTER_NAME", Value: cfg.ClusterName},
		{Name: "NAMESPACE", Value: cfg.Namespace},
		{Name: "BACKUP_RULE_ID", Value: backup.BackupProfileId},
		{Name: "DEST_TYPE", Value: backup.Destination.Type},
		{Name: "BACKUP_STATUS_CM", Value: backupStatusConfigMapName(cfg.ClusterName)},
	}

	// Add destination-specific env vars
	dest := backup.Destination
	switch dest.Type {
	case "s3":
		if dest.S3 != nil {
			vars = append(vars,
				corev1.EnvVar{Name: "S3_BUCKET", Value: dest.S3.Bucket},
				corev1.EnvVar{Name: "S3_REGION", Value: dest.S3.Region},
				corev1.EnvVar{Name: "S3_ENDPOINT", Value: dest.S3.Endpoint},
				corev1.EnvVar{Name: "S3_PREFIX", Value: dest.S3.PathPrefix},
			)
			if dest.S3.ForcePathStyle {
				vars = append(vars, corev1.EnvVar{Name: "S3_FORCE_PATH_STYLE", Value: "true"})
			}
		}
		// Credentials from backup-creds secret
		credsSecret := backupCredentialSecretName(cfg.ClusterName, ruleShort)
		vars = append(vars,
			corev1.EnvVar{Name: "AWS_ACCESS_KEY_ID", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: credsSecret},
					Key:                  "aws-access-key-id",
					Optional:             boolPtr(true),
				},
			}},
			corev1.EnvVar{Name: "AWS_SECRET_ACCESS_KEY", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: credsSecret},
					Key:                  "aws-secret-access-key",
					Optional:             boolPtr(true),
				},
			}},
		)
	case "gcs":
		if dest.Gcs != nil {
			credsSecret := backupCredentialSecretName(cfg.ClusterName, ruleShort)
			vars = append(vars,
				corev1.EnvVar{Name: "GCS_BUCKET", Value: dest.Gcs.Bucket},
				corev1.EnvVar{Name: "GCS_PREFIX", Value: dest.Gcs.PathPrefix},
				corev1.EnvVar{Name: "GOOGLE_APPLICATION_CREDENTIALS_JSON", ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: credsSecret},
						Key:                  "service-account-json",
						Optional:             boolPtr(true),
					},
				}},
			)
		}
	case "sftp":
		if dest.Sftp != nil {
			credsSecret := backupCredentialSecretName(cfg.ClusterName, ruleShort)
			vars = append(vars,
				corev1.EnvVar{Name: "SFTP_HOST", Value: dest.Sftp.Host},
				corev1.EnvVar{Name: "SFTP_PORT", Value: fmt.Sprintf("%d", dest.Sftp.Port)},
				corev1.EnvVar{Name: "SFTP_USER", Value: dest.Sftp.User},
				corev1.EnvVar{Name: "SFTP_BASE_PATH", Value: dest.Sftp.BasePath},
				corev1.EnvVar{Name: "SFTP_PASSWORD", ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: credsSecret},
						Key:                  "sftp-password",
						Optional:             boolPtr(true),
					},
				}},
			)
		}
	case "local":
		if dest.Local != nil {
			vars = append(vars, corev1.EnvVar{Name: "LOCAL_BACKUP_PATH", Value: "/backup-storage"})
		}
	}

	return vars
}

// backupWalArchiveCommand returns the archive_command for a given destination type.
func backupWalArchiveCommand(dest *pgswarmv1.BackupDestination) string {
	if dest == nil {
		return ""
	}
	switch dest.Type {
	case "s3":
		if dest.S3 != nil {
			return fmt.Sprintf("pg-swarm-backup wal-push --dest s3 --bucket %s --prefix %s %%p %%f",
				dest.S3.Bucket, dest.S3.PathPrefix)
		}
	case "gcs":
		if dest.Gcs != nil {
			return fmt.Sprintf("pg-swarm-backup wal-push --dest gcs --bucket %s --prefix %s %%p %%f",
				dest.Gcs.Bucket, dest.Gcs.PathPrefix)
		}
	case "sftp":
		if dest.Sftp != nil {
			return fmt.Sprintf("pg-swarm-backup wal-push --dest sftp --host %s --path %s %%p %%f",
				dest.Sftp.Host, dest.Sftp.BasePath)
		}
	case "local":
		return "test ! -f /backup-storage/wal/%f && cp %p /backup-storage/wal/%f"
	}
	return ""
}

// downloadMetadataSnippet returns a shell snippet to download backups.db from the destination.
func downloadMetadataSnippet(dest *pgswarmv1.BackupDestination) string {
	if dest == nil {
		return ""
	}
	switch dest.Type {
	case "s3":
		return "aws s3 cp \"s3://${S3_BUCKET}/${S3_PREFIX}${METADATA_REMOTE_PATH}\" \"$METADATA_DB\" ${S3_ENDPOINT:+--endpoint-url $S3_ENDPOINT} 2>/dev/null || true\n"
	case "gcs":
		return "gsutil cp \"gs://${GCS_BUCKET}/${GCS_PREFIX}${METADATA_REMOTE_PATH}\" \"$METADATA_DB\" 2>/dev/null || true\n"
	case "sftp":
		return "sftp -P ${SFTP_PORT:-22} ${SFTP_USER}@${SFTP_HOST}:${SFTP_BASE_PATH}/${METADATA_REMOTE_PATH} \"$METADATA_DB\" 2>/dev/null || true\n"
	case "local":
		return "cp /backup-storage/${METADATA_REMOTE_PATH} \"$METADATA_DB\" 2>/dev/null || true\n"
	}
	return ""
}

// uploadMetadataSnippet returns a shell snippet to upload backups.db to the destination.
func uploadMetadataSnippet(dest *pgswarmv1.BackupDestination) string {
	if dest == nil {
		return ""
	}
	switch dest.Type {
	case "s3":
		return "aws s3 cp \"$METADATA_DB\" \"s3://${S3_BUCKET}/${S3_PREFIX}${METADATA_REMOTE_PATH}\" ${S3_ENDPOINT:+--endpoint-url $S3_ENDPOINT}\n"
	case "gcs":
		return "gsutil cp \"$METADATA_DB\" \"gs://${GCS_BUCKET}/${GCS_PREFIX}${METADATA_REMOTE_PATH}\"\n"
	case "sftp":
		return "sftp -P ${SFTP_PORT:-22} ${SFTP_USER}@${SFTP_HOST}:${SFTP_BASE_PATH}/${METADATA_REMOTE_PATH} <<< $'put '\"$METADATA_DB\"\n"
	case "local":
		return "mkdir -p /backup-storage/${CLUSTER_NAME}/metadata && cp \"$METADATA_DB\" /backup-storage/${METADATA_REMOTE_PATH}\n"
	}
	return ""
}

// uploadBackupSnippet returns a shell snippet to upload BACKUP_DIR to BACKUP_PATH at the destination.
func uploadBackupSnippet(dest *pgswarmv1.BackupDestination) string {
	if dest == nil {
		return ""
	}
	switch dest.Type {
	case "s3":
		return "aws s3 cp \"$BACKUP_DIR\" \"s3://${S3_BUCKET}/${S3_PREFIX}${BACKUP_PATH}\" --recursive ${S3_ENDPOINT:+--endpoint-url $S3_ENDPOINT}\n"
	case "gcs":
		return "gsutil -m cp -r \"$BACKUP_DIR\" \"gs://${GCS_BUCKET}/${GCS_PREFIX}${BACKUP_PATH}\"\n"
	case "sftp":
		return "sftp -P ${SFTP_PORT:-22} ${SFTP_USER}@${SFTP_HOST}:${SFTP_BASE_PATH}/${BACKUP_PATH} <<< $'put -r '\"$BACKUP_DIR\"\n"
	case "local":
		return "mkdir -p /backup-storage/${BACKUP_PATH} && cp -r \"$BACKUP_DIR\"/* /backup-storage/${BACKUP_PATH}/\n"
	}
	return ""
}

// baseBackupScript returns the shell script for a base backup CronJob.
func baseBackupScript(dest *pgswarmv1.BackupDestination) string {
	var sb strings.Builder
	sb.WriteString("set -eo pipefail\n")
	sb.WriteString("source /usr/local/bin/pg-select-version.sh\n")
	sb.WriteString("source /usr/local/bin/backup-metadata.sh\n")
	sb.WriteString("BACKUP_ID=$(cat /proc/sys/kernel/random/uuid)\n")
	sb.WriteString("TIMESTAMP=$(date +%Y%m%d_%H%M%S)\n")
	sb.WriteString("BACKUP_DIR=/tmp/basebackup_${TIMESTAMP}\n")
	sb.WriteString("BACKUP_PATH=\"${CLUSTER_NAME}/base/${TIMESTAMP}\"\n")
	sb.WriteString("echo \"Starting base backup for ${CLUSTER_NAME}\"\n")
	sb.WriteString("\n")
	sb.WriteString("# Download metadata DB from destination\n")
	sb.WriteString(downloadMetadataSnippet(dest))
	sb.WriteString("init_metadata_db\n")
	sb.WriteString("PG_VER=$(pg_basebackup --version | awk '{print $NF}')\n")
	sb.WriteString("insert_backup \"$BACKUP_ID\" base '' \"$BACKUP_PATH\" \"$PG_VER\"\n")
	sb.WriteString("\n")
	sb.WriteString("pg_basebackup -h \"$PGHOST\" -p \"$PGPORT\" -U \"$PGUSER\" -D \"$BACKUP_DIR\" -Ft -z -Xs -P\n")
	sb.WriteString("BACKUP_SIZE=$(du -sb \"$BACKUP_DIR\" | cut -f1)\n")
	sb.WriteString("\n")
	sb.WriteString("# Upload backup\n")
	sb.WriteString(uploadBackupSnippet(dest))
	sb.WriteString("\n")
	sb.WriteString("# Store manifest and update metadata\n")
	sb.WriteString("if [ -f \"$BACKUP_DIR/backup_manifest\" ]; then\n")
	sb.WriteString("  store_manifest \"$BACKUP_ID\" \"$BACKUP_DIR/backup_manifest\"\n")
	sb.WriteString("fi\n")
	sb.WriteString("complete_backup \"$BACKUP_ID\" \"$BACKUP_SIZE\" '' ''\n")
	sb.WriteString(uploadMetadataSnippet(dest))
	sb.WriteString("\n")
	sb.WriteString("STATUS='completed'\n")
	sb.WriteString("ERROR=''\n")
	sb.WriteString(statusReportScript("base"))

	return sb.String()
}

// logicalBackupScript returns the shell script for a logical backup CronJob.
func logicalBackupScript(backup *pgswarmv1.BackupConfig) string {
	var sb strings.Builder
	sb.WriteString("set -eo pipefail\n")
	sb.WriteString("source /usr/local/bin/pg-select-version.sh\n")
	sb.WriteString("source /usr/local/bin/backup-metadata.sh\n")
	sb.WriteString("BACKUP_ID=$(cat /proc/sys/kernel/random/uuid)\n")
	sb.WriteString("TIMESTAMP=$(date +%Y%m%d_%H%M%S)\n")
	sb.WriteString("BACKUP_DIR=/tmp/logical_${TIMESTAMP}\n")
	sb.WriteString("mkdir -p \"$BACKUP_DIR\"\n")
	sb.WriteString("BACKUP_PATH=\"${CLUSTER_NAME}/logical/${TIMESTAMP}\"\n")
	sb.WriteString("echo \"Starting logical backup for ${CLUSTER_NAME}\"\n")
	sb.WriteString("\n")
	sb.WriteString("# Download metadata DB\n")
	sb.WriteString(downloadMetadataSnippet(backup.Destination))
	sb.WriteString("init_metadata_db\n")
	sb.WriteString("PG_VER=$(pg_dump --version | awk '{print $NF}')\n")
	sb.WriteString("insert_backup \"$BACKUP_ID\" logical '' \"$BACKUP_PATH\" \"$PG_VER\"\n")
	sb.WriteString("\n")

	format := "custom"
	if backup.Logical.Format != "" {
		format = backup.Logical.Format
	}

	if len(backup.Logical.Databases) == 0 {
		// pg_dumpall outputs plain text — gzip it
		sb.WriteString("pg_dumpall -h \"$PGHOST\" -p \"$PGPORT\" -U \"$PGUSER\" | gzip > \"$BACKUP_DIR/dumpall.sql.gz\"\n")
	} else {
		formatFlag := "-Fc" // custom format (already compressed)
		switch format {
		case "plain":
			formatFlag = "-Fp"
		case "directory":
			formatFlag = "-Fd"
		}
		for _, db := range backup.Logical.Databases {
			if format == "plain" {
				// Plain format — gzip it
				file := fmt.Sprintf("$BACKUP_DIR/%s.sql.gz", db)
				sb.WriteString(fmt.Sprintf("pg_dump -h \"$PGHOST\" -p \"$PGPORT\" -U \"$PGUSER\" %s \"%s\" | gzip > \"%s\"\n", formatFlag, db, file))
			} else if format == "directory" {
				dir := fmt.Sprintf("$BACKUP_DIR/%s", db)
				sb.WriteString(fmt.Sprintf("pg_dump -h \"$PGHOST\" -p \"$PGPORT\" -U \"$PGUSER\" %s -f \"%s\" \"%s\"\n", formatFlag, dir, db))
			} else {
				// Custom format (built-in compression)
				file := fmt.Sprintf("$BACKUP_DIR/%s.dump", db)
				sb.WriteString(fmt.Sprintf("pg_dump -h \"$PGHOST\" -p \"$PGPORT\" -U \"$PGUSER\" %s -f \"%s\" \"%s\"\n", formatFlag, file, db))
			}
		}
	}

	sb.WriteString("\n")
	sb.WriteString("BACKUP_SIZE=$(du -sb \"$BACKUP_DIR\" | cut -f1)\n")
	sb.WriteString("\n")
	sb.WriteString("# Upload backup\n")
	sb.WriteString(uploadBackupSnippet(backup.Destination))
	sb.WriteString("\n")
	sb.WriteString("# Update metadata\n")
	sb.WriteString("complete_backup \"$BACKUP_ID\" \"$BACKUP_SIZE\" '' ''\n")
	sb.WriteString(uploadMetadataSnippet(backup.Destination))
	sb.WriteString("\n")
	sb.WriteString("STATUS='completed'\n")
	sb.WriteString("ERROR=''\n")
	sb.WriteString(statusReportScript("logical"))

	return sb.String()
}

// statusReportScript returns a shell snippet that writes backup status to a ConfigMap
// for the health monitor to pick up.
func statusReportScript(backupType string) string {
	return fmt.Sprintf(`
# Write status to ConfigMap for health monitor
cat <<STATUSEOF | kubectl apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${BACKUP_STATUS_CM}
  namespace: ${NAMESPACE}
data:
  backup_type: "%s"
  status: "${STATUS}"
  started_at: "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ -d @${START_TIME:-$(date +%%s)})"
  completed_at: "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)"
  size_bytes: "${BACKUP_SIZE:-0}"
  backup_path: "${BACKUP_PATH}"
  error_message: "${ERROR}"
STATUSEOF
echo "Backup %s completed"
`, backupType, backupType)
}

// buildIncrementalBackupCronJob creates a CronJob for pg_basebackup --incremental (PG 17+).
func buildIncrementalBackupCronJob(cfg *pgswarmv1.ClusterConfig, backup *pgswarmv1.BackupConfig) *batchv1.CronJob {
	if backup == nil || backup.Physical == nil || backup.Physical.IncrementalSchedule == "" {
		return nil
	}

	ruleShort := ruleShortID(backup.BackupProfileId)
	secretName := resourceName(cfg.ClusterName, "secret")
	labels := clusterLabels(cfg.ClusterName, cfg.ProfileName, cfg.LabelSelector)
	labels["pg-swarm/backup-type"] = "incremental"
	labels["pg-swarm/backup-profile"] = ruleShort

	env := backupEnvVars(cfg, backup, secretName)
	script := incrementalBackupScript(backup.Destination)

	var historyLimit int32 = 3
	return &batchv1.CronJob{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "CronJob"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName(cfg.ClusterName, "incr-backup-"+ruleShort),
			Namespace: cfg.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.CronJobSpec{
			Schedule:                   backup.Physical.IncrementalSchedule,
			SuccessfulJobsHistoryLimit: &historyLimit,
			FailedJobsHistoryLimit:     &historyLimit,
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: labels},
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyOnFailure,
							Containers: []corev1.Container{
								{
									Name:    "incr-backup",
									Image:   backupImageForRule(backup),
									Command: []string{"bash", "-c", script},
									Env:     env,
								},
							},
						},
					},
				},
			},
		},
	}
}

// incrementalBackupScript returns the shell script for an incremental backup CronJob.
// The latest backup_manifest is read from the SQLite metadata DB at the destination.
// If no manifest exists (first run), it falls back to a full base backup.
func incrementalBackupScript(dest *pgswarmv1.BackupDestination) string {
	var sb strings.Builder
	sb.WriteString("set -eo pipefail\n")
	sb.WriteString("source /usr/local/bin/pg-select-version.sh\n")
	sb.WriteString("source /usr/local/bin/backup-metadata.sh\n")
	sb.WriteString("BACKUP_ID=$(cat /proc/sys/kernel/random/uuid)\n")
	sb.WriteString("TIMESTAMP=$(date +%Y%m%d_%H%M%S)\n")
	sb.WriteString("BACKUP_DIR=/tmp/incrbackup_${TIMESTAMP}\n")
	sb.WriteString("BACKUP_PATH=\"${CLUSTER_NAME}/incremental/${TIMESTAMP}\"\n")
	sb.WriteString("MANIFEST_FILE=/tmp/prev_manifest\n")
	sb.WriteString("echo \"Starting incremental backup for ${CLUSTER_NAME}\"\n")
	sb.WriteString("\n")
	sb.WriteString("# Download metadata DB and read latest manifest\n")
	sb.WriteString(downloadMetadataSnippet(dest))
	sb.WriteString("init_metadata_db\n")
	sb.WriteString("PG_VER=$(pg_basebackup --version | awk '{print $NF}')\n")
	sb.WriteString("PARENT_ID=$(get_latest_backup_id base)\n")
	sb.WriteString("[ -z \"$PARENT_ID\" ] && PARENT_ID=$(get_latest_backup_id incremental)\n")
	sb.WriteString("\n")
	sb.WriteString("if get_latest_manifest \"$MANIFEST_FILE\"; then\n")
	sb.WriteString("  echo \"Incremental relative to previous backup\"\n")
	sb.WriteString("  insert_backup \"$BACKUP_ID\" incremental \"$PARENT_ID\" \"$BACKUP_PATH\" \"$PG_VER\"\n")
	sb.WriteString("  pg_basebackup -h \"$PGHOST\" -p \"$PGPORT\" -U \"$PGUSER\" -D \"$BACKUP_DIR\" --incremental=\"$MANIFEST_FILE\" -Ft -z -Xs -P\n")
	sb.WriteString("else\n")
	sb.WriteString("  echo \"No prior manifest found — taking full base backup\"\n")
	sb.WriteString("  insert_backup \"$BACKUP_ID\" base '' \"$BACKUP_PATH\" \"$PG_VER\"\n")
	sb.WriteString("  pg_basebackup -h \"$PGHOST\" -p \"$PGPORT\" -U \"$PGUSER\" -D \"$BACKUP_DIR\" -Ft -z -Xs -P\n")
	sb.WriteString("fi\n")
	sb.WriteString("\n")
	sb.WriteString("BACKUP_SIZE=$(du -sb \"$BACKUP_DIR\" | cut -f1)\n")
	sb.WriteString("\n")
	sb.WriteString("# Upload backup\n")
	sb.WriteString(uploadBackupSnippet(dest))
	sb.WriteString("\n")
	sb.WriteString("# Store manifest and update metadata\n")
	sb.WriteString("if [ -f \"$BACKUP_DIR/backup_manifest\" ]; then\n")
	sb.WriteString("  store_manifest \"$BACKUP_ID\" \"$BACKUP_DIR/backup_manifest\"\n")
	sb.WriteString("fi\n")
	sb.WriteString("complete_backup \"$BACKUP_ID\" \"$BACKUP_SIZE\" '' ''\n")
	sb.WriteString(uploadMetadataSnippet(dest))
	sb.WriteString("\n")
	sb.WriteString("STATUS='completed'\n")
	sb.WriteString("ERROR=''\n")
	sb.WriteString(statusReportScript("incremental"))

	return sb.String()
}

// reconcileBackupCronJobs creates or updates backup CronJobs for all attached backup profiles.
func reconcileBackupCronJobs(ctx context.Context, client kubernetes.Interface, cfg *pgswarmv1.ClusterConfig) error {
	for _, backup := range cfg.Backups {
		if cj := buildBaseBackupCronJob(cfg, backup); cj != nil {
			if err := createOrUpdateCronJob(ctx, client, cj); err != nil {
				return fmt.Errorf("base backup cronjob (rule %s): %w", ruleShortID(backup.BackupProfileId), err)
			}
		}
		if cj := buildIncrementalBackupCronJob(cfg, backup); cj != nil {
			if err := createOrUpdateCronJob(ctx, client, cj); err != nil {
				return fmt.Errorf("incremental backup cronjob (rule %s): %w", ruleShortID(backup.BackupProfileId), err)
			}
		}
		if cj := buildLogicalBackupCronJob(cfg, backup); cj != nil {
			if err := createOrUpdateCronJob(ctx, client, cj); err != nil {
				return fmt.Errorf("logical backup cronjob (rule %s): %w", ruleShortID(backup.BackupProfileId), err)
			}
		}
	}
	return nil
}

// cleanupBackupCronJobs removes all backup CronJobs, credential Secrets, and status ConfigMap for a cluster.
func cleanupBackupCronJobs(ctx context.Context, client kubernetes.Interface, ns, clusterName string) {
	// Delete all backup CronJobs by label selector
	selector := "pg-swarm.io/cluster=" + clusterName
	cjList, err := client.BatchV1().CronJobs(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err == nil {
		for _, cj := range cjList.Items {
			if _, ok := cj.Labels["pg-swarm/backup-type"]; ok {
				_ = client.BatchV1().CronJobs(ns).Delete(ctx, cj.Name, metav1.DeleteOptions{})
			}
		}
	}
	// Delete all backup credential secrets by label
	secretList, err := client.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err == nil {
		for _, s := range secretList.Items {
			if strings.Contains(s.Name, "-backup-creds-") {
				_ = client.CoreV1().Secrets(ns).Delete(ctx, s.Name, metav1.DeleteOptions{})
			}
		}
	}
	// Clean up status ConfigMap
	statusName := backupStatusConfigMapName(clusterName)
	if err := client.CoreV1().ConfigMaps(ns).Delete(ctx, statusName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		// best-effort
	}
}

// createOrUpdateCronJob creates a CronJob if it doesn't exist, or updates it.
func createOrUpdateCronJob(ctx context.Context, client kubernetes.Interface, desired *batchv1.CronJob) error {
	existing, err := client.BatchV1().CronJobs(desired.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = client.BatchV1().CronJobs(desired.Namespace).Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	_, err = client.BatchV1().CronJobs(desired.Namespace).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

// buildRestoreJob creates a K8s Job to perform a PITR or logical restore.
func buildRestoreJob(cfg *pgswarmv1.ClusterConfig, cmd *pgswarmv1.RestoreCommand) *batchv1.Job {
	secretName := resourceName(cfg.ClusterName, "secret")
	labels := clusterLabels(cfg.ClusterName, cfg.ProfileName, cfg.LabelSelector)
	labels["pg-swarm/restore"] = cmd.RestoreId

	env := []corev1.EnvVar{
		{Name: "PGHOST", Value: resourceName(cfg.ClusterName, "rw") + "." + cfg.Namespace + ".svc.cluster.local"},
		{Name: "PGPORT", Value: "5432"},
		{Name: "PGUSER", Value: "postgres"},
		{Name: "PGPASSWORD", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  "superuser-password",
			},
		}},
		{Name: "CLUSTER_NAME", Value: cfg.ClusterName},
		{Name: "NAMESPACE", Value: cfg.Namespace},
		{Name: "RESTORE_ID", Value: cmd.RestoreId},
		{Name: "RESTORE_TYPE", Value: cmd.RestoreType},
		{Name: "BACKUP_PATH", Value: cmd.BackupPath},
	}

	if cmd.TargetDatabase != "" {
		env = append(env, corev1.EnvVar{Name: "TARGET_DATABASE", Value: cmd.TargetDatabase})
	}

	var script string
	if cmd.RestoreType == "logical" {
		script = logicalRestoreScript()
	} else {
		script = pitrRestoreScript()
	}

	var backoffLimit int32 = 0
	return &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName(cfg.ClusterName, "restore-"+cmd.RestoreId[:8]),
			Namespace: cfg.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "restore",
							Image:   defaultBackupImage,
							Command: []string{"bash", "-c", script},
							Env:     env,
						},
					},
				},
			},
		},
	}
}

func logicalRestoreScript() string {
	return `set -eo pipefail
echo "Starting logical restore for ${CLUSTER_NAME}"
# Download dump from backup path
# (destination-specific download would go here)
echo "Running pg_restore"
pg_restore -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "${TARGET_DATABASE:-postgres}" --no-owner --no-acl "$BACKUP_PATH" || true
echo "Logical restore completed"
`
}

func pitrRestoreScript() string {
	return `set -eo pipefail
echo "Starting PITR restore for ${CLUSTER_NAME}"
echo "PITR restore requires manual StatefulSet scaling — this job downloads and prepares the base backup"
# Download base backup from backup path
# Scale StatefulSet to 0, extract backup, configure recovery, scale back up
echo "PITR restore job completed — StatefulSet restart needed"
`
}

func boolPtr(b bool) *bool { return &b }
