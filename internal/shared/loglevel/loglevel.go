package loglevel

import (
	"fmt"
	"strings"

	"github.com/rs/zerolog"
)

// SetGlobalLevel parses a level string and sets zerolog's global level.
// Valid levels: "trace", "debug", "info", "warn", "error".
func SetGlobalLevel(levelStr string) (zerolog.Level, error) {
	level, err := zerolog.ParseLevel(strings.ToLower(levelStr))
	if err != nil {
		return zerolog.InfoLevel, fmt.Errorf("invalid log level %q: %w", levelStr, err)
	}
	zerolog.SetGlobalLevel(level)
	return level, nil
}
