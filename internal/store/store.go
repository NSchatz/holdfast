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

	// WouldTranscode is a file a DRY RUN decided: it passed every guard, so a run with
	// dry-run disabled would have transcoded it. It is TERMINAL for that scan - the
	// decision is taken, nothing further happens to the file in this run - and it is the
	// only terminal state that is RE-CLAIMABLE (see Claim).
	//
	// Both halves are load-bearing. Terminal, because the alternative is what shipped
	// before it existed: the row was left wherever the worker parked it while deciding, so
	// a decided file sat under `probing` beside files a worker was still examining, and
	// every figure that says what the run CONCLUDED reported the run as having concluded
	// nothing. Re-claimable, because this is a recorded decision about a scan and NOT a
	// disposal of the file: treating it the way done/skipped are treated would mean every
	// file decided during a dry run is silently skipped for ever once the operator turns
	// dry-run off - the library never transcoded, and nothing on the page saying so.
	WouldTranscode Status = "would-transcode"

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
//
// WouldTranscode is terminal in exactly that sense and no stronger one: the dry run's
// decision is taken and recorded, so the attempt is over, and the row is the record of
// it. Terminal is NOT a synonym for "never claimed again" here - Failed already is not,
// and this one is not either.
func (s Status) Terminal() bool {
	switch s {
	case Done, Skipped, Failed, WouldTranscode, Indeterminate, AppliedDespiteError:
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

// FailureClass says whether a terminal failure's verdict is a pure function of that
// job's inputs - the source bytes, the configuration and the pinned ffmpeg build - or
// whether a later attempt could reach a different one.
//
// It is a CLOSED vocabulary of exactly two values and, like the engine's skip tokens,
// it is a WIRE FORMAT: it is stored on the row, so renaming one changes what an
// operator's existing rows mean. It is NOT a status and NOT a guard token; a failure
// carries a class beside its reason, and nothing else does.
//
// It is what lets `max_failures` mean what it says. That bound buys re-attempts, and a
// re-attempt is only worth anything when the next one could come out differently: a
// size-increase reject on the same file under the same configuration will reject
// identically on attempt three, after another full encode. So the class decides whether
// the bound is spent one attempt at a time or all at once (see Finish).
//
// Anything outside the vocabulary is TRANSIENT, including the empty value a row written
// before the class existed carries. That is the fail-safe direction and the direction is
// not symmetric: the cost of a wrong "transient" is CPU, and the cost of a wrong
// "deterministic" is a file parked at its first failure that nobody revisits.
type FailureClass string

// The two failure classes. There is no third, and no "unknown": an unrecognised value
// resolves to Transient rather than becoming a state of its own (see Class).
const (
	// FailureTransient: a later attempt may reach a different verdict. A full disk, an
	// OOM-killed ffmpeg, a store error, an output that did not decode. Retried under
	// max_failures exactly as every failure has always been.
	FailureTransient FailureClass = "transient"

	// FailureDeterministic: the same source, the same configuration and the same
	// ffmpeg build will produce this same verdict, so the attempts max_failures would
	// buy are all spent reaching it again.
	FailureDeterministic FailureClass = "deterministic"
)

// Class resolves c to a member of the vocabulary. Exactly one value is deterministic;
// EVERYTHING else - the empty value, a token from a newer build, anything an operator or
// a repair script put in the column by hand - is transient, because retrying costs CPU
// and refusing to retry costs a file.
//
// It is applied on the way IN and on the way OUT (see Finish and outcomeScan.outcome),
// so a stored class is always one of the two and a read never hands a caller a value it
// would have to interpret for itself.
func (c FailureClass) Class() FailureClass {
	if c == FailureDeterministic {
		return FailureDeterministic
	}
	return FailureTransient
}

// Final reports whether c is the class whose verdict no re-attempt can change.
func (c FailureClass) Final() bool { return c.Class() == FailureDeterministic }

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
	// encode error, or the gate that rejected the output), and where that verdict is
	// FINAL it says so in front of that text rather than instead of it: an operator
	// reading the row learns both that it will not be tried again and what rejected it,
	// without going to the logs. For Skipped it is the name of the GUARD that fired — a
	// stable token from internal/engine, not prose, so a UI can key off it. Done needs
	// no excuse and leaves it "".
	Reason string

	// FailureClass is whether this failure's verdict is a pure function of the job's
	// inputs (see FailureClass). It is meaningful on a FAILED row and is "not recorded"
	// on every other status, exactly as the fields below are on a row that never reached
	// the code that fills them.
	//
	// Absence is NOT representable in the way the numeric fields' absence is, and that
	// is deliberate rather than an oversight: there is no "unclassified" failure to
	// represent, because a class that could not be established IS the transient one.
	// A read therefore always yields a member of the vocabulary.
	FailureClass FailureClass

	// Encoder is the encoder key (cpu / svtav1 / nvenc / …) the job actually ran, set
	// on every row that reached the encoder at all — a failure is as worth attributing
	// to its encoder as a success is.
	Encoder string

	// Profile is the name of the transcode_profiles entry that supplied this job's
	// settings, and "" when the top-level settings did — which is every row a
	// configuration without profiles can produce.
	//
	// It is on the row because Encoder alone stops answering "what ran" the moment
	// profiles exist. Two files in one run can be encoded by two different encoders
	// at two different quality targets in two different containers, and an operator
	// auditing a swap after the source is gone needs to know WHICH set of settings
	// decided it — including for a skip, where the profile is what decided that the
	// file was already at its target codec.
	//
	// "" is a real value here and not a missing measurement: it says the top-level
	// settings ran. It is stored as NULL like every other empty string in this
	// struct, and read back as "" — the two are the same statement for this field.
	Profile string

	// VmafMean and VmafMin are the pooled harmonic-mean and the worst-frame VMAF, and
	// VmafModel names the libvmaf model that produced them. All are nil/"" when the
	// VMAF gate did not run (disabled). The model is NOT decoration: a VMAF score
	// without the model and pooling that produced it is not a number anyone can
	// interpret, and displaying one without the other is the exact overclaim the
	// fidelity work exists to prevent.
	VmafMean  *float64
	VmafMin   *float64
	VmafModel string

	// VmafPixFmt is the single pixel format BOTH streams were converted to before
	// they were compared (GATE-4) - named by holdfast, never left to libavfilter's
	// automatic negotiation. It belongs on the row for the same reason VmafModel
	// does: `pixel_format: auto` floors output bit depth at 10, so an 8-bit source
	// and its 10-bit replacement are routinely compared after a conversion, and
	// upconverting the reference is not the same measurement as downconverting the
	// output. A score whose comparison format is unrecorded is a score whose meaning
	// cannot be stated afterwards.
	//
	// VmafChroma is the worst (sub)sampled frame's PSNR over the chroma planes, and
	// VmafChromaMetric names what that number is and in what unit. The VMAF model is
	// luma-only, so this is the ONLY figure on the row that says anything at all
	// about whether the colour survived. Pointer/"" for the same reason as everything
	// else here: 0.0 dB is an obliterated chroma plane, not a missing measurement.
	VmafPixFmt       string
	VmafChroma       *float64
	VmafChromaMetric string

	// VmafStream names WHICH video stream of each file the comparison was made against,
	// in the specifier vocabulary the probes use ("v:0"). It belongs on the row for the
	// reason VmafPixFmt does: a source can carry more than one video stream, and a score
	// that does not say which one it looked at cannot be lined up against the guards that
	// inspected the file. It is a stable token, treated as a wire format the way VmafModel
	// and VmafChromaMetric are.
	//
	// "" is NOT RECORDED, the rule every string here keeps: a row written before this
	// fact existed, and a job whose VMAF gate never ran, record no stream. It is NULL in
	// the column and absent on the wire; it is never a fabricated "v:0", which would claim
	// a comparison nobody made.
	VmafStream string

	// SourceCodec is the video codec the SOURCE was in when this job was decided, as
	// ffprobe named it ("h264", "mpeg4", …). It is recorded on a dry-run decision, which
	// is the row whose whole purpose is to say what a real run WOULD do to that file: an
	// operator sizing the job needs to know what is being re-encoded, and a candidate list
	// that does not say is a list they have to go and probe themselves.
	//
	// "" is NOT RECORDED, the same rule every string here keeps. It is NULL in the column
	// and an explicit absence on the wire; it is never a fabricated codec and never an
	// empty string presented as one.
	SourceCodec string

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

	// DecisionInputs is what the decision that wrote this row READ from the
	// configuration, and it is what lets Claim re-derive the row rather than treat it
	// as a permanent answer (see DecisionInputs and Claim). Its zero value is NOT
	// RECORDED, which is what every row written before the column existed carries and
	// what a failure - a verdict the configuration did not determine - carries too.
	DecisionInputs DecisionInputs

	// Decision names the library profile that decided this file. It is embedded so a
	// reader asks a row for o.LibraryRoot exactly as it asks for o.Encoder.
	Decision
}

// Decision names the library profile a row was decided under: which root's profile
// supplied the knobs the file was judged by, and what those knobs resolved to.
//
// It exists because a library root now carries its own encoder, crf, bitrate floor and
// VMAF floors, so "what was this file judged by" stopped being answerable from the
// configuration: the file has several profiles to choose from and the row had none. And
// it is a TYPE rather than two more strings in an argument list, because two adjacent
// strings is precisely the call that silently swaps, and a row naming its digest as its
// root would be worse than one naming neither.
//
// LibraryRoot is the CLEANED path of the root the file was enumerated under.
// ProfileDigest identifies that root's RESOLVED knob values, and it is what keeps the
// row interpretable after the profile has been edited: the path alone would go on naming
// a root that now means something else. Rows decided under identical resolved values
// carry the same digest; any different value carries a different one.
//
// "" is NOT RECORDED, the rule every string on an Outcome keeps, and it is what a row
// written by an earlier build reads as. It is never a fabricated root and never a digest
// of whatever the configuration happens to say now.
type Decision struct {
	LibraryRoot   string
	ProfileDigest string
}

// GuardRestoredOriginal is the one skip-guard token this package has to know by name.
//
// Every other token in Outcome.Reason is opaque here: the store records which guard
// fired and never acts on it. This one is different because the store is where the
// action has to happen. A `skipped / restored-original` row is what stands between an
// operator's rescued bytes and the gates that passed the encode they rejected, so it is
// NEVER re-opened - not by a configuration change, not by a requeue, not by a row that
// records no inputs at all - and a rule enforced only in internal/engine would be a rule
// with one check in front of an irreversible act.
//
// internal/engine defines its SkipRestoredOriginal as this constant, so there is exactly
// one spelling of a token that is a stored wire format.
const GuardRestoredOriginal = "restored-original"

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

// Prune is what one retention pass actually did (LEDGER-5). It is returned rather than
// logged inside the store so the caller can report it: a prune is the one IRREVERSIBLE
// act in this package, and an operator is owed the count.
//
// Removed is how many terminal rows were deleted. ReclaimedCarried is the number of bytes
// those rows contributed to the lifetime reclaimed total, moved into the durable
// carry-forward BEFORE they were deleted, so the published total does not move. Kept is
// how many rows the pass EXAMINED and refused to remove because removing them would change
// what the engine does with a file: a job parked at max_failures, or a row Prunable
// declined. It counts examined rows, not the whole table - a pass stops offering rows the
// moment enough are approved to meet the retention - so it is "what this pass refused",
// which is the figure that tells an operator why a table stayed large.
type Prune struct {
	Removed          int64
	ReclaimedCarried int64
	Kept             int64
}

// Prunable answers the one question the retention pass cannot answer for itself: may the
// terminal row for path+fingerprint be removed WITHOUT changing what the engine would do
// with that file?
//
// It exists because a terminal row is a DECISION and not only a record. Claim refuses a
// done or skipped row outright, and the skip guards that would re-derive the same verdict
// run after Claim, under whatever configuration is current - so a pruned row re-derives
// its own verdict only while the configuration it was taken under has not moved. Answering
// it means knowing whether that file is still in the library, which is a question about
// the filesystem, and this package never touches the filesystem (see the package comment:
// the store records job STATE and nothing else). So the caller answers, and internal/engine
// is the caller that can.
//
// TRUE means "removing this row can cause no encode": the file that row decided is no
// longer in the library. FALSE is the safe answer and must be the answer whenever the
// caller cannot tell.
type Prunable func(path, fingerprint string, s Status) bool

// RowTotal is a count of matching rows in the LEDGER, beside the capped rows a response
// actually ships (LEDGER-5). It exists because /api/queue and /api/history return at most
// a few hundred rows and, until now, said nothing about what they were a few hundred OF -
// leaving a client to derive it, which is exactly what a client cannot do correctly.
//
// Count is the number of matching rows. Err is this figure's OWN failure, carried the way
// an Aggregate carries one: a total that could not be read must be STATED as unreadable
// beside rows that still ship, never reported as a total of zero.
type RowTotal struct {
	Coverage Coverage
	Count    int64
	Err      error
}

// Retained is one original the undo window is holding: a SECOND LINK to the bytes a
// swap replaced, plus everything a restore needs to put them back safely (UNDO-6).
//
// It is not an Outcome and deliberately does not live on a jobs row: a jobs row is
// keyed (path, fingerprint) and the pre-swap row is pruned by the swap itself, so a
// retention recorded there would be deleted by the event it exists to undo.
//
// SourcePath is the path the original is restored TO - the file the swap consumed.
// SwappedPath is what the swap produced: the same path for an in-place rename, a
// different one when the container extension changed. RetainedPath is the second link
// holding the original's bytes alive.
//
// SwappedFingerprint is the size:mtime of SwappedPath taken immediately after the
// swap. A restore compares it against what is there NOW and refuses when the two
// disagree, so an operator can never overwrite content this tool did not write.
//
// SourceBytes is the size of the retained original, which is what the undo window is
// HOLDING - space a reclaimed figure must not count as returned, because it has not
// been.
//
// RestoredAt is nil until the original is put back; a RELEASED retention is deleted
// outright rather than flagged, so "something is retained for this path" is exactly
// "a row exists whose RestoredAt is nil".
type Retained struct {
	SourcePath         string
	SwappedPath        string
	RetainedPath       string
	SourceBytes        int64
	SwappedFingerprint string
	RetainedAt         int64
	ExpiresAt          int64
	RestoredAt         *int64
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
	// (false, nil) if it does not: the job is done/skipped and its recorded decision
	// inputs still hold, failed and already at/over maxFailures (parked), or currently
	// active (held by another worker, or stale - see RecoverStale). A fresh
	// path+fingerprint with no row yields a claim.
	//
	// A done or skipped row is terminal only FOR THE CONFIGURATION IT WAS TAKEN UNDER.
	// current is what that configuration is now - every decision input this build offers,
	// keyed by the configuration key holding it - and a row whose recorded inputs no
	// longer match it, or which records none at all, is RE-OPENED: the file is offered to
	// the pipeline exactly as an unseen file is. Re-opening is not re-encoding; the guards
	// run again, and a file that reaches the same verdict reaches it in microseconds and
	// records the current inputs on the way.
	//
	// current is a parameter rather than a setting this package holds, for the reason
	// maxFailures is one on Finish: it is the caller's configuration, the caller is the
	// only thing that can see it, and a cached copy would answer a question about a
	// configuration nobody could prove was still in force.
	//
	// Three rows are never re-opened however far the configuration has moved, because
	// what holds each of them out is not a configuration question: Indeterminate,
	// AppliedDespiteError, and a Skipped row carrying GuardRestoredOriginal.
	//
	// A WouldTranscode row DOES yield a claim, and unconditionally. It records what a dry
	// run decided about that scan, not a disposal of the file, so the run that is allowed
	// to transcode must be able to pick the file up - otherwise turning dry-run off would
	// leave every file the dry run examined permanently untouched.
	Claim(ctx context.Context, path, fingerprint, worker string, maxFailures int, current DecisionInputs) (bool, error)

	// Reopen clears what ONE terminal row recorded about the configuration its decision
	// was taken under, so the next Claim reads that decision as one it cannot re-derive
	// and offers the file to the pipeline. It is the store half of `holdfast requeue`:
	// the lever for the rows a configuration change cannot reason about.
	//
	// It re-opens by the SAME mechanism a configuration change does rather than by
	// deleting the row, and that is load-bearing twice over. A deleted done row takes its
	// contribution to the lifetime reclaimed total with it, and a deleted failed row
	// takes the attempt accounting that parked it.
	//
	// clearFailures additionally resets the attempt count, which is what re-opens a row
	// parked at max_failures - the count is the only thing holding it (see Claim).
	//
	// It REFUSES the three rows that are never re-opened, in its own WHERE clause and not
	// on the caller's word: Indeterminate, AppliedDespiteError, and a Skipped row
	// carrying GuardRestoredOriginal. Reports whether a row actually moved, so a caller
	// counts what it changed rather than what it asked for.
	Reopen(ctx context.Context, path, fingerprint string, clearFailures bool) (bool, error)

	// SurveyDecisionInputs reports what the ledger says about the configuration its
	// terminal decisions were taken under, measured against current: how many done and
	// skipped rows record inputs that have moved, how many record none at all, and how
	// many still match. A run announces the first two before its scan so a re-derivation
	// is stated rather than discovered. A pure read.
	SurveyDecisionInputs(ctx context.Context, current DecisionInputs) (DecisionInputsSurvey, error)

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
	//
	// maxFailures is the attempt bound the caller is running under - the same bound it
	// passes to Claim, which is why it is a parameter here rather than a setting this
	// package holds. It governs ONE thing: a Failed row whose class is deterministic
	// spends the whole bound in this write instead of one attempt of it, because the
	// verdict is a pure function of inputs that have not moved and the remaining
	// attempts would each cost a full encode to reach it again. No new status and no
	// second parking mechanism: the row is an ordinary failed row that Claim then
	// refuses on the ordinary fail_count >= maxFailures rule, and the count is the only
	// thing holding it. A transient failure is unaffected, and so is every non-Failed
	// status; 0 or less means "no bound", under which nothing is parked early.
	Finish(ctx context.Context, path, fingerprint string, s Status, o *Outcome, maxFailures int) error

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
	//
	// It is the sum over the live rows PLUS the durable carry-forward a prune leaves
	// behind (LEDGER-5), so bounding the ledger cannot make this figure run backwards.
	ReclaimedTotal(ctx context.Context) (int64, error)

	// PruneTerminal enforces a ledger retention of at most maxRows TERMINAL rows,
	// deleting the oldest beyond it (LEDGER-5). maxRows <= 0 is retention DISABLED and
	// is a no-op that reads nothing and deletes nothing - the shipped default, and the
	// behaviour of a configuration that never mentions retention.
	//
	// Three rules govern what it may take, and every one of them is a criterion this
	// method exists to satisfy rather than defensive taste:
	//
	//  1. A row's contribution to the durable lifetime reclaimed total is CARRIED FORWARD
	//     into ledger_totals in the same transaction that deletes it, so the published
	//     total is identical either side of a prune - on the running server, whose
	//     baseline is frozen at startup, and after the restart that re-reads it. A row
	//     that still contributed is therefore never lost, only relocated.
	//  2. A row prunable says no to is KEPT. A terminal row is a decision the engine
	//     enforces through Claim, so removing the row of a file that is still in the
	//     library hands that file to the encoder whenever the configuration the verdict
	//     was taken under has since moved. Only the caller can see the library; see
	//     Prunable. A nil prunable keeps every row.
	//  3. A FAILED row whose fail_count has reached maxFailures is PARKED: the engine
	//     refuses to claim it, and deleting it would reset that accounting and hand the
	//     file straight back to the encoder on the next scan. This one the store can see
	//     in its own columns, so it is refused here as well as by rule 2 - the one
	//     irreversible act in this package does not rest on a single check.
	//
	// Rules 2 and 3 are counted in Prune.Kept and are why the ledger may sit ABOVE
	// maxRows: a retention that cannot be met without causing an encode is not met, and
	// the count is reported rather than hidden.
	//
	// The pass runs in BATCHES, each its own transaction: a 300,000-row ledger must not
	// hold the single serialized write connection for the length of one enormous DELETE,
	// and a failure part way through leaves every row it did not remove in place with the
	// total already correct for the rows it did.
	PruneTerminal(ctx context.Context, maxRows, maxFailures int, prunable Prunable) (Prune, error)

	// CountRows counts the rows matching statuses, over the WHOLE table - the total a
	// capped response was capped against (LEDGER-5). An empty statuses counts every row.
	//
	// It is its own read, not a projection of Summary, for the reason the aggregates are
	// their own reads: /api/queue and /api/history must still return their rows when this
	// figure cannot be read, and a shared failure path would take the rows down with it.
	CountRows(ctx context.Context, statuses []Status) RowTotal

	// EachTerminal streams every terminal row, oldest transition first, calling fn once
	// per row. It is the export's read (LEDGER-5): a ledger that has outgrown a capped
	// API response has also outgrown a []Job, so the rows are handed over one at a time
	// and never accumulated. fn's error stops the walk and is returned. A pure read.
	EachTerminal(ctx context.Context, fn func(Job) error) error

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
	//
	// by is the library profile that decided the skip, recorded on the row for the same
	// reason Finish records it: a skipped row is a terminal record of a decision, and
	// the guard that produced this one (skip_hardlinked) is itself per root.
	RecordSkip(ctx context.Context, path, fingerprint, reason string, by Decision) (changed bool, err error)

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

	// HeldByUndoWindow is the number of bytes the undo window is still HOLDING: the
	// sum of SourceBytes over every retention that has neither been restored nor
	// released (UNDO-6). It is reported BESIDE ReclaimedTotal and never folded into
	// it, because a retained original's bytes have not been returned to the
	// filesystem - the second link is still there - and a reclaimed figure that
	// counted them would tell an operator space is free while it is not. It falls to
	// zero of its own accord as the window closes and the releases run. A pure read.
	HeldByUndoWindow(ctx context.Context) (int64, error)

	// Retain records one retained original (UNDO-6), replacing any earlier record for
	// the same source path - an earlier one can only be a retention that was already
	// restored (a live one blocks the swap, a released one is deleted), and that
	// history is superseded by the swap now being recorded.
	Retain(ctx context.Context, r Retained) error

	// GetRetained returns the retention record for path, which may be named EITHER by
	// its source path or by the path the swap produced: after a container-changing
	// swap the only name an operator can see in their library is the latter, and being
	// asked to restore the file that is actually there must not be a miss. Rows that
	// have already been restored ARE returned (exists=true) so a caller can report the
	// restore rather than an absence; a released retention is gone from the table
	// entirely and reads as exists=false.
	GetRetained(ctx context.Context, path string) (r Retained, exists bool, err error)

	// ListRetained returns every LIVE retention (not yet restored), oldest expiry
	// first. It is what the release sweep walks and what `holdfast restore` lists.
	ListRetained(ctx context.Context) ([]Retained, error)

	// MarkRestored stamps a retention as restored at unix second at. The row is KEPT:
	// the restore is a ledger fact ("it happened, and when") and deleting it would
	// leave the ledger reporting only the swap that has just been undone.
	MarkRestored(ctx context.Context, sourcePath string, at int64) error

	// DropRetained deletes a retention record outright. Used by the release sweep once
	// the retained name is gone, and by a restore that finds the retained original no
	// longer on disk: in both cases there is nothing left to restore, and a record that
	// promises one would be a promise the tool cannot keep.
	DropRetained(ctx context.Context, sourcePath string) error

	// Close releases the underlying database handle.
	Close() error
}
