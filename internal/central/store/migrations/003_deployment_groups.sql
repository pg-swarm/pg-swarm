-- Deployment groups: clusters that share a profile configuration
CREATE TABLE deployment_groups (
    id          UUID PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    profile_id  UUID NOT NULL REFERENCES cluster_profiles(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_deployment_groups_profile ON deployment_groups(profile_id);

-- Link clusters to deployment groups
ALTER TABLE cluster_configs ADD COLUMN deployment_group_id UUID REFERENCES deployment_groups(id);
CREATE INDEX idx_cluster_configs_deployment_group ON cluster_configs(deployment_group_id);
