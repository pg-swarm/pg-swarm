-- Track which profile version each cluster has applied.
-- Enables per-cluster approval: when a profile is updated, clusters show
-- "update available" until individually approved.
ALTER TABLE cluster_configs ADD COLUMN IF NOT EXISTS applied_profile_version INT NOT NULL DEFAULT 0;
