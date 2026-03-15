-- Default backup profiles: seeded on first run, can be edited or deleted.

-- Daily logical backup at 2 AM
INSERT INTO backup_profiles (id, name, description, config) VALUES (
    'a0000000-0000-0000-0000-000000000001',
    'daily-logical-2am',
    'Full logical dump of all databases every day at 2 AM',
    '{
        "logical": {
            "schedule": "0 2 * * *",
            "databases": [],
            "format": "custom"
        },
        "destination": {
            "type": "local",
            "local": { "size": "20Gi" }
        },
        "retention": {
            "logical_backup_count": 7
        }
    }'
) ON CONFLICT (name) DO NOTHING;

-- Daily base backup at 4 AM + hourly incrementals + WAL archiving
INSERT INTO backup_profiles (id, name, description, config) VALUES (
    'a0000000-0000-0000-0000-000000000002',
    'daily-physical-4am',
    'Full base backup at 4 AM, hourly incremental backups, continuous WAL archiving',
    '{
        "physical": {
            "base_schedule": "0 4 * * *",
            "incremental_schedule": "0 * * * *",
            "wal_archive_enabled": true,
            "archive_timeout_seconds": 60
        },
        "destination": {
            "type": "local",
            "local": { "size": "50Gi" }
        },
        "retention": {
            "base_backup_count": 7,
            "incremental_backup_count": 23,
            "wal_retention_days": 7
        }
    }'
) ON CONFLICT (name) DO NOTHING;
