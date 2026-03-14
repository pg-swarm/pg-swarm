-- Replace deployment_groups with deployment_rules.
-- A deployment rule maps a profile to an edge group + namespace + cluster name.
-- Fan-out: one ClusterConfig per satellite in the edge group.

-- Drop old references
ALTER TABLE cluster_configs DROP COLUMN IF EXISTS deployment_group_id;
DROP TABLE IF EXISTS deployment_groups;

-- New table
CREATE TABLE deployment_rules (
    id             UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name           TEXT NOT NULL UNIQUE,
    profile_id     UUID NOT NULL REFERENCES cluster_profiles(id),
    edge_group_id  UUID NOT NULL REFERENCES edge_groups(id),
    namespace      TEXT NOT NULL DEFAULT 'default',
    cluster_name   TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(edge_group_id, namespace, cluster_name)
);

CREATE INDEX idx_deployment_rules_profile ON deployment_rules(profile_id);
CREATE INDEX idx_deployment_rules_edge_group ON deployment_rules(edge_group_id);

-- Link cluster configs to the deployment rule that created them
ALTER TABLE cluster_configs ADD COLUMN deployment_rule_id UUID REFERENCES deployment_rules(id);
CREATE INDEX idx_cluster_configs_deployment_rule ON cluster_configs(deployment_rule_id);
