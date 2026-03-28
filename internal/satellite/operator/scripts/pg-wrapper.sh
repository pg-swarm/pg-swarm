#!/bin/bash
# pg-swarm wrapper (sentinel-enabled): keeps the container alive across PG restarts.
#
# Recovery decisions are made by the sentinel sidecar (leader lease, PGDATA
# deletion detection, WAL monitoring, log-based pattern matching). This wrapper
# executes the actions the sentinel requests via marker files and standby.signal.
#
# Only a K8s SIGTERM (pod deletion / rolling update) exits the container.

PRIMARY_HOST="{{PRIMARY_HOST}}"
ORDINAL=${POD_NAME##*-}

# --- Compute WAL segment name from a checkpoint LSN ---
pg_swarm_wal_segments() {
    CTLDATA=$(pg_controldata -D "$PGDATA" 2>/dev/null)
    CKPT_WAL=$(echo "$CTLDATA" | grep "REDO WAL file" | awk '{print $NF}')
    CKPT_LOC=$(echo "$CTLDATA" | grep "Latest checkpoint location" | awk '{print $NF}')
    CKPT_SEG_SIZE=$(echo "$CTLDATA" | grep "Bytes per WAL segment" | awk '{print $NF}')
    CKPT_REC_WAL=""
    if [ -n "$CKPT_WAL" ] && [ -n "$CKPT_LOC" ]; then
        _HI=$(printf '%d' "0x${CKPT_LOC%/*}")
        _LO=$(printf '%d' "0x${CKPT_LOC#*/}")
        _SEG=$(( _LO / ${CKPT_SEG_SIZE:-16777216} ))
        CKPT_REC_WAL=$(printf "%s%08X%08X" "${CKPT_WAL:0:8}" "$_HI" "$_SEG")
    fi
}

# --- Timeline recovery (simplified for sentinel-enabled mode) ---
# The sentinel's doRewind() writes standby.signal + stops PG, then this
# function handles the actual pg_rewind. On failure, writes the basebackup
# marker and returns — the marker handler picks it up next loop.
pg_swarm_recover() {
    if [ ! -f "$PGDATA/standby.signal" ] || [ ! -f "$PGDATA/PG_VERSION" ]; then
        return
    fi

    LOCAL_TLI=$(pg_controldata -D "$PGDATA" 2>/dev/null | grep "Latest checkpoint.s TimeLineID" | awk '{print $NF}')
    echo "pg-swarm: checking timeline, local=${LOCAL_TLI:-unknown}"

    for i in $(seq 1 6); do
        if pg_isready -h "$PRIMARY_HOST" -U postgres -t 5 >/dev/null 2>&1; then break; fi
        echo "pg-swarm: waiting for primary ($i/6)..."
        if [ "$i" = "6" ]; then
            echo "pg-swarm: primary not reachable — will start PG anyway"
            return
        fi
    done

    PRIMARY_TLI=$(PGPASSWORD="$POSTGRES_PASSWORD" psql -h "$PRIMARY_HOST" -U postgres -d postgres -tAc "SELECT timeline_id FROM pg_control_checkpoint()" 2>/dev/null || echo "")

    if [ -z "$PRIMARY_TLI" ] || [ -z "$LOCAL_TLI" ] || [ "$LOCAL_TLI" = "$PRIMARY_TLI" ]; then
        return
    fi

    echo "pg-swarm: TIMELINE DIVERGENCE local=$LOCAL_TLI primary=$PRIMARY_TLI"

    # Ensure clean shutdown state for pg_rewind
    DB_STATE=$(pg_controldata -D "$PGDATA" 2>/dev/null | grep "Database cluster state" | sed 's/.*: *//')
    case "$DB_STATE" in
        "shut down"|"shut down in recovery") ;;
        *)
            echo "pg-swarm: unclean state ($DB_STATE) — running single-user recovery"
            postgres --single -D "$PGDATA" -c config_file="$PGDATA/postgresql.conf" </dev/null >/dev/null 2>&1 || true
            ;;
    esac

    if PGPASSWORD="$POSTGRES_PASSWORD" pg_rewind \
        -D "$PGDATA" \
        --source-server="host=$PRIMARY_HOST port=5432 user=postgres password=$POSTGRES_PASSWORD dbname=postgres" \
        --progress 2>&1; then
        echo "pg-swarm: pg_rewind succeeded"
        rm -f "$PGDATA/backup_label" "$PGDATA/tablespace_map"
        pg_swarm_wal_segments
        if [ -n "$CKPT_WAL" ]; then
            echo "pg-swarm: cleaning stale WAL segments after pg_rewind (keeping $CKPT_WAL${CKPT_REC_WAL:+ and $CKPT_REC_WAL})"
            find "$PGDATA/pg_wal" -maxdepth 1 -type f \
                ! -name "$CKPT_WAL" ! -name "${CKPT_REC_WAL:-$CKPT_WAL}" ! -name "*.history" ! -name "*.backup" \
                -delete 2>/dev/null || true
        fi
        if [ -n "$CKPT_WAL" ] && [ ! -f "$PGDATA/pg_wal/$CKPT_WAL" ]; then
            echo "pg-swarm: checkpoint WAL missing after pg_rewind — requesting re-basebackup"
            touch "/var/lib/postgresql/data/.pg-swarm-needs-basebackup"
            return
        fi
    else
        echo "pg-swarm: pg_rewind failed — requesting re-basebackup"
        if ! pg_isready -h "$PRIMARY_HOST" -U postgres -t 5 >/dev/null 2>&1; then
            echo "pg-swarm: primary not reachable — skipping destructive recovery to preserve data"
            return
        fi
        touch "/var/lib/postgresql/data/.pg-swarm-needs-basebackup"
        return
    fi

    touch "$PGDATA/standby.signal"
    sed -i '/^primary_conninfo/d' "$PGDATA/postgresql.auto.conf" 2>/dev/null || true
    echo "primary_conninfo = 'host=$PRIMARY_HOST port=5432 user=repl_user password=$REPLICATION_PASSWORD application_name=$POD_NAME'" >> "$PGDATA/postgresql.auto.conf"
    echo "pg-swarm: timeline recovery complete"
}

# --- Helper: nuke PGDATA and re-basebackup from primary ---
pg_swarm_rebasebackup() {
    local reason="$1"
    local marker="/var/lib/postgresql/data/.pg-swarm-needs-basebackup"
    echo "pg-swarm: $reason — starting full re-basebackup"
    find "$PGDATA" -mindepth 1 -delete 2>/dev/null || true
    for i in $(seq 1 12); do
        # If the sentinel removed the marker (emergency deadlock breaker),
        # abort so the wrapper can start PG directly.
        if [ ! -f "$marker" ]; then
            echo "pg-swarm: basebackup marker removed by sentinel — aborting rebasebackup"
            return 1
        fi
        if pg_isready -h "$PRIMARY_HOST" -U postgres -t 2 >/dev/null 2>&1; then
            if PGPASSWORD="$REPLICATION_PASSWORD" pg_basebackup \
                -h "$PRIMARY_HOST" -U repl_user -D "$PGDATA" -R -Xs -P; then
                cp /etc/pg-config/postgresql.conf "$PGDATA/postgresql.conf"
                cp /etc/pg-config/pg_hba.conf "$PGDATA/pg_hba.conf"
                # pg_basebackup -R writes primary_conninfo without a password; fix it now.
                sed -i '/^primary_conninfo/d' "$PGDATA/postgresql.auto.conf" 2>/dev/null || true
                echo "primary_conninfo = 'host=$PRIMARY_HOST port=5432 user=repl_user password=$REPLICATION_PASSWORD application_name=$POD_NAME'" >> "$PGDATA/postgresql.auto.conf"
                rm -f "$marker"
                return 0
            fi
        fi
        sleep 5
    done
    echo "pg-swarm: re-basebackup failed after 12 attempts"
    return 1
}

# --- Main loop ---
SHUTTING_DOWN=false
CRASH_COUNT=0
trap 'SHUTTING_DOWN=true; kill -TERM $PG_PID 2>/dev/null' TERM

while true; do
    # Crash-loop handler: replicas do a full re-basebackup; for primaries,
    # the sentinel yields the lease so a healthy replica can promote.
    if [ "$CRASH_COUNT" -ge 3 ]; then
        echo "pg-swarm: CRASH LOOP DETECTED ($CRASH_COUNT consecutive fast crashes)"
        if [ "$ORDINAL" != "0" ]; then
            if pg_swarm_rebasebackup "crash loop — PGDATA is unrecoverable"; then
                CRASH_COUNT=0
                continue
            fi
        else
            echo "pg-swarm: primary crash loop — waiting for sentinel to yield lease"
            sleep 15
        fi
        CRASH_COUNT=0
    fi

    # Check and fix timeline divergence before starting PG
    pg_swarm_recover

    # Check if a forced re-basebackup was requested by the sentinel (e.g. WAL
    # gap, PGDATA loss). All pods honour this marker — after failover, ordinal 0
    # is no longer the primary and must rebuild from the new primary.
    MARKER="/var/lib/postgresql/data/.pg-swarm-needs-basebackup"
    if [ -f "$MARKER" ]; then
        echo "pg-swarm: forced re-basebackup requested"
        pg_swarm_rebasebackup "basebackup marker found" || { sleep 5; continue; }
        continue
    fi

    # Guard: corrupt PGDATA (no PG_VERSION but dir not empty). Clean up and
    # trigger re-basebackup. On first boot (no sentinel marker), let initdb run.
    SENTINEL="/var/lib/postgresql/data/.pg-swarm-initialized"
    if [ -d "$PGDATA" ] && [ -n "$(ls -A "$PGDATA" 2>/dev/null)" ] && [ ! -s "$PGDATA/PG_VERSION" ]; then
        echo "pg-swarm: corrupt PGDATA (no PG_VERSION) — cleaning up"
        find "$PGDATA" -mindepth 1 -delete 2>/dev/null || true
        if [ -f "$SENTINEL" ]; then
            touch "$MARKER"
            pg_swarm_rebasebackup "corrupt PGDATA" || { sleep 5; continue; }
            continue
        fi
        # First boot: clean slate for docker-entrypoint.sh initdb
    fi

    # Guard: wal_level=minimal in controldata means replicas cannot recover.
    if [ -f "$PGDATA/PG_VERSION" ] && [ -f "$PGDATA/standby.signal" ]; then
        WAL_LEVEL=$(pg_controldata -D "$PGDATA" 2>/dev/null | grep "wal_level setting" | awk '{print $NF}')
        if [ "$WAL_LEVEL" = "minimal" ]; then
            echo "pg-swarm: CRITICAL — wal_level=minimal in controldata, replica cannot recover"
            pg_swarm_rebasebackup "wal_level=minimal — unrecoverable" || { sleep 5; continue; }
        fi
    fi

    # Start PG
    PG_START=$(date +%s)
    docker-entrypoint.sh postgres &
    PG_PID=$!

    # Mark that this pod has been initialized at least once.
    if [ ! -f "$SENTINEL" ]; then
        for _i in $(seq 1 10); do
            if pg_isready -t 1 >/dev/null 2>&1; then
                touch "$SENTINEL"
                echo "pg-swarm: initialization sentinel written"
                break
            fi
            sleep 1
        done
    fi

    wait $PG_PID
    EXIT_CODE=$?

    if [ "$SHUTTING_DOWN" = "true" ]; then
        echo "pg-swarm: shutting down"
        exit 0
    fi

    # Track fast crashes for crash-loop detection
    PG_ELAPSED=$(( $(date +%s) - PG_START ))
    if [ "$PG_ELAPSED" -lt 30 ]; then
        CRASH_COUNT=$((CRASH_COUNT + 1))
        echo "pg-swarm: postgres exited after ${PG_ELAPSED}s (code=$EXIT_CODE, crash $CRASH_COUNT/3)"
    else
        CRASH_COUNT=0
        echo "pg-swarm: postgres exited after ${PG_ELAPSED}s (code=$EXIT_CODE) — recovering in-place"
    fi
    sleep 2
done
