// Package engine is the data-safety core of the transcoder — a faithful Go port of
// the bash orchestrator (media/transcoder/transcode.sh). Its whole purpose is the
// invariant the design defends: NEVER destroy a source until a replacement is
// proven good. The only filesystem mutation is an atomic same-directory rename that
// runs solely after the output passes every gate in verifyOutput; any failure
// discards the temp and leaves the source byte-for-byte untouched.
//
// Scope (TRANSCODE-1): the structural safety contract + the CPU libx265 encode +
// the resumable ledger, oneshot. Colour/HDR (TRANSCODE-3), VMAF (TRANSCODE-4), the
// SQLite queue + worker pool (TRANSCODE-5), and hardware codecs (TRANSCODE-6) build
// on this without weakening the invariant.
package engine

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/NSchatz/transcode/internal/config"
	"github.com/NSchatz/transcode/internal/hdr"
	"github.com/NSchatz/transcode/internal/ledger"
	"github.com/NSchatz/transcode/internal/probe"
	"github.com/NSchatz/transcode/internal/vmaf"
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
	Led   *ledger.Ledger
	Log   *slog.Logger

	// staticMetadataIncomplete, when non-nil, replaces hdr.StaticMetadataIncomplete
	// for the HDR10 static-metadata guard. Unexported test seam (mirrors the bash
	// suite's TRANSCODER_TEST_HOOKS; the engine tests are in this package) —
	// production leaves it nil and uses the real predicate.
	staticMetadataIncomplete func(flatSideData string) bool

	// vmafScore, when non-nil, replaces the real libvmaf measurement in the VMAF
	// gate. Unexported test seam — lets a test force a low score or an unavailable-
	// libvmaf error without a second real encode. Production leaves it nil.
	vmafScore func(ctx context.Context, distorted, reference string, subsample int, model string) (vmaf.Result, error)

	mu         sync.Mutex
	currentTmp string // the in-flight temp (removed on Stop); guarded by mu
}

// New constructs an Engine. All dependencies are injected so tests can supply a
// deterministic Encoder while using the real Prober/Ledger over real fixtures.
func New(cfg config.Config, p *probe.Prober, enc Encoder, led *ledger.Ledger, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	return &Engine{Cfg: cfg, Probe: p, Enc: enc, Led: led, Log: log}
}

func (e *Engine) setCurrentTmp(s string) {
	e.mu.Lock()
	e.currentTmp = s
	e.mu.Unlock()
}

// discardInFlightTemp removes the in-flight temp (if any) and clears it. Called on
// a failure path and on shutdown. The source is never touched here — the swap is
// the only mutation and only runs after a full verify, which a discarded temp by
// definition never reached.
func (e *Engine) discardInFlightTemp() {
	e.mu.Lock()
	t := e.currentTmp
	e.currentTmp = ""
	e.mu.Unlock()
	if t != "" {
		_ = os.Remove(t)
	}
}

// RunOneshot discards orphaned temps from any prior killed run, then scans every
// library root once. It is the TRANSCODE-1 entrypoint (resident/queue mode is
// TRANSCODE-5). A cancelled ctx (e.g. SIGTERM) stops the scan promptly and leaves
// the source untouched.
func (e *Engine) RunOneshot(ctx context.Context) error {
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

// scanOnce walks the roots, sorts matches for deterministic order, and processes
// each source file. It stops promptly if ctx is cancelled.
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
	for _, f := range files {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := e.ProcessFile(ctx, f); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			// A per-file error is logged and recorded inside ProcessFile; it never
			// takes down the scan. (ProcessFile returns nil for handled per-file
			// outcomes; a non-context error here is unexpected but non-fatal.)
			e.Log.Warn("process file error (continuing)", "file", f, "err", err)
		}
	}
	return nil
}

// ProcessFile applies the full safety pipeline to one source file. It returns
// context.Canceled/DeadlineExceeded if interrupted (source untouched, temp
// discarded); for every other outcome it records the result in the ledger and
// returns nil — a single bad file must never abort the scan.
func (e *Engine) ProcessFile(ctx context.Context, f string) error {
	fi, err := os.Stat(f)
	if err != nil || fi.IsDir() {
		return nil
	}

	// A path containing a literal tab or newline cannot be represented in the TSV
	// ledger (the row would mis-parse and the file re-encode every scan). Such names
	// are pathological — skip them, unrecorded.
	if strings.ContainsAny(f, "\t\n") {
		e.Log.Info("skip (path contains a tab/newline — unsupported)", "file", f)
		return nil
	}

	key := probe.Fingerprint(f)

	// Resume short-circuit. done/skipped are permanent; failed is retryable up to
	// MaxFailures (a transient ENOSPC/OOM must not exclude a file forever).
	prev, _ := e.Led.Status(f, key)
	switch prev {
	case ledger.Done, ledger.Skipped:
		return nil
	case ledger.Failed:
		fails, _ := e.Led.FailCount(f, key)
		if fails >= e.Cfg.MaxFailures {
			return nil // parked after MaxFailures attempts
		}
		// else fall through and retry
	}

	// Hardlink guard. A file with >1 hard link is almost always an *arr import that
	// is also an active seed. Replacing it via rename breaks the link — reclaiming
	// no space and silently breaking the seed. Skip, and DON'T record: the link
	// count is mutable (the seed may finish), so re-evaluate every scan.
	if e.Cfg.HardlinkSkip() && probe.NLink(f) > 1 {
		e.Log.Info("skip (hardlinked — swap would break a seed and reclaim nothing)", "file", f, "links", probe.NLink(f))
		return nil
	}

	codec := e.Probe.VideoCodec(ctx, f)
	if codec == "" {
		e.Log.Info("skip (unreadable / no video stream)", "file", f)
		_ = e.Led.Record(ledger.Failed, key, f)
		return nil
	}
	if codec == "hevc" || codec == "h265" {
		e.Log.Info("skip (already HEVC)", "file", f)
		_ = e.Led.Record(ledger.Skipped, key, f)
		return nil
	}

	if br := e.Probe.BitrateKbps(ctx, f); br > 0 && br < e.Cfg.MinBitrateKbps {
		e.Log.Info("skip (low bitrate)", "file", f, "kbps", br, "min", e.Cfg.MinBitrateKbps)
		_ = e.Led.Record(ledger.Skipped, key, f)
		return nil
	}

	// Interlace guard. This tool never deinterlaces — re-encoding an interlaced
	// source with a progressive-assuming pipeline bakes in combing artifacts
	// permanently. Progressive or unknown field_order proceeds.
	switch e.Probe.FieldOrder(ctx, f) {
	case "tt", "bb", "tb", "bt":
		e.Log.Info("skip (interlaced — not deinterlacing)", "file", f)
		_ = e.Led.Record(ledger.Skipped, key, f)
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
		_ = e.Led.Record(ledger.Skipped, key, f)
		return nil
	case hdr.ClassHDR10Plus:
		e.Log.Info("skip (HDR10+ dynamic metadata — cannot survive a generic re-encode)", "file", f)
		_ = e.Led.Record(ledger.Skipped, key, f)
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
			_ = e.Led.Record(ledger.Skipped, key, f)
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
			_ = e.Led.Record(ledger.Skipped, key, f)
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
			_ = e.Led.Record(ledger.Skipped, key, f)
			return nil
		}
	}

	if e.Cfg.DryRun {
		e.Log.Info("DRY_RUN would transcode", "file", f, "codec", codec, "target", final)
		return nil
	}

	_ = os.Remove(tmp) // clear any stale temp for this file
	e.Log.Info("transcode", "file", f, "codec", codec, "-> ", "hevc")
	e.setCurrentTmp(tmp)

	if err := e.Enc.Encode(ctx, f, tmp); err != nil {
		if ctx.Err() != nil { // interrupted: discard temp, DON'T record failed
			e.discardInFlightTemp()
			return ctx.Err()
		}
		e.Log.Warn("FAIL (encode error, source untouched)", "file", f, "err", err)
		e.discardInFlightTemp()
		_ = e.Led.Record(ledger.Failed, key, f)
		return nil
	}

	if reason := e.verifyOutput(ctx, f, tmp); reason != nil {
		if ctx.Err() != nil {
			e.discardInFlightTemp()
			return ctx.Err()
		}
		e.Log.Warn("FAIL (verify rejected, source untouched)", "file", f, "reason", reason.Error())
		e.discardInFlightTemp()
		_ = e.Led.Record(ledger.Failed, key, f)
		return nil
	}

	// Re-check the collision guard right before the swap: an encode can take hours,
	// during which another process could create `final`. Never overwrite a distinct
	// file that appeared while we were encoding.
	if final != f {
		if _, err := os.Lstat(final); err == nil {
			e.Log.Warn("FAIL (target appeared during encode — refusing to clobber)", "file", f, "target", final)
			e.discardInFlightTemp()
			_ = e.Led.Record(ledger.Failed, key, f)
			return nil
		}
	}

	// Atomic swap. Same directory => same filesystem => rename() is atomic. If the
	// ext is unchanged the rename replaces the source in one step; if it changed we
	// rename to the new name then remove the now-orphaned source.
	if err := os.Rename(tmp, final); err != nil {
		e.Log.Warn("FAIL (swap error, source untouched)", "file", f, "err", err)
		e.discardInFlightTemp()
		_ = e.Led.Record(ledger.Failed, key, f)
		return nil
	}
	e.setCurrentTmp("")
	if final != f {
		if err := os.Remove(f); err != nil {
			e.Log.Warn("transcoded ok but could not remove original", "file", f, "err", err)
		}
	}
	e.Log.Info("DONE", "file", final, "bytes", probe.FileSize(final))
	_ = e.Led.Record(ledger.Done, probe.Fingerprint(final), final)
	return nil
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
