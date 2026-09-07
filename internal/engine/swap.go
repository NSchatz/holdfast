package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/NSchatz/holdfast/internal/fsclass"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// The honest outcome of a FAILED swap (FILESYSTEM-1).
//
// Before this file, the rename error path was three lines: log "swap error, source
// untouched", delete the temp, record Failed. On a local filesystem that is true. On a
// network one rename(2) says outright that it is not knowable - "On NFS filesystems,
// you can not assume that if the operation failed, the file was not renamed. If the
// server does the rename operation and then crashes, the retransmitted RPC which will
// be processed when the server is up again causes a failure" - so holdfast was telling
// an operator the most comforting thing it could say at the exact moment it had the
// least idea whether the thing was true.
//
// What replaces it: re-stat the source after EVERY failed swap, whatever the storage,
// and decide between four exhaustive cases. Only one of them is allowed to say
// "untouched", and it requires BOTH that the observed attributes are the ones recorded
// for the source AND that the storage is positively classified local - because a client
// attribute cache populated before the swap returns exactly the pre-swap answer whether
// or not the rename was applied, so on network storage a match proves nothing at all.

// RetainedMarker is the fixed infix in the name of a replacement holdfast is KEEPING
// because the job that produced it did not complete cleanly. It is deliberately a
// DIFFERENT marker from TempMarker, and the difference is load-bearing.
//
// A `__transcoding__` file is work in progress: a killed run leaves them behind and
// the next startup sweeps them, which is right, because a partial encode is worth
// nothing. A `__holdfast-replacement__` file is the opposite: it PASSED every gate, it
// may be the only faithful copy of a source whose fate is unknown, and nothing in this
// program may ever delete it on its own initiative. Giving the two states two names is
// what lets the sweep keep doing its job while the retained file is untouchable - and
// it is what stops a fresh encode of a reclaimed source from colliding with a retained
// replacement of the same source, since the two constructions cannot produce the same
// string.
const RetainedMarker = "__holdfast-replacement__"

// maxPathCandidates bounds the search for a free constructed path. It is small on
// purpose: needing more than a handful means something is very wrong, and a loud
// failure beats silently walking a directory.
const maxPathCandidates = 64

// tempPath and retainedReplacementPath ARE the build's own construction of a
// replacement path, and they are the whole of it. Everything that holds a path back on
// its NAME rather than on a record matches exactly what these produce and nothing else
// - never a widened "anything with a dot in it" or "anything that looks temporary"
// pattern, which would hold back files holdfast did not write.
//
// The n suffix exists so a second retained replacement for the same source does not
// have to overwrite the first. n == 0 is the bare form, so the common case is the name
// this repo has always used.
func tempPath(dir, stem, ext string, n int) string {
	return filepath.Join(dir, stem+"."+TempMarker+suffix(n)+"."+ext)
}

func retainedReplacementPath(dir, stem, ext string, n int) string {
	return filepath.Join(dir, stem+"."+RetainedMarker+suffix(n)+"."+ext)
}

func suffix(n int) string {
	if n == 0 {
		return ""
	}
	return "." + strconv.Itoa(n)
}

// IsRetainedReplacementName reports whether base is EXACTLY a name
// retainedReplacementPath could have produced: a non-empty stem, the marker, an
// optional all-digit ordinal, and a simple extension.
//
// This is the RECORD-FREE hold-back, and it is the only one. It exists because the one
// case that most needs holding back is the one where no record could be written - the
// job store was unwritable, which is precisely what denied the record - so a hold-back
// that depended on a record would be absent exactly when it matters. It is matched
// exactly rather than by pattern: nothing else in the library is held back on its name,
// so an ordinary source in the same roots still enumerates, encodes and swaps.
//
// The extension is not checked against the configured video extensions, deliberately.
// Tying the matcher to configuration would mean editing a config key silently released
// a file holdfast wrote back into enumeration, and the fail-safe runs the other way.
func IsRetainedReplacementName(base string) bool {
	infix := "." + RetainedMarker + "."
	i := strings.Index(base, infix)
	if i <= 0 { // i == 0 would mean no stem at all
		return false
	}
	rest := base[i+len(infix):]
	if j := strings.IndexByte(rest, '.'); j >= 0 {
		return allDigits(rest[:j]) && isSimpleExt(rest[j+1:])
	}
	return isSimpleExt(rest)
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isSimpleExt(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// swapCause names the cause of a rename failure when the cause is one holdfast reports
// DISTINCTLY. Today there is exactly one: EXDEV, which rename(2) defines as "oldpath
// and newpath are not on the same mounted filesystem (Linux permits a filesystem to be
// mounted at multiple points, but rename() does not work across different mount points,
// even if the same filesystem is mounted on both)".
//
// It returns "" for every other error, which is the whole of AC19: no other swap
// failure is attributed to this cause. holdfast does not copy or fall back across the
// boundary either - the cause is REPORTED, not worked around, because a cross-filesystem
// copy is a different operation with a different (non-atomic) safety story.
func swapCause(err error) string {
	if errors.Is(err, syscall.EXDEV) {
		return store.SwapCauseCrossFilesystem
	}
	return ""
}

// swapOutcome is the decision the four exhaustive cases produce.
type swapOutcome struct {
	Status   store.Status // Failed (source untouched) | AppliedDespiteError | Indeterminate
	Case     string       // "a".."d", or "restat-failed" / "indistinguishable"
	Why      string       // one clause, for the log line and the stored reason
	Observed string       // what the re-stat saw, "" when it could not be completed
}

// decideFailedSwap applies AC14a's four exhaustive cases to one failed swap.
//
// The inputs are all facts, never assumptions: srcRec and replRec were recorded BEFORE
// the rename was attempted, observed/restatErr come from a re-stat taken AFTER it, and
// cls is a classification taken at the time of THAT swap rather than lifted from an
// earlier record - storage can be mounted under a path after a run began, and a stale
// record is wrong exactly when it matters most.
func decideFailedSwap(srcRec, replRec probe.Attributes, observed probe.Attributes, restatErr error, cls fsclass.Classification) swapOutcome {
	// The re-stat could not be completed at all. Nothing is established, so nothing is
	// reported: not success, not untouched, not applied.
	if restatErr != nil {
		return swapOutcome{
			Status: store.Indeterminate, Case: "restat-failed",
			Why: "the re-stat after the failed swap could not be completed (" + restatErr.Error() + "), so what happened at the source path is unknown",
		}
	}
	obs := observed.String()

	// The two pre-swap records are the same. A re-stat then cannot tell the two files
	// apart no matter what it returns, so neither the applied case nor the untouched
	// case is establishable and the honest answer is that this is unknown.
	if srcRec == replRec {
		return swapOutcome{
			Status: store.Indeterminate, Case: "indistinguishable", Observed: obs,
			Why: "the source and the replacement had identical pre-swap attributes (" + srcRec.String() + "), so a re-stat cannot tell them apart",
		}
	}

	switch {
	// (a) The observed attributes are the ones recorded for the REPLACEMENT. The
	// rename took effect despite reporting an error. This case does NOT consult the
	// classification: the replacement's attributes at the source path are not something
	// a stale cache can invent, because the cache was populated when the SOURCE was
	// there.
	case observed == replRec:
		return swapOutcome{
			Status: store.AppliedDespiteError, Case: "a", Observed: obs,
			Why: "the re-stat at the source path returned the attributes recorded for the replacement (" + replRec.String() + "), so the rename took effect despite reporting an error",
		}

	// (b) The observed attributes are the source's AND the storage is positively local.
	// This is the ONLY path to "untouched".
	case observed == srcRec && cls.IsLocal():
		return swapOutcome{
			Status: store.Failed, Case: "b", Observed: obs,
			Why: "the re-stat at the source path returned the attributes recorded for the source (" + srcRec.String() + ") on storage classified " + cls.String() + ", so the source is untouched",
		}

	// (c) The observed attributes are the source's but the storage is NOT local. A
	// client attribute cache populated before the swap returns exactly this answer
	// whether or not the rename was applied, so the match proves nothing.
	case observed == srcRec:
		return swapOutcome{
			Status: store.Indeterminate, Case: "c", Observed: obs,
			Why: "the re-stat returned the source's pre-swap attributes (" + srcRec.String() + ") but the storage is " + cls.String() + ", where a client attribute cache returns exactly that whether or not the rename was applied",
		}

	// (d) The observed attributes match neither record.
	default:
		return swapOutcome{
			Status: store.Indeterminate, Case: "d", Observed: obs,
			Why: "the re-stat at the source path returned " + obs + ", which matches neither the source's pre-swap record (" + srcRec.String() + ") nor the replacement's (" + replRec.String() + ")",
		}
	}
}

// residualWindowFor is the CLASS LABEL for the residual window that applies to storage
// with this classification. There are exactly two windows and no third: `undetermined`
// takes the NETWORK one, the same fail-safe that makes an unrecognised type not-local,
// so a guard-time lookup that failed, returned nothing, returned a name this build does
// not know, or was denied yields the network window.
//
// It is a label and never a duration. The network window belongs to the client's
// attribute cache and nfs(5) states only "Every few seconds" - no interval, no tunable -
// so a number here would be invented. What IS measured (the attributes compared and the
// resolution of the timestamp compared) is recorded beside it.
func residualWindowFor(c fsclass.Classification) string {
	if c.IsLocal() {
		return store.ResidualWindowLocal
	}
	return store.ResidualWindowNetwork
}

// replacementIsAt reports where the replacement ACTUALLY is after a failed rename.
//
// Normally that is the temp path: the rename reported an error and did not move
// anything. But the swap's target is not always the source path. When the output
// container extension differs from the source's (movie.mp4 -> movie.mkv, `container_ext`
// forced) the rename targets a DIFFERENT path, and a rename that took effect while
// reporting an error - the rename(2) retransmission hazard this whole file exists for -
// leaves the replacement at that target, under an ordinary source name, with the temp
// path empty.
//
// Following the file is what keeps the record honest (AC15a records THE PATH THE
// REPLACEMENT IS AT, so both files can be identified from the record alone) and what
// lets the retained-name hold-back reach it at all (AC15i): an ordinary source name is
// never held back on its name and must not be, so a replacement left sitting at one is
// held by nothing. Locating it is what lets the caller move it to a name that IS held.
//
// It is deliberately conservative in three directions at once:
//
//   - it looks at the target ONLY when the temp path is empty, so a rename that plainly
//     did not apply is never second-guessed;
//   - it looks at the target only when the target is NOT the source path, so the file at
//     the source path - which the four-case outcome decision is entirely about - is
//     never something this moves or reasons about;
//   - and it accepts what is at the target only when that file carries the attributes
//     recorded for the REPLACEMENT before the swap, which is the same evidence AC14a
//     case (a) decides on and the same evidence the record itself carries.
//
// Anything else falls back to the temp path. A record naming a path with no file at it
// is a case the operator action already handles honestly (AC15b/AC15f report it absent
// and still resolve the job); silently losing a file it never names is not.
func (e *Engine) replacementIsAt(tmp, final, src string, replRec probe.Attributes) string {
	if _, err := os.Lstat(tmp); err == nil {
		return tmp
	}
	if final == src {
		return tmp
	}
	if at, err := probe.StatAttributes(final); err == nil && at == replRec {
		return final
	}
	return tmp
}

// crossFilesystemReport is the DISTINCT report AC17 requires, and it names both paths
// (AC18) so an operator can see the boundary rather than infer it. It deliberately says
// nothing about the state of the source: that is the outcome decision's to report, and
// asserting "untouched" here is exactly the bug this phase removes.
func crossFilesystemReport(tmp, target string) string {
	return "cross-filesystem swap: the temp and the target are not on the same mounted filesystem" +
		" (temp " + tmp + ", target " + target + "); holdfast reports this cause and does not copy across the boundary"
}

// handleFailedSwap is the whole of the failed-swap path: classify at swap time, re-stat
// unconditionally, decide, then persist and report whatever was decided.
//
// It never returns an error to the caller for a per-file outcome - the scan must not
// stop for one file - so every branch either records something or reports loudly that
// it could not.
func (e *Engine) handleFailedSwap(ctx context.Context, f, key, tmp, final string,
	srcRec, replRec probe.Attributes, renameErr error, out *store.Outcome) {

	// AC14d: the classification is taken NOW, for THIS swap, never lifted from a record
	// made earlier in the run or by an earlier job. A NAS mounted beneath a root hours
	// after the run began is the case that makes an older record wrong exactly when it
	// matters most. This is not discovery: it imposes nothing on startup and never
	// reopens whether the run should have started.
	cls := fsclass.Of(e.fsLookup, f)

	// AC13/AC13a: re-stat before reporting ANY outcome. Not skipped because the storage
	// is local, not skipped because it is undetermined, and not skipped because the
	// failure looked unambiguous - the whole point is that an unambiguous-looking
	// failure is exactly what a retransmitted rename produces.
	observed, restatErr := e.restat(f)

	dec := decideFailedSwap(srcRec, replRec, observed, restatErr, cls)

	// WHERE THE REPLACEMENT IS. The outcome above is decided at the SOURCE path, which
	// is what AC13a names; where the replacement ended up is a separate question, and on
	// the ext-changing shape (final != f) the answer is not always the temp path. Asked
	// once, here, so every branch below records and holds back the path the file is
	// actually at rather than the path it started from.
	replAt := e.replacementIsAt(tmp, final, f, replRec)

	cause := swapCause(renameErr)
	out.SwapCause = cause
	report := "swap failed: " + renameErr.Error()
	if cause == store.SwapCauseCrossFilesystem {
		report = crossFilesystemReport(tmp, final)
	}

	// AC14e's guard: case (a) says the rename took effect, and a rename that took
	// effect cannot account for two files. If something is still at the path the record
	// will name as the replacement's, the applied story does not hold and the pair is
	// parked.
	if dec.Status == store.AppliedDespiteError {
		if _, err := os.Lstat(replAt); err == nil {
			dec = swapOutcome{
				Status: store.Indeterminate, Case: "a-contradicted", Observed: dec.Observed,
				Why: "the re-stat matched the replacement's pre-swap record but a file is STILL present at the recorded replacement path (" + replAt + "), and a rename that took effect cannot account for both",
			}
		}
	}

	switch dec.Status {
	case store.Failed:
		// Case (b), and the only branch that may say "untouched". This is the outcome
		// the pin always reported; the difference is that it is now established rather
		// than assumed. The temp is discarded exactly as before: this job recorded a
		// plain failure with the source intact, which is none of the three origins that
		// make a replacement untouchable (AC15i).
		_ = os.Remove(tmp)
		out.Reason = report + " - " + dec.Why
		if replAt != tmp {
			// The ext-changing shape, on storage this run positively identified as
			// local: the rename took effect at the TARGET even though it reported an
			// error, so the gate-passed replacement is there beside a source this
			// re-stat has just confirmed intact. Both files are on disk - the identical
			// state a crash between the rename and the source removal leaves, which this
			// repository already handles by leaving both and letting the collision guard
			// reconcile the duplicate on the next scan. Nothing is deleted here: the
			// source is established intact, so no data is at risk, and removing a file at
			// an ordinary media name on the strength of a size-and-whole-second-mtime
			// match is not a trade this tool makes. It IS reported, durably and not only
			// in the log, because an operator told only "the source is untouched" would
			// never learn a second file exists.
			dup := "the gate-passed replacement is at " + replAt +
				", beside the untouched source: both files are on disk and the next scan's collision guard reconciles the duplicate"
			out.Reason += " - " + dup
			e.Log.Warn("FAIL ("+report+"; source confirmed untouched; "+dup+")", "file", f,
				"cause", causeOrNone(cause), "storage", cls.String(), "case", dec.Case,
				"replacement", replAt)
		} else {
			e.Log.Warn("FAIL ("+report+"; source confirmed untouched)", "file", f,
				"cause", causeOrNone(cause), "storage", cls.String(), "case", dec.Case)
		}
		e.finish(ctx, f, key, store.Failed, out)
		return

	case store.AppliedDespiteError:
		e.Log.Warn("APPLIED DESPITE ERROR ("+report+")", "file", f,
			"cause", causeOrNone(cause), "storage", cls.String(),
			"observed", dec.Observed, "replacement_record", replRec.String())
		e.recordIncident(ctx, store.SwapIncident{
			SourcePath: f, SourceFingerprint: key,
			// The replacement is AT the source path now; the recorded replacement path
			// is where it was, and the guard above established there is nothing there.
			ReplacementPath:  replAt,
			SourceAttrs:      srcRec.String(),
			ReplacementAttrs: replRec.String(),
			ObservedAttrs:    dec.Observed,
			Outcome:          store.AppliedDespiteError,
			SwapError:        report + " - " + dec.Why,
			SwapCause:        cause,
			StorageClass:     string(cls.Class), StorageType: cls.Type,
			JobOutcome: out,
		}, f, replAt, srcRec, replRec)
		return

	default:
		// Indeterminate. Both files are kept. The replacement is first moved from
		// wherever it actually is to a retained name, which is what makes it
		// recognisable WITHOUT a record - the store write below may be the very thing
		// that fails.
		retained := e.retainReplacement(replAt, final)
		e.Log.Warn("INDETERMINATE ("+report+")", "file", f,
			"cause", causeOrNone(cause), "storage", cls.String(), "case", dec.Case,
			"replacement", retained, "source_record", srcRec.String(),
			"replacement_record", replRec.String(), "observed", orNotObserved(dec.Observed))
		e.recordIncident(ctx, store.SwapIncident{
			SourcePath: f, SourceFingerprint: key,
			ReplacementPath:  retained,
			SourceAttrs:      srcRec.String(),
			ReplacementAttrs: replRec.String(),
			ObservedAttrs:    dec.Observed,
			Outcome:          store.Indeterminate,
			SwapError:        report + " - " + dec.Why,
			SwapCause:        cause,
			StorageClass:     string(cls.Class), StorageType: cls.Type,
			JobOutcome: out,
		}, f, retained, srcRec, replRec)
	}
}

// recordIncident persists one of the two outcomes this phase adds and emits it, or -
// when the job store cannot be written - reports everything the record would have
// carried so an operator holds it anyway.
//
// The unwritable-store branch is the point of the whole function. Neither file is
// deleted, the swap is not re-attempted, the source is not re-queued in this run, and
// nothing claims success or an untouched source. No record survives to carry the
// enumeration exclusion into the next run, which is exactly why the replacement was
// moved to a name a later run recognises without one.
func (e *Engine) recordIncident(ctx context.Context, in store.SwapIncident,
	sourcePath, replacementPath string, srcRec, replRec probe.Attributes) {

	if err := e.Store.RecordSwapIncident(ctx, in); err != nil {
		e.Log.Error("COULD NOT PERSIST the outcome of a failed swap - both files are being kept and nothing was re-attempted",
			"outcome", string(in.Outcome),
			"source", sourcePath, "replacement", replacementPath,
			"source_pre_swap_attributes", srcRec.String(),
			"replacement_pre_swap_attributes", replRec.String(),
			"swap_error", in.SwapError,
			"store_error", err)
		return
	}
	e.emit(Event{Path: sourcePath, Status: in.Outcome, Outcome: in.JobOutcome})
}

// retainReplacement moves the replacement from wherever it is (`at` - see
// replacementIsAt, which is not always the temp path) to a retained-replacement name, so
// a later run recognises it as a file holdfast wrote even if no record of it survives.
// It returns the path the replacement is at - the retained one on success, `at` itself
// when the move could not be made, which is reported and is what the record then names.
//
// The name is derived from `final`, the swap's TARGET, so the retained file keeps the
// extension the replacement was encoded to rather than the source's.
//
// It uses os.Rename DIRECTLY and not the swap's rename seam. The seam substitutes THE
// SWAP - the one rename whose failure this whole file is about - and routing this move
// through it would mean a test injecting a swap failure also broke the retention that
// failure is supposed to trigger, hiding the behaviour under test behind the injection.
// This move is a same-directory rename of holdfast's own file and touches no source.
func (e *Engine) retainReplacement(at, final string) string {
	dir := filepath.Dir(at)
	base := filepath.Base(final)
	ext := strings.TrimPrefix(filepath.Ext(base), ".")
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	for n := 0; n < maxPathCandidates; n++ {
		p := retainedReplacementPath(dir, stem, ext, n)
		if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
			if err := os.Rename(at, p); err != nil {
				// Say only what is true. The old wording here asserted the file "stays
				// at the temp path and is held back by its record" - two claims this
				// branch cannot make, since the move most often fails because there is
				// nothing at that path at all, and the record that would hold it has not
				// been written yet (it may be exactly what fails next). What IS true is
				// the path the outcome record will name and whether a file is there.
				_, statErr := os.Lstat(at)
				e.Log.Warn("could not move the replacement to a held-back name - the outcome record below names this path and the operator action reports what is (or is not) there",
					"replacement", at, "retained", p, "file_present_there", statErr == nil, "err", err)
				return at
			}
			return p
		}
	}
	e.Log.Warn("no free retained-replacement name - the replacement stays where it is",
		"replacement", at, "candidates", maxPathCandidates)
	return at
}

func causeOrNone(cause string) string {
	if cause == "" {
		return "none"
	}
	return cause
}

func orNotObserved(s string) string {
	if s == "" {
		return "not observed"
	}
	return s
}

// resolvedForm is Definitions' "resolved form": symbolic links resolved, . and ..
// resolved away, any trailing separator removed. Two paths are the same path when
// their resolved forms are equal.
//
// EvalSymlinks needs the path to EXIST, and a recorded path whose file has been deleted
// by hand is precisely a case this has to survive - so a failure falls back to the
// lexical form, which still resolves . and .. and the trailing separator. That is
// weaker (it cannot see through a symlink to a file that is not there) and it is the
// safe direction: two paths that are really the same still compare equal unless a
// symlink is involved AND the target is gone.
func resolvedForm(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(real)
	}
	return filepath.Clean(p)
}

// holdBacks is one run's snapshot of every path that must not be touched, built once at
// the start of the run and read (never written) by the workers.
//
// There are exactly TWO hold-backs in this phase and this is both of them:
//
//  1. a parked job's two RECORDED paths (its source and its replacement), and
//  2. any recorded replacement path whose disposition is neither deleted nor absent.
//
// Everything else in the same roots enumerates, encodes and swaps normally. A parked
// job withholds two files from the work; it never narrows the run.
type holdBacks struct {
	paths  map[string]string // resolved form -> why
	parked []store.SwapIncident
}

func (h *holdBacks) held(p string) (string, bool) {
	if h == nil {
		return "", false
	}
	why, ok := h.paths[resolvedForm(p)]
	return why, ok
}

// loadHoldBacks reads the two record-based hold-backs and reports every parked job,
// naming both of its files. A store error is fail-safe in the cautious direction: it is
// reported and the run continues WITHOUT the record-based hold-backs, because refusing
// to scan at all would turn a store hiccup into a stopped library - but the record-free
// name-based hold-back still applies, so no file holdfast wrote is enumerated either way.
//
// That trade is deliberate and it is bounded, so the report says exactly which guarantee
// is standing on what:
//
//   - the RETAINED REPLACEMENT half is not affected at all. It is held back by its
//     NAME, which needs no record (that is what the name is for), so a replacement is
//     never enumerated, encoded or swept whatever the store says.
//   - the PARKED SOURCE half loses its hold-back and falls back to Claim, which refuses
//     an `indeterminate` row outright, so the file is not encoded or swapped. That
//     refusal is keyed on path+fingerprint, so it covers a parked source whose bytes
//     have not moved and NOT one that was rewritten since it was parked - which is the
//     residue, and the log names it rather than implying AC15c is fully enforced.
func (e *Engine) loadHoldBacks(ctx context.Context) *holdBacks {
	h := &holdBacks{paths: map[string]string{}}

	parked, err := e.Store.ParkedIncidents(ctx)
	if err != nil {
		e.Log.Error("could not read parked jobs - CONTINUING WITHOUT the record-based hold-back on parked paths, which is the one AC15c names; a retained replacement is still held back by its NAME, and a parked source is still refused by Claim unless its bytes have changed since it was parked",
			"err", err)
	}
	h.parked = parked
	for _, in := range parked {
		// AC15c: report it, naming both files. The key is the recorded pair of PATHS
		// and not a fingerprint - whether the bytes at the source path still match the
		// recorded attributes is the very thing that is unknown.
		e.Log.Warn("PARKED: a swap outcome could not be established and is awaiting an operator determination",
			"id", in.ID, "source", in.SourcePath, "replacement", in.ReplacementPath,
			"source_pre_swap_attributes", in.SourceAttrs,
			"replacement_pre_swap_attributes", in.ReplacementAttrs,
			"recorded", in.SwapError,
			"resolve_with", fmt.Sprintf("holdfast resolve --id %d", in.ID))
		h.paths[resolvedForm(in.SourcePath)] = "parked job " + strconv.FormatInt(in.ID, 10)
		h.paths[resolvedForm(in.ReplacementPath)] = "parked job " + strconv.FormatInt(in.ID, 10)
	}

	excluded, err := e.Store.ExcludedReplacementPaths(ctx)
	if err != nil {
		e.Log.Error("could not read recorded replacement paths - CONTINUING WITHOUT the record-based exclusion AC15d carries; every retained replacement is still held back by its NAME, which is what that name exists for",
			"err", err)
	}
	for _, p := range excluded {
		if _, ok := h.paths[resolvedForm(p)]; !ok {
			h.paths[resolvedForm(p)] = "a recorded replacement path"
		}
	}
	return h
}
