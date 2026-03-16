ALTER TABLE satellites ADD COLUMN IF NOT EXISTS tier_mappings JSONB NOT NULL DEFAULT '{}';

CREATE TABLE IF NOT EXISTS storage_tiers (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
