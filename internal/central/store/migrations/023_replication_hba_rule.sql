-- Add replication HBA rejection rule (global, no rule_set_id).
INSERT INTO event_rules (name, pattern, severity, category, enabled, builtin, cooldown_seconds, threshold, threshold_window_seconds)
VALUES (
    'replication-hba-rejection',
    'FATAL:.*no pg_hba.conf entry for.*replication connection',
    'critical', 'Streaming', true, true, 30, 1, 0
) ON CONFLICT (name) DO NOTHING;
