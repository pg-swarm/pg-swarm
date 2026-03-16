package registry

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/pg-swarm/pg-swarm/internal/central/auth"
	"github.com/pg-swarm/pg-swarm/internal/central/store"
	"github.com/pg-swarm/pg-swarm/internal/shared/models"
	"github.com/rs/zerolog/log"
)

type Registry struct {
	store store.Store
}

func New(s store.Store) *Registry {
	return &Registry{store: s}
}

// Register creates a new satellite in pending state and returns its ID + temp token
func (r *Registry) Register(ctx context.Context, hostname, k8sClusterName, region string, labels map[string]string) (satelliteID uuid.UUID, tempToken string, err error) {
	tempToken, err = auth.GenerateToken()
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("generate temp token: %w", err)
	}

	sat := &models.Satellite{
		ID:             uuid.New(),
		Hostname:       hostname,
		K8sClusterName: k8sClusterName,
		Region:         region,
		Labels:         labels,
		State:          models.SatelliteStatePending,
		TempTokenHash:  auth.HashToken(tempToken),
	}

	if err := r.store.CreateSatellite(ctx, sat); err != nil {
		return uuid.Nil, "", fmt.Errorf("create satellite: %w", err)
	}

	log.Info().Str("satellite_id", sat.ID.String()).Str("hostname", hostname).Msg("satellite registered")
	return sat.ID, tempToken, nil
}

// ConflictingSatellite returns the active satellite that occupies the same
// k8s_cluster_name as the given pending satellite, or nil if there is no conflict.
func (r *Registry) ConflictingSatellite(ctx context.Context, satelliteID uuid.UUID) (*models.Satellite, error) {
	sat, err := r.store.GetSatellite(ctx, satelliteID)
	if err != nil {
		return nil, fmt.Errorf("get satellite: %w", err)
	}
	existing, err := r.store.GetActiveSatelliteByK8sCluster(ctx, sat.K8sClusterName)
	if err != nil {
		return nil, nil // no conflict
	}
	if existing.ID == satelliteID {
		return nil, nil
	}
	return existing, nil
}

// Approve approves a pending satellite and generates an auth token.
// If replace is true and another active satellite exists for the same
// k8s_cluster_name, it is replaced: its cluster configs are reassigned
// and the old satellite is marked as "replaced".
// If replace is false and a conflict exists, Approve returns an error.
func (r *Registry) Approve(ctx context.Context, satelliteID uuid.UUID, replace bool) (replacedID *uuid.UUID, authToken string, err error) {
	sat, err := r.store.GetSatellite(ctx, satelliteID)
	if err != nil {
		return nil, "", fmt.Errorf("get satellite: %w", err)
	}
	if sat.State != models.SatelliteStatePending {
		return nil, "", fmt.Errorf("satellite is not in pending state (current: %s)", sat.State)
	}

	// Check for an existing active satellite on the same K8s cluster
	existing, _ := r.store.GetActiveSatelliteByK8sCluster(ctx, sat.K8sClusterName)
	if existing != nil && existing.ID != satelliteID {
		if !replace {
			return nil, "", fmt.Errorf("k8s cluster %q already has an active satellite (%s); approve with replace=true to replace it",
				sat.K8sClusterName, existing.ID)
		}
		// Replace the old satellite: transfer configs, mark replaced
		count, err := r.store.ReassignClusterConfigs(ctx, existing.ID, satelliteID)
		if err != nil {
			return nil, "", fmt.Errorf("reassign cluster configs: %w", err)
		}
		if err := r.store.UpdateSatelliteState(ctx, existing.ID, models.SatelliteStateReplaced); err != nil {
			return nil, "", fmt.Errorf("mark old satellite replaced: %w", err)
		}
		log.Info().
			Str("old_satellite_id", existing.ID.String()).
			Str("new_satellite_id", satelliteID.String()).
			Int("configs_reassigned", count).
			Msg("satellite replaced — cluster configs reassigned")
		replacedID = &existing.ID
	}

	authToken, err = auth.GenerateToken()
	if err != nil {
		return nil, "", fmt.Errorf("generate auth token: %w", err)
	}

	if err := r.store.SetSatelliteAuthToken(ctx, satelliteID, auth.HashToken(authToken)); err != nil {
		return nil, "", fmt.Errorf("set auth token: %w", err)
	}
	if err := r.store.UpdateSatelliteState(ctx, satelliteID, models.SatelliteStateApproved); err != nil {
		return nil, "", fmt.Errorf("update state: %w", err)
	}

	log.Info().Str("satellite_id", satelliteID.String()).Msg("satellite approved")
	return replacedID, authToken, nil
}

// Reject rejects/removes a pending satellite
func (r *Registry) Reject(ctx context.Context, satelliteID uuid.UUID) error {
	sat, err := r.store.GetSatellite(ctx, satelliteID)
	if err != nil {
		return fmt.Errorf("get satellite: %w", err)
	}
	if sat.State != models.SatelliteStatePending {
		return fmt.Errorf("satellite is not in pending state (current: %s)", sat.State)
	}
	if err := r.store.UpdateSatelliteState(ctx, satelliteID, models.SatelliteStateDisconnected); err != nil {
		return fmt.Errorf("update state: %w", err)
	}
	log.Info().Str("satellite_id", satelliteID.String()).Msg("satellite rejected")
	return nil
}

// CheckApproval checks if a satellite has been approved and returns the auth token
func (r *Registry) CheckApproval(ctx context.Context, satelliteID uuid.UUID, tempToken string) (approved bool, authToken string, err error) {
	sat, err := r.store.GetSatellite(ctx, satelliteID)
	if err != nil {
		return false, "", fmt.Errorf("get satellite: %w", err)
	}

	if !auth.ValidateToken(tempToken, sat.TempTokenHash) {
		return false, "", fmt.Errorf("invalid temp token")
	}

	if sat.State == models.SatelliteStatePending {
		return false, "", nil
	}

	if sat.State == models.SatelliteStateApproved || sat.State == models.SatelliteStateConnected {
		// Generate a new auth token for the satellite to use
		// The satellite should only get this once during approval check
		newToken, err := auth.GenerateToken()
		if err != nil {
			return false, "", fmt.Errorf("generate auth token: %w", err)
		}
		if err := r.store.SetSatelliteAuthToken(ctx, satelliteID, auth.HashToken(newToken)); err != nil {
			return false, "", fmt.Errorf("set auth token: %w", err)
		}
		return true, newToken, nil
	}

	return false, "", fmt.Errorf("satellite in unexpected state: %s", sat.State)
}
