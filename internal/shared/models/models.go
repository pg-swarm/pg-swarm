package models

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type SatelliteState string

const (
	SatelliteStatePending      SatelliteState = "pending"
	SatelliteStateApproved     SatelliteState = "approved"
	SatelliteStateConnected    SatelliteState = "connected"
	SatelliteStateDisconnected SatelliteState = "disconnected"
	SatelliteStateReplaced     SatelliteState = "replaced"
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
	Name           string             `json:"name" db:"name"`
	Hostname       string             `json:"hostname" db:"hostname"`
	K8sClusterName string             `json:"k8s_cluster_name" db:"k8s_cluster_name"`
	Region         string             `json:"region" db:"region"`
	Labels         map[string]string  `json:"labels" db:"labels"`
	StorageClasses []StorageClassInfo `json:"storage_classes" db:"storage_classes"`
	TierMappings   map[string]string  `json:"tier_mappings" db:"tier_mappings"`
	State          SatelliteState     `json:"state" db:"state"`
	AuthTokenHash  string             `json:"-" db:"auth_token_hash"`
	TempTokenHash  string             `json:"-" db:"temp_token_hash"`
	LastHeartbeat  *time.Time         `json:"last_heartbeat,omitempty" db:"last_heartbeat"`
	CreatedAt      time.Time          `json:"created_at" db:"created_at"`
	UpdatedAt      time.Time          `json:"updated_at" db:"updated_at"`
}

type ClusterConfig struct {
	ID                    uuid.UUID       `json:"id" db:"id"`
	Name                  string          `json:"name" db:"name"`
	Namespace             string          `json:"namespace" db:"namespace"`
	SatelliteID           *uuid.UUID      `json:"satellite_id,omitempty" db:"satellite_id"`
	ProfileID             *uuid.UUID      `json:"profile_id,omitempty" db:"profile_id"`
	DeploymentRuleID      *uuid.UUID      `json:"deployment_rule_id,omitempty" db:"deployment_rule_id"`
	Config                json.RawMessage `json:"config" db:"config"`
	ConfigVersion         int64           `json:"config_version" db:"config_version"`
	AppliedProfileVersion int             `json:"applied_profile_version" db:"applied_profile_version"`
	State                 ClusterState    `json:"state" db:"state"`
	Paused                bool            `json:"paused" db:"paused"`
	CreatedAt             time.Time       `json:"created_at" db:"created_at"`
	UpdatedAt             time.Time       `json:"updated_at" db:"updated_at"`
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
	Replicas           int32             `json:"replicas"`
	Postgres           PostgresSpec      `json:"postgres"`
	Storage            StorageSpec       `json:"storage"`
	WalStorage         *StorageSpec      `json:"wal_storage,omitempty"` // nil = WAL on data volume
	Resources          ResourceSpec      `json:"resources"`
	PgParams           map[string]string `json:"pg_params,omitempty"`
	HbaRules           []string          `json:"hba_rules,omitempty"`
	Archive            *ArchiveSpec      `json:"archive,omitempty"`             // nil = archiving disabled
	Failover           *FailoverSpec     `json:"failover,omitempty"`            // nil = failover disabled
	Backup             *BackupSpec       `json:"backup,omitempty"`              // nil = backups disabled
	DeletionProtection bool              `json:"deletion_protection,omitempty"` // adds finalizer to PVCs
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

// ValidateArchiveSpec validates the archive configuration.
// nil is valid (archiving disabled).
func ValidateArchiveSpec(a *ArchiveSpec) error {
	if a == nil {
		return nil
	}
	switch a.Mode {
	case "custom":
		if a.ArchiveCommand == "" {
			return fmt.Errorf("archive mode \"custom\" requires archive_command")
		}
	case "":
		return nil // disabled
	default:
		return fmt.Errorf("unknown archive mode %q (must be \"custom\")", a.Mode)
	}
	if a.ArchiveTimeoutSeconds < 0 {
		return fmt.Errorf("archive_timeout_seconds must be >= 0")
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
	ID                uuid.UUID       `json:"id" db:"id"`
	Name              string          `json:"name" db:"name"`
	Description       string          `json:"description" db:"description"`
	Config            json.RawMessage `json:"config" db:"config"`
	InUse             bool            `json:"locked" db:"in_use"` // computed: true when referenced by clusters or rules
	RecoveryRuleSetID *uuid.UUID      `json:"recovery_rule_set_id" db:"recovery_rule_set_id"`
	CreatedAt         time.Time       `json:"created_at" db:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at" db:"updated_at"`
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

// StorageTier is an admin-defined abstract storage tier (e.g. "fast", "replicated").
// Satellites map their concrete storage classes to these tiers.
type StorageTier struct {
	ID          uuid.UUID `json:"id" db:"id"`
	Name        string    `json:"name" db:"name"`
	Description string    `json:"description" db:"description"`
	CreatedAt   time.Time `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time `json:"updated_at" db:"updated_at"`
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

// ---------- Recovery Rule Sets ----------

// RecoveryRuleSet is a named collection of log-based recovery rules.
// Rules are stored as a JSON array in the Config column.
type RecoveryRuleSet struct {
	ID          uuid.UUID       `json:"id" db:"id"`
	Name        string          `json:"name" db:"name"`
	Description string          `json:"description" db:"description"`
	Builtin     bool            `json:"builtin" db:"builtin"`
	Config      json.RawMessage `json:"config" db:"config"` // JSON array of RecoveryRule
	CreatedAt   time.Time       `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at" db:"updated_at"`
}

// ---------- Cluster Databases ----------

// ClusterDatabase represents a database + user dynamically managed at the cluster level.
type ClusterDatabase struct {
	ID           uuid.UUID `json:"id" db:"id"`
	ClusterID    uuid.UUID `json:"cluster_id" db:"cluster_id"`
	DBName       string    `json:"db_name" db:"db_name"`
	DBUser       string    `json:"db_user" db:"db_user"`
	Password     []byte    `json:"-" db:"password"`                        // encrypted, never serialized to API
	AllowedCIDRs []string  `json:"allowed_cidrs" db:"allowed_cidrs"`       // e.g. ["10.0.0.0/8"]
	Status       string    `json:"status" db:"status"`                     // pending, created, failed
	ErrorMessage string    `json:"error_message,omitempty" db:"error_message"`
	CreatedAt    time.Time `json:"created_at" db:"created_at"`
	UpdatedAt    time.Time `json:"updated_at" db:"updated_at"`
}

// ---------- Config Versions ----------

// ConfigVersion stores a complete configuration snapshot for a profile.
// Each update creates a new version; versions are append-only.
type ConfigVersion struct {
	ID            uuid.UUID       `json:"id" db:"id"`
	ProfileID     uuid.UUID       `json:"profile_id" db:"profile_id"`
	Version       int             `json:"version" db:"version"`
	Config        json.RawMessage `json:"config" db:"config"`
	ChangeSummary string          `json:"change_summary" db:"change_summary"`
	ApplyStatus   string          `json:"apply_status" db:"apply_status"`
	CreatedBy     string          `json:"created_by" db:"created_by"`
	CreatedAt     time.Time       `json:"created_at" db:"created_at"`
}

// ---------- PG Parameter Classifications ----------

// PgParamClassification defines the restart behavior for a PostgreSQL parameter.
// Parameters not in this table default to sequential restart.
type PgParamClassification struct {
	Name        string    `json:"name" db:"name"`
	RestartMode string    `json:"restart_mode" db:"restart_mode"` // "sequential" or "full_restart"
	Description string    `json:"description" db:"description"`
	PgContext   string    `json:"pg_context" db:"pg_context"` // "postmaster", "sighup", etc.
	CreatedAt   time.Time `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time `json:"updated_at" db:"updated_at"`
}

// ---------- Backup ----------

// BackupSpec is the per-cluster backup configuration stored in a profile's config JSON.
type BackupSpec struct {
	StoreID   *uuid.UUID            `json:"store_id,omitempty"`
	Physical  *PhysicalBackupConfig `json:"physical,omitempty"`
	Logical   *LogicalBackupConfig  `json:"logical,omitempty"`
	Retention *BackupRetention      `json:"retention,omitempty"`
}

// PhysicalBackupConfig defines base/incremental backup schedules and WAL archiving.
type PhysicalBackupConfig struct {
	Enabled               bool   `json:"enabled"`
	BaseSchedule          string `json:"base_schedule"`          // 5-field cron
	IncrementalSchedule   string `json:"incremental_schedule"`   // 5-field cron (optional)
	WalArchiveEnabled     bool   `json:"wal_archive_enabled"`
	ArchiveTimeoutSeconds int32  `json:"archive_timeout_seconds"` // default 60
}

// LogicalBackupConfig defines pg_dump backup schedules.
type LogicalBackupConfig struct {
	Enabled   bool     `json:"enabled"`
	Schedule  string   `json:"schedule"`  // 5-field cron
	Databases []string `json:"databases"` // empty = all databases
	Format    string   `json:"format"`    // "custom", "plain", "directory"
}

// BackupRetention defines how many backups and how long WAL segments are kept.
type BackupRetention struct {
	BaseBackupCount        int `json:"base_backup_count"`
	IncrementalBackupCount int `json:"incremental_backup_count"`
	WalRetentionDays       int `json:"wal_retention_days"`
	LogicalBackupCount     int `json:"logical_backup_count"`
}

// BackupStore represents an admin-managed backup storage destination.
type BackupStore struct {
	ID             uuid.UUID       `json:"id" db:"id"`
	Name           string          `json:"name" db:"name"`
	Description    string          `json:"description" db:"description"`
	StoreType      string          `json:"store_type" db:"store_type"` // "gcs" or "sftp"
	Config         json.RawMessage `json:"config" db:"config"`
	Credentials    []byte          `json:"-" db:"credentials"`                              // encrypted, never serialized
	CredentialsSet map[string]bool `json:"credentials_set,omitempty" db:"-"`                // computed field
	CreatedAt      time.Time       `json:"created_at" db:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at" db:"updated_at"`
}

// GCSStoreConfig holds non-secret GCS configuration.
type GCSStoreConfig struct {
	Bucket     string `json:"bucket"`
	PathPrefix string `json:"path_prefix"`
}

// SFTPStoreConfig holds non-secret SFTP configuration.
type SFTPStoreConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	BasePath string `json:"base_path"`
}

// GCSCredentials holds the secret fields for GCS access.
type GCSCredentials struct {
	ServiceAccountJSON string `json:"service_account_json"`
}

// SFTPCredentials holds the secret fields for SFTP access.
type SFTPCredentials struct {
	Password   string `json:"password,omitempty"`
	PrivateKey string `json:"private_key,omitempty"`
}

// ValidateBackupStore validates a backup store for creation/update.
func ValidateBackupStore(s *BackupStore) error {
	if s.Name == "" {
		return fmt.Errorf("name is required")
	}
	if s.StoreType != "gcs" && s.StoreType != "sftp" {
		return fmt.Errorf("store_type must be 'gcs' or 'sftp'")
	}
	return nil
}

// ValidateBackupSpec validates the backup section of a profile config.
func ValidateBackupSpec(b *BackupSpec) error {
	if b == nil {
		return nil
	}
	if b.Physical != nil && b.Physical.Enabled {
		if b.StoreID == nil {
			return fmt.Errorf("backup store_id is required when physical backups are enabled")
		}
		if b.Physical.BaseSchedule == "" {
			return fmt.Errorf("base_schedule is required for physical backups")
		}
	}
	if b.Logical != nil && b.Logical.Enabled {
		if b.StoreID == nil {
			return fmt.Errorf("backup store_id is required when logical backups are enabled")
		}
		if b.Logical.Schedule == "" {
			return fmt.Errorf("schedule is required for logical backups")
		}
		if b.Logical.Format == "" {
			b.Logical.Format = "custom"
		}
		if b.Logical.Format != "custom" && b.Logical.Format != "plain" && b.Logical.Format != "directory" {
			return fmt.Errorf("logical backup format must be 'custom', 'plain', or 'directory'")
		}
	}
	if b.Retention != nil {
		if b.Retention.BaseBackupCount < 0 {
			return fmt.Errorf("base_backup_count must be >= 0")
		}
		if b.Retention.WalRetentionDays < 0 {
			return fmt.Errorf("wal_retention_days must be >= 0")
		}
	}
	return nil
}

// BackupInventory represents a single backup record.
type BackupInventory struct {
	ID           uuid.UUID  `json:"id" db:"id"`
	SatelliteID  uuid.UUID  `json:"satellite_id" db:"satellite_id"`
	ClusterName  string     `json:"cluster_name" db:"cluster_name"`
	BackupType   string     `json:"backup_type" db:"backup_type"` // "base", "incremental", "logical"
	Status       string     `json:"status" db:"status"`           // "pending", "running", "completed", "failed", "skipped"
	StartedAt    time.Time  `json:"started_at" db:"started_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty" db:"completed_at"`
	SizeBytes    int64      `json:"size_bytes" db:"size_bytes"`
	BackupPath   string     `json:"backup_path" db:"backup_path"`
	PgVersion    string     `json:"pg_version" db:"pg_version"`
	WalStartLSN  string     `json:"wal_start_lsn" db:"wal_start_lsn"`
	WalEndLSN    string     `json:"wal_end_lsn" db:"wal_end_lsn"`
	Databases    []string   `json:"databases" db:"databases"`
	ErrorMessage string     `json:"error_message" db:"error_message"`
	CreatedAt    time.Time  `json:"created_at" db:"created_at"`
}

// RestoreOperation represents a restore request and its progress.
type RestoreOperation struct {
	ID             uuid.UUID  `json:"id" db:"id"`
	SatelliteID    uuid.UUID  `json:"satellite_id" db:"satellite_id"`
	ClusterName    string     `json:"cluster_name" db:"cluster_name"`
	BackupID       *uuid.UUID `json:"backup_id,omitempty" db:"backup_id"`
	RestoreType    string     `json:"restore_type" db:"restore_type"` // "logical", "pitr"
	RestoreMode    string     `json:"restore_mode" db:"restore_mode"` // "in_place", "new_cluster"
	TargetTime     *time.Time `json:"target_time,omitempty" db:"target_time"`
	TargetDatabase string     `json:"target_database" db:"target_database"`
	Status         string     `json:"status" db:"status"` // "pending", "running", "completed", "failed"
	ErrorMessage   string     `json:"error_message" db:"error_message"`
	StartedAt      *time.Time `json:"started_at,omitempty" db:"started_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty" db:"completed_at"`
	CreatedAt      time.Time  `json:"created_at" db:"created_at"`
}
