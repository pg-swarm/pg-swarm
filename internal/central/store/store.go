package store

import (
	"context"

	"github.com/google/uuid"
	"github.com/pg-swarm/pg-swarm/internal/shared/models"
)

// Store defines the persistence layer used by the central control plane for
// managing satellites, cluster configurations, profiles, deployment rules,
// health reports, and events.
type Store interface {
	// Health
	Ping(ctx context.Context) error

	// Satellites
	CreateSatellite(ctx context.Context, sat *models.Satellite) error
	GetSatellite(ctx context.Context, id uuid.UUID) (*models.Satellite, error)
	GetSatelliteByToken(ctx context.Context, tokenHash string) (*models.Satellite, error)
	ListSatellites(ctx context.Context) ([]*models.Satellite, error)
	UpdateSatelliteState(ctx context.Context, id uuid.UUID, state models.SatelliteState) error
	UpdateSatelliteName(ctx context.Context, id uuid.UUID, name string) error
	SetSatelliteAuthToken(ctx context.Context, id uuid.UUID, tokenHash string) error
	UpdateSatelliteHeartbeat(ctx context.Context, id uuid.UUID) error
	UpdateSatelliteLabels(ctx context.Context, id uuid.UUID, labels map[string]string) error
	UpdateSatelliteStorageClasses(ctx context.Context, id uuid.UUID, classes []models.StorageClassInfo) error
	UpdateSatelliteTierMappings(ctx context.Context, id uuid.UUID, mappings map[string]string) error
	ListSatellitesByLabelSelector(ctx context.Context, selector map[string]string) ([]*models.Satellite, error)

	// Storage Tiers
	CreateStorageTier(ctx context.Context, tier *models.StorageTier) error
	GetStorageTier(ctx context.Context, id uuid.UUID) (*models.StorageTier, error)
	ListStorageTiers(ctx context.Context) ([]*models.StorageTier, error)
	UpdateStorageTier(ctx context.Context, tier *models.StorageTier) error
	DeleteStorageTier(ctx context.Context, id uuid.UUID) error
	GetActiveSatelliteByK8sCluster(ctx context.Context, k8sClusterName string) (*models.Satellite, error)
	ReassignClusterConfigs(ctx context.Context, oldSatelliteID, newSatelliteID uuid.UUID) (int, error)

	// Cluster Configs
	CreateClusterConfig(ctx context.Context, cfg *models.ClusterConfig) error
	GetClusterConfig(ctx context.Context, id uuid.UUID) (*models.ClusterConfig, error)
	ListClusterConfigs(ctx context.Context) ([]*models.ClusterConfig, error)
	UpdateClusterConfig(ctx context.Context, cfg *models.ClusterConfig) error
	DeleteClusterConfig(ctx context.Context, id uuid.UUID) error
	SetClusterPaused(ctx context.Context, id uuid.UUID, paused bool) (*models.ClusterConfig, error)
	GetClusterConfigsBySatellite(ctx context.Context, satelliteID uuid.UUID) ([]*models.ClusterConfig, error)
	GetClusterConfigsByProfile(ctx context.Context, profileID uuid.UUID) ([]*models.ClusterConfig, error)

	// Profiles
	CreateProfile(ctx context.Context, profile *models.ClusterProfile) error
	GetProfile(ctx context.Context, id uuid.UUID) (*models.ClusterProfile, error)
	ListProfiles(ctx context.Context) ([]*models.ClusterProfile, error)
	UpdateProfile(ctx context.Context, profile *models.ClusterProfile) error
	DeleteProfile(ctx context.Context, id uuid.UUID) error
	ForceDeleteProfile(ctx context.Context, id uuid.UUID) error
	TouchProfile(ctx context.Context, id uuid.UUID) error

	// Deployment Rules
	CreateDeploymentRule(ctx context.Context, rule *models.DeploymentRule) error
	GetDeploymentRule(ctx context.Context, id uuid.UUID) (*models.DeploymentRule, error)
	ListDeploymentRules(ctx context.Context) ([]*models.DeploymentRule, error)
	UpdateDeploymentRule(ctx context.Context, rule *models.DeploymentRule) error
	DeleteDeploymentRule(ctx context.Context, id uuid.UUID) error
	GetClusterConfigsByDeploymentRule(ctx context.Context, ruleID uuid.UUID) ([]*models.ClusterConfig, error)
	GetDeploymentRulesByProfile(ctx context.Context, profileID uuid.UUID) ([]*models.DeploymentRule, error)

	// Postgres Versions
	ListPostgresVersions(ctx context.Context) ([]*models.PostgresVersion, error)
	GetPostgresVersion(ctx context.Context, id uuid.UUID) (*models.PostgresVersion, error)
	GetPostgresVersionBySpec(ctx context.Context, version, variant string) (*models.PostgresVersion, error)
	CreatePostgresVersion(ctx context.Context, pv *models.PostgresVersion) error
	UpdatePostgresVersion(ctx context.Context, pv *models.PostgresVersion) error
	DeletePostgresVersion(ctx context.Context, id uuid.UUID) error
	SetDefaultPostgresVersion(ctx context.Context, id uuid.UUID) error

	// Postgres Variants
	ListPostgresVariants(ctx context.Context) ([]*models.PostgresVariant, error)
	CreatePostgresVariant(ctx context.Context, v *models.PostgresVariant) error
	DeletePostgresVariant(ctx context.Context, id uuid.UUID) error

	// Health
	UpdateClusterConfigState(ctx context.Context, satelliteID uuid.UUID, clusterName string, state models.ClusterState) error
	UpsertClusterHealth(ctx context.Context, health *models.ClusterHealth) error
	GetClusterHealth(ctx context.Context, satelliteID uuid.UUID, clusterName string) (*models.ClusterHealth, error)
	ListClusterHealth(ctx context.Context) ([]*models.ClusterHealth, error)

	// Events
	CreateEvent(ctx context.Context, event *models.Event) error
	ListEvents(ctx context.Context, limit int) ([]*models.Event, error)
	ListEventsByCluster(ctx context.Context, satelliteID uuid.UUID, clusterName string, limit int) ([]*models.Event, error)

	// Recovery Rule Sets
	CreateRecoveryRuleSet(ctx context.Context, rs *models.RecoveryRuleSet) error
	ListRecoveryRuleSets(ctx context.Context) ([]*models.RecoveryRuleSet, error)
	GetRecoveryRuleSet(ctx context.Context, id uuid.UUID) (*models.RecoveryRuleSet, error)
	UpdateRecoveryRuleSet(ctx context.Context, rs *models.RecoveryRuleSet) error
	DeleteRecoveryRuleSet(ctx context.Context, id uuid.UUID) error

	// Backup Stores
	CreateBackupStore(ctx context.Context, bs *models.BackupStore) error
	GetBackupStore(ctx context.Context, id uuid.UUID) (*models.BackupStore, error)
	ListBackupStores(ctx context.Context) ([]*models.BackupStore, error)
	UpdateBackupStore(ctx context.Context, bs *models.BackupStore) error
	DeleteBackupStore(ctx context.Context, id uuid.UUID) error

	// Backup Inventory
	CreateBackupInventory(ctx context.Context, bi *models.BackupInventory) error
	GetBackupInventory(ctx context.Context, id uuid.UUID) (*models.BackupInventory, error)
	ListBackupInventory(ctx context.Context, satelliteID uuid.UUID, clusterName string, limit int) ([]*models.BackupInventory, error)
	UpdateBackupInventory(ctx context.Context, bi *models.BackupInventory) error

	// Restore Operations
	CreateRestoreOperation(ctx context.Context, ro *models.RestoreOperation) error
	GetRestoreOperation(ctx context.Context, id uuid.UUID) (*models.RestoreOperation, error)
	ListRestoreOperations(ctx context.Context, satelliteID uuid.UUID, clusterName string, limit int) ([]*models.RestoreOperation, error)
	UpdateRestoreOperation(ctx context.Context, ro *models.RestoreOperation) error

	// Config Versions
	CreateConfigVersion(ctx context.Context, v *models.ConfigVersion) error
	ListConfigVersions(ctx context.Context, profileID uuid.UUID) ([]*models.ConfigVersion, error)
	GetConfigVersion(ctx context.Context, profileID uuid.UUID, version int) (*models.ConfigVersion, error)

	// Cluster Databases
	ListClusterDatabases(ctx context.Context, clusterID uuid.UUID) ([]*models.ClusterDatabase, error)
	GetClusterDatabaseByName(ctx context.Context, clusterID uuid.UUID, dbName string) (*models.ClusterDatabase, error)
	CreateClusterDatabase(ctx context.Context, db *models.ClusterDatabase) error
	UpdateClusterDatabase(ctx context.Context, db *models.ClusterDatabase) error
	UpdateClusterDatabaseStatus(ctx context.Context, id uuid.UUID, status, errorMsg string) error
	DeleteClusterDatabase(ctx context.Context, id uuid.UUID) error

	// PG Parameter Classifications
	ListPgParamClassifications(ctx context.Context) ([]*models.PgParamClassification, error)
	UpsertPgParamClassification(ctx context.Context, p *models.PgParamClassification) error
	DeletePgParamClassification(ctx context.Context, name string) error
}
