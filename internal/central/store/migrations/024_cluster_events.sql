CREATE TABLE IF NOT EXISTS cluster_events (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type   TEXT NOT NULL,
    cluster_name TEXT NOT NULL DEFAULT '',
    namespace    TEXT NOT NULL DEFAULT '',
    pod_name     TEXT NOT NULL DEFAULT '',
    severity     TEXT NOT NULL DEFAULT 'info',
    source       TEXT NOT NULL DEFAULT '',
    satellite_id UUID NOT NULL REFERENCES satellites(id),
    operation_id TEXT NOT NULL DEFAULT '',
    data         JSONB NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_cluster_events_cluster
    ON cluster_events (satellite_id, cluster_name, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_cluster_events_type
    ON cluster_events (event_type, created_at DESC);
