-- Seed the built-in Default recovery rule set with all 38 rules enabled.
INSERT INTO recovery_rule_sets (id, name, description, builtin, config) VALUES (
    'a0000000-0000-4000-8000-000000000001',
    'Default',
    'Built-in recovery rules for PostgreSQL HA clusters',
    true,
    '[
        {"name":"stale-wal-recovery","pattern":"invalid record length at .* expected at least \\d+, got 0","severity":"critical","action":"restart","cooldown_seconds":60,"enabled":true,"category":"WAL & Checkpoint","threshold":1,"threshold_window_seconds":0},
        {"name":"checkpoint-missing","pattern":"could not locate a valid checkpoint record","severity":"critical","action":"rebasebackup","cooldown_seconds":120,"enabled":true,"category":"WAL & Checkpoint","threshold":1,"threshold_window_seconds":0},
        {"name":"wal-read-error","pattern":"could not read WAL at .* invalid record length","severity":"critical","action":"restart","cooldown_seconds":60,"enabled":true,"category":"WAL & Checkpoint","threshold":1,"threshold_window_seconds":0},
        {"name":"wal-size-mismatch","pattern":"WAL file .* has size \\d+, should be \\d+","severity":"critical","action":"restart","cooldown_seconds":60,"enabled":true,"category":"WAL & Checkpoint","threshold":1,"threshold_window_seconds":0},
        {"name":"wal-prevlink-corrupt","pattern":"record with incorrect prev-link","severity":"critical","action":"restart","cooldown_seconds":60,"enabled":true,"category":"WAL & Checkpoint","threshold":1,"threshold_window_seconds":0},
        {"name":"timeline-not-in-history","pattern":"requested starting point .* is not in this server.s history","severity":"critical","action":"rewind","cooldown_seconds":120,"enabled":true,"category":"Timeline & Replication","threshold":1,"threshold_window_seconds":0},
        {"name":"ahead-of-flush","pattern":"requested starting point .* is ahead of the WAL flush position","severity":"critical","action":"rewind","cooldown_seconds":120,"enabled":true,"category":"Timeline & Replication","threshold":1,"threshold_window_seconds":0},
        {"name":"timeline-not-child","pattern":"requested timeline \\d+ is not a child of this server.s history","severity":"critical","action":"rewind","cooldown_seconds":120,"enabled":true,"category":"Timeline & Replication","threshold":1,"threshold_window_seconds":0},
        {"name":"timeline-fork","pattern":"new timeline \\d+ is not a child of database system timeline \\d+","severity":"critical","action":"rewind","cooldown_seconds":120,"enabled":true,"category":"Timeline & Replication","threshold":1,"threshold_window_seconds":0},
        {"name":"wal-stream-timeline","pattern":"could not receive data from WAL stream:.*timeline","severity":"critical","action":"rewind","cooldown_seconds":120,"enabled":true,"category":"Timeline & Replication","threshold":1,"threshold_window_seconds":0},
        {"name":"page-corruption","pattern":"invalid page in block \\d+ of relation","severity":"critical","action":"event","cooldown_seconds":300,"enabled":true,"category":"Corruption","threshold":1,"threshold_window_seconds":0},
        {"name":"read-block-error","pattern":"could not read block \\d+ in file","severity":"critical","action":"event","cooldown_seconds":300,"enabled":true,"category":"Corruption","threshold":1,"threshold_window_seconds":0},
        {"name":"disk-full","pattern":"could not write to file.*No space left on device","severity":"critical","action":"event","cooldown_seconds":60,"enabled":true,"category":"Corruption","threshold":1,"threshold_window_seconds":0},
        {"name":"fsync-error","pattern":"could not fsync file","severity":"critical","action":"event","cooldown_seconds":60,"enabled":true,"category":"Corruption","threshold":1,"threshold_window_seconds":0},
        {"name":"too-many-clients","pattern":"FATAL:.*sorry, too many clients already","severity":"error","action":"event","cooldown_seconds":30,"enabled":true,"category":"Connections","threshold":1,"threshold_window_seconds":0},
        {"name":"reserved-slots-full","pattern":"FATAL:.*remaining connection slots are reserved","severity":"error","action":"event","cooldown_seconds":30,"enabled":true,"category":"Connections","threshold":1,"threshold_window_seconds":0},
        {"name":"out-of-memory","pattern":"ERROR:.*out of memory","severity":"error","action":"event","cooldown_seconds":30,"enabled":true,"category":"Connections","threshold":1,"threshold_window_seconds":0},
        {"name":"shared-memory","pattern":"out of shared memory","severity":"error","action":"event","cooldown_seconds":60,"enabled":true,"category":"Connections","threshold":1,"threshold_window_seconds":0},
        {"name":"slot-invalidated","pattern":"replication slot .* has been invalidated","severity":"critical","action":"rebasebackup","cooldown_seconds":300,"enabled":true,"category":"Replication Slots","threshold":1,"threshold_window_seconds":0},
        {"name":"wal-removed","pattern":"requested WAL segment .* has already been removed","severity":"critical","action":"rebasebackup","cooldown_seconds":300,"enabled":true,"category":"Replication Slots","threshold":1,"threshold_window_seconds":0},
        {"name":"slot-missing","pattern":"replication slot .* does not exist","severity":"warning","action":"event","cooldown_seconds":120,"enabled":true,"category":"Replication Slots","threshold":1,"threshold_window_seconds":0},
        {"name":"walsender-timeout","pattern":"terminating walsender process due to replication timeout","severity":"warning","action":"event","cooldown_seconds":60,"enabled":true,"category":"Replication Slots","threshold":1,"threshold_window_seconds":0},
        {"name":"recovery-conflict","pattern":"canceling statement due to conflict with recovery","severity":"info","action":"event","cooldown_seconds":30,"enabled":true,"category":"Replication Slots","threshold":1,"threshold_window_seconds":0},
        {"name":"auth-failure","pattern":"FATAL:.*password authentication failed for user","severity":"warning","action":"event","cooldown_seconds":30,"enabled":true,"category":"Authentication","threshold":1,"threshold_window_seconds":0},
        {"name":"hba-rejection","pattern":"FATAL:.*no pg_hba.conf entry for","severity":"warning","action":"event","cooldown_seconds":30,"enabled":true,"category":"Authentication","threshold":1,"threshold_window_seconds":0},
        {"name":"stale-backup-label","pattern":"FATAL:.*could not open file.*backup_label","severity":"error","action":"restart","cooldown_seconds":60,"enabled":true,"category":"Recovery","threshold":1,"threshold_window_seconds":0},
        {"name":"filenode-map-missing","pattern":"could not open file.*pg_filenode\\.map","severity":"critical","action":"rebasebackup","cooldown_seconds":120,"enabled":true,"category":"Recovery","threshold":1,"threshold_window_seconds":0},
        {"name":"wal-level-minimal","pattern":"WAL was generated with .wal_level=minimal., cannot continue recovering","severity":"critical","action":"rebasebackup","cooldown_seconds":120,"enabled":true,"category":"Recovery","threshold":1,"threshold_window_seconds":0},
        {"name":"wal-dir-missing","pattern":"FATAL:.*could not open directory.*pg_wal","severity":"critical","action":"rebasebackup","cooldown_seconds":120,"enabled":true,"category":"Recovery","threshold":1,"threshold_window_seconds":0},
        {"name":"version-mismatch","pattern":"database files are incompatible with server","severity":"critical","action":"rebasebackup","cooldown_seconds":300,"enabled":true,"category":"Recovery","threshold":1,"threshold_window_seconds":0},
        {"name":"catalog-corruption","pattern":"cache lookup failed for (relation|type|function|operator)","severity":"critical","action":"rebasebackup","cooldown_seconds":300,"enabled":true,"category":"Recovery","threshold":1,"threshold_window_seconds":0},
        {"name":"tablespace-missing","pattern":"could not open tablespace directory","severity":"critical","action":"rebasebackup","cooldown_seconds":120,"enabled":true,"category":"Recovery","threshold":1,"threshold_window_seconds":0},
        {"name":"recovery-complete","pattern":"consistent recovery state reached","severity":"info","action":"event","cooldown_seconds":0,"enabled":true,"category":"Recovery","threshold":1,"threshold_window_seconds":0},
        {"name":"streaming-started","pattern":"started streaming WAL from primary","severity":"info","action":"event","cooldown_seconds":0,"enabled":true,"category":"Streaming","threshold":1,"threshold_window_seconds":0},
        {"name":"primary-unreachable","pattern":"FATAL:.*could not connect to the primary server","severity":"warning","action":"event","cooldown_seconds":30,"enabled":true,"category":"Streaming","threshold":1,"threshold_window_seconds":0},
        {"name":"replication-terminated","pattern":"replication terminated by primary server","severity":"warning","action":"event","cooldown_seconds":60,"enabled":true,"category":"Streaming","threshold":1,"threshold_window_seconds":0},
        {"name":"max-walsenders","pattern":"FATAL:.*number of requested standby connections exceeds max_wal_senders","severity":"error","action":"event","cooldown_seconds":120,"enabled":true,"category":"Streaming","threshold":1,"threshold_window_seconds":0},
        {"name":"archive-failed","pattern":"archive command failed with exit code","severity":"error","action":"event","cooldown_seconds":60,"enabled":true,"category":"Archive","threshold":1,"threshold_window_seconds":0}
    ]'
) ON CONFLICT (name) DO NOTHING;

-- Link the dev profile to the Default recovery rule set.
UPDATE cluster_profiles
SET recovery_rule_set_id = 'a0000000-0000-4000-8000-000000000001'
WHERE name = 'dev' AND recovery_rule_set_id IS NULL;
