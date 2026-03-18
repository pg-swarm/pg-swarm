-- Default dev cluster profile: lightweight single-replica PG 17 Alpine for local development.

INSERT INTO cluster_profiles (id, name, description, config) VALUES (
    'c0000000-0000-0000-0000-000000000001',
    'dev',
    'Lightweight single-replica PostgreSQL 17 Alpine for development and testing',
    '{
        "replicas": 3,
        "postgres": {
            "version": "17",
            "variant": "alpine"
        },
        "storage": {
            "size": "1Gi"
        },
        "wal_storage": {
            "size": "1Gi"
        },
        "resources": {
            "cpu_request": "100m",
            "cpu_limit": "500m",
            "memory_request": "256Mi",
            "memory_limit": "512Mi"
        },
        "deletion_protection": false,
        "databases": [
            {
                "name": "test",
                "user": "test",
                "password": "test"
            }
        ],
        "pg_params": {
            "log_statement": "all",
            "log_min_duration_statement": "0",
            "shared_buffers": "128MB",
            "work_mem": "8MB"
        },
        "hba_rules": [
            "host all all 0.0.0.0/0 md5"
        ],
        "failover": {
            "enabled": true,
            "health_check_interval_seconds": 5
        }
    }'
) ON CONFLICT (name) DO NOTHING;
