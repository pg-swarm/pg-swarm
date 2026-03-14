-- Postgres version-to-image mapping (admin-configurable).
CREATE TABLE IF NOT EXISTS postgres_versions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    version     TEXT    NOT NULL,            -- e.g. "16", "17", "18"
    variant     TEXT    NOT NULL,            -- e.g. "alpine", "debian"
    image_tag   TEXT    NOT NULL,            -- e.g. "17.9-alpine3.23"
    is_default  BOOLEAN NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (version, variant)
);

-- Seed default versions.
INSERT INTO postgres_versions (version, variant, image_tag, is_default) VALUES
    ('16', 'Alpine', '16.13-alpine3.23', FALSE),
    ('17', 'Alpine', '17.9-alpine3.23',  TRUE),
    ('18', 'Alpine', '18.3-alpine3.23',  FALSE),
    ('16', 'Debian', '16.13-trixie',     FALSE),
    ('17', 'Debian', '17.9-trixie',      FALSE),
    ('18', 'Debian', '18.3-trixie',      FALSE)
ON CONFLICT (version, variant) DO NOTHING;
