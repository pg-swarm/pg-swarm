-- Backup rules: reusable backup configurations that attach to profiles.
CREATE TABLE IF NOT EXISTS backup_rules (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT UNIQUE NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    config      JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Join table: many-to-many between profiles and backup rules.
CREATE TABLE IF NOT EXISTS profile_backup_rules (
    profile_id     UUID NOT NULL REFERENCES cluster_profiles(id) ON DELETE CASCADE,
    backup_rule_id UUID NOT NULL REFERENCES backup_rules(id) ON DELETE CASCADE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (profile_id, backup_rule_id)
);
