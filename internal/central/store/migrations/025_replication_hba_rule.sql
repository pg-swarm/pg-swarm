-- Add a specific rule for replication connection HBA rejections.
-- This fires when a replica cannot stream from the primary because
-- pg_hba.conf is missing or misconfigured (e.g. PGDATA was deleted).
-- Action is "event" (dashboard visibility); the wrapper sentinel logic
-- handles the actual failover.
UPDATE recovery_rule_sets
SET config = config || '[{"name":"replication-hba-rejection","pattern":"FATAL:.*no pg_hba.conf entry for.*replication connection","severity":"critical","action":"event","cooldown_seconds":30,"enabled":true,"category":"Streaming","threshold":1,"threshold_window_seconds":0}]'::jsonb
WHERE id = 'a0000000-0000-4000-8000-000000000001';
