CREATE TABLE IF NOT EXISTS backup_inventory (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    satellite_id      UUID NOT NULL REFERENCES satellites(id),
    cluster_name      TEXT NOT NULL,
    backup_type       TEXT NOT NULL CHECK (backup_type IN ('base', 'incremental', 'logical')),
    status            TEXT NOT NULL CHECK (status IN ('pending', 'running', 'completed', 'failed', 'skipped')),
    started_at        TIMESTAMPTZ NOT NULL,
    completed_at      TIMESTAMPTZ,
    size_bytes        BIGINT NOT NULL DEFAULT 0,
    backup_path       TEXT NOT NULL DEFAULT '',
    pg_version        TEXT NOT NULL DEFAULT '',
    wal_start_lsn     TEXT NOT NULL DEFAULT '',
    wal_end_lsn       TEXT NOT NULL DEFAULT '',
    databases         TEXT[] NOT NULL DEFAULT '{}',
    error_message     TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_backup_inventory_cluster
    ON backup_inventory (satellite_id, cluster_name, started_at DESC);

CREATE TABLE IF NOT EXISTS restore_operations (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    satellite_id    UUID NOT NULL REFERENCES satellites(id),
    cluster_name    TEXT NOT NULL,
    backup_id       UUID REFERENCES backup_inventory(id),
    restore_type    TEXT NOT NULL CHECK (restore_type IN ('logical', 'pitr')),
    restore_mode    TEXT CHECK (restore_mode IN ('in_place', 'new_cluster')),
    target_time     TIMESTAMPTZ,
    target_database TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL CHECK (status IN ('pending', 'running', 'completed', 'failed')),
    error_message   TEXT NOT NULL DEFAULT '',
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
