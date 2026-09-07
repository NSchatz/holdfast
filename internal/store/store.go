// Package store is the persistent, crash-safe job ledger for the transcoder. It
// replaces the flat-file internal/ledger with a SQLite/WAL table so a worker pool
// can safely claim files across goroutines (and, later, processes) with no risk of
// two workers encoding the same source concurrently.
//
// The data-safety invariant is unchanged: the store only ever records job STATE —
// it never touches the filesystem. The only filesystem mutation anywhere in the
// program remains the atomic same-directory rename in internal/engine, which runs
// solely after verifyOutput passes. A crash mid-encode leaves a job "stuck" in an
// active state (probing/encoding/verifying); RecoverStale resets it to pending on
// the next startup so it is safely retried — the source itself was never touched.
package store

import "context"

// Status is a job's lifecycle state.
type Status string

// The full set of statuses. Pending is the implicit initial state (a path+
// fingerprint with no row is treated as pending). Probing/Encoding/Verifying are
// the "active" sub-states a worker moves through while it holds the claim.
// Done/Skipped/Failed are terminal for that path+fingerprint (Failed is retryable
// up to a configured bound; see Claim).
const (
	Pending   Status = "pending"
	Probing   Status = "probing"
	Encoding  Status = "encoding"
	Verifying Status = "verifying"
	Done      Status = "done"
	Skipped   Status = "skipped"
	Failed    Status = "failed"

	// Indeterminate is a swap that FAILED and whose outcome could not be established:
	// the re-stat could not be completed, or it completed without either confirming
	// the source untouched or establishing that the swap was applied. It is its own
	// state precisely because it is neither of the two states that already exist -
	// not a success, and NOT "a failure that left the source intact", which is what
	// Failed has always meant on the swap path and what the pin's code asserted
	// unconditionally. A job in this state is PARKED: both files are kept, nothing is
	// re-attempted or re-queued, in this run or any later one, until an operator
	// records a determination.
	Indeterminate Status = "indeterminate"

	// AppliedDespiteError is a swap whose rename returned an error and whose
	// post-failure re-stat established that the rename NONETHELESS took effect - the
	// hazard rename(2) documents for NFS, where a retransmitted request reports a
	// failure for an operation the server already performed. It is established, not
	// unknown, so it is neither Indeterminate nor Failed; and it is not Done, because
	// the swap did not complete the way a success does (no durability fsync was
	// ordered after it, and the caller was told it failed). It needs no operator
	// action: the file at the source path IS the replacement.
	AppliedDespiteError Status = "applied-despite-error"
)

// Terminal reports whether s is a terminal status - no further processing will happen
// for that path+fingerprint (failed may still be retried by Claim, but the row itself
// is a terminal record of an attempt). Indeterminate and AppliedDespiteError are
// terminal for the same reason done/skipped are: the attempt is over and its record
// stands. Neither is ever re-claimed (see Claim).
func (s Status) Terminal() bool {
	switch s {
	case Done, Skipped, Failed, Indeterminate, AppliedDespiteError:
		return true
	default:
		return false
	}
}

// Active reports whether s is an in-progress sub-state a worker is actively
// holding (probing/encoding/verifying). An active row left behind by a crashed
// worker is what RecoverStale resets to pending.
func (s Status) Active() bool {
	switch s {
	case Probing, Encoding, Verifying:
		return true
	default:
		return false
	}
}

// Outcome is the durable PROOF of a terminal job's result — the facts the engine
// computed while deciding whether a swap was safe (TRANSCODE-13). Before this phase
// every one of them was computed and then thrown away, which is precisely why the
// ledger could not show fidelity, why a "reclaimed" total reset to zero on every
// restart, and why the API documented a failure `reason` field that did not exist.
//
// Absence is REPRESENTABLE, and must stay that way. Every numeric field is a POINTER
// for one reason: 0 is a legal value for all of them, so a plain zero cannot mean
// "nobody measured this". A VMAF of 0.0 is a destroyed frame, not a missing
// measurement. nil means NOT RECORDED, and a reader (the API, the UI) is required to
// render it as such — never as 0, never as a fabricated score. The string fields use
// "" for the same purpose, unambiguously: an empty reason/encoder/model carries no
// meaning of its own.
type Outcome struct {
	// Reason is WHY the job reached this status. For Failed it is the error text (the
	// encode error, or the gate that rejected the output). For Skipped it is the name
	// of the GUARD that fired — a stable token from internal/engine, not prose, so a
	// UI can key off it. Done needs no excuse and leaves it "".
	Reason string

	// Encoder is the encoder key (cpu / svtav1 / nvenc / …) the job actually ran, set
	// on every row that reached the encoder at all — a failure is as worth attributing
	// to its encoder as a success is.
	Encoder string

	// VmafMean and VmafMin are the pooled harmonic-mean and the worst-frame VMAF, and
	// VmafModel names the libvmaf model that produced them. All are nil/"" when the
	// VMAF gate did not run (disabled). The model is NOT decoration: a VMAF score
	// without the model and pooling that produced it is not a number anyone can
	// interpret, and displaying one without the other is the exact overclaim the
	// fidelity work exists to prevent.
	VmafMean  *float64
	VmafMin   *float64
	VmafModel string

	// SourceBytes and OutputBytes are the file sizes either side of the swap (Done).
	// BOTH are persisted rather than only their difference: that is what makes a
	// durable lifetime reclaimed total DERIVABLE (TRANSCODE-14 computes and shows it;
	// this phase only has to keep the facts) and what lets a UI show "before → after"
	// instead of a bare delta.
	SourceBytes *int64
	OutputBytes *int64

	// EncodeMs is the wall-clock encode duration in milliseconds (Done).
	EncodeMs *int64

	// --- the source-mutation guard's achieved granularity, per job -------------
	//
	// Recorded when the guard RUNS (immediately before the rename), and carried
	// through to this job's terminal row so it is retrievable after the run has
	// finished. All three are "" on a job that never reached the guard - a skip, an
	// encode failure, a rejected output - which is "not recorded", never a fabricated
	// window.

	// GuardAttributes names the source attributes the guard compared, e.g.
	// "size,mtime". It is the MEASURED half of the record: what was actually looked at.
	GuardAttributes string

	// GuardTimeResolution is the resolution of the timestamp the guard compared, as a
	// duration - "1s" for the whole-second mtime this build stats. This field is
	// REQUIRED to carry a duration: it is measured, not invented, and a build that
	// dropped it (or spelled it as a word to slip past a check) would have thrown away
	// the one number that says how sharp the guard actually is.
	GuardTimeResolution string

	// GuardResidualWindow is a CLASS LABEL - which of the two residual windows the
	// shipped documentation states applies to this job's storage - and NEVER a
	// duration. There are exactly two windows and no third: storage classified local
	// takes the local one, and storage that is not local (network-backed OR
	// undetermined) takes the network one, the same fail-safe that makes an
	// unrecognised type not-local.
	//
	// A duration is refused HERE and only here. The network window belongs to the
	// client's attribute cache, and nfs(5) says only "Every few seconds" - it names no
	// interval and no tunable - so any number recorded in this field would be
	// invented. What is measured is already in the two fields above.
	GuardResidualWindow string

	// SwapCause names the CAUSE of a swap failure when the cause is one this build
	// reports distinctly - today exactly one: SwapCauseCrossFilesystem. It is "" for
	// every other failure, so "the temp and the target are not on the same mounted
	// filesystem" is never attributed to a swap that failed for some other reason.
	SwapCause string
}

// SwapCauseCrossFilesystem is the distinct, machine-readable cause for a swap that
// failed because the temp and the target are not on the same mounted filesystem
// (rename(2)'s EXDEV: "oldpath and newpath are not on the same mounted filesystem").
// It is a stable token, not prose - treat it as a wire format. holdfast does NOT copy,
// move or otherwise fall back across filesystems when it sees this; it reports it.
const SwapCauseCrossFilesystem = "cross-filesystem"

// The two residual-window CLASS LABELS. They are deliberately the same identifiers as
// the anchors the shipped documentation carries, so the label in a job's record and
// the statement an operator reads are provably the same two things and a reader can
// grep from one to the other.
const (
	ResidualWindowLocal   = "residual-window-local"
	ResidualWindowNetwork = "residual-window-network"
)

// Job is a read-only snapshot of one row in the job ledger, returned by List. It
// is a reporting view (the API/UI in TRANSCODE-7 renders it) — never a handle the
// engine writes back through, so exposing it cannot affect file handling.
type Job struct {
	Path        string
	Fingerprint string
	Status      Status
	FailCount   int
	Worker      string // "" when the row carries no worker (e.g. a terminal row)
	UpdatedAt   int64  // unix seconds of the last state transition

	// Outcome is the recorded proof for a terminal row (TRANSCODE-13). Its fields are
	// all zero/nil on a non-terminal row, and on a terminal row written before this
	// phase existed — "not recorded", which a reader must show as such.
	Outcome Outcome
}

// Coverage states the SET a published figure was computed over. It travels WITH the
// figure, never beside it in a comment, because a number whose set is unstated is a
// number an operator will read as covering everything they own - and the queue and
// history views deliberately ship only the most recent rows, so "everything" is exactly
// what the page has never been able to promise before.
//
// Set names the matching rows (e.g. "every done row in the ledger"). Window is "" for a
// figure over that whole set, and otherwise names the BOUND that narrows it (e.g. "the
// most recent 200 rows"). Every aggregate here is currently whole-set, and the field
// exists so that a future bounded figure cannot ship without saying what it is bounded
// by - the descriptor is produced by the same function that runs the query, so the two
// cannot drift.
type Coverage struct {
	Set    string
	Window string
}

// Bucket is one keyed count in a Breakdown (a status, or a skip guard's token).
type Bucket struct {
	Key   string
	Count int64
}

// Breakdown is a keyed count over every matching row in the table.
//
// Counted is the number of rows that contributed a key; Excluded is the number of
// matching rows that recorded NO key and were therefore left out rather than folded
// into some "other" bucket that would read as a real category. Counted == 0 means NO
// ROW CONTRIBUTED - which a reader must render as "no data", never as an empty
// breakdown that looks like a set of zero counts.
//
// Err is this aggregate's OWN failure. It is per-aggregate on purpose: the snapshot's
// summary, queue and history must still ship when one figure cannot be read, so a
// failure is carried here and rendered as "unavailable" rather than returned up a
// path that would suppress the whole frame.
type Breakdown struct {
	Coverage Coverage
	Buckets  []Bucket
	Counted  int64
	Excluded int64
	Err      error
}

// Spread is the shape of one numeric aggregate over every matching row: how many rows
// contributed a recorded value, how many were excluded for want of one, and the low /
// mean / high of what was actually recorded.
//
// Min, Mean and Max are POINTERS for the same reason store.Outcome's fields are: 0 is a
// legal value for every one of them, so a plain zero cannot mean "nothing was
// measured". They are nil exactly when Counted == 0, and a reader must render that as
// "no data" - never as 0, and never as an average of 0.
//
// Deliberately min/mean/max and NOT a median or a percentile: those are version- and
// compile-flag-gated in SQLite (median() and percentile() need 3.51.0 built with
// SQLITE_ENABLE_PERCENTILE, or a loadable extension before that), and a query that
// resolves on the developer's build and not on the shipped image is a runtime failure
// on somebody else's machine. Everything here uses COUNT/MIN/AVG/MAX, which every
// SQLite build has.
//
// Err carries this aggregate's own failure; see Breakdown.
type Spread struct {
	Coverage Coverage
	Counted  int64
	Excluded int64
	Min      *float64
	Mean     *float64
	Max      *float64
	Err      error
}

// Aggregates is the whole-ledger report the dashboard publishes: the figures the
// queue and history views cannot give, because those ship at most a few hundred rows
// while a library holds hundreds of thousands.
//
// Every field is computed INDEPENDENTLY and carries its own Err, which is why this
// method returns no error of its own: there is no failure that belongs to the set, and
// a single error return would be an invitation to let one unreadable figure blank the
// live page.
type Aggregates struct {
	// Outcomes is the count of terminal rows per status, over the whole table.
	Outcomes Breakdown
	// SkipsByGuard breaks every skipped row down by the guard token that skipped it,
	// so an operator never has to read the logs to learn WHICH guard fired.
	SkipsByGuard Breakdown
	// SizeRatio is the spread of output size / source size over done rows (0.35 = the
	// replacement is 35% of the original).
	SizeRatio Spread
	// EncodeMs is the spread of wall-clock encode duration, in milliseconds.
	EncodeMs Spread
	// VmafMean and VmafMin are the spreads of the two pooled VMAF statistics each done
	// row recorded. A row that recorded neither (VMAF disabled) is excluded and
	// counted, never read as a zero - a VMAF of 0 is a destroyed frame.
	VmafMean Spread
	VmafMin  Spread
}

// Determination is what an operator decided about a parked job. There are exactly
// two, because there are exactly two things that could have happened to the rename.
type Determination string

// The two determinations an operator may record.
const (
	SwapWasApplied  Determination = "swap-was-applied"
	SourceIsIntact  Determination = "source-is-intact"
	determinationNo Determination = "" // still parked
)

// Valid reports whether d is one of the two determinations.
func (d Determination) Valid() bool { return d == SwapWasApplied || d == SourceIsIntact }

// Disposition is what happened to ONE of the two recorded paths when a parked job was
// resolved. Every resolution carries one for EACH path; a resolution that leaves
// either path without one is refused, because an unstated disposition is exactly the
// state in which nobody can say whether a file holdfast wrote is still out there.
type Disposition string

// The four dispositions.
const (
	// KeptInPlace: the file stays and later runs treat that path NORMALLY - it
	// re-enters enumeration and no hold-back is placed on it. It is never legal for a
	// recorded REPLACEMENT path: a file holdfast wrote is never handed back to
	// enumeration as if it were a source.
	KeptInPlace Disposition = "kept-in-place"
	// RetainedExcluded: the file stays and that path remains OUT of enumeration for as
	// long as the record survives.
	RetainedExcluded Disposition = "retained-excluded"
	// Deleted: removed, and only on the operator's explicit instruction.
	Deleted Disposition = "deleted"
	// Absent: there was no file there to dispose of.
	Absent Disposition = "absent"
)

// Valid reports whether d is one of the four dispositions.
func (d Disposition) Valid() bool {
	switch d {
	case KeptInPlace, RetainedExcluded, Deleted, Absent:
		return true
	default:
		return false
	}
}

// SwapIncident is the durable record of a swap that did not complete cleanly - the
// facts AC-level reporting needs to identify BOTH files without logs, plus the
// operator's determination once one is made.
//
// It lives in its own table rather than as more columns on the jobs row, for a reason
// that is load-bearing: the jobs row is keyed on path+fingerprint and is CLEARED by
// Claim (claiming begins a new attempt) and PRUNED after a successful transcode. The
// exclusion a recorded replacement path carries has to outlive all of that - the file
// holdfast wrote is still on disk regardless of what happens to the job that wrote it -
// so the record cannot be a passenger on a row with that lifecycle.
type SwapIncident struct {
	ID int64

	// SourcePath and SourceFingerprint are the job this incident belongs to.
	SourcePath        string
	SourceFingerprint string

	// ReplacementPath is where the replacement IS - the path holdfast last knows it to
	// be at, which after a retained failure is the retained-replacement path rather
	// than the in-flight temp.
	ReplacementPath string

	// SourceAttrs and ReplacementAttrs are the rename-invariant attribute records
	// taken BEFORE the rename was attempted, in probe.Attributes' "size:mtime"
	// spelling. They are what makes the two files identifiable from the record alone.
	SourceAttrs      string
	ReplacementAttrs string

	// ObservedAttrs is what the post-failure re-stat saw, or "" when the re-stat could
	// not be completed. On an Outcome of AppliedDespiteError this is the evidence the
	// applied case matched on.
	ObservedAttrs string

	// Outcome is Indeterminate or AppliedDespiteError.
	Outcome Status

	// SwapError is the error text the rename returned; SwapCause is
	// SwapCauseCrossFilesystem or "".
	SwapError string
	SwapCause string

	// StorageClass and StorageType are the classification of the source's storage
	// taken AT THE TIME OF THAT SWAP.
	StorageClass string
	StorageType  string

	CreatedAt int64

	// JobOutcome is the proof the pipeline had accumulated for this job when the swap
	// failed - the encoder, the VMAF pair and its model, the encode duration, and the
	// source-mutation guard's granularity record. RecordSwapIncident writes it onto
	// the JOB row in the same transaction as the incident, so a parked job's row still
	// carries everything an ordinary terminal row would. nil records none.
	JobOutcome *Outcome

	// --- the resolution half; every field is zero while the job is parked -------

	Resolution                      Determination
	ResolvedBy                      string // who made it: "operator"
	ResolvedAt                      int64
	ObservedAtResolutionSource      string
	ObservedAtResolutionReplacement string
	DispositionSource               Disposition
	DispositionReplacement          Disposition

	// RemovalError records that a removal the resolution licensed did NOT succeed.
	// When it is non-empty the replacement's disposition has been corrected away from
	// Deleted, so the surviving file is still held out of enumeration - a licensed
	// removal that fails must never leave a record claiming a deletion that did not
	// happen.
	RemovalError string
}

// Parked reports whether this incident is a parked job: recorded indeterminate, with
// no operator determination yet.
func (s SwapIncident) Parked() bool {
	return s.Outcome == Indeterminate && s.Resolution == determinationNo
}

// Resolution is the operator's instruction, as the store records it.
type Resolution struct {
	Determination          Determination
	By                     string
	ObservedSource         string
	ObservedReplacement    string
	DispositionSource      Disposition
	DispositionReplacement Disposition
	RemovalError           string
}

// Store is the persistent job ledger. Every method is safe for concurrent use by
// multiple workers (goroutines) within one process.
type Store interface {
	// RecoverStale resets any job left in an active state (probing/encoding/
	// verifying) back to pending — the mark of a prior crashed/killed run, since a
	// live worker holds its claim only for the duration of one in-process call.
	// Returns the number of jobs reset. Call once at startup, before any scan.
	RecoverStale(ctx context.Context) (int, error)

	// Claim atomically attempts to take ownership of path+fingerprint for worker.
	// Returns (true, nil) if the caller now owns the job (row moved to probing) and
	// (false, nil) if it does not: the job is done/skipped (permanent), failed and
	// already at/over maxFailures (parked), or currently active (held by another
	// worker, or stale — see RecoverStale). A fresh path+fingerprint with no row
	// yields a claim.
	Claim(ctx context.Context, path, fingerprint, worker string, maxFailures int) (bool, error)

	// Advance records a non-terminal state transition for a job the caller already
	// holds (e.g. probing -> encoding -> verifying).
	Advance(ctx context.Context, path, fingerprint string, s Status) error

	// Finish records a terminal outcome for path+fingerprint. Failed increments
	// fail_count (retry accounting); Done/Skipped do not.
	//
	// o is the proof of that outcome (TRANSCODE-13); nil records none. Finish always
	// writes the FULL outcome column set, so a nil o — or a nil field within it —
	// CLEARS the corresponding column. That is deliberate: a row's proof must always
	// describe its CURRENT status. A file that failed (reason recorded), was retried,
	// and then succeeded must not sit in the ledger as "done" with the old failure's
	// reason still attached to it.
	Finish(ctx context.Context, path, fingerprint string, s Status, o *Outcome) error

	// Delete removes the row for path+fingerprint (a no-op if absent). Used to prune
	// a job row that has been superseded — after a successful transcode the pre-swap
	// (path, old-fingerprint) row is deleted, so the table doesn't accumulate one
	// dangling row per transcoded file.
	Delete(ctx context.Context, path, fingerprint string) error

	// Get returns the current status and fail_count for path+fingerprint, and
	// whether a row exists at all (exists=false + status="" means never seen).
	Get(ctx context.Context, path, fingerprint string) (status Status, failCount int, exists bool, err error)

	// List returns job rows for reporting (TRANSCODE-7's API/UI), newest-updated
	// first. If statuses is non-empty only rows in that set are returned; an empty
	// statuses returns every row. limit > 0 caps the result to that many rows
	// (0 or negative = no cap). It is a pure read: it never mutates a row, so no
	// amount of API traffic can alter file handling.
	List(ctx context.Context, statuses []Status, limit int) ([]Job, error)

	// Summary returns a count of rows per status (only statuses with at least one
	// row appear). Used by the API/UI for at-a-glance queue/history totals.
	Summary(ctx context.Context) (map[Status]int, error)

	// ReclaimedTotal is the durable lifetime reclaimed-space total: the sum of
	// (source_bytes - output_bytes) over every Done row that recorded both sizes
	// (TRANSCODE-13 persists them, TRANSCODE-14 shows this). It is what a "reclaimed"
	// figure must be built on instead of a per-process counter that resets to 0 on
	// every restart. Rows written before the outcome columns existed carry no sizes
	// and are simply not counted (never counted as 0-reclaimed). A pure read.
	ReclaimedTotal(ctx context.Context) (int64, error)

	// Aggregates computes the published whole-ledger figures - each over EVERY
	// matching row in the table, never over the capped rows List ships. It is a pure
	// read.
	//
	// It returns no error: every figure is computed independently and reports its own
	// failure in its Err, so one unreadable aggregate can be rendered as unavailable
	// while the rest of the report - and the snapshot carrying it - still ships.
	Aggregates(ctx context.Context) Aggregates

	// RecordSkip persists a Skipped row carrying reason for a guard that fires BEFORE
	// Claim — today only the hardlink guard, whose decision must stay unclaimed (it
	// never enters the encode pipeline) yet must still be visible as a skip in the UI
	// (TRANSCODE-14: "which guard fired"). It INSERTs a fresh skipped row, or converts
	// a pending row; it deliberately does NOT overwrite a row that already carries a
	// terminal outcome (done/failed/another skip), so a real proof is never clobbered
	// by a mutable guard. Reports changed=true only when it actually inserted/converted
	// a row (not on the idempotent re-run where the skipped row already exists), so a
	// caller emits an event — and a metrics/notify observer counts the skip — exactly
	// once, not once per scan.
	RecordSkip(ctx context.Context, path, fingerprint, reason string) (changed bool, err error)

	// ClearSkip deletes the row for path+fingerprint ONLY when it is a Skipped row
	// whose reason matches — the re-evaluation half of a MUTABLE guard. The hardlink
	// guard re-checks every scan (a seed may finish, dropping the link count); when a
	// file it once skipped as "hardlinked" is no longer hardlinked, this removes that
	// stale skip so the file is reclaimed on the normal path. It never touches a
	// done/failed/other-skip row (the reason+status match guards that), so a real
	// outcome is never deleted. No-op when no such row exists.
	ClearSkip(ctx context.Context, path, fingerprint, reason string) error

	// RecordSwapIncident persists a swap that did not complete cleanly: an
	// indeterminate outcome, or one applied despite an error. It writes the incident
	// row AND moves the job row to the matching status in ONE transaction, so a job
	// can never sit in a state whose supporting facts were not written (or the
	// reverse). An error from here means NEITHER landed, which is the only condition
	// under which the caller may treat the outcome as unpersisted.
	RecordSwapIncident(ctx context.Context, in SwapIncident) error

	// ParkedIncidents returns every parked job - recorded indeterminate, no operator
	// determination yet - oldest first. A run reads this at startup to report them and
	// to hold both of each job's recorded paths back from the scan.
	ParkedIncidents(ctx context.Context) ([]SwapIncident, error)

	// ExcludedReplacementPaths returns every path a record carries as a job's
	// replacement path whose disposition is neither deleted nor absent - i.e. every
	// path where a file holdfast wrote may still be. Enumeration must not treat any of
	// them as a source. The exclusion is keyed to the EXISTENCE OF THE RECORD and not
	// to the parked state: a replacement retained after a job is resolved is still a
	// file holdfast wrote, and enumerating it would queue a gate-passed encode as a
	// source and leave the library a permanent duplicate.
	ExcludedReplacementPaths(ctx context.Context) ([]string, error)

	// ResolveIncident records an operator's determination against a parked incident
	// and RELEASES the job: the incident keeps the durable record, and the jobs row for
	// (source path, source fingerprint) is removed so a later run treats that path as
	// new work rather than as a job to re-park. Both dispositions are required and a
	// replacement path may never be kept-in-place; ResolveIncident refuses otherwise.
	// It returns an error if id is not a parked incident.
	ResolveIncident(ctx context.Context, id int64, r Resolution) error

	// AmendReplacementDisposition corrects an already-recorded replacement disposition
	// and attaches the reason. It exists for exactly one situation: the removal a
	// resolution licensed was ordered AFTER the record was made durable, and then did
	// not succeed. The record must not go on claiming a deletion that did not happen,
	// so the disposition moves back to retained-excluded and the surviving file stays
	// out of enumeration.
	AmendReplacementDisposition(ctx context.Context, id int64, d Disposition, removalErr string) error

	// IncidentByID returns one incident.
	IncidentByID(ctx context.Context, id int64) (SwapIncident, bool, error)

	// Close releases the underlying database handle.
	Close() error
}
