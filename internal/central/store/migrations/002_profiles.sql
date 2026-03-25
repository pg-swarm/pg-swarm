-- Cluster profiles: reusable, named configuration templates.
-- A profile becomes locked once a cluster is created from it.
CREATE TABLE IF NOT EXISTS cluster_profiles (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name        TEXT UNIQUE NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    config      JSONB NOT NULL DEFAULT '{}',
    locked      BOOLEAN NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Track which profile a cluster config was created from.
ALTER TABLE cluster_configs ADD COLUMN IF NOT EXISTS profile_id UUID REFERENCES cluster_profiles(id);
CREATE INDEX IF NOT EXISTS idx_cluster_configs_profile ON cluster_configs(profile_id);
