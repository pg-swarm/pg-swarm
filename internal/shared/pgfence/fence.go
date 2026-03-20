package pgfence

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PGExecer is the subset of *pgx.Conn / *pgxpool.Pool needed for fencing.
type PGExecer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// FenceOpts controls fencing behavior.
type FenceOpts struct {
	// DrainTimeout is how long to wait for active transactions to finish
	// before terminating connections. Zero means immediate kill.
	DrainTimeout time.Duration
}

// FencePrimary fences a PostgreSQL primary so it cannot accept writes.
// This is the aggressive mode — no drain, immediate kill of client connections.
func FencePrimary(ctx context.Context, db PGExecer) error {
	return FencePrimaryWithOpts(ctx, db, FenceOpts{})
}

// FencePrimaryWithOpts fences a primary with configurable drain behavior.
//
// Steps:
//  1. Set default_transaction_read_only = on and reload config (blocks new writes)
//  2. If DrainTimeout > 0, poll for active transactions to complete
//  3. Terminate all remaining client backend connections (preserves replication)
//  4. Verify the fence took effect
//
// All steps are attempted even if earlier ones fail. A drain timeout is not
// treated as an error — remaining connections are killed in step 3.
//
// We intentionally do NOT lower max_connections because ALTER SYSTEM persists
// in postgresql.auto.conf. If PG restarts after demotion with max_connections=1,
// it fails: "superuser_reserved_connections (3) must be less than max_connections (1)".
func FencePrimaryWithOpts(ctx context.Context, db PGExecer, opts FenceOpts) error {
	var errs []error

	// Step 1: Block new writes.
	if _, err := db.Exec(ctx, "ALTER SYSTEM SET default_transaction_read_only = on;"); err != nil {
		errs = append(errs, err)
	}
	if _, err := db.Exec(ctx, "SELECT pg_reload_conf();"); err != nil {
		errs = append(errs, err)
	}

	// Step 2: Drain active transactions (only if DrainTimeout > 0).
	if opts.DrainTimeout > 0 {
		deadline := time.After(opts.DrainTimeout)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

	drain:
		for {
			var count int
			row := db.QueryRow(ctx,
				"SELECT count(*) FROM pg_stat_activity"+
					" WHERE pid != pg_backend_pid()"+
					" AND backend_type = 'client backend'"+
					" AND state IN ('active', 'idle in transaction')")
			if err := row.Scan(&count); err != nil || count == 0 {
				break
			}
			select {
			case <-deadline:
				break drain
			case <-ctx.Done():
				break drain
			case <-ticker.C:
			}
		}
	}

	// Step 3: Terminate remaining client connections.
	if _, err := db.Exec(ctx,
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE pid != pg_backend_pid() AND backend_type = 'client backend';",
	); err != nil {
		errs = append(errs, err)
	}

	// Step 4: Verify fence.
	var readOnly string
	row := db.QueryRow(ctx, "SHOW default_transaction_read_only")
	if err := row.Scan(&readOnly); err != nil {
		errs = append(errs, fmt.Errorf("fence verify: %w", err))
	} else if readOnly != "on" {
		errs = append(errs, fmt.Errorf("fence verify: default_transaction_read_only is %q, want \"on\"", readOnly))
	}

	var remaining int
	row = db.QueryRow(ctx,
		"SELECT count(*) FROM pg_stat_activity WHERE pid != pg_backend_pid() AND backend_type = 'client backend'")
	if err := row.Scan(&remaining); err != nil {
		errs = append(errs, fmt.Errorf("fence verify: %w", err))
	} else if remaining != 0 {
		errs = append(errs, fmt.Errorf("fence verify: %d client backends still connected", remaining))
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
