-- Backup inventory: tracks completed/failed backups per satellite cluster.
CREATE TABLE IF NOT EXISTS backup_inventory (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    satellite_id   UUID NOT NULL REFERENCES satellites(id),
    cluster_name   TEXT NOT NULL,
    backup_profile_id UUID,
    backup_type    TEXT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'running',
    started_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at   TIMESTAMPTZ,
    size_bytes     BIGINT NOT NULL DEFAULT 0,
    backup_path    TEXT NOT NULL DEFAULT '',
    pg_version     TEXT NOT NULL DEFAULT '',
    wal_start_lsn  TEXT NOT NULL DEFAULT '',
    wal_end_lsn    TEXT NOT NULL DEFAULT '',
    error_message  TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_backup_inv_sat_cluster ON backup_inventory(satellite_id, cluster_name);
CREATE INDEX IF NOT EXISTS idx_backup_inv_started ON backup_inventory(started_at DESC);
