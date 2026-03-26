package reqid

import (
	"context"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type requestIDKey struct{}

// NewID generates a new UUID-based request ID.
func NewID() string {
	return uuid.NewString()
}

// WithRequestID stores a request ID in the context.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// FromContext extracts the request ID from context. Returns "" if absent.
func FromContext(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey{}).(string); ok {
		return id
	}
	return ""
}

// Logger returns a zerolog sub-logger enriched with the request ID from context.
func Logger(ctx context.Context) zerolog.Logger {
	id := FromContext(ctx)
	if id == "" {
		return log.Logger
	}
	return log.With().Str("request_id", id).Logger()
}
