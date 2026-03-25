-- Parameter classification for config change management.
-- Determines the update strategy when a pg_param is changed:
--   reload       = pg_reload_conf() on all pods (no restart, no downtime)
--   sequential   = rolling restart of pods one at a time (brief per-pod downtime)
--   full_restart = scale to 0, update, scale back (full outage)
-- Parameters NOT in this table default to 'reload'.
CREATE TABLE IF NOT EXISTS pg_param_classifications (
    name           TEXT PRIMARY KEY,
    restart_mode   TEXT NOT NULL DEFAULT 'reload'
                   CHECK (restart_mode IN ('reload', 'sequential', 'full_restart')),
    description    TEXT NOT NULL DEFAULT '',
    pg_context     TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Bootstrap all known PostgreSQL parameters with their correct context and update mode.

INSERT INTO pg_param_classifications (name, restart_mode, description, pg_context) VALUES

    -- ═══ full_restart: replication breaks on mismatch ═══
    ('wal_level', 'full_restart', 'WAL detail level — mismatch between primary and replica breaks streaming replication', 'postmaster'),

    -- ═══ sequential: postmaster context, requires process restart, but brief mismatch is safe ═══
    ('shared_buffers',                 'sequential', 'Shared memory buffer pool size',                          'postmaster'),
    ('max_connections',                'sequential', 'Maximum number of concurrent connections',                'postmaster'),
    ('superuser_reserved_connections', 'sequential', 'Connections reserved for superusers',                     'postmaster'),
    ('huge_pages',                     'sequential', 'Use of huge pages for shared memory',                     'postmaster'),
    ('max_wal_senders',                'sequential', 'Maximum number of WAL sender processes',                  'postmaster'),
    ('max_replication_slots',          'sequential', 'Maximum number of replication slots',                     'postmaster'),
    ('max_worker_processes',           'sequential', 'Maximum number of background worker processes',           'postmaster'),
    ('max_parallel_workers',           'sequential', 'Maximum number of parallel query workers',                'postmaster'),
    ('shared_preload_libraries',       'sequential', 'Libraries to preload at server start',                    'postmaster'),
    ('max_locks_per_transaction',      'sequential', 'Maximum number of locks per transaction',                 'postmaster'),
    ('max_prepared_transactions',      'sequential', 'Maximum number of simultaneously prepared transactions',  'postmaster'),
    ('wal_log_hints',                  'sequential', 'Writes full-page images of modified pages to WAL',        'postmaster'),
    ('archive_mode',                   'sequential', 'Enables WAL archiving',                                   'postmaster'),
    ('track_commit_timestamp',         'sequential', 'Collects transaction commit timestamps',                  'postmaster'),
    ('ssl',                            'sequential', 'Enables SSL connections',                                 'postmaster'),
    ('hot_standby',                    'sequential', 'Allows queries on standby servers',                       'postmaster'),
    ('max_files_per_process',          'sequential', 'Maximum number of open files per server process',         'postmaster'),
    ('bonjour',                        'sequential', 'Enables Bonjour advertising',                             'postmaster'),

    -- ═══ reload: sighup context, takes effect on pg_reload_conf() ═══

    -- Memory
    ('work_mem',                       'reload', 'Memory for sort operations and hash tables',               'sighup'),
    ('maintenance_work_mem',           'reload', 'Memory for maintenance operations (VACUUM, CREATE INDEX)', 'sighup'),
    ('effective_cache_size',           'reload', 'Planner estimate of effective disk cache size',             'sighup'),
    ('temp_buffers',                   'reload', 'Maximum number of temporary buffers per session',          'user'),

    -- WAL & Checkpoints
    ('checkpoint_completion_target',   'reload', 'Fraction of checkpoint interval for spreading writes',     'sighup'),
    ('checkpoint_timeout',             'reload', 'Maximum time between automatic WAL checkpoints',           'sighup'),
    ('checkpoint_warning',             'reload', 'Warn if checkpoints happen more frequently than this',     'sighup'),
    ('max_wal_size',                   'reload', 'Maximum WAL size between checkpoints',                     'sighup'),
    ('min_wal_size',                   'reload', 'Minimum WAL size to retain',                               'sighup'),
    ('archive_command',                'reload', 'Shell command to execute for WAL archiving',               'sighup'),
    ('archive_timeout',                'reload', 'Force WAL switch after this many seconds of inactivity',   'sighup'),
    ('wal_keep_size',                  'reload', 'Minimum WAL size to retain in pg_wal',                     'sighup'),

    -- Replication
    ('max_standby_streaming_delay',    'reload', 'Maximum delay before canceling queries on standby',        'sighup'),
    ('max_standby_archive_delay',      'reload', 'Maximum delay before canceling queries during recovery',   'sighup'),
    ('hot_standby_feedback',           'reload', 'Send feedback to primary about queries on standby',        'sighup'),
    ('max_slot_wal_keep_size',         'reload', 'Maximum WAL size retained by replication slots',           'sighup'),
    ('synchronous_commit',             'reload', 'Sets the current synchronous commit level',                'user'),
    ('synchronous_standby_names',      'reload', 'Standby servers for synchronous replication',              'sighup'),

    -- Query Planner
    ('random_page_cost',               'reload', 'Planner cost estimate for a random disk page fetch',       'user'),
    ('seq_page_cost',                  'reload', 'Planner cost estimate for a sequential disk page fetch',   'user'),
    ('cpu_tuple_cost',                 'reload', 'Planner cost estimate for processing each row',            'user'),
    ('cpu_index_tuple_cost',           'reload', 'Planner cost estimate for processing each index entry',    'user'),
    ('cpu_operator_cost',              'reload', 'Planner cost estimate for processing each operator',       'user'),
    ('effective_io_concurrency',       'reload', 'Number of simultaneous I/O operations for the planner',    'user'),
    ('default_statistics_target',      'reload', 'Default statistics target for table columns',              'user'),
    ('jit',                            'reload', 'Allow JIT compilation of queries',                         'user'),
    ('jit_above_cost',                 'reload', 'Query cost above which JIT is activated',                  'user'),
    ('jit_inline_above_cost',          'reload', 'Query cost above which JIT inlines functions',             'user'),
    ('jit_optimize_above_cost',        'reload', 'Query cost above which JIT applies optimizations',         'user'),
    ('enable_seqscan',                 'reload', 'Enable sequential scan plan type',                         'user'),
    ('enable_indexscan',               'reload', 'Enable index scan plan type',                              'user'),
    ('enable_hashjoin',                'reload', 'Enable hash join plan type',                               'user'),
    ('enable_mergejoin',               'reload', 'Enable merge join plan type',                              'user'),
    ('enable_nestloop',                'reload', 'Enable nested-loop join plan type',                        'user'),

    -- Logging
    ('log_min_duration_statement',     'reload', 'Log statements running longer than this (ms)',             'superuser'),
    ('log_min_messages',               'reload', 'Minimum message severity to log',                          'superuser'),
    ('log_min_error_statement',        'reload', 'Minimum error severity to log the statement',              'superuser'),
    ('log_statement',                  'reload', 'Which SQL statements to log (none/ddl/mod/all)',           'superuser'),
    ('log_duration',                   'reload', 'Log the duration of each completed statement',             'superuser'),
    ('log_connections',                'reload', 'Log each successful connection',                           'sighup'),
    ('log_disconnections',             'reload', 'Log end of each session',                                  'sighup'),
    ('log_lock_waits',                 'reload', 'Log long lock waits',                                      'superuser'),
    ('log_temp_files',                 'reload', 'Log temporary file usage above this size (kB)',            'superuser'),
    ('log_checkpoints',                'reload', 'Log each checkpoint',                                      'sighup'),
    ('log_autovacuum_min_duration',    'reload', 'Log autovacuum runs longer than this (ms)',                'sighup'),
    ('log_line_prefix',                'reload', 'Printf-style format string for log line prefix',           'sighup'),
    ('log_timezone',                   'reload', 'Timezone used for timestamps in log messages',             'sighup'),

    -- Autovacuum
    ('autovacuum',                     'reload', 'Enable autovacuum subprocess',                             'sighup'),
    ('autovacuum_max_workers',         'sequential', 'Maximum number of autovacuum worker processes',        'postmaster'),
    ('autovacuum_naptime',             'reload', 'Time between autovacuum runs',                             'sighup'),
    ('autovacuum_vacuum_threshold',    'reload', 'Minimum number of row updates before vacuum',              'sighup'),
    ('autovacuum_vacuum_scale_factor', 'reload', 'Fraction of table size before vacuum',                     'sighup'),
    ('autovacuum_analyze_threshold',   'reload', 'Minimum number of row updates before analyze',             'sighup'),
    ('autovacuum_analyze_scale_factor','reload', 'Fraction of table size before analyze',                    'sighup'),
    ('autovacuum_vacuum_cost_delay',   'reload', 'Vacuum cost delay for autovacuum (ms)',                    'sighup'),
    ('autovacuum_vacuum_cost_limit',   'reload', 'Vacuum cost limit for autovacuum',                         'sighup'),

    -- Vacuum throttling
    ('vacuum_cost_delay',              'reload', 'Vacuum cost delay for manual VACUUM (ms)',                 'user'),
    ('vacuum_cost_page_hit',           'reload', 'Vacuum cost for a page found in buffer cache',             'user'),
    ('vacuum_cost_page_miss',          'reload', 'Vacuum cost for a page not in buffer cache',               'user'),
    ('vacuum_cost_page_dirty',         'reload', 'Vacuum cost for dirtying a clean page',                    'user'),
    ('vacuum_cost_limit',              'reload', 'Total vacuum cost limit before napping',                   'user'),

    -- Client defaults
    ('statement_timeout',              'reload', 'Maximum allowed duration of any statement (ms)',            'user'),
    ('idle_in_transaction_session_timeout', 'reload', 'Maximum idle time in transaction before termination', 'user'),
    ('lock_timeout',                   'reload', 'Maximum time to wait for a lock (ms)',                     'user'),
    ('search_path',                    'reload', 'Schema search path',                                       'user'),
    ('default_transaction_isolation',  'reload', 'Default transaction isolation level',                      'user'),
    ('timezone',                       'reload', 'Timezone for displaying and interpreting timestamps',       'user'),
    ('datestyle',                      'reload', 'Display format for date and time values',                  'user'),
    ('client_min_messages',            'reload', 'Minimum message severity sent to client',                  'user'),

    -- Statistics & monitoring
    ('track_activities',               'reload', 'Collects information about executing commands',             'sighup'),
    ('track_counts',                   'reload', 'Collects statistics on database activity',                  'sighup'),
    ('track_io_timing',                'reload', 'Collects timing statistics for I/O operations',             'superuser'),
    ('track_functions',                'reload', 'Collects function-level statistics',                        'superuser'),
    ('pg_stat_statements.track',       'reload', 'Which statements pg_stat_statements tracks',               'superuser'),
    ('pg_stat_statements.max',         'sequential', 'Maximum number of tracked statements',                 'postmaster'),

    -- Connection & authentication
    ('tcp_keepalives_idle',            'reload', 'Seconds of inactivity before sending a TCP keepalive',     'user'),
    ('tcp_keepalives_interval',        'reload', 'Seconds between TCP keepalive retransmits',                'user'),
    ('tcp_keepalives_count',           'reload', 'Maximum number of TCP keepalive retransmits',              'user'),
    ('password_encryption',            'reload', 'Encryption method for stored passwords',                   'user'),
    ('authentication_timeout',         'reload', 'Maximum time to complete client authentication',           'sighup'),

    -- Miscellaneous
    ('max_parallel_workers_per_gather','reload', 'Maximum parallel workers per Gather plan node',            'user'),
    ('max_parallel_maintenance_workers','reload', 'Maximum parallel workers for maintenance operations',     'user'),
    ('parallel_tuple_cost',            'reload', 'Planner cost of passing each tuple from worker to leader', 'user'),
    ('parallel_setup_cost',            'reload', 'Planner cost of launching parallel workers',               'user'),
    ('recovery_target_timeline',       'reload', 'Target timeline for recovery',                             'postmaster'),
    ('wal_receiver_timeout',           'reload', 'Maximum wait time for WAL receiver to receive data',       'sighup'),
    ('wal_sender_timeout',             'reload', 'Maximum time for WAL sender to wait for replication',      'sighup')

ON CONFLICT (name) DO NOTHING;
