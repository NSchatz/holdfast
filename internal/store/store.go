// Package store is the persistent, crash-safe job ledger: a SQLite/WAL table so a worker
// pool can claim files across goroutines with no risk of two workers encoding one source.
//
// It only ever records job STATE and never touches the filesystem. The only filesystem
// mutation in the program is the atomic same-directory rename in internal/engine, which
// runs solely after verifyOutput passes. A crash mid-encode leaves a job stuck in an active
// state; RecoverStale resets it to pending on the next startup, the source untouched.
//
// Absence is REPRESENTABLE throughout, and must stay that way. Every numeric field that a
// measurement fills is a POINTER, and every string field uses "": 0 is a legal value for
// all of them, so a plain zero cannot mean "nobody measured this" - a VMAF of 0.0 is a
// destroyed frame. nil and "" mean NOT RECORDED, and a reader is required to render them
// as such rather than as a fabricated number.
package store

import "context"

// Status is a job's lifecycle state.
type Status string

// The full set of statuses. Pending is implicit: a path+fingerprint with no row.
// Probing/Encoding/Verifying are the active sub-states a worker moves through while it
// holds the claim. Done/Skipped/Failed are terminal, Failed retryable to a configured bound.
const (
	Pending   Status = "pending"
	Probing   Status = "probing"
	Encoding  Status = "encoding"
	Verifying Status = "verifying"
	Done      Status = "done"
	Skipped   Status = "skipped"
	Failed    Status = "failed"

	// Indeterminate is a swap that FAILED and whose outcome could not be established: the
	// re-stat could not be completed, or it completed without either confirming the source
	// untouched or establishing that the swap was applied. It is its own state because it
	// is neither a success nor "a failure that left the source intact", which is what
	// Failed means on the swap path. A job in this state is PARKED: both files are kept and
	// nothing is re-attempted, in this run or any later one, until an operator records a
	// determination.
	Indeterminate Status = "indeterminate"

	// WouldTranscode is a file a DRY RUN decided: it passed every guard, so a run with
	// dry-run disabled would have transcoded it. TERMINAL for that scan, because the
	// decision is taken and a row left under `probing` makes every figure that says what
	// the run CONCLUDED report nothing. RE-CLAIMABLE, and the only terminal state that is,
	// because this is a recorded decision about a scan and not a disposal of the file:
	// treating it as done is treated would silently skip every file a dry run examined for
	// ever once the operator turns dry-run off.
	WouldTranscode Status = "would-transcode"

	// AppliedDespiteError is a swap whose rename returned an error and whose post-failure
	// re-stat established that it NONETHELESS took effect - the hazard rename(2) documents
	// for NFS, where a retransmitted request reports a failure for an operation the server
	// already performed. It is established rather than unknown, so neither Indeterminate
	// nor Failed, and not Done because no durability fsync was ordered and the caller was
	// told it failed. It needs no operator action: the file at the source path IS the
	// replacement.
	AppliedDespiteError Status = "applied-despite-error"
)

// Terminal reports whether the attempt is over and the row is its record. It is NOT a
// synonym for "never claimed again": Failed may be retried by Claim and WouldTranscode is
// re-claimable, while Indeterminate and AppliedDespiteError never are.
func (s Status) Terminal() bool {
	switch s {
	case Done, Skipped, Failed, WouldTranscode, Indeterminate, AppliedDespiteError:
		return true
	default:
		return false
	}
}

// Active reports whether s is an in-progress sub-state a worker is holding. An active row
// left behind by a crashed worker is what RecoverStale resets to pending.
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
	// VmafModel names the libvmaf model that produced them. The model is NOT decoration: a
	// score without the model and pooling behind it is not a number anyone can interpret.
	VmafMean  *float64
	VmafMin   *float64
	VmafModel string

	// VmafPixFmt is the single pixel format BOTH streams were converted to before they were
	// compared, named by holdfast and never left to libavfilter's negotiation: an 8-bit
	// source and its 10-bit replacement are routinely compared after a conversion, and
	// upconverting the reference is not the same measurement as downconverting the output.
	//
	// VmafChroma is the worst (sub)sampled frame's PSNR over the chroma planes and
	// VmafChromaMetric names what that number is. The VMAF model is luma-only, so this is
	// the ONLY figure on the row that says whether the colour survived.
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
	// ffprobe named it. It is recorded on a dry-run decision, whose whole purpose is to say
	// what a real run WOULD do to that file: an operator sizing the job needs to know what
	// is being re-encoded.
	SourceCodec string

	// SourceBytes and OutputBytes are the file sizes either side of the swap. BOTH are
	// persisted rather than only their difference, which is what makes a durable lifetime
	// reclaimed total derivable and lets a UI show before and after.
	SourceBytes *int64
	OutputBytes *int64

	// EncodeMs is the wall-clock encode duration in milliseconds.
	EncodeMs *int64

	// The source-mutation guard's achieved granularity, recorded when the guard RUNS
	// (immediately before the rename) and carried to this job's terminal row. All three are
	// "" on a job that never reached the guard.

	// GuardAttributes names the source attributes the guard compared, e.g. "size,mtime".
	GuardAttributes string

	// GuardTimeResolution is the resolution of the timestamp the guard compared, as a
	// duration ("1s" for the whole-second mtime this build stats). It is REQUIRED to carry
	// a duration: it is measured, and a build that dropped it would have thrown away the
	// one number that says how sharp the guard actually is.
	GuardTimeResolution string

	// GuardResidualWindow is a CLASS LABEL - which of the two residual windows the shipped
	// documentation states applies to this job's storage - and NEVER a duration. Storage
	// classified local takes the local one; storage that is not local, network-backed or
	// undetermined alike, takes the network one. A duration is refused here because the
	// network window belongs to the client's attribute cache and nfs(5) says only "Every
	// few seconds", naming no interval and no tunable, so any number would be invented.
	GuardResidualWindow string

	// SwapCause names the CAUSE of a swap failure when this build reports it distinctly -
	// today only SwapCauseCrossFilesystem. It is "" for every other failure, so that cause
	// is never attributed to a swap that failed for some other reason.
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

// The two residual-window CLASS LABELS, deliberately the same identifiers as the anchors
// the shipped documentation carries, so a reader can grep from a job's record to the
// statement that explains it.
const (
	ResidualWindowLocal   = "residual-window-local"
	ResidualWindowNetwork = "residual-window-network"
)

// Job is a read-only snapshot of one row, returned by List. It is a reporting view and
// never a handle the engine writes back through, so exposing it cannot affect file handling.
type Job struct {
	Path        string
	Fingerprint string
	Status      Status
	FailCount   int
	Worker      string // "" when the row carries no worker (e.g. a terminal row)
	UpdatedAt   int64  // unix seconds of the last state transition

	// Outcome is the recorded proof for a terminal row, all zero on a non-terminal one.
	Outcome Outcome

	// Stamp is the schema version this row was last written under. A row written before
	// the stamp existed carries none and reads as such rather than as a version; see
	// SchemaStamp for what an unrecognised one means. It is deliberately NOT part of the
	// published API or export shape: it says what a reader is holding, which is a
	// question about the record rather than about the job.
	Stamp SchemaStamp
}

// Coverage states the SET a published figure was computed over. It travels WITH the figure,
// never beside it in a comment, because a number whose set is unstated is one an operator
// reads as covering everything they own, and the queue and history views ship only the most
// recent rows.
//
// Set names the matching rows; Window is "" for a figure over that whole set and otherwise
// names the BOUND that narrows it. The descriptor is produced by the same function that
// runs the query, so a future bounded figure cannot ship without saying what bounds it.
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
// Counted is the number of rows that contributed a key; Excluded is the number that
// recorded NO key and were left out rather than folded into an "other" bucket that would
// read as a real category. Counted == 0 means NO ROW CONTRIBUTED, which a reader must
// render as "no data" and never as a set of zero counts.
//
// Err is this aggregate's OWN failure, per-aggregate on purpose: the snapshot's summary,
// queue and history must still ship when one figure cannot be read, so it is rendered as
// unavailable rather than returned up a path that would suppress the whole frame.
type Breakdown struct {
	Coverage Coverage
	Buckets  []Bucket
	Counted  int64
	Excluded int64
	Err      error
}

// Spread is the shape of one numeric aggregate over every matching row: how many rows
// contributed a recorded value, how many were excluded for want of one, and the low, mean
// and high of what was recorded. Min, Mean and Max are nil exactly when Counted == 0.
//
// Deliberately min/mean/max and NOT a median or a percentile: those are version- and
// compile-flag-gated in SQLite (median() and percentile() need 3.51.0 built with
// SQLITE_ENABLE_PERCENTILE), and a query that resolves on the developer's build but not on
// the shipped image is a runtime failure on somebody else's machine. Everything here uses
// COUNT/MIN/AVG/MAX, which every SQLite build has.
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

// Aggregates is the whole-ledger report the dashboard publishes: the figures the queue and
// history views cannot give, because those ship at most a few hundred rows while a library
// holds hundreds of thousands. Every field is computed INDEPENDENTLY and carries its own
// Err, so one unreadable figure cannot blank the live page.
type Aggregates struct {
	// Outcomes is the count of terminal rows per status, over the whole table.
	Outcomes Breakdown
	// SkipsByGuard breaks every skipped row down by the guard token that skipped it, so an
	// operator never has to read the logs to learn WHICH guard fired.
	SkipsByGuard Breakdown
	// SizeRatio is the spread of output size / source size over done rows.
	SizeRatio Spread
	// EncodeMs is the spread of wall-clock encode duration, in milliseconds.
	EncodeMs Spread
	// VmafMean and VmafMin are the spreads of the two pooled VMAF statistics each done row
	// recorded. A row that recorded neither is excluded and counted, never read as a zero.
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
// resolved. Every resolution carries one for EACH path; a resolution that leaves either
// path without one is refused, because an unstated disposition is exactly the state in
// which nobody can say whether a file holdfast wrote is still out there.
type Disposition string

// The four dispositions.
const (
	// KeptInPlace: the file stays and later runs treat that path NORMALLY. It is never
	// legal for a recorded REPLACEMENT path: a file holdfast wrote is never handed back to
	// enumeration as if it were a source.
	KeptInPlace Disposition = "kept-in-place"
	// RetainedExcluded: the file stays and that path remains OUT of enumeration for as long
	// as the record survives.
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

// SwapIncident is the durable record of a swap that did not complete cleanly: what is
// needed to identify BOTH files without logs, plus the operator's determination once one is
// made.
//
// It lives in its own table rather than as more columns on the jobs row, and that is
// load-bearing: the jobs row is CLEARED by Claim and PRUNED after a successful transcode,
// while the exclusion a recorded replacement path carries has to outlive both, because the
// file holdfast wrote is still on disk regardless of what happens to the job that wrote it.
type SwapIncident struct {
	ID int64

	// SourcePath and SourceFingerprint are the job this incident belongs to.
	SourcePath        string
	SourceFingerprint string

	// ReplacementPath is the path holdfast last knows the replacement to be at, which after
	// a retained failure is the retained-replacement path rather than the in-flight temp.
	ReplacementPath string

	// SourceAttrs and ReplacementAttrs are the rename-invariant attribute records taken
	// BEFORE the rename was attempted, in probe.Attributes' "size:mtime" spelling. They are
	// what makes the two files identifiable from the record alone.
	SourceAttrs      string
	ReplacementAttrs string

	// ObservedAttrs is what the post-failure re-stat saw, "" when it could not be completed.
	// On an Outcome of AppliedDespiteError it is the evidence the applied case matched on.
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

	// JobOutcome is the proof the pipeline had accumulated when the swap failed.
	// RecordSwapIncident writes it onto the JOB row in the same transaction as the
	// incident, so a parked job's row still carries what an ordinary terminal row would.
	JobOutcome *Outcome

	// --- the resolution half; every field is zero while the job is parked -------

	Resolution                      Determination
	ResolvedBy                      string // who made it: "operator"
	ResolvedAt                      int64
	ObservedAtResolutionSource      string
	ObservedAtResolutionReplacement string
	DispositionSource               Disposition
	DispositionReplacement          Disposition

	// RemovalError records that a removal the resolution licensed did NOT succeed. When it
	// is non-empty the replacement's disposition has been corrected away from Deleted, so
	// the surviving file is still held out of enumeration: a licensed removal that fails
	// must never leave a record claiming a deletion that did not happen.
	RemovalError string

	// Stamp is the schema version this incident was last written under. It matters most
	// here: an incident outlives the job that created it, so it is the record most likely
	// to be read by a build that is not the one which wrote it.
	Stamp SchemaStamp
}

// Parked reports whether this incident is a parked job: indeterminate, undetermined.
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

// Prune is what one retention pass actually did. It is returned rather than logged inside
// the store so the caller can report it: a prune is the one IRREVERSIBLE act in this
// package, and an operator is owed the count.
//
// Removed is how many terminal rows were deleted. ReclaimedCarried is the bytes those rows
// contributed to the lifetime reclaimed total, moved into the durable carry-forward BEFORE
// they were deleted, so the published total does not move. Kept is how many rows the pass
// EXAMINED and refused, because removing them would change what the engine does with a
// file; it counts examined rows rather than the whole table, so it is "what this pass
// refused", the figure that tells an operator why a table stayed large.
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
// run after Claim under whatever configuration is current, so a pruned row re-derives its
// own verdict only while that configuration has not moved. Answering means knowing whether
// the file is still in the library, which is a question about the filesystem this package
// never touches, so the caller answers.
//
// TRUE means "removing this row can cause no encode". FALSE is the safe answer and must be
// the answer whenever the caller cannot tell.
type Prunable func(path, fingerprint string, s Status) bool

// RowTotal is a count of matching rows in the LEDGER, beside the capped rows a response
// ships: /api/queue and /api/history return at most a few hundred rows, and what they are a
// few hundred OF is not something a client can derive correctly.
//
// Err is this figure's OWN failure, carried the way an Aggregate carries one: a total that
// could not be read must be STATED as unreadable beside rows that still ship, never
// reported as a total of zero.
type RowTotal struct {
	Coverage Coverage
	Count    int64
	Err      error
}

// Retained is one original the undo window is holding: a SECOND LINK to the bytes a swap
// replaced, plus everything a restore needs to put them back safely. It does not live on a
// jobs row, which the swap itself prunes, so a retention recorded there would be deleted by
// the event it exists to undo.
//
// SourcePath is the path the original is restored TO. SwappedPath is what the swap
// produced: the same path for an in-place rename, a different one when the container
// extension changed. RetainedPath is the second link holding the original's bytes alive.
// SwappedFingerprint is the size:mtime of SwappedPath taken immediately after the swap, and
// a restore refuses when it disagrees with what is there NOW, so an operator can never
// overwrite content this tool did not write. SourceBytes is what the undo window is
// HOLDING: space a reclaimed figure must not count as returned, because it has not been.
// RestoredAt is nil until the original is put back, and a RELEASED retention is deleted
// outright, so "something is retained for this path" is exactly "a row exists whose
// RestoredAt is nil".
type Retained struct {
	SourcePath         string
	SwappedPath        string
	RetainedPath       string
	SourceBytes        int64
	SwappedFingerprint string
	RetainedAt         int64
	ExpiresAt          int64
	RestoredAt         *int64

	// Stamp is the schema version this retention was last written under, carried for the
	// reason every record carries one: a reader holding it can tell which build's
	// semantics filled it rather than inferring that from which fields are empty.
	Stamp SchemaStamp
}

// Store is the persistent job ledger. Every method is safe for concurrent use by
// multiple workers (goroutines) within one process.
type Store interface {
	// RecoverStale resets any job left in an active state back to pending - the mark of a
	// prior crashed run, since a live worker holds its claim only for the duration of one
	// in-process call. Call once at startup, before any scan.
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

	// Delete removes the row for path+fingerprint, a no-op if absent. After a successful
	// transcode the pre-swap row is deleted, so the table does not accumulate one dangling
	// row per transcoded file.
	Delete(ctx context.Context, path, fingerprint string) error

	// Get returns the current status and fail_count for path+fingerprint, and
	// whether a row exists at all (exists=false + status="" means never seen).
	Get(ctx context.Context, path, fingerprint string) (status Status, failCount int, exists bool, err error)

	// List returns job rows for reporting, newest-updated first. A non-empty statuses
	// filters to that set; limit > 0 caps the result. It is a pure read, so no amount of
	// API traffic can alter file handling.
	List(ctx context.Context, statuses []Status, limit int) ([]Job, error)

	// Summary counts rows per status; only statuses with at least one row appear.
	Summary(ctx context.Context) (map[Status]int, error)

	// ReclaimedTotal is the durable lifetime reclaimed-space total: the sum of
	// (source_bytes - output_bytes) over every Done row that recorded both, which is what a
	// reclaimed figure must be built on instead of a per-process counter that resets on
	// every restart. Rows carrying no sizes are not counted, never counted as zero. It sums
	// the live rows PLUS the durable carry-forward a prune leaves behind, so bounding the
	// ledger cannot make this figure run backwards.
	ReclaimedTotal(ctx context.Context) (int64, error)

	// PruneTerminal enforces a ledger retention of at most maxRows TERMINAL rows, deleting
	// the oldest beyond it. maxRows <= 0 is retention DISABLED and reads nothing.
	//
	// Three rules govern what it may take:
	//
	//  1. A row's contribution to the durable lifetime reclaimed total is CARRIED FORWARD
	//     into ledger_totals in the same transaction that deletes it, so the published
	//     total is identical either side of a prune, on the running server whose baseline
	//     is frozen at startup and after the restart that re-reads it.
	//  2. A row prunable says no to is KEPT. A terminal row is a decision the engine
	//     enforces through Claim, so removing the row of a file still in the library hands
	//     that file to the encoder whenever the configuration the verdict was taken under
	//     has since moved. Only the caller can see the library. A nil prunable keeps all.
	//  3. A FAILED row at maxFailures is PARKED: the engine refuses to claim it, and
	//     deleting it would reset that accounting and hand the file back to the encoder.
	//     The store can see this in its own columns, so it is refused here as well as by
	//     rule 2: the one irreversible act in this package does not rest on a single check.
	//
	// Rules 2 and 3 are counted in Prune.Kept and are why the ledger may sit ABOVE maxRows:
	// a retention that cannot be met without causing an encode is not met, and the count is
	// reported rather than hidden.
	//
	// The pass runs in BATCHES, each its own transaction: a 300,000-row ledger must not
	// hold the single serialized write connection for one enormous DELETE, and a failure
	// part way through leaves the rows it did not remove in place with the total already
	// correct for the rows it did.
	PruneTerminal(ctx context.Context, maxRows, maxFailures int, prunable Prunable) (Prune, error)

	// CountRows counts the rows matching statuses over the WHOLE table: the total a capped
	// response was capped against. It is its own read rather than a projection of Summary,
	// for the reason the aggregates are their own reads: /api/queue and /api/history must
	// still return their rows when this figure cannot be read.
	CountRows(ctx context.Context, statuses []Status) RowTotal

	// EachTerminal streams every terminal row, oldest transition first. A ledger that has
	// outgrown a capped API response has also outgrown a []Job, so rows are handed over one
	// at a time and never accumulated. fn's error stops the walk and is returned.
	EachTerminal(ctx context.Context, fn func(Job) error) error

	// Aggregates computes the published whole-ledger figures, each over EVERY matching row
	// and never over the capped rows List ships. It returns no error: every figure reports
	// its own failure in its Err, so one unreadable aggregate is rendered unavailable while
	// the snapshot carrying it still ships.
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

	// ClearSkip deletes the row ONLY when it is a Skipped row whose reason matches: the
	// re-evaluation half of a MUTABLE guard. The hardlink guard re-checks every scan, since
	// a seed may finish and drop the link count, and this removes the stale skip so the
	// file is reclaimed on the normal path. The reason+status match is what keeps it from
	// ever touching a real outcome. No-op when no such row exists.
	ClearSkip(ctx context.Context, path, fingerprint, reason string) error

	// RecordSwapIncident persists a swap that did not complete cleanly. It writes the
	// incident row AND moves the job row to the matching status in ONE transaction, so a
	// job can never sit in a state whose supporting facts were not written, or the reverse.
	// An error means NEITHER landed, which is the only condition under which the caller may
	// treat the outcome as unpersisted.
	RecordSwapIncident(ctx context.Context, in SwapIncident) error

	// ParkedIncidents returns every parked job - recorded indeterminate, no operator
	// determination yet - oldest first. A run reads this at startup to report them and
	// to hold both of each job's recorded paths back from the scan.
	ParkedIncidents(ctx context.Context) ([]SwapIncident, error)

	// ExcludedReplacementPaths returns every path a record carries as a job's replacement
	// path whose disposition is neither deleted nor absent: every path where a file
	// holdfast wrote may still be, none of which enumeration may treat as a source. The
	// exclusion is keyed to the EXISTENCE OF THE RECORD and not to the parked state, since
	// a replacement retained after a job is resolved is still a file holdfast wrote, and
	// enumerating it would queue a gate-passed encode as a source and leave a permanent
	// duplicate.
	ExcludedReplacementPaths(ctx context.Context) ([]string, error)

	// ResolveIncident records an operator's determination against a parked incident and
	// RELEASES the job: the incident keeps the durable record, and the jobs row is removed
	// so a later run treats that path as new work rather than as a job to re-park. Both
	// dispositions are required and a replacement path may never be kept-in-place.
	ResolveIncident(ctx context.Context, id int64, r Resolution) error

	// AmendReplacementDisposition corrects an already-recorded replacement disposition and
	// attaches the reason. It exists for one situation: the removal a resolution licensed
	// was ordered AFTER the record was made durable and then did not succeed. The record
	// must not go on claiming a deletion that did not happen, so the disposition moves back
	// to retained-excluded and the surviving file stays out of enumeration.
	AmendReplacementDisposition(ctx context.Context, id int64, d Disposition, removalErr string) error

	// IncidentByID returns one incident.
	IncidentByID(ctx context.Context, id int64) (SwapIncident, bool, error)

	// HeldByUndoWindow is the number of bytes the undo window is still HOLDING: the sum of
	// SourceBytes over every retention neither restored nor released. It is reported BESIDE
	// ReclaimedTotal and never folded into it, because a retained original's bytes have not
	// been returned to the filesystem and a reclaimed figure that counted them would tell an
	// operator space is free while it is not. It falls to zero as the releases run.
	HeldByUndoWindow(ctx context.Context) (int64, error)

	// Retain records one retained original, replacing any earlier record for the same
	// source path: an earlier one can only be a retention that was already restored, since
	// a live one blocks the swap and a released one is deleted.
	Retain(ctx context.Context, r Retained) error

	// GetRetained returns the retention record for path, which may be named EITHER by its
	// source path or by the path the swap produced: after a container-changing swap the
	// only name an operator can see in their library is the latter, and being asked to
	// restore the file that is actually there must not be a miss. Rows already restored ARE
	// returned, so a caller can report the restore rather than an absence; a released
	// retention is gone from the table entirely.
	GetRetained(ctx context.Context, path string) (r Retained, exists bool, err error)

	// ListRetained returns every LIVE retention, oldest expiry first. It is what the release
	// sweep walks and what `holdfast restore` lists.
	ListRetained(ctx context.Context) ([]Retained, error)

	// MarkRestored stamps a retention as restored at unix second at. The row is KEPT: the
	// restore is a ledger fact, and deleting it would leave the ledger reporting only the
	// swap that has just been undone.
	MarkRestored(ctx context.Context, sourcePath string, at int64) error

	// DropRetained deletes a retention record outright: the release sweep once the retained
	// name is gone, and a restore that finds the original no longer on disk. In both cases
	// there is nothing left to restore, and a record that promised one would be a promise
	// the tool cannot keep.
	DropRetained(ctx context.Context, sourcePath string) error

	// Close releases the underlying database handle.
	Close() error
}
