-- Add status tracking to cluster databases.
-- Status reflects whether the database was actually created in PostgreSQL.
ALTER TABLE cluster_databases ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending', 'created', 'failed'));
ALTER TABLE cluster_databases ADD COLUMN IF NOT EXISTS error_message TEXT NOT NULL DEFAULT '';
