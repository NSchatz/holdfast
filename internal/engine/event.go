package engine

import (
	"github.com/NSchatz/holdfast/internal/store"
)

// Event is a job-state change the engine emits to an optional Observer: a copy of
// already-committed facts. Emission sits beside the store writes, never in place of
// them, and nothing is read back, so no observer can slow, stall or alter file handling.
type Event struct {
	// Path is the FINAL post-swap path for a Done event, the source path otherwise.
	Path string
	// Status is the job's new state.
	Status store.Status
	// Worker owns the transition, "" when a skip was decided before any claim.
	Worker string

	// Outcome is the proof of a TERMINAL transition and nil on every other event. It is
	// the SAME value handed to Store.Finish, so an observer and the ledger cannot drift
	// apart. A nil numeric in it is "not recorded", never 0, and Done is emitted exactly
	// once, so a consumer may sum across Done events without double-counting.
	Outcome *store.Outcome

	// Progress, when non-nil, makes this a live report rather than a state TRANSITION:
	// the job has not moved. These carry no Outcome and count towards nothing, so a
	// consumer switching on Status is unaffected and must stay that way. Not persisted:
	// after a restart an in-flight job simply has no progress reported yet.
	Progress *Progress
}

// BytesReclaimed is the space a successful swap freed, derived from the Outcome rather
// than carried as a second field so the event and the ledger cannot disagree about it.
// It is 0 for any event that did not record both sizes, and never negative: the clamp
// means a bug in the strictly-smaller gate cannot make a reclaimed total run backwards.
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
// calls: several workers emit at once, and one that blocked here would stall an encode.
// nil disables emission.
type Observer func(Event)
