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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
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

	// SkipUndoRetentionFailed is the undo window's own guard (UNDO-6): the original could
	// not be retained, so the swap that would have destroyed it does not run. It is a
	// MUTABLE guard, like the hardlink one, so ProcessFile clears a stale one before the
	// claim rather than parking the file for ever.
	SkipUndoRetentionFailed = "undo-retention-failed"

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

	// staticMetadataIncomplete, when non-nil, replaces hdr.StaticMetadataIncomplete for the
	// HDR10 static-metadata guard. Unexported test seam; production leaves it nil.
	staticMetadataIncomplete func(flatSideData string) bool

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

	// onClaim, when non-nil, is called with a path the instant ProcessFile takes the claim
	// on it - the ONE signal that says a caller got PAST the door rather than being turned
	// away at it. The targeted-submission queue (submit.go) reads it to report "this file is
	// held out by a terminal row" without keeping a second copy of the store's re-opening
	// rule, which is the rule that decides it.
	//
	// Set ONCE, before serving, exactly as Observer is; it is then read from every worker
	// goroutine and never written again. It runs inline on a worker, so it must be
	// non-blocking and concurrency-safe - the same contract Observer carries.
	onClaim func(path string)
}

// EnsureHoldBacks publishes a hold-back snapshot when none has been published yet, so a
// route into ProcessFile that is not a scan still reads the two record-based hold-backs.
//
// It never REPLACES a live snapshot. RunOneshot publishes one per pass and that pass reads
// it from end to end, so overwriting it mid-pass would change what the scan in progress
// holds back - which is exactly what this work may not do. Between scans the snapshot is
// therefore as fresh as the last pass made it, which is the freshness a file enumerated
// late in a long pass already gets; and the residue is the one loadHoldBacks already
// documents, since Claim refuses an indeterminate row outright and a retained replacement
// is held back by its NAME whatever any record says.
func (e *Engine) EnsureHoldBacks(ctx context.Context) {
	if e.held.Load() == nil {
		e.held.CompareAndSwap(nil, e.loadHoldBacks(ctx))
	}
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

// heldBack reports whether a path is one of this run's two record-based hold-backs, and
// why. It is deliberately separate from the record-free name check, so a caller that needs
// both asks for both and it is always obvious which rule fired.
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
func (e *Engine) encode(ctx context.Context, worker, in, out string, props *probe.VideoProps, prof config.Profile) error {
	enc := e.Enc
	if pe, ok := enc.(ProfileEncoder); ok {
		enc = pe.ForProfile(prof)
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
	return &Engine{Cfg: cfg, Probe: p, Enc: enc, Store: st, Log: log, roots: cfg.RootProfiles()}
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
func (e *Engine) RunOneshot(ctx context.Context) error {
	// FILESYSTEM-1: read this run's hold-backs FIRST and publish them before anything
	// walks a root. Every parked job is reported here, naming both of its files, and
	// its two recorded paths - plus every recorded replacement path still carrying a
	// live exclusion - are withheld from the sweep, from the scan and from the workers.
	e.held.Store(e.loadHoldBacks(ctx))

	// S0085: the unpreserved-ownership notice is owed once per RUN, so a daemon scanning
	// every scan_interval_sec says it again on each pass.
	e.ownershipNoticeGiven.Store(false)

	// This pass's listings, taken here and used by the sweep and the enumeration between
	// them, so every covered directory is listed exactly once for the whole pass: the
	// startup walk's own where this is the first scan after that walk, this scan's where not.
	pass := e.passListings()

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
	e.sweepStaleTemps(ctx, pass)
	// The configured working location is swept here and not by sweepStaleTemps, and
	// it is the one sweep that does NOT read this pass's listings: the scratch
	// directory is not under a library root, so it is outside the coverage bound
	// those listings are taken over and nothing in `pass` can describe it. It lists
	// itself, once, under the same construction and the same hold-back exceptions -
	// see cleanScratch.
	e.cleanScratch(ctx)
	observed, err := e.scanOnce(ctx, pass)
	if err != nil {
		return err
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

// scanOnce walks the roots, sorts matches for deterministic order, and fans them out to a
// pool of workers (Cfg.EffectiveWorkers(), minimum 1). A cancelled ctx stops workers pulling
// new files and kills the in-flight ffmpeg subprocess through exec.CommandContext.
//
// It returns the set of directories this scan LISTED SUCCESSFULLY - the only places this run
// has evidence about, and therefore the only places the retention pass may read a missing
// file as a file that is gone (see rowIsSpent). It is the enumeration's own record rather
// than a re-derivation, so the two cannot disagree about where holdfast looked.
func (e *Engine) scanOnce(ctx context.Context, pass *listings) (map[string]bool, error) {
	files, observed := e.enumerateIn(pass)

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
				if ctx.Err() != nil {
					return
				}
				if err := e.ProcessFile(ctx, workerID, f); err != nil {
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						mu.Lock()
						if firstCancelErr == nil {
							firstCancelErr = err
						}
						mu.Unlock()
						return
					}
					// A per-file error is logged and recorded inside ProcessFile and never
					// takes down the scan; a non-context error here is unexpected.
					e.Log.Warn("process file error (continuing)", "file", f, "err", err)
				}
			}
		}(workerID)
	}

feed:
	for _, f := range files {
		// Pause control: stop handing out NEW files the moment we are paused. Checked
		// before the send, so pause only ever DELAYS work and never touches an in-flight
		// encode; the not-yet-fed files stay pending for the next scan after resume.
		if e.Paused != nil && e.Paused() {
			e.Log.Info("paused — stopping feed of new files; in-flight encodes finish safely")
			break feed
		}
		select {
		case <-ctx.Done():
			break feed
		case ch <- f:
		}
	}
	close(ch)
	wg.Wait()

	if firstCancelErr != nil {
		return observed, firstCancelErr
	}
	return observed, ctx.Err()
}

// enumerate returns every source this run may act on, sorted for a deterministic order, and
// the set of directories it LISTED SUCCESSFULLY to find them.
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

// enumerateIn is the enumeration as a pass runs it, over the listings that pass
// holds: the startup walk's where this is the first scan after that walk, the
// sweep's where the sweep already listed for this one, and its own otherwise. It
// RELEASES each directory's entries as it consumes them, so the entry
// information a walk collected does not outlive the scan that used it.
func (e *Engine) enumerateIn(pass *listings) ([]string, map[string]bool) {
	var files []string
	observed := map[string]bool{}
	if e.Coverage != nil {
		for _, dir := range e.Coverage {
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
				continue
			}
			observed[dir] = true
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
					if p := filepath.Join(dir, ent.Name); e.offered(p) {
						files = append(files, p)
					}
				}
			}
		}
		if pass.selfListed > 0 {
			// Entry information that was never collected is never evidence: where
			// none was carried in, this scan listed for itself and says so.
			e.Log.Info("this scan listed covered directories itself; the startup walk's own listings serve the first scan after that walk and no later one",
				"directories_this_scan_listed", pass.selfListed, "directories_covered", len(e.Coverage))
		}
		sort.Strings(files)
		return files, observed
	}
	for _, root := range e.Cfg.LibraryRoots {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				// WalkDir reports a directory it could not read by calling back a SECOND
				// time for it, carrying the error. Withdraw what was marked on the way in:
				// a listing that failed is not one this run may conclude from.
				if d != nil && d.IsDir() {
					delete(observed, path)
				}
				return nil
			}
			if d.IsDir() {
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
				if e.offered(path) {
					files = append(files, path)
				}
			}
			return nil
		})
	}
	sort.Strings(files)
	return files, observed
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
// before Claim but its RecordSkip/ClearSkip is a report-only write that never claims the
// file, so it cannot let two workers encode one source.
func (e *Engine) ProcessFile(ctx context.Context, worker, f string) error {
	fi, err := os.Stat(f)
	if err != nil || fi.IsDir() {
		return nil
	}

	// Hold-backs, re-checked here rather than trusted to the scan: ProcessFile is exported
	// and is the only door into the encode/swap pipeline, so the rule belongs on the door.
	if IsRetainedReplacementName(filepath.Base(f)) {
		return nil
	}
	if why, ok := e.heldBack(f); ok {
		e.Log.Info("not processing (held back)", "file", f, "why", why)
		return nil
	}

	// A path containing a literal tab or newline is pathological: skip it, unrecorded.
	// A row keyed on such a path would be legal SQL and worth nothing.
	if strings.ContainsAny(f, "\t\n") {
		e.Log.Info("skip (path contains a tab/newline — unsupported)", "file", f)
		return nil
	}

	// WHICH PROFILE DECIDES THIS FILE, resolved ONCE, here, from the root it was enumerated
	// under, and threaded through everything below: the hardlink guard, the bitrate floor,
	// the pixel-format derivation, the output container, the encode's argv and every gate in
	// verifyOutput. Re-deriving it at each use would let a file be guarded by one root's
	// floor and encoded at another root's crf.
	root, rooted := e.rootFor(f)
	prof := root.Profile
	by := decidedBy(root, rooted)

	key := probe.Fingerprint(f)

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

	// The undo window's own guard is MUTABLE (UNDO-6): a retention that could not be taken
	// is a condition that gets fixed, so a stale skip from a previous scan is dropped here
	// and the file re-enters the normal path. Not gated on the window still being enabled,
	// for the reason the release sweep is not: with the window off no retention is attempted
	// at all, so a row left over from when it was on would park that file indefinitely.
	if err := e.Store.ClearSkip(ctx, f, key, SkipUndoRetentionFailed); err != nil {
		e.Log.Warn("clear stale undo-retention skip failed (continuing)", "file", f, "err", err)
	}

	// Hardlink guard. A file with >1 hard link is almost always an *arr import that is also
	// an active seed, and replacing it via rename breaks the link, reclaiming no space and
	// silently breaking the seed. The skip is RECORDED as "hardlinked" so an operator sees
	// WHICH guard fired, and the link count is MUTABLE: RecordSkip only writes where no real
	// outcome exists, and the else-branch's ClearSkip removes the stale row once the seed
	// finishes.
	//
	// A link THIS TOOL holds is discounted (UNDO-6): the undo window takes a second link
	// before the rename, so a run interrupted there would otherwise park the very file the
	// window was protecting. Discounting is proved per link (same inode, live retention
	// record), never assumed from the count, so a foreign extra link still skips.
	if prof.HardlinkSkip() {
		if links := probe.NLink(f); links > 1 && links > 1+e.retainedLinks(ctx, f, key) {
			e.Log.Info("skip (hardlinked — swap would break a seed and reclaim nothing)", "file", f, "links", links)
			changed, err := e.Store.RecordSkip(ctx, f, key, SkipHardlinked, by, ts.Profile)
			if err != nil {
				// Fail safe: recording the skip is a reporting nicety, never the decision,
				// so a store hiccup still skips the file.
				e.Log.Warn("record hardlink skip failed (still skipping the file)", "file", f, "err", err)
			} else if changed {
				// Emit only when the skip was newly recorded, so a live client sees it once
				// rather than once per scan for the lifetime of the seed.
				e.emit(Event{Path: f, Status: store.Skipped, Outcome: e.because(SkipHardlinked, by, prof, ts)})
			}
			return nil
		}
		// Not (or no longer) hardlinked: drop any stale "hardlinked" skip from a previous
		// scan so the file re-enters the Claim path. A no-op if it was never hardlinked.
		if err := e.Store.ClearSkip(ctx, f, key, SkipHardlinked); err != nil {
			e.Log.Warn("clear stale hardlink skip failed (continuing)", "file", f, "err", err)
		}
	}

	// Claim: the resume short-circuit AND the cross-worker mutual-exclusion guard in one
	// atomic call. done/skipped hold the file out for as long as the decision inputs they
	// recorded still match what is handed in here and are RE-OPENED when they do not; failed
	// is retryable up to MaxFailures, since a transient ENOSPC must not exclude a file for
	// ever; active means another worker holds it, or it is stale and awaits RecoverStale.
	claimed, err := e.Store.Claim(ctx, f, key, worker, e.Cfg.MaxFailures, e.inputsFor(prof))
	if err != nil {
		// Fail safe: a store error must never be treated as "done". Log and skip
		// this pass; the file is retried on the next scan once the store recovers.
		e.Log.Warn("claim error (skipping this pass, will retry)", "file", f, "err", err)
		return nil
	}
	if !claimed {
		return nil
	}
	if e.onClaim != nil {
		e.onClaim(f)
	}
	// The claim moved this row to probing: surface it as a live "started" signal carrying
	// the worker, so the UI shows the file entering the pipeline immediately.
	e.emit(Event{Path: f, Status: store.Probing, Worker: worker})

	// Symlink guard (TRANSCODE-16). A symlink has nlink == 1 and slips past the hardlink
	// guard, and config.Validate refuses a symlinked ROOT but not a symlinked file within
	// the tree. The swap would replace the LINK itself with a regular file, orphaning the
	// real target and changing what the library entry means. Resolving and transcoding the
	// target is a deliberate non-goal, so a symlinked source is SKIPPED.
	if probe.IsSymlink(f) {
		e.Log.Info("skip (symlinked source — swap would replace the link, orphaning its target)", "file", f)
		e.finish(ctx, f, key, store.Skipped, e.because(SkipSymlink, by, prof, ts))
		return nil
	}

	// One probe snapshot of the source, shared by every skip guard below AND handed to the
	// encoder (TRANSCODE-PERF), in place of the ~15 separate ffprobe/ffmpeg processes a
	// single encode-bound file used to spawn. Reading every guard off one snapshot is more
	// self-consistent than re-probing a file mid-pipeline. The costly whole-file checks are
	// NOT here: they run in verifyOutput against the encoded temp.
	props := e.Probe.VideoProps(ctx, f)

	codec := props.Codec()
	if codec == "" {
		e.Log.Info("skip (unreadable / no video stream)", "file", f)
		e.finish(ctx, f, key, store.Failed, e.because(FailUnreadable, by, prof, ts))
		return nil
	}
	if isAlreadyTargetCodec(targetCodec, codec) {
		e.Log.Info("skip (already at target codec)", "file", f, "codec", codec,
			"target", targetCodec, "library_root", root.Clean, "encode_profile", ts.Profile)
		e.finish(ctx, f, key, store.Skipped, e.because(SkipAlreadyTargetCodec, by, prof, ts, InputTargetCodec))
		return nil
	}

	// The bitrate floor is the ROOT's and not this job's: an encode profile may change
	// what the encoder produces and may not move a gate that decides whether a source is
	// destroyed. Same for every threshold below.
	if br := props.BitrateKbps(); br > 0 && br < prof.MinBitrateKbps {
		e.Log.Info("skip (low bitrate)", "file", f, "kbps", br, "min", prof.MinBitrateKbps, "library_root", root.Clean)
		e.finish(ctx, f, key, store.Skipped, e.because(SkipLowBitrate, by, prof, ts, InputMinBitrateKbps))
		return nil
	}

	// Interlace guard. This tool never deinterlaces — re-encoding an interlaced
	// source with a progressive-assuming pipeline bakes in combing artifacts
	// permanently. Progressive or unknown field_order proceeds.
	switch props.FieldOrder() {
	case "tt", "bb", "tb", "bt":
		e.Log.Info("skip (interlaced — not deinterlacing)", "file", f)
		e.finish(ctx, f, key, store.Skipped, e.because(SkipInterlaced, by, prof, ts))
		return nil
	}

	// HDR/DV guard (TRANSCODE-3). A generic libx265 re-encode cannot preserve a Dolby Vision
	// RPU or HDR10+ dynamic metadata and would SILENTLY strip it, a permanent,
	// invisible-until-viewed loss, so detect and SKIP. HDR10 STATIC metadata IS carried
	// through the encode (hdr.DeriveColorArgs). Probed only here, on an encode-bound file,
	// so the cost falls on the minority actually re-encoded.
	switch hdr.ClassFrom(props.CodecTag(), props.SideData(), props.Color("color_transfer")) {
	case hdr.ClassDV:
		e.Log.Info("skip (Dolby Vision — RPU cannot survive a generic re-encode)", "file", f)
		e.finish(ctx, f, key, store.Skipped, e.because(SkipDolbyVision, by, prof, ts))
		return nil
	case hdr.ClassHDR10Plus:
		e.Log.Info("skip (HDR10+ dynamic metadata — cannot survive a generic re-encode)", "file", f)
		e.finish(ctx, f, key, store.Skipped, e.because(SkipHDR10Plus, by, prof, ts))
		return nil
	case hdr.ClassHDR10:
		// HDR10 static metadata IS carried through the encode, but a mastering-display or
		// content-light block this build cannot fully parse would be silently dropped.
		// Fail safe: SKIP rather than blind-encode.
		incomplete := e.staticMetadataIncomplete
		if incomplete == nil {
			incomplete = hdr.StaticMetadataIncomplete
		}
		if incomplete(props.FrameSideData()) {
			e.Log.Info("skip (HDR10 static metadata present but incomplete/unparseable — refusing to re-encode and drop it)", "file", f)
			e.finish(ctx, f, key, store.Skipped, e.because(SkipIncompleteHDRMetadata, by, prof, ts))
			return nil
		}
	}

	// Chroma/bit-depth guard. Preserve the source's chroma subsampling and floor bit-depth
	// at 10; an exotic pix_fmt is SKIPPED rather than silently subsampled or guessed. A
	// forced (non-"auto") PixelFormat bypasses derivation entirely.
	if ts.PixelFormatAuto() {
		srcPixFmt := props.PixFmt()
		if _, ok := hdr.DerivePixFmt(srcPixFmt); !ok {
			e.Log.Info("skip (unrecognized/exotic pixel format — refusing to silently subsample)", "file", f, "pix_fmt", srcPixFmt)
			e.finish(ctx, f, key, store.Skipped, e.because(SkipExoticPixelFormat, by, prof, ts, InputPixelFormat))
			return nil
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
		e.Log.Info("skip (a video stream beyond the first that is not an attached picture, or a stream shape the probe could not establish)",
			"file", f, "video_streams", len(streams), "probe_established", established)
		e.finish(ctx, f, key, store.Skipped, e.because(SkipMultiVideoStream, by, prof, ts))
		return nil
	}

	// Output container: "source"/"auto" (default) matches the SOURCE file's own extension,
	// so a stream type that does not round-trip through a different container (MP4 mov_text
	// into MKV) is not forced to change. A forced ContainerExt overrides this.
	outExt := ts.ContainerExt
	if ts.ContainerMatchesSource() {
		outExt = strings.TrimPrefix(filepath.Ext(f), ".")
	}

	dir := filepath.Dir(f)
	base := filepath.Base(f)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	// tmp is local to this call (and thus to this worker) — TRANSCODE-5 runs N
	// workers concurrently, each on a different source. Correctness does not depend on
	// tracking it in the Engine: every failure path below removes tmp directly, and a
	// ctx-cancel leaves it orphaned for cleanStaleTemps to sweep on the next startup. The
	// path is the build's own construction (see swap.go).
	final := filepath.Join(dir, stem+"."+outExt)

	// Collision guard. When the container ext changes, final is a DIFFERENT path than the
	// source, and a distinct file already there would be silently overwritten before the
	// source was deleted: two files destroyed. Refuse.
	if final != f {
		if _, err := os.Lstat(final); err == nil {
			e.Log.Info("skip (target already exists as a distinct file — refusing to clobber)", "file", f, "target", final)
			e.finish(ctx, f, key, store.Skipped, e.because(SkipTargetExists, by, prof, ts, InputContainerExt))
			return nil
		}
	}

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
		out := &store.Outcome{
			SourceCodec:    codec,
			Profile:        ts.Profile,
			Decision:       by,
			DecisionInputs: e.inputsRead(prof, InputTargetCodec, InputEncoder, InputCRF, InputPreset),
		}
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
			e.finish(ctx, f, key, store.Failed, &store.Outcome{Reason: err.Error(), Profile: ts.Profile, Decision: by})
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
			e.finish(ctx, f, key, store.Failed, &store.Outcome{Reason: err.Error(), Profile: ts.Profile, Decision: by})
			return nil
		}
		w, err := e.pickScratchPath(scratch, f, outExt)
		if err != nil {
			e.Log.Warn("FAIL (no free working path in the scratch directory, source untouched)", "file", f, "err", err)
			e.finish(ctx, f, key, store.Failed, &store.Outcome{Reason: err.Error(), Profile: ts.Profile, Decision: by})
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
	out := &store.Outcome{Encoder: ts.Encoder, Profile: ts.Profile, Decision: by}

	encStart := time.Now()
	if err := e.encode(ctx, worker, f, work, props, prof); err != nil {
		if ctx.Err() != nil { // interrupted: discard temp, DON'T finish — leave active for RecoverStale
			_ = os.Remove(tmp)
			return ctx.Err()
		}
		e.Log.Warn("FAIL (encode error, source untouched)", "file", f, "err", err)
		_ = os.Remove(tmp)
		out.Reason = err.Error() // the failure error — previously computed and dropped
		e.finish(ctx, f, key, store.Failed, out)
		return nil
	}
	encodeDur := time.Since(encStart)
	out.EncodeMs = ptr(encodeDur.Milliseconds())

	e.advance(ctx, f, key, store.Verifying)
	proof, class, reason := e.verifyOutput(ctx, f, work, prof, targetCodec)
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
	if reason != nil {
		if ctx.Err() != nil {
			_ = os.Remove(tmp)
			return ctx.Err()
		}
		e.Log.Warn("FAIL (verify rejected, source untouched)", "file", f,
			"reason", reason.Error(), "failure_class", class.Class())
		_ = os.Remove(tmp)
		// The gate decided both of these at the line that rejected the encode; nothing here
		// re-reads the message to work out what kind of rejection it was.
		out.FailureClass = class
		out.Reason = failureReason(class, reason.Error())
		e.finish(ctx, f, key, store.Failed, out)
		return nil
	}

	// Re-check the collision guard right before the swap: an encode can take hours, and a
	// distinct file that appeared at `final` in that window is never overwritten.
	if final != f {
		if _, err := os.Lstat(final); err == nil {
			e.Log.Warn("FAIL (target appeared during encode — refusing to clobber)", "file", f, "target", final)
			_ = os.Remove(tmp)
			out.Reason = "target appeared during encode — refused to clobber " + final
			e.finish(ctx, f, key, store.Failed, out)
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
			e.finish(ctx, f, key, store.Failed, out)
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
			e.finish(ctx, f, key, store.Failed, out)
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
		e.finish(ctx, f, key, store.Failed, out)
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
		e.finish(ctx, f, key, store.Failed, out)
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
			e.finish(ctx, f, key, store.Skipped, e.because(SkipUndoRetentionFailed, by, prof, ts))
			return nil
		}
		retained = r
		if e.hookAfterRetain != nil {
			if herr := e.hookAfterRetain(retained); herr != nil {
				u.discard(retained)
				_ = os.Remove(tmp)
				out.Reason = "aborted after retaining the original: " + herr.Error()
				e.finish(ctx, f, key, store.Failed, out)
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
		e.finish(ctx, f, key, store.Failed, out)
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
		e.finish(ctx, f, key, store.Failed, out)
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
		"vmaf_chroma", logScore(proof.ChromaMin), "vmaf_chroma_metric", logText(proof.ChromaMetric))
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
	if _, err := e.Store.Claim(ctx, final, finalKey, worker, e.Cfg.MaxFailures, e.inputsFor(prof)); err != nil {
		e.Log.Warn("claim of final key failed (done outcome still applies on disk)", "file", final, "err", err)
	}
	// What this encode was taken under: the codec it targeted and the three settings
	// that decided what came out. It is recorded HERE, on the done row, because that row
	// is the permanent answer about the replacement - and the moment any of the four
	// moves, the answer is one this build would no longer give, so the next scan offers
	// the file back to the guards rather than skipping it for ever.
	out.DecisionInputs = e.inputsRead(prof, InputTargetCodec, InputEncoder, InputCRF, InputPreset)
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
	}
}

// finish records a terminal outcome AND emits an event carrying the SAME proof — used
// for the skipped/failed terminal transitions (the Done swap path uses finishStore +
// its own rich emit, so Done is emitted exactly once).
func (e *Engine) finish(ctx context.Context, path, key string, s store.Status, o *store.Outcome) {
	e.finishStore(ctx, path, key, s, o)
	e.emit(Event{Path: path, Status: s, Outcome: o})
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
// ts is there for a DIFFERENT question and the two are deliberately not merged. An input
// is compared against the configuration in force to decide whether to re-open the row,
// so it has to be read where that comparison reads it - the root's profile, which is the
// unit Claim and the ledger survey both ask about. The encode profile's name is the
// ATTRIBUTION the ledger owes beside it: which named set of overrides supplied the
// settings this guard was decided against, "" when the root's own values stood, which is
// every row a configuration without encode_profiles can produce.
func (e *Engine) because(reason string, by store.Decision, prof config.Profile, ts config.Transcode, read ...string) *store.Outcome {
	return &store.Outcome{
		Reason:         reason,
		Decision:       by,
		Profile:        ts.Profile,
		DecisionInputs: e.inputsRead(prof, read...),
	}
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
