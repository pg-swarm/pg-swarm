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
