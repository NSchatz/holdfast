// Package server is TRANSCODE-7: a chi REST API + SSE live stream + embedded web
// UI over the transcode engine. It is a READ-AND-CONTROL surface on top of the
// config-as-code engine — the YAML file remains the source of truth, the SQLite
// store remains the source of job state, and NOTHING here ever touches a media file
// or weakens the data-safety invariant. The API can only: read the store, start a
// scan, and pause/resume the feeding of NEW files. An in-flight encode is never
// interrupted by anything in this package.
package server

import (
	"context"
	"log/slog"
	"sync"
)

// Scanner runs one full library scan (in production, engine.RunOneshot). It must
// honour ctx cancellation (shutdown kills any in-flight ffmpeg, discarding its temp
// — the source stays intact). Injected so the controller is unit-testable without
// a real engine.
type Scanner func(ctx context.Context) error

// Controller owns the two pieces of runtime control the API exposes: a pause flag
// (does the engine feed NEW files?) and scan orchestration (is a scan running, and
// start one on demand). It is the single source of truth for both, safe for
// concurrent callers (HTTP handlers + the scan goroutine + the engine's Paused
// hook all touch it).
//
// Pause semantics are deliberately conservative: pausing stops NEW files from being
// fed to workers; a file already encoding finishes normally (the atomic swap is
// never interrupted — that is the invariant). So pause only ever DELAYS work.
type Controller struct {
	scan Scanner
	log  *slog.Logger

	// baseCtx bounds every scan to the server's lifetime — shutdown cancels it,
	// stopping an in-flight scan safely (ffmpeg killed via ctx, temp discarded).
	baseCtx context.Context

	mu       sync.Mutex
	paused   bool
	scanning bool

	// wg tracks the in-flight scan goroutine so a shutdown can join it before the
	// caller closes resources the scan uses (the store). See Wait.
	wg sync.WaitGroup

	// onChange is notified (best-effort, outside the lock) whenever paused/scanning
	// changes, so the SSE hub can broadcast a fresh snapshot. nil = no listener.
	onChange func()
}

// NewController builds a Controller. baseCtx bounds all scans (cancel it on
// shutdown); scan is the work function (engine.RunOneshot in production).
func NewController(baseCtx context.Context, scan Scanner, log *slog.Logger) *Controller {
	if log == nil {
		log = slog.Default()
	}
	return &Controller{scan: scan, log: log, baseCtx: baseCtx}
}

// SetOnChange registers a callback fired after any paused/scanning change. Set it
// once, before serving. The callback must be non-blocking (the hub's is).
func (c *Controller) SetOnChange(fn func()) {
	c.mu.Lock()
	c.onChange = fn
	c.mu.Unlock()
}

// notify calls onChange without holding the lock (it must never be held while
// calling out, to avoid a re-entrant deadlock if the callback reads state).
func (c *Controller) notify() {
	c.mu.Lock()
	fn := c.onChange
	c.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// Paused reports whether new-file feeding is paused. This is the function wired
// into engine.Paused, so the engine consults the live flag between files.
func (c *Controller) Paused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paused
}

// Scanning reports whether a scan is currently running.
func (c *Controller) Scanning() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.scanning
}

// Pause stops the engine from feeding new files. A no-op if already paused.
func (c *Controller) Pause() {
	c.mu.Lock()
	changed := !c.paused
	c.paused = true
	c.mu.Unlock()
	if changed {
		c.log.Info("paused (new-file feeding stopped; in-flight encodes finish safely)")
		c.notify()
	}
}

// Resume clears the pause flag. It does NOT itself start a scan — the next periodic
// tick, or an explicit Rescan, picks up the files left pending. A no-op if already
// running.
func (c *Controller) Resume() {
	c.mu.Lock()
	changed := c.paused
	c.paused = false
	c.mu.Unlock()
	if changed {
		c.log.Info("resumed")
		c.notify()
	}
}

// Rescan starts a library scan if one is not already running and we are not paused.
// It returns (started, reason): started=false with a human reason when refused
// ("paused" or "already scanning"). The scan runs on its own goroutine bound to
// baseCtx; scanning flips false when it returns. Refusing to overlap scans is what
// keeps two scans from double-claiming — though the store's Claim is the actual
// mutual-exclusion guard, so an overlap would be safe regardless.
func (c *Controller) Rescan() (started bool, reason string) {
	c.mu.Lock()
	switch {
	case c.paused:
		c.mu.Unlock()
		return false, "paused"
	case c.scanning:
		c.mu.Unlock()
		return false, "already scanning"
	}
	c.scanning = true
	c.wg.Add(1)
	c.mu.Unlock()
	c.notify() // scanning -> true

	go func() {
		defer c.wg.Done()
		c.log.Info("scan started")
		if err := c.scan(c.baseCtx); err != nil && c.baseCtx.Err() == nil {
			// A cancelled baseCtx (shutdown) is expected and already handled by the
			// engine; only log an unexpected scan error.
			c.log.Warn("scan ended with error", "err", err)
		}
		c.mu.Lock()
		c.scanning = false
		c.mu.Unlock()
		c.log.Info("scan finished")
		c.notify() // scanning -> false
	}()
	return true, ""
}

// Wait blocks until any in-flight scan goroutine has returned. Call it during
// shutdown — after baseCtx is cancelled (which stops the scan) and before closing
// the store — so a worker can't issue a store call after the handle is closed.
func (c *Controller) Wait() { c.wg.Wait() }
