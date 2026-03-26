CREATE TABLE IF NOT EXISTS backup_stores (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT UNIQUE NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    store_type  TEXT NOT NULL CHECK (store_type IN ('gcs', 'sftp')),
    config      JSONB NOT NULL DEFAULT '{}',
    credentials BYTEA DEFAULT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
