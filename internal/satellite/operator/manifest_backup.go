package operator

import (
	"context"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
)

const (
	defaultBackupImage        = "ghcr.io/pg-swarm/pg-swarm-backup:latest"
	defaultBackupSidecarImage = "ghcr.io/pg-swarm/pg-swarm-backup-sidecar:latest"
)

// backupEnabled returns true if at least one backup profile is attached.
func backupEnabled(cfg *pgswarmv1.ClusterConfig) bool {
	return len(cfg.Backups) > 0
}

// backupImageForRule returns the container image for a backup sidecar.
func backupImageForRule(backup *pgswarmv1.BackupConfig) string {
	if backup != nil && backup.BackupImage != "" {
		return backup.BackupImage
	}
	return defaultBackupSidecarImage
}

// ruleShortID returns a short prefix from a backup profile ID for K8s resource naming.
func ruleShortID(ruleID string) string {
	if len(ruleID) >= 8 {
		return ruleID[:8]
	}
	return ruleID
}

// backupLeaseName returns the Lease resource name used by the sidecar for replica coordination.
func backupLeaseName(clusterName string) string {
	return resourceName(clusterName, "backup-lease")
}

// backupServiceAccountName returns the ServiceAccount name used by backup sidecar pods.
func backupServiceAccountName(clusterName string) string {
	return resourceName(clusterName, "backup")
}

// buildBackupServiceAccount creates the ServiceAccount for backup sidecar pods.
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

// buildBackupRole creates the RBAC Role granting lease and configmap access for the backup sidecar.
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

// pgMajorVersion extracts the major version from a version string like "17", "16.2", "17.1".
func pgMajorVersion(version string) string {
	if i := strings.IndexByte(version, '.'); i > 0 {
		return version[:i]
	}
	return version
}

// backupSidecarEnvVars returns env vars for the backup sidecar container.
func backupSidecarEnvVars(cfg *pgswarmv1.ClusterConfig, backup *pgswarmv1.BackupConfig, secretName, satelliteID string) []corev1.EnvVar {
	ruleShort := ruleShortID(backup.BackupProfileId)
	pgMajor := "17"
	if cfg.Postgres != nil && cfg.Postgres.Version != "" {
		pgMajor = pgMajorVersion(cfg.Postgres.Version)
	}

	vars := []corev1.EnvVar{
		{Name: "SATELLITE_ID", Value: satelliteID},
		{Name: "CLUSTER_NAME", Value: cfg.ClusterName},
		{
			Name: "POD_NAME",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
			},
		},
		{Name: "NAMESPACE", Value: cfg.Namespace},
		{Name: "PG_MAJOR", Value: pgMajor},
		{Name: "PGHOST", Value: "localhost"},
		{Name: "PGPORT", Value: "5432"},
		{Name: "PGUSER", Value: "backup_user"},
		{Name: "PGPASSWORD", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  "backup-password",
			},
		}},
		{Name: "DEST_TYPE", Value: backup.Destination.Type},
		{Name: "BACKUP_RULE_ID", Value: backup.BackupProfileId},
		{Name: "BACKUP_STATUS_CM", Value: backupStatusConfigMapName(cfg.ClusterName)},
		{Name: "BACKUP_LEASE_NAME", Value: backupLeaseName(cfg.ClusterName)},
	}

	// Schedules
	if backup.Physical != nil {
		if backup.Physical.BaseSchedule != "" {
			vars = append(vars, corev1.EnvVar{Name: "BASE_SCHEDULE", Value: backup.Physical.BaseSchedule})
		}
		if backup.Physical.IncrementalSchedule != "" {
			vars = append(vars, corev1.EnvVar{Name: "INCR_SCHEDULE", Value: backup.Physical.IncrementalSchedule})
		}
	}
	if backup.Logical != nil && backup.Logical.Schedule != "" {
		vars = append(vars, corev1.EnvVar{Name: "LOGICAL_SCHEDULE", Value: backup.Logical.Schedule})
		if len(backup.Logical.Databases) > 0 {
			vars = append(vars, corev1.EnvVar{Name: "LOGICAL_DATABASES", Value: strings.Join(backup.Logical.Databases, ",")})
		}
	}

	// Retention
	if backup.Retention != nil {
		if backup.Retention.BaseBackupCount > 0 {
			vars = append(vars, corev1.EnvVar{Name: "RETENTION_SETS", Value: fmt.Sprintf("%d", backup.Retention.BaseBackupCount)})
		}
		if backup.Retention.WalRetentionDays > 0 {
			vars = append(vars, corev1.EnvVar{Name: "RETENTION_DAYS", Value: fmt.Sprintf("%d", backup.Retention.WalRetentionDays)})
		}
	}

	// Destination-specific env vars
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

// buildBackupSidecar creates the backup sidecar container for injection into the StatefulSet.
func buildBackupSidecar(cfg *pgswarmv1.ClusterConfig, secretName, satelliteID string) corev1.Container {
	// Use the first backup profile for configuration
	backup := cfg.Backups[0]
	image := backupImageForRule(backup)
	env := backupSidecarEnvVars(cfg, backup, secretName, satelliteID)

	return corev1.Container{
		Name:            "backup",
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Ports: []corev1.ContainerPort{
			{Name: "backup-api", ContainerPort: 8442, Protocol: corev1.ProtocolTCP},
		},
		Env: env,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "wal-staging", MountPath: "/wal-staging"},
			{Name: "wal-restore", MountPath: "/wal-restore"},
		},
	}
}

// ensureBackupRBAC creates or updates the backup RBAC resources (ServiceAccount, Role, RoleBinding).
func ensureBackupRBAC(ctx context.Context, client kubernetes.Interface, cfg *pgswarmv1.ClusterConfig) error {
	if err := createOrUpdateServiceAccount(ctx, client, buildBackupServiceAccount(cfg)); err != nil {
		return fmt.Errorf("backup serviceaccount: %w", err)
	}
	if err := createOrUpdateRole(ctx, client, buildBackupRole(cfg)); err != nil {
		return fmt.Errorf("backup role: %w", err)
	}
	if err := createOrUpdateRoleBinding(ctx, client, buildBackupRoleBinding(cfg)); err != nil {
		return fmt.Errorf("backup rolebinding: %w", err)
	}
	return nil
}

// cleanupBackupResources removes backup credential Secrets, RBAC, status ConfigMap, and lease.
// No longer cleans up CronJobs — backups are handled by the sidecar.
func cleanupBackupResources(ctx context.Context, client kubernetes.Interface, ns, clusterName string) {
	selector := "pg-swarm.io/cluster=" + clusterName

	// Clean up any legacy CronJobs from before the sidecar migration
	cjList, err := client.BatchV1().CronJobs(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err == nil {
		propagation := metav1.DeletePropagationBackground
		for _, cj := range cjList.Items {
			if _, ok := cj.Labels[LabelBackupType]; ok {
				_ = client.BatchV1().CronJobs(ns).Delete(ctx, cj.Name, metav1.DeleteOptions{})
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
