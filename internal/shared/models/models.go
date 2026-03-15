package models

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

type SatelliteState string

const (
	SatelliteStatePending      SatelliteState = "pending"
	SatelliteStateApproved     SatelliteState = "approved"
	SatelliteStateConnected    SatelliteState = "connected"
	SatelliteStateDisconnected SatelliteState = "disconnected"
)

type ClusterState string

const (
	ClusterStateCreating ClusterState = "creating"
	ClusterStateRunning  ClusterState = "running"
	ClusterStateDegraded ClusterState = "degraded"
	ClusterStateFailed   ClusterState = "failed"
	ClusterStatePaused   ClusterState = "paused"
	ClusterStateDeleting ClusterState = "deleting"
)

type StorageClassInfo struct {
	Name              string `json:"name"`
	Provisioner       string `json:"provisioner"`
	ReclaimPolicy     string `json:"reclaim_policy"`
	VolumeBindingMode string `json:"volume_binding_mode"`
	IsDefault         bool   `json:"is_default"`
}

type Satellite struct {
	ID             uuid.UUID          `json:"id" db:"id"`
	Hostname       string             `json:"hostname" db:"hostname"`
	K8sClusterName string             `json:"k8s_cluster_name" db:"k8s_cluster_name"`
	Region         string             `json:"region" db:"region"`
	Labels         map[string]string  `json:"labels" db:"labels"`
	StorageClasses []StorageClassInfo `json:"storage_classes" db:"storage_classes"`
	State          SatelliteState     `json:"state" db:"state"`
	AuthTokenHash  string             `json:"-" db:"auth_token_hash"`
	TempTokenHash  string             `json:"-" db:"temp_token_hash"`
	LastHeartbeat  *time.Time         `json:"last_heartbeat,omitempty" db:"last_heartbeat"`
	CreatedAt      time.Time          `json:"created_at" db:"created_at"`
	UpdatedAt      time.Time          `json:"updated_at" db:"updated_at"`
}

type ClusterConfig struct {
	ID                uuid.UUID       `json:"id" db:"id"`
	Name              string          `json:"name" db:"name"`
	Namespace         string          `json:"namespace" db:"namespace"`
	SatelliteID       *uuid.UUID      `json:"satellite_id,omitempty" db:"satellite_id"`
	ProfileID         *uuid.UUID      `json:"profile_id,omitempty" db:"profile_id"`
	DeploymentRuleID  *uuid.UUID      `json:"deployment_rule_id,omitempty" db:"deployment_rule_id"`
	Config            json.RawMessage `json:"config" db:"config"`
	ConfigVersion     int64           `json:"config_version" db:"config_version"`
	State             ClusterState    `json:"state" db:"state"`
	Paused            bool            `json:"paused" db:"paused"`
	CreatedAt         time.Time       `json:"created_at" db:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at" db:"updated_at"`
}

// DeploymentRule maps a profile to satellites matching a label selector.
// Fan-out: one ClusterConfig is created per satellite whose labels contain the selector.
type DeploymentRule struct {
	ID            uuid.UUID         `json:"id" db:"id"`
	Name          string            `json:"name" db:"name"`
	ProfileID     uuid.UUID         `json:"profile_id" db:"profile_id"`
	LabelSelector map[string]string `json:"label_selector" db:"label_selector"`
	Namespace     string            `json:"namespace" db:"namespace"`
	ClusterName   string            `json:"cluster_name" db:"cluster_name"`
	CreatedAt     time.Time         `json:"created_at" db:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at" db:"updated_at"`
}

// ClusterSpec represents the desired PostgreSQL cluster specification.
// Stored as JSON in ClusterConfig.Config and parsed via ParseSpec().
type ClusterSpec struct {
	Replicas  int32             `json:"replicas"`
	Postgres  PostgresSpec      `json:"postgres"`
	Storage    StorageSpec       `json:"storage"`
	WalStorage *StorageSpec     `json:"wal_storage,omitempty"` // nil = WAL on data volume
	Resources  ResourceSpec     `json:"resources"`
	PgParams  map[string]string `json:"pg_params,omitempty"`
	HbaRules  []string          `json:"hba_rules,omitempty"`
	Archive   *ArchiveSpec      `json:"archive,omitempty"`   // nil = archiving disabled
	Databases []DatabaseSpec    `json:"databases,omitempty"` // databases to create with owner users
	Failover           *FailoverSpec     `json:"failover,omitempty"`  // nil = failover disabled
	DeletionProtection bool               `json:"deletion_protection,omitempty"` // adds finalizer to PVCs
}

type PostgresSpec struct {
	Version  string `json:"version"`
	Variant  string `json:"variant,omitempty"`  // "alpine" or "debian"
	Registry string `json:"registry,omitempty"` // optional registry prefix
	Image    string `json:"image"`              // resolved at deploy time
}

type StorageSpec struct {
	Size         string `json:"size"`
	StorageClass string `json:"storage_class,omitempty"`
}

type ResourceSpec struct {
	CPURequest    string `json:"cpu_request"`
	CPULimit      string `json:"cpu_limit"`
	MemoryRequest string `json:"memory_request"`
	MemoryLimit   string `json:"memory_limit"`
}

type ArchiveSpec struct {
	Mode                  string              `json:"mode"`
	ArchiveCommand        string              `json:"archive_command,omitempty"`
	RestoreCommand        string              `json:"restore_command,omitempty"`
	ArchiveTimeoutSeconds int32               `json:"archive_timeout_seconds,omitempty"`
	ArchiveStorage        *ArchiveStorageSpec `json:"archive_storage,omitempty"`
	CredentialsSecret     *SecretRef          `json:"credentials_secret,omitempty"`
}

type ArchiveStorageSpec struct {
	Size         string `json:"size"`
	StorageClass string `json:"storage_class,omitempty"`
}

type SecretRef struct {
	Name string `json:"name"`
}

type FailoverSpec struct {
	Enabled                    bool   `json:"enabled"`
	HealthCheckIntervalSeconds int32  `json:"health_check_interval_seconds,omitempty"`
	SidecarImage               string `json:"sidecar_image,omitempty"`
}

type DatabaseSpec struct {
	Name     string `json:"name"`
	User     string `json:"user"`
	Password string `json:"password"`
}

// ValidateArchiveSpec validates the archive configuration.
// nil is valid (archiving disabled).
func ValidateArchiveSpec(a *ArchiveSpec) error {
	if a == nil {
		return nil
	}
	switch a.Mode {
	case "pvc":
		if a.ArchiveStorage == nil || a.ArchiveStorage.Size == "" {
			return fmt.Errorf("archive mode \"pvc\" requires archive_storage.size")
		}
	case "custom":
		if a.ArchiveCommand == "" {
			return fmt.Errorf("archive mode \"custom\" requires archive_command")
		}
	case "":
		return nil // disabled
	default:
		return fmt.Errorf("unknown archive mode %q (must be \"pvc\" or \"custom\")", a.Mode)
	}
	if a.ArchiveTimeoutSeconds < 0 {
		return fmt.Errorf("archive_timeout_seconds must be >= 0")
	}
	return nil
}

// ValidateDatabases validates the databases configuration.
func ValidateDatabases(dbs []DatabaseSpec) error {
	seen := make(map[string]bool, len(dbs))
	for i, db := range dbs {
		if db.Name == "" {
			return fmt.Errorf("databases[%d]: name is required", i)
		}
		if db.User == "" {
			return fmt.Errorf("databases[%d] (%s): user is required", i, db.Name)
		}
		if db.Password == "" {
			return fmt.Errorf("databases[%d] (%s): password is required", i, db.Name)
		}
		if seen[db.Name] {
			return fmt.Errorf("databases[%d]: duplicate database name %q", i, db.Name)
		}
		seen[db.Name] = true
	}
	return nil
}

// ParseSpec deserializes the Config JSON into a ClusterSpec.
func (c *ClusterConfig) ParseSpec() (*ClusterSpec, error) {
	var spec ClusterSpec
	if err := json.Unmarshal(c.Config, &spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

type ClusterProfile struct {
	ID          uuid.UUID       `json:"id" db:"id"`
	Name        string          `json:"name" db:"name"`
	Description string          `json:"description" db:"description"`
	Config      json.RawMessage `json:"config" db:"config"`
	Locked      bool            `json:"locked" db:"locked"`
	CreatedAt   time.Time       `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at" db:"updated_at"`
}

// ParseSpec deserializes the Config JSON into a ClusterSpec.
func (p *ClusterProfile) ParseSpec() (*ClusterSpec, error) {
	var spec ClusterSpec
	if err := json.Unmarshal(p.Config, &spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

// PostgresVersion maps a major version + variant to a Docker image tag.
type PostgresVersion struct {
	ID        uuid.UUID `json:"id" db:"id"`
	Version   string    `json:"version" db:"version"`
	Variant   string    `json:"variant" db:"variant"`
	ImageTag  string    `json:"image_tag" db:"image_tag"`
	IsDefault bool      `json:"is_default" db:"is_default"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
	UpdatedAt time.Time `json:"updated_at" db:"updated_at"`
}

// PostgresVariant is a supported base-image variant (e.g. "alpine", "debian").
type PostgresVariant struct {
	ID          uuid.UUID `json:"id" db:"id"`
	Name        string    `json:"name" db:"name"`
	Description string    `json:"description" db:"description"`
	CreatedAt   time.Time `json:"created_at" db:"created_at"`
}

type ClusterHealth struct {
	SatelliteID uuid.UUID       `json:"satellite_id" db:"satellite_id"`
	ClusterName string          `json:"cluster_name" db:"cluster_name"`
	State       ClusterState    `json:"state" db:"state"`
	Instances   json.RawMessage `json:"instances" db:"instances"`
	UpdatedAt   time.Time       `json:"updated_at" db:"updated_at"`
}

type Event struct {
	ID          uuid.UUID `json:"id" db:"id"`
	SatelliteID uuid.UUID `json:"satellite_id" db:"satellite_id"`
	ClusterName string    `json:"cluster_name" db:"cluster_name"`
	Severity    string    `json:"severity" db:"severity"`
	Message     string    `json:"message" db:"message"`
	Source      string    `json:"source" db:"source"`
	CreatedAt   time.Time `json:"created_at" db:"created_at"`
}

// ---------- Backup ----------

type BackupProfile struct {
	ID          uuid.UUID       `json:"id" db:"id"`
	Name        string          `json:"name" db:"name"`
	Description string          `json:"description" db:"description"`
	Config      json.RawMessage `json:"config" db:"config"` // BackupProfileSpec JSON
	CreatedAt   time.Time       `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at" db:"updated_at"`
}

// ParseBackupProfileSpec deserializes the Config JSON into a BackupProfileSpec.
func (r *BackupProfile) ParseBackupProfileSpec() (*BackupProfileSpec, error) {
	var spec BackupProfileSpec
	if err := json.Unmarshal(r.Config, &spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

type BackupProfileSpec struct {
	Physical    *PhysicalBackupSpec `json:"physical,omitempty"`
	Logical     *LogicalBackupSpec  `json:"logical,omitempty"`
	Destination DestinationSpec     `json:"destination"`
	Retention   RetentionSpec       `json:"retention"`
	BackupImage string              `json:"backup_image,omitempty"` // default: ghcr.io/pg-swarm/pg-swarm-backup:latest
}

type PhysicalBackupSpec struct {
	BaseSchedule          string `json:"base_schedule"`                        // cron for full backups: "0 2 * * 0"
	IncrementalSchedule   string `json:"incremental_schedule,omitempty"`       // cron for incrementals: "0 2 * * 1-6" (PG 17+)
	WalArchiveEnabled     bool   `json:"wal_archive_enabled"`                  // enable continuous WAL archiving
	ArchiveTimeoutSecs    int32  `json:"archive_timeout_seconds,omitempty"`    // default 60
}

type LogicalBackupSpec struct {
	Schedule  string   `json:"schedule"`            // cron: "0 2 * * *"
	Databases []string `json:"databases,omitempty"` // empty = pg_dumpall
	Format    string   `json:"format,omitempty"`    // "custom" (default), "plain", "directory"
}

type DestinationSpec struct {
	Type  string       `json:"type"` // "gcs", "s3", "sftp", "local"
	GCS   *GCSConfig   `json:"gcs,omitempty"`
	S3    *S3Config    `json:"s3,omitempty"`
	SFTP  *SFTPConfig  `json:"sftp,omitempty"`
	Local *LocalConfig `json:"local,omitempty"`
}

type GCSConfig struct {
	Bucket             string `json:"bucket"`
	PathPrefix         string `json:"path_prefix,omitempty"`
	ServiceAccountJSON string `json:"service_account_json,omitempty"`
}

type S3Config struct {
	Bucket         string `json:"bucket"`
	Region         string `json:"region"`
	Endpoint       string `json:"endpoint,omitempty"`      // for S3-compatible (MinIO)
	PathPrefix     string `json:"path_prefix,omitempty"`
	AccessKeyID    string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
	ForcePathStyle bool   `json:"force_path_style,omitempty"`
}

type SFTPConfig struct {
	Host       string `json:"host"`
	Port       int    `json:"port,omitempty"` // default 22
	User       string `json:"user"`
	Password   string `json:"password,omitempty"`
	PrivateKey string `json:"private_key,omitempty"`
	BasePath   string `json:"base_path"`
}

type LocalConfig struct {
	Size         string `json:"size"`
	StorageClass string `json:"storage_class,omitempty"`
}

type RetentionSpec struct {
	BaseBackupCount        int `json:"base_backup_count,omitempty"`        // default 7
	IncrementalBackupCount int `json:"incremental_backup_count,omitempty"` // default 6 (per full cycle)
	WalRetentionDays       int `json:"wal_retention_days,omitempty"`       // default 14
	LogicalBackupCount     int `json:"logical_backup_count,omitempty"`     // default 30
}

// ValidateBackupProfileSpec validates the backup profile configuration.
func ValidateBackupProfileSpec(spec *BackupProfileSpec) error {
	if spec.Physical == nil && spec.Logical == nil {
		return fmt.Errorf("backup profile must define either physical or logical backup")
	}
	if spec.Physical != nil && spec.Logical != nil {
		return fmt.Errorf("backup profile must define either physical or logical backup, not both")
	}
	if spec.Physical != nil {
		if spec.Physical.BaseSchedule == "" {
			return fmt.Errorf("physical backup requires base_schedule")
		}
		// Validate WAL retention covers the base backup span
		if spec.Physical.WalArchiveEnabled {
			walDays := spec.Retention.WalRetentionDays
			if walDays <= 0 {
				walDays = 14
			}
			baseCount := spec.Retention.BaseBackupCount
			if baseCount <= 0 {
				baseCount = 7
			}
			if minDays := estimateCronIntervalDays(spec.Physical.BaseSchedule) * baseCount; minDays > 0 && walDays < minDays {
				return fmt.Errorf("wal_retention_days (%d) is too short to cover %d base backups at schedule %q (need at least %d days for PITR)",
					walDays, baseCount, spec.Physical.BaseSchedule, minDays)
			}
		}
	}
	if spec.Logical != nil {
		if spec.Logical.Schedule == "" {
			return fmt.Errorf("logical backup requires schedule")
		}
		if spec.Logical.Format != "" {
			switch spec.Logical.Format {
			case "custom", "plain", "directory":
			default:
				return fmt.Errorf("logical backup format must be \"custom\", \"plain\", or \"directory\"")
			}
		}
	}
	switch spec.Destination.Type {
	case "s3":
		if spec.Destination.S3 == nil || spec.Destination.S3.Bucket == "" {
			return fmt.Errorf("s3 destination requires bucket")
		}
	case "gcs":
		if spec.Destination.GCS == nil || spec.Destination.GCS.Bucket == "" {
			return fmt.Errorf("gcs destination requires bucket")
		}
	case "sftp":
		if spec.Destination.SFTP == nil || spec.Destination.SFTP.Host == "" || spec.Destination.SFTP.BasePath == "" {
			return fmt.Errorf("sftp destination requires host and base_path")
		}
	case "local":
		if spec.Destination.Local == nil || spec.Destination.Local.Size == "" {
			return fmt.Errorf("local destination requires size")
		}
	default:
		return fmt.Errorf("destination type must be \"s3\", \"gcs\", \"sftp\", or \"local\"")
	}
	return nil
}

// estimateCronIntervalDays returns the approximate interval in days for a cron schedule.
// Returns 0 if the schedule can't be parsed.
func estimateCronIntervalDays(cron string) int {
	parts := strings.Fields(cron)
	if len(parts) < 5 {
		return 0
	}
	dayOfMonth, month, dayOfWeek := parts[2], parts[3], parts[4]

	// Weekly: "* * 0" or "* * 1,4"
	if dayOfMonth == "*" && month == "*" && dayOfWeek != "*" {
		days := strings.Count(dayOfWeek, ",") + 1
		return 7 / days
	}
	// Daily: "* * *"
	if dayOfMonth == "*" && month == "*" && dayOfWeek == "*" {
		return 1
	}
	// Monthly: "1 * *"
	if dayOfMonth != "*" && month == "*" && dayOfWeek == "*" {
		dates := strings.Count(dayOfMonth, ",") + 1
		return 30 / dates
	}
	// Every N days: "*/N * *"
	if strings.HasPrefix(dayOfMonth, "*/") {
		n := 0
		fmt.Sscanf(dayOfMonth[2:], "%d", &n)
		if n > 0 {
			return n
		}
	}
	return 0
}

type BackupInventory struct {
	ID           uuid.UUID  `json:"id" db:"id"`
	SatelliteID  uuid.UUID  `json:"satellite_id" db:"satellite_id"`
	ClusterName  string     `json:"cluster_name" db:"cluster_name"`
	BackupProfileID uuid.UUID  `json:"backup_profile_id" db:"backup_profile_id"`
	BackupType   string     `json:"backup_type" db:"backup_type"`   // "base", "wal", "logical"
	Status       string     `json:"status" db:"status"`             // "running", "completed", "failed"
	StartedAt    time.Time  `json:"started_at" db:"started_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty" db:"completed_at"`
	SizeBytes    int64      `json:"size_bytes" db:"size_bytes"`
	BackupPath   string     `json:"backup_path" db:"backup_path"`
	PgVersion    string     `json:"pg_version" db:"pg_version"`
	WalStartLSN  string     `json:"wal_start_lsn,omitempty" db:"wal_start_lsn"`
	WalEndLSN    string     `json:"wal_end_lsn,omitempty" db:"wal_end_lsn"`
	ErrorMessage string     `json:"error_message,omitempty" db:"error_message"`
	CreatedAt    time.Time  `json:"created_at" db:"created_at"`
}

type RestoreOperation struct {
	ID             uuid.UUID  `json:"id" db:"id"`
	SatelliteID    uuid.UUID  `json:"satellite_id" db:"satellite_id"`
	ClusterName    string     `json:"cluster_name" db:"cluster_name"`
	BackupID       uuid.UUID  `json:"backup_id" db:"backup_id"`
	RestoreType    string     `json:"restore_type" db:"restore_type"` // "pitr", "logical"
	TargetTime     *time.Time `json:"target_time,omitempty" db:"target_time"`
	TargetDatabase string     `json:"target_database,omitempty" db:"target_database"`
	Status         string     `json:"status" db:"status"` // "pending", "running", "completed", "failed"
	ErrorMessage   string     `json:"error_message,omitempty" db:"error_message"`
	StartedAt      *time.Time `json:"started_at,omitempty" db:"started_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty" db:"completed_at"`
	CreatedAt      time.Time  `json:"created_at" db:"created_at"`
}
