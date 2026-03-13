CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TABLE edge_groups (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    labels JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE satellites (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    hostname TEXT NOT NULL,
    k8s_cluster_name TEXT NOT NULL,
    region TEXT NOT NULL DEFAULT '',
    labels JSONB NOT NULL DEFAULT '{}',
    state TEXT NOT NULL DEFAULT 'pending',
    auth_token_hash TEXT NOT NULL DEFAULT '',
    temp_token_hash TEXT NOT NULL DEFAULT '',
    group_id UUID REFERENCES edge_groups(id),
    last_heartbeat TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE cluster_configs (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name TEXT NOT NULL,
    namespace TEXT NOT NULL DEFAULT 'default',
    satellite_id UUID REFERENCES satellites(id),
    group_id UUID REFERENCES edge_groups(id),
    config JSONB NOT NULL DEFAULT '{}',
    config_version BIGINT NOT NULL DEFAULT 1,
    state TEXT NOT NULL DEFAULT 'creating',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(name, satellite_id)
);

CREATE TABLE cluster_health (
    satellite_id UUID NOT NULL REFERENCES satellites(id),
    cluster_name TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'creating',
    instances JSONB NOT NULL DEFAULT '[]',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (satellite_id, cluster_name)
);

CREATE TABLE events (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    satellite_id UUID NOT NULL REFERENCES satellites(id),
    cluster_name TEXT NOT NULL,
    severity TEXT NOT NULL DEFAULT 'info',
    message TEXT NOT NULL,
    source TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_satellites_state ON satellites(state);
CREATE INDEX idx_cluster_configs_satellite ON cluster_configs(satellite_id);
CREATE INDEX idx_cluster_configs_group ON cluster_configs(group_id);
CREATE INDEX idx_events_satellite_cluster ON events(satellite_id, cluster_name);
CREATE INDEX idx_events_created ON events(created_at DESC);
