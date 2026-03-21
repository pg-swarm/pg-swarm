CREATE TABLE IF NOT EXISTS backup_stores (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT UNIQUE NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    store_type  TEXT NOT NULL,
    config      JSONB NOT NULL DEFAULT '{}',
    credentials BYTEA NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
