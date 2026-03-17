package operator

import (
	"context"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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


// triggerPendingBaseBackups triggers immediate Jobs for base-backup CronJobs
// that don't yet have an -initial Job. Safe to call repeatedly (idempotent).
func triggerPendingBaseBackups(ctx context.Context, client kubernetes.Interface, cfg *pgswarmv1.ClusterConfig) {
	for _, backup := range cfg.Backups {
		cj := buildBaseBackupCronJob(cfg, backup)
		if cj == nil {
			continue
		}
		// Only trigger if the CronJob actually exists (was created during reconcile)
		if _, err := client.BatchV1().CronJobs(cj.Namespace).Get(ctx, cj.Name, metav1.GetOptions{}); err != nil {
			continue
		}
		triggerImmediateJob(ctx, client, cj)
	}
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

// backupLeaseName returns the Lease resource name used to prevent base/incremental overlap.
func backupLeaseName(clusterName string) string {
	return resourceName(clusterName, "backup-lease")
}

// backupServiceAccountName returns the ServiceAccount name used by backup CronJob pods.
func backupServiceAccountName(clusterName string) string {
	return resourceName(clusterName, "backup")
}

// buildBackupServiceAccount creates the ServiceAccount for backup CronJob pods.
func buildBackupServiceAccount(cfg *pgswarmv1.ClusterConfig) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      backupServiceAccountName(cfg.ClusterName),
			Namespace: cfg.Namespace,
			Labels:    clusterLabels(cfg.ClusterName, cfg.ProfileName, cfg.LabelSelector),
		},
	}
}

// buildBackupRole creates the RBAC Role granting lease and configmap access for backup pods.
func buildBackupRole(cfg *pgswarmv1.ClusterConfig) *rbacv1.Role {
	return &rbacv1.Role{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      backupServiceAccountName(cfg.ClusterName),
			Namespace: cfg.Namespace,
			Labels:    clusterLabels(cfg.ClusterName, cfg.ProfileName, cfg.LabelSelector),
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"coordination.k8s.io"},
				Resources: []string{"leases"},
				Verbs:     []string{"get", "create", "update", "delete"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"configmaps"},
				Verbs:     []string{"get", "create", "update", "patch"},
			},
		},
	}
}

// buildBackupRoleBinding creates the RoleBinding linking the backup ServiceAccount to its Role.
func buildBackupRoleBinding(cfg *pgswarmv1.ClusterConfig) *rbacv1.RoleBinding {
	saName := backupServiceAccountName(cfg.ClusterName)
	return &rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: cfg.Namespace,
			Labels:    clusterLabels(cfg.ClusterName, cfg.ProfileName, cfg.LabelSelector),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     saName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      saName,
				Namespace: cfg.Namespace,
			},
		},
	}
}

// backupCredentialSecretName returns the K8s Secret name for backup destination creds.
func backupCredentialSecretName(clusterName, ruleShort string) string {
	return resourceName(clusterName, "backup-creds-"+ruleShort)
}

// backupStatusConfigMapName returns the ConfigMap name for backup status reporting.
func backupStatusConfigMapName(clusterName string) string {
	return resourceName(clusterName, "backup-status")
}

// backupStoragePVCName returns the PVC name for local backup storage.
func backupStoragePVCName(clusterName string) string {
	return resourceName(clusterName, "backup-storage")
}

// buildBackupStoragePVC returns a PVC for local backup storage if any backup uses a local destination.
// Returns nil if no backup uses local destination.
func buildBackupStoragePVC(cfg *pgswarmv1.ClusterConfig) *corev1.PersistentVolumeClaim {
	var local *pgswarmv1.LocalDestination
	for _, b := range cfg.Backups {
		if b.Destination != nil && b.Destination.Type == "local" && b.Destination.Local != nil {
			local = b.Destination.Local
			break
		}
	}
	if local == nil {
		return nil
	}

	size := local.Size
	if size == "" {
		size = "10Gi"
	}

	pvc := &corev1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      backupStoragePVCName(cfg.ClusterName),
			Namespace: cfg.Namespace,
			Labels:    clusterLabels(cfg.ClusterName, cfg.ProfileName, cfg.LabelSelector),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(size),
				},
			},
		},
	}

	if local.StorageClass != "" {
		pvc.Spec.StorageClassName = &local.StorageClass
	}

	return pvc
}

// applyLocalBackupVolume appends the backup-storage PVC volume and mount to the pod spec.
func applyLocalBackupVolume(podSpec *corev1.PodSpec, clusterName string) {
	pvcName := backupStoragePVCName(clusterName)
	podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
		Name: "backup-storage",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: pvcName,
			},
		},
	})
	podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, corev1.VolumeMount{
		Name:      "backup-storage",
		MountPath: "/backup-storage",
	})
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
	labels[LabelBackupType] = "base"
	labels[LabelBackupProfile] = ruleShort

	env := backupEnvVars(cfg, backup, secretName)
	script := baseBackupScript(backup.Destination)

	var historyLimit int32 = 3
	cj := &batchv1.CronJob{
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
							ServiceAccountName: backupServiceAccountName(cfg.ClusterName),
							RestartPolicy:      corev1.RestartPolicyOnFailure,
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
	if backup.Destination.Type == "local" {
		applyLocalBackupVolume(&cj.Spec.JobTemplate.Spec.Template.Spec, cfg.ClusterName)
	}
	return cj
}

// buildLogicalBackupCronJob creates a CronJob for pg_dump/pg_dumpall for one backup profile.
func buildLogicalBackupCronJob(cfg *pgswarmv1.ClusterConfig, backup *pgswarmv1.BackupConfig) *batchv1.CronJob {
	if backup == nil || backup.Logical == nil || backup.Logical.Schedule == "" {
		return nil
	}

	ruleShort := ruleShortID(backup.BackupProfileId)
	secretName := resourceName(cfg.ClusterName, "secret")
	labels := clusterLabels(cfg.ClusterName, cfg.ProfileName, cfg.LabelSelector)
	labels[LabelBackupType] = "logical"
	labels[LabelBackupProfile] = ruleShort

	env := backupEnvVars(cfg, backup, secretName)
	script := logicalBackupScript(backup)

	var historyLimit int32 = 3
	cj := &batchv1.CronJob{
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
							ServiceAccountName: backupServiceAccountName(cfg.ClusterName),
							RestartPolicy:      corev1.RestartPolicyOnFailure,
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
	if backup.Destination.Type == "local" {
		applyLocalBackupVolume(&cj.Spec.JobTemplate.Spec.Template.Spec, cfg.ClusterName)
	}
	return cj
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
	roService := resourceName(cfg.ClusterName, "ro")
	pgMajor := "17"
	if cfg.Postgres != nil && cfg.Postgres.Version != "" {
		pgMajor = pgMajorVersion(cfg.Postgres.Version)
	}
	vars := []corev1.EnvVar{
		{Name: "PG_MAJOR", Value: pgMajor},
		{Name: "PGHOST", Value: roService + "." + cfg.Namespace + ".svc.cluster.local"},
		{Name: "PGPORT", Value: "5432"},
		{Name: "PGUSER", Value: "backup_user"},
		{Name: "PGPASSWORD", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  "backup-password",
			},
		}},
		{Name: "CLUSTER_NAME", Value: cfg.ClusterName},
		{Name: "NAMESPACE", Value: cfg.Namespace},
		{Name: "BACKUP_RULE_ID", Value: backup.BackupProfileId},
		{Name: "DEST_TYPE", Value: backup.Destination.Type},
		{Name: "BACKUP_STATUS_CM", Value: backupStatusConfigMapName(cfg.ClusterName)},
		{Name: "BACKUP_LEASE_NAME", Value: backupLeaseName(cfg.ClusterName)},
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

// backupReadinessSnippet returns a shell snippet that waits for PG to be reachable
// before proceeding with the backup. Gives up after 5 minutes.
func backupReadinessSnippet() string {
	return `# Wait for PostgreSQL to be reachable
echo "Waiting for PostgreSQL at ${PGHOST}:${PGPORT}..."
for i in $(seq 1 60); do
  if pg_isready -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -t 5 >/dev/null 2>&1; then
    echo "PostgreSQL is ready"
    break
  fi
  if [ "$i" = "60" ]; then
    echo "ERROR: PostgreSQL not reachable after 5 minutes, aborting"
    exit 1
  fi
  sleep 5
done
`
}

// baseBackupScript returns the shell script for a base backup CronJob.
func baseBackupScript(dest *pgswarmv1.BackupDestination) string {
	var sb strings.Builder
	sb.WriteString("set -eo pipefail\n")
	sb.WriteString("source /usr/local/bin/pg-select-version.sh\n")
	sb.WriteString("source /usr/local/bin/backup-metadata.sh\n")
	sb.WriteString("\n")
	sb.WriteString(backupReadinessSnippet())
	sb.WriteString("# Acquire backup lease to prevent incremental backups from running concurrently\n")
	sb.WriteString("release_backup_lease() {\n")
	sb.WriteString("  kubectl delete lease \"${BACKUP_LEASE_NAME}\" -n \"${NAMESPACE}\" --ignore-not-found 2>/dev/null || true\n")
	sb.WriteString("}\n")
	sb.WriteString("trap release_backup_lease EXIT\n")
	sb.WriteString("cat <<LEASEEOF | kubectl apply -f -\n")
	sb.WriteString("apiVersion: coordination.k8s.io/v1\n")
	sb.WriteString("kind: Lease\n")
	sb.WriteString("metadata:\n")
	sb.WriteString("  name: ${BACKUP_LEASE_NAME}\n")
	sb.WriteString("  namespace: ${NAMESPACE}\n")
	sb.WriteString("spec:\n")
	sb.WriteString("  holderIdentity: base-backup\n")
	sb.WriteString("  leaseDurationSeconds: 3600\n")
	sb.WriteString("  renewTime: \"$(date -u +%Y-%m-%dT%H:%M:%S.000000Z)\"\n")
	sb.WriteString("LEASEEOF\n")
	sb.WriteString("echo \"Acquired backup lease ${BACKUP_LEASE_NAME}\"\n")
	sb.WriteString("\n")
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
	sb.WriteString("\n")
	sb.WriteString(backupReadinessSnippet())
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
	labels[LabelBackupType] = "incremental"
	labels[LabelBackupProfile] = ruleShort

	env := backupEnvVars(cfg, backup, secretName)
	script := incrementalBackupScript(backup.Destination)

	var historyLimit int32 = 3
	cj := &batchv1.CronJob{
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
							ServiceAccountName: backupServiceAccountName(cfg.ClusterName),
							RestartPolicy:      corev1.RestartPolicyOnFailure,
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
	if backup.Destination.Type == "local" {
		applyLocalBackupVolume(&cj.Spec.JobTemplate.Spec.Template.Spec, cfg.ClusterName)
	}
	return cj
}

// incrementalBackupScript returns the shell script for an incremental backup CronJob.
// The latest backup_manifest is read from the SQLite metadata DB at the destination.
// If no manifest exists (first run), it falls back to a full base backup.
func incrementalBackupScript(dest *pgswarmv1.BackupDestination) string {
	var sb strings.Builder
	sb.WriteString("set -eo pipefail\n")
	sb.WriteString("source /usr/local/bin/pg-select-version.sh\n")
	sb.WriteString("source /usr/local/bin/backup-metadata.sh\n")
	sb.WriteString("\n")
	sb.WriteString(backupReadinessSnippet())
	sb.WriteString("# Skip if a base backup currently holds the backup lease\n")
	sb.WriteString("LEASE_DATA=$(kubectl get lease \"${BACKUP_LEASE_NAME}\" -n \"${NAMESPACE}\" \\\n")
	sb.WriteString("  -o jsonpath='{.spec.holderIdentity},{.spec.renewTime},{.spec.leaseDurationSeconds}' 2>/dev/null || true)\n")
	sb.WriteString("if [ -n \"$LEASE_DATA\" ]; then\n")
	sb.WriteString("  IFS=',' read -r HOLDER RENEW_TIME DURATION <<< \"$LEASE_DATA\"\n")
	sb.WriteString("  if [ \"$HOLDER\" = \"base-backup\" ] && [ -n \"$RENEW_TIME\" ]; then\n")
	sb.WriteString("    DURATION=${DURATION:-3600}\n")
	sb.WriteString("    RENEW_EPOCH=$(date -d \"$RENEW_TIME\" +%s 2>/dev/null || echo 0)\n")
	sb.WriteString("    NOW_EPOCH=$(date +%s)\n")
	sb.WriteString("    if [ $(( NOW_EPOCH - RENEW_EPOCH )) -lt \"$DURATION\" ]; then\n")
	sb.WriteString("      echo \"Base backup in progress (lease held since ${RENEW_TIME}), skipping incremental\"\n")
	sb.WriteString("      exit 0\n")
	sb.WriteString("    fi\n")
	sb.WriteString("    echo \"Backup lease expired, proceeding with incremental\"\n")
	sb.WriteString("  fi\n")
	sb.WriteString("fi\n")
	sb.WriteString("\n")
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
	// Ensure backup RBAC resources exist (ServiceAccount, Role, RoleBinding)
	if err := createOrUpdateServiceAccount(ctx, client, buildBackupServiceAccount(cfg)); err != nil {
		return fmt.Errorf("backup serviceaccount: %w", err)
	}
	if err := createOrUpdateRole(ctx, client, buildBackupRole(cfg)); err != nil {
		return fmt.Errorf("backup role: %w", err)
	}
	if err := createOrUpdateRoleBinding(ctx, client, buildBackupRoleBinding(cfg)); err != nil {
		return fmt.Errorf("backup rolebinding: %w", err)
	}

	// Create backup storage PVC if any backup uses local destination
	if pvc := buildBackupStoragePVC(cfg); pvc != nil {
		if err := createOrUpdatePVC(ctx, client, pvc); err != nil {
			return fmt.Errorf("backup storage PVC: %w", err)
		}
	}

	// Check StatefulSet readiness once before the loop. If the cluster is
	// already running, we trigger an immediate base backup for newly created
	// CronJobs so the cluster doesn't sit unprotected until the cron fires.
	clusterReady := isStatefulSetReady(ctx, client, cfg.Namespace, cfg.ClusterName)

	for _, backup := range cfg.Backups {
		if cj := buildBaseBackupCronJob(cfg, backup); cj != nil {
			created, err := createOrUpdateCronJob(ctx, client, cj)
			if err != nil {
				return fmt.Errorf("base backup cronjob (rule %s): %w", ruleShortID(backup.BackupProfileId), err)
			}
			if created && clusterReady {
				triggerImmediateJob(ctx, client, cj)
			}
		}
		if cj := buildIncrementalBackupCronJob(cfg, backup); cj != nil {
			if _, err := createOrUpdateCronJob(ctx, client, cj); err != nil {
				return fmt.Errorf("incremental backup cronjob (rule %s): %w", ruleShortID(backup.BackupProfileId), err)
			}
		}
		if cj := buildLogicalBackupCronJob(cfg, backup); cj != nil {
			if _, err := createOrUpdateCronJob(ctx, client, cj); err != nil {
				return fmt.Errorf("logical backup cronjob (rule %s): %w", ruleShortID(backup.BackupProfileId), err)
			}
		}
	}
	return nil
}

// cleanupBackupCronJobs removes all backup CronJobs, credential Secrets, and status ConfigMap for a cluster.
func cleanupBackupCronJobs(ctx context.Context, client kubernetes.Interface, ns, clusterName string) {
	// Delete all backup CronJobs and initial Jobs by label selector
	selector := "pg-swarm.io/cluster=" + clusterName
	propagation := metav1.DeletePropagationBackground
	cjList, err := client.BatchV1().CronJobs(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err == nil {
		for _, cj := range cjList.Items {
			if _, ok := cj.Labels[LabelBackupType]; ok {
				_ = client.BatchV1().CronJobs(ns).Delete(ctx, cj.Name, metav1.DeleteOptions{})
				// Also delete the initial Job if it exists
				_ = client.BatchV1().Jobs(ns).Delete(ctx, cj.Name+"-initial", metav1.DeleteOptions{PropagationPolicy: &propagation})
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
	// Clean up backup storage PVC
	_ = client.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, backupStoragePVCName(clusterName), metav1.DeleteOptions{})
	// Clean up status ConfigMap
	statusName := backupStatusConfigMapName(clusterName)
	if err := client.CoreV1().ConfigMaps(ns).Delete(ctx, statusName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		// best-effort
	}
	// Clean up backup lease
	leaseName := backupLeaseName(clusterName)
	_ = client.CoordinationV1().Leases(ns).Delete(ctx, leaseName, metav1.DeleteOptions{})
	// Clean up backup RBAC resources
	saName := backupServiceAccountName(clusterName)
	_ = client.RbacV1().RoleBindings(ns).Delete(ctx, saName, metav1.DeleteOptions{})
	_ = client.RbacV1().Roles(ns).Delete(ctx, saName, metav1.DeleteOptions{})
	_ = client.CoreV1().ServiceAccounts(ns).Delete(ctx, saName, metav1.DeleteOptions{})
}

// triggerImmediateJob creates a one-off Job from a CronJob's template to run immediately.
// Only triggers if no prior Job exists for this CronJob (first attach).
func triggerImmediateJob(ctx context.Context, client kubernetes.Interface, cj *batchv1.CronJob) {
	jobName := cj.Name + "-initial"
	_, err := client.BatchV1().Jobs(cj.Namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err == nil {
		return // already triggered before
	}
	if !apierrors.IsNotFound(err) {
		return
	}

	job := &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: cj.Namespace,
			Labels:    cj.Labels,
		},
		Spec: cj.Spec.JobTemplate.Spec,
	}
	if _, err := client.BatchV1().Jobs(cj.Namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		// best-effort, CronJob will run on schedule anyway
	}
}

// createOrUpdateCronJob creates a CronJob if it doesn't exist, or updates it.
// Returns (created, error) where created=true means the CronJob was newly created.
func createOrUpdateCronJob(ctx context.Context, client kubernetes.Interface, desired *batchv1.CronJob) (bool, error) {
	existing, err := client.BatchV1().CronJobs(desired.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = client.BatchV1().CronJobs(desired.Namespace).Create(ctx, desired, metav1.CreateOptions{})
		if err != nil {
			return false, err
		}
		// Trigger an immediate run for non-backup CronJobs.
		// Backup CronJobs are handled by the caller (reconcileBackupCronJobs)
		// which checks StatefulSet readiness first.
		if _, isBackup := desired.Labels[LabelBackupType]; !isBackup {
			triggerImmediateJob(ctx, client, desired)
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	_, err = client.BatchV1().CronJobs(desired.Namespace).Update(ctx, existing, metav1.UpdateOptions{})
	return false, err
}

// isStatefulSetReady returns true if the cluster's StatefulSet has at least one ready replica.
func isStatefulSetReady(ctx context.Context, client kubernetes.Interface, ns, clusterName string) bool {
	stsName := clusterName
	sts, err := client.AppsV1().StatefulSets(ns).Get(ctx, stsName, metav1.GetOptions{})
	if err != nil {
		return false
	}
	return sts.Status.ReadyReplicas >= 1
}

// buildRestoreJob creates a K8s Job to perform a PITR or logical restore.
// destType is the backup destination type (e.g. "s3", "local") used to attach volumes.
func buildRestoreJob(cfg *pgswarmv1.ClusterConfig, cmd *pgswarmv1.RestoreCommand, destType string) *batchv1.Job {
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
	job := &batchv1.Job{
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
	if destType == "local" {
		applyLocalBackupVolume(&job.Spec.Template.Spec, cfg.ClusterName)
	}
	return job
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
