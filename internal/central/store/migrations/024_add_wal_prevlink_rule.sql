-- Add the "incorrect prev-link" WAL rule to the Default recovery rule set.
-- This handles stale WAL segments where record chain pointers reference old timeline data.
UPDATE recovery_rule_sets
SET config = config || '[{"name":"wal-prevlink-corrupt","pattern":"record with incorrect prev-link","severity":"critical","action":"restart","cooldown_seconds":60,"enabled":true,"category":"WAL & Checkpoint"}]'::jsonb,
    updated_at = NOW()
WHERE name = 'Default' AND builtin = true;
