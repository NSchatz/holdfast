package engine

// The DATA-SAFETY proof for the transcode engine — a Go port of the bash suite
// homelab/scripts/test-transcoder.sh (cases 1–17 plus the HDR/source-property cases
// 18–22 + (a)-(d), TRANSCODE-3). It drives the engine over REAL ffmpeg fixtures and
// asserts the no-loss contract holds on every unhappy path. It is anti-advisory-only:
// it exercises the code, reds on a regression, and FAILS LOUD (never skips) if
// ffmpeg/ffprobe are missing — a skip would be a false green.
//
// Cases 2/3/4/16/17 inject a deterministic Encoder so the safety branches are proven
// without depending on codec/compression luck; case 5 uses the REAL libx265 path.

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/hdr"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
	"github.com/NSchatz/holdfast/internal/vmaf"
)

// errFake is the deterministic error returned by fake encoders simulating a failure.
var errFake = errors.New("simulated encode failure")

// discardLogger returns a logger that drops all output (tests assert on behaviour,
// not logs).
func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// ---- tooling (fail loud, never skip — this is the safety proof) --------------

func tools(t *testing.T) (ffmpeg, ffprobe string) {
	t.Helper()
	ffmpeg = envOr("HOLDFAST_FFMPEG", "ffmpeg")
	ffprobe = envOr("HOLDFAST_FFPROBE", "ffprobe")
	for _, b := range []string{ffmpeg, ffprobe} {
		if _, err := exec.LookPath(b); err != nil {
			t.Fatalf("::error:: %q not found — the transcoder safety proof requires ffmpeg+ffprobe (set HOLDFAST_FFMPEG/FFPROBE): %v", b, err)
		}
	}
	return ffmpeg, ffprobe
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ---- fixture builders (real ffmpeg) -----------------------------------------

func ff(t *testing.T, ffmpeg string, args ...string) {
	t.Helper()
	if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
	}
}

// mkH264 writes an H.264 clip (container inferred from path ext). Bitrate like "8M".
func mkH264(t *testing.T, ffmpeg, path, bitrate string) {
	t.Helper()
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=320x240:rate=10",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", bitrate, "-pix_fmt", "yuv420p", "--", path)
}

// mkH264Long writes a LONGER h264 clip: 240 frames (10s @ 24fps) rather than the 20
// of mkH264. The worst-frame-floor cases (TRANSCODE-11) need a realistic frame count,
// because the blind spot they prove only exists when the damaged frames are a small
// enough FRACTION of the file for the pooled mean to average them away. With 20
// frames a single bad frame is 5% of the file and the mean catches it on its own,
// which would prove nothing.
func mkH264Long(t *testing.T, ffmpeg, path, bitrate string) {
	t.Helper()
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=10:size=320x240:rate=24",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", bitrate, "-pix_fmt", "yuv420p", "--", path)
}

// mkH264DarkGrainy writes a LONG clip that is dark AND grainy — precisely the content
// VMAF is documented to handle worst (weak on banding and dark-region blockiness;
// grain is expensive to reproduce, so a faithful encode still scores lower there).
// It is the anti-flake fixture: if a worst-frame floor is going to false-reject an
// honest encode anywhere, it is here.
func mkH264DarkGrainy(t *testing.T, ffmpeg, path, bitrate string) {
	t.Helper()
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=10:size=320x240:rate=24,noise=alls=10:allf=t+u,eq=brightness=-0.30:contrast=0.9",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", bitrate, "-pix_fmt", "yuv420p", "--", path)
}

// mkHevc writes an HEVC clip.
func mkHevc(t *testing.T, ffmpeg, path, bitrate string) {
	t.Helper()
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=320x240:rate=10",
		"-c:v", "libx265", "-x265-params", "log-level=error", "-b:v", bitrate, "-pix_fmt", "yuv420p", "--", path)
}

// ---- assertion helpers -------------------------------------------------------

func md5f(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	s := md5.Sum(b)
	return hex.EncodeToString(s[:])
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// nTemp counts leftover work-in-progress temp files under dir.
func nTemp(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && isTempName(filepath.Base(p)) {
			n++
		}
		return nil
	})
	return n
}

// testStore wraps a *store.SQLite plus the directory it scans, so assertion
// helpers can find "the row for this file" the same way the tests used to grep the
// flat ledger file for a path substring — the store is keyed by (path,
// fingerprint), not by path alone, so helpers resolve pathSub to an actual on-disk
// path under root first (walking the directory, mirroring ledgerHas's old
// substring-of-the-recorded-path semantics) and then look up its CURRENT
// fingerprint. That is exactly the row a real caller would have written: a
// skipped/failed row is keyed by the source's fingerprint, which is never mutated
// on those paths; a done row is keyed by the FINAL file's post-swap fingerprint,
// which is what's on disk once the swap has happened.
type testStore struct {
	*store.SQLite
	root string
}

// findPath walks root for the first file whose path contains sub. Returns "" if
// none found.
func (ts *testStore) findPath(t *testing.T, sub string) string {
	t.Helper()
	var exact, contains string
	_ = filepath.WalkDir(ts.root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		// Prefer an exact basename match (robust when two files in a dir share a
		// substring, e.g. movie.mkv + movie.mp4); fall back to a substring contains.
		if filepath.Base(path) == sub && exact == "" {
			exact = path
		} else if strings.Contains(path, sub) && contains == "" {
			contains = path
		}
		return nil
	})
	if exact != "" {
		return exact
	}
	return contains
}

// ledgerHas reports whether the store holds a row with the given status for the
// file whose path contains pathSub (resolved against its CURRENT on-disk
// fingerprint — see testStore's doc comment).
func ledgerHas(t *testing.T, ts *testStore, status store.Status, pathSub string) bool {
	t.Helper()
	path := ts.findPath(t, pathSub)
	if path == "" {
		return false
	}
	got, _, exists, err := ts.Get(context.Background(), path, probe.Fingerprint(path))
	if err != nil {
		t.Fatalf("store.Get(%s): %v", path, err)
	}
	return exists && got == status
}

// skipReason returns the recorded Outcome.Reason for the file whose path contains
// pathSub (resolved against its current on-disk fingerprint). "" if there is no row.
func skipReason(t *testing.T, ts *testStore, pathSub string) string {
	t.Helper()
	path := ts.findPath(t, pathSub)
	if path == "" {
		return ""
	}
	rows, err := ts.List(context.Background(), []store.Status{store.Skipped}, 0)
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	for _, r := range rows {
		if r.Path == path {
			return r.Outcome.Reason
		}
	}
	return ""
}

// failCount returns the fail_count recorded for the file whose path contains
// pathSub. root is needed because failCount is called across multiple RunOneshot
// passes in TestCase13, where the file's fingerprint is stable (the encode always
// fails, so the source is never mutated) — so a single lookup by current
// fingerprint is correct on every call.
func failCount(t *testing.T, ts *testStore, pathSub string) int {
	t.Helper()
	path := ts.findPath(t, pathSub)
	if path == "" {
		return 0
	}
	_, fc, exists, err := ts.Get(context.Background(), path, probe.Fingerprint(path))
	if err != nil {
		t.Fatalf("store.Get(%s): %v", path, err)
	}
	if !exists {
		return 0
	}
	return fc
}

// ---- harness -----------------------------------------------------------------

// baseCfg is a fully-explicit engine config for tests, so an explicit
// MinBitrateKbps=0 is honoured — matching the bash MIN_BITRATE_KBPS=0.
// Preset ultrafast keeps the real-libx265 cases fast; on a 320x240 clip it still
// shrinks an 8 Mbit source far below it, so the size guard is unaffected.
func baseCfg(root string) config.Config {
	return config.Config{
		LibraryRoots:         []string{root},
		VideoExts:            []string{"mkv", "mp4", "avi", "mov", "m4v", "ts", "m2ts", "wmv", "flv"},
		Encoder:              "cpu",
		CRF:                  22,
		Preset:               "ultrafast",
		PixelFormat:          "yuv420p10le",
		ContainerExt:         "mkv",
		MinBitrateKbps:       0,
		MinSavingsPercent:    0,
		DurationToleranceSec: 1,
		MaxFailures:          3,
		// VMAF is a second full decode; leave it OFF for the structural cases (they
		// assert the size/parity/decode gates). The VMAF gate is exercised by its own
		// dedicated cases below with VmafEnable=true + MinVmaf set.
		VmafEnable: boolPtr(false),
	}
}

func boolPtr(b bool) *bool { return &b }

// newTestStore opens a fresh SQLite-backed store under root (a temp DB file, not
// under root itself so it's never mistaken for a video file by the scan).
func newTestStore(t *testing.T, root string) *testStore {
	t.Helper()
	dbDir := t.TempDir() // sibling temp dir, never scanned as a library root
	st, err := store.Open(filepath.Join(dbDir, "jobs.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &testStore{SQLite: st, root: root}
}

// run builds an engine over root with the given encoder and config mutation, then
// runs one oneshot pass. Returns the store for assertions.
func run(t *testing.T, ffmpeg, ffprobe, root string, enc Encoder, mutate func(*config.Config)) *testStore {
	t.Helper()
	cfg := baseCfg(root)
	if mutate != nil {
		mutate(&cfg)
	}
	ts := newTestStore(t, root)
	prober := probe.New(ffmpeg, ffprobe)
	if enc == nil {
		enc = FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}
	}
	eng := New(cfg, prober, enc, ts, discardLogger())
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	return ts
}

func codecOf(t *testing.T, ffprobe, path string) string {
	// VideoCodec uses ffprobe only; let probe.New default the (unused) ffmpeg binary
	// rather than hardcode a literal here.
	return probe.New("", ffprobe).VideoCodec(context.Background(), path)
}

// buildEngine constructs an Engine over root without running it, so a test can set
// a seam (e.g. StaticMetadataIncomplete) before calling RunOneshot itself — mirrors
// the bash suite's TRANSCODER_TEST_HOOKS. t is needed to open the backing store
// (temp-dir cleanup); pass the *testing.T from the calling test.
func buildEngine(t *testing.T, ffmpeg, ffprobe, root string, enc Encoder, mutate func(*config.Config)) *Engine {
	t.Helper()
	cfg := baseCfg(root)
	if mutate != nil {
		mutate(&cfg)
	}
	ts := newTestStore(t, root)
	prober := probe.New(ffmpeg, ffprobe)
	if enc == nil {
		enc = FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}
	}
	return New(cfg, prober, enc, ts, discardLogger())
}

// ---- HDR / source-property fixture builders (real ffmpeg) --------------------

// mkH264HDR10 writes a NON-HEVC (h264) 10-bit HDR10 clip: bt2020/PQ colour tags +
// a complete mastering-display + MaxCLL block via -x264-params. Mirrors the bash
// mk_h264_hdr10 fixture recipe exactly.
func mkH264HDR10(t *testing.T, ffmpeg, path, bitrate string) {
	t.Helper()
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=320x240:rate=10",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", bitrate, "-pix_fmt", "yuv420p10le", "-profile:v", "high10",
		"-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc", "-color_range", "tv",
		"-x264-params", "mastering-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1):cll=1000,400",
		"--", path)
}

// mkH264SDR writes a NON-HEVC SDR (bt709) source with explicit colour tags — must
// survive the transcode unflattened and get NO HDR params. Mirrors bash mk_h264_sdr.
func mkH264SDR(t *testing.T, ffmpeg, path, bitrate string) {
	t.Helper()
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=320x240:rate=10",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", bitrate, "-pix_fmt", "yuv420p",
		"-colorspace", "bt709", "-color_primaries", "bt709", "-color_trc", "bt709", "--", path)
}

// mkH264Interlaced writes a NON-HEVC interlaced source (field_order tt/bb/tb/bt).
func mkH264Interlaced(t *testing.T, ffmpeg, path, bitrate string) {
	t.Helper()
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=320x240:rate=10",
		"-vf", "tinterlace=4", "-c:v", "libx264", "-preset", "ultrafast", "-b:v", bitrate,
		"-pix_fmt", "yuv420p", "-flags", "+ilme+ildct", "-field_order", "tt", "--", path)
}

// mkH264Chroma422 writes a NON-HEVC 4:2:2 8-bit source.
func mkH264Chroma422(t *testing.T, ffmpeg, path, bitrate string) {
	t.Helper()
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=320x240:rate=10",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", bitrate, "-pix_fmt", "yuv422p", "--", path)
}

// mkMP4WithSubs writes an MP4 with an h264 video track and a mov_text subtitle
// track (the container type that doesn't round-trip cleanly into MKV).
func mkMP4WithSubs(t *testing.T, ffmpeg, path, bitrate string) {
	t.Helper()
	srtPath := path + ".srt"
	if err := os.WriteFile(srtPath, []byte("1\n00:00:00,000 --> 00:00:01,000\nHello\n\n"), 0o644); err != nil {
		t.Fatalf("write srt: %v", err)
	}
	defer os.Remove(srtPath)
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=duration=2:size=320x240:rate=10",
		"-i", srtPath,
		"-map", "0:v", "-map", "1:s", "-c:v", "libx264", "-preset", "ultrafast", "-b:v", bitrate,
		"-pix_fmt", "yuv420p", "-c:s", "mov_text", "--", path)
}

// mkH264VFR writes a NON-HEVC variable-frame-rate source (frame-selective drop +
// fps_mode vfr on encode), so a naive forced-CFR pipeline would be exercised.
func mkH264VFR(t *testing.T, ffmpeg, path, bitrate string) {
	t.Helper()
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=320x240:rate=30",
		"-vf", "select='not(mod(n\\,3))',setpts=N/(30*TB)",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", bitrate, "-pix_fmt", "yuv420p",
		"-fps_mode", "vfr", "--", path)
}

// mkHevcDVTagged writes an HEVC clip tagged with the Dolby Vision codec tag dvh1 —
// a real, buildable DV *signal* (the RPU itself needs an external toolchain and
// cannot be synthesized with ffmpeg+libx265, but the codec-tag detection path can be
// proven against a real file).
func mkHevcDVTagged(t *testing.T, ffmpeg, path string) {
	t.Helper()
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=1:size=320x240:rate=10",
		"-c:v", "libx265", "-pix_fmt", "yuv420p10le", "-x265-params", "log-level=error",
		"-tag:v", "dvh1", "--", path)
}

// ---- HDR / source-property assertion helpers ----------------------------------

// frameColor reads one colour tag from the first video frame (robust across
// MKV/MP4 — MKV keeps colour tags at frame level, not the container stream
// header). Mirrors the bash frame_color helper.
func frameColor(t *testing.T, ffprobe, path, field string) string {
	t.Helper()
	out, err := exec.Command(ffprobe, "-v", "error", "-select_streams", "v:0",
		"-read_intervals", "%+#1", "-show_frames", "-show_entries", "frame="+field,
		"-of", "default=nw=1:nk=1", "--", path).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
}

// hasSideData reports whether the first video frame carries a side-data block
// whose type contains want. Mirrors the bash has_sd helper.
func hasSideData(t *testing.T, ffprobe, path, want string) bool {
	t.Helper()
	out, err := exec.Command(ffprobe, "-v", "error", "-select_streams", "v:0",
		"-read_intervals", "%+#1", "-show_frames", "-show_entries", "frame=side_data_list",
		"-of", "default=nw=1", "--", path).Output()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), strings.ToLower(want))
}

// ---- the cases ---------------------------------------------------------------

func TestCase1_AlreadyHEVCSkipped(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	mkHevc(t, ffmpeg, filepath.Join(d, "movie.mkv"), "400k")
	before := md5f(t, filepath.Join(d, "movie.mkv"))
	led := run(t, ffmpeg, ffprobe, d, nil, nil)
	if md5f(t, filepath.Join(d, "movie.mkv")) != before {
		t.Error("already-HEVC source was modified")
	}
	if !ledgerHas(t, led, store.Skipped, "movie.mkv") {
		t.Error("expected skipped row")
	}
	if nTemp(t, d) != 0 {
		t.Error("temp left behind")
	}
}

func TestCase2_EncodeErrorSourceUntouched(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	mkH264(t, ffmpeg, filepath.Join(d, "movie.mkv"), "3M")
	before := md5f(t, filepath.Join(d, "movie.mkv"))
	enc := EncoderFunc(func(ctx context.Context, in, out string) error { return errFake })
	led := run(t, ffmpeg, ffprobe, d, enc, nil)
	if md5f(t, filepath.Join(d, "movie.mkv")) != before {
		t.Error("source modified after encode failure")
	}
	if codecOf(t, ffprobe, filepath.Join(d, "movie.mkv")) != "h264" {
		t.Error("source no longer h264 (should be untouched)")
	}
	if nTemp(t, d) != 0 {
		t.Error("temp not discarded")
	}
	if !ledgerHas(t, led, store.Failed, "movie.mkv") {
		t.Error("expected failed row")
	}
}

func TestCase3_CorruptOutputRejected(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	mkH264(t, ffmpeg, filepath.Join(d, "movie.mkv"), "3M")
	before := md5f(t, filepath.Join(d, "movie.mkv"))
	enc := EncoderFunc(func(ctx context.Context, in, out string) error {
		return os.WriteFile(out, make([]byte, 200), 0o644) // garbage, not a video
	})
	led := run(t, ffmpeg, ffprobe, d, enc, nil)
	if md5f(t, filepath.Join(d, "movie.mkv")) != before {
		t.Error("source modified by a corrupt output")
	}
	if nTemp(t, d) != 0 {
		t.Error("temp not discarded")
	}
	if !ledgerHas(t, led, store.Failed, "movie.mkv") {
		t.Error("expected failed row")
	}
}

func TestCase4_LargerOutputRejected(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	mkH264(t, ffmpeg, filepath.Join(d, "movie.mkv"), "300k") // small source
	big := filepath.Join(d, "big.mkv")
	// Same-duration (2s) but 720p LOSSLESS HEVC — reliably larger than the 240p
	// source regardless of x265 ABR, so this isolates the SIZE guard.
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=1280x720:rate=10",
		"-c:v", "libx265", "-x265-params", "lossless=1:log-level=error", "-pix_fmt", "yuv420p", "--", big)
	if probe.FileSize(big) <= probe.FileSize(filepath.Join(d, "movie.mkv")) {
		t.Fatal("fixture not actually larger")
	}
	before := md5f(t, filepath.Join(d, "movie.mkv"))
	enc := EncoderFunc(func(ctx context.Context, in, out string) error {
		b, err := os.ReadFile(big)
		if err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	})
	led := run(t, ffmpeg, ffprobe, d, enc, nil)
	if md5f(t, filepath.Join(d, "movie.mkv")) != before {
		t.Error("source modified by a larger output")
	}
	if nTemp(t, d) != 0 {
		t.Error("temp not discarded")
	}
	if !ledgerHas(t, led, store.Failed, "movie.mkv") {
		t.Error("expected failed row")
	}
}

func TestCase5_GoodSmallerSwapAndCase6_Resume(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M") // inflated so real libx265 beats it
	inSize := probe.FileSize(src)
	led := run(t, ffmpeg, ffprobe, d, nil, nil) // REAL libx265
	if codecOf(t, ffprobe, src) != "hevc" {
		t.Fatal("case5: output is not HEVC")
	}
	if probe.FileSize(src) >= inSize {
		t.Error("case5: output not smaller")
	}
	if !ledgerHas(t, led, store.Done, "movie.mkv") {
		t.Error("case5: expected done row")
	}
	if nTemp(t, d) != 0 {
		t.Error("case5: temp left")
	}
	sum := md5f(t, src)

	// case 6: a second pass must not reprocess or corrupt the case-5 result.
	run(t, ffmpeg, ffprobe, d, nil, nil)
	if md5f(t, src) != sum {
		t.Error("case6: resume changed the file")
	}
	if nTemp(t, d) != 0 {
		t.Error("case6: resume created a temp")
	}
}

func TestCase7_LowBitrateSkipped(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mp4") // mp4 reports bitrate reliably
	mkH264(t, ffmpeg, src, "500k")
	before := md5f(t, src)
	led := run(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) { c.MinBitrateKbps = 100000 })
	if md5f(t, src) != before {
		t.Error("low-bitrate source modified")
	}
	if !ledgerHas(t, led, store.Skipped, "movie.mp4") {
		t.Error("expected skipped row")
	}
}

func TestCase8_OrphanedTempDiscarded(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	mkHevc(t, ffmpeg, filepath.Join(d, "movie.mkv"), "400k")
	// leftover from a prior killed run
	if err := os.WriteFile(filepath.Join(d, "movie."+TempMarker+".mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := md5f(t, filepath.Join(d, "movie.mkv"))
	run(t, ffmpeg, ffprobe, d, nil, nil)
	if nTemp(t, d) != 0 {
		t.Error("orphaned temp not discarded")
	}
	if md5f(t, filepath.Join(d, "movie.mkv")) != before {
		t.Error("real source modified")
	}
}

func TestCase9_UnreadableNeverDeleted(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	if err := os.WriteFile(src, []byte("not a video"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := md5f(t, src)
	led := run(t, ffmpeg, ffprobe, d, nil, nil)
	if md5f(t, src) != before {
		t.Error("non-video source modified")
	}
	if !ledgerHas(t, led, store.Failed, "movie.mkv") {
		t.Error("expected failed row")
	}
}

func TestCase10_CollisionVsHEVCMasterNotClobbered(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	mkHevc(t, ffmpeg, filepath.Join(d, "movie.mkv"), "400k") // precious HEVC master
	mkH264(t, ffmpeg, filepath.Join(d, "movie.mp4"), "3M")   // same-basename non-HEVC dupe
	mkvBefore := md5f(t, filepath.Join(d, "movie.mkv"))
	mp4Before := md5f(t, filepath.Join(d, "movie.mp4"))
	led := run(t, ffmpeg, ffprobe, d, nil, nil)
	if md5f(t, filepath.Join(d, "movie.mkv")) != mkvBefore {
		t.Error("HEVC master was clobbered")
	}
	if codecOf(t, ffprobe, filepath.Join(d, "movie.mkv")) != "hevc" {
		t.Error("master no longer HEVC")
	}
	if md5f(t, filepath.Join(d, "movie.mp4")) != mp4Before {
		t.Error("mp4 dupe was modified")
	}
	if !exists(filepath.Join(d, "movie.mkv")) || !exists(filepath.Join(d, "movie.mp4")) {
		t.Error("a file went missing")
	}
	if !ledgerHas(t, led, store.Skipped, "movie.mp4") {
		t.Error("expected skipped row for the collision")
	}
}

func TestCase11_TwoDupesDoNotCollapse(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	mkH264(t, ffmpeg, filepath.Join(d, "clip.mkv"), "8M") // inflated so libx265 shrinks it
	mkH264(t, ffmpeg, filepath.Join(d, "clip.mp4"), "3M")
	mp4Before := md5f(t, filepath.Join(d, "clip.mp4"))
	run(t, ffmpeg, ffprobe, d, nil, nil)
	if !exists(filepath.Join(d, "clip.mkv")) || !exists(filepath.Join(d, "clip.mp4")) {
		t.Error("a file went missing")
	}
	if codecOf(t, ffprobe, filepath.Join(d, "clip.mkv")) != "hevc" {
		t.Error("clip.mkv not transcoded in place to HEVC")
	}
	if md5f(t, filepath.Join(d, "clip.mp4")) != mp4Before || codecOf(t, ffprobe, filepath.Join(d, "clip.mp4")) != "h264" {
		t.Error("clip.mp4 dupe was not left untouched")
	}
}

func TestCase12_HardlinkedSkipped(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	if err := os.Link(src, filepath.Join(d, "seed.mkv")); err != nil {
		t.Fatalf("hardlink: %v", err)
	}
	before := md5f(t, src)
	led := run(t, ffmpeg, ffprobe, d, nil, nil)
	if codecOf(t, ffprobe, src) != "h264" {
		t.Error("hardlinked source was transcoded")
	}
	if md5f(t, src) != before {
		t.Error("hardlinked source modified")
	}
	if !exists(src) || !exists(filepath.Join(d, "seed.mkv")) {
		t.Error("a link went missing")
	}
	// TRANSCODE-14: the hardlink skip is now RECORDED as a skipped/"hardlinked" row so
	// the dashboard can show WHICH guard fired — it must not read as a bare "skipped".
	// The source is still byte-for-byte intact (asserted above); recording the reason
	// is report-only and never claims or touches the file.
	if !ledgerHas(t, led, store.Skipped, "movie.mkv") {
		t.Error("hardlinked file must be recorded as skipped (so the UI can show the guard)")
	}
	if got := skipReason(t, led, "movie.mkv"); got != SkipHardlinked {
		t.Errorf("hardlink skip reason = %q, want %q", got, SkipHardlinked)
	}
}

// TestCase12b_HardlinkSkipIsReEvaluatedWhenTheSeedFinishes proves the recorded
// hardlink skip does NOT permanently park the file: once the extra link is gone (the
// seed finished), the next scan clears the stale skip and reclaims the file. This is
// the property that let the guard stay unrecorded before TRANSCODE-14 — it must
// survive the row now being written.
func TestCase12b_HardlinkSkipIsReEvaluatedWhenTheSeedFinishes(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	seed := filepath.Join(d, "seed.mkv")
	if err := os.Link(src, seed); err != nil {
		t.Fatalf("hardlink: %v", err)
	}

	// One engine over ONE store, run twice across a filesystem change (the same backing
	// store must persist between scans, so build it directly rather than via run()).
	ts := newTestStore(t, d)
	cfg := baseCfg(d)
	prober := probe.New(ffmpeg, ffprobe)
	eng := New(cfg, prober, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}, ts, discardLogger())
	scan := func() {
		if err := eng.RunOneshot(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	// Scan 1: hardlinked → skipped(hardlinked), source untouched.
	scan()
	if codecOf(t, ffprobe, src) != "h264" {
		t.Fatal("scan 1: hardlinked source was transcoded")
	}
	if got := skipReason(t, ts, "movie.mkv"); got != SkipHardlinked {
		t.Fatalf("scan 1: reason = %q, want %q", got, SkipHardlinked)
	}

	// The seed finishes: remove the extra link. The file's content — and therefore its
	// fingerprint — is unchanged, so a permanent skip row would park it forever.
	if err := os.Remove(seed); err != nil {
		t.Fatalf("remove seed: %v", err)
	}

	// Scan 2: no longer hardlinked → the stale skip is cleared and the file is reclaimed.
	scan()
	if codecOf(t, ffprobe, src) != "hevc" {
		t.Error("scan 2: file was not transcoded after the seed finished (stale hardlink skip not cleared)")
	}
}

func TestCase13_FailedRetriedThenParked(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "3M")
	before := md5f(t, src)
	enc := EncoderFunc(func(ctx context.Context, in, out string) error { return errFake })
	ts := newTestStore(t, d)
	cfg := baseCfg(d)
	cfg.MaxFailures = 3
	prober := probe.New(ffmpeg, ffprobe)
	do := func() {
		eng := New(cfg, prober, enc, ts, discardLogger())
		if err := eng.RunOneshot(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	do()
	if failCount(t, ts, "movie.mkv") != 1 {
		t.Fatalf("attempt 1: failCount=%d want 1", failCount(t, ts, "movie.mkv"))
	}
	do()
	if failCount(t, ts, "movie.mkv") != 2 {
		t.Fatalf("attempt 2 (retry): failCount=%d want 2", failCount(t, ts, "movie.mkv"))
	}
	do()
	if failCount(t, ts, "movie.mkv") != 3 {
		t.Fatalf("attempt 3: failCount=%d want 3", failCount(t, ts, "movie.mkv"))
	}
	do() // parked now — no new attempt
	if failCount(t, ts, "movie.mkv") != 3 {
		t.Fatalf("after MAX_FAILURES: failCount=%d want 3 (parked)", failCount(t, ts, "movie.mkv"))
	}
	if md5f(t, src) != before {
		t.Error("source modified across retries")
	}
}

func TestCase14_TabPathSkipped(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	tabName := filepath.Join(d, "mo\tvie.mkv")
	mkH264(t, ffmpeg, tabName, "8M")
	before := md5f(t, tabName)
	ts := run(t, ffmpeg, ffprobe, d, nil, nil)
	if md5f(t, tabName) != before {
		t.Error("tab-named file modified")
	}
	if codecOf(t, ffprobe, tabName) != "h264" {
		t.Error("tab-named file transcoded")
	}
	_, _, exists, err := ts.Get(context.Background(), tabName, probe.Fingerprint(tabName))
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if exists {
		t.Error("store should have no row for the tab-named path (unrecorded)")
	}
}

func TestCase15_UnknownDurationTranscodes(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	raw := filepath.Join(d, "movie.h264")
	// raw annexb h264: no container timing -> unknown duration -> packet-parity path
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=320x240:rate=10",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "8M", "-bsf:v", "h264_mp4toannexb", "-f", "h264", "--", raw)
	led := run(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) { c.VideoExts = []string{"h264"} })
	out := filepath.Join(d, "movie.mkv")
	if !exists(out) || codecOf(t, ffprobe, out) != "hevc" {
		t.Error("unknown-duration source not transcoded to HEVC")
	}
	if exists(raw) {
		t.Error("original raw source not removed")
	}
	if !ledgerHas(t, led, store.Done, "movie.mkv") {
		t.Error("expected done row")
	}
}

func TestCase16_TruncatedUnknownDurationRejected(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	raw := filepath.Join(d, "movie.h264")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=320x240:rate=10",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "8M", "-bsf:v", "h264_mp4toannexb", "-f", "h264", "--", raw)
	before := md5f(t, raw)
	// A clean-but-short HEVC (3 frames): decodes fine, smaller, right codec — only
	// packet-count parity can reject it (source has ~20 frames).
	enc := EncoderFunc(func(ctx context.Context, in, out string) error {
		return exec.Command(ffmpeg, "-hide_banner", "-nostdin", "-v", "error", "-y", "-i", in,
			"-c:v", "libx265", "-frames:v", "3", "-x265-params", "log-level=error", "--", out).Run()
	})
	led := run(t, ffmpeg, ffprobe, d, enc, func(c *config.Config) { c.VideoExts = []string{"h264"} })
	if !exists(raw) || md5f(t, raw) != before {
		t.Error("truncated encode: source not intact")
	}
	if exists(filepath.Join(d, "movie.mkv")) {
		t.Error("truncated encode: a HEVC output was swapped in")
	}
	if !ledgerHas(t, led, store.Failed, "movie.h264") {
		t.Error("expected failed row")
	}
}

func TestCase17_DroppedAudioTrackRejected(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	// source: 1 video + 1 audio
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=duration=2:size=320x240:rate=10",
		"-f", "lavfi", "-i", "sine=frequency=1000:duration=2",
		"-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-preset", "ultrafast", "-b:v", "8M",
		"-pix_fmt", "yuv420p", "-c:a", "aac", "--", src)
	prober := probe.New(ffmpeg, ffprobe)
	if got := prober.StreamCount(context.Background(), src, "a"); got != 1 {
		t.Fatalf("source audio streams = %d, want 1", got)
	}
	before := md5f(t, src)
	// hooked encode maps video only -> a valid, smaller, right-duration HEVC with NO
	// audio; only per-type stream-count parity can catch the loss.
	enc := EncoderFunc(func(ctx context.Context, in, out string) error {
		return exec.Command(ffmpeg, "-hide_banner", "-nostdin", "-v", "error", "-y", "-i", in,
			"-map", "0:v:0", "-c:v", "libx265", "-x265-params", "log-level=error",
			"-pix_fmt", "yuv420p10le", "--", out).Run()
	})
	led := run(t, ffmpeg, ffprobe, d, enc, nil)
	if md5f(t, src) != before {
		t.Error("source modified after a dropped-track output")
	}
	if codecOf(t, ffprobe, src) != "h264" {
		t.Error("source was swapped despite the dropped track")
	}
	if prober.StreamCount(context.Background(), src, "a") != 1 {
		t.Error("source audio track lost")
	}
	if nTemp(t, d) != 0 {
		t.Error("temp not discarded")
	}
	if !ledgerHas(t, led, store.Failed, "movie.mkv") {
		t.Error("expected failed row")
	}
}

// --- case 18: non-HEVC HDR10 source -> HDR10 static metadata carried through -----
// The centrepiece of TRANSCODE-3: a generic re-encode silently drops HDR10 static
// metadata. Source is a NON-HEVC (h264) 10-bit clip with bt2020/PQ + mastering-
// display + MaxCLL, inflated so libx265 shrinks it (size guard passes). The output
// must be HEVC AND re-probe with the colour + HDR10 metadata intact.
func TestCase18_NonHEVCHDR10ColorPreserved(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264HDR10(t, ffmpeg, src, "8M")
	if !hasSideData(t, ffprobe, src, "Mastering display metadata") {
		t.Fatal("fixture not built with mastering-display")
	}
	if codecOf(t, ffprobe, src) != "h264" {
		t.Fatal("fixture is not non-HEVC (would be skipped before reaching the encode)")
	}
	inSize := probe.FileSize(src)
	led := run(t, ffmpeg, ffprobe, d, nil, nil)
	if codecOf(t, ffprobe, src) != "hevc" {
		t.Fatal("case18: output is not HEVC")
	}
	if probe.FileSize(src) >= inSize {
		t.Error("case18: output not smaller")
	}
	if frameColor(t, ffprobe, src, "color_transfer") != "smpte2084" {
		t.Error("case18: output transfer is not PQ (smpte2084)")
	}
	if frameColor(t, ffprobe, src, "color_primaries") != "bt2020" {
		t.Error("case18: output primaries are not bt2020")
	}
	if frameColor(t, ffprobe, src, "color_space") != "bt2020nc" {
		t.Error("case18: output matrix is not bt2020nc")
	}
	if !hasSideData(t, ffprobe, src, "Mastering display metadata") {
		t.Error("case18: output lost mastering-display")
	}
	if !hasSideData(t, ffprobe, src, "Content light level metadata") {
		t.Error("case18: output lost content-light (MaxCLL)")
	}
	if !ledgerHas(t, led, store.Done, "movie.mkv") {
		t.Error("case18: expected done row")
	}
}

// --- case 19: non-HEVC SDR source -> colour tags preserved, no HDR params ---------
// The other side of the coin: an SDR (bt709) source must not be flattened OR
// wrongly tagged HDR. Output stays HEVC, smaller, keeps bt709, and carries NO
// mastering-display (a generic re-encode must not invent HDR metadata). This test
// REDS if the "under-signalled HDR10 defaults" branch in DeriveColorArgs is ever
// applied unconditionally instead of gated on smpte2084/mastering-display.
func TestCase19_NonHEVCSDRNoInventedHDR(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264SDR(t, ffmpeg, src, "8M")
	inSize := probe.FileSize(src)
	led := run(t, ffmpeg, ffprobe, d, nil, nil)
	if codecOf(t, ffprobe, src) != "hevc" {
		t.Fatal("case19: output is not HEVC")
	}
	if probe.FileSize(src) >= inSize {
		t.Error("case19: output not smaller")
	}
	if frameColor(t, ffprobe, src, "color_space") != "bt709" {
		t.Error("case19: output lost bt709 matrix")
	}
	if hasSideData(t, ffprobe, src, "Mastering display metadata") {
		t.Error("case19: SDR output must NOT carry an invented mastering-display block")
	}
	if !ledgerHas(t, led, store.Done, "movie.mkv") {
		t.Error("case19: expected done row")
	}
}

// --- case 20: DV detection (Classify + ClassFrom across every branch) ------------
// The end-to-end DV/HDR10+ SKIP cannot be fixture-driven: a NON-HEVC DV or HDR10+
// source (the only kind that reaches the guard — HEVC HDR is skipped at case 1)
// can't be produced with ffmpeg+libx265 (DV needs an external RPU toolchain;
// HDR10+ needs a libx265 built with libhdr10plus). So detection is proven against
// the shipping classifier two ways: (a) hdr.Classify on a real DV-codec-tagged
// file, (b) hdr.ClassFrom across every branch (also unit-tested directly in
// internal/hdr/hdr_test.go — repeated here against the real Prober plumbing).
func TestCase20_DVDetection(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	dv := filepath.Join(d, "dv.mp4")
	mkHevcDVTagged(t, ffmpeg, dv)
	prober := probe.New(ffmpeg, ffprobe)
	if got := hdr.Classify(context.Background(), prober, dv); got != hdr.ClassDV {
		t.Errorf("case20: hdr.Classify on a dvh1-tagged file = %q, want %q", got, hdr.ClassDV)
	}

	cases := []struct {
		name string
		tag  string
		flat string
		trc  string
		want string
	}{
		{"dvh1 tag -> dv", "dvh1", "", "", hdr.ClassDV},
		{"dvhe tag -> dv", "dvhe", "", "", hdr.ClassDV},
		{"DOVI config record -> dv", "hev1", `side_data_type="DOVI configuration record"`, "", hdr.ClassDV},
		{"SMPTE2094-40 -> hdr10plus", "hev1", "HDR Dynamic Metadata SMPTE2094-40 (HDR10+)", "", hdr.ClassHDR10Plus},
		{"PQ transfer -> hdr10", "hev1", "", "smpte2084", hdr.ClassHDR10},
		{"mastering-display -> hdr10", "hev1", `side_data_type="Mastering display metadata"`, "bt709", hdr.ClassHDR10},
		{"plain bt709 -> other", "hev1", "", "bt709", hdr.ClassOther},
	}
	for _, tc := range cases {
		if got := hdr.ClassFrom(tc.tag, tc.flat, tc.trc); got != tc.want {
			t.Errorf("case20: %s: ClassFrom = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// --- case 21: numeric mapping unit tests -----------------------------------------
// Covered directly (and more exhaustively) in internal/hdr/hdr_test.go
// (TestMasterDisplay_KnownBlob, TestMaxCLL_KnownBlob, TestMasterDisplay_PartialIsEmpty,
// TestStaticMetadataIncomplete) against the exact bash case-21 blob. No engine-level
// fixture is needed since these are pure functions — see that file.

// --- case 22: the HDR10-incomplete SKIP is actually WIRED into ProcessFile -------
// Case 21 (in internal/hdr) unit-tests the predicate; this proves ProcessFile's
// hdr10 branch actually calls it and SKIPS on true. A real non-HEVC PARTIAL-
// metadata source can't be synthesized reliably, so the test seam
// (Engine.StaticMetadataIncomplete) forces the predicate true and asserts a
// COMPLETE HDR10 source is skipped untouched — reverting the wiring in ProcessFile
// would transcode it and RED these checks.
func TestCase22_HDR10IncompleteSkipWired(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264HDR10(t, ffmpeg, src, "8M")
	before := md5f(t, src)

	eng := buildEngine(t, ffmpeg, ffprobe, d, nil, nil)
	ts := eng.Store.(*testStore)
	eng.staticMetadataIncomplete = func(flat string) bool { return true }
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	if md5f(t, src) != before {
		t.Error("case22: incomplete-HDR10 source was modified")
	}
	if codecOf(t, ffprobe, src) != "h264" {
		t.Error("case22: source was transcoded despite the forced-incomplete predicate")
	}
	if !ledgerHas(t, ts, store.Skipped, "movie.mkv") {
		t.Error("case22: expected skipped row (not failed/done)")
	}
	if nTemp(t, d) != 0 {
		t.Error("case22: temp left behind")
	}
}

// --- case (a): interlaced source is SKIPPED, never deinterlaced ------------------
// This tool never deinterlaces; re-encoding an interlaced source with a
// progressive-assuming pipeline bakes in permanent combing artifacts. REDS if the
// field_order guard in ProcessFile is removed.
func TestCaseA_InterlacedSkipped(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264Interlaced(t, ffmpeg, src, "8M")
	prober := probe.New(ffmpeg, ffprobe)
	switch fo := prober.FieldOrder(context.Background(), src); fo {
	case "tt", "bb", "tb", "bt":
		// fixture confirmed interlaced
	default:
		t.Fatalf("fixture not interlaced (field_order=%q)", fo)
	}
	before := md5f(t, src)
	led := run(t, ffmpeg, ffprobe, d, nil, nil)
	if md5f(t, src) != before {
		t.Error("caseA: interlaced source was modified")
	}
	if codecOf(t, ffprobe, src) != "h264" {
		t.Error("caseA: interlaced source was transcoded")
	}
	if !ledgerHas(t, led, store.Skipped, "movie.mkv") {
		t.Error("caseA: expected skipped row")
	}
	if nTemp(t, d) != 0 {
		t.Error("caseA: temp left behind")
	}
}

// --- case (b): 4:2:2 source transcodes and PRESERVES chroma subsampling ----------
// A naive fixed yuv420p10le output would silently subsample a 4:2:2 source. REDS if
// hdr.DerivePixFmt (or its wiring into FFmpegEncoder) stops preserving chroma
// subsampling.
func TestCaseB_Chroma422Preserved(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264Chroma422(t, ffmpeg, src, "8M")
	prober := probe.New(ffmpeg, ffprobe)
	if got := prober.PixFmt(context.Background(), src); got != "yuv422p" {
		t.Fatalf("fixture not 4:2:2 (pix_fmt=%q)", got)
	}
	inSize := probe.FileSize(src)
	// PixelFormat must be "auto" to exercise per-source derivation — baseCfg forces
	// yuv420p10le (TRANSCODE-1 back-compat default), which would flatten 4:2:2.
	led := run(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) { c.PixelFormat = "auto" })
	if codecOf(t, ffprobe, src) != "hevc" {
		t.Fatal("caseB: 4:2:2 source not transcoded")
	}
	if probe.FileSize(src) >= inSize {
		t.Error("caseB: output not smaller")
	}
	if got := prober.PixFmt(context.Background(), src); got != "yuv422p10le" {
		t.Errorf("caseB: output pix_fmt = %q, want yuv422p10le (chroma subsampling must be preserved, not flattened to 4:2:0)", got)
	}
	if !ledgerHas(t, led, store.Done, "movie.mkv") {
		t.Error("caseB: expected done row")
	}
}

// --- case (c): MP4 with mov_text subtitles transcodes in place (.mp4), track kept -
// Container-match by default: the output container = the source's own extension, so
// an MP4 with mov_text subtitles is never forced into MKV (which cannot carry
// mov_text) and aborted by the stream-count parity gate. REDS if ContainerExt
// stops defaulting to "source" or the container-match wiring in ProcessFile regresses.
func TestCaseC_MP4SubtitlesContainerMatch(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mp4")
	mkMP4WithSubs(t, ffmpeg, src, "8M")
	prober := probe.New(ffmpeg, ffprobe)
	if got := prober.StreamCount(context.Background(), src, "s"); got != 1 {
		t.Fatalf("fixture does not have exactly 1 subtitle stream (got %d)", got)
	}
	inSize := probe.FileSize(src)
	// ContainerExt left at the "source" sentinel (default Load() behaviour) — do
	// NOT force mkv here, that's the whole point of this case.
	led := run(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) { c.ContainerExt = "source" })
	if !exists(src) {
		t.Fatal("caseC: movie.mp4 no longer exists (container-match should keep the .mp4 path)")
	}
	if exists(filepath.Join(d, "movie.mkv")) {
		t.Error("caseC: an unexpected movie.mkv was created — container-match should stay .mp4")
	}
	if codecOf(t, ffprobe, src) != "hevc" {
		t.Fatal("caseC: output is not HEVC")
	}
	if probe.FileSize(src) >= inSize {
		t.Error("caseC: output not smaller")
	}
	if got := prober.StreamCount(context.Background(), src, "s"); got != 1 {
		t.Error("caseC: subtitle track was dropped")
	}
	if !ledgerHas(t, led, store.Done, "movie.mp4") {
		t.Error("caseC: expected done row")
	}
}

// --- case (d): VFR source is not false-rejected ----------------------------------
// -fps_mode passthrough must be wired into the encode so a variable-frame-rate
// source is not forced to CFR (which would fail duration/packet parity or silently
// alter timing). REDS if -fps_mode passthrough is removed from FFmpegEncoder.Encode.
func TestCaseD_VFRNotFalseRejected(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264VFR(t, ffmpeg, src, "8M")
	inSize := probe.FileSize(src)
	led := run(t, ffmpeg, ffprobe, d, nil, nil)
	if codecOf(t, ffprobe, src) != "hevc" {
		t.Fatal("caseD: VFR source not transcoded to HEVC")
	}
	if probe.FileSize(src) >= inSize {
		t.Error("caseD: output not smaller")
	}
	if !ledgerHas(t, led, store.Done, "movie.mkv") {
		t.Error("caseD: expected done row (VFR source false-rejected by a parity/timing gate)")
	}
	if nTemp(t, d) != 0 {
		t.Error("caseD: temp left behind")
	}
}

// --- case (e): exotic pix_fmt source is SKIPPED, never silently subsampled ------
// ProcessFile's chroma/bit-depth guard must catch an unrecognized source pix_fmt
// BEFORE the encoder is even invoked (Encode's own refusal in encode_test.go is a
// defence-in-depth backstop, not the primary guard). REDS if the pix_fmt guard in
// ProcessFile is removed — the file would then reach Encode and fail loud there
// instead of being cleanly skipped-and-recorded.
func TestCaseE_ExoticPixFmtSkipped(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	// yuv411p via ffv1 — libx264/libx265 can't produce it, so ffv1 is the only way
	// to actually land this exotic pix_fmt on disk as a real (non-HEVC) source.
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=1:size=320x240:rate=10",
		"-c:v", "ffv1", "-pix_fmt", "yuv411p", "--", src)
	prober := probe.New(ffmpeg, ffprobe)
	if got := prober.PixFmt(context.Background(), src); got != "yuv411p" {
		t.Fatalf("fixture not yuv411p (pix_fmt=%q)", got)
	}
	before := md5f(t, src)
	led := run(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) { c.PixelFormat = "auto" })
	if md5f(t, src) != before {
		t.Error("caseE: exotic-pix_fmt source was modified")
	}
	if codecOf(t, ffprobe, src) != "ffv1" {
		t.Error("caseE: exotic-pix_fmt source was transcoded")
	}
	if !ledgerHas(t, led, store.Skipped, "movie.mkv") {
		t.Error("caseE: expected skipped row")
	}
	if nTemp(t, d) != 0 {
		t.Error("caseE: temp left behind")
	}
}

// ---- VMAF perceptual-quality gate (TRANSCODE-4) ------------------------------

// degradedEncoder produces a same-resolution but heavily-degraded HEVC (downscale
// to 64x48 then back to 320x240 + high CRF) — structurally valid (right codec,
// decodes, smaller, same duration/streams) but perceptually bad, so ONLY the VMAF
// gate can reject it. This is the anti-advisory proof for the VMAF gate.
func degradedEncoder(ffmpeg string) EncoderFunc {
	return func(ctx context.Context, in, out string) error {
		return exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-nostdin", "-v", "error", "-y", "-i", in,
			"-vf", "scale=64:48,scale=320:240:flags=neighbor",
			"-c:v", "libx265", "-crf", "45", "-x265-params", "log-level=error",
			"-pix_fmt", "yuv420p10le", "--", out).Run()
	}
}

func TestVmaf_RejectsDegradedOutput(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	before := md5f(t, src)
	led := run(t, ffmpeg, ffprobe, d, degradedEncoder(ffmpeg), func(c *config.Config) {
		c.VmafEnable = boolPtr(true)
		c.MinVmaf = 95
	})
	if md5f(t, src) != before {
		t.Error("source modified by a low-VMAF output")
	}
	if codecOf(t, ffprobe, src) != "h264" {
		t.Error("source was swapped despite a low VMAF")
	}
	if nTemp(t, d) != 0 {
		t.Error("temp not discarded")
	}
	if !ledgerHas(t, led, store.Failed, "movie.mkv") {
		t.Error("expected a failed row (VMAF rejection)")
	}
}

func TestVmaf_AcceptsNormalEncode(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	// Real libx265 crf22 encode + VMAF gate on: a faithful encode scores ~99 → accepted.
	led := run(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) {
		c.VmafEnable = boolPtr(true)
		c.MinVmaf = 95
	})
	if codecOf(t, ffprobe, src) != "hevc" {
		t.Error("a normal encode was not accepted under the VMAF gate")
	}
	if !ledgerHas(t, led, store.Done, "movie.mkv") {
		t.Error("expected a done row")
	}
}

// ---- the worst-frame floor (TRANSCODE-11) ------------------------------------

// locallyBrokenEncoder is the adversary the pooled mean cannot see.
//
// It produces a HIGH-QUALITY HEVC encode (crf 18 — the bulk of the frames score ~99)
// with a SHORT DESTROYED SEGMENT inside it: frames 100-103 are replaced by a 56x42
// nearest-neighbour upscale, a blocky ruin scoring VMAF ~43. Everything else about
// the file is impeccable, and that is the point:
//
//   - every STRUCTURAL gate passes — right codec, decodes cleanly under
//     -xerror -err_detect +explode, strictly smaller, identical duration, identical
//     packet count (the overlay is frame-for-frame), identical stream counts;
//   - the POOLED HARMONIC MEAN passes — 4 ruined frames out of 240 pool to ~97.5,
//     comfortably clear of min_vmaf=95.
//
// So on the shipped pre-TRANSCODE-11 defaults this file is ACCEPTED, the source is
// atomically swapped, and the original is DELETED. Only the worst-frame floor sees
// it. In a real 2-hour film the same arithmetic buys an attacker — or a flaky
// encoder — over a minute of ruined video through the same gate.
func locallyBrokenEncoder(ffmpeg string) EncoderFunc {
	return func(ctx context.Context, in, out string) error {
		return exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-nostdin", "-v", "error", "-y", "-i", in,
			"-filter_complex",
			"[0:v]split=2[cl][dm];"+
				"[dm]scale=56:42,scale=320:240:flags=neighbor[bad];"+
				"[cl][bad]overlay=enable='between(n,100,103)'[v]",
			"-map", "[v]",
			"-c:v", "libx265", "-crf", "18", "-preset", "veryfast", "-x265-params", "log-level=error",
			"-pix_fmt", "yuv420p10le", "--", out).Run()
	}
}

// TestVmaf_MeanOnlyGateIsBlindToLocalDamage is the RED: it pins the bug in place.
//
// With the worst-frame floor off (vmaf_min_pool=0 — what the repo shipped before
// TRANSCODE-11), a locally-broken encode sails through every gate and the source is
// destroyed. This test asserts that broken behaviour ON PURPOSE, because it is the
// only thing that makes the next test meaningful: without it, a green
// "floor rejects the broken encode" could be the MEAN doing the rejecting, and the
// floor could be dead code. This proves the mean genuinely cannot see the damage.
func TestVmaf_MeanOnlyGateIsBlindToLocalDamage(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264Long(t, ffmpeg, src, "8M")
	led := run(t, ffmpeg, ffprobe, d, locallyBrokenEncoder(ffmpeg), func(c *config.Config) {
		c.VmafEnable = boolPtr(true)
		c.MinVmaf = 95
		c.VmafMinPool = 0 // the pre-TRANSCODE-11 default: mean pooling is the sole gate
	})
	// The source is GONE — swapped for a file with four destroyed frames in it.
	if codecOf(t, ffprobe, src) != "hevc" {
		t.Fatal("fixture is not exercising the blind spot: the mean-only gate rejected the " +
			"locally-broken encode, so the harmonic mean is NOT blind to this damage and the " +
			"floor test below would prove nothing. Re-tune locallyBrokenEncoder.")
	}
	if !ledgerHas(t, led, store.Done, "movie.mkv") {
		t.Error("expected a done row (the mean-only gate accepts the locally-broken encode)")
	}
}

// TestVmaf_WorstFrameFloorRejectsLocallyBrokenEncode is the GREEN: the same encode,
// the same gates, the floor now on by default — REJECTED, and the source survives
// byte-for-byte. This is the whole of TRANSCODE-11.
func TestVmaf_WorstFrameFloorRejectsLocallyBrokenEncode(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264Long(t, ffmpeg, src, "8M")
	before := md5f(t, src)
	led := run(t, ffmpeg, ffprobe, d, locallyBrokenEncoder(ffmpeg), func(c *config.Config) {
		c.VmafEnable = boolPtr(true)
		c.MinVmaf = 95
		c.VmafMinPool = 60 // the shipped default
	})
	if md5f(t, src) != before {
		t.Error("source modified by a locally-broken output")
	}
	if codecOf(t, ffprobe, src) != "h264" {
		t.Error("source was SWAPPED for a locally-broken encode — the worst-frame floor did not hold")
	}
	if nTemp(t, d) != 0 {
		t.Error("temp not discarded")
	}
	if !ledgerHas(t, led, store.Failed, "movie.mkv") {
		t.Error("expected a failed row (worst-frame floor rejection)")
	}
}

// TestVmaf_WorstFrameFloorDoesNotFalseRejectDarkGrainyEncode is the anti-flake
// counterpart the roadmap demands: a floor that rejects honest encodes is worse than
// no floor, because it teaches operators to switch it off — which reopens the hole.
//
// The content is dark AND grainy — VMAF's documented worst case. Measured on real
// libvmaf, an honest crf-22 encode of it bottoms out at a worst frame of ~91, which
// is 31 points clear of the default floor of 60. (min_vmaf is relaxed to 90 here
// only because dark/grainy content legitimately pools near ~94 at this CRF; the mean
// gate has its own cases above. What is under test here is the FLOOR, and the floor
// stays at its shipped default.)
func TestVmaf_WorstFrameFloorDoesNotFalseRejectDarkGrainyEncode(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264DarkGrainy(t, ffmpeg, src, "8M")
	led := run(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) {
		c.VmafEnable = boolPtr(true)
		c.MinVmaf = 90
		c.VmafMinPool = 60 // the shipped default
	})
	if codecOf(t, ffprobe, src) != "hevc" {
		t.Error("the worst-frame floor FALSE-REJECTED an honest encode of dark/grainy content")
	}
	if !ledgerHas(t, led, store.Done, "movie.mkv") {
		t.Error("expected a done row (an honest dark/grainy encode must clear the floor)")
	}
}

func TestVmaf_DisabledAcceptsDegraded(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	// VMAF OFF: the same degraded output passes the structural gates → accepted.
	// Proves the VMAF gate (not a structural check) is what rejects it when on.
	led := run(t, ffmpeg, ffprobe, d, degradedEncoder(ffmpeg), func(c *config.Config) {
		c.VmafEnable = boolPtr(false)
	})
	if codecOf(t, ffprobe, src) != "hevc" {
		t.Error("VMAF-off did not accept the (structurally-valid) degraded output")
	}
	if !ledgerHas(t, led, store.Done, "movie.mkv") {
		t.Error("expected a done row with VMAF disabled")
	}
}

func TestVmaf_UnavailableWhileEnabledRejects(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	before := md5f(t, src)
	// Inject a scorer that reports libvmaf unavailable. The gate must REJECT (never
	// accept an unmeasured encode).
	eng := buildEngine(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) {
		c.VmafEnable = boolPtr(true)
		c.MinVmaf = 95
	})
	ts := eng.Store.(*testStore)
	eng.vmafScore = func(ctx context.Context, distorted, reference string, sub int, model string) (vmaf.Result, error) {
		return vmaf.Result{}, vmaf.ErrUnavailable
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if md5f(t, src) != before {
		t.Error("source modified when VMAF was unavailable")
	}
	if codecOf(t, ffprobe, src) != "h264" {
		t.Error("source swapped when VMAF unavailable (should reject an unmeasured encode)")
	}
	if !ledgerHas(t, ts, store.Failed, "movie.mkv") {
		t.Error("expected a failed row (unmeasured encode rejected)")
	}
}

func TestResolveVmafModel(t *testing.T) {
	cases := []struct {
		cfg    string
		height int
		want   string
	}{
		{"auto", 1080, "version=vmaf_v0.6.1"},
		{"", 720, "version=vmaf_v0.6.1"},
		{"auto", 1440, "version=vmaf_v0.6.1"}, // boundary: not > 1440 -> HD
		{"auto", 1441, "version=vmaf_4k_v0.6.1"},
		{"auto", 2160, "version=vmaf_4k_v0.6.1"},
		{"vmaf_4k_v0.6.1", 1080, "version=vmaf_4k_v0.6.1"}, // bare id -> prefixed
		{"version=custom", 1080, "version=custom"},         // already a spec -> passthrough
	}
	for _, tc := range cases {
		if got := resolveVmafModel(tc.cfg, tc.height); got != tc.want {
			t.Errorf("resolveVmafModel(%q, %d) = %q, want %q", tc.cfg, tc.height, got, tc.want)
		}
	}
}

func TestVmaf_MinPoolFloorRejects(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	before := md5f(t, src)
	// A faithful crf22 encode clears the harmonic-mean gate (MinVmaf=90) but its worst
	// (sub)sampled frame falls below a high worst-frame floor (99) → rejected by the
	// min-pool branch. Proves VmafMinPool is wired (the harmonic-mean gate alone accepts
	// this same encode — see TestVmaf_AcceptsNormalEncode).
	led := run(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) {
		c.VmafEnable = boolPtr(true)
		c.MinVmaf = 90
		c.VmafMinPool = 99
	})
	if md5f(t, src) != before {
		t.Error("source modified despite the worst-frame VMAF below the min-pool floor")
	}
	if codecOf(t, ffprobe, src) != "h264" {
		t.Error("source was swapped (min-pool floor not enforced)")
	}
	if !ledgerHas(t, led, store.Failed, "movie.mkv") {
		t.Error("expected a failed row (min-pool rejection)")
	}
}

// ---- TRANSCODE-5: SQLite store + worker pool ----------------------------------

// TestWorkerPool_ConcurrentWorkersProcessEachFileExactlyOnce runs RunOneshot with
// Workers=4 over six independent H.264 sources. It proves the worker pool fans out
// correctly AND that store.Claim's mutual exclusion holds under real concurrency: no
// source is encoded twice (which would show up as either a double-processing race
// or, since a second Claim on an already-active/-done job must fail, simply as every
// file ending HEVC+done exactly once with no error). Each source gets its own
// distinguishing byte-size (via a different bitrate) so a "swapped source" bug
// (worker A's output landing on worker B's file) would show up as a wrong-content
// swap; codec+done-per-file is the primary assertion, matching how the rest of this
// suite verifies outcomes.
func TestWorkerPool_ConcurrentWorkersProcessEachFileExactlyOnce(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	const n = 6
	var srcs []string
	for i := 0; i < n; i++ {
		p := filepath.Join(d, fmt.Sprintf("movie%d.mkv", i))
		// Vary bitrate slightly per file — inflated so real libx265 reliably shrinks
		// every one of them regardless of which worker/order handles it.
		mkH264(t, ffmpeg, p, fmt.Sprintf("%dM", 6+i))
		srcs = append(srcs, p)
	}

	ts := run(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) { c.Workers = 4 }) // REAL libx265

	for _, src := range srcs {
		if codecOf(t, ffprobe, src) != "hevc" {
			t.Errorf("%s: not transcoded to HEVC by the worker pool", src)
		}
		if !ledgerHas(t, ts, store.Done, filepath.Base(src)) {
			t.Errorf("%s: expected a done row", src)
		}
	}
	if nTemp(t, d) != 0 {
		t.Error("worker pool left a temp file behind")
	}

	// Every source has its OWN done row (no collapsing / cross-assignment): six
	// distinct files means six distinct current fingerprints, each independently
	// resolvable via ledgerHas above; as a second, more direct check, confirm every
	// file's content is unique (no worker accidentally wrote another worker's output
	// onto more than one path).
	seen := map[string]bool{}
	for _, src := range srcs {
		sum := md5f(t, src)
		if seen[sum] {
			t.Errorf("%s: duplicate output content — a source may have been double-processed or cross-assigned", src)
		}
		seen[sum] = true
	}
}

// TestCrashRecovery_StaleActiveJobIsReclaimedAndCompleted seeds the store with a job
// left in `encoding` for an existing, untouched source (simulating a worker that was
// killed mid-encode in a PRIOR process — store.Advance(Encoding) had committed, but
// the process died before Finish). A fresh Engine (new *Engine, SAME store/db) must
// call RecoverStale on RunOneshot, which resets that job back to pending; the
// source, having never been touched by the dead worker (the swap never happened —
// that's the whole invariant), is then reprocessed normally and ends done.
func TestCrashRecovery_StaleActiveJobIsReclaimedAndCompleted(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	before := md5f(t, src)
	inSize := probe.FileSize(src)

	// Simulate a crash: a prior process claimed and advanced this job to encoding,
	// then died before ever touching the filesystem (no temp, no swap — consistent
	// with the invariant that the swap is the only mutation and it happens last).
	cfg := baseCfg(d)
	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "jobs.db")
	seedStore, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	key := probe.Fingerprint(src)
	if ok, err := seedStore.Claim(context.Background(), src, key, "dead-worker", cfg.MaxFailures); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}
	if err := seedStore.Advance(context.Background(), src, key, store.Encoding); err != nil {
		t.Fatalf("seed advance: %v", err)
	}
	if err := seedStore.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	if md5f(t, src) != before {
		t.Fatal("seeding the store must not touch the filesystem")
	}

	// Fresh Engine, SAME db path — mirrors a process restart after a crash.
	reopened, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open (reopen): %v", err)
	}
	defer reopened.Close()
	prober := probe.New(ffmpeg, ffprobe)
	enc := FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}
	eng := New(cfg, prober, enc, reopened, discardLogger())

	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	if codecOf(t, ffprobe, src) != "hevc" {
		t.Error("crash-recovered job was not reprocessed to HEVC")
	}
	if probe.FileSize(src) >= inSize {
		t.Error("crash-recovered job: output not smaller")
	}
	if nTemp(t, d) != 0 {
		t.Error("crash-recovered job left a temp behind")
	}
	finalKey := probe.Fingerprint(src)
	status, _, exists, err := reopened.Get(context.Background(), src, finalKey)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !exists || status != store.Done {
		t.Errorf("crash-recovered job: final status = %q exists=%v, want done/true", status, exists)
	}
}

func TestWorkerStore_PrunesSupersededRow(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	oldFp := probe.Fingerprint(src) // the source's identity before the transcode
	ts := run(t, ffmpeg, ffprobe, d, nil, nil)
	// The file transcoded to HEVC (its fingerprint changed).
	if codecOf(t, ffprobe, src) != "hevc" {
		t.Fatal("source was not transcoded")
	}
	// The pre-swap row (old fingerprint) is pruned — the table self-prunes rather
	// than accumulating one dangling row per transcoded file.
	if st, _, exists, err := ts.Get(context.Background(), src, oldFp); err != nil {
		t.Fatalf("Get: %v", err)
	} else if exists {
		t.Errorf("superseded pre-swap row not pruned (status=%s)", st)
	}
	// The current file's row is done (short-circuits a resume).
	if !ledgerHas(t, ts, store.Done, "movie.mkv") {
		t.Error("expected a done row under the transcoded file's identity")
	}
}
