#!/bin/bash
# pg-swarm standalone wrapper: keeps the container alive across PG restarts.
# Used when the sentinel sidecar is DISABLED — all recovery logic is self-contained.
#
# When PG crashes from a timeline mismatch or other error, the CONTAINER stays
# running. The wrapper detects the exit, runs pg_rewind if needed, and restarts
# PG in-place — no K8s container restart, no restart counter increment.
#
# Only a K8s SIGTERM (pod deletion / rolling update) exits the container.
# We distinguish the two cases via a SHUTTING_DOWN flag set by the trap.

PRIMARY_HOST="{{PRIMARY_HOST}}"
ORDINAL=${POD_NAME##*-}

# --- Compute WAL segment name from a checkpoint LSN ---
# Reads REDO WAL file + Latest checkpoint location from pg_controldata,
# and derives the segment containing the checkpoint record itself.
# Sets: CKPT_WAL (REDO file), CKPT_REC_WAL (checkpoint record file).
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

# --- Timeline recovery function ---
pg_swarm_recover() {
    if [ ! -f "$PGDATA/standby.signal" ] || [ ! -f "$PGDATA/PG_VERSION" ]; then
        return
    fi

    LOCAL_TLI=$(pg_controldata -D "$PGDATA" 2>/dev/null | grep "Latest checkpoint.s TimeLineID" | awk '{print $NF}')
    echo "pg-swarm: checking timeline, local=${LOCAL_TLI:-unknown}"

    # Wait for the primary (up to 30s)
    for i in $(seq 1 6); do
        if pg_isready -h "$PRIMARY_HOST" -U postgres -t 5 >/dev/null 2>&1; then
            break
        fi
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
        "shut down"|"shut down in recovery")
            ;;
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
        if [ -f "$PGDATA/backup_label" ]; then echo "pg-swarm: removing stale backup_label after pg_rewind"; rm -f "$PGDATA/backup_label"; fi
        if [ -f "$PGDATA/tablespace_map" ]; then echo "pg-swarm: removing stale tablespace_map after pg_rewind"; rm -f "$PGDATA/tablespace_map"; fi
        # Clean up stale/pre-allocated WAL segments to prevent "invalid record length".
        # Keep BOTH the REDO WAL file (replay start) AND the segment holding the
        # checkpoint record itself — they can be in different segments when a
        # checkpoint spans a WAL segment boundary.
        pg_swarm_wal_segments
        if [ -n "$CKPT_WAL" ]; then
            echo "pg-swarm: cleaning stale WAL segments after pg_rewind (keeping $CKPT_WAL${CKPT_REC_WAL:+ and $CKPT_REC_WAL})"
            find "$PGDATA/pg_wal" -maxdepth 1 -type f \
                ! -name "$CKPT_WAL" ! -name "${CKPT_REC_WAL:-$CKPT_WAL}" ! -name "*.history" ! -name "*.backup" \
                -delete 2>/dev/null || true
        fi
        if [ -n "$CKPT_WAL" ] && [ ! -f "$PGDATA/pg_wal/$CKPT_WAL" ]; then
            echo "pg-swarm: checkpoint WAL $CKPT_WAL missing after pg_rewind — falling back to pg_basebackup"
            if ! pg_isready -h "$PRIMARY_HOST" -U postgres -t 5 >/dev/null 2>&1; then
                echo "pg-swarm: primary not reachable — skipping destructive recovery to preserve data"
                return
            fi
            MARKER="/var/lib/postgresql/data/.pg-swarm-needs-basebackup"
            touch "$MARKER"
            find "$PGDATA" -mindepth 1 -delete 2>/dev/null || true
            if PGPASSWORD="$REPLICATION_PASSWORD" pg_basebackup \
                -h "$PRIMARY_HOST" -U repl_user -D "$PGDATA" -R -Xs -P; then
                rm -f "$MARKER"
                cp /etc/pg-config/postgresql.conf "$PGDATA/postgresql.conf"
                cp /etc/pg-config/pg_hba.conf "$PGDATA/pg_hba.conf"
            else
                echo "pg-swarm: pg_basebackup failed — will retry on next loop iteration"
                return
            fi
        fi
    else
        echo "pg-swarm: pg_rewind failed — checking if primary is available for re-basebackup"
        if ! pg_isready -h "$PRIMARY_HOST" -U postgres -t 5 >/dev/null 2>&1; then
            echo "pg-swarm: primary not reachable — skipping destructive recovery to preserve data"
            return
        fi
        MARKER="/var/lib/postgresql/data/.pg-swarm-needs-basebackup"
        touch "$MARKER"
        find "$PGDATA" -mindepth 1 -delete 2>/dev/null || true
        if PGPASSWORD="$REPLICATION_PASSWORD" pg_basebackup \
            -h "$PRIMARY_HOST" -U repl_user -D "$PGDATA" -R -Xs -P; then
            rm -f "$MARKER"
            cp /etc/pg-config/postgresql.conf "$PGDATA/postgresql.conf"
            cp /etc/pg-config/pg_hba.conf "$PGDATA/pg_hba.conf"
        else
            echo "pg-swarm: pg_basebackup failed — will retry on next loop iteration"
            return
        fi
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
        if [ ! -f "$marker" ]; then
            echo "pg-swarm: basebackup marker removed — aborting rebasebackup"
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
    # Crash-loop breaker: if PG crashed quickly 3 times in a row the data
    # directory is broken beyond what pg_rewind/guards can fix.
    # Force a full re-basebackup (replicas) or pg_resetwal (primary).
    if [ "$CRASH_COUNT" -ge 3 ]; then
        echo "pg-swarm: CRASH LOOP DETECTED ($CRASH_COUNT consecutive fast crashes)"
        if [ "$ORDINAL" != "0" ]; then
            if pg_swarm_rebasebackup "crash loop — PGDATA is unrecoverable"; then
                CRASH_COUNT=0
                continue
            fi
        else
            echo "pg-swarm: primary crash loop — attempting pg_resetwal"
            pg_resetwal -f -D "$PGDATA" 2>&1 || true
        fi
        CRASH_COUNT=0
    fi

    # Check and fix timeline divergence before starting PG
    pg_swarm_recover

    # Check if a forced re-basebackup was requested (e.g. WAL gap, PGDATA loss).
    # All pods — including ordinal 0 — honour this marker because after a
    # failover, ordinal 0 is no longer the primary and must rebuild from
    # the new primary.
    MARKER="/var/lib/postgresql/data/.pg-swarm-needs-basebackup"
    if [ -f "$MARKER" ]; then
        echo "pg-swarm: forced re-basebackup requested"
        pg_swarm_rebasebackup "basebackup marker found" || { sleep 5; continue; }
        continue
    fi

    # Guard: PGDATA is empty on primary after it was previously initialized
    # — this means PGDATA was deleted at runtime. Yield for failover, then
    # re-basebackup from the new primary once it's available.
    SENTINEL="/var/lib/postgresql/data/.pg-swarm-initialized"
    if [ "$ORDINAL" = "0" ] && [ -d "$PGDATA" ] && [ -z "$(ls -A "$PGDATA" 2>/dev/null)" ] && [ -f "$SENTINEL" ]; then
        echo "pg-swarm: PRIMARY PGDATA is empty but was previously initialized — yielding for failover"
        # Sleep to let the lease expire and a replica promote.
        sleep 30
        echo "pg-swarm: attempting re-basebackup from new primary"
        rm -f "$SENTINEL"
        pg_swarm_rebasebackup "primary PGDATA lost — rebuilding from new primary" || { sleep 5; continue; }
        continue
    fi

    # Guard: if PGDATA has files but no PG_VERSION, it is corrupt (e.g. a
    # previous pg_basebackup failed partway through). Clean up and either
    # re-basebackup (replicas) or yield for failover (primary, if previously initialized).
    if [ -d "$PGDATA" ] && [ -n "$(ls -A "$PGDATA" 2>/dev/null)" ] && [ ! -s "$PGDATA/PG_VERSION" ]; then
        echo "pg-swarm: corrupt PGDATA (no PG_VERSION) — cleaning up"
        if [ "$ORDINAL" != "0" ]; then
            pg_swarm_rebasebackup "corrupt PGDATA" || { sleep 5; continue; }
        elif [ -f "$SENTINEL" ]; then
            echo "pg-swarm: PRIMARY PGDATA lost at runtime — yielding for failover"
            sleep 30
            echo "pg-swarm: attempting re-basebackup from new primary"
            find "$PGDATA" -mindepth 1 -delete 2>/dev/null || true
            rm -f "$SENTINEL"
            pg_swarm_rebasebackup "primary PGDATA corrupt — rebuilding from new primary" || { sleep 5; continue; }
            continue
        else
            # First boot: clean up and let docker-entrypoint.sh initdb
            find "$PGDATA" -mindepth 1 -delete 2>/dev/null || true
        fi
    fi

    # Final guard: verify BOTH the REDO WAL file and the checkpoint record
    # WAL segment exist before starting PG. They can be different files when
    # a checkpoint spans a segment boundary.
    if [ -f "$PGDATA/PG_VERSION" ]; then
        pg_swarm_wal_segments
        WAL_MISSING=false
        if [ -n "$CKPT_WAL" ] && [ ! -f "$PGDATA/pg_wal/$CKPT_WAL" ]; then
            echo "pg-swarm: CRITICAL — REDO WAL $CKPT_WAL missing from pg_wal/"
            WAL_MISSING=true
        fi
        if [ -n "$CKPT_REC_WAL" ] && [ ! -f "$PGDATA/pg_wal/$CKPT_REC_WAL" ]; then
            echo "pg-swarm: CRITICAL — checkpoint record WAL $CKPT_REC_WAL missing from pg_wal/"
            WAL_MISSING=true
        fi
        if [ "$WAL_MISSING" = "true" ]; then
            if [ "$ORDINAL" != "0" ]; then
                pg_swarm_rebasebackup "checkpoint WAL missing" || { sleep 5; continue; }
            else
                echo "pg-swarm: primary — attempting pg_resetwal to recover"
                pg_resetwal -f -D "$PGDATA" 2>&1 || true
            fi
        fi
    fi

    # Guard: wal_level=minimal in controldata means WAL lacks replication
    # info. Replicas cannot recover from it — go straight to rebasebackup.
    # This typically happens after a pg_resetwal on the primary.
    if [ -f "$PGDATA/PG_VERSION" ] && [ -f "$PGDATA/standby.signal" ]; then
        WAL_LEVEL=$(pg_controldata -D "$PGDATA" 2>/dev/null | grep "wal_level setting" | awk '{print $NF}')
        if [ "$WAL_LEVEL" = "minimal" ]; then
            echo "pg-swarm: CRITICAL — wal_level=minimal in controldata, replica cannot recover"
            pg_swarm_rebasebackup "wal_level=minimal — unrecoverable" || { sleep 5; continue; }
        fi
    fi

    # Start PG in the background so we can catch its exit.
    # Tee output to a temp log so we can scan for fatal errors after exit.
    PG_LOG=$(mktemp /tmp/pg-swarm-pglog.XXXXXX)
    PG_START=$(date +%s)
    docker-entrypoint.sh postgres 2>&1 | tee "$PG_LOG" &
    PG_PID=$!

    # Mark that this pod has been initialized at least once.
    # Used to distinguish first boot from runtime PGDATA loss.
    SENTINEL="/var/lib/postgresql/data/.pg-swarm-initialized"
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

    # K8s sent SIGTERM → exit the container cleanly
    if [ "$SHUTTING_DOWN" = "true" ]; then
        rm -f "$PG_LOG"
        echo "pg-swarm: shutting down"
        exit 0
    fi

    # Scan PG output for fatal errors that are immediately unrecoverable.
    # React on the FIRST crash instead of waiting for the crash-loop breaker.
    if [ "$ORDINAL" != "0" ] && [ -f "$PG_LOG" ] && [ ! -f "$MARKER" ]; then
        if grep -qE 'wal_level=minimal.*cannot continue recovering|could not locate a valid checkpoint record|database files are incompatible with server|could not open directory.*pg_wal|could not open file.*pg_filenode\.map' "$PG_LOG"; then
            MATCHED=$(grep -oE 'wal_level=minimal.*cannot continue recovering|could not locate a valid checkpoint record|database files are incompatible with server|could not open directory.*pg_wal|could not open file.*pg_filenode\.map' "$PG_LOG" | head -1)
            echo "pg-swarm: FATAL unrecoverable error detected: $MATCHED"
            rm -f "$PG_LOG"
            pg_swarm_rebasebackup "fatal: $MATCHED" || { sleep 5; continue; }
            CRASH_COUNT=0
            continue
        fi
    fi
    rm -f "$PG_LOG"

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
