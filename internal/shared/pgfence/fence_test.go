package pgfence

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// mockExecer records Exec calls and returns pre-configured errors.
type mockExecer struct {
	calls []string
	errs  map[string]error
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

func (m *mockExecer) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return nil
}

func TestFencePrimary_AllSucceed(t *testing.T) {
	m := newMockExecer(nil)
	if err := FencePrimary(context.Background(), m); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(m.calls) != 3 {
		t.Fatalf("expected 3 SQL calls, got %d", len(m.calls))
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
	if len(m.calls) != 3 {
		t.Fatalf("expected all 3 calls despite first failure, got %d", len(m.calls))
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
		"ALTER SYSTEM SET default_transaction_read_only = on;":                                                                      e1,
		"SELECT pg_reload_conf();":                                                                                                  e2,
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
