// Package logging builds the process-wide structured logger. Everything the
// daemon emits goes through slog so logs are machine-parseable (the observability
// phase, TRANSCODE-8, consumes them alongside Prometheus metrics).
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// New returns a slog.Logger writing to stderr at the given level ("debug",
// "info", "warn", "error"; anything unrecognized falls back to info). The format
// is text for a TTY-friendly default; JSON output is a TRANSCODE-8 concern.
func New(level string) *slog.Logger { return To(os.Stderr, level) }

// To is New over a caller's own stream. A command whose stdout carries exactly one
// machine-readable document has to be able to send every narrated line to the stderr IT was
// handed rather than to the process's, so that redirecting one stream really does separate
// the data from the narration.
func To(w io.Writer, level string) *slog.Logger {
	h := slog.NewTextHandler(w, &slog.HandlerOptions{Level: Level(level)})
	return slog.New(h)
}

// Level parses a configured log level, falling back to info for anything unrecognized.
func Level(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
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
