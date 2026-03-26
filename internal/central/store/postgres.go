package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-swarm/pg-swarm/internal/shared/models"
	"github.com/rs/zerolog/log"
)

// PostgresStore implements the Store interface using a PostgreSQL connection pool.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore returns a new PostgresStore backed by the given connection pool.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

// Ping verifies the database connection is alive.
func (s *PostgresStore) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// Column lists used across queries (keep in sync with scanners below).
const (
	satCols  = `id, name, hostname, k8s_cluster_name, region, labels, storage_classes, tier_mappings, state, auth_token_hash, temp_token_hash, last_heartbeat, created_at, updated_at`
	cfgCols  = `id, name, namespace, satellite_id, profile_id, deployment_rule_id, config, config_version, applied_profile_version, state, paused, created_at, updated_at`
	ruleCols = `id, name, profile_id, label_selector, namespace, cluster_name, created_at, updated_at`
)

// scanSatellite scans a satellite row into a Satellite struct.
func scanSatellite(row pgx.Row) (*models.Satellite, error) {
	var sat models.Satellite
	var labelsJSON []byte
	var scJSON []byte
	var tmJSON []byte
	err := row.Scan(
		&sat.ID,
		&sat.Name,
		&sat.Hostname,
		&sat.K8sClusterName,
		&sat.Region,
		&labelsJSON,
		&scJSON,
		&tmJSON,
		&sat.State,
		&sat.AuthTokenHash,
		&sat.TempTokenHash,
		&sat.LastHeartbeat,
		&sat.CreatedAt,
		&sat.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if labelsJSON != nil {
		if err := json.Unmarshal(labelsJSON, &sat.Labels); err != nil {
			return nil, fmt.Errorf("unmarshal satellite labels: %w", err)
		}
	}
	if sat.Labels == nil {
		sat.Labels = make(map[string]string)
	}
	if scJSON != nil {
		if err := json.Unmarshal(scJSON, &sat.StorageClasses); err != nil {
			return nil, fmt.Errorf("unmarshal satellite storage_classes: %w", err)
		}
	}
	if sat.StorageClasses == nil {
		sat.StorageClasses = []models.StorageClassInfo{}
	}
	if tmJSON != nil {
		if err := json.Unmarshal(tmJSON, &sat.TierMappings); err != nil {
			return nil, fmt.Errorf("unmarshal satellite tier_mappings: %w", err)
		}
	}
	if sat.TierMappings == nil {
		sat.TierMappings = make(map[string]string)
	}
	return &sat, nil
}

// scanClusterConfig scans a cluster_configs row into a ClusterConfig struct.
func scanClusterConfig(row pgx.Row) (*models.ClusterConfig, error) {
	var cfg models.ClusterConfig
	err := row.Scan(
		&cfg.ID,
		&cfg.Name,
		&cfg.Namespace,
		&cfg.SatelliteID,
		&cfg.ProfileID,
		&cfg.DeploymentRuleID,
		&cfg.Config,
		&cfg.ConfigVersion,
		&cfg.AppliedProfileVersion,
		&cfg.State,
		&cfg.Paused,
		&cfg.CreatedAt,
		&cfg.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if cfg.Config == nil {
		cfg.Config = json.RawMessage("{}")
	}
	return &cfg, nil
}

// scanDeploymentRule scans a deployment_rules row into a DeploymentRule struct.
func scanDeploymentRule(row pgx.Row) (*models.DeploymentRule, error) {
	var r models.DeploymentRule
	var selectorJSON []byte
	err := row.Scan(
		&r.ID,
		&r.Name,
		&r.ProfileID,
		&selectorJSON,
		&r.Namespace,
		&r.ClusterName,
		&r.CreatedAt,
		&r.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if selectorJSON != nil {
		if err := json.Unmarshal(selectorJSON, &r.LabelSelector); err != nil {
			return nil, fmt.Errorf("unmarshal label_selector: %w", err)
		}
	}
	if r.LabelSelector == nil {
		r.LabelSelector = make(map[string]string)
	}
	return &r, nil
}

// scanProfile scans a cluster_profiles row into a ClusterProfile struct.
func scanProfile(row pgx.Row) (*models.ClusterProfile, error) {
	var p models.ClusterProfile
	err := row.Scan(
		&p.ID,
		&p.Name,
		&p.Description,
		&p.Config,
		&p.InUse,
		&p.RecoveryRuleSetID,
		&p.CreatedAt,
		&p.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if p.Config == nil {
		p.Config = json.RawMessage("{}")
	}
	return &p, nil
}

// scanClusterHealth scans a cluster_health row into a ClusterHealth struct.
func scanClusterHealth(row pgx.Row) (*models.ClusterHealth, error) {
	var h models.ClusterHealth
	err := row.Scan(
		&h.SatelliteID,
		&h.ClusterName,
		&h.State,
		&h.Instances,
		&h.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if h.Instances == nil {
		h.Instances = json.RawMessage("[]")
	}
	return &h, nil
}

// scanEvent scans an events row into an Event struct.
func scanEvent(row pgx.Row) (*models.Event, error) {
	var e models.Event
	err := row.Scan(
		&e.ID,
		&e.SatelliteID,
		&e.ClusterName,
		&e.Severity,
		&e.Message,
		&e.Source,
		&e.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// ---------- Satellites ----------

// CreateSatellite inserts a new satellite record.
func (s *PostgresStore) CreateSatellite(ctx context.Context, sat *models.Satellite) error {
	if sat.ID == uuid.Nil {
		sat.ID = uuid.New()
	}
	if sat.Labels == nil {
		sat.Labels = make(map[string]string)
	}
	labelsJSON, err := json.Marshal(sat.Labels)
	if err != nil {
		return fmt.Errorf("marshal satellite labels: %w", err)
	}
	now := time.Now()
	sat.CreatedAt = now
	sat.UpdatedAt = now
	if sat.State == "" {
		sat.State = models.SatelliteStatePending
	}
	if sat.Name == "" {
		sat.Name = sat.K8sClusterName // Default to k8s cluster name initially
	}

	_, err = s.pool.Exec(ctx,
		`INSERT INTO satellites (id, name, hostname, k8s_cluster_name, region, labels, state, auth_token_hash, temp_token_hash, last_heartbeat, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		sat.ID, sat.Name, sat.Hostname, sat.K8sClusterName, sat.Region, labelsJSON, sat.State,
		sat.AuthTokenHash, sat.TempTokenHash, sat.LastHeartbeat,
		sat.CreatedAt, sat.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("create satellite: %w", err)
	}
	return nil
}

// GetSatellite returns a satellite by its ID.
func (s *PostgresStore) GetSatellite(ctx context.Context, id uuid.UUID) (*models.Satellite, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+satCols+` FROM satellites WHERE id = $1`, id)
	sat, err := scanSatellite(row)
	if err != nil {
		return nil, fmt.Errorf("get satellite %s: %w", id, err)
	}
	return sat, nil
}

// GetSatelliteByToken returns a satellite matching the given auth token hash.
func (s *PostgresStore) GetSatelliteByToken(ctx context.Context, tokenHash string) (*models.Satellite, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+satCols+` FROM satellites WHERE auth_token_hash = $1`, tokenHash)
	sat, err := scanSatellite(row)
	if err != nil {
		return nil, fmt.Errorf("get satellite by token: %w", err)
	}
	return sat, nil
}

// ListSatellites returns all satellites ordered by creation time.
func (s *PostgresStore) ListSatellites(ctx context.Context) ([]*models.Satellite, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+satCols+` FROM satellites WHERE state != 'replaced' ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list satellites: %w", err)
	}
	defer rows.Close()

	var result []*models.Satellite
	for rows.Next() {
		sat, err := scanSatellite(rows)
		if err != nil {
			return nil, fmt.Errorf("scan satellite row: %w", err)
		}
		result = append(result, sat)
	}
	return result, rows.Err()
}

// UpdateSatelliteState sets the state of a satellite.
func (s *PostgresStore) UpdateSatelliteState(ctx context.Context, id uuid.UUID, state models.SatelliteState) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE satellites SET state = $1, updated_at = NOW() WHERE id = $2`, state, id)
	if err != nil {
		return fmt.Errorf("update satellite state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("satellite %s not found", id)
	}
	return nil
}

// UpdateSatelliteName sets the unique name of a satellite.
func (s *PostgresStore) UpdateSatelliteName(ctx context.Context, id uuid.UUID, name string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE satellites SET name = $1, updated_at = NOW() WHERE id = $2`, name, id)
	if err != nil {
		return fmt.Errorf("update satellite name: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("satellite %s not found", id)
	}
	return nil
}

// SetSatelliteAuthToken updates the auth token hash for a satellite.
func (s *PostgresStore) SetSatelliteAuthToken(ctx context.Context, id uuid.UUID, tokenHash string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE satellites SET auth_token_hash = $1, updated_at = NOW() WHERE id = $2`, tokenHash, id)
	if err != nil {
		return fmt.Errorf("set satellite auth token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("satellite %s not found", id)
	}
	return nil
}

// UpdateSatelliteHeartbeat records the current time as the satellite's last heartbeat.
func (s *PostgresStore) UpdateSatelliteHeartbeat(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE satellites SET last_heartbeat = NOW(), updated_at = NOW() WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("update satellite heartbeat: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("satellite %s not found", id)
	}
	return nil
}

// UpdateSatelliteLabels replaces all labels on a satellite.
func (s *PostgresStore) UpdateSatelliteLabels(ctx context.Context, id uuid.UUID, labels map[string]string) error {
	if labels == nil {
		labels = make(map[string]string)
	}
	labelsJSON, err := json.Marshal(labels)
	if err != nil {
		return fmt.Errorf("marshal satellite labels: %w", err)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE satellites SET labels = $1, updated_at = NOW() WHERE id = $2`, labelsJSON, id)
	if err != nil {
		return fmt.Errorf("update satellite labels: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("satellite %s not found", id)
	}
	return nil
}

// UpdateSatelliteStorageClasses replaces the storage class list for a satellite.
func (s *PostgresStore) UpdateSatelliteStorageClasses(ctx context.Context, id uuid.UUID, classes []models.StorageClassInfo) error {
	if classes == nil {
		classes = []models.StorageClassInfo{}
	}
	scJSON, err := json.Marshal(classes)
	if err != nil {
		return fmt.Errorf("marshal storage classes: %w", err)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE satellites SET storage_classes = $1, updated_at = NOW() WHERE id = $2`, scJSON, id)
	if err != nil {
		return fmt.Errorf("update satellite storage classes: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("satellite %s not found", id)
	}
	return nil
}

// UpdateSatelliteTierMappings replaces the tier mappings for a satellite.
func (s *PostgresStore) UpdateSatelliteTierMappings(ctx context.Context, id uuid.UUID, mappings map[string]string) error {
	if mappings == nil {
		mappings = make(map[string]string)
	}
	tmJSON, err := json.Marshal(mappings)
	if err != nil {
		return fmt.Errorf("marshal tier mappings: %w", err)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE satellites SET tier_mappings = $1, updated_at = NOW() WHERE id = $2`, tmJSON, id)
	if err != nil {
		return fmt.Errorf("update satellite tier mappings: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("satellite %s not found", id)
	}
	return nil
}

// ---- Storage Tiers ----

func (s *PostgresStore) CreateStorageTier(ctx context.Context, tier *models.StorageTier) error {
	if tier.ID == uuid.Nil {
		tier.ID = uuid.New()
	}
	now := time.Now()
	tier.CreatedAt = now
	tier.UpdatedAt = now
	_, err := s.pool.Exec(ctx,
		`INSERT INTO storage_tiers (id, name, description, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5)`,
		tier.ID, tier.Name, tier.Description, tier.CreatedAt, tier.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create storage tier: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetStorageTier(ctx context.Context, id uuid.UUID) (*models.StorageTier, error) {
	var t models.StorageTier
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, description, created_at, updated_at FROM storage_tiers WHERE id = $1`, id).
		Scan(&t.ID, &t.Name, &t.Description, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get storage tier: %w", err)
	}
	return &t, nil
}

func (s *PostgresStore) ListStorageTiers(ctx context.Context) ([]*models.StorageTier, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, description, created_at, updated_at FROM storage_tiers ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list storage tiers: %w", err)
	}
	defer rows.Close()
	var tiers []*models.StorageTier
	for rows.Next() {
		var t models.StorageTier
		if err := rows.Scan(&t.ID, &t.Name, &t.Description, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan storage tier: %w", err)
		}
		tiers = append(tiers, &t)
	}
	if tiers == nil {
		tiers = []*models.StorageTier{}
	}
	return tiers, rows.Err()
}

func (s *PostgresStore) UpdateStorageTier(ctx context.Context, tier *models.StorageTier) error {
	tier.UpdatedAt = time.Now()
	tag, err := s.pool.Exec(ctx,
		`UPDATE storage_tiers SET name = $1, description = $2, updated_at = $3 WHERE id = $4`,
		tier.Name, tier.Description, tier.UpdatedAt, tier.ID)
	if err != nil {
		return fmt.Errorf("update storage tier: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("storage tier %s not found", tier.ID)
	}
	return nil
}

func (s *PostgresStore) DeleteStorageTier(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM storage_tiers WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete storage tier: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("storage tier %s not found", id)
	}
	return nil
}

// ListSatellitesByLabelSelector returns satellites whose labels contain all selector key-value pairs.
func (s *PostgresStore) ListSatellitesByLabelSelector(ctx context.Context, selector map[string]string) ([]*models.Satellite, error) {
	if len(selector) == 0 {
		return nil, nil
	}
	selectorJSON, err := json.Marshal(selector)
	if err != nil {
		return nil, fmt.Errorf("marshal label selector: %w", err)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+satCols+` FROM satellites WHERE labels @> $1::jsonb AND state != 'replaced' ORDER BY created_at DESC`, selectorJSON)
	if err != nil {
		return nil, fmt.Errorf("list satellites by label selector: %w", err)
	}
	defer rows.Close()

	var result []*models.Satellite
	for rows.Next() {
		sat, err := scanSatellite(rows)
		if err != nil {
			return nil, fmt.Errorf("scan satellite row: %w", err)
		}
		result = append(result, sat)
	}
	return result, rows.Err()
}

// GetActiveSatelliteByK8sCluster returns the active (approved/connected) satellite
// for a given K8s cluster name, or nil if none exists.
func (s *PostgresStore) GetActiveSatelliteByK8sCluster(ctx context.Context, k8sClusterName string) (*models.Satellite, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+satCols+` FROM satellites
		 WHERE k8s_cluster_name = $1 AND state IN ('approved', 'connected')
		 LIMIT 1`, k8sClusterName)
	sat, err := scanSatellite(row)
	if err != nil {
		return nil, err
	}
	return sat, nil
}

// ReassignClusterConfigs transfers all cluster configs from one satellite to another,
// bumping config_version so the new satellite picks them up.
func (s *PostgresStore) ReassignClusterConfigs(ctx context.Context, oldSatelliteID, newSatelliteID uuid.UUID) (int, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE cluster_configs
		 SET satellite_id = $1, config_version = config_version + 1, updated_at = NOW()
		 WHERE satellite_id = $2`,
		newSatelliteID, oldSatelliteID)
	if err != nil {
		return 0, fmt.Errorf("reassign cluster configs: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ---------- Cluster Configs ----------

// CreateClusterConfig inserts a new cluster configuration.
func (s *PostgresStore) CreateClusterConfig(ctx context.Context, cfg *models.ClusterConfig) error {
	if cfg.ID == uuid.Nil {
		cfg.ID = uuid.New()
	}
	if cfg.Config == nil {
		cfg.Config = json.RawMessage("{}")
	}
	if cfg.State == "" {
		cfg.State = models.ClusterStateCreating
	}
	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}
	if cfg.ConfigVersion == 0 {
		cfg.ConfigVersion = 1
	}
	now := time.Now()
	cfg.CreatedAt = now
	cfg.UpdatedAt = now

	_, err := s.pool.Exec(ctx,
		`INSERT INTO cluster_configs (id, name, namespace, satellite_id, profile_id, deployment_rule_id, config, config_version, applied_profile_version, state, paused, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		cfg.ID, cfg.Name, cfg.Namespace, cfg.SatelliteID, cfg.ProfileID,
		cfg.DeploymentRuleID, cfg.Config, cfg.ConfigVersion, cfg.AppliedProfileVersion, cfg.State, cfg.Paused, cfg.CreatedAt, cfg.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("create cluster config: %w", err)
	}
	return nil
}

// GetClusterConfig returns a cluster configuration by its ID.
func (s *PostgresStore) GetClusterConfig(ctx context.Context, id uuid.UUID) (*models.ClusterConfig, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+cfgCols+` FROM cluster_configs WHERE id = $1`, id)
	cfg, err := scanClusterConfig(row)
	if err != nil {
		return nil, fmt.Errorf("get cluster config %s: %w", id, err)
	}
	return cfg, nil
}

// ListClusterConfigs returns all cluster configurations ordered by creation time.
func (s *PostgresStore) ListClusterConfigs(ctx context.Context) ([]*models.ClusterConfig, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+cfgCols+` FROM cluster_configs ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list cluster configs: %w", err)
	}
	defer rows.Close()

	var result []*models.ClusterConfig
	for rows.Next() {
		cfg, err := scanClusterConfig(rows)
		if err != nil {
			return nil, fmt.Errorf("scan cluster config row: %w", err)
		}
		result = append(result, cfg)
	}
	return result, rows.Err()
}

// UpdateClusterConfig updates a cluster configuration and bumps its config version.
// The bumped config_version is written back to cfg so callers see the new value.
func (s *PostgresStore) UpdateClusterConfig(ctx context.Context, cfg *models.ClusterConfig) error {
	if cfg.Config == nil {
		cfg.Config = json.RawMessage("{}")
	}
	cfg.UpdatedAt = time.Now()

	var newVersion int64
	err := s.pool.QueryRow(ctx,
		`UPDATE cluster_configs SET name = $1, namespace = $2, satellite_id = $3,
		 profile_id = $4, deployment_rule_id = $5, config = $6, config_version = config_version + 1,
		 applied_profile_version = $7, paused = $8, updated_at = $9
		 WHERE id = $10
		 RETURNING config_version`,
		cfg.Name, cfg.Namespace, cfg.SatelliteID,
		cfg.ProfileID, cfg.DeploymentRuleID, cfg.Config, cfg.AppliedProfileVersion, cfg.Paused, cfg.UpdatedAt, cfg.ID,
	).Scan(&newVersion)
	if err != nil {
		return fmt.Errorf("update cluster config: %w", err)
	}
	cfg.ConfigVersion = newVersion
	return nil
}

// SetClusterPaused toggles the paused state of a cluster configuration.
func (s *PostgresStore) SetClusterPaused(ctx context.Context, id uuid.UUID, paused bool) (*models.ClusterConfig, error) {
	state := models.ClusterStatePaused
	if !paused {
		state = models.ClusterStateCreating
	}
	row := s.pool.QueryRow(ctx,
		`UPDATE cluster_configs SET paused = $1, state = $2, config_version = config_version + 1, updated_at = NOW()
		 WHERE id = $3 RETURNING `+cfgCols,
		paused, state, id,
	)
	cfg, err := scanClusterConfig(row)
	if err != nil {
		return nil, fmt.Errorf("set cluster paused %s: %w", id, err)
	}
	return cfg, nil
}

// DeleteClusterConfig removes a cluster configuration by its ID.
func (s *PostgresStore) DeleteClusterConfig(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM cluster_configs WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete cluster config: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("cluster config %s not found", id)
	}
	return nil
}

// GetClusterConfigsBySatellite returns all cluster configurations assigned to a satellite.
func (s *PostgresStore) GetClusterConfigsBySatellite(ctx context.Context, satelliteID uuid.UUID) ([]*models.ClusterConfig, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+cfgCols+` FROM cluster_configs WHERE satellite_id = $1 ORDER BY created_at DESC`, satelliteID)
	if err != nil {
		return nil, fmt.Errorf("get cluster configs by satellite: %w", err)
	}
	defer rows.Close()

	var result []*models.ClusterConfig
	for rows.Next() {
		cfg, err := scanClusterConfig(rows)
		if err != nil {
			return nil, fmt.Errorf("scan cluster config row: %w", err)
		}
		result = append(result, cfg)
	}
	return result, rows.Err()
}

// GetClusterConfigsByProfile returns all cluster configurations linked to a profile (directly or via deployment rules).
func (s *PostgresStore) GetClusterConfigsByProfile(ctx context.Context, profileID uuid.UUID) ([]*models.ClusterConfig, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+cfgCols+` FROM cluster_configs WHERE profile_id = $1 ORDER BY created_at DESC`, profileID)
	if err != nil {
		return nil, fmt.Errorf("get cluster configs by profile: %w", err)
	}
	defer rows.Close()

	var result []*models.ClusterConfig
	for rows.Next() {
		cfg, err := scanClusterConfig(rows)
		if err != nil {
			return nil, fmt.Errorf("scan cluster config row: %w", err)
		}
		result = append(result, cfg)
	}
	return result, rows.Err()
}

// ---------- Profiles ----------

// CreateProfile inserts a new cluster profile.
func (s *PostgresStore) CreateProfile(ctx context.Context, p *models.ClusterProfile) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	if p.Config == nil {
		p.Config = json.RawMessage("{}")
	}
	now := time.Now()
	p.CreatedAt = now
	p.UpdatedAt = now

	_, err := s.pool.Exec(ctx,
		`INSERT INTO cluster_profiles (id, name, description, config, recovery_rule_set_id, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		p.ID, p.Name, p.Description, p.Config, p.RecoveryRuleSetID, p.CreatedAt, p.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("create profile: %w", err)
	}
	return nil
}

// GetProfile returns a cluster profile by its ID.
func (s *PostgresStore) GetProfile(ctx context.Context, id uuid.UUID) (*models.ClusterProfile, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT p.id, p.name, p.description, p.config,
		        (EXISTS(SELECT 1 FROM deployment_rules r WHERE r.profile_id = p.id) OR
		         EXISTS(SELECT 1 FROM cluster_configs c WHERE c.profile_id = p.id)) AS in_use,
		        p.recovery_rule_set_id, p.created_at, p.updated_at
		 FROM cluster_profiles p WHERE p.id = $1`, id)
	p, err := scanProfile(row)
	if err != nil {
		return nil, fmt.Errorf("get profile %s: %w", id, err)
	}
	return p, nil
}

// ListProfiles returns all cluster profiles ordered by creation time.
func (s *PostgresStore) ListProfiles(ctx context.Context) ([]*models.ClusterProfile, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT p.id, p.name, p.description, p.config,
		        (EXISTS(SELECT 1 FROM deployment_rules r WHERE r.profile_id = p.id) OR
		         EXISTS(SELECT 1 FROM cluster_configs c WHERE c.profile_id = p.id)) AS in_use,
		        p.recovery_rule_set_id, p.created_at, p.updated_at
		 FROM cluster_profiles p ORDER BY p.created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list profiles: %w", err)
	}
	defer rows.Close()

	var result []*models.ClusterProfile
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, fmt.Errorf("scan profile row: %w", err)
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

// UpdateProfile updates a cluster profile if it is not in use.
func (s *PostgresStore) UpdateProfile(ctx context.Context, p *models.ClusterProfile) error {
	if p.Config == nil {
		p.Config = json.RawMessage("{}")
	}
	p.UpdatedAt = time.Now()

	tag, err := s.pool.Exec(ctx,
		`UPDATE cluster_profiles SET name = $1, description = $2, config = $3, recovery_rule_set_id = $4, updated_at = $5
		 WHERE id = $6`,
		p.Name, p.Description, p.Config, p.RecoveryRuleSetID, p.UpdatedAt, p.ID,
	)
	if err != nil {
		return fmt.Errorf("update profile: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("profile %s not found", p.ID)
	}
	return nil
}

// TouchProfile bumps the updated_at timestamp on a cluster profile without
// changing any other fields.
func (s *PostgresStore) TouchProfile(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE cluster_profiles SET updated_at = NOW() WHERE id = $1`, id)
	return err
}

// DeleteProfile removes a cluster profile if it is not in use.
func (s *PostgresStore) DeleteProfile(ctx context.Context, id uuid.UUID) error {
	var inUse bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM deployment_rules WHERE profile_id = $1) OR 
		       EXISTS(SELECT 1 FROM cluster_configs WHERE profile_id = $1)`, id).Scan(&inUse)
	if err != nil {
		return fmt.Errorf("check profile lock state: %w", err)
	}
	if inUse {
		return fmt.Errorf("profile %s is currently in use and cannot be deleted", id)
	}

	tag, err := s.pool.Exec(ctx,
		`DELETE FROM cluster_profiles WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete profile: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("profile %s not found", id)
	}
	return nil
}

// ForceDeleteProfile removes a cluster profile regardless of lock state.
// Used by cascade deletion where the caller has already cleaned up dependents.
func (s *PostgresStore) ForceDeleteProfile(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM cluster_profiles WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("force delete profile: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("profile %s not found", id)
	}
	return nil
}

// ---------- Deployment Rules ----------

// CreateDeploymentRule inserts a new deployment rule.
func (s *PostgresStore) CreateDeploymentRule(ctx context.Context, r *models.DeploymentRule) error {
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	if r.LabelSelector == nil {
		r.LabelSelector = make(map[string]string)
	}
	selectorJSON, err := json.Marshal(r.LabelSelector)
	if err != nil {
		return fmt.Errorf("marshal label selector: %w", err)
	}
	now := time.Now()
	r.CreatedAt = now
	r.UpdatedAt = now
	if r.Namespace == "" {
		r.Namespace = "default"
	}

	_, err = s.pool.Exec(ctx,
		`INSERT INTO deployment_rules (id, name, profile_id, label_selector, namespace, cluster_name, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		r.ID, r.Name, r.ProfileID, selectorJSON, r.Namespace, r.ClusterName, r.CreatedAt, r.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("create deployment rule: %w", err)
	}
	return nil
}

// GetDeploymentRule returns a deployment rule by its ID.
func (s *PostgresStore) GetDeploymentRule(ctx context.Context, id uuid.UUID) (*models.DeploymentRule, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+ruleCols+` FROM deployment_rules WHERE id = $1`, id)
	r, err := scanDeploymentRule(row)
	if err != nil {
		return nil, fmt.Errorf("get deployment rule %s: %w", id, err)
	}
	return r, nil
}

// ListDeploymentRules returns all deployment rules ordered by creation time.
func (s *PostgresStore) ListDeploymentRules(ctx context.Context) ([]*models.DeploymentRule, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+ruleCols+` FROM deployment_rules ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list deployment rules: %w", err)
	}
	defer rows.Close()

	var result []*models.DeploymentRule
	for rows.Next() {
		r, err := scanDeploymentRule(rows)
		if err != nil {
			return nil, fmt.Errorf("scan deployment rule row: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// UpdateDeploymentRule updates a deployment rule's fields.
func (s *PostgresStore) UpdateDeploymentRule(ctx context.Context, r *models.DeploymentRule) error {
	if r.LabelSelector == nil {
		r.LabelSelector = make(map[string]string)
	}
	selectorJSON, err := json.Marshal(r.LabelSelector)
	if err != nil {
		return fmt.Errorf("marshal label selector: %w", err)
	}
	r.UpdatedAt = time.Now()

	tag, err := s.pool.Exec(ctx,
		`UPDATE deployment_rules SET name = $1, profile_id = $2, label_selector = $3,
		 namespace = $4, cluster_name = $5, updated_at = $6
		 WHERE id = $7`,
		r.Name, r.ProfileID, selectorJSON, r.Namespace, r.ClusterName, r.UpdatedAt, r.ID,
	)
	if err != nil {
		return fmt.Errorf("update deployment rule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("deployment rule %s not found", r.ID)
	}
	return nil
}

// DeleteDeploymentRule removes a deployment rule by its ID.
func (s *PostgresStore) DeleteDeploymentRule(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM deployment_rules WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete deployment rule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("deployment rule %s not found", id)
	}
	return nil
}

// GetClusterConfigsByDeploymentRule returns cluster configs created by a deployment rule.
func (s *PostgresStore) GetClusterConfigsByDeploymentRule(ctx context.Context, ruleID uuid.UUID) ([]*models.ClusterConfig, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+cfgCols+` FROM cluster_configs WHERE deployment_rule_id = $1 ORDER BY created_at DESC`, ruleID)
	if err != nil {
		return nil, fmt.Errorf("get cluster configs by deployment rule: %w", err)
	}
	defer rows.Close()

	var result []*models.ClusterConfig
	for rows.Next() {
		cfg, err := scanClusterConfig(rows)
		if err != nil {
			return nil, fmt.Errorf("scan cluster config row: %w", err)
		}
		result = append(result, cfg)
	}
	return result, rows.Err()
}

// GetDeploymentRulesByProfile returns all deployment rules linked to a profile.
func (s *PostgresStore) GetDeploymentRulesByProfile(ctx context.Context, profileID uuid.UUID) ([]*models.DeploymentRule, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+ruleCols+` FROM deployment_rules WHERE profile_id = $1 ORDER BY created_at DESC`, profileID)
	if err != nil {
		return nil, fmt.Errorf("get deployment rules by profile: %w", err)
	}
	defer rows.Close()

	var result []*models.DeploymentRule
	for rows.Next() {
		r, err := scanDeploymentRule(rows)
		if err != nil {
			return nil, fmt.Errorf("scan deployment rule row: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// ---------- Postgres Versions ----------

const pvCols = `id, version, variant, image_tag, is_default, created_at, updated_at`

func scanPostgresVersion(row pgx.Row) (*models.PostgresVersion, error) {
	var pv models.PostgresVersion
	err := row.Scan(&pv.ID, &pv.Version, &pv.Variant, &pv.ImageTag, &pv.IsDefault, &pv.CreatedAt, &pv.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &pv, nil
}

// ListPostgresVersions returns all registered PostgreSQL versions.
func (s *PostgresStore) ListPostgresVersions(ctx context.Context) ([]*models.PostgresVersion, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+pvCols+` FROM postgres_versions ORDER BY version, variant`)
	if err != nil {
		return nil, fmt.Errorf("list postgres versions: %w", err)
	}
	defer rows.Close()

	var result []*models.PostgresVersion
	for rows.Next() {
		pv, err := scanPostgresVersion(rows)
		if err != nil {
			return nil, fmt.Errorf("scan postgres version row: %w", err)
		}
		result = append(result, pv)
	}
	return result, rows.Err()
}

// GetPostgresVersion returns a PostgreSQL version by its ID.
func (s *PostgresStore) GetPostgresVersion(ctx context.Context, id uuid.UUID) (*models.PostgresVersion, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+pvCols+` FROM postgres_versions WHERE id = $1`, id)
	pv, err := scanPostgresVersion(row)
	if err != nil {
		return nil, fmt.Errorf("get postgres version %s: %w", id, err)
	}
	return pv, nil
}

// GetPostgresVersionBySpec returns a PostgreSQL version matching the given version and variant.
func (s *PostgresStore) GetPostgresVersionBySpec(ctx context.Context, version, variant string) (*models.PostgresVersion, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+pvCols+` FROM postgres_versions WHERE LOWER(version) = LOWER($1) AND LOWER(variant) = LOWER($2)`, version, variant)
	pv, err := scanPostgresVersion(row)
	if err != nil {
		return nil, fmt.Errorf("get postgres version %s/%s: %w", version, variant, err)
	}
	return pv, nil
}

// CreatePostgresVersion inserts a new PostgreSQL version record.
func (s *PostgresStore) CreatePostgresVersion(ctx context.Context, pv *models.PostgresVersion) error {
	if pv.ID == uuid.Nil {
		pv.ID = uuid.New()
	}
	now := time.Now()
	pv.CreatedAt = now
	pv.UpdatedAt = now

	_, err := s.pool.Exec(ctx,
		`INSERT INTO postgres_versions (id, version, variant, image_tag, is_default, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		pv.ID, pv.Version, pv.Variant, pv.ImageTag, pv.IsDefault, pv.CreatedAt, pv.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("create postgres version: %w", err)
	}
	return nil
}

// UpdatePostgresVersion updates a PostgreSQL version record.
func (s *PostgresStore) UpdatePostgresVersion(ctx context.Context, pv *models.PostgresVersion) error {
	pv.UpdatedAt = time.Now()
	tag, err := s.pool.Exec(ctx,
		`UPDATE postgres_versions SET version = $1, variant = $2, image_tag = $3, is_default = $4, updated_at = $5
		 WHERE id = $6`,
		pv.Version, pv.Variant, pv.ImageTag, pv.IsDefault, pv.UpdatedAt, pv.ID,
	)
	if err != nil {
		return fmt.Errorf("update postgres version: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("postgres version %s not found", pv.ID)
	}
	return nil
}

// DeletePostgresVersion removes a PostgreSQL version by its ID.
func (s *PostgresStore) DeletePostgresVersion(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM postgres_versions WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete postgres version: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("postgres version %s not found", id)
	}
	return nil
}

// SetDefaultPostgresVersion marks a version as the default, clearing any previous default.
func (s *PostgresStore) SetDefaultPostgresVersion(ctx context.Context, id uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `UPDATE postgres_versions SET is_default = FALSE WHERE is_default = TRUE`); err != nil {
		return fmt.Errorf("clear default: %w", err)
	}
	tag, err := tx.Exec(ctx, `UPDATE postgres_versions SET is_default = TRUE, updated_at = NOW() WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("set default: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("postgres version %s not found", id)
	}
	return tx.Commit(ctx)
}

// ---------- Postgres Variants ----------

// ListPostgresVariants returns all registered variant names.
func (s *PostgresStore) ListPostgresVariants(ctx context.Context) ([]*models.PostgresVariant, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, description, created_at FROM postgres_variants ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list postgres variants: %w", err)
	}
	defer rows.Close()

	var result []*models.PostgresVariant
	for rows.Next() {
		var v models.PostgresVariant
		if err := rows.Scan(&v.ID, &v.Name, &v.Description, &v.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan postgres variant: %w", err)
		}
		result = append(result, &v)
	}
	return result, rows.Err()
}

// CreatePostgresVariant inserts a new variant.
func (s *PostgresStore) CreatePostgresVariant(ctx context.Context, v *models.PostgresVariant) error {
	if v.ID == uuid.Nil {
		v.ID = uuid.New()
	}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO postgres_variants (id, name, description) VALUES ($1, $2, $3) RETURNING created_at`,
		v.ID, v.Name, v.Description,
	).Scan(&v.CreatedAt)
	if err != nil {
		return fmt.Errorf("create postgres variant: %w", err)
	}
	return nil
}

// DeletePostgresVariant removes a variant by its ID.
func (s *PostgresStore) DeletePostgresVariant(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM postgres_variants WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete postgres variant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("postgres variant %s not found", id)
	}
	return nil
}

// ---------- Health ----------

// UpdateClusterConfigState sets the cluster config state based on health reports.
// It only updates if the current state is not paused or deleting (user-controlled states).
func (s *PostgresStore) UpdateClusterConfigState(ctx context.Context, satelliteID uuid.UUID, clusterName string, state models.ClusterState) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE cluster_configs SET state = $1, updated_at = NOW()
		 WHERE satellite_id = $2 AND name = $3 AND state NOT IN ('paused', 'deleting')`,
		state, satelliteID, clusterName,
	)
	if err != nil {
		return fmt.Errorf("update cluster config state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		log.Warn().Str("satellite_id", satelliteID.String()).Str("cluster", clusterName).Str("state", string(state)).Msg("state update matched no rows")
	}
	return nil
}

// UpsertClusterHealth inserts or updates the health record for a cluster.
func (s *PostgresStore) UpsertClusterHealth(ctx context.Context, health *models.ClusterHealth) error {
	if health.Instances == nil {
		health.Instances = json.RawMessage("[]")
	}
	health.UpdatedAt = time.Now()

	_, err := s.pool.Exec(ctx,
		`INSERT INTO cluster_health (satellite_id, cluster_name, state, instances, updated_at)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (satellite_id, cluster_name) DO UPDATE
		 SET state = EXCLUDED.state, instances = EXCLUDED.instances, updated_at = EXCLUDED.updated_at`,
		health.SatelliteID, health.ClusterName, health.State, health.Instances, health.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert cluster health: %w", err)
	}
	return nil
}

// GetClusterHealth returns the health record for a specific cluster on a satellite.
func (s *PostgresStore) GetClusterHealth(ctx context.Context, satelliteID uuid.UUID, clusterName string) (*models.ClusterHealth, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT satellite_id, cluster_name, state, instances, updated_at
		 FROM cluster_health WHERE satellite_id = $1 AND cluster_name = $2`,
		satelliteID, clusterName)
	h, err := scanClusterHealth(row)
	if err != nil {
		return nil, fmt.Errorf("get cluster health: %w", err)
	}
	return h, nil
}

// ListClusterHealth returns all cluster health records ordered by last update.
func (s *PostgresStore) ListClusterHealth(ctx context.Context) ([]*models.ClusterHealth, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT satellite_id, cluster_name, state, instances, updated_at
		 FROM cluster_health ORDER BY updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list cluster health: %w", err)
	}
	defer rows.Close()

	var result []*models.ClusterHealth
	for rows.Next() {
		h, err := scanClusterHealth(rows)
		if err != nil {
			return nil, fmt.Errorf("scan cluster health row: %w", err)
		}
		result = append(result, h)
	}
	return result, rows.Err()
}

// ---------- Events ----------

// CreateEvent inserts a new event record.
func (s *PostgresStore) CreateEvent(ctx context.Context, event *models.Event) error {
	if event.ID == uuid.Nil {
		event.ID = uuid.New()
	}
	if event.Severity == "" {
		event.Severity = "info"
	}
	event.CreatedAt = time.Now()

	_, err := s.pool.Exec(ctx,
		`INSERT INTO events (id, satellite_id, cluster_name, severity, message, source, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		event.ID, event.SatelliteID, event.ClusterName, event.Severity,
		event.Message, event.Source, event.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("create event: %w", err)
	}
	return nil
}

// ListEvents returns the most recent events, up to the given limit.
func (s *PostgresStore) ListEvents(ctx context.Context, limit int) ([]*models.Event, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, satellite_id, cluster_name, severity, message, source, created_at
		 FROM events ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	var result []*models.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan event row: %w", err)
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

// ListEventsByCluster returns the most recent events for a specific cluster.
func (s *PostgresStore) ListEventsByCluster(ctx context.Context, satelliteID uuid.UUID, clusterName string, limit int) ([]*models.Event, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, satellite_id, cluster_name, severity, message, source, created_at
		 FROM events WHERE satellite_id = $1 AND cluster_name = $2
		 ORDER BY created_at DESC LIMIT $3`,
		satelliteID, clusterName, limit)
	if err != nil {
		return nil, fmt.Errorf("list events by cluster: %w", err)
	}
	defer rows.Close()

	var result []*models.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan event row: %w", err)
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

// ── Recovery Rule Sets ──────────────────────────────────────────────────────

const recoveryRuleSetCols = `id, name, description, builtin, config, created_at, updated_at`

func scanRecoveryRuleSet(row pgx.Row) (*models.RecoveryRuleSet, error) {
	var rs models.RecoveryRuleSet
	err := row.Scan(&rs.ID, &rs.Name, &rs.Description, &rs.Builtin, &rs.Config, &rs.CreatedAt, &rs.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if rs.Config == nil {
		rs.Config = json.RawMessage("[]")
	}
	return &rs, nil
}

func (s *PostgresStore) CreateRecoveryRuleSet(ctx context.Context, rs *models.RecoveryRuleSet) error {
	if rs.ID == uuid.Nil {
		rs.ID = uuid.New()
	}
	if rs.Config == nil {
		rs.Config = json.RawMessage("[]")
	}
	now := time.Now()
	rs.CreatedAt = now
	rs.UpdatedAt = now
	_, err := s.pool.Exec(ctx,
		`INSERT INTO recovery_rule_sets (id, name, description, builtin, config, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		rs.ID, rs.Name, rs.Description, rs.Builtin, rs.Config, rs.CreatedAt, rs.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("create recovery rule set: %w", err)
	}
	return nil
}

func (s *PostgresStore) ListRecoveryRuleSets(ctx context.Context) ([]*models.RecoveryRuleSet, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+recoveryRuleSetCols+` FROM recovery_rule_sets ORDER BY builtin DESC, created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list recovery rule sets: %w", err)
	}
	defer rows.Close()
	var result []*models.RecoveryRuleSet
	for rows.Next() {
		rs, err := scanRecoveryRuleSet(rows)
		if err != nil {
			return nil, fmt.Errorf("scan recovery rule set: %w", err)
		}
		result = append(result, rs)
	}
	return result, rows.Err()
}

func (s *PostgresStore) GetRecoveryRuleSet(ctx context.Context, id uuid.UUID) (*models.RecoveryRuleSet, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+recoveryRuleSetCols+` FROM recovery_rule_sets WHERE id = $1`, id)
	return scanRecoveryRuleSet(row)
}

func (s *PostgresStore) UpdateRecoveryRuleSet(ctx context.Context, rs *models.RecoveryRuleSet) error {
	if rs.Config == nil {
		rs.Config = json.RawMessage("[]")
	}
	rs.UpdatedAt = time.Now()
	tag, err := s.pool.Exec(ctx,
		`UPDATE recovery_rule_sets SET name = $1, description = $2, config = $3, updated_at = $4
		 WHERE id = $5`,
		rs.Name, rs.Description, rs.Config, rs.UpdatedAt, rs.ID,
	)
	if err != nil {
		return fmt.Errorf("update recovery rule set: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("recovery rule set %s not found", rs.ID)
	}
	return nil
}

func (s *PostgresStore) DeleteRecoveryRuleSet(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM recovery_rule_sets WHERE id = $1 AND builtin = false`, id)
	if err != nil {
		return fmt.Errorf("delete recovery rule set: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("recovery rule set %s not found or is built-in", id)
	}
	return nil
}

// ---------- Config Versions ----------

const configVersionCols = `id, profile_id, version, config, change_summary, apply_status, created_by, created_at`

func scanConfigVersion(row pgx.Row) (*models.ConfigVersion, error) {
	var v models.ConfigVersion
	err := row.Scan(&v.ID, &v.ProfileID, &v.Version, &v.Config, &v.ChangeSummary, &v.ApplyStatus, &v.CreatedBy, &v.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// CreateConfigVersion inserts a new config version with an auto-incremented version number.
func (s *PostgresStore) CreateConfigVersion(ctx context.Context, v *models.ConfigVersion) error {
	if v.ID == uuid.Nil {
		v.ID = uuid.New()
	}
	v.CreatedAt = time.Now()
	if v.ApplyStatus == "" {
		v.ApplyStatus = "pending"
	}

	// Auto-increment version within the profile.
	var nextVersion int
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(version), 0) + 1 FROM config_versions WHERE profile_id = $1`,
		v.ProfileID).Scan(&nextVersion)
	if err != nil {
		return fmt.Errorf("compute next config version: %w", err)
	}
	v.Version = nextVersion

	_, err = s.pool.Exec(ctx,
		`INSERT INTO config_versions (id, profile_id, version, config, change_summary, apply_status, created_by, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		v.ID, v.ProfileID, v.Version, v.Config, v.ChangeSummary, v.ApplyStatus, v.CreatedBy, v.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("create config version: %w", err)
	}
	return nil
}

// ListConfigVersions returns all versions for a profile, newest first.
func (s *PostgresStore) ListConfigVersions(ctx context.Context, profileID uuid.UUID) ([]*models.ConfigVersion, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+configVersionCols+` FROM config_versions WHERE profile_id = $1 ORDER BY version DESC`,
		profileID)
	if err != nil {
		return nil, fmt.Errorf("list config versions: %w", err)
	}
	defer rows.Close()

	var result []*models.ConfigVersion
	for rows.Next() {
		v, err := scanConfigVersion(rows)
		if err != nil {
			return nil, fmt.Errorf("scan config version row: %w", err)
		}
		result = append(result, v)
	}
	return result, rows.Err()
}

// GetConfigVersion returns a specific version for a profile.
func (s *PostgresStore) GetConfigVersion(ctx context.Context, profileID uuid.UUID, version int) (*models.ConfigVersion, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+configVersionCols+` FROM config_versions WHERE profile_id = $1 AND version = $2`,
		profileID, version)
	v, err := scanConfigVersion(row)
	if err != nil {
		return nil, fmt.Errorf("get config version %d for profile %s: %w", version, profileID, err)
	}
	return v, nil
}

// ---------- Cluster Databases ----------

func (s *PostgresStore) ListClusterDatabases(ctx context.Context, clusterID uuid.UUID) ([]*models.ClusterDatabase, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, cluster_id, db_name, db_user, password, allowed_cidrs, status, error_message, created_at, updated_at
		 FROM cluster_databases WHERE cluster_id = $1 ORDER BY db_name`, clusterID)
	if err != nil {
		return nil, fmt.Errorf("list cluster databases: %w", err)
	}
	defer rows.Close()

	var result []*models.ClusterDatabase
	for rows.Next() {
		var db models.ClusterDatabase
		if err := rows.Scan(&db.ID, &db.ClusterID, &db.DBName, &db.DBUser, &db.Password, &db.AllowedCIDRs, &db.Status, &db.ErrorMessage, &db.CreatedAt, &db.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan cluster database: %w", err)
		}
		result = append(result, &db)
	}
	return result, rows.Err()
}

func (s *PostgresStore) GetClusterDatabaseByName(ctx context.Context, clusterID uuid.UUID, dbName string) (*models.ClusterDatabase, error) {
	var db models.ClusterDatabase
	err := s.pool.QueryRow(ctx,
		`SELECT id, cluster_id, db_name, db_user, password, allowed_cidrs, status, error_message, created_at, updated_at
		 FROM cluster_databases WHERE cluster_id = $1 AND db_name = $2`,
		clusterID, dbName,
	).Scan(&db.ID, &db.ClusterID, &db.DBName, &db.DBUser, &db.Password, &db.AllowedCIDRs, &db.Status, &db.ErrorMessage, &db.CreatedAt, &db.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get cluster database %q: %w", dbName, err)
	}
	return &db, nil
}

func (s *PostgresStore) CreateClusterDatabase(ctx context.Context, db *models.ClusterDatabase) error {
	if db.ID == uuid.Nil {
		db.ID = uuid.New()
	}
	now := time.Now()
	db.CreatedAt = now
	db.UpdatedAt = now
	if db.AllowedCIDRs == nil {
		db.AllowedCIDRs = []string{}
	}

	_, err := s.pool.Exec(ctx,
		`INSERT INTO cluster_databases (id, cluster_id, db_name, db_user, password, allowed_cidrs, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		db.ID, db.ClusterID, db.DBName, db.DBUser, db.Password, db.AllowedCIDRs, db.CreatedAt, db.UpdatedAt,
	)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "23505") {
			return fmt.Errorf("database %q already exists on this cluster", db.DBName)
		}
		return fmt.Errorf("create cluster database: %w", err)
	}
	return nil
}

func (s *PostgresStore) UpdateClusterDatabase(ctx context.Context, db *models.ClusterDatabase) error {
	db.UpdatedAt = time.Now()
	if db.AllowedCIDRs == nil {
		db.AllowedCIDRs = []string{}
	}

	tag, err := s.pool.Exec(ctx,
		`UPDATE cluster_databases SET db_user = $1, password = $2, allowed_cidrs = $3, updated_at = $4
		 WHERE id = $5`,
		db.DBUser, db.Password, db.AllowedCIDRs, db.UpdatedAt, db.ID,
	)
	if err != nil {
		return fmt.Errorf("update cluster database: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("cluster database %s not found", db.ID)
	}
	return nil
}

func (s *PostgresStore) UpdateClusterDatabaseStatus(ctx context.Context, id uuid.UUID, status, errorMsg string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE cluster_databases SET status = $1, error_message = $2, updated_at = NOW() WHERE id = $3`,
		status, errorMsg, id,
	)
	if err != nil {
		return fmt.Errorf("update cluster database status: %w", err)
	}
	return nil
}

func (s *PostgresStore) DeleteClusterDatabase(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM cluster_databases WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete cluster database: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("cluster database %s not found", id)
	}
	return nil
}

// ---------- PG Parameter Classifications ----------

// ListPgParamClassifications returns all parameter classifications.
func (s *PostgresStore) ListPgParamClassifications(ctx context.Context) ([]*models.PgParamClassification, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT name, restart_mode, description, pg_context, created_at, updated_at
		 FROM pg_param_classifications ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list pg param classifications: %w", err)
	}
	defer rows.Close()

	var result []*models.PgParamClassification
	for rows.Next() {
		var p models.PgParamClassification
		if err := rows.Scan(&p.Name, &p.RestartMode, &p.Description, &p.PgContext, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan pg param classification: %w", err)
		}
		result = append(result, &p)
	}
	return result, rows.Err()
}

// UpsertPgParamClassification creates or updates a parameter classification.
func (s *PostgresStore) UpsertPgParamClassification(ctx context.Context, p *models.PgParamClassification) error {
	if p.Name == "" {
		return fmt.Errorf("parameter name is required")
	}
	if p.RestartMode == "" {
		p.RestartMode = "sequential"
	}
	now := time.Now()
	_, err := s.pool.Exec(ctx,
		`INSERT INTO pg_param_classifications (name, restart_mode, description, pg_context, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $5)
		 ON CONFLICT (name) DO UPDATE SET restart_mode = $2, description = $3, pg_context = $4, updated_at = $5`,
		p.Name, p.RestartMode, p.Description, p.PgContext, now,
	)
	if err != nil {
		return fmt.Errorf("upsert pg param classification: %w", err)
	}
	p.UpdatedAt = now
	return nil
}

// DeletePgParamClassification removes a parameter classification (reverts to default sequential).
func (s *PostgresStore) DeletePgParamClassification(ctx context.Context, name string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM pg_param_classifications WHERE name = $1`, name)
	if err != nil {
		return fmt.Errorf("delete pg param classification: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("parameter %q not found", name)
	}
	return nil
}

// Compile-time check that PostgresStore implements Store.
var _ Store = (*PostgresStore)(nil)
