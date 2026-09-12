package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/NSchatz/holdfast/internal/encoder"
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
// A `__transcoding__` file is USUALLY work in progress: a killed run leaves them behind
// and the next startup sweeps them, which is right, because a partial encode is worth
// nothing. A `__holdfast-replacement__` file is the opposite: it PASSED every gate, it
// may be the only faithful copy of a source whose fate is unknown, and nothing in this
// program may ever delete it on its own initiative. Giving the two states two names is
// what lets the sweep keep doing its job while the retained file is untouchable - and
// it is what stops a fresh encode of a reclaimed source from colliding with a retained
// replacement of the same source, since the two constructions cannot produce the same
// string.
//
// "Usually" is the load-bearing word, and it is why strayReplacementHold exists. Moving
// a replacement to this name is a WRITE into the media directory, and the failure that
// strands a replacement in the first place is very often the same failure that denies
// that write. A `__transcoding__` file therefore has to be examined rather than assumed
// disposable; the name is the cheap half of the question and the content is the rest.
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

// splitConstruction decomposes base into the stem and extension the named construction
// would have been called with, and reports whether base is EXACTLY something that
// construction could have produced: a non-empty stem, the marker, an optional all-digit
// ordinal, and a simple extension.
//
// It is the whole of the RECORD-FREE basis, for BOTH markers. That basis exists because
// the one case that most needs holding back is the one where no record could be written -
// the job store was unwritable, which is precisely what denied the record - so a
// hold-back that depended on a record would be absent exactly when it matters. It is
// matched exactly rather than by pattern: nothing else in the library is held back on
// its name, so an ordinary source in the same roots still enumerates, encodes and swaps.
//
// The extension is not checked against the configured video extensions, deliberately.
// Tying the matcher to configuration would mean editing a config key silently released
// a file holdfast wrote back into enumeration, and the fail-safe runs the other way.
func splitConstruction(base, marker string) (stem, ext string, ok bool) {
	infix := "." + marker + "."
	i := strings.Index(base, infix)
	if i <= 0 { // i == 0 would mean no stem at all
		return "", "", false
	}
	stem = base[:i]
	rest := base[i+len(infix):]
	if j := strings.IndexByte(rest, '.'); j >= 0 {
		if !allDigits(rest[:j]) {
			return "", "", false
		}
		rest = rest[j+1:]
	}
	if !isSimpleExt(rest) {
		return "", "", false
	}
	return stem, rest, true
}

// IsRetainedReplacementName reports whether base is EXACTLY a name
// retainedReplacementPath could have produced.
func IsRetainedReplacementName(base string) bool {
	_, _, ok := splitConstruction(base, RetainedMarker)
	return ok
}

// IsTempConstructionName reports whether base is EXACTLY a name tempPath could have
// produced. It is the SECOND half of the record-free basis, and it is deliberately
// narrower than isTempName.
//
// isTempName ("contains .__transcoding__.") decides what the stale-temp SWEEP looks at,
// and it has to stay wide: a temp that ends up with an odd name is still work in
// progress and still has to be reclaimed. This one decides what may be HELD BACK on its
// name, and AC15i bounds that to what the build's own construction could have produced -
// "matched exactly and never by a widened temp-or-dotfile pattern". Holding a path back
// withholds it from the library for as long as the file is there, so the two questions
// get two matchers rather than one loose one shared between them.
func IsTempConstructionName(base string) bool {
	_, _, ok := splitConstruction(base, TempMarker)
	return ok
}

// strayReplacementHold reports WHY a file sitting at a temp path must be left alone, or
// "" when it is an ordinary orphan the sweep may take. It is the record-free hold-back
// at the one place a file holdfast wrote can still be destroyed: the stale-temp sweep
// and the temp-path picker, both of which remove a file at a path tempPath produced.
//
// It exists because the retained NAME is not, on its own, a record-free protection:
// getting a file to that name is itself a WRITE INTO THE MEDIA DIRECTORY. A library
// filesystem that has gone read-only (ext4's errors=remount-ro default, an NFS export
// turned ro, a full-ish volume) makes the swap rename fail - which is how a run reaches
// handleFailedSwap at all - and makes retainReplacement's rename, a rename in the same
// directory, fail for exactly the same reason; a state directory on that same mount then
// refuses the incident write and AC15h fires. One cause, every record denied, and a
// gate-passed replacement left sitting at a `__transcoding__` path. AC15i is
// unconditional across runs and names that origin explicitly, so the hold-back has to
// reach it without any write at all.
//
// THE TENSION, stated rather than glossed: an ordinary orphaned temp MUST still be
// swept, or a killed run's half-written encodes accumulate for ever and crash-safety
// regresses. So a path is NOT held on the temp name alone, and the content decides.
//
// THE RULE THAT DECIDES IT, and the direction each half fails in:
//
//  1. its NAME must be exactly what tempPath could have produced - never a widened
//     temp-or-dotfile pattern (AC15i bounds the record-free basis to the construction).
//     A name outside the construction is not this rule's business at all.
//  2. AN UNANSWERED QUESTION IS NOT A "NO". Every question past 3 costs an ffprobe
//     subprocess, and a "no" here is a DELETION - of the one file this phase exists to
//     protect, on a repository whose blast radius is "a wrong verdict is unrecoverable".
//     So the sweep needs a POSITIVE finding that the file is work in progress; anything
//     it could not establish HOLDS.
//  3. IS THERE ANYTHING BESIDE IT TO MEASURE IT AGAINST? Asked FIRST, before any
//     question that can fail, and answered by os.Lstat alone - no subprocess, no
//     configuration, nothing a failing host can take away. With no source beside it
//     there is nothing to measure the file against and NOTHING here can establish what
//     it is, so it holds; that is the case where the file may be the only copy of the
//     film there is, and the case the RetainedMarker comment above says can never
//     happen ("nothing in this program may ever delete it on its own initiative").
//     Being first is the whole point: a fail-safe reached only after three questions
//     that can each answer "no" for reasons that are not about the file is a fail-safe
//     that is unreachable exactly when it is needed.
//  4. WHAT IS THIS FILE? Asked as "could SOME encoder this build ships have written
//     it" (couldThisBuildHaveWrittenIt), never as "is it at the codec configured right
//     now". A stranded file was written by whichever encoder was configured THEN, and
//     `encoder:` is an ordinary config key: keying the hold to it would let an
//     unrelated edit release a file AC15i says may never be deleted in any later run.
//     A refusal here licenses a deletion only when the refusal is positively ABOUT THE
//     FILE, which takes three separate confirmations, because on the wire a verdict
//     about the file is indistinguishable from three things that are not one: it must
//     have ANSWERED (probe.VideoCodecAnswered -
//     not a binary that could not be started, not a killed subprocess), this host's
//     ffprobe must answer anything at all (Prober.Usable - a half-installed build exits
//     non-zero for every question, which is indistinguishable from reading a file and
//     rejecting it), and this process must be able to READ THE PATH (readableNow - a
//     restrictive mode, a `user:` change, an NFS export squashing the writing uid, an
//     SELinux denial and a transient EIO all make a perfectly working ffprobe exit
//     non-zero on a file that is present and byte-intact, and that is an answer about
//     the ACCESS, not about the content).
//  5. IS IT FINISHED? The verify gate's own length parity (gate 3, lengthParity, the
//     same function - so the sweep's licence to delete and the gate's licence to swap
//     cannot drift), measured against the source question 3 found. lengthParity itself
//     convicts only on evidence it has - two measured lengths that disagree - and
//     returns "no objection" for anything it could not measure, so the fail-safe
//     direction survives a probe failure here too.
//
// WHAT THIS ACTUALLY GUARANTEES, stated as the property rather than as the intent.
// Question 3 is unconditional and needs no subprocess, so a replacement with nothing
// beside it is held whatever else fails - no probe, no host and no config key can reach
// that answer. Past it, a replacement that reached a swap passes 4 and 5 by
// construction, so the residue is bounded to what can make one of those two answers
// wrong ABOUT A FILE THAT STILL HAS ITS SOURCE BESIDE IT: a source replaced by a
// DIFFERENT film between the two runs (length parity then fails against a file that is
// not the one the replacement was encoded from), a build whose encoder registry has
// since dropped the codec the file was written at, and a sandbox that denies ffprobe's
// domain a read this process is granted. Each costs an ENCODE, never the only copy,
// because the source is by construction still there. It is deliberately loose in the
// other direction - a partial encode that satisfies 4 and 5 is KEPT and reported, which
// costs an operator some disk and never a file.
//
// Both content checks are needed and neither is decorative. Measured on real ffmpeg: a
// libx265 encode of a 20-second source, killed part-way, ends up 3.6 seconds long while
// reporting codec `hevc` and DECODING CLEANLY - so question 4 alone (and a decode
// integrity check alone) would hold every partial encode for ever, and question 5 is
// what tells them apart. A hard-killed encode leaves a zero-length or header-only file,
// which a working ffprobe REFUSES outright while it stays perfectly readable, and
// question 4 takes it.
func (e *Engine) strayReplacementHold(ctx context.Context, path string) string {
	// Nothing there, or not a regular file: nothing to hold, and no subprocess spent
	// asking. This is also what keeps the picker's common case free of an extra probe.
	if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	stem, ext, ok := splitConstruction(filepath.Base(path), TempMarker)
	if !ok {
		return ""
	}
	// A cancelled run can establish nothing: CommandContext kills every probe below, so
	// each would come back empty for a reason that is not about the file. The sweep's
	// entry loops stop on the same signal; this is what makes the answer safe when the
	// cancellation lands after that check, and what covers pickTempPath as well.
	if ctx.Err() != nil {
		return "a file at a temp path this build constructed, on a run that is being cancelled - " +
			"nothing can be asked about it, and an unanswered question is not permission to delete"
	}

	// Question 3, FIRST. The only question here that cannot fail for a reason that is
	// not about the file, so it is the one the deepest fail-safe is built on.
	src, found := e.sourceBeside(filepath.Dir(path), stem, ext)
	if !found {
		return "a file at a temp path this build constructed with no source beside it left to measure it " +
			"against - nothing here can establish what it is, and it may be the only copy of the film there is"
	}

	// Question 4. What is this file?
	codec, answered := e.Probe.VideoCodecAnswered(ctx, path)
	if !answered {
		return "a file at a temp path this build constructed that ffprobe could not be asked about - " +
			"an unanswered question is not permission to delete"
	}
	if !couldThisBuildHaveWrittenIt(codec) {
		// ffprobe exited of its own accord. That is NOT yet a verdict on the file:
		// ProcessState.Exited() is true whether it exited because the file is not media
		// or because open() failed, and the two are the same on the wire. Both remaining
		// confirmations are about which of those it was.
		if !e.Probe.Usable(ctx) {
			return "a file at a temp path this build constructed, with ffprobe answering nothing at all on " +
				"this host - its refusal is evidence about the host, not about the file"
		}
		if err := readableNow(path); err != nil {
			return "a file at a temp path this build constructed that this process cannot read (" + err.Error() +
				") - ffprobe's refusal is evidence about the access to the path, not about the content of the file"
		}
		return "" // work in progress, or not media at all: the sweep's to take
	}

	// Question 5. It IS something this build could have written; all that is left is
	// whether it is finished, and only the source beside it can say.
	if _, err := e.lengthParity(ctx, src, path); err != nil {
		return "" // a truncated encode: work in progress, and the sweep's to take
	}
	return "a finished " + codec + " encode holdfast wrote, the length of the source beside it (" + filepath.Base(src) + ")"
}

// readableNow reports whether THIS PROCESS can actually read the bytes at path, right
// now, and returns the reason it cannot.
//
// It exists for one caller and one purpose: ffprobe reports "I read this path and it is
// not media" and "I could not open this path" with the SAME wire answer - a non-zero
// exit of its own accord - and only the first of those is evidence about the file. The
// second is evidence about this process's access to it, and docs/docker.md documents the
// commonest cause as an operator knob (`user:`), promising that getting it wrong is
// "safe but useless: every encode fails at the write step, and every source is left
// byte-for-byte intact". A restrictive umask, an NFS export that squashes the writing
// uid, an SELinux denial and a transient EIO on a failing disk all produce the same
// answer, and each would otherwise license deleting a gate-passed replacement that is
// present and byte-intact.
//
// It opens and READS, rather than stat-ing or checking a mode: a mode says what the
// kernel intends to allow, and the failures above are decided at open() and at the first
// read, by things a mode cannot see. A zero-length file reads io.EOF immediately and IS
// readable - a hard-killed encode leaves exactly that, and it is one the sweep must
// still take.
func readableNow(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var b [1]byte
	if _, err := f.Read(b[:]); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// couldThisBuildHaveWrittenIt reports whether codec is one ffprobe would report for an
// output SOME encoder this build ships could have produced.
//
// It is deliberately the whole registry and not any one job's target codec. A target
// codec is resolved per job, from the root profile's `encoder` and then from whatever an
// encode profile overrode it with (targetCodecFor), and a replacement stranded on disk
// was written by whichever encoder was configured for whichever root and pattern it fell
// under when it was written - so asking about a current key would make AC15i's
// protection turn on a setting that has nothing to do with the file, in exactly the way
// the criterion forbids it to turn on a record ("holding it SHALL NOT depend on one,
// since the write that failed is exactly what denied it"). Two layers of profile make
// that argument stronger, not weaker: two files in one run can be stranded by two
// different encoders, and there is now more than one current key to be wrong about.
//
// "h265" is ffprobe's legacy alias for hevc and is accepted for the same reason
// isAlreadyTargetCodec accepts it: the question is what the file IS.
func couldThisBuildHaveWrittenIt(codec string) bool {
	for _, target := range encoder.TargetCodecs() {
		if codec == target || (target == "hevc" && codec == "h265") {
			return true
		}
	}
	return false
}

// sourceBeside finds the source a stray temp was being encoded FROM: the sibling sharing
// its stem that carries a video extension. tempPath puts the temp in the source's own
// directory under the source's own stem, so this is that construction read backwards and
// not a search. The temp's own extension is tried first because it IS the source's
// whenever the output container matches the source (the default); the configured
// extensions cover a forced container_ext, where the two differ.
func (e *Engine) sourceBeside(dir, stem, tempExt string) (string, bool) {
	exts := make([]string, 0, len(e.Cfg.VideoExts)+1)
	exts = append(exts, tempExt)
	exts = append(exts, e.Cfg.VideoExts...)
	for _, ext := range exts {
		p := filepath.Join(dir, stem+"."+ext)
		if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() {
			return p, true
		}
	}
	return "", false
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
//
// abandon (UNDO-6) drops the undo window's retained original, and this function is
// where that decision belongs, because it is the same question: did the swap happen?
// A retention behind a swap that did NOT happen is an orphan raising the source's link
// count for nothing, and every other refusal in ProcessFile drops it. But a retention
// behind a swap that DID happen is the undo window doing its job, and a retention
// behind a swap nobody can adjudicate may be the ONLY remaining name for the original's
// bytes - discarding it there would delete the very thing this phase exists to protect.
// So it is called in exactly one branch, case (b), the one that ESTABLISHED the source
// untouched. Applied-despite-error and every indeterminate outcome HOLD it. It may be
// nil when there was no retention to take.
func (e *Engine) handleFailedSwap(ctx context.Context, f, key, tmp, final string,
	srcRec, replRec probe.Attributes, renameErr error, out *store.Outcome, abandon func()) {

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
		//
		// This is the ONE failed-swap branch that may drop the undo retention, and it may
		// only because the re-stat positively established the source is still at its own
		// path: the retained link therefore names bytes that are not the only copy. Every
		// other outcome below holds it.
		if abandon != nil {
			abandon()
		}
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
				// Say only what is true, and say what is now standing on what. The move
				// failing is the case where an operator most needs to know which
				// protection is left, because the write that just failed is very often
				// the same write that is about to deny the outcome record below.
				//
				// What holds the file then is strayReplacementHold: a file at a path THIS
				// BUILD'S TEMP CONSTRUCTION produced is never enumerated, encoded, swapped
				// or swept - with or without a record - unless the sweep can positively
				// establish it is work in progress, which takes a source beside it to
				// measure against AND a readable file AND a working ffprobe answering
				// about it. What that does NOT cover is a file at an ordinary media name,
				// which is reachable only when the rename took effect at a target whose
				// extension differs from the source's; the rename having taken effect is
				// itself evidence the directory accepts writes, so the move above is
				// expected to succeed in exactly that case.
				_, statErr := os.Lstat(at)
				held := "no file is there"
				if statErr == nil {
					held = "the file there is held back on its name and its content (a whole encode at a codec " +
						"this build's own encoders write, at a temp path this build constructed), with or without a record"
					if !IsTempConstructionName(filepath.Base(at)) {
						held = "the file there is at an ordinary media name, so ONLY the outcome record below can hold it back - " +
							"if that record cannot be written either, move or remove this file by hand"
					}
				}
				e.Log.Warn("could not move the replacement to a held-back name - "+held+
					"; the outcome record below names this path and the operator action reports what is (or is not) there",
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
