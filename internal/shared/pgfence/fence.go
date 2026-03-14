package pgfence

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PGExecer is the subset of *pgx.Conn / *pgxpool.Pool needed for fencing.
type PGExecer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// FencePrimary fences a PostgreSQL primary so it cannot accept writes.
// All steps are attempted even if earlier ones fail.
//
// Steps:
//  1. Set default_transaction_read_only = on (blocks writes)
//  2. Reload config to apply immediately
//  3. Terminate all client backend connections (preserves replication)
//
// We intentionally do NOT lower max_connections because ALTER SYSTEM persists
// in postgresql.auto.conf. If PG restarts after demotion with max_connections=1,
// it fails: "superuser_reserved_connections (3) must be less than max_connections (1)".
func FencePrimary(ctx context.Context, db PGExecer) error {
	var errs []error

	// 1. Block new writes
	if _, err := db.Exec(ctx, "ALTER SYSTEM SET default_transaction_read_only = on;"); err != nil {
		errs = append(errs, err)
	}

	// 2. Apply immediately
	if _, err := db.Exec(ctx, "SELECT pg_reload_conf();"); err != nil {
		errs = append(errs, err)
	}

	// 3. Kill existing client sessions (preserve replication connections)
	if _, err := db.Exec(ctx,
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE pid != pg_backend_pid() AND backend_type = 'client backend';",
	); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// UnfencePrimary reverses fencing when a pod legitimately reacquires the lease.
func UnfencePrimary(ctx context.Context, db PGExecer) error {
	var errs []error

	if _, err := db.Exec(ctx, "ALTER SYSTEM RESET default_transaction_read_only;"); err != nil {
		errs = append(errs, err)
	}

	if _, err := db.Exec(ctx, "SELECT pg_reload_conf();"); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// IsFenced checks whether PG is currently fenced by reading the live
// default_transaction_read_only setting. Returns true if it is 'on'.
func IsFenced(ctx context.Context, db PGExecer) (fenced bool) {
	if db == nil {
		return false
	}
	// QueryRow on a closed/nil *pgx.Conn can panic; treat that as "unknown".
	defer func() {
		if r := recover(); r != nil {
			fenced = false
		}
	}()
	var val string
	row := db.QueryRow(ctx, "SHOW default_transaction_read_only")
	if err := row.Scan(&val); err != nil {
		return false // can't tell, assume not fenced
	}
	return val == "on"
}
