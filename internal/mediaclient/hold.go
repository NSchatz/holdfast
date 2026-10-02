package mediaclient

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"time"
)

// HoldCacheTTL is how long one answer from Plex about what is being played is reused. A
// burst of jobs asking at once costs one request, and a swap never acts on an answer older
// than this.
const HoldCacheTTL = 2 * time.Second

// HoldReason is why a held file is held, as the engine's record states it.
const HoldReason = "the file is being played in Plex"

// sessionSource is what PlayHold asks: the files being played, as holdfast sees them.
type sessionSource interface {
	Playing(ctx context.Context) (files map[string]bool, class string, ok bool)
}

// PlayHold answers whether a file is being played in Plex right now, for the engine to hold
// that file's job and its swap while it is (docs/design/media-clients.md#play-hold).
//
// It FAILS OPEN, as the Tautulli pause does: when Plex cannot be asked - refused, a non-2xx
// answer, an answer that does not parse, no answer in time - no file is held. That outage is
// one warn record, not one per file per poll: nothing more is said until Plex has answered
// again.
type PlayHold struct {
	source sessionSource
	log    *slog.Logger
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	at      time.Time
	asked   bool
	playing map[string]bool
	failing bool
}

// NewPlayHold builds the hold over a Plex client. ttl <= 0 uses HoldCacheTTL.
func NewPlayHold(plex *Plex, ttl time.Duration, log *slog.Logger) *PlayHold {
	return newPlayHold(plex, ttl, log)
}

func newPlayHold(source sessionSource, ttl time.Duration, log *slog.Logger) *PlayHold {
	if log == nil {
		log = slog.Default()
	}
	if ttl <= 0 {
		ttl = HoldCacheTTL
	}
	return &PlayHold{source: source, log: log, ttl: ttl, now: time.Now}
}

// Held reports whether path (a file as holdfast sees it) is being played, and why it is held
// when it is. It matches the path a session names exactly, after the path map and cleaning;
// it resolves no symbolic link. Safe for concurrent callers: one of them asks Plex and the
// rest reuse the answer.
func (h *PlayHold) Held(ctx context.Context, path string) (bool, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.asked || h.now().Sub(h.at) >= h.ttl {
		h.refresh(ctx)
	}
	if h.playing[filepath.Clean(path)] {
		return true, HoldReason
	}
	return false, ""
}

// refresh asks Plex once and records the answer, or the fact that there was none.
func (h *PlayHold) refresh(ctx context.Context) {
	files, class, ok := h.source.Playing(ctx)
	if ctx.Err() != nil {
		// The caller was cancelled mid-request. That is not an outage and not an answer, so
		// nothing is recorded and the next caller asks again.
		return
	}
	h.asked, h.at = true, h.now()
	if !ok {
		h.playing = nil
		if !h.failing {
			h.failing = true
			h.log.Warn("the Plex play hold could not ask Plex what is being played, so NO file is held "+
				"(the hold fails open); this is said once and not again until Plex answers",
				"target", "plex", "attempted", "GET /status/sessions", "failure", class,
				"next", "jobs and swaps proceed without the play hold; it resumes when Plex answers")
		}
		return
	}
	if h.failing {
		h.failing = false
		h.log.Info("the Plex play hold is in force again: Plex answered", "target", "plex")
	}
	h.playing = files
}
