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

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

// scanSatellite scans a satellite row into a Satellite struct, handling JSON unmarshaling for labels.
func scanSatellite(row pgx.Row) (*models.Satellite, error) {
	var sat models.Satellite
	var labelsJSON []byte
	err := row.Scan(
		&sat.ID,
		&sat.Hostname,
		&sat.K8sClusterName,
		&sat.Region,
		&labelsJSON,
		&sat.State,
		&sat.AuthTokenHash,
		&sat.TempTokenHash,
		&sat.GroupID,
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
	return &sat, nil
}

// scanGroup scans an edge_group row into an EdgeGroup struct.
func scanGroup(row pgx.Row) (*models.EdgeGroup, error) {
	var g models.EdgeGroup
	var labelsJSON []byte
	err := row.Scan(
		&g.ID,
		&g.Name,
		&g.Description,
		&labelsJSON,
		&g.CreatedAt,
		&g.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if labelsJSON != nil {
		if err := json.Unmarshal(labelsJSON, &g.Labels); err != nil {
			return nil, fmt.Errorf("unmarshal group labels: %w", err)
		}
	}
	if g.Labels == nil {
		g.Labels = make(map[string]string)
	}
	return &g, nil
}

// scanClusterConfig scans a cluster_configs row into a ClusterConfig struct.
func scanClusterConfig(row pgx.Row) (*models.ClusterConfig, error) {
	var cfg models.ClusterConfig
	err := row.Scan(
		&cfg.ID,
		&cfg.Name,
		&cfg.Namespace,
		&cfg.SatelliteID,
		&cfg.GroupID,
		&cfg.Config,
		&cfg.ConfigVersion,
		&cfg.State,
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
		`INSERT INTO satellites (id, hostname, k8s_cluster_name, region, labels, state, auth_token_hash, temp_token_hash, group_id, last_heartbeat, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		sat.ID, sat.Hostname, sat.K8sClusterName, sat.Region, labelsJSON, sat.State,
		sat.AuthTokenHash, sat.TempTokenHash, sat.GroupID, sat.LastHeartbeat,
		sat.CreatedAt, sat.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("create satellite: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetSatellite(ctx context.Context, id uuid.UUID) (*models.Satellite, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, hostname, k8s_cluster_name, region, labels, state, auth_token_hash, temp_token_hash, group_id, last_heartbeat, created_at, updated_at
		 FROM satellites WHERE id = $1`, id)
	sat, err := scanSatellite(row)
	if err != nil {
		return nil, fmt.Errorf("get satellite %s: %w", id, err)
	}
	return sat, nil
}

func (s *PostgresStore) GetSatelliteByToken(ctx context.Context, tokenHash string) (*models.Satellite, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, hostname, k8s_cluster_name, region, labels, state, auth_token_hash, temp_token_hash, group_id, last_heartbeat, created_at, updated_at
		 FROM satellites WHERE auth_token_hash = $1`, tokenHash)
	sat, err := scanSatellite(row)
	if err != nil {
		return nil, fmt.Errorf("get satellite by token: %w", err)
	}
	return sat, nil
}

func (s *PostgresStore) ListSatellites(ctx context.Context) ([]*models.Satellite, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, hostname, k8s_cluster_name, region, labels, state, auth_token_hash, temp_token_hash, group_id, last_heartbeat, created_at, updated_at
		 FROM satellites ORDER BY created_at DESC`)
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
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate satellites: %w", err)
	}
	return result, nil
}

func (s *PostgresStore) UpdateSatelliteState(ctx context.Context, id uuid.UUID, state models.SatelliteState) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE satellites SET state = $1, updated_at = NOW() WHERE id = $2`,
		state, id)
	if err != nil {
		return fmt.Errorf("update satellite state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("satellite %s not found", id)
	}
	return nil
}

func (s *PostgresStore) SetSatelliteAuthToken(ctx context.Context, id uuid.UUID, tokenHash string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE satellites SET auth_token_hash = $1, updated_at = NOW() WHERE id = $2`,
		tokenHash, id)
	if err != nil {
		return fmt.Errorf("set satellite auth token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("satellite %s not found", id)
	}
	return nil
}

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

// ---------- Groups ----------

func (s *PostgresStore) CreateGroup(ctx context.Context, group *models.EdgeGroup) error {
	if group.ID == uuid.Nil {
		group.ID = uuid.New()
	}
	if group.Labels == nil {
		group.Labels = make(map[string]string)
	}
	labelsJSON, err := json.Marshal(group.Labels)
	if err != nil {
		return fmt.Errorf("marshal group labels: %w", err)
	}
	now := time.Now()
	group.CreatedAt = now
	group.UpdatedAt = now

	_, err = s.pool.Exec(ctx,
		`INSERT INTO edge_groups (id, name, description, labels, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		group.ID, group.Name, group.Description, labelsJSON,
		group.CreatedAt, group.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("create group: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetGroup(ctx context.Context, id uuid.UUID) (*models.EdgeGroup, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, name, description, labels, created_at, updated_at
		 FROM edge_groups WHERE id = $1`, id)
	g, err := scanGroup(row)
	if err != nil {
		return nil, fmt.Errorf("get group %s: %w", id, err)
	}
	return g, nil
}

func (s *PostgresStore) ListGroups(ctx context.Context) ([]*models.EdgeGroup, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, description, labels, created_at, updated_at
		 FROM edge_groups ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	defer rows.Close()

	var result []*models.EdgeGroup
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, fmt.Errorf("scan group row: %w", err)
		}
		result = append(result, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate groups: %w", err)
	}
	return result, nil
}

func (s *PostgresStore) AssignSatelliteToGroup(ctx context.Context, satelliteID, groupID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE satellites SET group_id = $1, updated_at = NOW() WHERE id = $2`,
		groupID, satelliteID)
	if err != nil {
		return fmt.Errorf("assign satellite to group: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("satellite %s not found", satelliteID)
	}
	return nil
}

// ---------- Cluster Configs ----------

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
		`INSERT INTO cluster_configs (id, name, namespace, satellite_id, group_id, config, config_version, state, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		cfg.ID, cfg.Name, cfg.Namespace, cfg.SatelliteID, cfg.GroupID,
		cfg.Config, cfg.ConfigVersion, cfg.State, cfg.CreatedAt, cfg.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("create cluster config: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetClusterConfig(ctx context.Context, id uuid.UUID) (*models.ClusterConfig, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, name, namespace, satellite_id, group_id, config, config_version, state, created_at, updated_at
		 FROM cluster_configs WHERE id = $1`, id)
	cfg, err := scanClusterConfig(row)
	if err != nil {
		return nil, fmt.Errorf("get cluster config %s: %w", id, err)
	}
	return cfg, nil
}

func (s *PostgresStore) ListClusterConfigs(ctx context.Context) ([]*models.ClusterConfig, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, namespace, satellite_id, group_id, config, config_version, state, created_at, updated_at
		 FROM cluster_configs ORDER BY created_at DESC`)
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
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cluster configs: %w", err)
	}
	return result, nil
}

func (s *PostgresStore) UpdateClusterConfig(ctx context.Context, cfg *models.ClusterConfig) error {
	if cfg.Config == nil {
		cfg.Config = json.RawMessage("{}")
	}
	cfg.UpdatedAt = time.Now()

	tag, err := s.pool.Exec(ctx,
		`UPDATE cluster_configs SET name = $1, namespace = $2, satellite_id = $3, group_id = $4,
		 config = $5, config_version = config_version + 1, state = $6, updated_at = $7
		 WHERE id = $8`,
		cfg.Name, cfg.Namespace, cfg.SatelliteID, cfg.GroupID,
		cfg.Config, cfg.State, cfg.UpdatedAt, cfg.ID,
	)
	if err != nil {
		return fmt.Errorf("update cluster config: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("cluster config %s not found", cfg.ID)
	}
	return nil
}

func (s *PostgresStore) DeleteClusterConfig(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM cluster_configs WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete cluster config: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("cluster config %s not found", id)
	}
	return nil
}

func (s *PostgresStore) GetClusterConfigsBySatellite(ctx context.Context, satelliteID uuid.UUID) ([]*models.ClusterConfig, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, namespace, satellite_id, group_id, config, config_version, state, created_at, updated_at
		 FROM cluster_configs WHERE satellite_id = $1 ORDER BY created_at DESC`, satelliteID)
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
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cluster configs: %w", err)
	}
	return result, nil
}

func (s *PostgresStore) GetClusterConfigsByGroup(ctx context.Context, groupID uuid.UUID) ([]*models.ClusterConfig, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, namespace, satellite_id, group_id, config, config_version, state, created_at, updated_at
		 FROM cluster_configs WHERE group_id = $1 ORDER BY created_at DESC`, groupID)
	if err != nil {
		return nil, fmt.Errorf("get cluster configs by group: %w", err)
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
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cluster configs: %w", err)
	}
	return result, nil
}

// ---------- Health ----------

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
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cluster health: %w", err)
	}
	return result, nil
}

// ---------- Events ----------

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
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}
	return result, nil
}

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
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}
	return result, nil
}

// Compile-time check that PostgresStore implements Store.
var _ Store = (*PostgresStore)(nil)
