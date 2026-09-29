// Package logging builds the process-wide structured logger. Everything the
// daemon emits goes through slog so logs are machine-parseable (the observability
// phase, TRANSCODE-8, consumes them alongside Prometheus metrics).
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
)

// New returns a slog.Logger writing to stderr at the given level ("debug",
// "info", "warn", "error"; anything unrecognized falls back to info). The format
// is text for a TTY-friendly default; JSON output is a TRANSCODE-8 concern.
//
// It also installs that logger as the process-wide default, so a component handed no
// logger (it falls back to slog.Default) writes the same clock as the command that built
// it. Without that, the default renders through the standard log package's layout, which
// carries no offset at all.
func New(level string) *slog.Logger {
	l := To(os.Stderr, level)
	slog.SetDefault(l)
	return l
}

// To is New over a caller's own stream. A command whose stdout carries exactly one
// machine-readable document has to be able to send every narrated line to the stderr IT was
// handed rather than to the process's, so that redirecting one stream really does separate
// the data from the narration. It leaves the process-wide default alone: that one belongs to
// the process's own stderr.
func To(w io.Writer, level string) *slog.Logger {
	h := slog.NewTextHandler(w, &slog.HandlerOptions{Level: Level(level), ReplaceAttr: numericOffset})
	return slog.New(h)
}

// timeLayout is the stock text handler's time rendering - RFC 3339 at millisecond precision -
// with one change: `-07:00` writes the offset as digits in every zone, where the stock
// RFC3339Nano layout's `Z07:00` writes a bare `Z` for UTC. So a line logged in UTC says
// `+00:00`, and a timeline stitched from two processes states its clock on every line.
// `-00:00` is never written: RFC 3339 section 4.3 gives it the meaning "the offset to local
// time is unknown" (https://www.rfc-editor.org/rfc/rfc3339 , read 2026-09-29), and the
// offset here is always known.
const timeLayout = "2006-01-02T15:04:05.000-07:00"

// numericOffset renders the record's own time field with timeLayout and leaves every other
// attribute exactly as the stock handler writes it. The handler hands the built-in time
// field to ReplaceAttr with no groups, so a time-valued attribute inside a group, or under
// any other key, is not touched.
func numericOffset(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
		t := a.Value.Time().Truncate(time.Millisecond)
		return slog.String(slog.TimeKey, t.Format(timeLayout))
	}
	return a
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
