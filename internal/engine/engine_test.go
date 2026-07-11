package engine

// The DATA-SAFETY proof for the transcode engine — a Go port of the bash suite
// homelab/scripts/test-transcoder.sh (cases 1–17; the HDR cases 18–22 arrive with
// TRANSCODE-3). It drives the engine over REAL ffmpeg fixtures and asserts the
// no-loss contract holds on every unhappy path. It is anti-advisory-only: it
// exercises the code, reds on a regression, and FAILS LOUD (never skips) if
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
	"github.com/NSchatz/transcode/internal/ledger"
	"github.com/NSchatz/transcode/internal/probe"
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

// baseCfg is a fully-explicit engine config for tests (ApplyDefaults is NOT used, so
// an explicit MinBitrateKbps=0 is honoured — matching the bash MIN_BITRATE_KBPS=0).
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
	}
}

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
		enc = FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg}
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
