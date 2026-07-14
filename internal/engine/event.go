package engine

import (
	"github.com/NSchatz/holdfast/internal/store"
)

// Event is a job-state change the engine emits to an optional Observer so a
// reporting surface (TRANSCODE-7's API/SSE, TRANSCODE-8's metrics/notifications) can
// react live. It is a NOTIFICATION only — a copy of already-committed facts. The
// engine never reads anything back through the Observer and never blocks on it (emit
// is a straight call the Observer must make non-blocking), so no observer can slow,
// stall, or alter file handling. The data-safety invariant is untouched: emit sits
// beside the store writes, never in place of them.
type Event struct {
	// Path is the file the transition is about (the FINAL path for a Done event —
	// the post-swap name — otherwise the source path).
	Path string
	// Status is the job's new state.
	Status store.Status
	// Worker is the worker id that owns the transition, when known ("" otherwise —
	// e.g. a skip decided before a claim, or a store-advance wrapper).
	Worker string

	// Outcome is the proof of a TERMINAL transition (done/skipped/failed): the reason,
	// the encoder, the VMAF pair and its model, the sizes either side of the swap, the
	// encode duration. nil on every non-terminal event.
	//
	// It is the SAME value handed to Store.Finish for this transition (TRANSCODE-13),
	// which is the point: what an observer sees and what the ledger keeps cannot drift
	// apart, because there is only one of them. Read its fields honestly — a nil
	// numeric is "not recorded", never 0.
	//
	// Done is emitted exactly once (the rich event from ProcessFile's swap path — the
	// store-only finish on that path does not emit), so a consumer may safely sum
	// across Done events without double-counting.
	Outcome *store.Outcome
}

// BytesReclaimed is the space a successful swap freed (source size − output size),
// derived from the Outcome rather than carried as a second field so the event and the
// ledger can never disagree about it. It is 0 for any event that did not record both
// sizes — i.e. everything but a Done.
//
// It never returns a negative: the strictly-smaller gate in verifyOutput precludes an
// output bigger than its source, and this clamp means a future bug there cannot make a
// reclaimed-space total run backwards.
func (e Event) BytesReclaimed() int64 {
	if e.Outcome == nil || e.Outcome.SourceBytes == nil || e.Outcome.OutputBytes == nil {
		return 0
	}
	n := *e.Outcome.SourceBytes - *e.Outcome.OutputBytes
	if n < 0 {
		return 0
	}
	return n
}

// Observer receives engine Events. It MUST be non-blocking and safe for concurrent
// calls: under the worker pool several workers emit at once, and an Observer that
// blocked here would stall an encode. nil disables emission.
type Observer func(Event)
