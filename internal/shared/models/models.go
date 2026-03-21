package models

import (
	"encoding/json"
	"fmt"
	"regexp"
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
	ID               uuid.UUID       `json:"id" db:"id"`
	Name             string          `json:"name" db:"name"`
	Namespace        string          `json:"namespace" db:"namespace"`
	SatelliteID      *uuid.UUID      `json:"satellite_id,omitempty" db:"satellite_id"`
	ProfileID        *uuid.UUID      `json:"profile_id,omitempty" db:"profile_id"`
	DeploymentRuleID *uuid.UUID      `json:"deployment_rule_id,omitempty" db:"deployment_rule_id"`
	Config           json.RawMessage `json:"config" db:"config"`
	ConfigVersion    int64           `json:"config_version" db:"config_version"`
	State            ClusterState    `json:"state" db:"state"`
	Paused           bool            `json:"paused" db:"paused"`
	CreatedAt        time.Time       `json:"created_at" db:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at" db:"updated_at"`
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
	Databases          []DatabaseSpec    `json:"databases,omitempty"`           // databases to create with owner users
	Failover           *FailoverSpec     `json:"failover,omitempty"`            // nil = failover disabled
	DeletionProtection bool              `json:"deletion_protection,omitempty"` // adds finalizer to PVCs
	Backup             *BackupSpec       `json:"backup,omitempty"`              // nil = no backup configured
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

type BackupSpec struct {
	StoreID  *uuid.UUID            `json:"store_id,omitempty"` // references a BackupStore
	Physical *PhysicalBackupConfig `json:"physical,omitempty"`
	Logical  *LogicalBackupConfig  `json:"logical,omitempty"`
}

type PhysicalBackupConfig struct {
	BaseSchedule        string            `json:"base_schedule"`
	IncrementalSchedule string            `json:"incremental_schedule,omitempty"`
	WalArchiveEnabled   bool              `json:"wal_archive_enabled"`
	ArchiveTimeoutSecs  int32             `json:"archive_timeout_seconds,omitempty"`
	Retention           PhysicalRetention `json:"retention"`
}

type PhysicalRetention struct {
	BaseBackupCount        int `json:"base_backup_count,omitempty"`
	IncrementalBackupCount int `json:"incremental_backup_count,omitempty"`
	WalRetentionDays       int `json:"wal_retention_days,omitempty"`
}

type LogicalBackupConfig struct {
	Schedule  string           `json:"schedule"`
	Databases []string         `json:"databases,omitempty"`
	Format    string           `json:"format,omitempty"`
	Retention LogicalRetention `json:"retention"`
}

type LogicalRetention struct {
	BackupCount int `json:"backup_count,omitempty"`
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

// cronRe is a basic structural check for 5-field cron expressions.
var cronRe = regexp.MustCompile(`^(\S+\s+){4}\S+$`)

// ValidateBackupSpec validates the backup configuration.
// nil is valid (no backup configured).
func ValidateBackupSpec(b *BackupSpec) error {
	if b == nil {
		return nil
	}
	if (b.Physical != nil || b.Logical != nil) && b.StoreID == nil {
		return fmt.Errorf("backup: store_id is required when physical or logical backups are configured")
	}
	if b.Physical != nil {
		p := b.Physical
		if p.BaseSchedule == "" {
			return fmt.Errorf("backup.physical: base_schedule is required")
		}
		if !cronRe.MatchString(p.BaseSchedule) {
			return fmt.Errorf("backup.physical: base_schedule must be a 5-field cron expression")
		}
		if p.IncrementalSchedule != "" && !cronRe.MatchString(p.IncrementalSchedule) {
			return fmt.Errorf("backup.physical: incremental_schedule must be a 5-field cron expression")
		}
		if p.ArchiveTimeoutSecs < 0 {
			return fmt.Errorf("backup.physical: archive_timeout_seconds must be >= 0")
		}
		if p.Retention.BaseBackupCount < 1 {
			return fmt.Errorf("backup.physical: retention.base_backup_count must be >= 1")
		}
		if p.WalArchiveEnabled && p.Retention.WalRetentionDays < 1 {
			return fmt.Errorf("backup.physical: retention.wal_retention_days must be >= 1 when WAL archiving is enabled")
		}
	}
	if b.Logical != nil {
		l := b.Logical
		if l.Schedule == "" {
			return fmt.Errorf("backup.logical: schedule is required")
		}
		if !cronRe.MatchString(l.Schedule) {
			return fmt.Errorf("backup.logical: schedule must be a 5-field cron expression")
		}
		switch l.Format {
		case "", "custom", "plain", "directory":
			// valid
		default:
			return fmt.Errorf("backup.logical: format must be \"custom\", \"plain\", or \"directory\"")
		}
		if l.Retention.BackupCount < 1 {
			return fmt.Errorf("backup.logical: retention.backup_count must be >= 1")
		}
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
	ID                 uuid.UUID       `json:"id" db:"id"`
	Name               string          `json:"name" db:"name"`
	Description        string          `json:"description" db:"description"`
	Config             json.RawMessage `json:"config" db:"config"`
	Locked             bool            `json:"locked" db:"locked"`
	RecoveryRuleSetID  *uuid.UUID      `json:"recovery_rule_set_id" db:"recovery_rule_set_id"`
	CreatedAt          time.Time       `json:"created_at" db:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at" db:"updated_at"`
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

// ---------- Backup Stores ----------

type BackupStore struct {
	ID             uuid.UUID       `json:"id"`
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	StoreType      string          `json:"store_type"`
	Config         json.RawMessage `json:"config"`
	Credentials    []byte          `json:"-"`                         // never serialized to API
	CredentialsSet map[string]bool `json:"credentials_set,omitempty"` // computed on read
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// Non-secret config (stored as plaintext JSONB)
type S3StoreConfig struct {
	Bucket         string `json:"bucket"`
	Region         string `json:"region"`
	Endpoint       string `json:"endpoint,omitempty"`
	ForcePathStyle bool   `json:"force_path_style,omitempty"`
}
type GCSStoreConfig struct {
	Bucket     string `json:"bucket"`
	PathPrefix string `json:"path_prefix,omitempty"`
}
type SFTPStoreConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port,omitempty"`
	User     string `json:"user"`
	BasePath string `json:"base_path"`
}
type LocalStoreConfig struct {
	Size         string `json:"size"`
	StorageClass string `json:"storage_class,omitempty"`
}

// Secret credentials (stored encrypted as BYTEA)
type S3StoreCredentials struct {
	AccessKeyID    string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
}
type GCSStoreCredentials struct {
	ServiceAccountJSON string `json:"service_account_json,omitempty"`
}
type SFTPStoreCredentials struct {
	Password   string `json:"password,omitempty"`
	PrivateKey string `json:"private_key,omitempty"`
}

// ValidateBackupStore validates a backup store's required fields.
func ValidateBackupStore(store *BackupStore) error {
	if store.Name == "" {
		return fmt.Errorf("name is required")
	}
	switch store.StoreType {
	case "s3":
		var cfg S3StoreConfig
		if err := json.Unmarshal(store.Config, &cfg); err != nil {
			return fmt.Errorf("invalid s3 config: %w", err)
		}
		if cfg.Bucket == "" {
			return fmt.Errorf("s3 store requires bucket")
		}
	case "gcs":
		var cfg GCSStoreConfig
		if err := json.Unmarshal(store.Config, &cfg); err != nil {
			return fmt.Errorf("invalid gcs config: %w", err)
		}
		if cfg.Bucket == "" {
			return fmt.Errorf("gcs store requires bucket")
		}
	case "sftp":
		var cfg SFTPStoreConfig
		if err := json.Unmarshal(store.Config, &cfg); err != nil {
			return fmt.Errorf("invalid sftp config: %w", err)
		}
		if cfg.Host == "" || cfg.BasePath == "" {
			return fmt.Errorf("sftp store requires host and base_path")
		}
	case "local":
		var cfg LocalStoreConfig
		if err := json.Unmarshal(store.Config, &cfg); err != nil {
			return fmt.Errorf("invalid local config: %w", err)
		}
		if cfg.Size == "" {
			return fmt.Errorf("local store requires size")
		}
	default:
		return fmt.Errorf("store_type must be \"s3\", \"gcs\", \"sftp\", or \"local\"")
	}
	return nil
}

// ComputeCredentialsSet unmarshals decrypted credential JSON and returns which fields are non-empty.
func ComputeCredentialsSet(storeType string, creds []byte) map[string]bool {
	if len(creds) == 0 {
		return nil
	}
	result := make(map[string]bool)
	switch storeType {
	case "s3":
		var c S3StoreCredentials
		if json.Unmarshal(creds, &c) == nil {
			result["access_key_id"] = c.AccessKeyID != ""
			result["secret_access_key"] = c.SecretAccessKey != ""
		}
	case "gcs":
		var c GCSStoreCredentials
		if json.Unmarshal(creds, &c) == nil {
			result["service_account_json"] = c.ServiceAccountJSON != ""
		}
	case "sftp":
		var c SFTPStoreCredentials
		if json.Unmarshal(creds, &c) == nil {
			result["password"] = c.Password != ""
			result["private_key"] = c.PrivateKey != ""
		}
	case "local":
		return nil
	}
	// Remove entries where value is false
	for k, v := range result {
		if !v {
			delete(result, k)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

type BackupInventory struct {
	ID              uuid.UUID  `json:"id" db:"id"`
	SatelliteID     uuid.UUID  `json:"satellite_id" db:"satellite_id"`
	ClusterName     string     `json:"cluster_name" db:"cluster_name"`
	BackupProfileID *uuid.UUID `json:"backup_profile_id,omitempty" db:"backup_profile_id"`
	BackupType      string     `json:"backup_type" db:"backup_type"` // "base", "wal", "logical"
	Status          string     `json:"status" db:"status"`           // "running", "completed", "failed"
	StartedAt       time.Time  `json:"started_at" db:"started_at"`
	CompletedAt     *time.Time `json:"completed_at,omitempty" db:"completed_at"`
	SizeBytes       int64      `json:"size_bytes" db:"size_bytes"`
	BackupPath      string     `json:"backup_path" db:"backup_path"`
	PgVersion       string     `json:"pg_version" db:"pg_version"`
	WalStartLSN     string     `json:"wal_start_lsn,omitempty" db:"wal_start_lsn"`
	WalEndLSN       string     `json:"wal_end_lsn,omitempty" db:"wal_end_lsn"`
	ErrorMessage    string     `json:"error_message,omitempty" db:"error_message"`
	CreatedAt       time.Time  `json:"created_at" db:"created_at"`
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
