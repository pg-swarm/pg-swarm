-- Dev and hourly backup profiles for rapid development and lighter-weight production use.

-- Base backup every minute for development/testing
INSERT INTO backup_profiles (id, name, description, config) VALUES (
    'a0000000-0000-0000-0000-000000000003',
    'dev-physical-every-minute',
    'Base backup every minute with WAL archiving for rapid development and testing',
    '{
        "physical": {
            "base_schedule": "* */1 * * *",
            "incremental_schedule": "*/5 * * * *",
            "wal_archive_enabled": true,
            "archive_timeout_seconds": 30
        },
        "destination": {
            "type": "local",
            "local": { "size": "5Gi" }
        },
        "retention": {
            "base_backup_count": 2,
            "incremental_backup_count": 5,
            "wal_retention_days": 1
        }
    }'
) ON CONFLICT (name) DO NOTHING;

-- Base backup every hour for lighter-weight production use
INSERT INTO backup_profiles (id, name, description, config) VALUES (
    'a0000000-0000-0000-0000-000000000004',
    'hourly-physical',
    'Base backup every hour with WAL archiving for lighter-weight production workloads',
    '{
        "physical": {
            "base_schedule": "0 * * * *",
            "incremental_schedule": "*/15 * * * *",
            "wal_archive_enabled": true,
            "archive_timeout_seconds": 60
        },
        "destination": {
            "type": "local",
            "local": { "size": "30Gi" }
        },
        "retention": {
            "base_backup_count": 24,
            "incremental_backup_count": 3,
            "wal_retention_days": 3
        }
    }'
) ON CONFLICT (name) DO NOTHING;
