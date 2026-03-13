-- Replace group-based targeting with label-based targeting.
-- Deployment rules now carry a label_selector JSONB instead of edge_group_id.
-- Satellites are matched via labels @> label_selector.

-- 1. Add label_selector to deployment_rules
ALTER TABLE deployment_rules ADD COLUMN label_selector JSONB NOT NULL DEFAULT '{}';

-- 2. Migrate existing rules: copy edge group labels into label_selector
UPDATE deployment_rules dr
SET label_selector = COALESCE(
    (SELECT eg.labels FROM edge_groups eg WHERE eg.id = dr.edge_group_id),
    '{}'::jsonb
);

-- 3. Drop edge_group_id from deployment_rules
ALTER TABLE deployment_rules DROP CONSTRAINT IF EXISTS deployment_rules_edge_group_id_namespace_cluster_name_key;
DROP INDEX IF EXISTS idx_deployment_rules_edge_group;
ALTER TABLE deployment_rules DROP COLUMN edge_group_id;

-- 4. Drop group_id from satellites
ALTER TABLE satellites DROP COLUMN IF EXISTS group_id;

-- 5. Drop group_id from cluster_configs
ALTER TABLE cluster_configs DROP COLUMN IF EXISTS group_id;

-- 6. Drop edge_groups table
DROP TABLE IF EXISTS edge_groups;

-- 7. GIN index on satellites.labels for fast @> queries
CREATE INDEX IF NOT EXISTS idx_satellites_labels ON satellites USING GIN (labels);
