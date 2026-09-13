package engine

// The targeted-submission queue (S0093): the engine half of `POST /api/scan`.
//
// An *arr, a Jellyfin plugin or a shell script already knows a file landed. This is how
// it says "look at THIS file now" instead of "re-examine the whole library", and the
// whole design question is what it may NOT become while doing so.
//
// It is a QUEUE and a POOL and nothing else. It adds no gate, skips none, and knows
// nothing about codecs, profiles, swaps or the undo window: an accepted path is handed to
// ProcessFile, which is the only door into the encode/swap pipeline and carries the
// hold-back re-checks, the profile resolution, the hardlink guard and the claim. A file
// submitted here therefore reaches the same guards and records the same verdict a scan
// would have reached on it, because it reaches them through the same function.
//
// Two things it owns that a scan does not:
//
//   - The eligibility decision (Eligibility.Judge), because a caller on the network can
//     send a path that climbs out of every root while a walk cannot. Judge resolves the
//     path before it is judged, which is the property that keeps a network-reachable
//     entry point out of trees nobody configured.
//   - The bound. A scan enumerates what is there; a submission takes what it is given, so
//     the channel is FIXED-SIZE and Offer refuses rather than blocks. A handler that
//     blocked on a full queue would be a handler held open across an encode.

import (
	"context"
	"strconv"
	"sync"

	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// SubmissionResult is what the queue observed about ONE submitted path once processing
// returned. It is the submission's own report, and it exists because the HTTP response
// cannot carry this: the endpoint answers before the file has been processed, which is
// the point of a queue.
type SubmissionResult struct {
	// Path is the RESOLVED path that was processed - the one every eligibility rule was
	// answered against, and the one the ledger is keyed on.
	Path string

	// Before is the status the ledger carried for this path when the submission picked it
	// up; "" where there was no row at all.
	Before store.Status

	// Claimed reports whether the submission got PAST the door. It is the exact
	// complement of what store.Claim decides, taken from the claim itself rather than
	// re-derived, so this package holds no second copy of the re-opening rule.
	Claimed bool

	// Err is whatever ProcessFile returned - a cancellation, in practice, since every
	// per-file outcome is recorded rather than returned.
	Err error
}

// HeldByTerminalRow reports the case AC5 of S0093 names: the submission reached a file
// whose terminal row still holds under the configuration in force, so nothing was
// re-encoded and the row was left exactly as it was. It is "a terminal row was there, and
// the claim was refused" - which is the store's own answer, not a second reading of it.
func (r SubmissionResult) HeldByTerminalRow() bool { return r.Before.Terminal() && !r.Claimed }

// DefaultSubmissionQueue is how many accepted paths may wait at once. It is a bound on
// MEMORY and on how far behind the queue may fall, never a bound on what an operator may
// submit over time: a full queue is reported to the caller, which is a retry they can see
// rather than a path that vanished.
const DefaultSubmissionQueue = 1024

// MaxSubmissionResults bounds the report Results returns. The report is a TAIL, not a
// journal: this endpoint exists for a deployment where every import fires a webhook and the
// process is long-lived, so a slice appended to once per processed file and never trimmed
// would grow for the life of that process. The ledger is what keeps the durable record of
// every file - each processed submission writes the row a scan would have written - and
// this is the recent window over what the QUEUE saw, which is a different and much smaller
// question. The oldest entry is dropped when a newer one arrives.
const MaxSubmissionResults = 1024

// claimKey identifies ONE worker's claim on ONE path. The engine's claim hook is
// engine-wide, so a note keyed on the path alone cannot say whose claim it was: a scan
// worker claiming the same file between a submission's arm and its own refused claim would
// be read as the submission getting through, and two workers holding one path in
// succession would each read the other's note. The pair is unambiguous.
type claimKey struct{ worker, path string }

// Submissions is the targeted-scan queue: eligibility, a bounded channel, and a small
// pool that drains it through ProcessFile.
type Submissions struct {
	eng     *Engine
	el      Eligibility
	ch      chan string
	workers int

	// wg tracks the drain goroutines, so Wait joins any file being processed RIGHT NOW
	// before a caller closes the store those goroutines are writing to.
	wg sync.WaitGroup

	// once guards Run: a second Run over the same channel would double the pool without
	// doubling the bound, and nothing needs it.
	once sync.Once

	mu      sync.Mutex
	claims  map[claimKey]bool
	results []SubmissionResult
}

// NewSubmissions builds the queue for this engine. workers bounds how many submitted
// files are encoded at once and defaults to the engine's own worker count, so a targeted
// submission costs the host what a scan costs it; capacity bounds how many may wait.
//
// It registers the claim observer, which is the one signal this package needs from the
// pipeline and which nothing else uses.
func (e *Engine) NewSubmissions(workers, capacity int) *Submissions {
	if workers <= 0 {
		workers = e.Cfg.EffectiveWorkers()
	}
	if capacity <= 0 {
		capacity = DefaultSubmissionQueue
	}
	s := &Submissions{
		eng:     e,
		el:      e.Eligibility(),
		ch:      make(chan string, capacity),
		workers: workers,
		claims:  map[claimKey]bool{},
	}
	e.onClaim = s.noteClaim
	return s
}

// Judge applies the ONE eligibility decision to a submitted path. It returns the resolved
// path the pipeline would be handed and ok=true, or ok=false with the rule token that
// refused it and a detail for the human. It enqueues nothing and encodes nothing - a
// caller judges first so it can report every path, then offers the ones that passed.
func (s *Submissions) Judge(p string) (resolved, rule, detail string, ok bool) {
	resolved, bad := s.el.Judge(p)
	if bad != nil {
		return "", bad.Rule, bad.Detail, false
	}
	return resolved, "", "", true
}

// Offer enqueues a resolved path, reporting false when the queue is full. It NEVER
// blocks: the caller is an HTTP handler, and a handler that waited for room would be a
// handler waiting for an encode.
func (s *Submissions) Offer(resolved string) bool {
	select {
	case s.ch <- resolved:
		return true
	default:
		return false
	}
}

// Cap reports the queue's capacity, so a refusal can name the bound it hit rather than
// leaving a caller to guess it.
func (s *Submissions) Cap() int { return cap(s.ch) }

// Pending is how many accepted paths are waiting to be processed. It is what makes "a 202
// that enqueued nothing" and "a refusal that enqueued something anyway" assertable: the
// difference between admitting work and doing it is otherwise invisible from outside.
func (s *Submissions) Pending() int { return len(s.ch) }

// Run drains the queue until ctx is cancelled. It is the serve command's driver and runs
// in the caller's goroutine, exactly as the hub's does.
//
// A cancelled ctx stops the pool taking NEW work; a file already being processed finishes
// through ProcessFile's own cancellation discipline (the in-flight ffmpeg is killed, its
// temp is discarded and the source is untouched). Paths still waiting in the channel are
// DROPPED, unprocessed and unrecorded: nothing has looked at them, so a ledger row for one
// would be a record of a decision nobody took.
func (s *Submissions) Run(ctx context.Context) {
	s.once.Do(func() {
		// Report what is already parked, once, as the pool comes up. This is NOT the gate
		// the submissions are judged by: ProcessFile asks holdBacksInForce per file, so a
		// hold-back recorded at any point after this line still holds. It is here because
		// a daemon running with scan_interval_sec: 0 never takes a pass, and a pass is
		// otherwise the only thing that tells an operator what is waiting on them.
		s.eng.EnsureHoldBacks(ctx)
		for i := 0; i < s.workers; i++ {
			s.wg.Add(1)
			go s.drain(ctx, "submit-w"+strconv.Itoa(i))
		}
	})
	s.wg.Wait()
}

// drain is one worker. The ctx check before processing is what makes a cancelled queue
// drop its backlog rather than work through it: a receive that wins the race with the
// cancellation still hands back a path, and starting an encode at that point would be
// starting work the shutdown has already decided not to do.
func (s *Submissions) drain(ctx context.Context, worker string) {
	defer s.wg.Done()
	for {
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case p := <-s.ch:
			if ctx.Err() != nil {
				return
			}
			s.process(ctx, worker, p)
		}
	}
}

// Wait blocks until the pool has stopped and any file being processed has finished. Call
// it during shutdown - after the base ctx is cancelled and BEFORE the store handle is
// closed - so a worker can never issue a store call against a closed handle. It is the
// same contract Controller.Wait carries for the scan.
func (s *Submissions) Wait() { s.wg.Wait() }

// Results is the most recent MaxSubmissionResults processed submissions, oldest first. It
// is the submission's own report of what it reached, which is the only place that fact is
// available: the endpoint has answered long before.
func (s *Submissions) Results() []SubmissionResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SubmissionResult(nil), s.results...)
}

// process hands ONE path to the pipeline, through the same exported door a scan's worker
// uses, and records what it reached.
//
// The status is read BEFORE - a read, never a gate - so the result can say whether the
// submission was turned away at the door by a terminal row that still holds. Nothing here
// decides anything about the file: ProcessFile is called unconditionally, whatever that
// read said, so there is no fast path and no second answer to a question Claim owns.
func (s *Submissions) process(ctx context.Context, worker, path string) {
	before, _, exists, err := s.eng.Store.Get(ctx, path, probe.Fingerprint(path))
	if err != nil || !exists {
		before = ""
	}
	key := claimKey{worker: worker, path: path}
	s.arm(key)
	perr := s.eng.ProcessFile(ctx, worker, path)
	if perr != nil {
		s.eng.Log.Warn("targeted submission ended with an error", "file", path, "err", perr)
	}
	s.mu.Lock()
	claimed := s.claims[key]
	delete(s.claims, key)
	res := SubmissionResult{Path: path, Before: before, Claimed: claimed, Err: perr}
	s.results = append(s.results, res)
	// The report is a tail: drop from the front so the slice is bounded for the life of a
	// process that never scans and is fed by every import (MaxSubmissionResults).
	if over := len(s.results) - MaxSubmissionResults; over > 0 {
		s.results = append(s.results[:0], s.results[over:]...)
	}
	s.mu.Unlock()

	// Say it out loud. An operator whose *arr fired a webhook at a file a terminal row
	// still holds out learns nothing from a queue report nothing in the daemon reads, and
	// "holdfast did not touch the file I just told it about" is precisely the outcome that
	// otherwise looks like the endpoint silently failing.
	if res.HeldByTerminalRow() {
		s.eng.Log.Info("targeted submission reached a file its recorded outcome still holds out - nothing was "+
			"re-encoded and the row was left exactly as it was; the row is re-opened by a configuration change "+
			"it can reason about, or by the local `holdfast requeue` (see docs/requeue.md)",
			"file", path, "recorded", string(res.Before))
	}
}

// arm clears any earlier claim note for this worker and path, so a worker that processes
// the same file twice reads its OWN claim rather than the previous one's.
func (s *Submissions) arm(key claimKey) {
	s.mu.Lock()
	delete(s.claims, key)
	s.mu.Unlock()
}

// noteClaim is the claim observer. It runs on a worker goroutine (a scan's as well as a
// submission's, since the engine has one of these) and must stay cheap: it records the
// worker's claim on a path and returns.
func (s *Submissions) noteClaim(worker, path string) {
	s.mu.Lock()
	s.claims[claimKey{worker: worker, path: path}] = true
	s.mu.Unlock()
}
