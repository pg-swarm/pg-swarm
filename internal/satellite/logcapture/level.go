package logcapture

import (
	"github.com/pg-swarm/pg-swarm/internal/shared/loglevel"
	"github.com/rs/zerolog"
)

// SetGlobalLevel parses a level string and sets zerolog's global level.
// Valid levels: "trace", "debug", "info", "warn", "error".
func SetGlobalLevel(levelStr string) (zerolog.Level, error) {
	return loglevel.SetGlobalLevel(levelStr)
}
