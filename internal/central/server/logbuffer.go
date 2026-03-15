package server

import (
	"sync"

	"github.com/google/uuid"
)

const defaultRingSize = 1000

// LogEntryJSON is the JSON representation of a satellite log entry
// for the REST API and SSE streaming.
type LogEntryJSON struct {
	Level     string            `json:"level"`
	Message   string            `json:"message"`
	Fields    map[string]string `json:"fields"`
	Timestamp string            `json:"timestamp"`
	Logger    string            `json:"logger"`
}

// ring is a fixed-size circular buffer for log entries.
type ring struct {
	buf  []*LogEntryJSON
	head int
	size int
	cap  int
}

func newRing(capacity int) *ring {
	return &ring{
		buf: make([]*LogEntryJSON, capacity),
		cap: capacity,
	}
}

func (r *ring) push(e *LogEntryJSON) {
	r.buf[r.head] = e
	r.head = (r.head + 1) % r.cap
	if r.size < r.cap {
		r.size++
	}
}

// recent returns the last n entries, oldest first.
func (r *ring) recent(n int) []*LogEntryJSON {
	if n > r.size {
		n = r.size
	}
	out := make([]*LogEntryJSON, 0, n)
	start := (r.head - n + r.cap) % r.cap
	for i := 0; i < n; i++ {
		idx := (start + i) % r.cap
		out = append(out, r.buf[idx])
	}
	return out
}

// LogBuffer is an in-memory ring buffer per satellite with SSE fan-out.
type LogBuffer struct {
	mu      sync.RWMutex
	buffers map[uuid.UUID]*ring
	subs    map[uuid.UUID][]chan *LogEntryJSON
}

// NewLogBuffer creates an empty LogBuffer.
func NewLogBuffer() *LogBuffer {
	return &LogBuffer{
		buffers: make(map[uuid.UUID]*ring),
		subs:    make(map[uuid.UUID][]chan *LogEntryJSON),
	}
}

// Push stores a log entry in the ring buffer for the given satellite
// and fans it out to all SSE subscribers.
func (lb *LogBuffer) Push(satID uuid.UUID, entry *LogEntryJSON) {
	lb.mu.Lock()
	r, ok := lb.buffers[satID]
	if !ok {
		r = newRing(defaultRingSize)
		lb.buffers[satID] = r
	}
	r.push(entry)

	// Fan out to subscribers (non-blocking)
	for _, ch := range lb.subs[satID] {
		select {
		case ch <- entry:
		default:
		}
	}
	lb.mu.Unlock()
}

// Recent returns the last `limit` entries for a satellite, optionally
// filtered to entries at or above minLevel.
func (lb *LogBuffer) Recent(satID uuid.UUID, limit int, minLevel string) []*LogEntryJSON {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	r, ok := lb.buffers[satID]
	if !ok {
		return nil
	}

	entries := r.recent(limit)
	if minLevel == "" || minLevel == "trace" {
		return entries
	}

	filtered := make([]*LogEntryJSON, 0, len(entries))
	minPri := levelPriority(minLevel)
	for _, e := range entries {
		if levelPriority(e.Level) >= minPri {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

// Subscribe creates a channel that receives new log entries for the
// given satellite. Returns the channel and an unsubscribe function.
func (lb *LogBuffer) Subscribe(satID uuid.UUID) (chan *LogEntryJSON, func()) {
	ch := make(chan *LogEntryJSON, 64)

	lb.mu.Lock()
	lb.subs[satID] = append(lb.subs[satID], ch)
	lb.mu.Unlock()

	unsub := func() {
		lb.mu.Lock()
		defer lb.mu.Unlock()
		subs := lb.subs[satID]
		for i, s := range subs {
			if s == ch {
				lb.subs[satID] = append(subs[:i], subs[i+1:]...)
				break
			}
		}
		close(ch)
	}
	return ch, unsub
}

// levelPriority maps level strings to a numeric priority for filtering.
func levelPriority(level string) int {
	switch level {
	case "trace":
		return 0
	case "debug":
		return 1
	case "info":
		return 2
	case "warn", "warning":
		return 3
	case "error":
		return 4
	case "fatal":
		return 5
	default:
		return 0
	}
}
