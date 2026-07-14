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
// source. Everything before Claim (the tab/newline and hardlink skips) is
// deliberately unrecorded and must stay before Claim so it never creates a store
// row.
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
	// no space and silently breaking the seed. Skip, and DON'T record: the link
	// count is mutable (the seed may finish), so re-evaluate every scan. Must run
	// before Claim so it never creates a store row.
	if e.Cfg.HardlinkSkip() && probe.NLink(f) > 1 {
		e.Log.Info("skip (hardlinked — swap would break a seed and reclaim nothing)", "file", f, "links", probe.NLink(f))
		return nil
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

	codec := e.Probe.VideoCodec(ctx, f)
	if codec == "" {
		e.Log.Info("skip (unreadable / no video stream)", "file", f)
		e.finish(ctx, f, key, store.Failed)
		return nil
	}
	if e.isAlreadyTargetCodec(codec) {
		e.Log.Info("skip (already at target codec)", "file", f, "codec", codec, "target", e.targetCodec)
		e.finish(ctx, f, key, store.Skipped)
		return nil
	}

	if br := e.Probe.BitrateKbps(ctx, f); br > 0 && br < e.Cfg.MinBitrateKbps {
		e.Log.Info("skip (low bitrate)", "file", f, "kbps", br, "min", e.Cfg.MinBitrateKbps)
		e.finish(ctx, f, key, store.Skipped)
		return nil
	}

	// Interlace guard. This tool never deinterlaces — re-encoding an interlaced
	// source with a progressive-assuming pipeline bakes in combing artifacts
	// permanently. Progressive or unknown field_order proceeds.
	switch e.Probe.FieldOrder(ctx, f) {
	case "tt", "bb", "tb", "bt":
		e.Log.Info("skip (interlaced — not deinterlacing)", "file", f)
		e.finish(ctx, f, key, store.Skipped)
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
	switch hdr.Classify(ctx, e.Probe, f) {
	case hdr.ClassDV:
		e.Log.Info("skip (Dolby Vision — RPU cannot survive a generic re-encode)", "file", f)
		e.finish(ctx, f, key, store.Skipped)
		return nil
	case hdr.ClassHDR10Plus:
		e.Log.Info("skip (HDR10+ dynamic metadata — cannot survive a generic re-encode)", "file", f)
		e.finish(ctx, f, key, store.Skipped)
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
		if incomplete(e.Probe.FrameSideDataFlat(ctx, f)) {
			e.Log.Info("skip (HDR10 static metadata present but incomplete/unparseable — refusing to re-encode and drop it)", "file", f)
			e.finish(ctx, f, key, store.Skipped)
			return nil
		}
	}

	// Chroma/bit-depth guard. Preserve the source's chroma subsampling and floor
	// bit-depth at 10; an unrecognized/exotic pix_fmt is SKIPPED rather than
	// silently subsampled or guessed. A forced (non-"auto") PixelFormat bypasses
	// derivation entirely (back-compat).
	if e.Cfg.PixelFormatAuto() {
		srcPixFmt := e.Probe.PixFmt(ctx, f)
		if _, ok := hdr.DerivePixFmt(srcPixFmt); !ok {
			e.Log.Info("skip (unrecognized/exotic pixel format — refusing to silently subsample)", "file", f, "pix_fmt", srcPixFmt)
			e.finish(ctx, f, key, store.Skipped)
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
			e.finish(ctx, f, key, store.Skipped)
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

	// Time the encode purely for the metrics histogram (TRANSCODE-8). This is a
	// measurement only — it wraps nothing but time.Since around the existing call and
	// changes no behaviour on any path.
	encStart := time.Now()
	if err := e.Enc.Encode(ctx, f, tmp); err != nil {
		if ctx.Err() != nil { // interrupted: discard temp, DON'T finish — leave active for RecoverStale
			_ = os.Remove(tmp)
			return ctx.Err()
		}
		e.Log.Warn("FAIL (encode error, source untouched)", "file", f, "err", err)
		_ = os.Remove(tmp)
		e.finish(ctx, f, key, store.Failed)
		return nil
	}
	encodeDur := time.Since(encStart)

	e.advance(ctx, f, key, store.Verifying)
	vmafScore, reason := e.verifyOutput(ctx, f, tmp)
	if reason != nil {
		if ctx.Err() != nil {
			_ = os.Remove(tmp)
			return ctx.Err()
		}
		e.Log.Warn("FAIL (verify rejected, source untouched)", "file", f, "reason", reason.Error())
		_ = os.Remove(tmp)
		e.finish(ctx, f, key, store.Failed)
		return nil
	}

	// Re-check the collision guard right before the swap: an encode can take hours,
	// during which another process could create `final`. Never overwrite a distinct
	// file that appeared while we were encoding.
	if final != f {
		if _, err := os.Lstat(final); err == nil {
			e.Log.Warn("FAIL (target appeared during encode — refusing to clobber)", "file", f, "target", final)
			_ = os.Remove(tmp)
			e.finish(ctx, f, key, store.Failed)
			return nil
		}
	}

	// Atomic swap. Same directory => same filesystem => rename() is atomic. If the
	// ext is unchanged the rename replaces the source in one step; if it changed we
	// rename to the new name then remove the now-orphaned source.
	if err := os.Rename(tmp, final); err != nil {
		e.Log.Warn("FAIL (swap error, source untouched)", "file", f, "err", err)
		_ = os.Remove(tmp)
		e.finish(ctx, f, key, store.Failed)
		return nil
	}
	if final != f {
		if err := os.Remove(f); err != nil {
			e.Log.Warn("transcoded ok but could not remove original", "file", f, "err", err)
		}
	}
	// Reclaimed space = original source size − final output size. fi was stat'd at
	// entry (the pre-encode source), so this is accurate even though f may already
	// be gone (a container-changing swap removed it). A byte-carrying Done event
	// lets the API/UI total reclaimed space live; the generic Done emit from the
	// finish wrapper below carries 0, so summing the field never double-counts.
	newSize := probe.FileSize(final)
	reclaimed := fi.Size() - newSize
	if reclaimed < 0 {
		reclaimed = 0 // defensive: the strictly-smaller gate should preclude this
	}
	e.Log.Info("DONE", "file", final, "bytes", newSize, "reclaimed", reclaimed,
		"encode_ms", encodeDur.Milliseconds(), "vmaf", vmafScore)
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
	// emit ONE rich Done event carrying the reclaimed bytes, encode duration, and VMAF
	// score. Emitting exactly once here (rather than a generic finish emit plus a
	// separate byte emit) keeps a metrics consumer's per-outcome counters from
	// double-counting done.
	e.finishStore(ctx, final, finalKey, store.Done)
	e.emit(Event{
		Path:           final,
		Status:         store.Done,
		Worker:         worker,
		BytesReclaimed: reclaimed,
		EncodeDuration: encodeDur,
		VmafScore:      vmafScore,
	})
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

// finishStore records a terminal outcome in the store WITHOUT emitting an event.
// The Done swap path uses this (then emits one rich Done event itself); every other
// terminal path uses finish, which emits a generic event.
func (e *Engine) finishStore(ctx context.Context, path, key string, s store.Status) {
	if err := e.Store.Finish(ctx, path, key, s); err != nil {
		e.Log.Warn("store finish failed", "file", path, "status", s, "err", err)
	}
}

// finish records a terminal outcome AND emits a generic event for it — used for the
// skipped/failed terminal transitions (the Done swap path uses finishStore + its own
// rich emit, so Done is emitted exactly once).
func (e *Engine) finish(ctx context.Context, path, key string, s store.Status) {
	e.finishStore(ctx, path, key, s)
	e.emit(Event{Path: path, Status: s})
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
