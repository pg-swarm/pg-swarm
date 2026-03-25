#!/bin/bash
# pg-swarm init container: first-time setup ONLY.
# All recovery (pg_rewind, WAL cleanup, crash-loop basebackup) is handled
# by the wrapper script in the main container.
set -e

ORDINAL=${POD_NAME##*-}
PGDATA="{{PGDATA}}"
PRIMARY_HOST="{{RW_SVC}}.{{NAMESPACE}}.svc.cluster.local"

# Already initialized — copy config and exit. The wrapper handles recovery.
if [ -f "$PGDATA/PG_VERSION" ]; then
    echo "PGDATA already initialized, copying config only"
    cp /etc/pg-config/postgresql.conf "$PGDATA/postgresql.conf"
    cp /etc/pg-config/pg_hba.conf "$PGDATA/pg_hba.conf"{{WAL_SYMLINK_IDEMPOTENT}}
    exit 0
fi

# Check if this pod was a demoted primary that needs re-basebackup (not initdb).
# The marker lives on the PVC root (outside PGDATA) so it survives cleanup.
MARKER="/var/lib/postgresql/data/.pg-swarm-needs-basebackup"
NEEDS_BASEBACKUP=false
if [ -f "$MARKER" ]; then
    echo "Found needs-basebackup marker — previous re-basebackup failed, retrying"
    NEEDS_BASEBACKUP=true
    find "$PGDATA" -mindepth 1 -delete 2>/dev/null || true
    rm -f "$MARKER"
fi

# Clean stale partial data from a previous failed init (no PG_VERSION but dir not empty)
if [ -d "$PGDATA" ] && [ -n "$(ls -A "$PGDATA" 2>/dev/null)" ]; then
    echo "Cleaning stale PGDATA (no PG_VERSION but directory not empty)"
    find "$PGDATA" -mindepth 1 -delete 2>/dev/null || true
fi

if [ "$NEEDS_BASEBACKUP" = "false" ] && [ "$ORDINAL" = "0" ]; then
    echo "Initializing primary (ordinal 0)"
    initdb -D "$PGDATA" --auth-local=trust --auth-host=scram-sha-256

    # Set superuser password (ensure SCRAM hashing)
    pg_ctl -D "$PGDATA" start -w -o "-c listen_addresses='localhost' -c password_encryption='scram-sha-256'"
    psql -U postgres -c "ALTER USER postgres PASSWORD '$POSTGRES_PASSWORD';"
    psql -U postgres -c "CREATE ROLE repl_user WITH REPLICATION LOGIN PASSWORD '$REPLICATION_PASSWORD';"
    psql -U postgres -c "CREATE ROLE backup_user WITH REPLICATION LOGIN PASSWORD '$BACKUP_PASSWORD' IN ROLE pg_read_all_data;"
    psql -U postgres -c "CREATE EXTENSION IF NOT EXISTS pg_stat_statements;"
    # Databases are managed at cluster level via sidecar
    pg_ctl -D "$PGDATA" stop -w

    # Copy config
    cp /etc/pg-config/postgresql.conf "$PGDATA/postgresql.conf"
    cp /etc/pg-config/pg_hba.conf "$PGDATA/pg_hba.conf"{{PRIMARY_ARCHIVE}}{{WAL_SYMLINK}}
else
    echo "Initializing replica (ordinal $ORDINAL)"
    # PRIMARY_HOST is set at the top of the script (RW service follows pg-swarm.io/role=primary).
    until pg_isready -h "$PRIMARY_HOST" -U postgres; do
        echo "Waiting for primary..."
        sleep 2
    done
    PGPASSWORD="$REPLICATION_PASSWORD" pg_basebackup \
        -h "$PRIMARY_HOST" -U repl_user -D "$PGDATA" -R -Xs -P
    cp /etc/pg-config/postgresql.conf "$PGDATA/postgresql.conf"
    cp /etc/pg-config/pg_hba.conf "$PGDATA/pg_hba.conf"{{REPLICA_RESTORE}}{{WAL_SYMLINK}}
fi
