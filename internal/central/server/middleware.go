package server

import (
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/pg-swarm/pg-swarm/internal/shared/reqid"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// requestLogMiddleware logs every REST API request with method, path, status,
// latency, client IP, and a request ID (generated or forwarded).
func (s *RESTServer) requestLogMiddleware(c *fiber.Ctx) error {
	path := c.Path()

	// Skip non-API paths (SPA catch-all), health checks, WebSocket, and SSE streams.
	if !strings.HasPrefix(path, "/api") ||
		path == "/api/v1/ws" ||
		path == "/api/v1/health" ||
		strings.HasSuffix(path, "/logs/stream") {
		return c.Next()
	}

	// Extract or generate request ID.
	id := c.Get("X-Request-ID")
	if id == "" {
		id = reqid.NewID()
	}
	c.Locals("request_id", id)
	c.Set("X-Request-ID", id)

	start := time.Now()
	err := c.Next()
	latency := time.Since(start)

	status := c.Response().StatusCode()
	level := requestLogLevel(c.Method(), status)

	log.WithLevel(level).
		Str("request_id", id).
		Str("method", c.Method()).
		Str("path", path).
		Int("status", status).
		Dur("latency", latency).
		Str("client_ip", c.IP()).
		Msg("http request")

	return err
}

// requestLogLevel returns the appropriate zerolog level for a request.
func requestLogLevel(method string, status int) zerolog.Level {
	switch {
	case status >= 500:
		return zerolog.ErrorLevel
	case status >= 400:
		return zerolog.WarnLevel
	case method == fiber.MethodGet:
		return zerolog.DebugLevel
	default:
		return zerolog.InfoLevel
	}
}

// requestIDFromFiber extracts the request ID stored by the middleware.
func requestIDFromFiber(c *fiber.Ctx) string {
	if id, ok := c.Locals("request_id").(string); ok {
		return id
	}
	return ""
}

// loggerFromFiber returns a request-scoped sub-logger with request ID.
func loggerFromFiber(c *fiber.Ctx) zerolog.Logger {
	id := requestIDFromFiber(c)
	if id == "" {
		return log.Logger
	}
	return log.With().Str("request_id", id).Logger()
}

// auditLog emits a structured audit log entry for user-initiated mutations.
// The "audit":"true" field enables filtering in any log aggregation system.
func auditLog(c *fiber.Ctx, action, resourceType, resourceID, resourceName, detail string) {
	log.Info().
		Str("audit", "true").
		Str("action", action).
		Str("request_id", requestIDFromFiber(c)).
		Str("resource_type", resourceType).
		Str("resource_id", resourceID).
		Str("resource_name", resourceName).
		Str("client_ip", c.IP()).
		Str("detail", detail).
		Msg("audit")
}
