-- Restore operations: tracks restore attempts per satellite cluster.
CREATE TABLE IF NOT EXISTS restore_operations (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    satellite_id    UUID NOT NULL REFERENCES satellites(id),
    cluster_name    TEXT NOT NULL,
    backup_id       UUID NOT NULL REFERENCES backup_inventory(id),
    restore_type    TEXT NOT NULL,
    target_time     TIMESTAMPTZ,
    target_database TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'pending',
    error_message   TEXT NOT NULL DEFAULT '',
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
