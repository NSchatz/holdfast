// Package notify sends best-effort notifications about holdfast activity
// (TRANSCODE-8) via shoutrrr — one service URL fans out to ntfy/Discord/Gotify/etc.
// It is strictly a side-channel: a send runs on a background goroutine (never on an
// engine worker, so a slow/hanging notification endpoint can never stall an encode),
// and a send failure is logged, never propagated — notifications can never crash the
// daemon or alter file handling. An empty service URL disables it entirely.
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/nicholas-fedor/shoutrrr"

	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/store"
)

// humanBytes renders a byte count in binary units for a human-readable summary.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// SendFunc delivers message to the shoutrrr service URL. Seam for tests; production
// uses shoutrrr.Send.
type SendFunc func(url, message string) error

// Notifier turns engine events into notifications: a message per failed file, and a
// summary per scan. Safe for concurrent Observe calls (engine workers).
type Notifier struct {
	url   string
	label string // log-safe redaction of url (scheme only — never a credential)
	log   *slog.Logger
	send  SendFunc

	ch chan string // outbound queue; enqueue is non-blocking (drop-and-log if full)

	mu    sync.Mutex
	tally tally
}

// redactURL returns a log-safe label for a shoutrrr service URL: the scheme only.
// A shoutrrr URL carries its credential in the URL itself — in the userinfo
// (discord://TOKEN@id), the host (slack://TOKEN-A/…), the path (gotify://host/TOKEN),
// or the query — so ONLY the scheme is universally safe to log. The raw URL and the
// shoutrrr error string (which quotes the raw URL) must never reach the logs.
func redactURL(raw string) string {
	if i := strings.Index(raw, "://"); i > 0 {
		return raw[:i] + "://<redacted>"
	}
	if raw == "" {
		return ""
	}
	return "<redacted>"
}

type tally struct {
	done, skipped, failed int
	reclaimed             int64
}

// New builds a Notifier for the given shoutrrr service URL ("" disables it).
func New(url string, log *slog.Logger) *Notifier {
	if log == nil {
		log = slog.Default()
	}
	return &Notifier{
		url:   url,
		label: redactURL(url),
		log:   log,
		send:  func(u, m string) error { return shoutrrr.Send(u, m) },
		ch:    make(chan string, 64),
	}
}

// Enabled reports whether a service URL is configured.
func (n *Notifier) Enabled() bool { return n.url != "" }

// Run drains the outbound queue until ctx is cancelled, sending each message
// best-effort. Start it once in a goroutine before serving. A no-op sender loop when
// disabled (nothing is ever enqueued). On shutdown (ctx cancelled) it returns
// immediately WITHOUT flushing the buffer — a best-effort side-channel must not
// block shutdown on a slow endpoint, so an unsent message at stop is dropped.
func (n *Notifier) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-n.ch:
			if err := n.send(n.url, msg); err != nil {
				// Best-effort: a notification failure must never crash or block the
				// daemon — log and move on. CRUCIAL: never log err (nor n.url) — the
				// shoutrrr error quotes the raw service URL, which carries the
				// credential. Log only the scheme-redacted label. See redactURL.
				n.log.Warn("notification send failed (ignored — check the notify endpoint)", "service", n.label)
			}
		}
	}
}

// enqueue hands a message to Run without blocking; if the buffer is full the message
// is dropped (logged) rather than stalling the caller (an engine worker).
func (n *Notifier) enqueue(msg string) {
	if !n.Enabled() {
		return
	}
	select {
	case n.ch <- msg:
	default:
		n.log.Warn("notification queue full — dropping message", "msg", msg)
	}
}

// Observe implements engine.Observer. It tallies terminal outcomes for the scan
// summary and fires an immediate message on a failure. Non-blocking.
func (n *Notifier) Observe(ev engine.Event) {
	switch ev.Status {
	case store.Done:
		n.mu.Lock()
		n.tally.done++
		n.tally.reclaimed += ev.BytesReclaimed
		n.mu.Unlock()
	case store.Skipped:
		n.mu.Lock()
		n.tally.skipped++
		n.mu.Unlock()
	case store.Failed:
		n.mu.Lock()
		n.tally.failed++
		n.mu.Unlock()
		n.enqueue(fmt.Sprintf("holdfast: FAILED to transcode %s (source left untouched)", ev.Path))
	}
}

// ScanStarted resets the per-scan tally. Call at the start of each scan.
func (n *Notifier) ScanStarted() {
	n.mu.Lock()
	n.tally = tally{}
	n.mu.Unlock()
}

// ScanFinished emits a per-scan summary (unless nothing happened) and resets the
// tally. Call when a scan completes.
func (n *Notifier) ScanFinished() {
	n.mu.Lock()
	t := n.tally
	n.tally = tally{}
	n.mu.Unlock()

	if t.done == 0 && t.skipped == 0 && t.failed == 0 {
		return // nothing to report
	}
	n.enqueue(fmt.Sprintf(
		"transcode scan complete: %d transcoded (%s reclaimed), %d skipped, %d failed",
		t.done, humanBytes(t.reclaimed), t.skipped, t.failed))
}
