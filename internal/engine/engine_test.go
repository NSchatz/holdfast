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
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/transcode/internal/config"
	"github.com/NSchatz/transcode/internal/hdr"
	"github.com/NSchatz/transcode/internal/ledger"
	"github.com/NSchatz/transcode/internal/probe"
	"github.com/NSchatz/transcode/internal/vmaf"
)

// errFake is the deterministic error returned by fake encoders simulating a failure.
var errFake = errors.New("simulated encode failure")

// discardLogger returns a logger that drops all output (tests assert on behaviour,
// not logs).
func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// ---- tooling (fail loud, never skip — this is the safety proof) --------------

func tools(t *testing.T) (ffmpeg, ffprobe string) {
	t.Helper()
	ffmpeg = envOr("TRANSCODE_FFMPEG", "ffmpeg")
	ffprobe = envOr("TRANSCODE_FFPROBE", "ffprobe")
	for _, b := range []string{ffmpeg, ffprobe} {
		if _, err := exec.LookPath(b); err != nil {
			t.Fatalf("::error:: %q not found — the transcoder safety proof requires ffmpeg+ffprobe (set TRANSCODE_FFMPEG/FFPROBE): %v", b, err)
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

// ledgerHas reports whether the ledger holds a row with the given status whose path
// contains pathSub.
func ledgerHas(t *testing.T, led *ledger.Ledger, status, pathSub string) bool {
	t.Helper()
	b, err := os.ReadFile(led.Path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) == 3 && parts[0] == status && strings.Contains(parts[2], pathSub) {
			return true
		}
	}
	return false
}

func failCount(t *testing.T, led *ledger.Ledger) int {
	t.Helper()
	b, err := os.ReadFile(led.Path)
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, ledger.Failed+"\t") {
			n++
		}
	}
	return n
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

// run builds an engine over root with the given encoder and config mutation, then
// runs one oneshot pass. Returns the ledger for assertions.
func run(t *testing.T, ffmpeg, ffprobe, root string, enc Encoder, mutate func(*config.Config)) *ledger.Ledger {
	t.Helper()
	cfg := baseCfg(root)
	if mutate != nil {
		mutate(&cfg)
	}
	led := ledger.New(filepath.Join(root, "l.ledger"))
	prober := probe.New(ffmpeg, ffprobe)
	if enc == nil {
		enc = FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}
	}
	eng := New(cfg, prober, enc, led, discardLogger())
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	return led
}

func codecOf(t *testing.T, ffprobe, path string) string {
	// VideoCodec uses ffprobe only; let probe.New default the (unused) ffmpeg binary
	// rather than hardcode a literal here.
	return probe.New("", ffprobe).VideoCodec(context.Background(), path)
}

// buildEngine constructs an Engine over root without running it, so a test can set
// a seam (e.g. StaticMetadataIncomplete) before calling RunOneshot itself — mirrors
// the bash suite's TRANSCODER_TEST_HOOKS.
func buildEngine(ffmpeg, ffprobe, root string, enc Encoder, mutate func(*config.Config)) *Engine {
	cfg := baseCfg(root)
	if mutate != nil {
		mutate(&cfg)
	}
	led := ledger.New(filepath.Join(root, "l.ledger"))
	prober := probe.New(ffmpeg, ffprobe)
	if enc == nil {
		enc = FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}
	}
	return New(cfg, prober, enc, led, discardLogger())
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
	if !ledgerHas(t, led, ledger.Skipped, "movie.mkv") {
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
	if !ledgerHas(t, led, ledger.Failed, "movie.mkv") {
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
	if !ledgerHas(t, led, ledger.Failed, "movie.mkv") {
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
	if !ledgerHas(t, led, ledger.Failed, "movie.mkv") {
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
	if !ledgerHas(t, led, ledger.Done, "movie.mkv") {
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
	if !ledgerHas(t, led, ledger.Skipped, "movie.mp4") {
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
	if !ledgerHas(t, led, ledger.Failed, "movie.mkv") {
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
	if !ledgerHas(t, led, ledger.Skipped, "movie.mp4") {
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
	if ledgerHas(t, led, ledger.Skipped, "movie.mkv") {
		t.Error("hardlinked file must NOT be recorded (re-evaluated later)")
	}
}

func TestCase13_FailedRetriedThenParked(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "3M")
	before := md5f(t, src)
	enc := EncoderFunc(func(ctx context.Context, in, out string) error { return errFake })
	led := ledger.New(filepath.Join(d, "l.ledger"))
	cfg := baseCfg(d)
	cfg.MaxFailures = 3
	prober := probe.New(ffmpeg, ffprobe)
	do := func() {
		eng := New(cfg, prober, enc, led, discardLogger())
		if err := eng.RunOneshot(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	do()
	if failCount(t, led) != 1 {
		t.Fatalf("attempt 1: failCount=%d want 1", failCount(t, led))
	}
	do()
	if failCount(t, led) != 2 {
		t.Fatalf("attempt 2 (retry): failCount=%d want 2", failCount(t, led))
	}
	do()
	if failCount(t, led) != 3 {
		t.Fatalf("attempt 3: failCount=%d want 3", failCount(t, led))
	}
	do() // parked now — no new attempt
	if failCount(t, led) != 3 {
		t.Fatalf("after MAX_FAILURES: failCount=%d want 3 (parked)", failCount(t, led))
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
	led := run(t, ffmpeg, ffprobe, d, nil, nil)
	if md5f(t, tabName) != before {
		t.Error("tab-named file modified")
	}
	if codecOf(t, ffprobe, tabName) != "h264" {
		t.Error("tab-named file transcoded")
	}
	if fi, err := os.Stat(led.Path); err == nil && fi.Size() > 0 {
		t.Error("ledger should be empty (tab path unrecorded)")
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
	if !ledgerHas(t, led, ledger.Done, "movie.mkv") {
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
	if !ledgerHas(t, led, ledger.Failed, "movie.h264") {
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
	if !ledgerHas(t, led, ledger.Failed, "movie.mkv") {
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
	if !ledgerHas(t, led, ledger.Done, "movie.mkv") {
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
	if !ledgerHas(t, led, ledger.Done, "movie.mkv") {
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

	eng := buildEngine(ffmpeg, ffprobe, d, nil, nil)
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
	if !ledgerHas(t, eng.Led, ledger.Skipped, "movie.mkv") {
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
	if !ledgerHas(t, led, ledger.Skipped, "movie.mkv") {
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
	if !ledgerHas(t, led, ledger.Done, "movie.mkv") {
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
	if !ledgerHas(t, led, ledger.Done, "movie.mp4") {
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
	if !ledgerHas(t, led, ledger.Done, "movie.mkv") {
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
	if !ledgerHas(t, led, ledger.Skipped, "movie.mkv") {
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
	if !ledgerHas(t, led, ledger.Failed, "movie.mkv") {
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
	if !ledgerHas(t, led, ledger.Done, "movie.mkv") {
		t.Error("expected a done row")
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
	if !ledgerHas(t, led, ledger.Done, "movie.mkv") {
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
	eng := buildEngine(ffmpeg, ffprobe, d, nil, func(c *config.Config) {
		c.VmafEnable = boolPtr(true)
		c.MinVmaf = 95
	})
	eng.vmafScore = func(ctx context.Context, distorted, reference string, sub int, model string) (vmaf.Result, error) {
		return vmaf.Result{}, vmaf.ErrUnavailable
	}
	led := eng.Led
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if md5f(t, src) != before {
		t.Error("source modified when VMAF was unavailable")
	}
	if codecOf(t, ffprobe, src) != "h264" {
		t.Error("source swapped when VMAF unavailable (should reject an unmeasured encode)")
	}
	if !ledgerHas(t, led, ledger.Failed, "movie.mkv") {
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
	if !ledgerHas(t, led, ledger.Failed, "movie.mkv") {
		t.Error("expected a failed row (min-pool rejection)")
	}
}
