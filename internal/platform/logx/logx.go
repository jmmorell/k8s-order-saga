// Package logx builds the JSON slog logger shared by all services.
package logx

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// New returns a JSON logger writing to stdout with a "service" attribute on every line.
// level is one of debug, info, warn, error; anything else means info.
func New(service, level string) *slog.Logger {
	return NewWithWriter(os.Stdout, service, level)
}

// NewWithWriter is like New but writes to w.
func NewWithWriter(w io.Writer, service, level string) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: ParseLevel(level)})
	return slog.New(h).With("service", service)
}

// ParseLevel maps a level name (case-insensitive) to a slog level, defaulting to info.
func ParseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
