ALTER TABLE cluster_profiles
    ADD COLUMN IF NOT EXISTS recovery_rule_set_id UUID REFERENCES recovery_rule_sets(id) ON DELETE SET NULL;
