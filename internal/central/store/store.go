package store

import (
	"context"

	"github.com/google/uuid"
	"github.com/pg-swarm/pg-swarm/internal/shared/models"
)

type Store interface {
	// Satellites
	CreateSatellite(ctx context.Context, sat *models.Satellite) error
	GetSatellite(ctx context.Context, id uuid.UUID) (*models.Satellite, error)
	GetSatelliteByToken(ctx context.Context, tokenHash string) (*models.Satellite, error)
	ListSatellites(ctx context.Context) ([]*models.Satellite, error)
	UpdateSatelliteState(ctx context.Context, id uuid.UUID, state models.SatelliteState) error
	SetSatelliteAuthToken(ctx context.Context, id uuid.UUID, tokenHash string) error
	UpdateSatelliteHeartbeat(ctx context.Context, id uuid.UUID) error
	UpdateSatelliteLabels(ctx context.Context, id uuid.UUID, labels map[string]string) error
	UpdateSatelliteStorageClasses(ctx context.Context, id uuid.UUID, classes []models.StorageClassInfo) error
	ListSatellitesByLabelSelector(ctx context.Context, selector map[string]string) ([]*models.Satellite, error)

	// Cluster Configs
	CreateClusterConfig(ctx context.Context, cfg *models.ClusterConfig) error
	GetClusterConfig(ctx context.Context, id uuid.UUID) (*models.ClusterConfig, error)
	ListClusterConfigs(ctx context.Context) ([]*models.ClusterConfig, error)
	UpdateClusterConfig(ctx context.Context, cfg *models.ClusterConfig) error
	DeleteClusterConfig(ctx context.Context, id uuid.UUID) error
	SetClusterPaused(ctx context.Context, id uuid.UUID, paused bool) (*models.ClusterConfig, error)
	GetClusterConfigsBySatellite(ctx context.Context, satelliteID uuid.UUID) ([]*models.ClusterConfig, error)

	// Profiles
	CreateProfile(ctx context.Context, profile *models.ClusterProfile) error
	GetProfile(ctx context.Context, id uuid.UUID) (*models.ClusterProfile, error)
	ListProfiles(ctx context.Context) ([]*models.ClusterProfile, error)
	UpdateProfile(ctx context.Context, profile *models.ClusterProfile) error
	DeleteProfile(ctx context.Context, id uuid.UUID) error
	LockProfile(ctx context.Context, id uuid.UUID) error

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

	// Health
	UpsertClusterHealth(ctx context.Context, health *models.ClusterHealth) error
	GetClusterHealth(ctx context.Context, satelliteID uuid.UUID, clusterName string) (*models.ClusterHealth, error)
	ListClusterHealth(ctx context.Context) ([]*models.ClusterHealth, error)

	// Events
	CreateEvent(ctx context.Context, event *models.Event) error
	ListEvents(ctx context.Context, limit int) ([]*models.Event, error)
	ListEventsByCluster(ctx context.Context, satelliteID uuid.UUID, clusterName string, limit int) ([]*models.Event, error)
}
