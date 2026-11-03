// Package logging builds the structured logger of the core (log/slog).
package logging

import (
	"io"
	"log/slog"
	"strings"
)

// New returns a logger writing JSON (format "json") or human-readable text
// (any other format) at the given level (debug, info, warn, error; unknown
// levels mean info).
func New(w io.Writer, format, level string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}
	if strings.EqualFold(format, "json") {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

func parseLevel(level string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		return slog.LevelInfo
	}
	return l
}
