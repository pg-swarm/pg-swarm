package eventbus

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
)

// Handler processes an event. Returning an error logs a warning but does not
// stop dispatch — events are facts, and all subscribers must see them.
type Handler func(ctx context.Context, evt *pgswarmv1.Event) error

// ForwardFunc sends an event to central via the satellite's gRPC stream.
// Nil means forwarding is disabled (e.g., during tests).
type ForwardFunc func(evt *pgswarmv1.Event)

// EventBus routes events to registered handlers. It supports exact-match
// subscriptions ("cluster.create") and prefix subscriptions ("instance.*").
//
// All events are also forwarded to central via the ForwardFunc, unless the
// event source is "central" (to avoid echo loops).
type EventBus struct {
	exact    map[string][]namedHandler // "cluster.create" -> handlers
	prefixes map[string][]namedHandler // "instance" -> matches "instance.*"
	mu       sync.RWMutex
	forward  ForwardFunc
	logger   zerolog.Logger
}

type namedHandler struct {
	name    string // for logging: "lifecycle", "recovery", etc.
	handler Handler
}

// New creates an EventBus. The forward function is called for every event
// to send it to central. Pass nil to disable forwarding.
func New(forward ForwardFunc) *EventBus {
	return &EventBus{
		exact:    make(map[string][]namedHandler),
		prefixes: make(map[string][]namedHandler),
		forward:  forward,
		logger:   log.With().Str("component", "eventbus").Logger(),
	}
}

// Subscribe registers a handler for an event pattern.
//
//   - "cluster.create"  — exact match only
//   - "instance.*"      — matches all types starting with "instance."
//   - "*"               — matches everything (use sparingly)
//
// The name parameter identifies the handler in log output.
func (b *EventBus) Subscribe(pattern string, name string, h Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()

	nh := namedHandler{name: name, handler: h}

	if prefix, ok := strings.CutSuffix(pattern, ".*"); ok {
		b.prefixes[prefix] = append(b.prefixes[prefix], nh)
		b.logger.Info().
			Str("pattern", pattern).
			Str("handler", name).
			Msg("subscribed prefix handler")
	} else if pattern == "*" {
		b.prefixes[""] = append(b.prefixes[""], nh)
		b.logger.Info().
			Str("pattern", pattern).
			Str("handler", name).
			Msg("subscribed wildcard handler")
	} else {
		b.exact[pattern] = append(b.exact[pattern], nh)
		b.logger.Info().
			Str("pattern", pattern).
			Str("handler", name).
			Msg("subscribed exact handler")
	}
}

// Publish dispatches an event to all matching handlers, then forwards it
// to central. Handlers are called synchronously in registration order.
// Errors are logged but do not stop dispatch.
func (b *EventBus) Publish(ctx context.Context, evt *pgswarmv1.Event) error {
	if evt == nil {
		return nil
	}

	start := time.Now()
	evtType := evt.GetType()

	b.logger.Debug().
		Str("event_type", evtType).
		Str("cluster", evt.GetClusterName()).
		Str("pod", evt.GetPodName()).
		Str("severity", evt.GetSeverity()).
		Str("source", evt.GetSource()).
		Str("operation_id", evt.GetOperationId()).
		Msg("publishing event")

	b.mu.RLock()
	// Collect matching handlers under read lock
	var matched []namedHandler
	matched = append(matched, b.exact[evtType]...)
	for prefix, handlers := range b.prefixes {
		if prefix == "" || strings.HasPrefix(evtType, prefix+".") {
			matched = append(matched, handlers...)
		}
	}
	b.mu.RUnlock()

	// Dispatch to handlers outside the lock
	handlerCount := 0
	for _, nh := range matched {
		handlerCount++
		if err := nh.handler(ctx, evt); err != nil {
			b.logger.Warn().Err(err).
				Str("event_type", evtType).
				Str("handler", nh.name).
				Str("cluster", evt.GetClusterName()).
				Str("pod", evt.GetPodName()).
				Msg("handler returned error")
		}
	}

	if handlerCount == 0 {
		b.logger.Debug().
			Str("event_type", evtType).
			Str("cluster", evt.GetClusterName()).
			Msg("no handlers matched event")
	}

	// Forward to central (skip if the event originated from central to avoid echo)
	if b.forward != nil && evt.GetSource() != "central" {
		b.forward(evt)
		b.logger.Debug().
			Str("event_type", evtType).
			Str("cluster", evt.GetClusterName()).
			Msg("forwarded event to central")
	}

	b.logger.Debug().
		Str("event_type", evtType).
		Int("handlers", handlerCount).
		Dur("duration", time.Since(start)).
		Msg("event dispatch complete")

	return nil
}

// HandlerCount returns the total number of registered handlers (for diagnostics).
func (b *EventBus) HandlerCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	count := 0
	for _, handlers := range b.exact {
		count += len(handlers)
	}
	for _, handlers := range b.prefixes {
		count += len(handlers)
	}
	return count
}
