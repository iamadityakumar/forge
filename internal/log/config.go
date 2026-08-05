package log

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// Setup initializes the global slog logger according to LOG_FORMAT and LOG_LEVEL.
// It sets slog.SetDefault so all slog.X calls carry the "service" attribute.
func Setup(service string) (*slog.Logger, error) {
	levelStr := os.Getenv("LOG_LEVEL")
	level, err := parseLevel(levelStr)
	if err != nil {
		return nil, err
	}

	formatStr := strings.ToLower(strings.TrimSpace(os.Getenv("LOG_FORMAT")))
	var h slog.Handler
	switch formatStr {
	case "json":
		h = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	case "", "text":
		h = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	default:
		return nil, fmt.Errorf("unknown LOG_FORMAT %q (expected text|json)", formatStr)
	}

	logger := slog.New(h).With("service", service)
	slog.SetDefault(logger)
	return logger, nil
}

func parseLevel(s string) (slog.Level, error) {
	if s == "" {
		return slog.LevelInfo, nil
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("unknown LOG_LEVEL %q", s)
	}
}
