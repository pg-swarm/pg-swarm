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

	// Groups
	CreateGroup(ctx context.Context, group *models.EdgeGroup) error
	GetGroup(ctx context.Context, id uuid.UUID) (*models.EdgeGroup, error)
	ListGroups(ctx context.Context) ([]*models.EdgeGroup, error)
	AssignSatelliteToGroup(ctx context.Context, satelliteID, groupID uuid.UUID) error

	// Cluster Configs
	CreateClusterConfig(ctx context.Context, cfg *models.ClusterConfig) error
	GetClusterConfig(ctx context.Context, id uuid.UUID) (*models.ClusterConfig, error)
	ListClusterConfigs(ctx context.Context) ([]*models.ClusterConfig, error)
	UpdateClusterConfig(ctx context.Context, cfg *models.ClusterConfig) error
	DeleteClusterConfig(ctx context.Context, id uuid.UUID) error
	GetClusterConfigsBySatellite(ctx context.Context, satelliteID uuid.UUID) ([]*models.ClusterConfig, error)
	GetClusterConfigsByGroup(ctx context.Context, groupID uuid.UUID) ([]*models.ClusterConfig, error)

	// Profiles
	CreateProfile(ctx context.Context, profile *models.ClusterProfile) error
	GetProfile(ctx context.Context, id uuid.UUID) (*models.ClusterProfile, error)
	ListProfiles(ctx context.Context) ([]*models.ClusterProfile, error)
	UpdateProfile(ctx context.Context, profile *models.ClusterProfile) error
	DeleteProfile(ctx context.Context, id uuid.UUID) error
	LockProfile(ctx context.Context, id uuid.UUID) error

	// Deployment Groups
	CreateDeploymentGroup(ctx context.Context, dg *models.DeploymentGroup) error
	GetDeploymentGroup(ctx context.Context, id uuid.UUID) (*models.DeploymentGroup, error)
	ListDeploymentGroups(ctx context.Context) ([]*models.DeploymentGroup, error)
	UpdateDeploymentGroup(ctx context.Context, dg *models.DeploymentGroup) error
	DeleteDeploymentGroup(ctx context.Context, id uuid.UUID) error
	GetClusterConfigsByDeploymentGroup(ctx context.Context, dgID uuid.UUID) ([]*models.ClusterConfig, error)

	// Health
	UpsertClusterHealth(ctx context.Context, health *models.ClusterHealth) error
	GetClusterHealth(ctx context.Context, satelliteID uuid.UUID, clusterName string) (*models.ClusterHealth, error)
	ListClusterHealth(ctx context.Context) ([]*models.ClusterHealth, error)

	// Events
	CreateEvent(ctx context.Context, event *models.Event) error
	ListEvents(ctx context.Context, limit int) ([]*models.Event, error)
	ListEventsByCluster(ctx context.Context, satelliteID uuid.UUID, clusterName string, limit int) ([]*models.Event, error)
}
