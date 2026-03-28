-- Event Rules framework: global entities + rule set filtering
--
-- action_types:             catalogue of sentinel operation types with command templates
-- event_rule_sets:          named groups of handlers, assigned to profiles
-- event_rules:              global log pattern → named event (no rule_set_id)
-- event_actions:            global named actions referencing a type (no rule_set_id)
-- event_handlers:           global event→action bindings (no rule_set_id)
-- event_rule_set_handlers:  junction: which handlers are in which rule set

CREATE TABLE IF NOT EXISTS action_types (
    name        TEXT PRIMARY KEY CHECK (name ~ '^[a-z][a-z0-9_-]*$'),
    description TEXT  NOT NULL DEFAULT '',
    template    TEXT,
    variables   JSONB NOT NULL DEFAULT '[]'
);

INSERT INTO action_types (name, description, template, variables) VALUES
    ('restart', 'Restart the PostgreSQL process via the sentinel sidecar',
     NULL,
     '[{"name":"timeout_seconds","description":"Seconds to wait before forcing kill (default 30)"}]'),
    ('reload',  'Reload PostgreSQL configuration without restart (pg_reload_conf)',
     NULL, '[]'),
    ('rebuild', 'Rebuild the instance from primary using pg_basebackup — destructive',
     NULL, '[]'),
    ('reboot',  'Restart the container / pod',
     NULL,
     '[{"name":"force","description":"Force-kill the pod rather than graceful shutdown (default false)"}]'),
    ('rewind',  'Re-synchronize standby timeline using pg_rewind',
     'pg_rewind --target-pgdata=$PGDATA --source-server="host=$PRIMARY_HOST port=$PRIMARY_PORT user=$REPLICATION_USER"',
     '[{"name":"PRIMARY_HOST","description":"Hostname of the primary"},{"name":"PRIMARY_PORT","description":"Port of the primary (default 5432)"},{"name":"REPLICATION_USER","description":"Replication user for pg_rewind connection"}]'),
    ('exec',    'Run a custom command inside the postgres container',
     NULL,
     '[{"name":"command","description":"Shell command to execute"}]')
ON CONFLICT (name) DO NOTHING;

CREATE TABLE IF NOT EXISTS event_rule_sets (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT        UNIQUE NOT NULL,
    description TEXT        NOT NULL DEFAULT '',
    builtin     BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS event_rules (
    id                       UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    name                     TEXT        UNIQUE NOT NULL,
    pattern                  TEXT        NOT NULL,
    severity                 TEXT        NOT NULL DEFAULT 'info' CHECK (severity IN ('critical','error','warning','info')),
    category                 TEXT        NOT NULL DEFAULT 'Custom',
    enabled                  BOOLEAN     NOT NULL DEFAULT TRUE,
    builtin                  BOOLEAN     NOT NULL DEFAULT FALSE,
    cooldown_seconds         INTEGER     NOT NULL DEFAULT 60,
    threshold                INTEGER     NOT NULL DEFAULT 1,
    threshold_window_seconds INTEGER     NOT NULL DEFAULT 0,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS event_actions (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT        UNIQUE NOT NULL,
    type        TEXT        NOT NULL REFERENCES action_types(name) ON UPDATE CASCADE,
    description TEXT        NOT NULL DEFAULT '',
    config      JSONB       NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS event_handlers (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    event_rule_id   UUID        NOT NULL REFERENCES event_rules(id)   ON DELETE CASCADE,
    event_action_id UUID        NOT NULL REFERENCES event_actions(id) ON DELETE CASCADE,
    enabled         BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (event_rule_id, event_action_id)
);

CREATE TABLE IF NOT EXISTS event_rule_set_handlers (
    rule_set_id UUID NOT NULL REFERENCES event_rule_sets(id)  ON DELETE CASCADE,
    handler_id  UUID NOT NULL REFERENCES event_handlers(id)   ON DELETE CASCADE,
    PRIMARY KEY (rule_set_id, handler_id)
);

CREATE INDEX ON event_handlers (event_rule_id);
CREATE INDEX ON event_handlers (event_action_id);
CREATE INDEX ON event_rule_set_handlers (handler_id);

ALTER TABLE cluster_profiles
    ADD COLUMN IF NOT EXISTS event_rule_set_id UUID REFERENCES event_rule_sets(id) ON DELETE SET NULL;

-- ─── Bootstrap ───────────────────────────────────────────────────────────────

INSERT INTO event_rule_sets (id, name, description, builtin) VALUES (
    'a0000000-0000-4000-8000-000000000001',
    'Default',
    'Built-in event rule set — all handlers enabled',
    true
) ON CONFLICT (name) DO NOTHING;

-- 38 global detection rules
INSERT INTO event_rules (name, pattern, severity, category, enabled, builtin, cooldown_seconds, threshold, threshold_window_seconds) VALUES
    ('stale-wal-recovery',     'invalid record length at .* expected at least \d+, got 0',              'critical', 'WAL & Checkpoint',      true, true,  60, 1, 0),
    ('checkpoint-missing',     'could not locate a valid checkpoint record',                             'critical', 'WAL & Checkpoint',      true, true, 120, 1, 0),
    ('wal-read-error',         'could not read WAL at .* invalid record length',                         'critical', 'WAL & Checkpoint',      true, true,  60, 1, 0),
    ('wal-size-mismatch',      'WAL file .* has size \d+, should be \d+',                               'critical', 'WAL & Checkpoint',      true, true,  60, 1, 0),
    ('wal-prevlink-corrupt',   'record with incorrect prev-link',                                        'critical', 'WAL & Checkpoint',      true, true,  60, 1, 0),
    ('timeline-not-in-history','requested starting point .* is not in this server.s history',           'critical', 'Timeline & Replication', true, true, 120, 1, 0),
    ('ahead-of-flush',         'requested starting point .* is ahead of the WAL flush position',        'critical', 'Timeline & Replication', true, true, 120, 1, 0),
    ('timeline-not-child',     'requested timeline \d+ is not a child of this server.s history',        'critical', 'Timeline & Replication', true, true, 120, 1, 0),
    ('timeline-fork',          'new timeline \d+ is not a child of database system timeline \d+',       'critical', 'Timeline & Replication', true, true, 120, 1, 0),
    ('wal-stream-timeline',    'could not receive data from WAL stream:.*timeline',                      'critical', 'Timeline & Replication', true, true, 120, 1, 0),
    ('page-corruption',        'invalid page in block \d+ of relation',                                 'critical', 'Corruption',             true, true, 300, 1, 0),
    ('read-block-error',       'could not read block \d+ in file',                                      'critical', 'Corruption',             true, true, 300, 1, 0),
    ('disk-full',              'could not write to file.*No space left on device',                       'critical', 'Corruption',             true, true,  60, 1, 0),
    ('fsync-error',            'could not fsync file',                                                   'critical', 'Corruption',             true, true,  60, 1, 0),
    ('too-many-clients',       'FATAL:.*sorry, too many clients already',                               'error',    'Connections',            true, true,  30, 1, 0),
    ('reserved-slots-full',    'FATAL:.*remaining connection slots are reserved',                        'error',    'Connections',            true, true,  30, 1, 0),
    ('out-of-memory',          'ERROR:.*out of memory',                                                  'error',    'Connections',            true, true,  30, 1, 0),
    ('shared-memory',          'out of shared memory',                                                   'error',    'Connections',            true, true,  60, 1, 0),
    ('slot-invalidated',       'replication slot .* has been invalidated',                              'critical', 'Replication Slots',      true, true, 300, 1, 0),
    ('wal-removed',            'requested WAL segment .* has already been removed',                     'critical', 'Replication Slots',      true, true, 300, 1, 0),
    ('slot-missing',           'replication slot .* does not exist',                                    'warning',  'Replication Slots',      true, true, 120, 1, 0),
    ('walsender-timeout',      'terminating walsender process due to replication timeout',               'warning',  'Replication Slots',      true, true,  60, 1, 0),
    ('recovery-conflict',      'canceling statement due to conflict with recovery',                      'info',     'Replication Slots',      true, true,  30, 1, 0),
    ('auth-failure',           'FATAL:.*password authentication failed for user',                        'warning',  'Authentication',         true, true,  30, 1, 0),
    ('hba-rejection',          'FATAL:.*no pg_hba.conf entry for',                                      'warning',  'Authentication',         true, true,  30, 1, 0),
    ('stale-backup-label',     'FATAL:.*could not open file.*backup_label',                             'error',    'Recovery',               true, true,  60, 1, 0),
    ('filenode-map-missing',   'could not open file.*pg_filenode\.map',                                  'critical', 'Recovery',               true, true, 120, 1, 0),
    ('wal-level-minimal',      'WAL was generated with .wal_level=minimal., cannot continue recovering', 'critical', 'Recovery',               true, true, 120, 1, 0),
    ('wal-dir-missing',        'FATAL:.*could not open directory.*pg_wal',                              'critical', 'Recovery',               true, true, 120, 1, 0),
    ('version-mismatch',       'database files are incompatible with server',                            'critical', 'Recovery',               true, true, 300, 1, 0),
    ('catalog-corruption',     'cache lookup failed for (relation|type|function|operator)',              'critical', 'Recovery',               true, true, 300, 1, 0),
    ('tablespace-missing',     'could not open tablespace directory',                                    'critical', 'Recovery',               true, true, 120, 1, 0),
    ('recovery-complete',      'consistent recovery state reached',                                      'info',     'Recovery',               true, true,   0, 1, 0),
    ('streaming-started',      'started streaming WAL from primary',                                     'info',     'Streaming',              true, true,   0, 1, 0),
    ('primary-unreachable',    'FATAL:.*could not connect to the primary server',                        'warning',  'Streaming',              true, true,  30, 1, 0),
    ('replication-terminated', 'replication terminated by primary server',                               'warning',  'Streaming',              true, true,  60, 1, 0),
    ('max-walsenders',         'FATAL:.*number of requested standby connections exceeds max_wal_senders','error',    'Streaming',              true, true, 120, 1, 0),
    ('archive-failed',         'archive command failed with exit code',                                  'error',    'Archive',                true, true,  60, 1, 0)
ON CONFLICT (name) DO NOTHING;

-- 5 global actions
INSERT INTO event_actions (name, type, description, config) VALUES
    ('pg-restart', 'restart', 'Restart the PostgreSQL process via the sentinel sidecar',         '{"timeout_seconds":30}'),
    ('pg-reload',  'reload',  'Reload PostgreSQL configuration without restart (pg_reload_conf)', '{}'),
    ('pg-rebuild', 'rebuild', 'Rebuild the instance from primary — pg_basebackup (destructive)',  '{}'),
    ('pg-reboot',  'reboot',  'Restart the container / pod',                                      '{"force":false}'),
    ('pg-rewind',  'rewind',  'Re-synchronize standby timeline using pg_rewind',                  '{"PRIMARY_HOST":"primary","PRIMARY_PORT":"5432","REPLICATION_USER":"replicator"}')
ON CONFLICT (name) DO NOTHING;

-- 19 global handlers
INSERT INTO event_handlers (event_rule_id, event_action_id, enabled)
SELECT er.id, ea.id, true
FROM event_rules er, event_actions ea
WHERE (er.name, ea.name) IN (VALUES
    ('stale-wal-recovery',      'pg-restart'),
    ('wal-read-error',          'pg-restart'),
    ('wal-size-mismatch',       'pg-restart'),
    ('wal-prevlink-corrupt',    'pg-restart'),
    ('stale-backup-label',      'pg-restart'),
    ('checkpoint-missing',      'pg-rebuild'),
    ('slot-invalidated',        'pg-rebuild'),
    ('wal-removed',             'pg-rebuild'),
    ('filenode-map-missing',    'pg-rebuild'),
    ('wal-level-minimal',       'pg-rebuild'),
    ('wal-dir-missing',         'pg-rebuild'),
    ('version-mismatch',        'pg-rebuild'),
    ('catalog-corruption',      'pg-rebuild'),
    ('tablespace-missing',      'pg-rebuild'),
    ('timeline-not-in-history', 'pg-rewind'),
    ('ahead-of-flush',          'pg-rewind'),
    ('timeline-not-child',      'pg-rewind'),
    ('timeline-fork',           'pg-rewind'),
    ('wal-stream-timeline',     'pg-rewind')
)
ON CONFLICT (event_rule_id, event_action_id) DO NOTHING;

-- Add all 19 handlers to the Default rule set
INSERT INTO event_rule_set_handlers (rule_set_id, handler_id)
SELECT 'a0000000-0000-4000-8000-000000000001', id FROM event_handlers
ON CONFLICT DO NOTHING;

-- Link dev profile to Default rule set
UPDATE cluster_profiles
SET event_rule_set_id = 'a0000000-0000-4000-8000-000000000001'
WHERE name = 'dev' AND event_rule_set_id IS NULL;
