package mediaclient

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/secret"
	"github.com/NSchatz/holdfast/internal/store"
)

// QueueSize is how many swaps may wait for their rescan requests at once. One more is
// dropped with a warn record: the queue is bounded so a target that hangs can never grow
// what this process holds.
const QueueSize = 64

// DrainBound is how long `run` at its end, and `serve` at shutdown, keep attempting the
// rescan requests still pending before they exit anyway. docs/post-swap-hook.md states it.
const DrainBound = 30 * time.Second

// Result is what one target did about one swapped file's directory.
type Result struct {
	// Dir is the directory the target was asked about, in the TARGET's view.
	Dir string
	// Attempted names the request that was being made when the result was decided.
	Attempted string
	// Sent is true when the rescan or partial scan was accepted (a 2xx answer).
	Sent bool
	// NoOwner is true when the target has no movie, series or section owning Dir, so
	// nothing was sent.
	NoOwner bool
	// Failure is why the request did not succeed, nil otherwise.
	Failure *failure
}

// Target is one media server a swap is announced to.
type Target interface {
	// Name is radarr, sonarr or plex.
	Name() string
	// Rescan asks the owner of dir (a directory as holdfast sees it) to rescan it, with one
	// attempt and no retry.
	Rescan(ctx context.Context, dir string) Result
}

// Hook is the post-swap rescan: an engine.Observer that, for every job that ended with a
// replacement at its path, asks each configured target once to look at that file's
// directory.
//
// It observes facts the engine has already committed and can change none of them. Observe
// only enqueues; one goroutine of its own sends the requests; a failure is a warn record and
// nothing else. It is not on the swap path and holds no engine worker.
type Hook struct {
	targets []Target
	log     *slog.Logger

	queue chan string
	// ctx is cancelled when a drain's bound passes, which aborts the request in flight.
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	closed  bool
	pending int // swaps accepted by Observe and not yet finished by the worker
	done    chan struct{}
}

// NewHook builds the hook over its targets. With none it returns nil: nothing is configured,
// so nothing observes and nothing is sent.
func NewHook(targets []Target, log *slog.Logger) *Hook {
	if len(targets) == 0 {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Hook{targets: targets, log: log, queue: make(chan string, QueueSize),
		ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

// TargetsFor builds the enabled rescan targets of a configuration, in its fixed order
// (radarr, sonarr, plex), from the secrets resolved at start. A target that is not enabled
// contributes nothing.
func TargetsFor(cfg *config.Config, resolved func(key string) secret.Value) []Target {
	var out []Target
	for _, t := range cfg.MediaTargets() {
		if !t.Enabled {
			continue
		}
		switch t.Name {
		case "radarr":
			out = append(out, NewRadarr(t.URL, resolved(t.CredentialKey), t.PathMap))
		case "sonarr":
			out = append(out, NewSonarr(t.URL, resolved(t.CredentialKey), t.PathMap))
		case "plex":
			out = append(out, NewPlex(t.URL, resolved(t.CredentialKey), t.PathMap))
		}
	}
	return out
}

// Start runs the hook's one sending goroutine. Call it once, before the engine runs.
func (h *Hook) Start() {
	go func() {
		defer close(h.done)
		for path := range h.queue {
			h.announce(path)
			h.mu.Lock()
			h.pending--
			h.mu.Unlock()
		}
	}()
}

// Observe implements engine.Observer. It acts on a TRANSITION to `done` or
// `applied-despite-error` only - the two states in which the file at the path IS the
// replacement - and on nothing else: not a progress report, not a skip, a failure, a parked
// job or a dry run's decision. It never blocks: a full queue drops the request and says so.
func (h *Hook) Observe(ev engine.Event) {
	if ev.Progress != nil {
		return
	}
	if ev.Status != store.Done && ev.Status != store.AppliedDespiteError {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	select {
	case h.queue <- ev.Path:
		h.pending++
	default:
		h.log.Warn("post-swap rescan dropped: the rescan queue is full, so no target is told about this "+
			"swap; the swap stands and each service's own scheduled rescan remains the fallback",
			"file", ev.Path, "queue_size", QueueSize)
	}
}

// announce tells every target about one swapped file, each exactly once. A target that
// fails does not stop the next one being asked.
func (h *Hook) announce(path string) {
	// The owner is found by DIRECTORY: an applied-despite-error event carries the source
	// path and a container change moves the file name, and neither moves the directory.
	dir := filepath.Dir(path)
	for _, t := range h.targets {
		res := t.Rescan(h.ctx, dir)
		if h.ctx.Err() != nil {
			// The drain's bound passed while this was in flight. The swap is counted as not
			// delivered by Drain's one record, so nothing is said per target here.
			return
		}
		switch {
		case res.Failure != nil:
			h.log.Warn("post-swap rescan failed: the swap stands and the request is not retried; the "+
				"service's own scheduled rescan remains the fallback",
				"target", t.Name(), "file", path, "directory", res.Dir,
				"attempted", res.Attempted, "failure", res.Failure.String())
		case res.NoOwner:
			h.log.Info("post-swap rescan not sent: the target has nothing that owns this directory",
				"target", t.Name(), "file", path, "directory", res.Dir)
		default:
			h.log.Info("post-swap rescan requested", "target", t.Name(), "file", path, "directory", res.Dir)
		}
	}
}

// Drain stops accepting swaps and keeps attempting the pending requests for at most bound,
// then aborts what is left and returns how many swaps were not delivered to every target.
// A count above zero is one warn record. It is safe to call once; the hook observes nothing
// afterwards.
func (h *Hook) Drain(bound time.Duration) (undelivered int) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return 0
	}
	h.closed = true
	close(h.queue)
	h.mu.Unlock()

	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-h.done:
		return 0
	case <-timer.C:
	}
	h.mu.Lock()
	undelivered = h.pending
	h.mu.Unlock()
	h.cancel()
	if undelivered > 0 {
		h.log.Warn("post-swap rescans not delivered: the drain bound passed with requests still pending; "+
			"every swap stands and each service's own scheduled rescan remains the fallback",
			"not_delivered", undelivered, "drain_bound", bound.String())
	}
	return undelivered
}
