-- Config version history for profiles.
-- Each version stores the complete ClusterSpec snapshot.
CREATE TABLE IF NOT EXISTS config_versions (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    profile_id     UUID NOT NULL REFERENCES cluster_profiles(id) ON DELETE CASCADE,
    version        INT NOT NULL,
    config         JSONB NOT NULL,
    change_summary TEXT NOT NULL DEFAULT '',
    apply_status   TEXT NOT NULL DEFAULT 'pending'
                   CHECK (apply_status IN ('pending', 'applied', 'failed', 'reverted')),
    created_by     TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_config_versions_profile_version
    ON config_versions (profile_id, version);

-- The locked column is no longer needed; in_use status is computed at query time.
ALTER TABLE cluster_profiles DROP COLUMN IF EXISTS locked;
