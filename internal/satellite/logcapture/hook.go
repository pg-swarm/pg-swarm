package logcapture

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/protobuf/types/known/timestamppb"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
)

// StreamHook is a zerolog Hook that captures log entries and sends them
// over a channel for streaming to central. It uses a bounded channel
// and drops entries on overflow to never block the logger.
type StreamHook struct {
	ch          chan *pgswarmv1.LogEntry
	streamLevel atomic.Int32 // zerolog.Level stored as int32
	component   string
}

// NewStreamHook creates a hook that captures log entries at or above
// the given minimum level. The component string tags each entry
// (e.g. "agent", "operator", "stream", "health").
func NewStreamHook(component string, minLevel zerolog.Level) *StreamHook {
	h := &StreamHook{
		ch:        make(chan *pgswarmv1.LogEntry, 256),
		component: component,
	}
	h.streamLevel.Store(int32(minLevel))
	return h
}

// Drain drains the capture channel and calls sendFn for each entry.
// It blocks until ctx is cancelled.
func (h *StreamHook) Drain(ctx context.Context, sendFn func(*pgswarmv1.LogEntry)) {
	for {
		select {
		case <-ctx.Done():
			return
		case entry := <-h.ch:
			sendFn(entry)
		}
	}
}

// SetStreamLevel changes the minimum level for streamed entries.
func (h *StreamHook) SetStreamLevel(level zerolog.Level) {
	h.streamLevel.Store(int32(level))
}

// Run implements zerolog.Hook. It captures the event if its level
// is at or above the configured stream level.
func (h *StreamHook) Run(e *zerolog.Event, level zerolog.Level, message string) {
	minLevel := zerolog.Level(h.streamLevel.Load())
	if level < minLevel {
		return
	}

	entry := &pgswarmv1.LogEntry{
		Level:     level.String(),
		Message:   message,
		Timestamp: timestamppb.New(time.Now()),
		Logger:    h.component,
	}

	// Non-blocking send — drop on overflow
	select {
	case h.ch <- entry:
	default:
	}
}
