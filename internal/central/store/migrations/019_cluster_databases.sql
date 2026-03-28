-- Cluster-level databases managed independently of profiles.
-- Each record represents a database + owner user + allowed connection subnets.
CREATE TABLE IF NOT EXISTS cluster_databases (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    cluster_id    UUID NOT NULL REFERENCES cluster_configs(id) ON DELETE CASCADE,
    db_name       TEXT NOT NULL,
    db_user       TEXT NOT NULL,
    password      BYTEA DEFAULT NULL,
    allowed_cidrs TEXT[] NOT NULL DEFAULT '{}',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(cluster_id, db_name)
);

CREATE INDEX IF NOT EXISTS idx_cluster_databases_cluster_id ON cluster_databases (cluster_id);
