package backup

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
)

// HealthStatus describes the health of the PostgreSQL instance for backup purposes.
type HealthStatus struct {
	Healthy             bool
	WalReceiverStatus   string // "streaming", "stopped", "catchup", "unknown"
	ReplicationLagBytes int64
	Reason              string
}

const (
	// maxHealthyLagBytes is the maximum replication lag (64 MB) before a replica
	// is considered unhealthy for backup purposes.
	maxHealthyLagBytes = 64 * 1024 * 1024
)

// waitForClusterReady polls the local PostgreSQL for readiness before activating
// backup responsibilities. Returns true if ready, false on timeout (proceeds
// anyway — degraded backup > no backup).
func (s *Sidecar) waitForClusterReady(ctx context.Context, role Role) bool {
	timeout := 5 * time.Minute
	interval := 5 * time.Second
	deadline := time.Now().Add(timeout)

	log.Info().Str("role", role.String()).Msg("waiting for cluster readiness")

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		default:
		}

		ready, err := s.isClusterReady(ctx, role)
		if err == nil && ready {
			log.Info().Str("role", role.String()).Msg("cluster ready for backups")
			return true
		}
		if err != nil {
			log.Debug().Err(err).Msg("cluster readiness check failed")
		}

		select {
		case <-ctx.Done():
			return false
		case <-time.After(interval):
		}
	}

	log.Warn().Str("role", role.String()).Msg("cluster readiness timed out — proceeding with degraded backups")
	return false
}

// isClusterReady performs a single readiness check based on role.
func (s *Sidecar) isClusterReady(ctx context.Context, role Role) (bool, error) {
	connStr := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=postgres sslmode=disable",
		s.cfg.PGHost, s.cfg.PGPort, s.cfg.PGUser, s.cfg.PGPassword)

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		return false, fmt.Errorf("open: %w", err)
	}
	defer db.Close()

	switch role {
	case RoleReplica:
		// Wait for WAL receiver to be streaming
		var status sql.NullString
		err := db.QueryRowContext(ctx, "SELECT status FROM pg_stat_wal_receiver LIMIT 1").Scan(&status)
		if err != nil {
			return false, fmt.Errorf("pg_stat_wal_receiver: %w", err)
		}
		return status.Valid && status.String == "streaming", nil

	case RolePrimary:
		// Confirm WAL generation is active
		var lsn string
		err := db.QueryRowContext(ctx, "SELECT pg_current_wal_lsn()::text").Scan(&lsn)
		if err != nil {
			return false, fmt.Errorf("pg_current_wal_lsn: %w", err)
		}
		return lsn != "", nil

	default:
		return false, fmt.Errorf("unknown role")
	}
}

// checkHealth performs a health check before running a backup.
func (s *Sidecar) checkHealth(ctx context.Context) HealthStatus {
	s.mu.RLock()
	role := s.role
	s.mu.RUnlock()

	connStr := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=postgres sslmode=disable",
		s.cfg.PGHost, s.cfg.PGPort, s.cfg.PGUser, s.cfg.PGPassword)

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		return HealthStatus{Healthy: false, WalReceiverStatus: "unknown", Reason: fmt.Sprintf("open: %v", err)}
	}
	defer db.Close()

	switch role {
	case RoleReplica:
		return s.checkReplicaHealth(ctx, db)
	case RolePrimary:
		return s.checkPrimaryHealth(ctx, db)
	default:
		return HealthStatus{Healthy: false, WalReceiverStatus: "unknown", Reason: "unknown role"}
	}
}

// checkReplicaHealth queries pg_stat_wal_receiver for status and replication lag.
func (s *Sidecar) checkReplicaHealth(ctx context.Context, db *sql.DB) HealthStatus {
	hs := HealthStatus{WalReceiverStatus: "unknown"}

	var status sql.NullString
	err := db.QueryRowContext(ctx, "SELECT status FROM pg_stat_wal_receiver LIMIT 1").Scan(&status)
	if err != nil {
		hs.Reason = fmt.Sprintf("pg_stat_wal_receiver query failed: %v", err)
		return hs
	}
	if status.Valid {
		hs.WalReceiverStatus = status.String
	}

	// Compute replication lag via pg_wal_lsn_diff
	var lagBytes sql.NullInt64
	err = db.QueryRowContext(ctx,
		`SELECT pg_wal_lsn_diff(pg_last_wal_receive_lsn(), pg_last_wal_replay_lsn())::bigint`).Scan(&lagBytes)
	if err == nil && lagBytes.Valid {
		hs.ReplicationLagBytes = lagBytes.Int64
	}

	// Healthy if streaming and lag < 64MB
	if hs.WalReceiverStatus == "streaming" && hs.ReplicationLagBytes < maxHealthyLagBytes {
		hs.Healthy = true
	} else {
		hs.Reason = fmt.Sprintf("status=%s lag=%d bytes", hs.WalReceiverStatus, hs.ReplicationLagBytes)
	}

	return hs
}

// checkPrimaryHealth checks primary WAL sender count (informational, always healthy).
func (s *Sidecar) checkPrimaryHealth(ctx context.Context, db *sql.DB) HealthStatus {
	hs := HealthStatus{Healthy: true, WalReceiverStatus: "n/a"}

	var senderCount int
	err := db.QueryRowContext(ctx, "SELECT count(*) FROM pg_stat_replication").Scan(&senderCount)
	if err != nil {
		hs.Reason = fmt.Sprintf("pg_stat_replication query failed: %v", err)
		// Primary is still healthy for backup purposes
	} else {
		hs.Reason = fmt.Sprintf("walsender_count=%d", senderCount)
	}

	return hs
}
