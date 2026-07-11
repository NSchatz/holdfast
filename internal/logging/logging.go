// Package logging builds the process-wide structured logger. Everything the
// daemon emits goes through slog so logs are machine-parseable (the observability
// phase, TRANSCODE-8, consumes them alongside Prometheus metrics).
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// New returns a slog.Logger writing to stderr at the given level ("debug",
// "info", "warn", "error"; anything unrecognized falls back to info). The format
// is text for a TTY-friendly default; JSON output is a TRANSCODE-8 concern.
func New(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	return slog.New(h)
}
