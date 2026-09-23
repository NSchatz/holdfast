// Package engine is the data-safety core of the transcoder — a faithful Go port of
// the bash orchestrator (media/transcoder/transcode.sh). Its whole purpose is the
// invariant the design defends: NEVER destroy a source until a replacement is
// proven good. The only filesystem mutation is an atomic same-directory rename that
// runs solely after the output passes every gate in verifyOutput; any failure
// discards the temp and leaves the source byte-for-byte untouched.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/cpuquota"
	"github.com/NSchatz/holdfast/internal/downscale"
	"github.com/NSchatz/holdfast/internal/fsclass"
	"github.com/NSchatz/holdfast/internal/hdr"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
	"github.com/NSchatz/holdfast/internal/vmaf"
)

// TempMarker is the fixed infix in a work-in-progress temp file's name, so a
// leftover temp from a killed run is always identifiable and discardable regardless
// of which file/pid produced it: `<name>.__transcoding__.<ext>`.
const TempMarker = "__transcoding__"

// The reasons a job reaches a terminal state, recorded on the row (TRANSCODE-13).
//
// The SKIP reasons are a closed, stable VOCABULARY, not prose: they are the answer to
// "which guard", which a UI cannot key off a sentence for, so treat them as a wire format
// - add to them freely, but renaming one changes what a stored row means. FAILURE reasons
// are NOT in this set, with one exception: a failure's reason is the error text itself,
// which is not drawn from a fixed set and where the detail is the whole value.
const (
	SkipAlreadyTargetCodec    = "already-at-target-codec"
	SkipLowBitrate            = "low-bitrate"
	SkipHardlinked            = "hardlinked"
	SkipInterlaced            = "interlaced"
	SkipDolbyVision           = "dolby-vision"
	SkipHDR10Plus             = "hdr10-plus"
	SkipIncompleteHDRMetadata = "incomplete-hdr-metadata"
	SkipExoticPixelFormat     = "exotic-pixel-format"
	SkipTargetExists          = "target-already-exists"
	SkipSymlink               = "symlinked-source"

	// SkipMultiVideoStream is the source-SHAPE guard: the source carries a video stream
	// beyond the first that is not an attached picture, or ffprobe could not establish what
	// its video streams are. Every property this pipeline derives is read from v:0 and the
	// VMAF gate compares v:0 against v:0, so a second moving-picture stream would be
	// re-encoded against a description of a DIFFERENT stream that no decision in front of
	// the swap ever inspected. An indeterminate probe answer skips under the same token
	// rather than encoding on a guess.
	SkipMultiVideoStream = "multi-video-stream"

	// SkipUnreadableStreamList is the source's FULL-shape guard: ffprobe could not
	// establish what streams the source carries at all, so the intended stream map this
	// job would be built from and checked against cannot be derived.
	//
	// It skips rather than encoding on a guess, for the reason every unknown in this
	// pipeline does: the map is what stands between a silently-dropped track and the
	// deletion of the source, and a map derived from a stream list nobody could read
	// would be a gate checking an answer it invented. Like the shape guard above it reads
	// no configuration key - what streams a file carries is a property of the file - so
	// its rows record nothing read and only a requeue (or a source that changed) revisits
	// them.
	SkipUnreadableStreamList = "unreadable-stream-list"

	// SkipUndoRetentionFailed is the undo window's own guard (UNDO-6): the original could
	// not be retained, so the swap that would have destroyed it does not run. It is a
	// MUTABLE guard, like the hardlink one, so ProcessFile clears a stale one before the
	// claim rather than parking the file for ever.
	SkipUndoRetentionFailed = "undo-retention-failed"

	// SkipUndeterminedSourceHeight is the SOURCE-HEIGHT guard: this file's configuration
	// cannot decide it without knowing how tall the source is, and the probe could not
	// establish that. Two configurations need it - a root carrying at least one rule whose
	// `when` selects on the source height, and a root or rule setting a `max_height` output
	// ceiling - and they share this token because they share a verdict and a remedy.
	//
	// It is the fail-safe rule applied to a height. With none there is no answer to which
	// rule applies, and no answer to whether this source is above the ceiling or below it;
	// every plausible fallback is a confident wrong result. Deciding the file under the
	// root's own profile ignores a band the operator wrote for it; reading an unreadable
	// height as 0 drops it into whichever band admits zero; encoding it against a default or
	// guessed height scales a file by a factor nobody measured and then hands the perceptual
	// gate a source resolution nobody measured either. Each judges a file against something
	// nobody chose, on a tool that deletes the source of every file it accepts. So the file
	// is decided under NO rule and under no ceiling at all, and the row says why.
	//
	// It READS the keys that needed the height - the rule list (InputRules) where the root
	// has rules, the ceiling (InputMaxHeight) where one is in force - which is what makes it
	// re-derivable: remove them and the next scan offers the file to the pipeline again.
	SkipUndeterminedSourceHeight = "undetermined-source-height"

	// SkipDownscaleUnacknowledged is the FINAL-SWAP guard, and it fires only where all three
	// of its conditions hold: this file would be scaled down by a configured `max_height`,
	// the undo window is disabled so the swap that replaces it is FINAL, and the governing
	// profile did not separately acknowledge that trade.
	//
	// The two keys are not one statement. `max_height` says what the replacement should look
	// like; `downscale_acknowledged` says the operator accepts that the original is not
	// coming back. With `undo_window_hours: 0` - the shipped default - those differ, because
	// the rename that publishes the replacement destroys the source and nothing retains it.
	// This build will not make an irreversible swap to a smaller picture on the strength of
	// one key, so it SKIPS: nothing is encoded, no temp is written, and the source is
	// byte-for-byte what it was.
	//
	// It is a skip and not a failure because nothing about the FILE is wrong. It reads both
	// keys it weighed (InputMaxHeight and InputUndoWindow), so either remedy - acknowledging
	// the trade, or opening the undo window so the swap can be walked back - offers every
	// held file straight back on the next scan.
	SkipDownscaleUnacknowledged = "downscale-unacknowledged"

	// SkipTelecineCadence is the CADENCE guard, and it fires only where a deinterlace was
	// configured: the source is telecined, or its cadence could not be established either
	// way, so the deinterlace that was asked for is not the right operation for it.
	//
	// Telecine is progressive film carried in an interlaced stream by repeating fields on a
	// 3:2 pattern. Deinterlacing one interpolates fields that were never a moving picture
	// and leaves judder that NO perceptual metric flags well - the frames it produces are
	// individually plausible, so VMAF scores them highly while the motion is visibly wrong,
	// and the swap then deletes the source. That is the worst outcome this pipeline has: a
	// wrong answer every gate agrees with. Undoing telecine is inverse telecine, a different
	// transformation with its own gates to argue, and it is not built here - so a telecined
	// source is left exactly as it is, whatever `deinterlace` is set to.
	//
	// An UNESTABLISHED cadence skips under the same token, for the reason every unknown in
	// this pipeline does: a detector that could not decide is not a licence to transform.
	// The two share a token because they share a remedy - this build leaves the file alone -
	// and the row's log line carries which of them it was.
	//
	// It reads the `deinterlace` key (it exists only because that key is on), so its rows
	// record that value and turning the key off re-derives them. `requeue --guard
	// telecine-cadence` is the lever for the rest: a re-encoded source, or a later build
	// that can tell the two apart where this one could not.
	SkipTelecineCadence = "telecine-cadence"

	// SkipUnknownFieldOrder is the FIELD-ORDER guard: ffprobe did not establish whether the
	// source is progressive or interlaced, so nothing in front of the encoder knows which it
	// is.
	//
	// It is the fail-safe rule applied to scan type. An unestablished field order used to
	// reach the encoder BY OMISSION - the interlace guard named the four interlaced
	// spellings and let everything else through - so an interlaced source whose container
	// never said so was re-encoded by a progressive-assuming pipeline, which bakes combing
	// in permanently, and the source was then deleted. That is a silent wrong result, which
	// is the one outcome this pipeline may never produce: an unknown is classified, not
	// assumed to be the common case.
	//
	// It reads no configuration key - what a file says about its own fields is a property of
	// the file - so its rows record nothing read and no configuration change re-derives one.
	// That is why the token is in SkipGuards: `requeue --guard unknown-field-order` is the
	// only lever, for a source that has since been remuxed or an ffprobe that can now read
	// what the one before it could not.
	SkipUnknownFieldOrder = "unknown-field-order"

	// SkipOperatorExcluded is the operator's OWN withholding: a path they took out of the
	// pipeline from the surface that showed them the file, recorded as runtime state this
	// daemon holds (store.PathExclusion) and never in the configuration file.
	//
	// It is a MUTABLE guard, like the hardlink one, and the mutation is the operator
	// removing the record: the stale row is cleared at the top of the next pass and the
	// file re-enters the ordinary path. That is what keeps a wrongly recorded withholding
	// from being a file that silently stops being worked on for ever, and it is why this
	// token is not in SkipGuards - there is nothing for a requeue to re-open, because
	// removing the withholding is the lever and the dashboard offers it beside the record.
	SkipOperatorExcluded = "operator-excluded"

	// SkipRestoredOriginal marks a file an operator has deliberately put back through the
	// undo window. It is NOT mutable: the next scan must not re-encode a file somebody just
	// rescued, through the very gates that passed the encode they rejected. Changing the
	// file clears it, because that is a new fingerprint and a new row; a configuration
	// change does not, and neither does a requeue. It is defined AS internal/store's
	// constant rather than beside it, because a wire format with two spellings has a silent
	// fork in it.
	SkipRestoredOriginal = store.GuardRestoredOriginal

	// FailUnreadable is the one failure whose reason is a token: there is no error to
	// quote, because the probe simply reported no video stream.
	FailUnreadable = "unreadable-or-no-video-stream"
)

// SkipVocabulary is the WHOLE skip vocabulary - every Skip* token declared above, in one
// place a consumer can enumerate at runtime.
//
// It is NOT SkipGuards and the two must not be confused. SkipGuards is the smaller set
// `requeue --guard` accepts, and it deliberately omits the MUTABLE guards (hardlinked,
// undo-retention-failed, operator-excluded) because a requeue has nothing to re-open for
// a verdict the next pass clears and re-derives by itself. Those guards fire in ordinary
// operation, so a consumer that has to account for every skip this engine can record -
// the /metrics label set is the standing one - reads THIS list. One built on SkipGuards
// would silently have no bucket for three live guards.
//
// Adding a Skip* constant means adding it here. The metrics surface asserts this list
// against the constants themselves (parsed out of this package), so a token added above
// and forgotten here reds that test rather than appearing unannounced at first use.
var SkipVocabulary = []string{
	SkipAlreadyTargetCodec,
	SkipLowBitrate,
	SkipHardlinked,
	SkipInterlaced,
	SkipDolbyVision,
	SkipHDR10Plus,
	SkipIncompleteHDRMetadata,
	SkipExoticPixelFormat,
	SkipTargetExists,
	SkipSymlink,
	SkipMultiVideoStream,
	SkipUnreadableStreamList,
	SkipUndoRetentionFailed,
	SkipUndeterminedSourceHeight,
	SkipDownscaleUnacknowledged,
	SkipUnknownFieldOrder,
	SkipTelecineCadence,
	SkipOperatorExcluded,
	SkipRestoredOriginal,
}

// mutableGuardSkips are the skip reasons that are a CONDITION rather than a verdict about
// the file: a seed that will finish, a retention area that will become writable, a
// withholding an operator will lift. Each parks its file behind a skipped row, and each is
// re-evaluated on every scan, so a row carrying one is cleared the moment its guard stops
// firing and the file re-enters the pipeline on THAT scan.
//
// They are handed to Claim, which already reads the status and the reason inside the one
// transaction that decides whether the file is claimable, so the clearing is a write only
// where there is a row to clear. A DELETE per reason per file per pass, which is what
// asking from outside that transaction costs, matches no row on any file of a fully
// processed library and is paid on every one of them for ever.
//
// It is a CLOSED list and what is NOT on it matters as much. A skip recording a verdict
// about the FILE - its codec, its bitrate, its shape, a height nobody could read - is a
// real outcome, and the decision-inputs rule is the only thing that re-opens one. A
// `restored-original` row is refused by that rule outright and must never appear here:
// re-opening it would feed an operator's rescued bytes back to the very gates that passed
// the encode they rejected.
var mutableGuardSkips = []string{SkipUndoRetentionFailed, SkipOperatorExcluded, SkipHardlinked}

// The GATE vocabulary: WHICH gate or stage refused a job that failed. Like the skip
// tokens above it is a closed, stable wire format - it is published as a metric label,
// and a renamed one silently breaks every dashboard built on it.
//
// It is decided AT THE SITE that rejected the job and travels out on the Event beside the
// failure class, never derived from the error text: the text is unbounded, and a
// classifier matching on strings drifts the moment a message is reworded. The class and
// the gate are orthogonal and both are kept - the class answers "will retrying help", the
// gate answers "what rejected this".
//
// Each member is a rejection an operator acts on DIFFERENTLY; that is the whole test for
// membership. The three VMAF floors are three members rather than one for exactly that
// reason: a rise in chroma-floor rejections says something about the encoder or the
// content changed, and folded into a single "vmaf" it would be indistinguishable.
const (
	// GateProbe: the source could not be read, or it carries no video stream.
	GateProbe = "probe"
	// GateEncode: the encoder itself failed, or produced no usable output file.
	GateEncode = "encode"
	// GateCodec: the output is not the codec this job was asked to produce.
	GateCodec = "codec"
	// GateLength: duration parity or packet-count parity failed - a truncated encode.
	GateLength = "length"
	// GateSize: the min-savings reject. An output that did not reclaim enough space.
	GateSize = "size"
	// GateStreamParity: the output does not carry the streams this job intended, or its
	// streams could not be enumerated to check.
	GateStreamParity = "stream-parity"
	// GateDecode: the decode-integrity healthcheck - the output does not fully decode.
	GateDecode = "decode"
	// GateVmafMean: the pooled harmonic mean fell below min_vmaf.
	GateVmafMean = "vmaf-mean"
	// GateVmafMin: the worst (sub)sampled frame fell below vmaf_min_pool - the encode is
	// locally broken however good its average is.
	GateVmafMin = "vmaf-min"
	// GateVmafChroma: the worst frame's chroma PSNR fell below vmaf_min_chroma - the
	// COLOUR planes were damaged, which the luma-only model cannot see.
	GateVmafChroma = "vmaf-chroma"
	// GateVmafUnmeasured: the perceptual gate could not MEASURE - libvmaf absent from the
	// build, the measurement failed, or no comparison pixel format could be named. An
	// unmeasured encode is refused, so this is a rejection like any other.
	GateVmafUnmeasured = "vmaf-unmeasured"
	// GateSwap: any refusal from the pre-swap steps onward - the collision re-check, the
	// copy back beside the source, the metadata carry, the fsync, the retention hook, the
	// re-fingerprint, the pre-swap attribute record, and the rename itself.
	GateSwap = "swap"
	// GateOther: the fallback, and a real member rather than an absence. A failure
	// attributable to no gate above is counted HERE, so the failures a consumer counts
	// always add up to the failures that happened.
	GateOther = "other"
)

// GateVocabulary is the whole gate vocabulary, in one place a consumer can enumerate.
// A member may be added; none may be renamed.
var GateVocabulary = []string{
	GateProbe,
	GateEncode,
	GateCodec,
	GateLength,
	GateSize,
	GateStreamParity,
	GateDecode,
	GateVmafMean,
	GateVmafMin,
	GateVmafChroma,
	GateVmafUnmeasured,
	GateSwap,
	GateOther,
}

// Engine drives the transcode over a set of library roots.
type Engine struct {
	Cfg   config.Config
	Probe *probe.Prober
	Enc   Encoder
	Store store.Store
	Log   *slog.Logger

	// roots are the library roots with their RESOLVED profiles, read from Cfg once in
	// New. A file's profile comes from the root it was enumerated under (rootFor), and
	// nested roots are refused at validate time, so exactly one root can ever contain a
	// path and the answer needs no precedence rule.
	roots []config.Root

	// vmafThreads is the CPU bandwidth the quality gates in this run share, read once in
	// New (see deriveVmafThreads). It is read once per run and not per file because the
	// quota does not move under a running process, and because a per-file reading would
	// put the same warn on every file of a library.
	vmafThreads vmafThreadPlan
	// vmafThreadsSaid guards the one announcement of that derivation. The workers reach
	// the gate concurrently, so the guard is a Once and not a flag.
	vmafThreadsSaid sync.Once
	// gateFlight counts the files this engine has claimed under a root with the perceptual
	// gate on and whose gates have not yet ruled, across EVERY pool that feeds ProcessFile -
	// the scan's workers, the submission queue's and the watch's alike. It is what a gate divides the quota by when more files are in
	// flight than the configured workers (see vmafThreadCount).
	gateFlight atomic.Int64
	// gateThreads is the account of the threads the gates scoring right now name, and the
	// bound that keeps their sum inside the quota (see takeGateThreads).
	gateThreads gateBudget

	// staticMetadataIncomplete, when non-nil, replaces hdr.StaticMetadataIncomplete for the
	// HDR10 static-metadata guard. Unexported test seam; production leaves it nil.
	staticMetadataIncomplete func(flatSideData string) bool

	// planObserver, when non-nil, receives the intended stream map at each of the two
	// points that read one: the encode the argv is built for, and the gate the output is
	// checked by. Unexported test seam (the engine tests are in this package), nil in
	// production.
	//
	// It exists for the one question no output file can answer: did the argv and the gate
	// read ONE derivation of the map? A test that merely runs a job and finds the output
	// acceptable passes just as well against two derivations that happen to agree on the
	// fixture, and the whole hazard is a pair that agrees on a fixture and disagrees on a
	// library. Announcing the plan's IDENTITY at both seams makes the question answerable,
	// and answerable NO.
	planObserver func(stage string, plan *StreamPlan)

	// vmafScore, when non-nil, replaces the real libvmaf measurement in the VMAF gate, so a
	// test can force a low score or an unavailable-libvmaf error without a second real
	// encode. It receives the whole vmaf.Request, comparison pixel format included, so a
	// test can assert on the format the gate NAMED as well as on the numbers.
	vmafScore func(ctx context.Context, req vmaf.Request) (vmaf.Result, error)

	// fsyncPath, when non-nil, replaces the real fsync in the durable swap (TRANSCODE-17).
	// A test uses it to observe or force-fail the fsync of the temp (before the rename) or
	// the parent directory (after it): the code discipline is testable even though
	// power-loss durability itself is not, which would need a power-cut harness.
	fsyncPath func(path string) error

	// hookAfterRename, when non-nil, is called immediately after a successful ext-CHANGING
	// rename (final != f) and BEFORE the now-orphaned source is removed, so a test can
	// simulate a crash in that exact window through the REAL swap path. A non-nil error
	// aborts before the delete exactly as a crashed process would, leaving BOTH files on
	// disk (a duplicate, never a loss) for the next scan to reconcile.
	hookAfterRename func() error

	// hookBeforeDryRunRecord, when non-nil, is called immediately before a dry run's
	// decision is recorded, carrying the path. It drives the one condition under which the
	// source size read can fail on a file that passed every guard - the file going away in
	// that window - proving the decision is recorded with the size NOT RECORDED.
	hookBeforeDryRunRecord func(path string)

	// hookAfterRetain, when non-nil, is called immediately after the undo window has taken
	// its second link to the source and BEFORE the re-fingerprint and the rename, so a test
	// can observe the state that exists only there: the retained original and the source
	// being one inode, and the filesystem having grown by nothing. A non-nil error aborts
	// the swap exactly as a failure there would.
	hookAfterRetain func(retained string) error

	// afterCopyBack, when non-nil, is called with the copy made beside the source
	// immediately after it has been made durable and BEFORE the identity re-check that
	// decides whether the swap may read it. It exists so a test can do the one thing no
	// fixture can arrange: put bytes that are NOT the accepted bytes at that path, and
	// prove the re-check catches it. A guard that cannot be shown to fail is not a guard.
	afterCopyBack func(copyPath string) error

	// freeBytes, when non-nil, replaces the free-space lookup the per-job scratch pre-check
	// makes. A CI runner cannot fill a filesystem on demand, and a check that could only be
	// proved by filling one would not be proved at all.
	freeBytes func(path string) (uint64, error)

	// undoNow, when non-nil, replaces the clock the undo window reads, so a test can place
	// a retention's expiry in the past and exercise the release sweep for real.
	undoNow func() time.Time

	// Observer, when non-nil, receives an Event on every job-state transition. It is a
	// fire-and-forget NOTIFICATION beside the store writes, never a substitute for them and
	// never on the critical path: emit calls it directly, so the Observer contract requires
	// it to be non-blocking and concurrency-safe.
	Observer Observer

	// Coverage, when non-nil, is the set of directories the STARTUP WALK traversed
	// successfully (FILESYSTEM-1), and it BOUNDS this run: a subtree the walk declined to
	// re-enter, could not read, or failed to traverse contributes no file and no swap can
	// happen under it. It is also what makes the scan terminate over a bind-mount loop,
	// which a plain recursive walk would follow for ever. nil walks the roots directly,
	// which is what an Engine built without the startup check gets.
	//
	// Set it with SetCoverage to carry the walk's own listings across with it; assigning the
	// field alone leaves the scan to list those directories itself.
	Coverage []string

	// carried is the entry information one startup walk collected, waiting for the FIRST
	// scan after that walk (see listings.go). RunOneshot takes it atomically, so a second
	// scan racing it gets nothing rather than a shared half-consumed map.
	carried atomic.Pointer[listings]

	// readDirFn, when non-nil, replaces the directory listing the coverage-bounded pass
	// makes. It is a seam in its own right because what a pass COSTS is a property nothing
	// else can observe: a test counts the listings a whole run issues, which is how "each
	// covered directory is listed once" is asserted rather than assumed.
	readDirFn func(dir string) ([]os.DirEntry, error)

	// Paused, when non-nil and returning true, tells scanOnce to stop feeding NEW files to
	// workers this pass. It is checked between files only: an in-flight encode is NEVER
	// interrupted, and paused work is left pending for the next scan after resume.
	Paused func() bool

	// --- the three FILESYSTEM-1 seams --------------------------------------------
	//
	// The gate has no network mount and no second real filesystem, and it never will: a test
	// that needed one would be skipped, which for a data-safety proof is a false green.
	// These substitutions are what make the failed-swap contract testable on local storage.

	// fsLookup, when non-nil, replaces the real filesystem-type lookup. It supplies a type
	// NAME only: the enumeration still decides the class, so a suite built entirely on
	// substituted lookups still reds against a build whose recognised-local set was emptied.
	fsLookup fsclass.Lookup

	// renameFn, when non-nil, replaces os.Rename in the swap. A test uses it to inject a
	// failure - optionally one that nonetheless APPLIES the rename, which is the shape
	// rename(2) describes for NFS and the single most important case here.
	renameFn func(oldpath, newpath string) error

	// restatFn, when non-nil, replaces the re-stat that follows a failed rename. It is a
	// seam in its own right because one scenario cannot be produced by the other two: a
	// rename that genuinely took effect whose re-stat returns the SOURCE's pre-swap
	// attributes, as a client attribute cache populated before the swap would.
	restatFn func(path string) (probe.Attributes, error)

	// statFn, when non-nil, replaces the attribute read the pre-claim path takes of a
	// source. Two things need it and neither has another route.
	//
	// What a pass COSTS is the first: a case counts the reads through this seam, which is a
	// counter the code exposes rather than a stopwatch - on a warm page cache elapsed time
	// cannot tell one read from three, so timing would grade nothing.
	//
	// The read FAILURES are the second. A file that went away between the enumeration and
	// the claim is a race no fixture can hold open, and a path this process may not stat is
	// not producible under the unprivileged uid the gate runs as; both are conditions the
	// scan must survive, so both are injected here.
	statFn func(path string) (os.FileInfo, error)

	// --- the four S0085 metadata seams ---------------------------------------------
	//
	// The swap carries the SOURCE's mode, ownership and modification time onto the
	// replacement (see metadata.go). The gate runs as an unprivileged uid with NO CAP_CHOWN,
	// so a real chown(2) to another uid is not exercisable here: what the swap REQUESTS is
	// the observable and what the kernel answers is the injection. A process that owns a
	// file can always chmod and utimes it, so those failure paths have to be injected to
	// exist at all, and the metadata read has to fail while the source is STILL THERE, so
	// the fixture can assert it byte-for-byte intact afterwards.

	// statMetadataFn, when non-nil, replaces the read of the SOURCE's metadata.
	statMetadataFn func(path string) (fileMetadata, error)

	// chownFn, when non-nil, replaces os.Chown on the replacement.
	chownFn func(path string, uid, gid int) error

	// chmodFn, when non-nil, replaces os.Chmod on the replacement.
	chmodFn func(path string, mode fs.FileMode) error

	// chtimesFn, when non-nil, replaces the modification-time write on the replacement.
	// It takes the mtime alone: the access time is deliberately left unchanged.
	chtimesFn func(path string, mtime time.Time) error

	// ownershipNoticeGiven is the once-per-RUN latch behind the unpreserved-ownership
	// report; RunOneshot clears it before anything is swapped.
	ownershipNoticeGiven atomic.Bool

	// held is the current run's hold-back snapshot, published once by RunOneshot before any
	// worker starts and only read afterwards. An atomic pointer rather than a plain field so
	// a second RunOneshot can never be observed mid-write.
	held atomic.Pointer[holdBacks]

	// terminal counts the terminal ledger rows this ENGINE has recorded: one per file
	// carried to a state the ledger records as final, whether that is a swap, a failure, a
	// skip with a reason or a swap that did not complete cleanly. Every writer of such a
	// row raises it through countTerminalRow, which is where the enumeration of them lives
	// and is the only place this moves, so a file that was enumerated, or that was turned
	// away at the claim by a row it already had, raises nothing - which is what makes
	// `--limit` a count of decisions rather than of directory entries (S0100). It only ever
	// grows; a bounded pass reads the difference across itself rather than the absolute
	// figure (see budget).
	terminal atomic.Int64

	// passes counts the scan passes IN FLIGHT, raised by RunOneshot just after it publishes
	// this pass's snapshot and lowered when the pass returns. It answers one question: is
	// there a scan whose snapshot a decision taken now must agree with? See holdBacksInForce,
	// its only reader.
	passes atomic.Int64

	// onClaim, when non-nil, is called with the WORKER and the path the instant ProcessFile
	// takes the claim on it - the ONE signal that says a caller got PAST the door rather
	// than being turned away at it. The targeted-submission queue (submit.go) reads it to
	// report "this file is held out by a terminal row" without keeping a second copy of the
	// store's re-opening rule, which is the rule that decides it.
	//
	// The worker is carried because the hook is ENGINE-wide: a scan's workers call it too,
	// and a note keyed on the path alone cannot say WHOSE claim it was, so a reader would
	// attribute one caller's claim to another.
	//
	// Set ONCE, before serving, exactly as Observer is; it is then read from every worker
	// goroutine and never written again. It runs inline on a worker, so it must be
	// non-blocking and concurrency-safe - the same contract Observer carries.
	onClaim func(worker, path string)
}

// EnsureHoldBacks publishes a snapshot when none has been published yet: the
// once-per-process PARKED report (loadHoldBacks writes it as it builds one) for a daemon
// that may never take a pass, and a non-nil snapshot for the pass-scoped readers. It never
// REPLACES a live one, because a pass reads its own end to end.
//
// IT IS NOT THE GATE. A snapshot published once is frozen, and a hold-back recorded after
// it was taken is not in it. What decides whether a file may be processed is
// holdBacksInForce, asked on the door per file, and that is where the freshness lives.
func (e *Engine) EnsureHoldBacks(ctx context.Context) {
	if e.held.Load() == nil {
		e.held.CompareAndSwap(nil, e.loadHoldBacks(ctx))
	}
}

// holdBacksInForce returns the record-based hold-backs a file being processed AT THIS
// MOMENT must be judged against. It is the door's question, and it has two answers because
// there are two situations to be consistent with.
//
// WHILE A PASS IS IN FLIGHT, that pass's snapshot: it is published before the pass walks
// anything and read end to end, so a file arriving mid-pass by any route reaches the verdict
// that pass's own worker reaches on the file beside it. Re-reading here would make a
// submission stricter than the scan it must agree with, and REPLACING the snapshot would
// change what a scan already in progress holds back.
//
// WHILE NONE IS, the store. The last snapshot anybody published is then as old as whatever
// published it, and with scan_interval_sec: 0 no further pass happens at all - so a swap
// that parks an incident after startup records two paths a submission would walk straight
// past for the life of the process. This read is the one a scan starting now would make,
// and it reads QUIETLY: the PARKED report is owed once per pass, not once per file.
func (e *Engine) holdBacksInForce(ctx context.Context) *holdBacks {
	if e.passes.Load() > 0 {
		return e.held.Load()
	}
	return e.readHoldBacks(ctx)
}

// rename performs the swap's rename, routing through the test seam when one is set.
func (e *Engine) rename(oldpath, newpath string) error {
	if e.renameFn != nil {
		return e.renameFn(oldpath, newpath)
	}
	return os.Rename(oldpath, newpath)
}

// restat reads a path's rename-invariant attributes after a failed swap, routing
// through the test seam when one is set.
func (e *Engine) restat(path string) (probe.Attributes, error) {
	if e.restatFn != nil {
		return e.restatFn(path)
	}
	return probe.StatAttributes(path)
}

// stat reads a source's attributes, routing through the test seam when one is set. EVERY
// pre-claim read of a source goes through here, which is what makes "one read per file"
// a property a case can count rather than a claim about a profile.
//
// It is os.Stat and it FOLLOWS a symbolic link, exactly as the three reads it replaced
// did. That is the half of this that cannot move: the fingerprint derived from it is the
// key every terminal row is held out by, and a read that resolved a symlinked source to
// the link instead of its target would re-key every one of those rows, offering files that
// were already done back to a pipeline that deletes sources. The symlink guard is the
// other half and it is the one call that must NOT follow the link (probe.IsSymlink, via
// Lstat); it runs after the claim, where it always has.
func (e *Engine) stat(path string) (os.FileInfo, error) {
	if e.statFn != nil {
		return e.statFn(path)
	}
	return os.Stat(path)
}

// heldBack reports whether a path is one of the PUBLISHED snapshot's two record-based
// hold-backs, and why. It is deliberately separate from the record-free name check, so a
// caller that needs both asks for both and it is always obvious which rule fired.
//
// Its callers are the pass-scoped ones - the enumeration, the temp sweep, the two temp-path
// constructions - which run inside a pass, or beside strayReplacementHold's own live
// per-path question. The DOOR uses holdBacksInForce instead.
func (e *Engine) heldBack(p string) (string, bool) { return e.held.Load().held(p) }

// emit delivers ev to the Observer if one is set. It must stay cheap and non-blocking: it
// runs inline on a worker goroutine, so the Observer owns any buffering.
func (e *Engine) emit(ev Event) {
	if e.Observer != nil {
		e.Observer(ev)
	}
}

// progressEmitInterval throttles live progress reporting to at most one event per job per
// interval (S0030). ffmpeg's default -stats_period is 0.5s and every event costs the
// reporting hub a full store-derived snapshot rebuild, so an unthrottled two-hour film would
// drive thousands of them on the machine doing the encoding. Dropping an update is
// granularity lost, never correctness lost.
const progressEmitInterval = time.Second

// encode runs the configured Encoder over one file UNDER prof - the profile of the root the
// file was enumerated under - collecting live progress when there is both an Encoder that
// can report it and an Observer to receive it. Anything else falls through to Encode: a job
// whose progress is never reported is a state the reporting surface already handles.
//
// The profile is applied FIRST and to the Encoder itself, through ProfileEncoder, so what
// runs is an encoder built from that root's knobs rather than one told about them
// afterwards. An Encoder that is not profile-aware encodes from what it was constructed
// with, exactly as before.
func (e *Engine) encode(ctx context.Context, worker, in, out string, props *probe.VideoProps, prof config.Profile, plan *StreamPlan) error {
	enc := e.Enc
	if pe, ok := enc.(ProfileEncoder); ok {
		enc = pe.ForProfile(prof)
	}
	if se, ok := enc.(StreamPlanEncoder); ok {
		// THE SEAM. The plan the argv is built from is handed over HERE and announced
		// here, and the gate announces the plan it checked against at its own seam; a test
		// holding both can ask whether they are ONE derivation, which is a question that
		// can be answered NO - and would be, if anything on either side ever derived a map
		// of its own (see SameDerivation).
		e.observePlan(planStageEncode, plan)
		enc = se.ForStreamPlan(plan)
	}
	pe, ok := enc.(ProgressEncoder)
	if !ok || e.Observer == nil {
		return enc.Encode(ctx, in, out, props)
	}
	return pe.EncodeWithProgress(ctx, in, out, props, e.progressSink(worker, in, sourceDuration(props)))
}

// sourceDuration is the length a live progress figure is measured against, taken from
// the snapshot ProcessFile ALREADY probed (TRANSCODE-PERF) so establishing it costs no
// additional subprocess. nil means UNKNOWN and is the honest answer for a container that
// reports no duration; a non-positive value is treated as unknown too, because dividing
// by it would fabricate a fraction rather than measure one.
func sourceDuration(props *probe.VideoProps) *float64 {
	if props == nil {
		return nil
	}
	d, ok := props.DurationSec()
	if !ok || !(d > 0) {
		return nil
	}
	return &d
}

// progressSink builds the per-job ProgressSink handed to a ProgressEncoder. It is called
// only from the single goroutine draining that job's progress pipe, so the throttle state
// needs no lock; and all it does is emit, which the Observer contract already requires to
// be non-blocking — the reporting path must never be able to slow an encode.
func (e *Engine) progressSink(worker, path string, dur *float64) ProgressSink {
	var last time.Time
	return func(p Progress) {
		now := time.Now()
		if !last.IsZero() && now.Sub(last) < progressEmitInterval {
			return
		}
		last = now
		p.DurationSec = dur
		e.emit(Event{Path: path, Status: store.Encoding, Worker: worker, Progress: &p})
	}
}

// fsync flushes path (a regular file OR a directory) to durable storage, routing
// through the test seam when one is set. It is the core of TRANSCODE-17's power-loss
// durability discipline (see the "Durability" comment in ProcessFile).
func (e *Engine) fsync(path string) error {
	if e.fsyncPath != nil {
		return e.fsyncPath(path)
	}
	return fsyncPath(path)
}

// fsyncPath opens path and fsyncs it. For a regular file this forces its data blocks to
// disk; for a DIRECTORY it forces the name entries, which is what makes a rename() into (or
// a remove() from) that directory survive a power loss rather than merely a clean crash:
// os.Rename is atomic against a concurrent reader, but POSIX does not make it PERSISTENT
// until the containing directory is fsync'd. On Linux an O_RDONLY fd flushes both, so a
// read-only open suffices. The Sync error is the one that matters.
func fsyncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// New constructs an Engine. All dependencies are injected so tests can supply a
// deterministic Encoder while using the real Prober/Store over real fixtures.
func New(cfg config.Config, p *probe.Prober, enc Encoder, st store.Store, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	return &Engine{
		Cfg: cfg, Probe: p, Enc: enc, Store: st, Log: log, roots: cfg.RootProfiles(),
		// Derived here and announced at the gate, not here: `plan` and `analyze` build an
		// Engine to enumerate and never score one file, and a scoring thread count stated
		// on their output is a line about work that is not going to happen.
		vmafThreads: deriveVmafThreads(cpuquota.DefaultRoot, cfg.EffectiveWorkers()),
	}
}

// rootFor answers the one question every decision below depends on: which library root was
// this file enumerated under, and therefore which profile decides it. It is asked ONCE per
// file, at the top of ProcessFile, and threaded through the rest of it, so a file cannot be
// guarded by one root's bitrate floor and encoded at another root's crf.
//
// Nested roots are refused by config.Validate, so at most one root can contain a path and
// this needs no longest-prefix rule. A path under NO configured root falls back to the
// top-level profile and records no root rather than inventing one.
func (e *Engine) rootFor(path string) (config.Root, bool) {
	for _, r := range e.roots {
		if r.Contains(path) {
			return r, true
		}
	}
	return config.Root{Profile: e.Cfg.TopLevelProfile()}, false
}

// effectiveProfile is WHICH PROFILE DECIDES ONE FILE: the root's own resolved profile with
// the first matching resolution rule laid over it.
//
// It returns the snapshot it took, so the caller can hand the SAME one to the guard chain -
// a file under a banded root therefore pays for one ffprobe, not two, exactly as it did
// before rules existed. A root that needs no source height is not probed here at all and the
// returned snapshot is nil: the common case, and every configuration written before this
// item.
//
// established is FALSE when this configuration needs the source's height and the probe could
// not read it. The caller must then decide the file under nothing at all (see
// SkipUndeterminedSourceHeight); the profile it gets back is the root's own, which is what
// the claim and the row are attributed to, and no rule has been applied to it.
//
// TWO CONFIGURATIONS NEED THE HEIGHT and the answer is the same for both. A root whose rules
// band on it needs it to pick a rule; a root or rule setting a `max_height` ceiling needs it
// to know whether this source is above the ceiling and by how much. The second is asked
// AFTER the rules are applied where it can be, because a rule may be what set the ceiling -
// so a list of unbounded rules is resolved first, at a height nothing reads, and only then is
// the resolved ceiling weighed.
//
// It is resolved BEFORE the claim on purpose. The claim compares what this configuration
// offers for this file against what the row recorded, and a row that recorded a rule's floor
// while the claim offered the root's would be re-opened on every scan for ever - which on a
// library is a re-encode of everything, and every accepted encode deletes its source.
func (e *Engine) effectiveProfile(ctx context.Context, f string, root config.Root,
	snapshot func(context.Context, string) *probe.VideoProps) (config.Profile, *probe.VideoProps, bool) {
	prof := root.Profile
	switch {
	case len(prof.Rules) == 0:
		if !prof.DownscaleEnabled() {
			return prof, nil, true
		}
	case !prof.Rules.NeedsSourceHeight():
		// Every rule matches every file, so the first one decides and no height is read for
		// THAT. The height argument is unused in that case and 0 is passed rather than
		// probed - but the rule the first match supplied may itself carry a ceiling, so the
		// resolved profile is what is asked about one.
		resolved := prof.WithRules(0)
		if !resolved.DownscaleEnabled() {
			return resolved, nil, true
		}
	}
	props := snapshot(ctx, f)
	_, height, ok := props.Dimensions()
	if !ok {
		return prof, props, false
	}
	return prof.WithRules(height), props, true
}

// reuse hands the SAME probe snapshot back to the guard chain that the rule resolution
// already paid for, so a banded root costs no extra ffprobe. A nil snapshot means none was
// taken and the chain takes its own.
func reuse(taken *probe.VideoProps,
	fallback func(context.Context, string) *probe.VideoProps) func(context.Context, string) *probe.VideoProps {
	if taken == nil {
		return fallback
	}
	return func(context.Context, string) *probe.VideoProps { return taken }
}

// decidedBy is what a terminal row records about the profile that judged the file: the
// cleaned root, and a digest of that root's resolved knobs. A file under no configured
// root records NO root - the digest still describes the profile that actually decided
// it, and a fabricated root would be worse than an absent one.
func decidedBy(root config.Root, known bool) store.Decision {
	d := store.Decision{ProfileDigest: root.Profile.Digest()}
	if known {
		d.LibraryRoot = root.Clean
	}
	return d
}

// RunOneshot discards orphaned temps from any prior killed run, resets any job left active
// by a prior crashed run back to pending, then scans every library root once and fans the
// discovered files out to a pool of workers. A cancelled ctx stops workers picking up new
// files and kills the in-flight encode's subprocess; the temp it was writing is orphaned but
// never swapped in, and cleanStaleTemps sweeps it on the next startup.
func (e *Engine) RunOneshot(ctx context.Context) error { return e.runPass(ctx, Bound{}) }

// runPass is the oneshot pass, unbounded or bounded, in one body. A bounded run is a
// SMALLER run and not a lighter one, so the sequence below is the same sequence in the
// same order and the bound changes exactly three things, each of them named in bounded.go:
// what is offered, the two whole-library passes that are not run, and the record that says
// so.
func (e *Engine) runPass(ctx context.Context, b Bound) error {
	// FILESYSTEM-1: read this run's hold-backs FIRST and publish them before anything
	// walks a root. Every parked job is reported here, naming both of its files, and
	// its two recorded paths - plus every recorded replacement path still carrying a
	// live exclusion - are withheld from the sweep, from the scan and from the workers.
	e.held.Store(e.loadHoldBacks(ctx))

	// IN FLIGHT from here - after the snapshot above is published, never before - until this
	// returns, so every route into ProcessFile meanwhile is judged against that snapshot
	// rather than a fresh read (holdBacksInForce).
	e.passes.Add(1)
	defer e.passes.Add(-1)

	// S0085: the unpreserved-ownership notice is owed once per RUN, so a daemon scanning
	// every scan_interval_sec says it again on each pass.
	e.ownershipNoticeGiven.Store(false)

	// What this pass is bounded by and what that costs, said out loud BEFORE any of it
	// happens: the two passes below that will not run are the operator-visible difference
	// between this and an ordinary scan (S0100 AC-11).
	if b.bounded() {
		e.reportBound(b)
	}

	if _, err := e.Store.RecoverStale(ctx); err != nil {
		// Fail safe: a stuck "active" row means one file is skipped this pass (Claim treats
		// it as held), never a false completion, so log and continue.
		e.Log.Warn("recover stale jobs failed (continuing)", "err", err)
	}
	// The undo window closes here (UNDO-6), at the START of the pass and before anything is
	// encoded, so the figure an operator sees covers the whole pass and the disk this run is
	// about to write to has had back whatever the last one held.
	//
	// It is deliberately NOT gated on undo_window_hours still being non-zero. Each retention
	// carries the expiry it was GIVEN, and setting the key back to 0 is the documented way
	// to stop paying for the window, so gating the sweep on it would strand every original
	// already retained: the second link on disk for ever, the space never returned. The
	// setting governs whether a NEW retention is taken, nothing else.
	e.undo().ReleaseExpired(ctx)

	// A single named file is carried straight to the pipeline's own door: nothing is
	// listed, so nothing below needs this pass's listings (see processOne).
	if b.File != "" {
		return e.processOne(ctx, b.File)
	}

	// This pass's listings, taken here and used by the sweep and the enumeration between
	// them, so every covered directory is listed exactly once for the whole pass: the
	// startup walk's own where this is the first scan after that walk, this scan's where not.
	pass := e.passListings()

	// THE TWO WHOLE-LIBRARY PASSES, and a bounded run runs neither (S0100 AC-10). Both
	// reason from "this pass looked at the whole library": the sweep decides a file
	// holdfast wrote is orphaned, and the retention pass decides a row's file is gone. A
	// partial pass has no evidence for either, and the retention pass's mistake is an
	// irreversible delete of audit history that no re-run restores.
	if !b.bounded() {
		e.sweepStaleTemps(ctx, pass)
		// The configured working location is swept here and not by sweepStaleTemps, and
		// it is the one sweep that does NOT read this pass's listings: the scratch
		// directory is not under a library root, so it is outside the coverage bound
		// those listings are taken over and nothing in `pass` can describe it. It lists
		// itself, once, under the same construction and the same hold-back exceptions -
		// see cleanScratch.
		e.cleanScratch(ctx)
	}
	bud := e.budgetFor(b)
	observed, err := e.scanOnce(ctx, pass, bud)
	if err != nil {
		return err
	}
	if b.bounded() {
		bud.reportReached()
		return nil
	}
	e.enforceRetention(ctx, observed)
	return nil
}

// enforceRetention brings the ledger back within Cfg.HistoryRetentionRows, and is the ONLY
// caller of the prune, which is what makes the bound hold with no operator action.
//
// It runs AFTER the scan, never during it: a prune competes for the single serialized store
// connection the engine writes every Claim/Advance/Finish through, and it also means "the
// oldest beyond the retention" is evaluated against a complete picture.
//
// A failure here is LOGGED and survivable, deliberately: a store that cannot be pruned must
// not stop the daemon serving or the next scan encoding, and the rows it did not remove are
// still there. observed is what THIS scan actually listed, and decides which rows may go
// (see rowIsSpent).
func (e *Engine) enforceRetention(ctx context.Context, observed map[string]bool) {
	if !e.Cfg.RetentionEnabled() {
		// Retention disabled (the shipped default): every row is kept and the store is
		// not so much as read. Growth stays visible through the metrics that already
		// exist - holdfast_queue_depth{state} is read from the store on every scrape.
		return
	}
	var stillInLibrary, notObserved int64
	prunable := e.rowIsSpent(observed, &stillInLibrary, &notObserved)
	p, err := e.Store.PruneTerminal(ctx, e.Cfg.HistoryRetentionRows, e.Cfg.MaxFailures, prunable)
	if err != nil {
		e.Log.Warn("ledger retention pass failed (rows it did not remove are untouched; serving and encoding continue)",
			"err", err, "history_retention_rows", e.Cfg.HistoryRetentionRows,
			"removed", p.Removed, "reclaimed_carried_bytes", p.ReclaimedCarried)
		return
	}
	if p.Removed == 0 && p.Kept == 0 {
		return
	}
	e.Log.Info("ledger retention enforced (an irreversible delete of audit history)",
		"history_retention_rows", e.Cfg.HistoryRetentionRows,
		"removed", p.Removed, "reclaimed_carried_bytes", p.ReclaimedCarried, "kept", p.Kept)
	if p.Kept > 0 {
		e.Log.Info("ledger is above the retention by design: these rows are what holds their files out of "+
			"the encoder, and deleting one would hand that file back to it on the next scan",
			"kept_rows", p.Kept, "kept_file_still_in_the_library", stillInLibrary,
			"kept_directory_this_run_did_not_list", notObserved, "max_failures", e.Cfg.MaxFailures)
	}
}

// rowIsSpent answers store.Prunable: may this terminal row be removed without changing what
// the engine does with that file?
//
// A row is SPENT only when this run LOOKED where the file should be and it was not there.
//
//   - Looked. A row is only ever spent if THIS run listed its directory (observed). The
//     shape that matters is the nested mount that is down, where the roots list fine and a
//     whole subtree is simply absent: pruning on that absence and meeting the files again
//     when the mount returns is a library-wide re-encode. A missing or unlistable ROOT
//     refuses the run outright (FILESYSTEM-1).
//   - Not there. Absent, or present under a fingerprint that is not this row's, which is
//     the superseded row the swap already deletes by hand. Claim is keyed on
//     path+fingerprint, so such a row holds nothing out of the encoder. Any other stat
//     failure is not an absence and is answered like an unlisted directory: keep.
//
// A library that is not churning has one row per file and every one of them is holding that
// file out of the encoder, so its ledger is bounded by the library rather than by
// history_retention_rows (README, "Bounding the ledger").
func (e *Engine) rowIsSpent(observed map[string]bool, stillInLibrary, notObserved *int64) store.Prunable {
	return func(path, fingerprint string, _ store.Status) bool {
		if !observed[filepath.Dir(path)] {
			*notObserved++
			return false
		}
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return true
			}
			*notObserved++
			return false
		}
		if probe.Fingerprint(path) == fingerprint {
			*stillInLibrary++
			return false
		}
		return true
	}
}

// ReleaseExpired releases every retained original whose undo window has closed and
// reports what that returned. RunOneshot calls it at the start of every pass; it is
// exported so the release is drivable on its own, which is what makes "the space the
// release actually returned" an assertable figure rather than a log line.
func (e *Engine) ReleaseExpired(ctx context.Context) ReleaseReport {
	return e.undo().ReleaseExpired(ctx)
}

// Restore puts a retained original back at its own path. It is the engine-side entry
// point `holdfast restore` drives; see UndoWindow.Restore.
func (e *Engine) Restore(ctx context.Context, path string) (RestoreResult, error) {
	return e.undo().Restore(ctx, path)
}

// cleanStaleTemps deletes `*.__transcoding__.*` files left under the roots by a prior killed
// run: the swap is the only mutation, so a half-written encode is worth nothing.
//
// It sweeps WORK IN PROGRESS and nothing else. Three kinds of file it must never take:
//
//   - a RETAINED replacement (`*.__holdfast-replacement__.*`), which passed every gate and
//     may be the only faithful copy of a source whose fate is unknown. Its different marker
//     puts it out of this sweep's reach by construction.
//   - a temp path a live record names as a job's replacement, which is what a failed move
//     to a retained name leaves.
//   - a temp path holding a FINISHED replacement with no record at all, which is what a
//     library that went read-only leaves behind: the same failure denies the swap, the move
//     to the retained name and the incident write alike (strayReplacementHold).
func (e *Engine) cleanStaleTemps(ctx context.Context) { e.sweepStaleTemps(ctx, e.passListings()) }

// sweepStaleTemps is the sweep as a pass runs it: over the listings that pass already has,
// and listing only what it does not. It reads them and does not release them - the
// enumeration wants the same entries next, and neither must ask the filesystem twice.
func (e *Engine) sweepStaleTemps(ctx context.Context, pass *listings) {
	n := 0
	if e.Coverage != nil {
		// Bounded by the startup walk exactly as the scan is: a directory the
		// walk did not traverse is one this run touches in no way at all.
		for _, dir := range e.Coverage {
			if ctx.Err() != nil {
				break
			}
			got := e.listIn(pass, dir)
			if got.err != nil {
				continue
			}
			for _, ent := range got.entries {
				// Per ENTRY, not per directory: deciding a file's fate costs probes that a
				// cancelled context kills, so a SIGTERM landing in this loop would leave
				// every remaining file judged by questions nothing can answer.
				if ctx.Err() != nil {
					break
				}
				// The kind the LISTING reported, links not followed: a symbolic link is a
				// name to remove here, never a directory to step into.
				if !ent.IsDir && isTempName(ent.Name) && e.sweepTemp(ctx, filepath.Join(dir, ent.Name)) {
					n++
				}
			}
		}
		if n > 0 {
			e.Log.Info("discarded orphaned temp file(s) from a prior run", "count", n)
		}
		return
	}
	for _, root := range e.Cfg.LibraryRoots {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable entry — skip, never abort the sweep
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if d.IsDir() || !isTempName(filepath.Base(path)) {
				return nil
			}
			if e.sweepTemp(ctx, path) {
				n++
			}
			return nil
		})
	}
	if n > 0 {
		e.Log.Info("discarded orphaned temp file(s) from a prior run", "count", n)
	}
}

// sweepTemp discards ONE work-in-progress temp, and reports whether it did. It is the single
// place the sweep's two independent exceptions live, so the coverage-bounded branch and the
// walking branch cannot disagree about them: a temp path a live RECORD names as a job's
// replacement (AC15d), and a temp path holding a finished replacement that no record
// survived to name (AC15i, strayReplacementHold). The second is asked even when the first
// says nothing, because the case it exists for is the one where the store write that would
// have made the record is what failed.
func (e *Engine) sweepTemp(ctx context.Context, path string) bool {
	if why, ok := e.heldBack(path); ok {
		e.Log.Warn("leaving a file holdfast wrote in place (not an orphaned temp)", "file", path, "why", why)
		return false
	}
	if why := e.strayReplacementHold(ctx, path); why != "" {
		e.Log.Warn("leaving a file holdfast wrote in place - NO RECORD of it survives, so it is held back on its name and its content alone: "+
			"it is never enumerated, encoded, swapped or swept, in this run or any later one, and removing it is an operator's call",
			"file", path, "why", why)
		return false
	}
	return os.Remove(path) == nil
}

// scanOnce walks the roots and fans what it finds out to a pool of workers
// (Cfg.EffectiveWorkers(), minimum 1) AS IT FINDS IT. A cancelled ctx stops workers pulling
// new files and kills the in-flight ffmpeg subprocess through exec.CommandContext.
//
// The enumeration and the feed are the same loop, run on THIS goroutine: the workers start
// first, and each source reaches one the moment its directory's listing returns, rather than
// after the last directory in the library has been read. So the first encode no longer waits
// for the last readdir, and what the scan holds is one directory's listing plus whatever is
// in flight rather than every path in the library at once.
//
// It returns the set of directories this scan LISTED SUCCESSFULLY - the only places this run
// has evidence about, and therefore the only places the retention pass may read a missing
// file as a file that is gone (see rowIsSpent). It is the enumeration's own record rather
// than a re-derivation, so the two cannot disagree about where holdfast looked. Streaming
// does not weaken that: the map is written by this goroutine alone, it is returned only once
// the enumeration has finished with it, and a directory enters it only after a listing of it
// RETURNED - so a pass cut short mid-stream reports the directories it did list and no more.
// bud, when non-nil, is this pass's bound on how many files may reach a terminal outcome.
// It is asked before each file is offered and released as each returns, so the pass stops
// OFFERING at the bound rather than stopping work that is already under way.
func (e *Engine) scanOnce(ctx context.Context, pass *listings, bud *budget) (map[string]bool, error) {
	n := e.Cfg.EffectiveWorkers()
	ch := make(chan string)
	var wg sync.WaitGroup
	// firstCancelErr captures the first context-cancellation error surfaced by any worker.
	// It is kept explicit, rather than left to the `return ctx.Err()` fallback below, so a
	// child-context deadline inside ProcessFile is surfaced rather than masked by a
	// not-yet-done parent ctx.
	var mu sync.Mutex
	var firstCancelErr error

	for i := 0; i < n; i++ {
		workerID := "w" + strconv.Itoa(i)
		wg.Add(1)
		go func(workerID string) {
			defer wg.Done()
			for f := range ch {
				// The release is deferred inside this closure rather than written after
				// the call, so every way out of one file - a cancellation, an error, an
				// ordinary return - gives the bound its slot back exactly once.
				done := func() bool {
					defer bud.release()
					if ctx.Err() != nil {
						return true
					}
					if err := e.ProcessFile(ctx, workerID, f); err != nil {
						if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
							mu.Lock()
							if firstCancelErr == nil {
								firstCancelErr = err
							}
							mu.Unlock()
							return true
						}
						// A per-file error is logged and recorded inside ProcessFile and
						// never takes down the scan; a non-context error here is unexpected.
						e.Log.Warn("process file error (continuing)", "file", f, "err", err)
					}
					return false
				}()
				if done {
					return
				}
			}
		}(workerID)
	}

	// Pause control: stop handing out NEW files the moment we are paused, and say so ONCE.
	// Asked before each send and before each directory is listed, so a pause lands inside a
	// stretch of directories holding no source at all just as it lands between two files.
	// Pause only ever DELAYS work and never touches an in-flight encode; the files this pass
	// never handed out stay pending for the next scan after resume, and the directories it
	// never reached are simply not listed, so they are not reported as observed either.
	said := false
	stop := func() bool {
		if ctx.Err() != nil {
			return true
		}
		if e.Paused != nil && e.Paused() {
			if !said {
				said = true
				e.Log.Info("paused - stopping the feed of new files; in-flight encodes finish safely")
			}
			return true
		}
		return false
	}

	// What the feed may decline to spend a worker on: a candidate whose ledger row Claim
	// would refuse anyway (S0095 AC-9). Built once per pass, asked per candidate, and it
	// decides NOTHING about the file - see feedHoldOut.
	holdOut := e.newFeedHoldOut()

	observed := e.enumerateOrdered(pass, sink{
		offer: func(f string) bool {
			if stop() {
				return false
			}
			// Asked BEFORE the bound takes a slot: a row this feed declines to spend a
			// worker on is one no decision is reached about, so it must not consume one of
			// the decisions a bounded pass was told to make.
			if holdOut.declines(ctx, f) {
				return true
			}
			// The bound is asked HERE, where a file would be handed out, and it takes a
			// slot before the send: a file in flight is a terminal outcome this pass has
			// already committed to, so counting only what is RECORDED would let a pool of
			// workers overshoot the bound by up to one file each. admit blocks rather
			// than refusing while every slot is merely in flight - see budget.
			if !bud.admit() {
				return false
			}
			select {
			case <-ctx.Done():
				bud.release()
				return false
			case ch <- f:
				return true
			}
		},
		stopped: func() bool { return stop() || bud.met() },
	})
	holdOut.report()
	close(ch)
	wg.Wait()

	if firstCancelErr != nil {
		return observed, firstCancelErr
	}
	return observed, ctx.Err()
}

// sink is where a streamed enumeration puts what it finds, and the only thing that can stop
// it short.
//
// Both halves are called on the ENUMERATION's own goroutine, in the order the enumeration
// reaches them, so neither needs a lock and the hand-out order simply IS the order offer was
// called in. Returning false from either stops the enumeration where it stands: nothing
// further is listed, so nothing further is reported as observed.
type sink struct {
	// offer takes one source path this run may act on, in hand-out order, and reports
	// whether the enumeration should carry on.
	offer func(path string) bool

	// stopped is asked before each directory is listed, so a scan cancelled or paused while
	// it is crossing a run of directories that hold no source at all stops there rather than
	// listing the rest of the library for nothing. nil is never stopped.
	stopped func() bool
}

// halted reports whether this sink has asked the enumeration to stop.
func (s sink) halted() bool { return s.stopped != nil && s.stopped() }

// collect is a sink that gathers every path into files and never stops the enumeration. It is
// how a caller that wants the whole list - the read-only plan pass - drives the same
// enumeration the scan streams.
func collect(files *[]string) sink {
	return sink{offer: func(p string) bool { *files = append(*files, p); return true }}
}

// enumerate returns every source this run may act on, in hand-out order, and the set of
// directories it LISTED SUCCESSFULLY to find them.
//
// With a Coverage set (FILESYSTEM-1) it lists exactly the directories the startup walk
// traversed successfully and nothing else: no recursion of its own, so a directory the walk
// declined yields no file here and a bind-mount loop the walk cut cannot be followed.
// Without one it walks the roots directly.
//
// Two hold-backs are applied here and they are the only two the swap half creates.
// IsSourceName carries the RECORD-FREE one: a retained replacement is a file holdfast wrote
// and is never anybody's source, whether or not a record of it survived. offered() carries
// the RECORD-based one: a parked job's two recorded paths, and any recorded replacement path
// whose disposition still excludes it.
//
// The observed set is the SAME evidence in its other form: a directory is in it only when a
// listing of that directory returned and that listing is the one this scan drew its sources
// from. A missing, unreadable or declined directory is absent, which the retention pass
// reads as "no evidence" rather than "the files are gone". A covered directory that listed
// EMPTY is in it: holdfast looked and found nothing, which is not the same as never looking.
func (e *Engine) enumerate() ([]string, map[string]bool) { return e.enumerateIn(e.passListings()) }

// enumerateIn is the enumeration as a pass runs it, collected into a slice. It is what a
// caller that wants the whole answer at once drives - the read-only plan pass, and the cases
// that assert what a given tree enumerates - and it is the SAME traversal the scan streams,
// in the same hand-out order, so a plan and the scan it predicts cannot disagree about
// either the set or the sequence.
//
// The scan itself does NOT come through here: it calls enumerateStream directly, so nothing
// on the path a daemon takes ever holds the library's paths at once.
func (e *Engine) enumerateIn(pass *listings) ([]string, map[string]bool) {
	var files []string
	observed := e.enumerateOrdered(pass, collect(&files))
	return files, observed
}

// enumerateStream is the enumeration as a pass runs it, over the listings that pass
// holds: the startup walk's where this is the first scan after that walk, the
// sweep's where the sweep already listed for this one, and its own otherwise. It
// RELEASES each directory's entries as it consumes them, so the entry
// information a walk collected does not outlive the scan that used it.
//
// It hands each source to the sink AS IT FINDS IT and keeps nothing: what this holds is one
// directory's listing plus the observed map, and neither grows with the number of FILES in
// the library. See docs/enumeration.md for the order it hands them out in and for what
// a later declared queue order may build on it.
func (e *Engine) enumerateStream(pass *listings, to sink) map[string]bool {
	// filtered counts the files the configured path filters kept out of this scan. It
	// counts FILES and never directories, because the filters are applied to an entry
	// this scan has ALREADY LISTED: what a filter changes is the set of files offered,
	// never the set of directories listed, and the observed map below is the evidence
	// that difference rests on (AC-10).
	var filtered int
	observed := map[string]bool{}
	if e.Coverage != nil {
	covered:
		for _, dir := range e.Coverage {
			// Asked BEFORE the listing, so a cancelled or paused scan stops here with this
			// directory unlisted - and therefore unobserved - rather than paying for a
			// listing whose files it has already decided not to hand out.
			if to.halted() {
				break covered
			}
			// The retention area holds this tool's own retained originals and nothing
			// else. Its files are already excluded by name (IsSourceName); skipping the
			// directory too means no route at all feeds rescued bytes back to the encoder.
			if IsRetentionDir(dir) {
				continue
			}
			got, ok := pass.take(dir)
			if !ok {
				ents, err := e.listDir(dir)
				got = listed{entries: ents, err: err}
				pass.selfListed++
			}
			if got.err != nil {
				e.reportUnlistedDirectory(dir, got.err)
				continue
			}
			// Marked observed HERE, and this is the only statement on this branch that
			// writes the map: physically downstream of a listing that RETURNED, so no
			// interruption below can leave a directory in the set that this run did not
			// list. Over-reporting is the one failure this enumeration cannot be allowed
			// to have - the retention pass reads a file missing from an observed directory
			// as a file that is gone, and expires the undo record that is the only route
			// back to the original bytes.
			observed[dir] = true
			// The hand-out order within one directory is entry-NAME order, imposed here
			// rather than inherited from whatever produced the listing.
			sortEntriesByName(got.entries)
			for _, ent := range got.entries {
				source := IsSourceName(ent.Name, e.Cfg.VideoExts)
				// The kind the listing reported, and - where anything established
				// it - what following the entry reaches. A directory under a source
				// name is not a source by either spelling.
				if ent.IsDir || ent.ResolvesToDir {
					if source {
						e.skipSourceNamedDirectory(filepath.Join(dir, ent.Name))
					}
					continue
				}
				if source {
					p := filepath.Join(dir, ent.Name)
					// The filter is asked AFTER this directory was listed and marked
					// observed, and it is asked about the FILE. That ordering is the
					// whole of AC-10: an excluded directory is still listed, so the
					// retention pass has exactly the evidence it had before.
					if !e.filterAllows(p) {
						filtered++
						continue
					}
					// offered() is the record-based hold-back and it is asked HERE, on
					// the last step before a path leaves the enumeration, exactly where
					// it was asked when this loop filled a slice instead.
					if e.offered(p) && !to.offer(p) {
						break covered
					}
				}
			}
		}
		e.reportFiltered(filtered)
		if pass.selfListed > 0 {
			// Entry information that was never collected is never evidence: where
			// none was carried in, this scan listed for itself and says so.
			e.Log.Info("this scan listed covered directories itself; the startup walk's own listings serve the first scan after that walk and no later one",
				"directories_this_scan_listed", pass.selfListed, "directories_covered", len(e.Coverage))
		}
		return observed
	}
	for _, root := range e.Cfg.LibraryRoots {
		if to.halted() {
			break
		}
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				// WalkDir reports a directory it could not read by calling back a SECOND
				// time for it, carrying the error. Withdraw what was marked on the way in:
				// a listing that failed is not one this run may conclude from. The
				// withdrawal happens before any entry under that directory could be
				// reached - there are none, the listing is what failed - so the set this
				// returns never names a directory whose listing did not return.
				if d != nil && d.IsDir() {
					delete(observed, path)
					e.reportUnlistedDirectory(path, err)
				}
				return nil
			}
			if d.IsDir() {
				if to.halted() {
					// Stopped on the way IN, before this directory is marked: a scan that
					// stopped here did not list it and must not claim to have.
					return fs.SkipAll
				}
				// The retention area is never a source, and is skipped BEFORE being marked
				// observed: a directory this run declined to list is not evidence about
				// what is in it.
				if IsRetentionDir(path) {
					return filepath.SkipDir
				}
				observed[path] = true
				if IsSourceName(d.Name(), e.Cfg.VideoExts) {
					e.skipSourceNamedDirectory(path)
				}
				return nil
			}
			if IsSourceName(filepath.Base(path), e.Cfg.VideoExts) {
				// A walk does not follow links, so a symbolic link to a DIRECTORY arrives
				// here looking exactly like a file. This is the branch that has to ask:
				// the other is told by the startup walk, which followed the link already.
				if d.Type()&fs.ModeSymlink != 0 && isDirectory(path) {
					e.skipSourceNamedDirectory(path)
					return nil
				}
				// Asked here and not on the way into the directory, for the reason the
				// covered branch above gives: a filter changes which FILES are offered
				// and never which directories were listed (AC-10).
				if !e.filterAllows(path) {
					filtered++
					return nil
				}
				if e.offered(path) && !to.offer(path) {
					return fs.SkipAll
				}
			}
			return nil
		})
	}
	e.reportFiltered(filtered)
	return observed
}

// reportUnlistedDirectory states a directory this run could not list: WHICH directory, WHAT
// was tried, and WHAT HAPPENS NEXT (observability O4). Before this the coverage branch
// dropped a failed listing in silence, which left the one fact an operator needs - that a
// region of the library was not looked at this pass - visible nowhere at all.
//
// It is `warn` and not `error` (observability O3): the scan continues over every other
// directory it can list, no human has to act for the rest of the pass to finish, and a
// library with one unreadable directory in it would otherwise raise an alarm on every scan
// for ever. What it costs is stated rather than implied - the directory is left OUT of the
// observed set, so the retention pass reads it as "no evidence" and not as "the files under
// it are gone", which is the fail-safe direction.
func (e *Engine) reportUnlistedDirectory(dir string, err error) {
	e.Log.Warn("a directory could not be listed; this scan continues without it",
		"directory", dir,
		"operation", "list the directory",
		"err", errText(err),
		"next", "no file under it is enumerated this pass and it is NOT reported as observed, so the "+
			"ledger retention pass draws no conclusion from a file that is missing under it")
}

// filterAllows reports whether the path filters in force for this path's library root
// let this run offer it, and says which root excluded it when they do not.
//
// It decides from the PATH: nothing is opened, stat'd or listed here, which is what lets
// it be asked about an entry the scan has already listed without touching what that
// listing recorded. A path under no configured root is left alone - the enumeration
// reaches none, and a filter is a statement about a root's own tree.
//
// The per-file line is DEBUG and the count below is INFO, deliberately. One line per
// excluded file is the right detail when an operator is asking why a particular file was
// not touched, and the wrong volume for a run over an excluded subtree of ten thousand
// files - which is the configuration this feature exists for.
func (e *Engine) filterAllows(p string) bool {
	root, known := e.rootFor(p)
	if !known || !root.Filters.InForce() || root.Offers(p) {
		return true
	}
	e.Log.Debug("not enumerating (a configured path filter excludes it)",
		"file", p, "library_root", root.Clean,
		"exclude_paths", root.Filters.Exclude, "include_paths", root.Filters.Include)
	return false
}

// reportFiltered states what the path filters kept out of one enumeration, and states
// beside it the thing an operator cannot see and the ledger depends on: every directory
// was still listed, so the retention pass has the same evidence it would have had with
// no filter configured.
func (e *Engine) reportFiltered(filtered int) {
	if filtered == 0 {
		return
	}
	e.Log.Info("path filters kept files out of this scan; every directory this scan would "+
		"have listed was still listed, so the ledger retention pass reads exactly the evidence it "+
		"read before the filters were configured",
		"files_excluded_by_a_path_filter", filtered)
}

// offered reports whether a path this run found may be OFFERED to the pipeline, and
// logs the reason when it may not. It is the record-based hold-back at the one place
// enumeration decides what to hand a worker.
func (e *Engine) offered(path string) bool {
	if why, ok := e.heldBack(path); ok {
		e.Log.Info("not enumerating (held back)", "file", path, "why", why)
		return false
	}
	return true
}

// ProcessFile applies the full safety pipeline to one source file on behalf of worker. It
// returns context.Canceled/DeadlineExceeded if interrupted (source untouched, temp
// discarded); for every other outcome it records the result in the store and returns nil,
// because a single bad file must never abort the scan.
//
// The store.Claim call is the mutual-exclusion guard: the ONLY thing standing between two
// workers, or two overlapping runs, both encoding the same source. The hardlink guard runs
// before Claim but its RecordSkip is a report-only write that never claims the file, so it
// cannot let two workers encode one source.
//
// ONE ATTRIBUTE READ reaches that claim, and everything before it is derived from that one
// read: whether there is a file at the other end of this path at all, the source's
// pre-encode size, the fingerprint the claim is keyed by, and the hard-link count. It is
// os.Stat and it FOLLOWS a symbolic link, which is not a detail - the fingerprint is the
// key that holds a terminal row out of a pipeline that deletes sources, and every stored
// row for a symlinked source is keyed by its TARGET's attributes. The symlink guard is the
// one read that must not follow the link, and it is an Lstat after the claim, where it has
// always been.
func (e *Engine) ProcessFile(ctx context.Context, worker, f string) error {
	// Both of the questions this door asks about the PATH itself, answered by the one
	// function the read-only plan pass and the census ask too (DeclinedPath), so a refusal
	// added there reaches the daemon and every report that predicts it with no second edit.
	// The character rule is still said out loud below the hold-backs, as it always was.
	//
	// The door hands back the stat it took, and that is the whole consolidation: every
	// question below about this file's NUMBERS is answered from it rather than by asking
	// the filesystem again. Two reads of one path are also two answers about a file that
	// can change between them.
	door := declinedPath(f, e.stat)
	if door.statErr != nil {
		// Nothing is at the other end of this path, or this process may not look: a
		// dangling link, a file that went away between the enumeration and here, a
		// directory it cannot traverse. The file is left ALONE - no row, no mutation - and
		// the scan carries on over the rest of the library. info rather than warn or
		// error: the condition is handled here and completely, the next pass re-reads the
		// path with no operator action in between, and a library with a dangling link in
		// it would otherwise raise an alarm on every scan for ever.
		e.Log.Info("skip (this file's attributes could not be read, so it is left untouched "+
			"and re-read on the next scan)", "file", f, "err", door.statErr)
		return nil
	}
	if door.declined && door.rule != RuleUnsupportedCharacters {
		return nil
	}

	// The source's PRE-ENCODE size, which the terminal row records and the undo window
	// measures its retention by, off the door's own read.
	fi := door.fi
	if fi == nil {
		// The path carries a tab or a newline, so the door answered on the name alone and
		// took no stat. The number below still has to come from somewhere, and a path this
		// build refuses to key a row on is the one case where a second read costs nothing.
		var err error
		if fi, err = e.stat(f); err != nil {
			return nil
		}
	}

	// Hold-backs, re-checked here rather than trusted to the scan: ProcessFile is exported
	// and is the only door into the encode/swap pipeline, so the rule belongs on the door.
	// The record-based half asks for the hold-backs IN FORCE, never a snapshot published
	// earlier: a recorded replacement path is not always a name this build recognises (see
	// swap.go's applied-despite-error branch and retainReplacement), so for those files the
	// record is the only thing holding them.
	if IsRetainedReplacementName(filepath.Base(f)) {
		return nil
	}
	if why, ok := e.holdBacksInForce(ctx).held(f); ok {
		e.Log.Info("not processing (held back)", "file", f, "why", why)
		return nil
	}

	// The half of that answer this daemon says out loud, in the place it has always said it.
	if door.declined {
		e.Log.Info("skip (path contains a tab/newline — unsupported)", "file", f)
		return nil
	}

	// WHICH PROFILE DECIDES THIS FILE, resolved ONCE, here, from the root it was enumerated
	// under, and threaded through everything below: the hardlink guard, the bitrate floor,
	// the pixel-format derivation, the output container, the encode's argv and every gate in
	// verifyOutput. Re-deriving it at each use would let a file be guarded by one root's
	// floor and encoded at another root's crf.
	//
	// The root's own profile is only half of it now. A root may carry RESOLUTION RULES, an
	// ordered first-match list of per-band overrides, and the profile that decides this file
	// is the root's with the first matching rule laid over it. That resolution reads the
	// source height where a band needs one, and the snapshot it takes is handed on to the
	// guard chain below rather than taken twice.
	root, rooted := e.rootFor(f)
	prof, pre, heightKnown := e.effectiveProfile(ctx, f, root, e.Probe.VideoProps)
	by := decidedBy(root, rooted)

	// The key this file's row is stored under, derived from the read already taken. It is
	// the SAME two fields rendered the same way probe.Fingerprint renders them, off the
	// same followed-link stat, because every row already in the field is keyed that way and
	// a key that moved would offer files that were already done back to the pipeline.
	key := probe.AttributesOf(fi).String()

	// THIS JOB's effective ENCODE settings, resolved once, here, from that root's profile
	// and the source path: the root's own values overlaid with the first matching encode
	// profile's overrides, plus the name of the profile that supplied them. Every decision
	// below about what the encoder PRODUCES reads this rather than prof - the
	// already-at-target-codec skip, the output container, the pixel-format guard, the
	// output-codec acceptance check, the encode's argv and the encoder on the row.
	//
	// prof stays the authority for everything that decides whether the source may be
	// destroyed: the hardlink guard, the bitrate floor, the savings floor and the VMAF
	// floors all read it, and no encode profile can carry any of them.
	//
	// Resolved BEFORE the first terminal outcome so every one of them, skip and failure
	// alike, can record which profile chose this job's settings.
	ts := e.Cfg.TranscodeIn(prof, f)
	targetCodec := targetCodecFor(ts.Encoder)

	// The operator's own withholding, and it runs FIRST among the guards for a reason:
	// every guard below it is this build deciding something about the file, and this one is
	// a person deciding it. A row naming a guard that ran after the withholding would
	// report a verdict nothing reached.
	//
	// It is read here, per file, rather than off a snapshot taken when the run began, so a
	// path withheld while a scan is under way is withheld for the rest of it - which is
	// exactly the case an operator watching the dashboard creates.
	//
	// A store error fails SAFE IN THE WITHHOLDING DIRECTION: a store that cannot say
	// whether an operator withheld this path is not one this pass may hand the file to, so
	// the file is left for the next scan. That is the same shape as the claim error below
	// and the opposite trade from the record-based hold-backs, deliberately - those fall
	// back on a name and on Claim, and this one has nothing behind it.
	withheld, err := e.Store.PathIsExcluded(ctx, f)
	if err != nil {
		e.Log.Warn("cannot read the withheld paths (skipping this pass, will retry)", "file", f, "err", err)
		return nil
	}
	if withheld {
		e.Log.Info("skip (an operator withheld this path from the pipeline)", "file", f)
		changed, err := e.Store.RecordSkip(ctx, f, key, SkipOperatorExcluded, by, ts.Profile)
		if err != nil {
			// Fail safe: recording the skip is the reporting half, never the decision, so
			// a store hiccup still withholds the file.
			e.Log.Warn("record withheld skip failed (still withholding the file)", "file", f, "err", err)
		} else if changed {
			// A terminal row was newly recorded here, so this file is one of the decisions
			// a bounded run counts (see countTerminalRow).
			e.countTerminalRow(store.Skipped)
			// Emitted only when it was newly recorded, so a live client sees it once and
			// not once per scan for as long as the withholding stands.
			e.emit(Event{Path: f, Status: store.Skipped, Outcome: e.because(SkipOperatorExcluded, by, prof, ts, pre)})
		}
		return nil
	}

	// THE BAND GUARD, and it stands in front of every guard that reads a knob a rule may
	// supply. This root selects on the source height and the probe could not establish one,
	// so there is no answer to which rule applies - and the fail-safe rule says a file this
	// build cannot place is decided under NOTHING rather than under a plausible guess. It
	// runs after the operator's own withholding, which outranks every verdict this build
	// reaches, and before the profile decides anything at all.
	if !heightKnown {
		e.recordUndeterminedHeight(ctx, worker, f, key, root, by, prof, ts, pre)
		return nil
	}

	// Hardlink guard. A file with >1 hard link is almost always an *arr import that is also
	// an active seed, and replacing it via rename breaks the link, reclaiming no space and
	// silently breaking the seed. The skip is RECORDED as "hardlinked" so an operator sees
	// WHICH guard fired, and the link count is MUTABLE: RecordSkip only writes where no real
	// outcome exists, and reaching the claim below without firing is what drops the stale
	// row once the seed finishes (mutableGuardSkips).
	//
	// The count comes out of the read this file already cost, and NOT following the link
	// count to a fresh stat is what the fail-safe rests on: a stat record this build cannot
	// read the count out of answers 1, so an unreadable count never trips the guard.
	//
	// A link THIS TOOL holds is discounted (UNDO-6): the undo window takes a second link
	// before the rename, so a run interrupted there would otherwise park the very file the
	// window was protecting. Discounting is proved per link (same inode, live retention
	// record), never assumed from the count, so a foreign extra link still skips.
	if prof.HardlinkSkip() {
		if links := probe.NLinkOf(fi); links > 1 && links > 1+e.retainedLinks(ctx, f, key) {
			e.Log.Info("skip (hardlinked — swap would break a seed and reclaim nothing)", "file", f, "links", links)
			changed, err := e.Store.RecordSkip(ctx, f, key, SkipHardlinked, by, ts.Profile)
			if err != nil {
				// Fail safe: recording the skip is a reporting nicety, never the decision,
				// so a store hiccup still skips the file.
				e.Log.Warn("record hardlink skip failed (still skipping the file)", "file", f, "err", err)
			} else if changed {
				// A terminal row was newly recorded here too (see countTerminalRow).
				e.countTerminalRow(store.Skipped)
				// Emit only when the skip was newly recorded, so a live client sees it once
				// rather than once per scan for the lifetime of the seed.
				e.emit(Event{Path: f, Status: store.Skipped, Outcome: e.because(SkipHardlinked, by, prof, ts, pre)})
			}
			return nil
		}
	}

	// Claim: the resume short-circuit, the cross-worker mutual-exclusion guard AND the
	// mutable guards' re-evaluation, in one atomic call. done/skipped hold the file out for
	// as long as the decision inputs they recorded still match what is handed in here and
	// are RE-OPENED when they do not; failed is retryable up to MaxFailures, since a
	// transient ENOSPC must not exclude a file for ever; active means another worker holds
	// it, or it is stale and awaits RecoverStale.
	//
	// mutableGuardSkips is the third of those. Reaching this line is every mutable guard
	// above saying its condition no longer holds, and the claim already reads the status
	// and the reason inside the transaction that decides claimability - so a row parked by
	// one of them is cleared there, where the answer is known, instead of by a DELETE per
	// guard per file per pass that matches nothing on every file of a processed library.
	// What it decides is unchanged; what it costs is a write only where there is a row.
	claimed, err := e.Store.Claim(ctx, f, key, worker, e.Cfg.MaxFailures,
		e.inputsFor(prof, ts), mutableGuardSkips...)
	if err != nil {
		// Fail safe: a store error must never be treated as "done", and it must never be
		// treated as a verdict about the file either. Nothing about the file is touched,
		// no row is written for it, and it stays eligible: the next scan asks again. The
		// record names the dependency, what was being attempted and what happens next,
		// because a trace alone does not tell an operator whether the file is lost.
		e.Log.Warn("the store could not take the claim, so this file is left untouched this pass "+
			"and retried on the next scan", "dependency", "store", "attempted", "claim",
			"next", "retry on the next scan", "file", f, "err", err)
		return nil
	}
	if !claimed {
		return nil
	}
	if e.onClaim != nil {
		e.onClaim(worker, f)
	}
	// From the claim until its gates have ruled, a file under a root with the perceptual
	// gate on is one the gate may be asked to score, so every gate that starts meanwhile
	// divides the quota with it in mind. Every way out of this function lets go of it; the
	// ordinary path lets go as soon as the gates return, because the swap that follows
	// scores nothing.
	leaveGateFlight := func() {}
	if prof.VmafGate() {
		leaveGateFlight = e.enterGateFlight()
	}
	defer leaveGateFlight()
	// The claim moved this row to probing: surface it as a live "started" signal carrying
	// the worker, so the UI shows the file entering the pipeline immediately.
	e.emit(Event{Path: f, Status: store.Probing, Worker: worker})

	// Every source-side guard, in one call, off one probe snapshot and writing nothing.
	// What each of them decides is unchanged; what changed is that the read-only plan pass
	// asks the same chain rather than carrying a second copy of it (see guardSource).
	//
	// The snapshot is the one the rule resolution already took where it took one, so a
	// banded root costs the same single ffprobe an unbanded one does.
	props, v := e.guardSource(ctx, f, root, prof, ts, targetCodec, reuse(pre, e.Probe.VideoProps))
	if v.stopped() {
		e.Log.Info(v.log, append([]any{"file", f}, v.logArgs...)...)
		out := e.because(v.guard, by, prof, ts, props, v.inputs...)
		if v.failed {
			// The one source-side verdict that FAILS rather than skips: the probe reported
			// no video stream, so the gate that refused this job is the probe.
			e.fail(ctx, f, key, GateProbe, out)
			return nil
		}
		e.finish(ctx, f, key, store.Skipped, out)
		return nil
	}

	// THE FIELD-DOUBLING REFUSAL, taken before anything is encoded and before a temp path
	// is even chosen, so the source is byte-for-byte what it was and there is nothing to
	// discard. A configuration that would emit one frame per FIELD produces an output whose
	// frame count is twice the source's, and packet-count parity and duration parity are
	// graded against the source's count - so encoding it would mean either failing a parity
	// gate after paying for a full encode, or weakening the gate that stands in front of a
	// deletion. Neither is acceptable, and the refusal is recorded rather than logged: a row
	// is what an operator reads after the fact.
	//
	// config.Validate refuses the same configuration at START and names the key, so this is
	// the backstop rather than the operator-facing message - the same shape the exotic
	// pixel-format guard and the encoder's own derivation backstop already have. It is
	// reachable by a Config assembled in Go, which is how this package's own callers build
	// one, and it is what makes the refusal a property of the ENGINE rather than of whoever
	// remembered to validate.
	if _, err := deinterlaceFor(prof); err != nil {
		e.Log.Error("FAIL (the deinterlace configured for this root would change the output's frame "+
			"count, so nothing was encoded and the source is untouched)", "file", f,
			"library_root", root.Clean, "err", err)
		e.fail(ctx, f, key, GateEncode, withSourceDimensions(&store.Outcome{
			Reason:  err.Error(),
			Profile: ts.Profile,
			// TRANSIENT, and deliberately: the verdict is a pure function of the
			// configuration, but the remedy is an edit to that configuration and a retry
			// costs NOTHING here - no encode runs, no gate runs, no byte is written. Parking
			// the file would hold it by an attempt count that the operator's fix does not
			// clear, so correcting the key would leave the file exactly where it was and the
			// only way out would be `requeue --failed`.
			FailureClass:   store.FailureTransient,
			Decision:       by,
			DecisionInputs: e.inputsRead(prof, ts, InputDeinterlace),
		}, props))
		return nil
	}

	codec := v.codec
	outExt := v.outExt
	// final is where the swap publishes, chosen by the same guard chain that just refused
	// to clobber anything already there.
	final := v.target

	dir := filepath.Dir(f)
	base := filepath.Base(f)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	// tmp is local to this call (and thus to this worker) — TRANSCODE-5 runs N
	// workers concurrently, each on a different source. Correctness does not depend on
	// tracking it in the Engine: every failure path below removes tmp directly, and a
	// ctx-cancel leaves it orphaned for cleanStaleTemps to sweep on the next startup. The
	// path is the build's own construction (see swap.go).

	if e.Cfg.DryRun {
		// A DRY RUN'S DECISION IS RECORDED: the file passed every guard, so a run with
		// dry_run off would transcode it, which is the question an operator turns dry_run on
		// to answer. Nothing is encoded, swapped or deleted - this returns before the temp
		// path is even chosen - and the row leaves `probing`, where a claim left it, so the
		// figures that say what the run CONCLUDED no longer report nothing.
		//
		// The size is a FRESH stat of the file that was decided, and a stat that fails
		// records NOTHING: nil is "not recorded", and a fabricated size would put a number
		// into the one figure an operator uses to size the job.
		if e.hookBeforeDryRunRecord != nil {
			e.hookBeforeDryRunRecord(f)
		}
		// The decision inputs a dry run records are the ones the ENCODE it is predicting
		// would have been taken under. They change nothing about whether the row is
		// re-claimed - a dry-run decision always is - so they are for the operator reading
		// it, not for the engine.
		//
		// The profile travels on this row like it travels on every other terminal one. A dry
		// run answers "what would a real run do with this file", and with encode_profiles
		// configured the honest answer names the profile whose settings that run would have
		// used. "" says the top-level settings would have run, which is the true answer and
		// not a missing measurement.
		//
		// The target path is on the ROW and not only in the line below it, because the name
		// the swap would publish under is a fact about this file that an operator cannot
		// re-derive from the row alone: container_ext is layered per root and per encode
		// profile. It is recorded on the row that reaches no swap, so it never says a file is
		// there - the four facts together are what a run WOULD do, and none of them is a
		// measurement of work nobody did.
		out := withSourceDimensions(&store.Outcome{
			SourceCodec:    codec,
			TargetPath:     final,
			Profile:        ts.Profile,
			Decision:       by,
			DecisionInputs: e.inputsRead(prof, ts, InputTargetCodec, InputEncoder, InputCRF, InputPreset),
		}, props)
		if st, err := os.Stat(f); err == nil {
			out.SourceBytes = ptr(st.Size())
		} else {
			e.Log.Warn("dry-run decision: source size could not be read (recorded as not recorded)", "file", f, "err", err)
		}
		e.Log.Info("DRY_RUN would transcode", "file", f, "codec", codec,
			"source_bytes", logSize(out.SourceBytes), "target", final)
		e.finish(ctx, f, key, store.WouldTranscode, out)
		return nil
	}

	// THE INTENDED STREAM MAP, derived ONCE, here, and threaded into both the encode and
	// the gate. It is what the argv is built from and what the output is checked against,
	// and it is one value rather than two derivations because two derivations would be two
	// answers to whether a track was lost - and that answer decides whether the source is
	// deleted.
	//
	// The source's stream list is fetched HERE and not earlier. It is a second ffprobe, and
	// every guard above this line can skip a file; a list fetched at the top of the pass
	// would be paid for by every enumerated file, including the ones that never reach an
	// encoder (see probe.VideoProps.AllStreams, which is lazy and memoised for exactly
	// this reason). A dry run returns above without paying for it at all: it encodes
	// nothing, so it drops nothing.
	sourceStreams, enumerated := props.AllStreams()
	if !enumerated {
		// AC-9's source half, and the fail-safe rule this whole pipeline keeps: an unknown
		// shape is never read as the common one. Nothing is encoded and the source is
		// untouched.
		e.Log.Info("skip (ffprobe could not enumerate the source's streams - "+
			"refusing to encode without knowing which streams the output must carry)", "file", f)
		e.finish(ctx, f, key, store.Skipped, e.because(SkipUnreadableStreamList, by, prof, ts, props))
		return nil
	}
	plan := DeriveStreamPlan(sourceStreams, prof, codec)
	if plan.AudioSelectionNotApplied() {
		// The run CONTINUES, in a state the operator did not configure: warn, which in this
		// fleet means exactly that. The row records the same fact durably, because a log
		// line is not evidence about a file whose source is about to be deleted.
		e.Log.Warn("the audio selection was NOT applied: applying it would have left this file "+
			"with no audio at all, so every audio stream is carried forward instead",
			"file", f, "why", SelectionNotAppliedNoAudio,
			"audio_languages", strings.Join(prof.AudioLanguageCodes(), ","),
			"keep_commentary", prof.CommentaryKept())
	}

	// WHERE THE ENCODER WRITES. With no scratch_dir configured this is a temp beside the
	// source and the pipeline below is byte for byte the one this repository has always run:
	// one file, encoded in place, verified in place, renamed onto the source. With a
	// scratch_dir configured the encoder writes THERE instead, and nothing appears under the
	// source's directory until the acceptance gates have accepted the encode - which is what
	// makes "a failed or aborted transcode is kept off the array" true rather than nearly
	// true. Either way the SWAP is unchanged and reads a path in the source's own directory.
	// See copyBackBesideSource, below the gates.
	scratch := strings.TrimSpace(e.Cfg.ScratchDir)
	var work string
	if scratch == "" {
		// Pick the temp path and clear any stale temp at it. The n-suffixed candidates exist
		// for one real situation: a source resolved intact is encoded again while the
		// replacement of the FAILED attempt may still sit beside it, at a temp path because
		// it could not be MOVED to its retained name. Such a candidate is skipped, never
		// cleared; a stale temp at a path nothing holds back is cleared as before.
		w, err := e.pickTempPath(ctx, dir, stem, outExt)
		if err != nil {
			e.Log.Warn("FAIL (no free temp path beside the source, source untouched)", "file", f, "err", err)
			// Nothing has been encoded and no gate has run: this is the working area
			// refusing, which no member of the gate vocabulary names, so it is the fallback.
			e.fail(ctx, f, key, GateOther, withSourceDimensions(
				&store.Outcome{Reason: err.Error(), Profile: ts.Profile, Decision: by}, props))
			return nil
		}
		work = w
	} else {
		// The per-job free-space pre-check, taken at the moment this job is about to encode
		// and BEFORE the encoder has written a byte. The startup floor cannot do this job: it
		// has no per-file size to check against, and a filesystem can fill from outside
		// holdfast at any point after a run begins. A job that cannot fit fails here, names
		// the figures, leaves the source untouched, and the scan carries on.
		if err := e.scratchRoomFor(scratch, f, fi.Size()); err != nil {
			e.Log.Warn("FAIL (not enough room in the scratch directory, source untouched)", "file", f, "err", err)
			e.fail(ctx, f, key, GateOther, withSourceDimensions(
				&store.Outcome{Reason: err.Error(), Profile: ts.Profile, Decision: by}, props))
			return nil
		}
		w, err := e.pickScratchPath(scratch, f, outExt)
		if err != nil {
			e.Log.Warn("FAIL (no free working path in the scratch directory, source untouched)", "file", f, "err", err)
			e.fail(ctx, f, key, GateOther, withSourceDimensions(
				&store.Outcome{Reason: err.Error(), Profile: ts.Profile, Decision: by}, props))
			return nil
		}
		work = w
		// The scratch working file is disposable by construction and goes on EVERY exit path:
		// it never becomes the file the swap reads (that is always a copy beside the source),
		// so no outcome is protected by keeping it. A run killed before this runs leaves it
		// for the scratch sweep.
		defer func() { _ = os.Remove(work) }()
	}
	// tmp is the path the SWAP will read. Without a scratch directory it is the
	// working file itself, exactly as before. With one it becomes the copy made
	// beside the source once the gates have accepted - assigned below, never here,
	// because nothing may exist under the source's directory until then.
	tmp := work
	e.Log.Info("transcode", "file", f, "codec", codec, "-> ", targetCodec, "worker", worker,
		"library_root", root.Clean, "crf", ts.CRF, "encoder", ts.Encoder,
		"encode_profile", ts.Profile, "working_file", work)
	e.advance(ctx, f, key, store.Encoding)

	// out is the PROOF, accumulated as the pipeline learns each fact (TRANSCODE-13). Every
	// terminal path below hands this same value to the store and to the Observer, so the
	// ledger and the live UI cannot disagree about what happened. From here on the file has
	// reached the encoder, so the encoder is attributable - on a failure as much as on a
	// success - and so is the encode profile that chose it.
	out := withSourceDimensions(&store.Outcome{Encoder: ts.Encoder, Profile: ts.Profile, Decision: by}, props)
	// What this job's selection did, recorded from the SAME plan the argv is built from
	// and the gate is checked against - on a failure as much as on a success, because a
	// rejected encode is exactly where an operator wants to know which streams it was
	// asked to carry. An empty record is a record: it says this job applied a selection
	// and dropped nothing, which is a different statement from a row that says nothing.
	out.DroppedStreams = plan.DroppedRecord()
	if plan.AudioSelectionNotApplied() {
		out.SelectionNotApplied = SelectionNotAppliedNoAudio
	}

	// THE DEINTERLACE THIS JOB APPLIES, resolved once from the profile and this source's own
	// snapshot. The encoder resolves the same value from the same two inputs through the same
	// function, and the perceptual gate is HANDED this one - so the filter that ran, the
	// filter the reference is produced by and the filter the row records are one answer. The
	// error was already refused above, before a temp path was chosen.
	film, _ := deinterlaceApplied(prof, props)

	// THE RESOLUTION CEILING THIS JOB APPLIES, resolved once from the same profile and the
	// same snapshot, on exactly the deinterlace's terms. The encoder resolves the same value
	// from the same two inputs through the same function, and the perceptual gate is HANDED
	// this one - so the scale that ran, the scale the output is measured back up through and
	// the scale the row records are one answer.
	shrink := downscaleApplied(prof, props)

	encStart := time.Now()
	if err := e.encode(ctx, worker, f, work, props, prof, plan); err != nil {
		if ctx.Err() != nil { // interrupted: discard temp, DON'T finish — leave active for RecoverStale
			_ = os.Remove(tmp)
			return ctx.Err()
		}
		e.Log.Warn("FAIL (encode error, source untouched)", "file", f, "err", err)
		_ = os.Remove(tmp)
		out.Reason = err.Error() // the failure error — previously computed and dropped
		e.fail(ctx, f, key, GateEncode, out)
		return nil
	}
	encodeDur := time.Since(encStart)
	out.EncodeMs = ptr(encodeDur.Milliseconds())
	// What actually came out, measured on the file the encoder wrote and recorded on the row
	// whether the gates below then accept it or reject it: a rejected encode is exactly where
	// an operator wants to know what was produced. A job under no `max_height` ceiling scales
	// nothing, so these are the source's own dimensions on every such job - which is what
	// makes a row where they are NOT a fact worth having. An output the probe cannot measure
	// records nothing rather than a zero.
	if w, h, ok := e.Probe.Dimensions(ctx, work); ok {
		out.OutputWidth, out.OutputHeight = ptr(w), ptr(h)
	} else {
		e.Log.Warn("the output's resolution could not be measured (recorded as not recorded)",
			"file", f, "working_file", work)
	}

	e.advance(ctx, f, key, store.Verifying)
	proof, gate, class, reason := e.verifyOutput(ctx, f, work, prof, targetCodec, plan, film, shrink)
	leaveGateFlight()
	// Record whatever VMAF measured, on the reject path too: the numbers that rejected an
	// encode are exactly the ones an operator wants to see.
	out.VmafMean, out.VmafMin, out.VmafModel = proof.Mean, proof.Min, proof.Model
	// The comparison format and the chroma measurement travel with the score (GATE-4): a
	// stored score whose pixel format is unrecorded does not say which pixels were compared,
	// and a verdict with no chroma figure does not say whether the colour survived.
	out.VmafPixFmt, out.VmafChroma, out.VmafChromaMetric = proof.PixFmt, proof.ChromaMin, proof.ChromaMetric
	// And which video stream those pixels came from. An unmeasured gate carries "" and the
	// column stays NULL: not recorded, never the stream this build would have scored.
	out.VmafStream = proof.Stream
	// WHAT THIS JOB DID TO THE PICTURE. It is recorded from the filter the encoder applied,
	// not from the proof, because it is true of the job whether or not the perceptual gate
	// ran - and it is recorded on the reject path as much as on the accept path, because a
	// row that says a file was refused says nothing about provenance unless it also says
	// what was tried. An explicit FALSE here is a statement this build ran the job and
	// deinterlaced nothing, which is a different fact from the NULL every row written before
	// this column carries.
	out.Deinterlaced, out.DeinterlaceFilter = ptr(film.Enabled()), film.Spec
	// And WHETHER THE PICTURE WAS MADE SMALLER, recorded on the same terms and from the same
	// place: the scale the encoder applied, not the proof, because it is true of the job
	// whether or not the perceptual gate ran, and recorded on the reject path as much as on
	// the accept path. An explicit FALSE is this build saying it ran the job and scaled
	// nothing, which is a different fact from the NULL every row written before this column
	// carries. The SCALER travels with it because "scaled" with no algorithm named beside it
	// does not say what was done: two resamplers produce two different pictures from one
	// source, and this row outlives that source.
	out.Downscaled = ptr(shrink.Enabled())
	if shrink.Enabled() {
		out.DownscaleScaler = downscale.Scaler
	}
	// The resolution the perceptual gate MEASURED at, which on a downscaling job is the
	// source's rather than the output's. It comes off the proof and not off the scale,
	// because it is a fact about the measurement: a job whose gate did not run records
	// nothing here, exactly as it records no score.
	out.VmafScoredWidth, out.VmafScoredHeight = proof.ScaledWidth, proof.ScaledHeight
	// Why the gate did not run, when it did not. It travels beside the figures rather than
	// instead of them: every VMAF field above is "" or nil on such a row, so what a reader
	// gets is "not measured, and here is why" - never a zero, which would be a fabricated
	// measurement of a gate nobody ran.
	out.VmafSkipped = proof.Skipped
	if proof.Skipped != "" {
		e.Log.Warn("the VMAF gate did not run on this job", "file", f, "why", proof.Skipped,
			"established_instead", "every carried video stream is identical to the source stream it came from")
	}
	if reason != nil {
		if ctx.Err() != nil {
			_ = os.Remove(tmp)
			return ctx.Err()
		}
		e.Log.Warn("FAIL (verify rejected, source untouched)", "file", f,
			"reason", reason.Error(), "failure_class", class.Class())
		_ = os.Remove(tmp)
		// The gate decided all three of these at the line that rejected the encode; nothing
		// here re-reads the message to work out what kind of rejection it was, or which
		// check made it.
		out.FailureClass = class
		out.Reason = failureReason(class, reason.Error())
		e.fail(ctx, f, key, gate, out)
		return nil
	}

	// Re-check the collision guard right before the swap: an encode can take hours, and a
	// distinct file that appeared at `final` in that window is never overwritten.
	if final != f {
		if _, err := os.Lstat(final); err == nil {
			e.Log.Warn("FAIL (target appeared during encode — refusing to clobber)", "file", f, "target", final)
			_ = os.Remove(tmp)
			out.Reason = "target appeared during encode — refused to clobber " + final
			e.fail(ctx, f, key, GateSwap, out)
			return nil
		}
	}

	// THE COPY BACK, and the ONE place a scratch_dir changes what happens beside the source.
	// The gates have ACCEPTED, so - and only so - a file may now appear in the source's own
	// directory. It is built by the same construction as the in-place temp (pickTempPath), so
	// the stale-temp sweep, the record-based hold-backs and the record-free
	// stray-replacement hold all cover it with no new rule; it is proved to carry exactly the
	// bytes the gates passed; and it is what the EXISTING swap below renames, unchanged, from
	// the source's own directory.
	//
	// Nothing is ever renamed or moved out of the scratch directory onto the source or into
	// its directory. The swap's whole no-loss story rests on an atomic same-filesystem rename
	// whose failure means it did not happen, and the EXDEV refusal in swap.go says outright
	// that holdfast reports a filesystem boundary rather than copying across it. Copying into
	// a temp and renaming that keeps exactly one swap shape, on every mount.
	//
	// It runs BEFORE the metadata carry below, a constraint rather than a preference: with a
	// scratch_dir configured `tmp` is the copy and does not exist until this block has made
	// it, so a carry taken first would have no file to carry onto. The copy is bytes only, so
	// the two compose in this order and in no other.
	if scratch != "" {
		t, err := e.pickTempPath(ctx, dir, stem, outExt)
		if err != nil {
			e.Log.Warn("FAIL (no free temp path beside the source for the accepted encode, source untouched)", "file", f, "err", err)
			out.Reason = err.Error()
			e.fail(ctx, f, key, GateSwap, out)
			return nil
		}
		if err := e.copyBackBesideSource(work, t); err != nil {
			// Nothing is renamed and the source is untouched. The copy is removed:
			// it is at a temp name, never at an ordinary media name, so no reader
			// and no later run can mistake it for anybody's media.
			_ = os.Remove(t)
			e.Log.Warn("FAIL (the accepted encode could not be established beside the source, source untouched)",
				"file", f, "scratch_working_file", work, "copy", t, "err", err)
			out.Reason = err.Error()
			e.fail(ctx, f, key, GateSwap, out)
			return nil
		}
		tmp = t
	}

	// Carry the SOURCE's metadata onto the replacement (S0085); see metadata.go for what is
	// carried, what is not, and why a failure here is a FAILED SWAP with one narrow
	// exemption absorbed inside carrySourceMetadata.
	//
	// Placement is a constraint and not a preference, the same argument the temp fsync and
	// the undo retention are both hoisted for: BEFORE the fsync below, so the attributes the
	// rename publishes are as durable as the bytes, and therefore outside the TRANSCODE-16
	// re-fingerprint window, whose promise is a TOCTOU of the microseconds between that stat
	// and the rename syscall.
	//
	// The RESIDUAL, stated rather than implied: a concurrent chmod or chown on the source
	// inside this window moves neither size nor mtime, is not detected, and the replacement
	// is published with the attributes read a moment earlier.
	if step, merr := e.carrySourceMetadata(f, tmp); merr != nil {
		e.Log.Warn("FAIL (could not "+step+", source untouched)", "file", f, "temp", tmp, "err", merr)
		_ = os.Remove(tmp)
		out.Reason = "could not " + step + ": " + merr.Error()
		e.fail(ctx, f, key, GateSwap, out)
		return nil
	}

	// Durability before the swap (TRANSCODE-17). os.Rename is atomic against a concurrent
	// reader, but atomicity is not PERSISTENCE: after Encode returns the temp's data blocks
	// may live only in the page cache, so a POWER LOSS could make the rename durable while
	// the bytes it names are not, leaving a zero-length or torn file where the source was.
	// The durable-rename discipline is to fsync the temp's DATA before the rename and the
	// parent DIRECTORY after it (below). A temp that cannot be made durable is NOT swapped.
	//
	// This runs BEFORE the TRANSCODE-16 re-fingerprint, deliberately: the temp fsync flushes
	// the encode's residual dirty pages and can be slow, and it must not sit inside the
	// window the re-fingerprint promises is microseconds wide. It neither reads nor depends
	// on the source, so hoisting it is free.
	if err := e.fsync(tmp); err != nil {
		e.Log.Warn("FAIL (could not fsync the encode before the swap, source untouched)", "file", f, "err", err)
		_ = os.Remove(tmp)
		out.Reason = "fsync temp before swap: " + err.Error()
		e.fail(ctx, f, key, GateSwap, out)
		return nil
	}

	// Retain the original BEFORE the rename (UNDO-6). The rename is the only irreversible
	// act this tool performs and every gate in front of it is an estimate; a second link
	// taken here keeps the original's bytes alive after the rename has taken its only other
	// name away. Placement mirrors the temp fsync, hoisted OUT of the re-fingerprint's
	// window so it does not grow by a link() and a possible mkdir(); if the re-fingerprint
	// then refuses the swap, the retention is discarded on the way out.
	//
	// A retention that cannot be taken SKIPS the file: a swap this tool could not undo is
	// not one it performs while that promise is in force.
	var retained string
	if e.Cfg.UndoEnabled() {
		u := e.undo()
		r, rerr := u.retain(f, key)
		if rerr != nil {
			e.Log.Info("skip (the original could not be retained, so the swap could not be undone — source untouched)",
				"file", f, "err", rerr)
			_ = os.Remove(tmp)
			e.finish(ctx, f, key, store.Skipped, e.because(SkipUndoRetentionFailed, by, prof, ts, props))
			return nil
		}
		retained = r
		if e.hookAfterRetain != nil {
			if herr := e.hookAfterRetain(retained); herr != nil {
				u.discard(retained)
				_ = os.Remove(tmp)
				out.Reason = "aborted after retaining the original: " + herr.Error()
				e.fail(ctx, f, key, GateSwap, out)
				return nil
			}
		}
	}
	// abandon undoes the retention on every path below that decides NOT to swap. Such a link
	// is not a loss - it names bytes the source still has - but it would raise the source's
	// link count for nothing and orphan a name in the retention area.
	abandon := func() {
		if retained != "" {
			e.undo().discard(retained)
		}
	}

	// Re-fingerprint the SOURCE right before the swap (TRANSCODE-16, the headline no-loss
	// hazard). The source was fingerprinted ONCE at entry and only the TARGET re-checked
	// since; an encode runs for hours, and a source Plex or an *arr rewrote in that window
	// would be atomically overwritten with a re-encode of stale bytes that every gate
	// "passed" only because they ran against the OLD file. It bites both swap shapes: an
	// in-place rename overwrites the new source, and an ext-change swap deletes it
	// afterwards. A moved fingerprint discards the temp and FAILS, never swaps. Nothing slow
	// runs between this stat and the rename syscall, so the TOCTOU is microseconds wide; an
	// exclusive lock on another process's file is the only thing that would close it.
	//
	// The guard's achieved GRANULARITY is recorded here, as it runs (FILESYSTEM-1).
	// Two of the three fields are measured facts about the check just performed - which
	// attributes it compared, and the resolution of the timestamp it compared, which is
	// a real duration and stays a duration. The third is a CLASS LABEL naming which of
	// the two residual windows the shipped documentation states applies, keyed to a
	// classification taken WHEN THIS GUARD RUNS: a root that was local when the run
	// began can have a NAS mounted beneath it hours later, and the record has to
	// describe the storage the check actually ran against. `undetermined` takes the
	// network window, the same fail-safe that makes an unrecognised type not-local.
	guardClass := fsclass.Of(e.fsLookup, f)
	out.GuardAttributes = probe.AttributeNames
	out.GuardTimeResolution = probe.MTimeResolution
	out.GuardResidualWindow = residualWindowFor(guardClass)

	if cur := probe.Fingerprint(f); cur != key {
		e.Log.Warn("FAIL (source changed during encode — refusing to overwrite the newer content)", "file", f, "entry", key, "now", cur)
		abandon()
		_ = os.Remove(tmp)
		out.Reason = "source changed during encode (fingerprint " + key + " -> " + cur + ") — refused to overwrite newer content"
		e.fail(ctx, f, key, GateSwap, out)
		return nil
	}

	// Record BOTH files' rename-invariant attributes before attempting the rename, so
	// a re-stat afterwards has two records to compare against. Rename-invariance is
	// what makes the comparison mean anything: these attributes describe the content at
	// a path, so the replacement's record still matches when the replacement is
	// observed at the SOURCE path - which is the only way the applied case is reachable
	// at all. If either record cannot be taken, do not attempt the swap: a rename whose
	// outcome could never be established afterwards is not one to start.
	srcRec, srcErr := probe.StatAttributes(f)
	replRec, replErr := probe.StatAttributes(tmp)
	if srcErr != nil || replErr != nil {
		e.Log.Warn("FAIL (could not record the pre-swap attributes, source untouched - refusing a swap whose outcome could not be established)",
			"file", f, "source_err", srcErr, "replacement_err", replErr)
		_ = os.Remove(tmp)
		out.Reason = "could not record the pre-swap attributes of both files - refused the swap: " +
			errText(srcErr) + " / " + errText(replErr)
		e.fail(ctx, f, key, GateSwap, out)
		return nil
	}

	// Atomic swap. Same directory => same filesystem => rename() is atomic. If the
	// ext is unchanged the rename replaces the source in one step; if it changed we
	// rename to the new name then remove the now-orphaned source.
	//
	// A FAILED rename is no longer self-reporting. It used to log "swap error, source
	// untouched" unconditionally, which on a network filesystem can be simply false -
	// rename(2) says you cannot assume a failed rename was not performed. What happens
	// now is in handleFailedSwap: re-stat, classify at swap time, and decide between
	// four exhaustive cases, only one of which is allowed to say "untouched".
	//
	// abandon is handed DOWN rather than called here, because whether the retention is
	// an orphan is exactly the question handleFailedSwap answers and this line cannot.
	// It is dropped only in the branch that ESTABLISHED the source untouched; under an
	// applied-despite-error or an indeterminate outcome the retained link may be the
	// only remaining name for the original's bytes, so it is held.
	if err := e.rename(tmp, final); err != nil {
		e.handleFailedSwap(ctx, f, key, tmp, final, srcRec, replRec, err, out, abandon)
		return nil
	}

	// Durability after the swap (TRANSCODE-17). fsync the parent directory so the
	// rename above survives a power loss. dir is the source's directory and, because
	// the temp is same-dir, also final's directory, so one fsync covers the whole swap.
	dirErr := e.fsync(dir)

	if final != f {
		if dirErr != nil {
			// The rename is not provably durable. Removing the source now would risk a
			// power-loss window where the rename is lost AND the source removal persists,
			// leaving the library entry pointing at nothing — the exact hazard this phase
			// closes. Leave BOTH files (a duplicate, never a loss), identical to a crash
			// in this window (fixture 3): the store row stays active for the next
			// RunOneshot's RecoverStale, and the collision guard reconciles the duplicate
			// on the next scan. Return a non-context error — it is logged and the scan
			// continues; the source is never removed under an unproven rename.
			e.Log.Warn("swap durability unproven (parent dir fsync failed) — leaving both files for the next scan to reconcile", "file", f, "final", final, "err", dirErr)
			// The source is still on disk, so there is nothing for the undo window to
			// hold: drop the retention rather than leave an unreferenced link raising
			// the source's link count while the next scan reconciles the duplicate.
			abandon()
			return fmt.Errorf("fsync parent dir after rename of %s: %w", final, dirErr)
		}
		// Test seam (TRANSCODE-16 fixture c): simulate a crash in the window between the
		// rename and the delete. nil in production, so this is a no-op there.
		if e.hookAfterRename != nil {
			if err := e.hookAfterRename(); err != nil {
				// Both files are on disk now (final = the new encode, f = the original):
				// a duplicate, never a loss. Abort exactly as a crashed process would —
				// the store row stays active for the next RunOneshot's RecoverStale, and
				// the collision guard reconciles the duplicate on the next scan.
				return err
			}
		}
		if err := os.Remove(f); err != nil {
			e.Log.Warn("transcoded ok but could not remove original", "file", f, "err", err)
		}
		// Persist the removal too. Unlike the rename this is best-effort: a lost removal
		// only leaves a duplicate (fail-safe, reconciled on the next scan), never a loss,
		// so a failure here is logged, not a gate.
		if err := e.fsync(dir); err != nil {
			e.Log.Warn("could not fsync parent dir after removing original (durability not guaranteed)", "file", f, "err", err)
		}
	} else if dirErr != nil {
		// In-place swap: the rename already atomically replaced the source, so a reader
		// always sees either the old or the new file, never nothing — a dir-fsync failure
		// here cannot lose data, it only means the swap's durability isn't guaranteed
		// across a power loss. There is nothing to roll back; log and proceed.
		e.Log.Warn("swap durability unproven (parent dir fsync failed) — in-place swap already applied", "file", f, "err", dirErr)
	}
	// The sizes either side of the swap. fi was stat'd at entry (the pre-encode
	// source), so SourceBytes is accurate even though f may already be gone (a
	// container-changing swap removed it). BOTH are persisted, not just their
	// difference: that is what makes a durable lifetime reclaimed total derivable
	// (TRANSCODE-14) and what lets a UI show "before → after" instead of a bare delta.
	newSize := probe.FileSize(final)
	out.SourceBytes, out.OutputBytes = ptr(fi.Size()), ptr(newSize)

	// The swap has happened, so the retention becomes a promise this tool has to keep:
	// record it, with the fingerprint of what the swap left behind so a restore can
	// refuse to overwrite anything else. Recorded AFTER the swap deliberately - before
	// it the record would describe a swap that may never happen, and a ledger that
	// claims a file was replaced when it was not is worse than one that is a moment
	// behind. The window that opens here is covered without a record: the retained
	// original carries this tool's own marker and the source's own fingerprint in its
	// NAME, which is how the hardlink guard recognises it even if this write is lost.
	if retained != "" {
		if err := e.undo().record(ctx, f, final, retained, fi.Size()); err != nil {
			e.Log.Warn("swapped, but could not record the retained original (it will not be released automatically until the record exists)",
				"file", f, "retained", retained, "err", err)
		}
	}

	// The log line carries the comparison format, the scored stream and the chroma figure
	// beside the score, because "recorded alongside the score" has to mean everywhere the
	// score is recorded - a log line that reports a bare 98.4 is one more surface where a
	// reader cannot say which pixels were compared, or which stream they came from.
	e.Log.Info("DONE", "file", final, "bytes", newSize, "reclaimed", fi.Size()-newSize,
		"encode_ms", encodeDur.Milliseconds(),
		"vmaf", logScore(proof.Mean), "vmaf_min", logScore(proof.Min),
		"vmaf_pix_fmt", logText(proof.PixFmt), "vmaf_stream", logText(proof.Stream),
		"vmaf_chroma", logScore(proof.ChromaMin), "vmaf_chroma_metric", logText(proof.ChromaMetric),
		// The filter the reference was produced by, beside the format the comparison was made
		// in, for the same reason: a score whose reference nobody can name is not a number a
		// reader can act on, and this line is one of the surfaces the score is recorded on.
		"deinterlace", logText(film.Spec))
	// The done row is keyed under the FINAL file's own path+fingerprint (mirroring
	// the pre-TRANSCODE-5 ledger behaviour) so a resume short-circuits on the new
	// file's identity, not the pre-swap source's. The post-swap fingerprint is ALWAYS a
	// fresh key with no existing row, so Claim it first (Finish alone would be a no-op
	// UPDATE against a nonexistent row and the done outcome would be silently lost).
	//
	// WHICH HALF OF THAT IS LOAD-BEARING CHANGED WITH S0085. probe.Fingerprint is
	// size:mtime, and this used to rest on both halves moving - "even when final == f, the
	// rename changed the file's size/mtime in place". With preserve_mtime on (the shipped
	// default) the replacement carries the SOURCE's mtime, so that half does not move at
	// all and the whole of the guarantee now rests on the SIZE.
	//
	// The size is what makes it safe, and it is a gate rather than an expectation: the
	// verify gate refuses an output that is not strictly smaller than its source
	// (min_savings_percent, 0 = strictly smaller), so a swap that reached this line
	// necessarily shrank the file. If that ever stopped holding, this row would key under
	// the SOURCE's identity, the pre-swap row would be the same row rather than a
	// superseded one, and the next scan would re-encode the replacement as if it were the
	// untouched source. TestSwap_PreserveMtimeStillProducesAFreshFingerprint asserts both
	// halves of that - the mtime did not move, the size did - so this comment cannot
	// quietly stop being true.
	finalKey := probe.Fingerprint(final)
	// The row this writes is keyed under the REPLACEMENT's path, so its inputs are resolved
	// for that path and not for the source's. Symmetry is the whole of the reason: the next
	// scan meets the file at `final` and compares against `final`'s resolution, and where
	// the container ext moved (film.mkv -> film.mp4) an encode profile matching `*.mkv` no
	// longer selects it. A row recording the source's resolution would then be a row nothing
	// can re-derive - re-opened on every scan for ever, which on a library is a re-encode of
	// everything, each accepted encode deleting its source.
	tsFinal := e.Cfg.TranscodeIn(prof, final)
	if _, err := e.Store.Claim(ctx, final, finalKey, worker, e.Cfg.MaxFailures, e.inputsFor(prof, tsFinal)); err != nil {
		e.Log.Warn("claim of final key failed (done outcome still applies on disk)", "file", final, "err", err)
	}
	// What this encode was taken under: the codec it targeted and the three settings
	// that decided what came out. It is recorded HERE, on the done row, because that row
	// is the permanent answer about the replacement - and the moment any of the four
	// moves, the answer is one this build would no longer give, so the next scan offers
	// the file back to the guards rather than skipping it for ever.
	out.DecisionInputs = e.inputsRead(prof, tsFinal, InputTargetCodec, InputEncoder, InputCRF, InputPreset)
	// Record the terminal Done state in the store WITHOUT emitting (finishStore), then
	// emit ONE rich Done event carrying the same proof. Emitting exactly once here
	// (rather than a generic finish emit plus a separate rich one) keeps a metrics
	// consumer's per-outcome counters from double-counting done.
	e.finishStore(ctx, final, finalKey, store.Done, out)
	e.emit(Event{Path: final, Status: store.Done, Worker: worker, Outcome: out})
	// Prune the superseded pre-swap row (the source's old identity), so the table
	// doesn't accumulate one dangling row per transcoded file. The swap always changes the
	// file's SIZE (see the fresh-key argument above - with preserve_mtime on, the mtime is
	// the source's and the size is the whole of the difference), so (f,key) is never the
	// same row as the fresh (final,finalKey) done row just written - but guard it anyway.
	if f != final || key != finalKey {
		if err := e.Store.Delete(ctx, f, key); err != nil {
			e.Log.Warn("could not prune superseded job row", "file", f, "err", err)
		}
	}
	return nil
}

// sourceVerdict is what the source-side guards concluded about one file: the guard that
// stopped it, or none, plus the three things a caller past the guards needs from the same
// probe snapshot they were answered from.
//
// It carries its own LOG LINE because a caller switching on the token to decide what to say
// would put the wording of a skip beside the thing that COUNTS it rather than beside the
// guard that decided it, and the two would drift the first time a guard learned a reason.
type sourceVerdict struct {
	// guard is the Skip*/Fail* token, empty when every guard passed.
	guard string
	// failed marks the one verdict that is a FAILURE rather than a skip: a source the probe
	// reported no video stream for.
	failed bool
	// inputs are the configuration keys this verdict READ, recorded on the terminal row so
	// a later configuration change can re-derive it.
	inputs []string
	// log and logArgs are what the daemon says when this guard fires. The caller supplies
	// the file, which every one of them names first.
	log     string
	logArgs []any

	// codec is the source codec the snapshot read, and outExt/target are what the swap would
	// publish. All three come off the same snapshot the guards read, so nothing past them
	// re-derives one.
	codec  string
	outExt string
	target string
}

// stopped reports whether a guard refused the file.
func (v sourceVerdict) stopped() bool { return v.guard != "" }

// guardSource runs every source-side skip guard, in the order the pipeline has always run
// them, off ONE probe snapshot, and WRITES NOTHING. ProcessFile turns what comes back into a
// terminal row; the read-only plan pass counts it. Both therefore ask the same guards the
// same question in the same order, and a guard added here reaches both with no second edit -
// which is what stops a report about what a run would do from drifting from the run.
//
// snapshot takes the file's probe snapshot. It is a parameter rather than e.Probe.VideoProps
// so a caller can COUNT the snapshots one pass takes, which is the property "one invocation
// is one pass over the library" is graded by; the daemon hands in the prober's own.
//
// The returned snapshot is nil exactly when the guards stopped the file before one was taken
// (the symlink guard), which is why that guard is where it is: a symbolic link never pays for
// an ffprobe.
// prof is the EFFECTIVE library profile for this file - the root's own values with the
// first matching resolution rule laid over them - and it is a parameter rather than read off
// root.Profile because the two differ exactly where a band applies. The bitrate floor and
// every threshold below are still the library's and not this job's: an encode profile may
// change what the encoder produces and may not move a gate that decides whether a source is
// destroyed, and a rule may move the threshold only for the band its operator wrote it for.
func (e *Engine) guardSource(ctx context.Context, f string, root config.Root, prof config.Profile,
	ts config.Transcode, targetCodec string,
	snapshot func(context.Context, string) *probe.VideoProps) (*probe.VideoProps, sourceVerdict) {
	// Symlink guard (TRANSCODE-16). A symlink has nlink == 1 and slips past the hardlink
	// guard, and config.Validate refuses a symlinked ROOT but not a symlinked file within
	// the tree. The swap would replace the LINK itself with a regular file, orphaning the
	// real target and changing what the library entry means. Resolving and transcoding the
	// target is a deliberate non-goal, so a symlinked source is SKIPPED.
	if probe.IsSymlink(f) {
		return nil, sourceVerdict{guard: SkipSymlink,
			log: "skip (symlinked source — swap would replace the link, orphaning its target)"}
	}

	// One probe snapshot of the source, shared by every skip guard below AND handed to the
	// encoder (TRANSCODE-PERF), in place of the ~15 separate ffprobe/ffmpeg processes a
	// single encode-bound file used to spawn. Reading every guard off one snapshot is more
	// self-consistent than re-probing a file mid-pipeline. The costly whole-file checks are
	// NOT here: they run in verifyOutput against the encoded temp.
	props := snapshot(ctx, f)

	codec := props.Codec()
	if codec == "" {
		return props, sourceVerdict{guard: FailUnreadable, failed: true,
			log: "skip (unreadable / no video stream)"}
	}
	if isAlreadyTargetCodec(targetCodec, codec) {
		return props, sourceVerdict{guard: SkipAlreadyTargetCodec, codec: codec,
			inputs: []string{InputTargetCodec},
			log:    "skip (already at target codec)",
			logArgs: []any{"codec", codec, "target", targetCodec,
				"library_root", root.Clean, "encode_profile", ts.Profile}}
	}

	if br := props.BitrateKbps(); br > 0 && br < prof.MinBitrateKbps {
		return props, sourceVerdict{guard: SkipLowBitrate, codec: codec,
			inputs:  []string{InputMinBitrateKbps},
			log:     "skip (low bitrate)",
			logArgs: []any{"kbps", br, "min", prof.MinBitrateKbps, "library_root", root.Clean}}
	}

	// THE FINAL-SWAP GUARD, and it stands here because it is a refusal about the
	// CONFIGURATION rather than about the file: a root that sets a ceiling with the undo
	// window closed and no acknowledgement refuses every source above that ceiling, and
	// telling the operator about the one file's field order first would bury the thing they
	// have to fix. Everything above it is cheaper still and leaves this file out of the
	// question entirely - a source already at the target codec is not re-encoded at all, so
	// nothing would scale it.
	//
	// It costs no probe: the dimensions come off the snapshot every guard below reads.
	//
	// The SOURCE-HEIGHT arm is the backstop behind ProcessFile's own, which decides the file
	// under nothing at all before a claim is taken (see effectiveProfile). It is reachable
	// through the read-only plan pass and through a Config assembled in Go, and without it a
	// ceiling would be resolved against dimensions nobody established.
	if prof.DownscaleEnabled() {
		w, h, established := props.Dimensions()
		if !established {
			return props, sourceVerdict{guard: SkipUndeterminedSourceHeight, codec: codec,
				inputs: []string{InputMaxHeight, InputUndoWindow},
				log: "skip (ffprobe did not establish this source's dimensions and this library root " +
					"sets a max_height ceiling - refusing to scale a file by a factor nobody measured)",
				logArgs: []any{"library_root", root.Clean, "max_height", prof.MaxHeight}}
		}
		if shrink := prof.DownscaleFor(w, h); downscaleRefused(&e.Cfg, prof, shrink) {
			return props, sourceVerdict{guard: SkipDownscaleUnacknowledged, codec: codec,
				inputs: []string{InputMaxHeight, InputUndoWindow},
				log: "skip (this file would be scaled down by max_height AND the undo window is " +
					"disabled, so the swap would be final, AND this root did not set " +
					"downscale_acknowledged - refusing to make an irreversible swap to fewer pixels " +
					"on the strength of one key; nothing was encoded and the source is untouched. " +
					"Set downscale_acknowledged: true to accept it, or set undo_window_hours so the " +
					"swap can be walked back)",
				logArgs: []any{"library_root", root.Clean, "max_height", prof.MaxHeight,
					"undo_window_hours", e.Cfg.UndoWindowHours,
					"source", strconv.Itoa(w) + "x" + strconv.Itoa(h),
					"would_become", strconv.Itoa(shrink.Width) + "x" + strconv.Itoa(shrink.Height)}}
		}
	}

	// Scan-type guards. Every branch of the field order is answered here and none of them
	// falls through: an interlaced source is skipped unless a deinterlace is configured for
	// it, and a field order ffprobe could not establish is CLASSIFIED rather than assumed
	// progressive. Only `progressive` proceeds on the strength of what the file said about
	// itself.
	switch order := props.FieldOrder(); {
	case interlacedFieldOrder(order):
		if v, stop := e.interlacedVerdict(ctx, f, prof, props, codec); stop {
			return props, v
		}
	case order == "progressive":
		// The one answer that licenses the progressive encode path.
	default:
		// Neither progressive nor one of the four interlaced spellings: ffprobe reported
		// `unknown`, `N/A` or nothing at all (normalised to "" by probe.normFieldOrder), or
		// a value this build does not know. Re-encoding it would be a guess about whether
		// combing is about to be baked into the replacement of a file this tool then
		// deletes, so the file is decided under no scan type at all and the row says why.
		return props, sourceVerdict{guard: SkipUnknownFieldOrder, codec: codec,
			log:     "skip (ffprobe did not establish this source's field order - refusing to encode it as progressive on a guess)",
			logArgs: []any{"field_order", props.FieldOrderRaw()}}
	}

	// HDR/DV guard (TRANSCODE-3). A generic libx265 re-encode cannot preserve a Dolby Vision
	// RPU or HDR10+ dynamic metadata and would SILENTLY strip it, a permanent,
	// invisible-until-viewed loss, so detect and SKIP. HDR10 STATIC metadata IS carried
	// through the encode (hdr.DeriveColorArgs). Probed only here, on an encode-bound file,
	// so the cost falls on the minority actually re-encoded.
	switch hdr.ClassFrom(props.CodecTag(), props.SideData(), props.Color("color_transfer")) {
	case hdr.ClassDV:
		return props, sourceVerdict{guard: SkipDolbyVision, codec: codec,
			log: "skip (Dolby Vision — RPU cannot survive a generic re-encode)"}
	case hdr.ClassHDR10Plus:
		return props, sourceVerdict{guard: SkipHDR10Plus, codec: codec,
			log: "skip (HDR10+ dynamic metadata — cannot survive a generic re-encode)"}
	case hdr.ClassHDR10:
		// HDR10 static metadata IS carried through the encode, but a mastering-display or
		// content-light block this build cannot fully parse would be silently dropped.
		// Fail safe: SKIP rather than blind-encode.
		incomplete := e.staticMetadataIncomplete
		if incomplete == nil {
			incomplete = hdr.StaticMetadataIncomplete
		}
		if incomplete(props.FrameSideData()) {
			return props, sourceVerdict{guard: SkipIncompleteHDRMetadata, codec: codec,
				log: "skip (HDR10 static metadata present but incomplete/unparseable — refusing to re-encode and drop it)"}
		}
	}

	// Chroma/bit-depth guard. Preserve the source's chroma subsampling and floor bit-depth
	// at 10; an exotic pix_fmt is SKIPPED rather than silently subsampled or guessed. A
	// forced (non-"auto") PixelFormat bypasses derivation entirely.
	if ts.PixelFormatAuto() {
		srcPixFmt := props.PixFmt()
		if _, ok := hdr.DerivePixFmt(srcPixFmt); !ok {
			return props, sourceVerdict{guard: SkipExoticPixelFormat, codec: codec,
				inputs:  []string{InputPixelFormat},
				log:     "skip (unrecognized/exotic pixel format — refusing to silently subsample)",
				logArgs: []any{"pix_fmt", srcPixFmt}}
		}
	}

	// Source-shape guard. Everything above reads v:0 and only v:0, so a source carrying a
	// SECOND moving-picture stream is one this pipeline cannot honestly claim to preserve:
	// the encode would re-encode it off the first stream's properties and no gate in front
	// of the swap ever looked at it. An ATTACHED PICTURE is the one exception, because it is
	// carried through unencoded (attachedPictureCopyIndexes). The probe is the second and
	// last ffprobe a file pays for, taken here rather than in the eager snapshot so a file
	// that skipped at a cheap guard above never pays for it.
	//
	// The outcome records NO decision inputs, because this guard reads no configuration key:
	// a source's stream shape is a property of the file, so no value an operator edits
	// re-derives the verdict. That is why the token is in SkipGuards - `requeue --guard
	// multi-video-stream` is then the only lever, and a token missing from that list would
	// be a permanent exclusion with no lever at all.
	streams, established := props.VideoStreams()
	if !established || !carriableVideoStreams(streams) {
		return props, sourceVerdict{guard: SkipMultiVideoStream, codec: codec,
			log:     "skip (a video stream beyond the first that is not an attached picture, or a stream shape the probe could not establish)",
			logArgs: []any{"video_streams", len(streams), "probe_established", established}}
	}

	// Output container: "source"/"auto" (default) matches the SOURCE file's own extension,
	// so a stream type that does not round-trip through a different container (MP4 mov_text
	// into MKV) is not forced to change. A forced ContainerExt overrides this.
	outExt := ts.ContainerExt
	if ts.ContainerMatchesSource() {
		outExt = strings.TrimPrefix(filepath.Ext(f), ".")
	}
	base := filepath.Base(f)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	final := filepath.Join(filepath.Dir(f), stem+"."+outExt)

	// Collision guard. When the container ext changes, final is a DIFFERENT path than the
	// source, and a distinct file already there would be silently overwritten before the
	// source was deleted: two files destroyed. Refuse.
	if final != f {
		if _, err := os.Lstat(final); err == nil {
			return props, sourceVerdict{guard: SkipTargetExists, codec: codec,
				inputs:  []string{InputContainerExt},
				log:     "skip (target already exists as a distinct file — refusing to clobber)",
				logArgs: []any{"target", final}}
		}
	}

	return props, sourceVerdict{codec: codec, outExt: outExt, target: final}
}

// interlacedVerdict answers what happens to a source ffprobe reported as interlaced, and
// reports whether the guards STOP there.
//
// It exists as its own function because the answer stopped being one line: the interlace
// skip is what this tool has always done and remains what it does unless the root's profile
// asks for a deinterlace, and the cases that refuse a deinterlace refuse it under their own
// tokens rather than under the interlace one.
func (e *Engine) interlacedVerdict(ctx context.Context, f string, prof config.Profile,
	props *probe.VideoProps, codec string) (sourceVerdict, bool) {
	// The interlace skip records the key it READ, which is what makes the verdict
	// re-derivable rather than permanent: configure a deinterlace for this root and the next
	// scan offers every file this guard held back. A root that configures none records
	// nothing here, because the key is offered only where it is enabled (see
	// InputDeinterlace) - so a row written for such a root is the row this build's
	// predecessor wrote.
	interlaced := sourceVerdict{guard: SkipInterlaced, codec: codec,
		inputs: []string{InputDeinterlace},
		log:    "skip (interlaced - not deinterlacing)"}

	// A root that asks for no deinterlace - and one that asks for one it cannot perform,
	// because it stream-copies the video - keeps the skip this tool has always taken.
	if !deinterlaceWanted(prof) {
		return interlaced, true
	}

	// A deinterlace was asked for, so the CADENCE decides. It costs a bounded decode of the
	// source and is taken here, on a file that has already cleared every cheap guard and is
	// about to be transformed - never on the ordinary path, where the answer would be paid
	// for by every file in a library and read by nothing.
	cad := props.Cadence()
	if !cad.Deinterlaceable() {
		return sourceVerdict{guard: SkipTelecineCadence, codec: codec,
			inputs: []string{InputDeinterlace},
			log: "skip (this source is not one a deinterlace is the right operation for - a telecined " +
				"source needs inverse telecine, which this build does not do, and an unestablished " +
				"cadence is not a licence to transform)",
			logArgs: []any{
				"cadence", string(cad.Class),
				"frames_classified", cad.Frames,
				"repeated_fields", cad.Repeated,
				"interlaced_frames", cad.Interlaced,
				"why", cad.Why,
			}}, true
	}
	// Real interlacing, no pulldown, and a filter configured for it: the file proceeds to
	// the encode, which applies that filter.
	return sourceVerdict{}, false
}

// advance is a small logged wrapper around Store.Advance — a store error here is
// never fatal to the pipeline (the encode/verify still proceeds), it just means the
// store's picture of this job's sub-state may lag; RecoverStale's active-state
// sweep still eventually reconciles it via the next Claim/Finish call.
func (e *Engine) advance(ctx context.Context, path, key string, s store.Status) {
	if err := e.Store.Advance(ctx, path, key, s); err != nil {
		e.Log.Warn("store advance failed (continuing)", "file", path, "status", s, "err", err)
	}
	e.emit(Event{Path: path, Status: s})
}

// recordUndeterminedHeight writes the source-height guard's terminal row: this root's
// configuration needs to know how tall the source is and the probe could not establish it,
// so no rule and no ceiling decides this file.
//
// It claims the row first, exactly as every verdict that records what it READ must: the
// claim is where a row's recorded inputs are compared against what the configuration now
// offers, and a skip written outside it would be a verdict nothing re-derives. The inputs it
// records are exactly the keys that needed the height - the RULE LIST where the root bands
// its files, and the CEILING where one is in force - so removing whichever of them the
// operator wrote offers the file to the pipeline again on the next scan.
//
// A store error is survived in the withholding direction, the same trade every claim path in
// this function takes: the file is simply left for the next scan.
func (e *Engine) recordUndeterminedHeight(ctx context.Context, worker, f, key string, root config.Root,
	by store.Decision, prof config.Profile, ts config.Transcode, props *probe.VideoProps) {
	// The same mutable-guard re-evaluation the ordinary claim makes, and for the same
	// reason: this file reached the band guard with every mutable guard above it silent, so
	// a row one of them parked is stale and the verdict recorded here supersedes it.
	claimed, err := e.Store.Claim(ctx, f, key, worker, e.Cfg.MaxFailures,
		e.inputsFor(prof, ts), mutableGuardSkips...)
	if err != nil {
		e.Log.Warn("the store could not take the claim, so this file is left untouched this pass "+
			"and retried on the next scan", "dependency", "store", "attempted", "claim",
			"next", "retry on the next scan", "file", f, "err", err)
		return
	}
	if !claimed {
		return
	}
	// The keys this verdict READ, which is what makes it re-derivable. inputsRead offers only
	// the ones the configuration actually carries, so a banded root with no ceiling records
	// exactly what it recorded before this item.
	read := []string{InputRules, InputMaxHeight, InputUndoWindow}
	// warn, which in this fleet means the process continued in a degraded state: the run
	// goes on and this file is not in it, which is a condition an operator fixes. It names
	// the DEPENDENCY, what was tried and what happens next, and it names both configurations
	// that can need a height so the operator is not sent to the one they did not write.
	e.Log.Warn("skip (ffprobe did not establish this source's height, and this library root needs "+
		"one - a band selects thresholds by it, a max_height ceiling decides whether this source is "+
		"above it - so the file is decided under no band and no ceiling rather than under a guess)",
		"file", f, "library_root", root.Clean, "rules", prof.Rules.Canonical(),
		"max_height", prof.MaxHeight)
	e.finish(ctx, f, key, store.Skipped,
		e.because(SkipUndeterminedSourceHeight, by, prof, ts, props, read...))
}

// countTerminalRow raises the count of terminal ledger rows this engine has written, for a
// row that WAS written and whose status the ledger itself calls final.
//
// It is the ONE place that count moves. Three store calls in this package can create a
// terminal row, and each reaches here after its own write succeeded:
//
//   - Finish, through finishStore - the swap's done row, every failure, every skip a guard
//     records after the claim.
//   - RecordSkip, at the two guards that fire BEFORE the claim, and only where the write
//     actually recorded a row rather than finding one already there.
//   - RecordSwapIncident, through recordIncident - a swap that did not complete cleanly,
//     which records indeterminate or applied-despite-error. Neither of those is ever
//     re-claimable, so each is as final as a done row is.
//
// That enumeration is the whole of it, and it is what a count bound rests on: a writer that
// does not reach here is a `--limit N` run that never observes a decision being recorded
// and carries every eligible file in the library instead of the N it was asked for.
//
// store.Status.Terminal() is ASKED rather than assumed, because the ledger is the authority
// on which of its own rows are final: a status it does not call final is not a decision a
// bounded run has spent, and a status it starts calling final is counted here the day it
// does.
func (e *Engine) countTerminalRow(s store.Status) {
	if !s.Terminal() {
		return
	}
	e.terminal.Add(1)
}

// finishStore records a terminal outcome + its proof in the store WITHOUT emitting an
// event. The Done swap path uses this (then emits one rich Done event itself); every
// other terminal path uses finish, which emits as well.
//
// The attempt bound goes with the write because a deterministic failure is parked by
// spending it (see store.Finish), and this run's bound is the one that applies.
//
// A store error is LOGGED AND SURVIVED, which is the pre-existing fail-safe and is
// deliberately unchanged for the park. Nothing on disk depends on this write: the source
// is byte-for-byte intact either way, and with no durable record of the park a later pass
// simply meets the file again and is free to attempt it. The alternative - treating an
// unwritten park as a park - would be a park that exists only in a process that has since
// exited, which is not a park at all.
func (e *Engine) finishStore(ctx context.Context, path, key string, s store.Status, o *store.Outcome) {
	if err := e.Store.Finish(ctx, path, key, s, o, e.Cfg.MaxFailures); err != nil {
		e.Log.Warn("store finish failed", "file", path, "status", s, "err", err)
		return
	}
	// A terminal row was WRITTEN, which is what a bounded run counts (see countTerminalRow).
	// It is counted here rather than at each call site so every terminal transition - done,
	// failed, skipped - is counted by construction, and a write that failed counts nothing
	// because nothing was recorded.
	e.countTerminalRow(s)
}

// finish records a terminal outcome AND emits an event carrying the SAME proof — used
// for the skipped/failed terminal transitions (the Done swap path uses finishStore +
// its own rich emit, so Done is emitted exactly once).
func (e *Engine) finish(ctx context.Context, path, key string, s store.Status, o *store.Outcome) {
	e.finishStore(ctx, path, key, s, o)
	e.emit(Event{Path: path, Status: s, Outcome: o})
}

// fail is finish for the FAILED terminal transition, carrying the GATE that refused the
// job out to the observer beside the proof (see Event.Gate).
//
// It exists so the token is chosen at each rejecting call site rather than re-derived
// afterwards from the message, and so that every failed row leaves through one door: a
// path that reached for finish instead would emit a failure attributed to no gate, which
// is the one thing a per-gate count cannot detect by itself.
//
// Nothing about what is STORED changes here - o is handed to the store exactly as finish
// hands it - so the verdict, the Reason text and the failure class are untouched.
func (e *Engine) fail(ctx context.Context, path, key, gate string, o *store.Outcome) {
	e.finishStore(ctx, path, key, store.Failed, o)
	e.emit(Event{Path: path, Status: store.Failed, Outcome: o, Gate: gate})
}

// because builds the Outcome a guard records: WHICH guard fired, WHICH library profile
// the guard was reading when it fired, WHICH encode profile supplied the settings it was
// decided against, and the value that library profile gave for each configuration key
// the guard READ. Skips happen before the encoder runs, so there is nothing else to prove
// about them.
//
// The inputs are what make the verdict re-derivable instead of permanent: a low-bitrate
// skip records the threshold it compared against, so lowering that threshold offers the
// file to the pipeline again, while changing a key the guard never read leaves it exactly
// where it is. A guard that read NO configuration is called with no keys and records the
// empty set - which is a record, and is why a verdict nothing can move is not re-opened on
// every scan for ever.
//
// The threshold it compared against is the one on ITS OWN ROOT, which is why prof is a
// parameter rather than read off e.Cfg: the same file skips under one root's
// min_bitrate_kbps and proceeds under another's, and a row recording the top-level value
// would name a number no guard ever looked at. by carries the same answer in the form a
// reader of the ledger asks it - which root, and what that root's knobs resolved to.
//
// ts answers the other half, and it answers it for BOTH questions the row holds. It is the
// ATTRIBUTION a reader asks - which named set of overrides supplied the settings this guard
// was decided against, "" when the root's own values stood - and it is where every input an
// encode profile can move is READ, because that is where the guard that read it read it.
// The two are one reading on purpose: an input exists to be compared, and a row recording a
// value the layering for its own path never produces is a row nothing can re-derive.
// props is the source snapshot the guard was decided off, or nil where the verdict was
// reached before one was taken. It supplies the SOURCE's pixel dimensions, which every
// terminal row carries now that a root can band its files by them: a row that did not say
// how tall its source was would leave an operator inferring the band from the verdict. A nil
// snapshot, or one whose dimensions the probe could not establish, records nothing at all
// rather than a zero.
func (e *Engine) because(reason string, by store.Decision, prof config.Profile, ts config.Transcode,
	props *probe.VideoProps, read ...string) *store.Outcome {
	o := &store.Outcome{
		Reason:         reason,
		Decision:       by,
		Profile:        ts.Profile,
		DecisionInputs: e.inputsRead(prof, ts, read...),
	}
	return withSourceDimensions(o, props)
}

// withSourceDimensions records the SOURCE's pixel dimensions on a terminal outcome, from the
// snapshot the decision was taken off.
//
// Nothing is recorded where there is no snapshot or where the probe could not establish both
// dimensions. That is the discipline every measurement on this row keeps: nil is NOT
// RECORDED, and 0 is not a legal pixel dimension for anything, so a zero here would be a
// claim about a file nobody measured - in the one table whose job is to be evidence about
// sources that have since been deleted.
func withSourceDimensions(o *store.Outcome, props *probe.VideoProps) *store.Outcome {
	if props == nil {
		return o
	}
	if w, h, ok := props.Dimensions(); ok {
		o.SourceWidth, o.SourceHeight = ptr(w), ptr(h)
	}
	return o
}

// finalVerdictPrefix precedes the gate's own text on a failure no retry can change. It
// is the operator-facing half of the class: the column says "deterministic" to a
// machine, and this says the same thing to whoever is reading the row.
//
// It states the SCOPE of the finality as well as the fact, because the finality is
// relative to a configuration and not absolute - the same file under a different
// min_savings_percent, a different encoder or a different VMAF floor is a different
// question, and an operator who reads "final" as "this file is hopeless" has been
// misled by a row that meant "final while nothing changes".
const finalVerdictPrefix = "FINAL under this configuration (parked now, not retried; " +
	"a configuration change or a requeue is what revisits it): "

// failureReason composes what a failed row records. A transient failure records the
// error text alone, exactly as every failure always has. A deterministic one records
// that same text WITH the verdict's finality in front of it - never instead of it: an
// operator reading the row has to learn both that it will not be tried again and what
// rejected it, and a row that says only the first sends them to the logs for the second.
func failureReason(class store.FailureClass, text string) string {
	if class.Final() {
		return finalVerdictPrefix + text
	}
	return text
}

// ptr takes the address of a value — the Outcome's numeric fields are pointers so that
// "not recorded" (nil) stays distinct from a recorded zero, and Go has no way to take
// the address of a method call's result without this.
func ptr[T any](v T) *T { return &v }

// logScore renders an optional VMAF score for a log line: the number, or "not recorded"
// when the gate did not run. Handing slog the *float64 directly would print the POINTER
// — `vmaf=0xc000012120` — because slog formats an unknown type with %v and a pointer's
// %v is its address. That would silently gut the one operator-facing line that reports
// what a swap was worth.
func logScore(p *float64) any {
	if p == nil {
		return "not recorded"
	}
	return *p
}

// logSize is logScore for a byte count: the number, or "not recorded" when the stat that
// would have produced it failed. Handing slog the *int64 directly would print the POINTER,
// for the same reason logScore exists.
func logSize(p *int64) any {
	if p == nil {
		return "not recorded"
	}
	return *p
}

// logText is logScore for the string half of the measurement (the comparison pixel
// format, the chroma metric's name). An empty string is NOT RECORDED and says so:
// slog would render "" as an empty value, which reads as a field that exists and is
// blank rather than a measurement that was never taken.
func logText(s string) string {
	if s == "" {
		return "not recorded"
	}
	return s
}

// isAlreadyTargetCodec reports whether a source's probed video codec already IS the
// target codec THIS JOB's effective encoder produces, generalizing the pre-TRANSCODE-6
// hardcoded "already HEVC" check: for an hevc target, "hevc" and its legacy ffprobe
// alias "h265" both count; for an av1 target, "av1" counts. A source already at its own
// job's target is skipped rather than pointlessly re-encoded.
//
// The target is a parameter rather than engine state because `encoder` is resolved twice
// over: one root may re-encode to hevc while another re-encodes to av1, and an encode
// profile may override either for the files its pattern selects. Asking about any other
// job's target would skip every av1 file under an av1 root as already done, and would
// leave a file whose own profile asks for a different codec with nothing to do.
func isAlreadyTargetCodec(target, codec string) bool {
	switch target {
	case "hevc":
		return codec == "hevc" || codec == "h265"
	default:
		return codec == target
	}
}

// carriableVideoStreams reports whether a source's video streams are ones this pipeline
// can honestly carry through an encode: at most one MOVING-picture stream, and it sits at
// v:0, with every other video stream an attached picture.
//
// The rule follows from what the pipeline reads. Codec, bitrate, field order, pixel
// format, the colour tags, the HDR class and both sides of the VMAF comparison all come
// from v:0, so a second moving-picture stream would be re-encoded against a description
// of another stream and would enter no decision at all. A stream shape with no moving
// picture at v:0 - every video stream an attached picture - fails for the same reason
// from the other side: the properties everything downstream derives would be a cover
// image's, so the file is refused rather than encoded on that basis.
//
// A source carrying exactly ONE video stream is every ordinary file and is always
// carriable, attached picture or not: it is the path that existed before this guard, and
// nothing here narrows it.
func carriableVideoStreams(streams []probe.VideoStream) bool {
	if len(streams) <= 1 {
		return true
	}
	if streams[0].AttachedPicture {
		return false
	}
	for _, s := range streams[1:] {
		if !s.AttachedPicture {
			return false
		}
	}
	return true
}

// attachedPictureCopyIndexes returns the VIDEO-RELATIVE indexes (the N of an ffmpeg
// `v:N` specifier) of the attached-picture streams that must be carried through the
// encode UNENCODED, in stream order.
//
// It is empty for a source carrying one video stream, which is the whole of the ordinary
// path: a single-video-stream source gets the identical argv it got before attached
// pictures were handled at all, no per-stream option added. Cover art is a JPEG or PNG,
// and running it through the configured video encoder either fails the mux outright (the
// common case, which then costs max_failures full encodes to rediscover) or succeeds and
// leaves a one-frame HEVC stream where a picture used to be. Copying it is the only
// outcome under which the bytes survive.
func attachedPictureCopyIndexes(streams []probe.VideoStream) []int {
	if len(streams) <= 1 {
		return nil
	}
	var idx []int
	for i, s := range streams {
		if s.AttachedPicture {
			idx = append(idx, i)
		}
	}
	return idx
}

// isTempName reports whether a basename is a transcoder work-in-progress temp.
func isTempName(base string) bool {
	return strings.Contains(base, "."+TempMarker+".")
}

// pickTempPath returns a free temp path from this build's own construction and clears
// any stale temp sitting at it.
//
// The candidate at n == 0 is the name this repo has always used, so the ordinary case
// is unchanged. A candidate is SKIPPED rather than cleared when a file holdfast wrote is
// sitting at it - whether a record says so or the file itself does (strayReplacementHold).
// This is the SECOND route to the same deletion the sweep guards: a source resolved "the
// source is intact", or one simply re-queued after a failed swap, comes back round to
// this function, and the replacement of the failed attempt may be sitting at exactly the
// path it is about to pick. Running out of candidates is a loud failure, never a silent
// walk.
func (e *Engine) pickTempPath(ctx context.Context, dir, stem, ext string) (string, error) {
	for n := 0; n < maxPathCandidates; n++ {
		p := tempPath(dir, stem, ext, n)
		if why, ok := e.heldBack(p); ok {
			e.Log.Info("not using a temp path a record holds back", "path", p, "why", why)
			continue
		}
		if why := e.strayReplacementHold(ctx, p); why != "" {
			e.Log.Info("not using a temp path a file holdfast wrote is sitting at (no record of it survives)", "path", p, "why", why)
			continue
		}
		_ = os.Remove(p) // clear any stale temp for this file
		return p, nil
	}
	return "", fmt.Errorf("no free temp path beside %s after %d candidates", filepath.Join(dir, stem+"."+ext), maxPathCandidates)
}

// errText renders an error for a reason string, or "ok" when there is none, so a
// two-part message reads honestly when only one half failed.
func errText(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

// matchesVideoExt reports whether base has one of the configured video extensions
// (case-insensitive), matching the bash `-iname` scan.
func matchesVideoExt(base string, exts []string) bool {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(base), "."))
	if ext == "" {
		return false
	}
	for _, want := range exts {
		if ext == strings.ToLower(want) {
			return true
		}
	}
	return false
}
