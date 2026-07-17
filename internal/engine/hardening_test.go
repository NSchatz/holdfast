package engine

// TRANSCODE-16 — adversarial no-loss hardening. Three fixtures that exercise the
// swap-boundary hazards the shipped suite (engine_test.go cases 1–17, 18–22, a–e)
// does NOT cover, each verified against the real swap code in engine.go:
//
//   (1) the source-mutation TOCTOU: the source is fingerprinted ONCE at entry and
//       never re-checked, so a source rewritten mid-encode is atomically overwritten
//       by a re-encode of the stale content — silent data loss. RED without the
//       pre-swap re-fingerprint guard, GREEN with it.
//   (2) a symlinked source (nlink == 1) slips the hardlink guard; an in-place rename
//       replaces the LINK with a regular file, orphaning its target. RED without the
//       symlink guard, GREEN with it.
//   (3) the ext-change two-step rename+delete fails safe: a crash injected between
//       the rename and the delete leaves BOTH files (a duplicate, never a loss) and
//       the next scan reconciles the orphan via the collision guard — it is
//       re-classified (skipped), NOT re-encoded into a second generation-loss.
//
// Like the rest of the suite these drive REAL ffmpeg and FAIL LOUD (never skip) when
// it is absent — a skipped safety proof is a false green.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// mkH264Mandelbrot writes a NON-HEVC h264 clip of the mandelbrot pattern — visually
// DISTINCT from testsrc2, so a test can prove "this specific content survived" by
// byte-comparing the file, not merely that some h264 file is present. 2s so its
// duration matches a testsrc2 encode (the verify duration-parity gate must pass on
// the pre-fix path, or the swap that demonstrates the loss never happens).
func mkH264Mandelbrot(t *testing.T, ffmpeg, path, bitrate string) {
	t.Helper()
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "mandelbrot=size=320x240:rate=10", "-t", "2",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", bitrate, "-pix_fmt", "yuv420p", "--", path)
}

// realHEVCEncode is a faithful libx265 crf22 encode of in -> out (no downscale, no
// injected damage), so the resulting temp passes every structural and perceptual gate
// against the ORIGINAL source — the point of fixture (1) is that a clean encode of the
// stale content is what would overwrite the newer source if the swap weren't guarded.
func realHEVCEncode(ctx context.Context, ffmpeg, in, out string) error {
	return exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-nostdin", "-v", "error", "-y", "-i", in,
		"-c:v", "libx265", "-crf", "22", "-preset", "veryfast", "-x265-params", "log-level=error",
		"-pix_fmt", "yuv420p10le", "--", out).Run()
}

// failReason returns the recorded Outcome.Reason for the first FAILED row whose path
// contains pathSub. Unlike ledgerHas/skipReason it does NOT re-derive the row by the
// file's CURRENT fingerprint — the source-mutation case deliberately CHANGES that
// fingerprint after the row is written (under the ENTRY fingerprint), so a
// current-fingerprint lookup would miss it. The failed row's Path column still holds
// the source path, which is what we match on.
func failReason(t *testing.T, ts *testStore, pathSub string) string {
	t.Helper()
	rows, err := ts.List(context.Background(), []store.Status{store.Failed}, 0)
	if err != nil {
		t.Fatalf("store.List(failed): %v", err)
	}
	for _, r := range rows {
		if strings.Contains(r.Path, pathSub) {
			return r.Outcome.Reason
		}
	}
	return ""
}

// TestHardening_SourceRewrittenMidEncodeIsNotOverwritten is fixture (1) — the
// headline hazard. A fake encoder produces a faithful HEVC encode of the ORIGINAL
// source and then, in the same call (simulating Plex / an *arr / a user replacing
// the file mid-encode), rewrites the source with entirely different, NEWER content.
// Every structural gate then passes against the new source (right codec, matching
// duration, strictly smaller, no dropped track, decodes clean) — so on the un-hardened
// code the swap fires and atomically overwrites the newer content with a re-encode of
// the stale bytes. The pre-swap source re-fingerprint refuses the swap instead.
//
// RED without the guard: the source is gone (now HEVC). GREEN with it: the newer
// content survives byte-for-byte and the job is recorded FAILED with a source-changed
// reason.
func TestHardening_SourceRewrittenMidEncodeIsNotOverwritten(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M") // ORIGINAL content A (testsrc2), inflated so a real encode shrinks it

	// The NEWER content B that lands on the source path mid-encode: a DIFFERENT
	// pattern, a DIFFERENT size (so the size:mtime fingerprint provably moves), same
	// duration so verify's duration-parity gate still passes against it.
	newContent := filepath.Join(d, "new_content.mkv")
	mkH264Mandelbrot(t, ffmpeg, newContent, "3M")
	newBytes, err := os.ReadFile(newContent)
	if err != nil {
		t.Fatalf("read new content: %v", err)
	}
	newMD5 := md5f(t, newContent)
	if err := os.Remove(newContent); err != nil { // keep it out of the scan
		t.Fatalf("remove staging file: %v", err)
	}

	// The adversary: a real, faithful HEVC encode of `in`, THEN the source is rewritten
	// with B. Order matters — encode reads the original, then B overwrites it.
	racingEncoder := EncoderFunc(func(ctx context.Context, in, out string, _ *probe.VideoProps) error {
		if err := realHEVCEncode(ctx, ffmpeg, in, out); err != nil {
			return err
		}
		return os.WriteFile(in, newBytes, 0o644)
	})

	// VMAF off: this fixture is about the STRUCTURAL swap boundary, and VMAF would
	// compare the stale encode against the new source and reject on its own, masking
	// the very hazard under test.
	led := run(t, ffmpeg, ffprobe, d, racingEncoder, nil)

	if md5f(t, src) != newMD5 {
		t.Error("the newer source content was overwritten — the mid-encode source rewrite was not caught before the swap")
	}
	if got := codecOf(t, ffprobe, src); got != "h264" {
		t.Errorf("source was swapped for the stale encode (codec=%q, want h264 — the new content)", got)
	}
	if nTemp(t, d) != 0 {
		t.Error("temp not discarded after refusing the swap")
	}
	if r := failReason(t, led, "movie.mkv"); !strings.Contains(r, "source changed") {
		t.Errorf("expected a failed row explaining the source changed, got %q", r)
	}
}

// TestHardening_SymlinkedSourceIsSkippedNotReplaced is fixture (2). The source is a
// symlink (nlink == 1, so it slips the hardlink guard) whose target lives OUTSIDE the
// scanned library. On the un-hardened code the in-place rename replaces the LINK with
// a regular file, silently orphaning the real target. The symlink guard skips it.
//
// RED without the guard: movie.mkv is no longer a symlink (it became a regular HEVC
// file). GREEN with it: the link and its target are untouched and the job is recorded
// as skipped/symlinked-source.
func TestHardening_SymlinkedSourceIsSkippedNotReplaced(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()     // the scanned library
	external := t.TempDir() // where the real target lives (never scanned)

	target := filepath.Join(external, "real.mkv")
	mkH264(t, ffmpeg, target, "8M") // encode-worthy, so on the un-hardened path a swap really happens
	targetMD5 := md5f(t, target)

	link := filepath.Join(root, "movie.mkv")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	led := run(t, ffmpeg, ffprobe, root, nil, nil) // real encoder — but the guard fires before it runs

	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat link: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlinked source was replaced by a regular file — the link was swapped and its target orphaned")
	}
	if md5f(t, target) != targetMD5 {
		t.Error("the symlink target was modified")
	}
	if nTemp(t, root) != 0 {
		t.Error("temp left behind for a symlinked source")
	}
	if r := skipReason(t, led, "movie.mkv"); r != SkipSymlink {
		t.Errorf("expected skip reason %q, got %q", SkipSymlink, r)
	}
}

// TestHardening_CrashBetweenRenameAndDeleteLosesNothing is fixture (3). It forces an
// ext-changing swap (movie.mp4 -> movie.mkv) and injects a crash in the exact window
// between the rename and the delete, through the real swap path. It asserts two
// things:
//
//   - no data is lost: BOTH files are on disk after the crash (the new HEVC and the
//     original), a duplicate rather than a loss; and
//   - the next scan reconciles: the orphaned original is RE-CLASSIFIED (skipped
//     because its target already exists), NOT re-encoded into a second generation
//     of quality loss, and the completed encode is left intact.
func TestHardening_CrashBetweenRenameAndDeleteLosesNothing(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	mp4 := filepath.Join(d, "movie.mp4")
	mkv := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, mp4, "8M") // h264 source; ext change forces the two-step
	origMD5 := md5f(t, mp4)

	// Build the engine directly so the crash seam is reachable and the SAME store
	// persists across both RunOneshot calls (the second is the reconciling scan;
	// RecoverStale needs the row the first left active).
	cfg := baseCfg(d) // ContainerExt "mkv" => movie.mp4's target is movie.mkv (ext change)
	ts := newTestStore(t, d)
	prober := probe.New(ffmpeg, ffprobe)
	eng := New(cfg, prober, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}, ts, discardLogger())

	// --- the crash: fire once, after the rename, before the delete ---
	crashed := false
	eng.hookAfterRename = func() error {
		crashed = true
		return errors.New("simulated crash between rename and delete")
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("first RunOneshot: %v", err)
	}
	if !crashed {
		t.Fatal("the two-step swap never ran — the fixture is not exercising the rename/delete window (is the ext change happening?)")
	}
	// No data lost: both files present, the original byte-for-byte intact.
	if !exists(mkv) {
		t.Error("the renamed encode is missing after the crash")
	}
	if !exists(mp4) {
		t.Fatal("the original source is gone after a crash BEFORE the delete — data was lost")
	}
	if md5f(t, mp4) != origMD5 {
		t.Error("the original source was modified during the crashed swap")
	}

	// --- the next scan reconciles ---
	mkvBytes := md5f(t, mkv)
	eng.hookAfterRename = nil
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("reconciling RunOneshot: %v", err)
	}
	// The orphan is re-classified, not re-encoded: it stays h264, byte-for-byte.
	if got := codecOf(t, ffprobe, mp4); got != "h264" {
		t.Errorf("the orphaned original was re-encoded (codec=%q, want h264) — a second generation-loss", got)
	}
	if md5f(t, mp4) != origMD5 {
		t.Error("the orphaned original was mutated by the reconciling scan")
	}
	if !ledgerHas(t, ts, store.Skipped, "movie.mp4") {
		t.Error("expected the orphaned original to be reconciled as a skip")
	}
	if r := skipReason(t, ts, "movie.mp4"); r != SkipTargetExists {
		t.Errorf("expected skip reason %q for the reconciled orphan, got %q", SkipTargetExists, r)
	}
	// The completed encode is untouched by reconciliation.
	if !exists(mkv) || md5f(t, mkv) != mkvBytes {
		t.Error("the completed encode was altered by the reconciling scan")
	}
	if nTemp(t, d) != 0 {
		t.Error("temp left behind after reconciliation")
	}
}
