-- Postgres variant master (admin-configurable).
CREATE TABLE IF NOT EXISTS postgres_variants (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT    NOT NULL UNIQUE,   -- e.g. "alpine", "debian"
    description TEXT    NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Seed default variants.
INSERT INTO postgres_variants (name, description) VALUES
    ('Alpine', 'Alpine Linux — minimal image, smaller size'),
    ('Debian', 'Debian — full-featured base image')
ON CONFLICT (name) DO NOTHING;

-- Normalize existing variant values to match the master table (title case).
UPDATE postgres_versions SET variant = INITCAP(variant);

-- Add FK from postgres_versions.variant → postgres_variants.name.
ALTER TABLE postgres_versions
    ADD CONSTRAINT fk_postgres_versions_variant
    FOREIGN KEY (variant) REFERENCES postgres_variants(name)
    ON UPDATE CASCADE ON DELETE RESTRICT;
