package eventbus

import (
	"context"
	"fmt"
	"sync"
	"testing"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
)

func TestExactMatch(t *testing.T) {
	var called bool
	bus := New(nil)
	bus.Subscribe("cluster.create", "test", func(_ context.Context, evt *pgswarmv1.Event) error {
		called = true
		if evt.GetType() != "cluster.create" {
			t.Errorf("unexpected event type: %s", evt.GetType())
		}
		return nil
	})

	evt := NewEvent("cluster.create", "pg-test", "default", "central")
	_ = bus.Publish(context.Background(), evt)
	if !called {
		t.Fatal("exact-match handler was not called")
	}
}

func TestExactMatch_NoFalsePositive(t *testing.T) {
	var called bool
	bus := New(nil)
	bus.Subscribe("cluster.create", "test", func(_ context.Context, _ *pgswarmv1.Event) error {
		called = true
		return nil
	})

	evt := NewEvent("cluster.delete", "pg-test", "default", "central")
	_ = bus.Publish(context.Background(), evt)
	if called {
		t.Fatal("handler should not be called for non-matching event")
	}
}

func TestPrefixMatch(t *testing.T) {
	var types []string
	bus := New(nil)
	bus.Subscribe("instance.*", "test", func(_ context.Context, evt *pgswarmv1.Event) error {
		types = append(types, evt.GetType())
		return nil
	})

	_ = bus.Publish(context.Background(), NewPodEvent("instance.pg_up", "pg", "ns", "pod-0", "sidecar"))
	_ = bus.Publish(context.Background(), NewPodEvent("instance.pg_down", "pg", "ns", "pod-0", "sidecar"))
	_ = bus.Publish(context.Background(), NewEvent("cluster.create", "pg", "ns", "central"))

	if len(types) != 2 {
		t.Fatalf("expected 2 matches, got %d: %v", len(types), types)
	}
	if types[0] != "instance.pg_up" || types[1] != "instance.pg_down" {
		t.Fatalf("unexpected types: %v", types)
	}
}

func TestWildcardMatch(t *testing.T) {
	var count int
	bus := New(nil)
	bus.Subscribe("*", "audit", func(_ context.Context, _ *pgswarmv1.Event) error {
		count++
		return nil
	})

	_ = bus.Publish(context.Background(), NewEvent("cluster.create", "pg", "ns", "central"))
	_ = bus.Publish(context.Background(), NewPodEvent("instance.pg_down", "pg", "ns", "pod", "sidecar"))
	_ = bus.Publish(context.Background(), NewEvent("reconcile.scheduled", "pg", "ns", "satellite"))

	if count != 3 {
		t.Fatalf("wildcard should match all events, got %d", count)
	}
}

func TestHandlerErrorDoesNotStopDispatch(t *testing.T) {
	var secondCalled bool
	bus := New(nil)

	bus.Subscribe("test.event", "failing", func(_ context.Context, _ *pgswarmv1.Event) error {
		return fmt.Errorf("intentional failure")
	})
	bus.Subscribe("test.event", "succeeding", func(_ context.Context, _ *pgswarmv1.Event) error {
		secondCalled = true
		return nil
	})

	_ = bus.Publish(context.Background(), NewEvent("test.event", "pg", "ns", "satellite"))
	if !secondCalled {
		t.Fatal("second handler should be called despite first handler error")
	}
}

func TestForwardCalledForNonCentralSource(t *testing.T) {
	var forwarded bool
	bus := New(func(evt *pgswarmv1.Event) {
		forwarded = true
	})

	_ = bus.Publish(context.Background(), NewPodEvent("instance.pg_up", "pg", "ns", "pod", "sidecar"))
	if !forwarded {
		t.Fatal("event from sidecar should be forwarded to central")
	}
}

func TestForwardSkippedForCentralSource(t *testing.T) {
	var forwarded bool
	bus := New(func(_ *pgswarmv1.Event) {
		forwarded = true
	})

	_ = bus.Publish(context.Background(), NewEvent("cluster.create", "pg", "ns", "central"))
	if forwarded {
		t.Fatal("event from central should NOT be forwarded back to central")
	}
}

func TestNilEvent(t *testing.T) {
	bus := New(nil)
	if err := bus.Publish(context.Background(), nil); err != nil {
		t.Fatalf("nil event should be a no-op, got: %v", err)
	}
}

func TestConcurrentPublish(t *testing.T) {
	var mu sync.Mutex
	var count int
	bus := New(nil)
	bus.Subscribe("test.*", "counter", func(_ context.Context, _ *pgswarmv1.Event) error {
		mu.Lock()
		count++
		mu.Unlock()
		return nil
	})

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = bus.Publish(context.Background(), NewEvent(
				fmt.Sprintf("test.event_%d", i), "pg", "ns", "satellite",
			))
		}(i)
	}
	wg.Wait()

	if count != 100 {
		t.Fatalf("expected 100 events processed, got %d", count)
	}
}

func TestHandlerCount(t *testing.T) {
	bus := New(nil)
	bus.Subscribe("cluster.create", "h1", func(_ context.Context, _ *pgswarmv1.Event) error { return nil })
	bus.Subscribe("cluster.delete", "h2", func(_ context.Context, _ *pgswarmv1.Event) error { return nil })
	bus.Subscribe("instance.*", "h3", func(_ context.Context, _ *pgswarmv1.Event) error { return nil })

	if bus.HandlerCount() != 3 {
		t.Fatalf("expected 3 handlers, got %d", bus.HandlerCount())
	}
}

func TestEventHelpers(t *testing.T) {
	evt := NewPodEvent("instance.pg_down", "pg-test", "production", "pg-test-1", "sidecar")
	WithData(evt, "error", "connection refused")
	WithData(evt, "down_count", "3")
	WithSeverity(evt, "error")
	WithOperationID(evt, "op-123")

	if evt.GetType() != "instance.pg_down" {
		t.Errorf("type: got %s", evt.GetType())
	}
	if evt.GetPodName() != "pg-test-1" {
		t.Errorf("pod: got %s", evt.GetPodName())
	}
	if evt.GetSeverity() != "error" {
		t.Errorf("severity: got %s", evt.GetSeverity())
	}
	if evt.Data["error"] != "connection refused" {
		t.Errorf("data[error]: got %s", evt.Data["error"])
	}
	if evt.Data["down_count"] != "3" {
		t.Errorf("data[down_count]: got %s", evt.Data["down_count"])
	}
	if evt.GetOperationId() != "op-123" {
		t.Errorf("operation_id: got %s", evt.GetOperationId())
	}
	if evt.GetId() == "" {
		t.Error("event should have a UUID")
	}
	if evt.GetTimestamp() == nil {
		t.Error("event should have a timestamp")
	}
}
