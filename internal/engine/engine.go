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
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/encoder"
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
	// libvmaf error without a second real encode. Production leaves it nil.
	vmafScore func(ctx context.Context, distorted, reference string, subsample int, model string) (vmaf.Result, error)

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

	// Observer, when non-nil, receives an Event on every job-state transition
	// (TRANSCODE-7's API/SSE hub subscribes here). It is a fire-and-forget
	// NOTIFICATION beside the store writes — never a substitute for them and never
	// on the critical path: emit calls it directly, so the Observer contract
	// requires it to be non-blocking and concurrency-safe (see Observer). nil =
	// no emission, the pre-TRANSCODE-7 behaviour.
	Observer Observer

	// Paused, when non-nil and returning true, tells scanOnce to stop feeding NEW
	// files to workers this pass (TRANSCODE-7's pause control). It is checked
	// between files only — an in-flight encode is NEVER interrupted (that would
	// risk the invariant); paused work is simply left pending for the next scan
	// after resume. nil = never paused.
	Paused func() bool
}

// emit delivers ev to the Observer if one is set. It is deliberately trivial and
// must stay cheap + non-blocking: it runs inline on a worker goroutine, so the
// Observer (not the engine) owns any buffering/decoupling from slow consumers.
func (e *Engine) emit(ev Event) {
	if e.Observer != nil {
		e.Observer(ev)
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
	target := "hevc"
	if spec, ok := encoder.Lookup(cfg.Encoder); ok {
		target = spec.TargetCodec
	}
	return &Engine{Cfg: cfg, Probe: p, Enc: enc, Store: st, Log: log, targetCodec: target}
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
	if _, err := e.Store.RecoverStale(ctx); err != nil {
		// Fail safe: if we can't tell what was left active by a prior crash, log and
		// continue — a stuck "active" row just means that one file is skipped this
		// pass (Claim treats it as held), never a false completion. It will be
		// picked up once the store is healthy again.
		e.Log.Warn("recover stale jobs failed (continuing)", "err", err)
	}
	e.cleanStaleTemps(ctx)
	return e.scanOnce(ctx)
}

// cleanStaleTemps deletes any `*.__transcoding__.*` files left under the roots by a
// prior killed run (crash-safety: the swap is the only mutation, so a leftover temp
// is always safe to discard).
func (e *Engine) cleanStaleTemps(ctx context.Context) {
	n := 0
	for _, root := range e.Cfg.LibraryRoots {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable entry — skip, never abort the sweep
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !d.IsDir() && isTempName(filepath.Base(path)) {
				if os.Remove(path) == nil {
					n++
				}
			}
			return nil
		})
	}
	if n > 0 {
		e.Log.Info("discarded orphaned temp file(s) from a prior run", "count", n)
	}
}

// scanOnce walks the roots, sorts matches for deterministic order, and fans them
// out to a pool of workers (Cfg.EffectiveWorkers(), default/minimum 1 — the
// pre-TRANSCODE-5 behaviour). It stops promptly if ctx is cancelled: workers stop
// pulling new files from the channel, and the in-flight ffmpeg subprocess (if any)
// is killed via ctx cancellation propagating through exec.CommandContext.
func (e *Engine) scanOnce(ctx context.Context) error {
	var files []string
	for _, root := range e.Cfg.LibraryRoots {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			base := filepath.Base(path)
			if isTempName(base) { // a temp is itself a *.mkv — never a source
				return nil
			}
			if e.hasVideoExt(base) {
				files = append(files, path)
			}
			return nil
		})
	}
	sort.Strings(files)

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
		return firstCancelErr
	}
	return ctx.Err()
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

	// Hardlink guard. A file with >1 hard link is almost always an *arr import that
	// is also an active seed. Replacing it via rename breaks the link — reclaiming
	// no space and silently breaking the seed. Skip — and RECORD the skip as
	// "hardlinked" so an operator sees WHICH guard fired (TRANSCODE-14) instead of the
	// bare word "skipped". This still runs before Claim (the file never enters the
	// encode pipeline), and re-evaluation is preserved because the link count is
	// MUTABLE: RecordSkip only writes a skipped/hardlinked row where none with a real
	// outcome exists, and once the file is no longer hardlinked the else-branch's
	// ClearSkip removes that stale row so the file is reclaimed on the normal path.
	if e.Cfg.HardlinkSkip() {
		if links := probe.NLink(f); links > 1 {
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
				e.emit(Event{Path: f, Status: store.Skipped, Outcome: because(SkipHardlinked)})
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
	// in one atomic call. done/skipped are permanent; failed is retryable up to
	// MaxFailures (a transient ENOSPC/OOM must not exclude a file forever); active
	// (probing/encoding/verifying) means another worker holds it (or it's stale,
	// awaiting the next RunOneshot's RecoverStale). Any of these => not claimed =>
	// nothing to do here.
	claimed, err := e.Store.Claim(ctx, f, key, worker, e.Cfg.MaxFailures)
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
		e.finish(ctx, f, key, store.Skipped, because(SkipSymlink))
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
		e.finish(ctx, f, key, store.Failed, because(FailUnreadable))
		return nil
	}
	if e.isAlreadyTargetCodec(codec) {
		e.Log.Info("skip (already at target codec)", "file", f, "codec", codec, "target", e.targetCodec)
		e.finish(ctx, f, key, store.Skipped, because(SkipAlreadyTargetCodec))
		return nil
	}

	if br := props.BitrateKbps(); br > 0 && br < e.Cfg.MinBitrateKbps {
		e.Log.Info("skip (low bitrate)", "file", f, "kbps", br, "min", e.Cfg.MinBitrateKbps)
		e.finish(ctx, f, key, store.Skipped, because(SkipLowBitrate))
		return nil
	}

	// Interlace guard. This tool never deinterlaces — re-encoding an interlaced
	// source with a progressive-assuming pipeline bakes in combing artifacts
	// permanently. Progressive or unknown field_order proceeds.
	switch props.FieldOrder() {
	case "tt", "bb", "tb", "bt":
		e.Log.Info("skip (interlaced — not deinterlacing)", "file", f)
		e.finish(ctx, f, key, store.Skipped, because(SkipInterlaced))
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
		e.finish(ctx, f, key, store.Skipped, because(SkipDolbyVision))
		return nil
	case hdr.ClassHDR10Plus:
		e.Log.Info("skip (HDR10+ dynamic metadata — cannot survive a generic re-encode)", "file", f)
		e.finish(ctx, f, key, store.Skipped, because(SkipHDR10Plus))
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
			e.finish(ctx, f, key, store.Skipped, because(SkipIncompleteHDRMetadata))
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
			e.finish(ctx, f, key, store.Skipped, because(SkipExoticPixelFormat))
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
	tmp := filepath.Join(dir, stem+"."+TempMarker+"."+outExt)
	final := filepath.Join(dir, stem+"."+outExt)

	// Collision guard. When the container ext changes (movie.mp4 -> movie.mkv),
	// final is a DIFFERENT path than the source. If a distinct file already lives
	// there, the swap would silently overwrite it and then delete our source —
	// destroying two files. Refuse. (When final == f the rename replaces the source
	// in place, which is intended.)
	if final != f {
		if _, err := os.Lstat(final); err == nil {
			e.Log.Info("skip (target already exists as a distinct file — refusing to clobber)", "file", f, "target", final)
			e.finish(ctx, f, key, store.Skipped, because(SkipTargetExists))
			return nil
		}
	}

	if e.Cfg.DryRun {
		e.Log.Info("DRY_RUN would transcode", "file", f, "codec", codec, "target", final)
		return nil
	}

	_ = os.Remove(tmp) // clear any stale temp for this file
	e.Log.Info("transcode", "file", f, "codec", codec, "-> ", e.targetCodec, "worker", worker)
	e.advance(ctx, f, key, store.Encoding)

	// out is the PROOF, accumulated as the pipeline learns each fact (TRANSCODE-13).
	// Every terminal path below hands this same value to both the store and the
	// Observer, so the ledger and the live UI cannot disagree about what happened.
	// From here on the file has reached the encoder, so the encoder is attributable —
	// on a failure as much as on a success.
	out := &store.Outcome{Encoder: e.Cfg.Encoder}

	encStart := time.Now()
	if err := e.Enc.Encode(ctx, f, tmp, props); err != nil {
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
	proof, reason := e.verifyOutput(ctx, f, tmp)
	// Record whatever VMAF measured, on the reject path too: the numbers that rejected
	// an encode are exactly the ones an operator wants to see, and a rejection whose
	// score is thrown away is the defect this phase exists to fix.
	out.VmafMean, out.VmafMin, out.VmafModel = proof.Mean, proof.Min, proof.Model
	if reason != nil {
		if ctx.Err() != nil {
			_ = os.Remove(tmp)
			return ctx.Err()
		}
		e.Log.Warn("FAIL (verify rejected, source untouched)", "file", f, "reason", reason.Error())
		_ = os.Remove(tmp)
		out.Reason = reason.Error()
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
	if cur := probe.Fingerprint(f); cur != key {
		e.Log.Warn("FAIL (source changed during encode — refusing to overwrite the newer content)", "file", f, "entry", key, "now", cur)
		_ = os.Remove(tmp)
		out.Reason = "source changed during encode (fingerprint " + key + " -> " + cur + ") — refused to overwrite newer content"
		e.finish(ctx, f, key, store.Failed, out)
		return nil
	}

	// Atomic swap. Same directory => same filesystem => rename() is atomic. If the
	// ext is unchanged the rename replaces the source in one step; if it changed we
	// rename to the new name then remove the now-orphaned source.
	if err := os.Rename(tmp, final); err != nil {
		e.Log.Warn("FAIL (swap error, source untouched)", "file", f, "err", err)
		_ = os.Remove(tmp)
		out.Reason = err.Error()
		e.finish(ctx, f, key, store.Failed, out)
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

	e.Log.Info("DONE", "file", final, "bytes", newSize, "reclaimed", fi.Size()-newSize,
		"encode_ms", encodeDur.Milliseconds(),
		"vmaf", logScore(proof.Mean), "vmaf_min", logScore(proof.Min))
	// The done row is keyed under the FINAL file's own path+fingerprint (mirroring
	// the pre-TRANSCODE-5 ledger behaviour) so a resume short-circuits on the new
	// file's identity, not the pre-swap source's. The post-swap fingerprint (new
	// size/mtime) is ALWAYS a fresh key with no existing row — even when
	// final == f, the rename changed the file's size/mtime in place — so Claim it
	// first (Finish alone would be a no-op UPDATE against a nonexistent row and the
	// done outcome would be silently lost).
	finalKey := probe.Fingerprint(final)
	if _, err := e.Store.Claim(ctx, final, finalKey, worker, e.Cfg.MaxFailures); err != nil {
		e.Log.Warn("claim of final key failed (done outcome still applies on disk)", "file", final, "err", err)
	}
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
func (e *Engine) finishStore(ctx context.Context, path, key string, s store.Status, o *store.Outcome) {
	if err := e.Store.Finish(ctx, path, key, s, o); err != nil {
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

// because builds the one-field Outcome that a guard records: WHICH guard fired. Skips
// happen before the encoder runs, so there is nothing else to prove about them.
func because(reason string) *store.Outcome { return &store.Outcome{Reason: reason} }

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

// hasVideoExt reports whether base has one of the configured video extensions
// (case-insensitive), matching the bash `-iname` scan.
func (e *Engine) hasVideoExt(base string) bool {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(base), "."))
	if ext == "" {
		return false
	}
	for _, want := range e.Cfg.VideoExts {
		if ext == strings.ToLower(want) {
			return true
		}
	}
	return false
}
