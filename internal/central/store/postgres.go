package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-swarm/pg-swarm/internal/shared/models"
)

// PostgresStore implements the Store interface using a PostgreSQL connection pool.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore returns a new PostgresStore backed by the given connection pool.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

// Column lists used across queries (keep in sync with scanners below).
const (
	satCols = `id, hostname, k8s_cluster_name, region, labels, storage_classes, state, auth_token_hash, temp_token_hash, last_heartbeat, created_at, updated_at`
	cfgCols = `id, name, namespace, satellite_id, profile_id, deployment_rule_id, config, config_version, state, paused, created_at, updated_at`
	ruleCols = `id, name, profile_id, label_selector, namespace, cluster_name, created_at, updated_at`
)

// scanSatellite scans a satellite row into a Satellite struct.
func scanSatellite(row pgx.Row) (*models.Satellite, error) {
	var sat models.Satellite
	var labelsJSON []byte
	var scJSON []byte
	err := row.Scan(
		&sat.ID,
		&sat.Hostname,
		&sat.K8sClusterName,
		&sat.Region,
		&labelsJSON,
		&scJSON,
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
		&p.Locked,
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

	_, err = s.pool.Exec(ctx,
		`INSERT INTO satellites (id, hostname, k8s_cluster_name, region, labels, state, auth_token_hash, temp_token_hash, last_heartbeat, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		sat.ID, sat.Hostname, sat.K8sClusterName, sat.Region, labelsJSON, sat.State,
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
	rows, err := s.pool.Query(ctx, `SELECT `+satCols+` FROM satellites ORDER BY created_at DESC`)
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
		`SELECT `+satCols+` FROM satellites WHERE labels @> $1::jsonb ORDER BY created_at DESC`, selectorJSON)
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
		`INSERT INTO cluster_configs (id, name, namespace, satellite_id, profile_id, deployment_rule_id, config, config_version, state, paused, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		cfg.ID, cfg.Name, cfg.Namespace, cfg.SatelliteID, cfg.ProfileID,
		cfg.DeploymentRuleID, cfg.Config, cfg.ConfigVersion, cfg.State, cfg.Paused, cfg.CreatedAt, cfg.UpdatedAt,
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
func (s *PostgresStore) UpdateClusterConfig(ctx context.Context, cfg *models.ClusterConfig) error {
	if cfg.Config == nil {
		cfg.Config = json.RawMessage("{}")
	}
	cfg.UpdatedAt = time.Now()

	tag, err := s.pool.Exec(ctx,
		`UPDATE cluster_configs SET name = $1, namespace = $2, satellite_id = $3,
		 profile_id = $4, deployment_rule_id = $5, config = $6, config_version = config_version + 1, state = $7, paused = $8, updated_at = $9
		 WHERE id = $10`,
		cfg.Name, cfg.Namespace, cfg.SatelliteID,
		cfg.ProfileID, cfg.DeploymentRuleID, cfg.Config, cfg.State, cfg.Paused, cfg.UpdatedAt, cfg.ID,
	)
	if err != nil {
		return fmt.Errorf("update cluster config: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("cluster config %s not found", cfg.ID)
	}
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
		`INSERT INTO cluster_profiles (id, name, description, config, locked, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		p.ID, p.Name, p.Description, p.Config, p.Locked, p.CreatedAt, p.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("create profile: %w", err)
	}
	return nil
}

// GetProfile returns a cluster profile by its ID.
func (s *PostgresStore) GetProfile(ctx context.Context, id uuid.UUID) (*models.ClusterProfile, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, name, description, config, locked, created_at, updated_at
		 FROM cluster_profiles WHERE id = $1`, id)
	p, err := scanProfile(row)
	if err != nil {
		return nil, fmt.Errorf("get profile %s: %w", id, err)
	}
	return p, nil
}

// ListProfiles returns all cluster profiles ordered by creation time.
func (s *PostgresStore) ListProfiles(ctx context.Context) ([]*models.ClusterProfile, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, description, config, locked, created_at, updated_at
		 FROM cluster_profiles ORDER BY created_at DESC`)
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

// UpdateProfile updates a cluster profile if it is not locked.
func (s *PostgresStore) UpdateProfile(ctx context.Context, p *models.ClusterProfile) error {
	if p.Config == nil {
		p.Config = json.RawMessage("{}")
	}
	p.UpdatedAt = time.Now()

	tag, err := s.pool.Exec(ctx,
		`UPDATE cluster_profiles SET name = $1, description = $2, config = $3, updated_at = $4
		 WHERE id = $5 AND locked = FALSE`,
		p.Name, p.Description, p.Config, p.UpdatedAt, p.ID,
	)
	if err != nil {
		return fmt.Errorf("update profile: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("profile %s not found or is locked", p.ID)
	}
	return nil
}

// DeleteProfile removes a cluster profile if it is not locked.
func (s *PostgresStore) DeleteProfile(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM cluster_profiles WHERE id = $1 AND locked = FALSE`, id)
	if err != nil {
		return fmt.Errorf("delete profile: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("profile %s not found or is locked", id)
	}
	return nil
}

// LockProfile marks a profile as locked, preventing further edits or deletion.
func (s *PostgresStore) LockProfile(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE cluster_profiles SET locked = TRUE, updated_at = NOW() WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("lock profile: %w", err)
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
	_, err := s.pool.Exec(ctx,
		`UPDATE cluster_configs SET state = $1, updated_at = NOW()
		 WHERE satellite_id = $2 AND name = $3 AND state NOT IN ('paused', 'deleting')`,
		state, satelliteID, clusterName,
	)
	if err != nil {
		return fmt.Errorf("update cluster config state: %w", err)
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

// ---------- Backup Profiles ----------

const backupProfileCols = `id, name, description, config, created_at, updated_at`

func scanBackupProfile(row pgx.Row) (*models.BackupProfile, error) {
	var r models.BackupProfile
	err := row.Scan(&r.ID, &r.Name, &r.Description, &r.Config, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if r.Config == nil {
		r.Config = json.RawMessage("{}")
	}
	return &r, nil
}

// CreateBackupProfile inserts a new backup profile.
func (s *PostgresStore) CreateBackupProfile(ctx context.Context, rule *models.BackupProfile) error {
	if rule.ID == uuid.Nil {
		rule.ID = uuid.New()
	}
	if rule.Config == nil {
		rule.Config = json.RawMessage("{}")
	}
	now := time.Now()
	rule.CreatedAt = now
	rule.UpdatedAt = now

	_, err := s.pool.Exec(ctx,
		`INSERT INTO backup_profiles (id, name, description, config, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		rule.ID, rule.Name, rule.Description, rule.Config, rule.CreatedAt, rule.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("create backup profile: %w", err)
	}
	return nil
}

// GetBackupProfile returns a backup profile by its ID.
func (s *PostgresStore) GetBackupProfile(ctx context.Context, id uuid.UUID) (*models.BackupProfile, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+backupProfileCols+` FROM backup_profiles WHERE id = $1`, id)
	r, err := scanBackupProfile(row)
	if err != nil {
		return nil, fmt.Errorf("get backup profile %s: %w", id, err)
	}
	return r, nil
}

// ListBackupProfiles returns all backup profiles ordered by creation time.
func (s *PostgresStore) ListBackupProfiles(ctx context.Context) ([]*models.BackupProfile, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+backupProfileCols+` FROM backup_profiles ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list backup profiles: %w", err)
	}
	defer rows.Close()

	var result []*models.BackupProfile
	for rows.Next() {
		r, err := scanBackupProfile(rows)
		if err != nil {
			return nil, fmt.Errorf("scan backup profile row: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// UpdateBackupProfile updates a backup profile.
func (s *PostgresStore) UpdateBackupProfile(ctx context.Context, rule *models.BackupProfile) error {
	if rule.Config == nil {
		rule.Config = json.RawMessage("{}")
	}
	rule.UpdatedAt = time.Now()

	tag, err := s.pool.Exec(ctx,
		`UPDATE backup_profiles SET name = $1, description = $2, config = $3, updated_at = $4
		 WHERE id = $5`,
		rule.Name, rule.Description, rule.Config, rule.UpdatedAt, rule.ID,
	)
	if err != nil {
		return fmt.Errorf("update backup profile: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("backup profile %s not found", rule.ID)
	}
	return nil
}

// DeleteBackupProfile removes a backup profile by its ID.
func (s *PostgresStore) DeleteBackupProfile(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM backup_profiles WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete backup profile: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("backup profile %s not found", id)
	}
	return nil
}

// AttachBackupProfileToProfile links a backup profile to a profile via the join table.
func (s *PostgresStore) AttachBackupProfileToProfile(ctx context.Context, profileID, backupProfileID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO profile_backup_profiles (profile_id, backup_profile_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		profileID, backupProfileID,
	)
	if err != nil {
		return fmt.Errorf("attach backup profile to profile: %w", err)
	}
	return nil
}

// DetachBackupProfileFromProfile removes a backup profile from a profile.
func (s *PostgresStore) DetachBackupProfileFromProfile(ctx context.Context, profileID, backupProfileID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM profile_backup_profiles WHERE profile_id = $1 AND backup_profile_id = $2`,
		profileID, backupProfileID,
	)
	if err != nil {
		return fmt.Errorf("detach backup profile from profile: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("backup profile %s not attached to profile %s", backupProfileID, profileID)
	}
	return nil
}

// ListBackupProfilesForProfile returns all backup profiles attached to a profile.
func (s *PostgresStore) ListBackupProfilesForProfile(ctx context.Context, profileID uuid.UUID) ([]*models.BackupProfile, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+backupProfileCols+` FROM backup_profiles
		 WHERE id IN (SELECT backup_profile_id FROM profile_backup_profiles WHERE profile_id = $1)
		 ORDER BY created_at DESC`, profileID)
	if err != nil {
		return nil, fmt.Errorf("list backup profiles for profile: %w", err)
	}
	defer rows.Close()

	var result []*models.BackupProfile
	for rows.Next() {
		r, err := scanBackupProfile(rows)
		if err != nil {
			return nil, fmt.Errorf("scan backup profile row: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// ---------- Backup Inventory ----------

const backupInvCols = `id, satellite_id, cluster_name, backup_profile_id, backup_type, status, started_at, completed_at, size_bytes, backup_path, pg_version, wal_start_lsn, wal_end_lsn, error_message, created_at`

func scanBackupInventory(row pgx.Row) (*models.BackupInventory, error) {
	var inv models.BackupInventory
	err := row.Scan(
		&inv.ID, &inv.SatelliteID, &inv.ClusterName, &inv.BackupProfileID,
		&inv.BackupType, &inv.Status, &inv.StartedAt, &inv.CompletedAt,
		&inv.SizeBytes, &inv.BackupPath, &inv.PgVersion,
		&inv.WalStartLSN, &inv.WalEndLSN, &inv.ErrorMessage, &inv.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &inv, nil
}

// CreateBackupInventory inserts a new backup inventory record.
func (s *PostgresStore) CreateBackupInventory(ctx context.Context, inv *models.BackupInventory) error {
	if inv.ID == uuid.Nil {
		inv.ID = uuid.New()
	}
	if inv.Status == "" {
		inv.Status = "running"
	}
	now := time.Now()
	if inv.StartedAt.IsZero() {
		inv.StartedAt = now
	}
	inv.CreatedAt = now

	_, err := s.pool.Exec(ctx,
		`INSERT INTO backup_inventory (id, satellite_id, cluster_name, backup_profile_id, backup_type, status, started_at, completed_at, size_bytes, backup_path, pg_version, wal_start_lsn, wal_end_lsn, error_message, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		inv.ID, inv.SatelliteID, inv.ClusterName, inv.BackupProfileID,
		inv.BackupType, inv.Status, inv.StartedAt, inv.CompletedAt,
		inv.SizeBytes, inv.BackupPath, inv.PgVersion,
		inv.WalStartLSN, inv.WalEndLSN, inv.ErrorMessage, inv.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("create backup inventory: %w", err)
	}
	return nil
}

// UpdateBackupInventory updates an existing backup inventory record.
func (s *PostgresStore) UpdateBackupInventory(ctx context.Context, inv *models.BackupInventory) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE backup_inventory SET status = $1, completed_at = $2, size_bytes = $3, backup_path = $4,
		 pg_version = $5, wal_start_lsn = $6, wal_end_lsn = $7, error_message = $8
		 WHERE id = $9`,
		inv.Status, inv.CompletedAt, inv.SizeBytes, inv.BackupPath,
		inv.PgVersion, inv.WalStartLSN, inv.WalEndLSN, inv.ErrorMessage, inv.ID,
	)
	if err != nil {
		return fmt.Errorf("update backup inventory: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("backup inventory %s not found", inv.ID)
	}
	return nil
}

// ListBackupInventory returns backup records for a satellite cluster.
func (s *PostgresStore) ListBackupInventory(ctx context.Context, satelliteID uuid.UUID, clusterName string) ([]*models.BackupInventory, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+backupInvCols+` FROM backup_inventory
		 WHERE satellite_id = $1 AND cluster_name = $2
		 ORDER BY started_at DESC`,
		satelliteID, clusterName)
	if err != nil {
		return nil, fmt.Errorf("list backup inventory: %w", err)
	}
	defer rows.Close()

	var result []*models.BackupInventory
	for rows.Next() {
		inv, err := scanBackupInventory(rows)
		if err != nil {
			return nil, fmt.Errorf("scan backup inventory row: %w", err)
		}
		result = append(result, inv)
	}
	return result, rows.Err()
}

// GetBackupInventory returns a backup inventory record by its ID.
func (s *PostgresStore) GetBackupInventory(ctx context.Context, id uuid.UUID) (*models.BackupInventory, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+backupInvCols+` FROM backup_inventory WHERE id = $1`, id)
	inv, err := scanBackupInventory(row)
	if err != nil {
		return nil, fmt.Errorf("get backup inventory %s: %w", id, err)
	}
	return inv, nil
}

// ---------- Restore Operations ----------

const restoreOpCols = `id, satellite_id, cluster_name, backup_id, restore_type, target_time, target_database, status, error_message, started_at, completed_at, created_at`

func scanRestoreOperation(row pgx.Row) (*models.RestoreOperation, error) {
	var op models.RestoreOperation
	err := row.Scan(
		&op.ID, &op.SatelliteID, &op.ClusterName, &op.BackupID,
		&op.RestoreType, &op.TargetTime, &op.TargetDatabase,
		&op.Status, &op.ErrorMessage, &op.StartedAt, &op.CompletedAt, &op.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &op, nil
}

// CreateRestoreOperation inserts a new restore operation record.
func (s *PostgresStore) CreateRestoreOperation(ctx context.Context, op *models.RestoreOperation) error {
	if op.ID == uuid.Nil {
		op.ID = uuid.New()
	}
	if op.Status == "" {
		op.Status = "pending"
	}
	op.CreatedAt = time.Now()

	_, err := s.pool.Exec(ctx,
		`INSERT INTO restore_operations (id, satellite_id, cluster_name, backup_id, restore_type, target_time, target_database, status, error_message, started_at, completed_at, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		op.ID, op.SatelliteID, op.ClusterName, op.BackupID,
		op.RestoreType, op.TargetTime, op.TargetDatabase,
		op.Status, op.ErrorMessage, op.StartedAt, op.CompletedAt, op.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("create restore operation: %w", err)
	}
	return nil
}

// UpdateRestoreOperation updates an existing restore operation.
func (s *PostgresStore) UpdateRestoreOperation(ctx context.Context, op *models.RestoreOperation) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE restore_operations SET status = $1, error_message = $2, started_at = $3, completed_at = $4
		 WHERE id = $5`,
		op.Status, op.ErrorMessage, op.StartedAt, op.CompletedAt, op.ID,
	)
	if err != nil {
		return fmt.Errorf("update restore operation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("restore operation %s not found", op.ID)
	}
	return nil
}

// GetRestoreOperation returns a restore operation by its ID.
func (s *PostgresStore) GetRestoreOperation(ctx context.Context, id uuid.UUID) (*models.RestoreOperation, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+restoreOpCols+` FROM restore_operations WHERE id = $1`, id)
	op, err := scanRestoreOperation(row)
	if err != nil {
		return nil, fmt.Errorf("get restore operation %s: %w", id, err)
	}
	return op, nil
}

// ListRestoreOperations returns restore operations for a satellite cluster.
func (s *PostgresStore) ListRestoreOperations(ctx context.Context, satelliteID uuid.UUID, clusterName string) ([]*models.RestoreOperation, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+restoreOpCols+` FROM restore_operations
		 WHERE satellite_id = $1 AND cluster_name = $2
		 ORDER BY created_at DESC`,
		satelliteID, clusterName)
	if err != nil {
		return nil, fmt.Errorf("list restore operations: %w", err)
	}
	defer rows.Close()

	var result []*models.RestoreOperation
	for rows.Next() {
		op, err := scanRestoreOperation(rows)
		if err != nil {
			return nil, fmt.Errorf("scan restore operation row: %w", err)
		}
		result = append(result, op)
	}
	return result, rows.Err()
}

// Compile-time check that PostgresStore implements Store.
var _ Store = (*PostgresStore)(nil)
