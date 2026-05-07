package observability

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// Config controls how slog is wired at startup.
type Config struct {
	Env    string // "production" → JSON handler; anything else → text handler
	Level  slog.Level
	Output io.Writer // nil defaults to os.Stdout
}

// ParseLevel maps a string (typically from LOG_LEVEL env var) to a slog level.
// Unknown values default to LevelInfo. Empty string also returns LevelInfo.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// NewLogger returns a configured *slog.Logger. In production emits JSON;
// otherwise emits human-readable text.
func NewLogger(cfg Config) *slog.Logger {
	out := cfg.Output
	if out == nil {
		out = os.Stdout
	}
	opts := &slog.HandlerOptions{Level: cfg.Level}
	var handler slog.Handler
	if cfg.Env == "production" {
		handler = slog.NewJSONHandler(out, opts)
	} else {
		handler = slog.NewTextHandler(out, opts)
	}
	return slog.New(handler)
}
