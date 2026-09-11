// Package engine is the data-safety core of the transcoder — a faithful Go port of
// the bash orchestrator (media/transcoder/transcode.sh). Its whole purpose is the
// invariant the design defends: NEVER destroy a source until a replacement is
// proven good. The only filesystem mutation is an atomic same-directory rename that
// runs solely after the output passes every gate in verifyOutput; any failure
// discards the temp and leaves the source byte-for-byte untouched.
//
// Scope (TRANSCODE-1): the structural safety contract + the CPU libx265 encode +
// the resumable ledger, oneshot. Colour/HDR (TRANSCODE-3), VMAF (TRANSCODE-4), and
// the SQLite queue + worker pool (TRANSCODE-5, this file) build on this without
// weakening the invariant. Hardware codecs (TRANSCODE-6) are next.
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
// The SKIP reasons are a closed, stable VOCABULARY, not prose: an operator seeing the
// bare word "skipped" has to go and read the logs to find out which of eight guards
// fired, and a UI cannot key off a sentence. These tokens are the answer to "which
// guard", so treat them as a wire format — add to them freely, but renaming one
// changes what a stored row means.
//
// FAILURE reasons are NOT in this set, with one exception: a failure's reason is the
// error text itself (the encode error, or the gate that rejected the output), because
// unlike a guard it is not drawn from a fixed set and the detail is the whole value.
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

	// SkipUndoRetentionFailed is the undo window's own guard (UNDO-6): the original
	// could not be retained, so the swap that would have destroyed it does not run.
	// It is a MUTABLE guard, like the hardlink one - a full disk or an unwritable
	// retention area is fixed and the next scan reclaims the file - so ProcessFile
	// clears a stale one before the claim rather than parking the file for ever.
	SkipUndoRetentionFailed = "undo-retention-failed"

	// SkipRestoredOriginal marks a file an operator has deliberately put back through
	// the undo window. It is NOT mutable: the next scan must not re-encode a file
	// somebody just rescued, through the very gates that passed the encode they
	// rejected. Changing the file clears it, because that is a new fingerprint and a
	// new row. A configuration change does not clear it either, and neither does a
	// requeue - which is why it is the one token internal/store also has to know by
	// name, and why this is defined AS that constant rather than beside it: a wire
	// format with two spellings is a wire format with a silent fork in it.
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

	// targetCodec is what ffprobe should report codec_name as for a SUCCESSFUL
	// output — "hevc" for the cpu/nvenc/qsv/vaapi/amf encoders, "av1" for
	// svtav1/av1_nvenc. Set in New from encoder.Lookup(cfg.Encoder).TargetCodec,
	// defaulting "hevc" for an unknown/empty key (Validate rejects an unknown
	// encoder before the engine is ever built, so this default is a defensive
	// fallback, not a real code path). Drives the skip-already-target guard
	// (ProcessFile) and the output-codec check (verifyOutput) — TRANSCODE-6
	// generalizes both away from a hardcoded "hevc".
	targetCodec string

	// staticMetadataIncomplete, when non-nil, replaces hdr.StaticMetadataIncomplete
	// for the HDR10 static-metadata guard. Unexported test seam (mirrors the bash
	// suite's TRANSCODER_TEST_HOOKS; the engine tests are in this package) —
	// production leaves it nil and uses the real predicate.
	staticMetadataIncomplete func(flatSideData string) bool

	// vmafScore, when non-nil, replaces the real libvmaf measurement in the VMAF
	// gate. Unexported test seam — lets a test force a low score or an unavailable-
	// libvmaf error without a second real encode. Production leaves it nil. It
	// receives the whole vmaf.Request, comparison pixel format included, so a test
	// can assert on the format the gate NAMED as well as on what it did with the
	// numbers that came back.
	vmafScore func(ctx context.Context, req vmaf.Request) (vmaf.Result, error)

	// fsyncPath, when non-nil, replaces the real fsync in the durable swap
	// (TRANSCODE-17). A test uses it to observe or force-fail the fsync of the temp
	// (before the rename) or the parent directory (after it), proving the durability
	// discipline is present and fails safe on an fsync error — the code discipline is
	// testable even though power-loss durability itself is not (that needs a power-cut
	// harness; see the "Durability" comment on the swap). Production leaves it nil and
	// fsyncs for real via the package-level fsyncPath.
	fsyncPath func(path string) error

	// hookAfterRename, when non-nil, is called immediately after a successful
	// ext-CHANGING rename (final != f) and BEFORE the now-orphaned source is removed.
	// Production leaves it nil; it exists only so a test can simulate a crash in that
	// exact window (TRANSCODE-16 fixture c) through the REAL swap path — the only way
	// to prove the two-step rename+delete fails safe. When it returns a non-nil error,
	// ProcessFile aborts before the delete, exactly as a crashed process would, leaving
	// BOTH files on disk (a duplicate, never a loss) for the next scan to reconcile.
	hookAfterRename func() error

	// hookBeforeDryRunRecord, when non-nil, is called immediately before a dry run's
	// decision is recorded, carrying the path it is about. Production leaves it nil; it
	// exists only so a test can drive the one condition under which the source size read
	// can fail on a file that has already passed every guard - the file going away in
	// that window - and prove the decision is still recorded with the size NOT RECORDED
	// rather than fabricated as a zero.
	hookBeforeDryRunRecord func(path string)

	// hookAfterRetain, when non-nil, is called immediately after the undo window has
	// taken its second link to the source and BEFORE the re-fingerprint and the
	// rename. Production leaves it nil. It exists so a test can observe the state
	// that only exists in that window - the retained original and the source being
	// the same inode, and the filesystem having grown by nothing - which is the whole
	// of the claim that retention costs no space. Returning a non-nil error aborts
	// the swap exactly as a failure there would, so it also drives the abandon path.
	hookAfterRetain func(retained string) error

	// undoNow, when non-nil, replaces the clock the undo window reads. Production
	// leaves it nil. A test uses it to place a retention's expiry in the past, so the
	// release sweep is exercised for real rather than by rewriting a ledger row.
	undoNow func() time.Time

	// Observer, when non-nil, receives an Event on every job-state transition
	// (TRANSCODE-7's API/SSE hub subscribes here). It is a fire-and-forget
	// NOTIFICATION beside the store writes — never a substitute for them and never
	// on the critical path: emit calls it directly, so the Observer contract
	// requires it to be non-blocking and concurrency-safe (see Observer). nil =
	// no emission, the pre-TRANSCODE-7 behaviour.
	Observer Observer

	// Coverage, when non-nil, is the set of directories the STARTUP WALK
	// traversed successfully (FILESYSTEM-1), and it BOUNDS this run: a source is
	// enumerated from exactly these directories and from nowhere else, so a
	// subtree the walk declined to re-enter, could not read, or failed to
	// traverse contributes no file to this run and no swap can happen under it.
	// It is also what makes the scan terminate over a bind-mount loop, which the
	// startup walk cut by region and a plain recursive walk would follow for
	// ever.
	//
	// nil restores the pre-check behaviour of walking the roots directly, which
	// is what an Engine built without the startup check (the engine's own tests)
	// gets.
	//
	// Set it with SetCoverage to carry the walk's own listings across with it;
	// assigning the field alone is a coverage set with no entry information, and
	// the scan then lists those directories itself.
	Coverage []string

	// carried is the entry information one startup walk collected, waiting for
	// the FIRST scan after that walk (see listings.go). RunOneshot takes it -
	// atomically, so a second scan racing it gets nothing rather than a shared
	// half-consumed map - and it is gone once that scan has used it.
	carried atomic.Pointer[listings]

	// readDirFn, when non-nil, replaces the directory listing the
	// coverage-bounded pass makes. Production leaves it nil. It is a seam in its
	// own right because what a pass COSTS is a property nothing else can
	// observe: a test substitutes it to count the listings a whole run issues,
	// which is how "each covered directory is listed once" is asserted rather
	// than assumed.
	readDirFn func(dir string) ([]os.DirEntry, error)

	// Paused, when non-nil and returning true, tells scanOnce to stop feeding NEW
	// files to workers this pass (TRANSCODE-7's pause control). It is checked
	// between files only — an in-flight encode is NEVER interrupted (that would
	// risk the invariant); paused work is simply left pending for the next scan
	// after resume. nil = never paused.
	Paused func() bool

	// --- the three FILESYSTEM-1 seams --------------------------------------------
	//
	// The gate has no network mount and no second real filesystem, and it never will:
	// a CI runner cannot conjure an NFS server, and a test that needed one would be
	// skipped, which for a data-safety proof is a false green. These three
	// substitutions are what make the whole failed-swap contract testable on ordinary
	// local storage. Production leaves all three nil and uses the real thing.

	// fsLookup, when non-nil, replaces the real filesystem-type lookup. A test uses it
	// to report "ext4", "nfs", a type string this build does not recognise, or an
	// error. It supplies a type NAME only: the enumeration still decides the class, so
	// a suite built entirely on substituted lookups still reds against a build whose
	// recognised-local set was emptied.
	fsLookup fsclass.Lookup

	// renameFn, when non-nil, replaces os.Rename in the swap. A test uses it to inject
	// a failure - optionally one that nonetheless APPLIES the rename, which is the
	// shape rename(2) describes for NFS and the single most important case here.
	renameFn func(oldpath, newpath string) error

	// restatFn, when non-nil, replaces the re-stat that follows a failed rename. It is
	// a seam in its own right because one scenario cannot be produced by the other two:
	// a rename that genuinely took effect, whose re-stat nonetheless returns the
	// SOURCE's pre-swap attributes, as a client attribute cache populated before the
	// swap would. No substitute for the lookup or for the rename can make a real stat
	// lie about a real file.
	restatFn func(path string) (probe.Attributes, error)

	// held is the current run's hold-back snapshot, published once by RunOneshot
	// before any worker starts and only read afterwards. An atomic pointer rather than
	// a plain field so a second RunOneshot (the serve loop's scan racing an operator's
	// rescan) can never be observed mid-write.
	held atomic.Pointer[holdBacks]
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

// heldBack reports whether a path is one of this run's two record-based hold-backs,
// and why. It is deliberately separate from the record-free name check: a caller that
// needs both asks for both, so it is always obvious which rule fired.
func (e *Engine) heldBack(p string) (string, bool) { return e.held.Load().held(p) }

// emit delivers ev to the Observer if one is set. It is deliberately trivial and
// must stay cheap + non-blocking: it runs inline on a worker goroutine, so the
// Observer (not the engine) owns any buffering/decoupling from slow consumers.
func (e *Engine) emit(ev Event) {
	if e.Observer != nil {
		e.Observer(ev)
	}
}

// progressEmitInterval throttles live progress reporting to at most one event per job
// per interval (S0030). ffmpeg's default -stats_period is 0.5s and every event costs the
// reporting hub a full store-derived snapshot rebuild, so an unthrottled two-hour film
// would drive thousands of them on the same machine that is doing the encoding. Dropping
// an update is granularity lost, never correctness lost — the same trade the hub already
// makes when it coalesces events and drops frames for a slow subscriber.
const progressEmitInterval = time.Second

// encode runs the configured Encoder over one file, collecting live progress when there
// is both an Encoder that can report it and an Observer to receive it. Anything else
// falls straight through to Encode — an Encoder with no progress capability is not a
// failure, it is a job whose progress is simply never reported, which is a state the
// reporting surface already has to handle.
func (e *Engine) encode(ctx context.Context, worker, in, out string, props *probe.VideoProps) error {
	pe, ok := e.Enc.(ProgressEncoder)
	if !ok || e.Observer == nil {
		return e.Enc.Encode(ctx, in, out, props)
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

// fsyncPath opens path and fsyncs it. For a regular file this forces its data blocks
// to disk; for a DIRECTORY it forces the directory's name entries to disk — which is
// what makes a rename() into (or a remove() from) that directory survive a power loss,
// not merely a clean crash. os.Rename is atomic w.r.t. a concurrent reader, but POSIX
// does not make the rename PERSISTENT until the containing directory is fsync'd. On
// Linux fsync of an O_RDONLY fd flushes both a file's dirty pages and a directory's
// entries, so a read-only open suffices for both. The Sync error is the one that
// matters (it is what reports a failed flush); a Close error on an fd we only read is
// surfaced only when Sync itself succeeded.
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
	return &Engine{Cfg: cfg, Probe: p, Enc: enc, Store: st, Log: log, targetCodec: targetCodecFor(cfg)}
}

// RunOneshot discards orphaned temps from any prior killed run, resets any job left
// active by a prior crashed run back to pending, then scans every library root once
// and fans the discovered files out to a pool of workers. It is the TRANSCODE-1
// (sequential) / TRANSCODE-5 (worker pool) entrypoint. A cancelled ctx (e.g.
// SIGTERM) stops workers from picking up new files and lets the in-flight encode's
// subprocess be killed via ctx (CommandContext); the temp it was writing is orphaned
// but never swapped in, and cleanStaleTemps sweeps it on the next startup — the
// source is untouched either way.
func (e *Engine) RunOneshot(ctx context.Context) error {
	// FILESYSTEM-1: read this run's hold-backs FIRST and publish them before anything
	// walks a root. Every parked job is reported here, naming both of its files, and
	// its two recorded paths - plus every recorded replacement path still carrying a
	// live exclusion - are withheld from the sweep, from the scan and from the workers.
	e.held.Store(e.loadHoldBacks(ctx))

	// This pass's listings, taken here and used by the sweep and the enumeration
	// between them, so every covered directory is listed exactly once for the
	// whole pass: the startup walk's own listings where this is the first scan
	// after that walk, and this scan's where it is not.
	pass := e.passListings()

	if _, err := e.Store.RecoverStale(ctx); err != nil {
		// Fail safe: if we can't tell what was left active by a prior crash, log and
		// continue — a stuck "active" row just means that one file is skipped this
		// pass (Claim treats it as held), never a false completion. It will be
		// picked up once the store is healthy again.
		e.Log.Warn("recover stale jobs failed (continuing)", "err", err)
	}
	// The undo window closes here (UNDO-6), at the START of the pass and before
	// anything is encoded: a retention whose window has passed is released, and the
	// space that release actually returned is reported. Doing it first means the
	// figure an operator sees for this pass covers the whole pass, and that the disk
	// this run is about to write to has already had back whatever the last one held.
	//
	// It is deliberately NOT gated on undo_window_hours still being non-zero. Each
	// retention carries the expiry it was GIVEN, so releasing is a promise this tool
	// already made about bytes it is already holding, and setting the key back to 0
	// is the documented way to stop paying for the window, so gating the sweep on it
	// would make that setting strand every original it had retained: the second link
	// on disk for ever, the row live for ever, the space never returned. The setting
	// governs whether a NEW retention is taken, nothing else.
	e.undo().ReleaseExpired(ctx)
	e.sweepStaleTemps(ctx, pass)
	observed, err := e.scanOnce(ctx, pass)
	if err != nil {
		return err
	}
	e.enforceRetention(ctx, observed)
	return nil
}

// enforceRetention brings the ledger back within Cfg.HistoryRetentionRows, and it is the
// ONLY caller of the prune - which is what makes the bound hold with no operator action
// and no request to the API, on `run` and on every scan `serve` starts alike.
//
// It runs AFTER the scan, never during it. A prune competes for the single serialized
// store connection the engine writes every Claim/Advance/Finish through, and the scan is
// the thing that must not be slowed; it also means the rows this pass just wrote are the
// newest ones, so "the oldest beyond the retention" is evaluated against a complete
// picture rather than a half-finished one.
//
// A failure here is LOGGED and survivable, deliberately: the prune is bookkeeping about
// history, and history is not the reason this process exists. A store that cannot be
// pruned must not stop the daemon serving, and must not stop the next scan encoding - so
// the error goes to the log with what the pass had already done, and the run continues.
// The rows it did not remove are still there; nothing about the engine's decisions changed.
//
// observed is what THIS scan actually listed (see scanOnce), and it is what decides which
// rows may go: see rowIsSpent.
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

// rowIsSpent answers store.Prunable: may this terminal row be removed without changing
// what the engine does with that file?
//
// A row is SPENT only when this run LOOKED where the file should be and it was not there.
// Both halves are load-bearing, and the second is the one that is easy to get wrong:
//
//   - Looked. A row is only ever spent if THIS run listed its directory (observed). A
//     directory that could not be listed, or that does not exist, yields no evidence at
//     all - and the shape that matters is the nested mount that is down, where the roots
//     list fine and a whole subtree is simply absent. Pruning on that absence and then
//     meeting the files again when the mount returns is the re-encode this rule exists to
//     prevent, over an entire library at once. A missing or unlistable ROOT refuses the
//     run outright (FILESYSTEM-1), so the two rules together mean no absence is ever read
//     as "gone" unless holdfast successfully listed the directory it was absent from.
//   - Not there. Absent, or present under a fingerprint that is not this row's - which is
//     the superseded row the swap already deletes by hand after every transcode. Claim is
//     keyed on path+fingerprint, so such a row can hold nothing out of the encoder. Any
//     other stat failure (a permission error, an I/O error) is not an absence and is
//     answered like an unlisted directory: keep.
//
// What it costs is stated where an operator will meet it (README, "Bounding the ledger"):
// a library that is not churning has one row per file and every one of them is holding
// that file out of the encoder, so its ledger is bounded by the library and not by
// history_retention_rows. Retention bounds what the library has FINISHED with.
//
// The counters are plain ints because PruneTerminal calls this synchronously, on this
// goroutine, one row at a time.
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

// cleanStaleTemps deletes `*.__transcoding__.*` files left under the roots by a prior
// killed run (crash-safety: the swap is the only mutation, so a half-written encode is
// worth nothing and reclaiming it is right).
//
// It sweeps WORK IN PROGRESS and nothing else. Three kinds of file it must never take:
//
//   - a RETAINED replacement (`*.__holdfast-replacement__.*`). It passed every gate and
//     may be the only faithful copy of a source whose fate is unknown. It carries a
//     different marker precisely so this sweep cannot reach it by construction.
//   - a temp path a live record names as a job's replacement. That happens when the
//     move to a retained name could not be made, so the record says what the file is.
//   - a temp path holding a FINISHED replacement with no record at all, which is what a
//     library that went read-only leaves behind: the same failure denies the swap, the
//     move to the retained name and the incident write alike (strayReplacementHold).
func (e *Engine) cleanStaleTemps(ctx context.Context) { e.sweepStaleTemps(ctx, e.passListings()) }

// sweepStaleTemps is the sweep as a pass runs it: over the listings that pass
// already has, and listing only what it does not. It reads them and does not
// release them - the enumeration wants the same entries next, and the whole
// point is that neither asks the filesystem twice.
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
				// Per ENTRY, not per directory. Deciding a file's fate costs probes
				// that a cancelled context kills, so a SIGTERM landing inside this
				// loop would otherwise leave every remaining file to be judged by
				// questions nothing can answer. The walking branch below already
				// checks per path; this is the same check, in the branch that was
				// only asking once a directory.
				if ctx.Err() != nil {
					break
				}
				// The kind the LISTING reported, links not followed, which is the
				// same question this branch asked when it listed for itself: a
				// symbolic link is a name to remove here, never a directory to
				// step into.
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

// sweepTemp discards ONE work-in-progress temp, and reports whether it did. It is the
// single place the sweep's exceptions live, so the coverage-bounded branch and the
// walking branch cannot disagree about them. There are two, and they are independent:
//
//   - a temp path a live RECORD names as a job's replacement (AC15d), and
//   - a temp path holding a finished replacement that no record survived to name
//     (AC15i's record-free half - strayReplacementHold).
//
// The second is asked even when the first says nothing, because the case it exists for
// is precisely the one where the store write that would have made the record is what
// failed.
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

// scanOnce walks the roots, sorts matches for deterministic order, and fans them
// out to a pool of workers (Cfg.EffectiveWorkers(), default/minimum 1 — the
// pre-TRANSCODE-5 behaviour). It stops promptly if ctx is cancelled: workers stop
// pulling new files from the channel, and the in-flight ffmpeg subprocess (if any)
// is killed via ctx cancellation propagating through exec.CommandContext.
//
// It returns the set of directories this scan LISTED SUCCESSFULLY - the only places
// this run has evidence about, and therefore the only places the retention pass may
// read a missing file as a file that is gone (see rowIsSpent). It is the enumeration's
// own record rather than a re-derivation, so the two cannot disagree about where
// holdfast looked.
func (e *Engine) scanOnce(ctx context.Context, pass *listings) (map[string]bool, error) {
	files, observed := e.enumerateIn(pass)

	n := e.Cfg.EffectiveWorkers()
	ch := make(chan string)
	var wg sync.WaitGroup
	// firstCancelErr captures the first context-cancellation error surfaced by any
	// worker, so RunOneshot propagates it exactly as the sequential version did. In
	// practice this is close to the `return ctx.Err()` fallback below (a worker only
	// sees Canceled/DeadlineExceeded because ctx was cancelled), but it is kept
	// explicit so a future child-context deadline inside ProcessFile would still be
	// surfaced rather than masked by a not-yet-done parent ctx.
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
					// A per-file error is logged and recorded inside ProcessFile; it
					// never takes down the scan. (ProcessFile returns nil for handled
					// per-file outcomes; a non-context error here is unexpected but
					// non-fatal.)
					e.Log.Warn("process file error (continuing)", "file", f, "err", err)
				}
			}
		}(workerID)
	}

feed:
	for _, f := range files {
		// Pause control (TRANSCODE-7): stop handing out NEW files the moment we're
		// paused. Workers already mid-file finish safely (the atomic swap is never
		// interrupted); the not-yet-fed files stay pending for the next scan after
		// resume. Checked here — before the send — so pause only ever DELAYS work,
		// never touches an in-flight encode.
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

// enumerate returns every source this run may act on, sorted for a deterministic
// order, and the set of directories it LISTED SUCCESSFULLY to find them.
//
// With a Coverage set (FILESYSTEM-1) it lists exactly the directories the startup
// walk traversed successfully and nothing else: no recursion of its own, so a
// directory the walk declined, could not read, or never reached yields no file
// here, and a bind-mount loop the walk cut cannot be followed. Without one it
// falls back to walking the roots directly, which is the behaviour of an Engine
// built without the startup check.
//
// Two hold-backs are applied here and they are the only two the swap half creates
// (FILESYSTEM-1). IsSourceName carries the RECORD-FREE one: a retained
// replacement is a file holdfast wrote and is never anybody's source, whether or not a
// record of it survived - and where the store could not be written, none did. offered()
// carries the RECORD-based one: a parked job's two recorded paths, and any recorded
// replacement path whose disposition still excludes it. Everything else in the same
// roots enumerates exactly as before, so a parked job withholds two files from the work
// and never narrows the run.
//
// The observed set is the SAME evidence in its other form: a directory is in it only
// when a listing of that directory returned and that listing is the one this scan drew
// its sources from. A directory that is missing, unreadable, or that the walk declined
// is absent from it, and the retention pass reads that as "no evidence" rather than as
// "the files are gone". A covered directory that listed EMPTY is in it: holdfast looked
// and found nothing, which is evidence and is not the same as never having looked.
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
			// else. Its files are already excluded by name (IsSourceName), and the
			// directory is skipped here as well so a retained original cannot become
			// a source through any route at all - re-encoding one would feed an
			// operator's rescued bytes straight back to the encoder.
			if filepath.Base(dir) == UndoDirName {
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
				// WalkDir reports a directory it could not read by calling back a
				// SECOND time for that directory, carrying the error. Withdraw it:
				// it was marked on the way in, and a listing that failed is not one
				// this run may draw a conclusion from.
				if d != nil && d.IsDir() {
					delete(observed, path)
				}
				return nil
			}
			if d.IsDir() {
				// The retention area is never a source, and it is skipped BEFORE it is
				// marked observed - exactly as the Coverage branch above skips it. A
				// directory this run declined to list is not evidence about what is in
				// it, and the retention pass must read it as "no evidence" rather than
				// as "the files are gone".
				if d.Name() == UndoDirName {
					return filepath.SkipDir
				}
				observed[path] = true
				if IsSourceName(d.Name(), e.Cfg.VideoExts) {
					e.skipSourceNamedDirectory(path)
				}
				return nil
			}
			if IsSourceName(filepath.Base(path), e.Cfg.VideoExts) {
				// A walk does not follow links, so a symbolic link to a DIRECTORY
				// arrives here looking exactly like a file. Both branches answer it
				// the same way, and this is the branch that has to ask: the other
				// is told by the startup walk, which followed the link already.
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

// ProcessFile applies the full safety pipeline to one source file on behalf of
// worker. It returns context.Canceled/DeadlineExceeded if interrupted (source
// untouched, temp discarded); for every other outcome it records the result in the
// store and returns nil — a single bad file must never abort the scan.
//
// The store.Claim call is the mutual-exclusion guard: it is the ONLY thing that
// stands between two workers (or two overlapping runs) both encoding the same
// source. The tab/newline skip before it stays unrecorded (a pathological path is
// not worth a row); the hardlink guard runs before Claim too but DOES record a
// "hardlinked" skip via RecordSkip/ClearSkip — a report-only write that never claims
// the file, so it cannot let two workers encode the same source.
func (e *Engine) ProcessFile(ctx context.Context, worker, f string) error {
	fi, err := os.Stat(f)
	if err != nil || fi.IsDir() {
		return nil
	}

	// Hold-backs, re-checked here rather than trusted to the scan. ProcessFile is the
	// only door into the encode/swap pipeline and it is exported, so the rule that a
	// parked job's two paths and a recorded replacement path are never encoded,
	// swapped, deleted or re-queued belongs on the door itself.
	if IsRetainedReplacementName(filepath.Base(f)) {
		return nil
	}
	if why, ok := e.heldBack(f); ok {
		e.Log.Info("not processing (held back)", "file", f, "why", why)
		return nil
	}

	// A path containing a literal tab or newline is pathological — skip it,
	// unrecorded, same as before the store existed (a store row keyed on such a
	// path would still be legal SQL, but there is no reason to track it and the
	// bash-ledger-era rationale for staying unrecorded still applies: keep the
	// no-op explicit and obvious).
	if strings.ContainsAny(f, "\t\n") {
		e.Log.Info("skip (path contains a tab/newline — unsupported)", "file", f)
		return nil
	}

	key := probe.Fingerprint(f)

	// The undo window's own guard is MUTABLE (UNDO-6): a retention that could not be
	// taken - a full disk, an unwritable retention area - is a condition that gets
	// fixed, so a stale skip from a previous scan is dropped here and the file
	// re-enters the normal path. Same discipline as the hardlink guard below.
	//
	// Not gated on the window still being enabled, for the same reason the release
	// sweep and the hardlink discount are not: turning the window off must not strand
	// what it left behind. With the window off no retention is attempted at all, so a
	// retention failure is no longer a reason to skip anything, and a row left over
	// from when it was on would park that file for as long as the setting stayed off.
	if err := e.Store.ClearSkip(ctx, f, key, SkipUndoRetentionFailed); err != nil {
		e.Log.Warn("clear stale undo-retention skip failed (continuing)", "file", f, "err", err)
	}

	// Hardlink guard. A file with >1 hard link is almost always an *arr import that
	// is also an active seed. Replacing it via rename breaks the link — reclaiming
	// no space and silently breaking the seed. Skip — and RECORD the skip as
	// "hardlinked" so an operator sees WHICH guard fired (TRANSCODE-14) instead of the
	// bare word "skipped". This still runs before Claim (the file never enters the
	// encode pipeline), and re-evaluation is preserved because the link count is
	// MUTABLE: RecordSkip only writes a skipped/hardlinked row where none with a real
	// outcome exists, and once the file is no longer hardlinked the else-branch's
	// ClearSkip removes that stale row so the file is reclaimed on the normal path.
	//
	// A link THIS TOOL holds is discounted (UNDO-6). The undo window takes a second
	// link to the source before the rename, so a run interrupted in that window leaves
	// the source at two links with nothing foreign about the extra one - and the guard,
	// reading only the count, would park the very file the window was protecting.
	// Discounting is proved per link (same inode, live retention record), never
	// assumed from the count, so a foreign extra link still skips exactly as it did.
	if e.Cfg.HardlinkSkip() {
		if links := probe.NLink(f); links > 1 && links > 1+e.retainedLinks(ctx, f, key) {
			e.Log.Info("skip (hardlinked — swap would break a seed and reclaim nothing)", "file", f, "links", links)
			changed, err := e.Store.RecordSkip(ctx, f, key, SkipHardlinked)
			if err != nil {
				// Fail safe: recording the skip is a reporting nicety, never the decision.
				// If the store hiccups, still skip the file (the point of the guard) — the
				// UI just won't show this particular hardlink skip until the next scan.
				e.Log.Warn("record hardlink skip failed (still skipping the file)", "file", f, "err", err)
			} else if changed {
				// Emit once, only when the skip was newly recorded, so a live client
				// (and the metrics/notify observers) see this skip exactly once — not
				// once per scan for the lifetime of the seed.
				e.emit(Event{Path: f, Status: store.Skipped, Outcome: e.because(SkipHardlinked)})
			}
			return nil
		}
		// Not (or no longer) hardlinked: drop any stale "hardlinked" skip we recorded on
		// a previous scan (the seed finished), so this file re-enters the normal Claim
		// path below and gets reclaimed. A no-op for a file that was never hardlinked.
		if err := e.Store.ClearSkip(ctx, f, key, SkipHardlinked); err != nil {
			e.Log.Warn("clear stale hardlink skip failed (continuing)", "file", f, "err", err)
		}
	}

	// Claim: the resume short-circuit AND the cross-worker mutual-exclusion guard
	// in one atomic call. done/skipped hold the file out for as long as the decision
	// inputs they recorded still match the configuration handed in here, and are
	// RE-OPENED when they do not; failed is retryable up to MaxFailures (a transient
	// ENOSPC/OOM must not exclude a file forever); active (probing/encoding/verifying)
	// means another worker holds it (or it's stale, awaiting the next RunOneshot's
	// RecoverStale). Any of these => not claimed => nothing to do here.
	claimed, err := e.Store.Claim(ctx, f, key, worker, e.Cfg.MaxFailures, e.currentInputs())
	if err != nil {
		// Fail safe: a store error must never be treated as "done". Log and skip
		// this pass; the file is retried on the next scan once the store recovers.
		e.Log.Warn("claim error (skipping this pass, will retry)", "file", f, "err", err)
		return nil
	}
	if !claimed {
		return nil
	}
	// The claim moved this row to probing — surface that as a live "started" signal
	// (carrying the worker) so the API/UI shows the file entering the pipeline
	// immediately, not only once it advances to encoding.
	e.emit(Event{Path: f, Status: store.Probing, Worker: worker})

	// Symlink guard (TRANSCODE-16). The hardlink guard above catches nlink > 1, but a
	// symlink has nlink == 1 (os.Stat resolves it to its target) and slips through;
	// config.Validate refuses a symlinked ROOT but not a symlinked file WITHIN the
	// tree. The swap is os.Rename(tmp, final) with final == f — which replaces the
	// LINK itself with a regular file, silently orphaning the real target it pointed
	// at and changing what the library entry means. Resolving and transcoding the
	// target is a deliberate non-goal here, so a symlinked source is SKIPPED with a
	// logged reason, never swapped in place.
	if probe.IsSymlink(f) {
		e.Log.Info("skip (symlinked source — swap would replace the link, orphaning its target)", "file", f)
		e.finish(ctx, f, key, store.Skipped, e.because(SkipSymlink))
		return nil
	}

	// One probe snapshot of the source, shared by every skip guard below AND handed to
	// the encoder (TRANSCODE-PERF). This replaces the ~15 separate ffprobe/ffmpeg
	// processes a single encode-bound file used to spawn — codec, bitrate, field_order,
	// codec tag, the four colour tags, pix_fmt, and the side data, each fetched more
	// than once across the guards, Classify, the HDR-incomplete check and the encoder.
	// Behaviour-preserving: each accessor returns exactly what its old Prober method
	// did (shared normalisation, byte-identical side data), and reading the guards off
	// one snapshot is if anything MORE self-consistent than re-probing a file mid-
	// pipeline. The costly whole-file checks (DecodeOK, packet count, output duration/
	// stream counts) are NOT here — they run in verifyOutput against the encoded temp.
	props := e.Probe.VideoProps(ctx, f)

	codec := props.Codec()
	if codec == "" {
		e.Log.Info("skip (unreadable / no video stream)", "file", f)
		e.finish(ctx, f, key, store.Failed, e.because(FailUnreadable))
		return nil
	}
	if e.isAlreadyTargetCodec(codec) {
		e.Log.Info("skip (already at target codec)", "file", f, "codec", codec, "target", e.targetCodec)
		e.finish(ctx, f, key, store.Skipped, e.because(SkipAlreadyTargetCodec, InputTargetCodec))
		return nil
	}

	if br := props.BitrateKbps(); br > 0 && br < e.Cfg.MinBitrateKbps {
		e.Log.Info("skip (low bitrate)", "file", f, "kbps", br, "min", e.Cfg.MinBitrateKbps)
		e.finish(ctx, f, key, store.Skipped, e.because(SkipLowBitrate, InputMinBitrateKbps))
		return nil
	}

	// Interlace guard. This tool never deinterlaces — re-encoding an interlaced
	// source with a progressive-assuming pipeline bakes in combing artifacts
	// permanently. Progressive or unknown field_order proceeds.
	switch props.FieldOrder() {
	case "tt", "bb", "tb", "bt":
		e.Log.Info("skip (interlaced — not deinterlacing)", "file", f)
		e.finish(ctx, f, key, store.Skipped, e.because(SkipInterlaced))
		return nil
	}

	// HDR/DV guard (TRANSCODE-3). A generic libx265 re-encode cannot preserve a
	// Dolby Vision RPU or HDR10+ dynamic metadata (needs an external RPU
	// toolchain) — transcoding would SILENTLY strip it, a permanent,
	// invisible-until-viewed loss. Detect and SKIP; HDR10 STATIC metadata IS
	// carried through the encode (see hdr.DeriveColorArgs, wired into
	// FFmpegEncoder). Probed only here, on a non-HEVC encode-bound file
	// (already-HEVC HDR was skipped above), so the cost falls on the small
	// minority actually re-encoded.
	switch hdr.ClassFrom(props.CodecTag(), props.SideData(), props.Color("color_transfer")) {
	case hdr.ClassDV:
		e.Log.Info("skip (Dolby Vision — RPU cannot survive a generic re-encode)", "file", f)
		e.finish(ctx, f, key, store.Skipped, e.because(SkipDolbyVision))
		return nil
	case hdr.ClassHDR10Plus:
		e.Log.Info("skip (HDR10+ dynamic metadata — cannot survive a generic re-encode)", "file", f)
		e.finish(ctx, f, key, store.Skipped, e.because(SkipHDR10Plus))
		return nil
	case hdr.ClassHDR10:
		// HDR10 static metadata IS carried through the encode — but if the source
		// has a static-metadata block (mastering-display or content-light) we
		// cannot fully parse, a re-encode would silently drop it. Fail safe: SKIP
		// rather than blind-encode. A source with no such block, or one that
		// parses cleanly, proceeds normally.
		incomplete := e.staticMetadataIncomplete
		if incomplete == nil {
			incomplete = hdr.StaticMetadataIncomplete
		}
		if incomplete(props.FrameSideData()) {
			e.Log.Info("skip (HDR10 static metadata present but incomplete/unparseable — refusing to re-encode and drop it)", "file", f)
			e.finish(ctx, f, key, store.Skipped, e.because(SkipIncompleteHDRMetadata))
			return nil
		}
	}

	// Chroma/bit-depth guard. Preserve the source's chroma subsampling and floor
	// bit-depth at 10; an unrecognized/exotic pix_fmt is SKIPPED rather than
	// silently subsampled or guessed. A forced (non-"auto") PixelFormat bypasses
	// derivation entirely (back-compat).
	if e.Cfg.PixelFormatAuto() {
		srcPixFmt := props.PixFmt()
		if _, ok := hdr.DerivePixFmt(srcPixFmt); !ok {
			e.Log.Info("skip (unrecognized/exotic pixel format — refusing to silently subsample)", "file", f, "pix_fmt", srcPixFmt)
			e.finish(ctx, f, key, store.Skipped, e.because(SkipExoticPixelFormat, InputPixelFormat))
			return nil
		}
	}

	// Output container: "source"/"auto" (default) matches the SOURCE file's own
	// extension (in-place transcode) so a stream type that doesn't round-trip
	// through a different container (e.g. MP4 mov_text into MKV) isn't forced to
	// change. A forced ContainerExt overrides this.
	outExt := e.Cfg.ContainerExt
	if e.Cfg.ContainerMatchesSource() {
		outExt = strings.TrimPrefix(filepath.Ext(f), ".")
	}

	dir := filepath.Dir(f)
	base := filepath.Base(f)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	// tmp is local to this call (and thus to this worker) — TRANSCODE-5 runs N
	// workers concurrently, each on a different source file, so each needs its own
	// in-flight-temp tracking rather than one shared field. Correctness doesn't
	// depend on tracking it in the Engine at all: on any failure path below we
	// remove tmp directly, and on a ctx-cancel (SIGTERM) the ffmpeg subprocess is
	// killed by exec.CommandContext leaving tmp orphaned on disk — cleanStaleTemps
	// sweeps it on the next startup, exactly like the pre-worker-pool code path.
	//
	// The path is the build's own construction (see swap.go).
	final := filepath.Join(dir, stem+"."+outExt)

	// Collision guard. When the container ext changes (movie.mp4 -> movie.mkv),
	// final is a DIFFERENT path than the source. If a distinct file already lives
	// there, the swap would silently overwrite it and then delete our source —
	// destroying two files. Refuse. (When final == f the rename replaces the source
	// in place, which is intended.)
	if final != f {
		if _, err := os.Lstat(final); err == nil {
			e.Log.Info("skip (target already exists as a distinct file — refusing to clobber)", "file", f, "target", final)
			e.finish(ctx, f, key, store.Skipped, e.because(SkipTargetExists, InputContainerExt))
			return nil
		}
	}

	if e.Cfg.DryRun {
		// A DRY RUN'S DECISION IS RECORDED, and that is the whole of what changes here.
		//
		// The file has passed every guard, so a run with dry_run off would transcode it -
		// the question an operator turns dry_run on to answer. Until now the answer was
		// thrown away: this branch returned with the row still sitting in `probing`, where
		// the worker's claim had left it, so the file was indistinguishable from one a
		// worker was still examining and every figure that says what the run CONCLUDED
		// reported the run as having concluded nothing.
		//
		// Nothing else about a dry run changes. Nothing is encoded, nothing is swapped and
		// nothing is deleted: this returns before the temp path is even chosen, exactly as
		// it did before, and the two facts recorded below are read from the source and
		// written to the ledger.
		//
		// The size is a FRESH stat of the file that was decided rather than the one taken
		// at the top of ProcessFile, so what is recorded is the file as it was at the
		// moment of the decision. A stat that fails records NOTHING - nil is "not
		// recorded", which a reader shows as such - because a fabricated size would put a
		// number into the one figure an operator uses to size the job.
		// Test seam: nil in production, so this is a no-op there.
		if e.hookBeforeDryRunRecord != nil {
			e.hookBeforeDryRunRecord(f)
		}
		// The decision inputs a dry run records are the ones the ENCODE it is predicting
		// would have been taken under, which is what makes the row say what it says: this
		// file, under this encoder at this crf and preset, is one a real run would
		// transcode. They change nothing about whether it is re-claimed - a dry-run
		// decision is always re-claimable (see store.Claim) - so they are here for the
		// operator reading the row, not for the engine.
		out := &store.Outcome{
			SourceCodec:    codec,
			DecisionInputs: e.inputsRead(InputTargetCodec, InputEncoder, InputCRF, InputPreset),
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

	// Pick the temp path and clear any stale temp at it. The n-suffixed candidates
	// exist for exactly one situation, and it is a real one: a source resolved "the
	// source is intact" is handed back to enumeration and encoded again, while the
	// replacement of the FAILED attempt may still be sitting beside it. The retained
	// marker already keeps those two apart by construction; the candidate search covers
	// the remaining case, a replacement that could not be MOVED to its retained name and
	// so is still at a temp path - whether a record names it or the file itself is the
	// only evidence left. Such a candidate is skipped, never cleared. Clearing a stale
	// temp at a path nothing holds back is the pre-existing behaviour and is kept.
	tmp, err := e.pickTempPath(ctx, dir, stem, outExt)
	if err != nil {
		e.Log.Warn("FAIL (no free temp path beside the source, source untouched)", "file", f, "err", err)
		e.finish(ctx, f, key, store.Failed, &store.Outcome{Reason: err.Error()})
		return nil
	}
	e.Log.Info("transcode", "file", f, "codec", codec, "-> ", e.targetCodec, "worker", worker)
	e.advance(ctx, f, key, store.Encoding)

	// out is the PROOF, accumulated as the pipeline learns each fact (TRANSCODE-13).
	// Every terminal path below hands this same value to both the store and the
	// Observer, so the ledger and the live UI cannot disagree about what happened.
	// From here on the file has reached the encoder, so the encoder is attributable —
	// on a failure as much as on a success.
	out := &store.Outcome{Encoder: e.Cfg.Encoder}

	encStart := time.Now()
	if err := e.encode(ctx, worker, f, tmp, props); err != nil {
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
	proof, class, reason := e.verifyOutput(ctx, f, tmp)
	// Record whatever VMAF measured, on the reject path too: the numbers that rejected
	// an encode are exactly the ones an operator wants to see, and a rejection whose
	// score is thrown away is the defect this phase exists to fix.
	out.VmafMean, out.VmafMin, out.VmafModel = proof.Mean, proof.Min, proof.Model
	// The comparison format and the chroma measurement travel with the score, on the
	// reject path too and for the same reason (GATE-4). A stored score whose pixel
	// format is unrecorded does not say which pixels were compared, and a stored
	// verdict with no chroma figure does not say whether the colour survived.
	out.VmafPixFmt, out.VmafChroma, out.VmafChromaMetric = proof.PixFmt, proof.ChromaMin, proof.ChromaMetric
	// And which video stream those pixels came from. A source can carry more than one,
	// so this is what lines the score up against the guards that inspected the file. An
	// unmeasured gate carries "" here and the column stays NULL: not recorded, never the
	// stream this build would have scored had it run.
	out.VmafStream = proof.Stream
	if reason != nil {
		if ctx.Err() != nil {
			_ = os.Remove(tmp)
			return ctx.Err()
		}
		e.Log.Warn("FAIL (verify rejected, source untouched)", "file", f,
			"reason", reason.Error(), "failure_class", class.Class())
		_ = os.Remove(tmp)
		// The gate's own verdict decides both of these, and it decided them at the line
		// that rejected the encode - nothing here re-reads the message to work out what
		// kind of rejection it was.
		out.FailureClass = class
		out.Reason = failureReason(class, reason.Error())
		e.finish(ctx, f, key, store.Failed, out)
		return nil
	}

	// Re-check the collision guard right before the swap: an encode can take hours,
	// during which another process could create `final`. Never overwrite a distinct
	// file that appeared while we were encoding.
	if final != f {
		if _, err := os.Lstat(final); err == nil {
			e.Log.Warn("FAIL (target appeared during encode — refusing to clobber)", "file", f, "target", final)
			_ = os.Remove(tmp)
			out.Reason = "target appeared during encode — refused to clobber " + final
			e.finish(ctx, f, key, store.Failed, out)
			return nil
		}
	}

	// Durability before the swap (TRANSCODE-17). os.Rename is atomic w.r.t. a
	// concurrent reader, but atomicity is not PERSISTENCE: after Encode returns, the
	// temp's data blocks may still live only in the page cache, so a POWER LOSS (not a
	// clean crash — fixture (3) already proves the clean-crash window fails safe) could
	// make the rename durable while the bytes it now names are not, yielding a
	// zero-length or torn file where the source used to be. The POSIX/Linux
	// durable-rename discipline is: fsync the temp's DATA before the rename (so the
	// bytes are on disk before any name points at them), then fsync the parent
	// DIRECTORY after the rename (below, so the rename entry itself survives). If the
	// temp cannot be made durable, do NOT swap — discard it and fail, source untouched.
	//
	// This runs BEFORE the TRANSCODE-16 source re-fingerprint below, deliberately: the
	// temp fsync can be slow (it flushes the encode's residual dirty pages), and the
	// re-fingerprint's guarantee is that its window is the microseconds between IT and
	// the rename syscall — so the fsync must not sit inside that window. Ordering here
	// is independent of the re-fingerprint (fsyncing the temp neither reads nor depends
	// on the source), so moving it up is free and keeps the -16 TOCTOU window tight.
	if err := e.fsync(tmp); err != nil {
		e.Log.Warn("FAIL (could not fsync the encode before the swap, source untouched)", "file", f, "err", err)
		_ = os.Remove(tmp)
		out.Reason = "fsync temp before swap: " + err.Error()
		e.finish(ctx, f, key, store.Failed, out)
		return nil
	}

	// Retain the original BEFORE the rename (UNDO-6). The rename is the only
	// irreversible act this tool performs, and every gate in front of it is an
	// estimate; a second link taken here keeps the original's bytes alive after the
	// rename has taken its only other name away, so a bad encode that cleared every
	// gate can still be walked back.
	//
	// Placement mirrors the temp fsync above and for the same reason: it is hoisted
	// OUT of the re-fingerprint's window, so the TOCTOU the -16 guard narrows stays
	// the microseconds between that stat and the rename syscall and does not grow by
	// a link() and a possible mkdir(). Retaining first is safe in the other direction
	// too - if the re-fingerprint below then refuses the swap, the retention is
	// discarded on the way out and the source is left exactly as it was.
	//
	// A retention that cannot be taken SKIPS the file. The window's promise is that a
	// swap can be undone, and a swap this tool could not undo is not one it performs
	// while that promise is in force.
	var retained string
	if e.Cfg.UndoEnabled() {
		u := e.undo()
		r, rerr := u.retain(f, key)
		if rerr != nil {
			e.Log.Info("skip (the original could not be retained, so the swap could not be undone — source untouched)",
				"file", f, "err", rerr)
			_ = os.Remove(tmp)
			e.finish(ctx, f, key, store.Skipped, e.because(SkipUndoRetentionFailed))
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
	// abandon undoes the retention on every path below that decides NOT to swap. A
	// retained link with no swap behind it is not a loss (it names the same bytes the
	// source still has), but it would raise the source's link count for nothing and
	// leave an orphan in the retention area, so each refusal cleans up after itself.
	abandon := func() {
		if retained != "" {
			e.undo().discard(retained)
		}
	}

	// Re-fingerprint the SOURCE right before the swap (TRANSCODE-16 — the headline
	// no-loss hazard). ProcessFile fingerprinted the source ONCE at entry (key) and
	// has re-checked only the TARGET since; the source's own identity was never
	// re-verified. An encode can run for hours, and if Plex / an *arr / a user
	// rewrote or replaced the source in that window, its size:mtime moved. The swap
	// below would then atomically overwrite the NEWER content with a re-encode of the
	// stale bytes — silent data loss that every structural and VMAF gate "passed"
	// only because they ran against the OLD file. This bites BOTH swap shapes: an
	// in-place rename (final == f) overwrites the new source directly, and an
	// ext-change swap removes f afterwards, deleting the new content. If the
	// fingerprint moved, discard the temp and FAIL with a logged reason — never swap.
	// This narrows the TOCTOU to the microseconds between this stat and the rename
	// syscall (nothing slow runs between them — the temp fsync above is deliberately
	// hoisted out of this window); it cannot close it entirely (only an exclusive lock
	// on a file another process owns could), but it turns "guaranteed loss on any
	// mid-encode rewrite" into "loss only on a sub-millisecond race". Symmetric with
	// the target re-check.
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
	// file's identity, not the pre-swap source's. The post-swap fingerprint (new
	// size/mtime) is ALWAYS a fresh key with no existing row — even when
	// final == f, the rename changed the file's size/mtime in place — so Claim it
	// first (Finish alone would be a no-op UPDATE against a nonexistent row and the
	// done outcome would be silently lost).
	finalKey := probe.Fingerprint(final)
	if _, err := e.Store.Claim(ctx, final, finalKey, worker, e.Cfg.MaxFailures, e.currentInputs()); err != nil {
		e.Log.Warn("claim of final key failed (done outcome still applies on disk)", "file", final, "err", err)
	}
	// What this encode was taken under: the codec it targeted and the three settings
	// that decided what came out. It is recorded HERE, on the done row, because that row
	// is the permanent answer about the replacement - and the moment any of the four
	// moves, the answer is one this build would no longer give, so the next scan offers
	// the file back to the guards rather than skipping it for ever.
	out.DecisionInputs = e.inputsRead(InputTargetCodec, InputEncoder, InputCRF, InputPreset)
	// Record the terminal Done state in the store WITHOUT emitting (finishStore), then
	// emit ONE rich Done event carrying the same proof. Emitting exactly once here
	// (rather than a generic finish emit plus a separate rich one) keeps a metrics
	// consumer's per-outcome counters from double-counting done.
	e.finishStore(ctx, final, finalKey, store.Done, out)
	e.emit(Event{Path: final, Status: store.Done, Worker: worker, Outcome: out})
	// Prune the superseded pre-swap row (the source's old identity), so the table
	// doesn't accumulate one dangling row per transcoded file. The swap always
	// changes the file's size/mtime, so (f,key) is never the same row as the fresh
	// (final,finalKey) done row just written — but guard it anyway.
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

// because builds the Outcome a guard records: WHICH guard fired, and the current value
// of the configuration keys that guard READ to decide it. Skips happen before the encoder
// runs, so there is nothing else to prove about them.
//
// The inputs are what make the verdict re-derivable instead of permanent: a low-bitrate
// skip records the threshold it compared against, so lowering that threshold offers the
// file to the pipeline again, while changing a key the guard never read leaves it exactly
// where it is. A guard that read NO configuration is called with no keys and records the
// empty set - which is a record, and is why a verdict nothing can move is not re-opened on
// every scan for ever.
func (e *Engine) because(reason string, read ...string) *store.Outcome {
	return &store.Outcome{Reason: reason, DecisionInputs: e.inputsRead(read...)}
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

// isAlreadyTargetCodec reports whether a source's probed video codec already IS
// the engine's configured target codec, generalizing the pre-TRANSCODE-6 hardcoded
// "already HEVC" check: for an hevc target, "hevc" and its legacy ffprobe alias
// "h265" both count; for an av1 target, "av1" counts. A source already at the
// target is skipped rather than pointlessly re-encoded.
func (e *Engine) isAlreadyTargetCodec(codec string) bool {
	switch e.targetCodec {
	case "hevc":
		return codec == "hevc" || codec == "h265"
	default:
		return codec == e.targetCodec
	}
}

// isTempName reports whether a basename is a transcoder work-in-progress temp.
func isTempName(base string) bool {
	return strings.Contains(base, "."+TempMarker+".")
}

// IsSourceName reports whether a file BASENAME is one a scan would enumerate as
// a source: it carries one of the configured video extensions and is not one of this
// tool's own working files. Three things are excluded and each is a file holdfast
// itself wrote, none of which is ever anybody's source even though each is itself a
// *.mkv (or whatever the source was):
//
//   - a work-in-progress temp;
//   - an original the undo window is holding (UNDO-6). A retention area whose files
//     were enumerated would hand the encoder the very bytes the undo window is
//     holding, re-encode them, and swap the result over them - destroying the thing an
//     operator was given a window to recover;
//   - a replacement this tool RETAINED (FILESYSTEM-1's record-free hold-back: a file
//     holdfast wrote is never anybody's source, whether or not a record of it
//     survived, and where the store could not be written none did).
//
// It is the ONE definition of "a media file this run would enumerate", shared by the
// scan and by the startup walk, which must decide it from the name alone - it opens no
// file, so the walk's cost is bounded by the directory tree and not by the library.
func IsSourceName(base string, exts []string) bool {
	return !isTempName(base) && !isUndoName(base) && !IsRetainedReplacementName(base) &&
		matchesVideoExt(base, exts)
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
