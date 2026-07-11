package engine

import "github.com/NSchatz/transcode/internal/store"

// Event is a job-state change the engine emits to an optional Observer so a
// reporting surface (TRANSCODE-7's API/SSE) can react live. It is a NOTIFICATION
// only — a copy of already-committed facts. The engine never reads anything back
// through the Observer and never blocks on it (emit is a straight call the Observer
// must make non-blocking), so no observer can slow, stall, or alter file handling.
// The data-safety invariant is untouched: emit sits beside the store writes, never
// in place of them.
type Event struct {
	// Path is the file the transition is about (the FINAL path for a Done event —
	// the post-swap name — otherwise the source path).
	Path string
	// Status is the job's new state.
	Status store.Status
	// Worker is the worker id that owns the transition, when known ("" otherwise —
	// e.g. a skip decided before a claim, or a store-advance wrapper).
	Worker string
	// BytesReclaimed is the space a successful swap freed (source size − output
	// size), set ONLY on the Done event that follows an atomic swap; 0 everywhere
	// else. A consumer sums this field, so the always-zero generic Done emit from
	// the finish wrapper contributes nothing and never double-counts.
	BytesReclaimed int64
}

// Observer receives engine Events. It MUST be non-blocking and safe for concurrent
// calls: under the worker pool several workers emit at once, and an Observer that
// blocked here would stall an encode. nil disables emission.
type Observer func(Event)
