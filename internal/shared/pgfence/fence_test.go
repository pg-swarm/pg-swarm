package pgfence

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// mockRow implements pgx.Row for testing.
type mockRow struct {
	scanFn func(dest ...any) error
}

func (r *mockRow) Scan(dest ...any) error {
	if r.scanFn != nil {
		return r.scanFn(dest...)
	}
	return nil
}

// defaultVerifyRow returns mock rows that make verification pass:
// SHOW → "on", count → 0.
func defaultVerifyRow(sql string) pgx.Row {
	if strings.HasPrefix(sql, "SHOW") {
		return &mockRow{scanFn: func(dest ...any) error {
			*dest[0].(*string) = "on"
			return nil
		}}
	}
	return &mockRow{scanFn: func(dest ...any) error {
		*dest[0].(*int) = 0
		return nil
	}}
}

// mockExecer records Exec/QueryRow calls and returns pre-configured responses.
type mockExecer struct {
	calls      []string
	errs       map[string]error
	queryRowFn func(sql string) pgx.Row
}

func newMockExecer(errs map[string]error) *mockExecer {
	if errs == nil {
		errs = map[string]error{}
	}
	return &mockExecer{errs: errs}
}

func (m *mockExecer) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	m.calls = append(m.calls, sql)
	return pgconn.NewCommandTag("OK"), m.errs[sql]
}

func (m *mockExecer) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	m.calls = append(m.calls, sql)
	if m.queryRowFn != nil {
		return m.queryRowFn(sql)
	}
	return defaultVerifyRow(sql)
}

// --- FencePrimary backward-compat tests ---

func TestFencePrimary_AllSucceed(t *testing.T) {
	m := newMockExecer(nil)
	if err := FencePrimary(context.Background(), m); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	// 3 Exec (alter, reload, terminate) + 2 QueryRow (verify)
	if len(m.calls) != 5 {
		t.Fatalf("expected 5 SQL calls, got %d: %v", len(m.calls), m.calls)
	}
}

func TestFencePrimary_AlterFails_StillAttemptsAll(t *testing.T) {
	alterErr := errors.New("alter failed")
	m := newMockExecer(map[string]error{
		"ALTER SYSTEM SET default_transaction_read_only = on;": alterErr,
	})
	err := FencePrimary(context.Background(), m)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, alterErr) {
		t.Fatalf("expected alter error in result, got %v", err)
	}
	// All 5 calls still attempted despite first failure
	if len(m.calls) != 5 {
		t.Fatalf("expected 5 calls despite first failure, got %d: %v", len(m.calls), m.calls)
	}
}

func TestFencePrimary_TerminateFails(t *testing.T) {
	termErr := errors.New("terminate failed")
	m := newMockExecer(map[string]error{
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE pid != pg_backend_pid() AND backend_type = 'client backend';": termErr,
	})
	err := FencePrimary(context.Background(), m)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, termErr) {
		t.Fatalf("expected terminate error in result, got %v", err)
	}
}

func TestFencePrimary_AllFail(t *testing.T) {
	e1 := errors.New("err1")
	e2 := errors.New("err2")
	e3 := errors.New("err3")
	m := newMockExecer(map[string]error{
		"ALTER SYSTEM SET default_transaction_read_only = on;": e1,
		"SELECT pg_reload_conf();":                             e2,
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE pid != pg_backend_pid() AND backend_type = 'client backend';": e3,
	})
	err := FencePrimary(context.Background(), m)
	if err == nil {
		t.Fatal("expected error")
	}
	for _, e := range []error{e1, e2, e3} {
		if !errors.Is(err, e) {
			t.Errorf("expected %v in combined error", e)
		}
	}
}

func TestUnfencePrimary_Success(t *testing.T) {
	m := newMockExecer(nil)
	if err := UnfencePrimary(context.Background(), m); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(m.calls) != 2 {
		t.Fatalf("expected 2 SQL calls, got %d", len(m.calls))
	}
}

// --- FencePrimaryWithOpts tests ---

func TestFencePrimaryWithOpts_Immediate(t *testing.T) {
	m := newMockExecer(nil)
	err := FencePrimaryWithOpts(context.Background(), m, FenceOpts{})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	for _, sql := range m.calls {
		if strings.Contains(sql, "state IN") {
			t.Fatal("drain query should not be issued in immediate mode")
		}
	}
	// 3 Exec + 2 QueryRow = 5
	if len(m.calls) != 5 {
		t.Fatalf("expected 5 calls, got %d: %v", len(m.calls), m.calls)
	}
}

func TestFencePrimaryWithOpts_DrainCompletes(t *testing.T) {
	m := newMockExecer(nil)
	// Default mock returns count=0 for all count queries, so drain
	// completes immediately on first poll.
	err := FencePrimaryWithOpts(context.Background(), m, FenceOpts{DrainTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	hasDrain := false
	for _, sql := range m.calls {
		if strings.Contains(sql, "state IN") {
			hasDrain = true
			break
		}
	}
	if !hasDrain {
		t.Fatal("expected drain query in graceful mode")
	}
}

func TestFencePrimaryWithOpts_DrainTimesOut(t *testing.T) {
	m := newMockExecer(nil)
	m.queryRowFn = func(sql string) pgx.Row {
		if strings.Contains(sql, "state IN") {
			return &mockRow{scanFn: func(dest ...any) error {
				*dest[0].(*int) = 5 // always has active connections
				return nil
			}}
		}
		return defaultVerifyRow(sql)
	}
	err := FencePrimaryWithOpts(context.Background(), m, FenceOpts{DrainTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("expected no error (drain timeout is not an error), got %v", err)
	}
	hasDrain := false
	hasTerm := false
	for _, sql := range m.calls {
		if strings.Contains(sql, "state IN") {
			hasDrain = true
		}
		if strings.Contains(sql, "pg_terminate_backend") {
			hasTerm = true
		}
	}
	if !hasDrain {
		t.Fatal("expected at least one drain query")
	}
	if !hasTerm {
		t.Fatal("expected terminate query after drain timeout")
	}
}

func TestFencePrimaryWithOpts_VerifyReadOnlyFails(t *testing.T) {
	m := newMockExecer(nil)
	m.queryRowFn = func(sql string) pgx.Row {
		if strings.HasPrefix(sql, "SHOW") {
			return &mockRow{scanFn: func(dest ...any) error {
				*dest[0].(*string) = "off"
				return nil
			}}
		}
		return defaultVerifyRow(sql)
	}
	err := FencePrimaryWithOpts(context.Background(), m, FenceOpts{})
	if err == nil {
		t.Fatal("expected verification error")
	}
	if !strings.Contains(err.Error(), "fence verify") {
		t.Fatalf("expected 'fence verify' in error, got %v", err)
	}
}

func TestFencePrimaryWithOpts_VerifyClientsFail(t *testing.T) {
	m := newMockExecer(nil)
	m.queryRowFn = func(sql string) pgx.Row {
		if strings.HasPrefix(sql, "SHOW") {
			return &mockRow{scanFn: func(dest ...any) error {
				*dest[0].(*string) = "on"
				return nil
			}}
		}
		// Verify count query (no state filter) returns remaining clients.
		if strings.Contains(sql, "count(*)") && !strings.Contains(sql, "state IN") {
			return &mockRow{scanFn: func(dest ...any) error {
				*dest[0].(*int) = 3
				return nil
			}}
		}
		return defaultVerifyRow(sql)
	}
	err := FencePrimaryWithOpts(context.Background(), m, FenceOpts{})
	if err == nil {
		t.Fatal("expected verification error")
	}
	if !strings.Contains(err.Error(), "client backends still connected") {
		t.Fatalf("expected 'client backends still connected' in error, got %v", err)
	}
}
