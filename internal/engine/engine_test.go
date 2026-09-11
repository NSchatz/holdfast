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
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/docscheck"
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

// ---- multi-video-stream fixture builders (real ffmpeg) -----------------------
//
// Both are built at test time from lavfi sources by the pinned ffmpeg, like every
// other fixture here, and both ASSERT THE SHAPE THEY CLAIM before returning. That
// assertion is not ceremony: the whole multi-video-stream story is about what a
// container carries, so a builder that quietly produced a one-stream file would turn
// every test below green while proving nothing at all.

// mkMP4WithCoverArt writes an MP4 carrying an h264 video track AND embedded artwork -
// which a container carries as a SECOND VIDEO STREAM with the attached_pic
// disposition, not as an attachment. That is the shape that makes cover art dangerous
// here: `-c:v <codec>` applies to it like any other video stream.
func mkMP4WithCoverArt(t *testing.T, ffmpeg, ffprobe, path, bitrate string) {
	t.Helper()
	cover := path + ".cover.jpg"
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=1:size=160x120:rate=10", "-frames:v", "1",
		"-c:v", "mjpeg", "-pix_fmt", "yuvj420p", "--", cover)
	defer os.Remove(cover)
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=duration=2:size=320x240:rate=10",
		"-i", cover,
		"-map", "0:v", "-map", "1:v",
		"-c:v:0", "libx264", "-preset", "ultrafast", "-b:v:0", bitrate, "-pix_fmt", "yuv420p",
		"-c:v:1", "copy", "-disposition:v:1", "attached_pic", "--", path)
	assertVideoStreamShape(t, ffprobe, path, []bool{false, true})
}

// mkTwoVideoStreams writes a container carrying TWO genuine moving-picture video
// streams - a second angle or a bonus reel muxed in - and NEITHER is an attached
// picture. Different lavfi sources, so the two streams are visibly different content
// and a mux that silently dropped one could not be mistaken for a mux that kept both.
func mkTwoVideoStreams(t *testing.T, ffmpeg, ffprobe, path, bitrate string) {
	t.Helper()
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=duration=2:size=320x240:rate=10",
		"-f", "lavfi", "-i", "mandelbrot=size=160x120:rate=10",
		"-map", "0:v", "-map", "1:v", "-t", "2",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", bitrate, "-pix_fmt", "yuv420p", "--", path)
	assertVideoStreamShape(t, ffprobe, path, []bool{false, false})
}

// assertVideoStreamShape fails unless path carries exactly the video streams described:
// one entry per stream in container order, true where that stream is an attached
// picture. It reads the disposition through ffprobe's `default=nw=1` output rather than
// through internal/probe, deliberately - a fixture assertion that went through the very
// parser the tests are keeping honest could be satisfied by a broken parser.
func assertVideoStreamShape(t *testing.T, ffprobe, path string, want []bool) {
	t.Helper()
	out, err := exec.Command(ffprobe, "-v", "error", "-select_streams", "v",
		"-show_entries", "stream_disposition=attached_pic", "-of", "default=nw=1", "--", path).Output()
	if err != nil {
		t.Fatalf("fixture %s: ffprobe could not read it: %v", path, err)
	}
	var got []bool
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		_, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		got = append(got, v == "1")
	}
	if len(got) != len(want) {
		t.Fatalf("fixture %s carries %d video stream(s) (%v), want %d (%v) - the fixture is not the "+
			"shape the test is about", path, len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fixture %s: video stream v:%d attached_pic=%v, want %v", path, i, got[i], want[i])
		}
	}
}

// extractVideoStream copies one video stream out of a container WITHOUT re-encoding it
// (`-c copy`), and returns its bytes. It is how "the cover art survived" is asked as a
// question about BYTES rather than about whether a swap happened: a stream that was
// re-encoded comes out as different bytes, and a stream that was carried comes out as
// the same ones.
func extractVideoStream(t *testing.T, ffmpeg, path string, videoIndex int, dst string) []byte {
	t.Helper()
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", path,
		"-map", fmt.Sprintf("0:v:%d", videoIndex), "-c", "copy", "-f", "image2", "--", dst)
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read the extracted stream %s: %v", dst, err)
	}
	if len(b) == 0 {
		t.Fatalf("the stream extracted from %s is empty, so comparing it proves nothing", path)
	}
	return b
}

// dirListing is every entry under dir, relative and sorted - the whole directory, so a
// "nothing was written and nothing was deleted" assertion can compare the before and
// after rather than checking for the absence of the files a test happened to think of.
func dirListing(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	if err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		out = append(out, rel)
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	sort.Strings(out)
	return out
}

// delegatingFFprobe writes a fake "ffprobe" that REFUSES one ffprobe question and hands
// every other one to the real binary. refuseEntries is matched against the
// -show_entries argument; refusePathSub, when non-empty, narrows the refusal to paths
// containing it.
//
// It exists because "the probe could not establish this" is a real, reachable state of a
// working installation (a half-installed build, a container whose ffprobe is being
// replaced under it, a file the demuxer chokes on part-way) and the engine's answer to it
// is a data-safety decision. Simulating it by pointing the engine at a broken binary
// would prove nothing, because then NOTHING probes; this refuses exactly one question, so
// the file still reaches the guard whose behaviour is under test.
func delegatingFFprobe(t *testing.T, dir, realFFprobe, refuseEntries, refusePathSub string) string {
	t.Helper()
	fake := filepath.Join(dir, "fake-ffprobe.sh")
	script := "#!/bin/sh\n" +
		"want=0\n" +
		"for a in \"$@\"; do\n" +
		"  [ \"$a\" = '" + refuseEntries + "' ] && want=1\n" +
		"done\n" +
		"if [ \"$want\" = 1 ]; then\n" +
		"  case \" $* \" in\n" +
		"    *'" + refusePathSub + "'*) exit 3 ;;\n" +
		"  esac\n" +
		"fi\n" +
		"exec \"" + realFFprobe + "\" \"$@\"\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ffprobe: %v", err)
	}
	return fake
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
	enc := EncoderFunc(func(ctx context.Context, in, out string, _ *probe.VideoProps) error { return errFake })
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
	enc := EncoderFunc(func(ctx context.Context, in, out string, _ *probe.VideoProps) error {
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
	enc := EncoderFunc(func(ctx context.Context, in, out string, _ *probe.VideoProps) error {
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
	enc := EncoderFunc(func(ctx context.Context, in, out string, _ *probe.VideoProps) error { return errFake })
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
	enc := EncoderFunc(func(ctx context.Context, in, out string, _ *probe.VideoProps) error {
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
	enc := EncoderFunc(func(ctx context.Context, in, out string, _ *probe.VideoProps) error {
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
	return func(ctx context.Context, in, out string, _ *probe.VideoProps) error {
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
	return func(ctx context.Context, in, out string, _ *probe.VideoProps) error {
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
	eng.vmafScore = func(ctx context.Context, req vmaf.Request) (vmaf.Result, error) {
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
	if ok, err := seedStore.Claim(context.Background(), src, key, "dead-worker", cfg.MaxFailures, store.DecisionInputs{}); err != nil || !ok {
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

// ---- S0030: live progress, and what it must not cost -------------------------

// The per-job subprocess budget for one done job with the VMAF gate off: 16 ffprobe
// invocations and 2 ffmpeg invocations (the encode and the decode-integrity check).
//
// The ffprobe side is the source snapshot, the source's stream-shape probe that the
// multi-video-stream guard reads, and verifyOutput's codec/duration/packet probes plus its
// per-type stream-count pass - which asks both files about each of FOUR types (v, a, s, t),
// so the parity loop alone is eight of them.
//
// These constants are NOT the evidence for AC12 and must not be read as it — they are a
// long-run ceiling, so that a per-PR "no worse than last time" cannot ratchet the cost up
// one probe at a time across many changes. The criterion itself is proved by MEASURING
// both arms in the same run: see TestProbeBudget_ProgressAddsNoSubprocess, which also
// fails if these numbers ever stop matching what it measures, so they cannot decay into
// folklore.
const (
	probeBudgetFFprobe = 16
	probeBudgetFFmpeg  = 2
)

// countingWrapper writes a shell wrapper for a real binary that appends one line per
// invocation to a log and then delegates. Pointing the engine's Prober and Encoder at
// the wrappers turns "how many subprocesses did this job spawn?" into a countable fact
// rather than an argument.
func countingWrapper(t *testing.T, dir, name, real string) (wrapper, log string) {
	t.Helper()
	wrapper = filepath.Join(dir, "counting-"+name+".sh")
	log = filepath.Join(dir, "counting-"+name+".log")
	script := "#!/bin/sh\n" +
		"printf 'call\\n' >> \"" + log + "\"\n" +
		"exec \"" + real + "\" \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("write %s wrapper: %v", name, err)
	}
	return wrapper, log
}

func countCalls(t *testing.T, log string) int {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read %s: %v", log, err)
	}
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n
}

// countJobSubprocesses runs ONE job end to end under counting wrappers and returns how
// many ffprobe and ffmpeg subprocesses it spawned. progress selects whether live progress
// collection is active.
//
// The progress=false arm is the load-bearing one: the engine collects progress only when
// an Observer is set (see Engine.encode), and with none it takes EXACTLY the path this
// package took before progress collection existed. So that arm is not a stand-in for the
// pre-change tree, it IS the pre-change per-job subprocess behaviour, executed in the same
// process, on the same machine, against the same ffmpeg, in the same run as the arm it is
// compared with.
func countJobSubprocesses(t *testing.T, progress bool) (probes, ffmpegs int) {
	t.Helper()
	realFFmpeg, realFFprobe := tools(t)
	root := t.TempDir()
	src := filepath.Join(root, "movie.mkv")
	mkH264(t, realFFmpeg, src, "8M") // fixtures use the REAL binaries — never counted

	d := t.TempDir()
	countedFFmpeg, ffmpegLog := countingWrapper(t, d, "ffmpeg", realFFmpeg)
	countedFFprobe, ffprobeLog := countingWrapper(t, d, "ffprobe", realFFprobe)

	eng := buildEngine(t, countedFFmpeg, countedFFprobe, root, nil, nil)
	var c collector
	if progress {
		eng.Observer = c.observe
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot(progress=%v): %v", progress, err)
	}
	if codecOf(t, realFFprobe, src) != "hevc" {
		t.Fatalf("the fixture job did not complete (progress=%v) — a budget measured over a job that did nothing proves nothing", progress)
	}
	if progress && len(c.snapshot()) == 0 {
		t.Fatal("no events were observed on the progress arm — the budget would be measured with the feature quietly off")
	}
	return countCalls(t, ffprobeLog), countCalls(t, ffmpegLog)
}

// TestProbeBudget_ProgressAddsNoSubprocess is AC12: a job processed end to end spawns no
// more probe subprocesses than the same job spawns before this change.
//
// This is the guard on the obvious wrong way to build this feature. A progress figure is
// meaningless without the source length it is measured against, and the easy way to get
// that length is a second ffprobe per job — on a tool that walks a whole library, which
// is exactly the regression TRANSCODE-PERF was raised to remove. So the duration rides
// the snapshot ProcessFile ALREADY takes: one more -show_entries section on a call that
// was being made anyway.
//
// It is a CONTROLLED EXPERIMENT rather than a constant with a ceiling under it. The same
// job is run twice in the same environment, once with progress collection off (the
// pre-change path, exactly) and once with it on, and the criterion is asserted between the
// two MEASURED numbers. A recorded constant can only ever hold the line at a number
// somebody wrote down; the control arm holds it at what this tree actually costs today.
// The constants stay as a long-run ceiling and are themselves re-measured here, so they
// can never drift into being a comment about a tree nobody has run.
//
// Honest about its reach: a count is blind to a change that ADDS one probe and REMOVES
// another, which nets zero. That limit is AC12's own ("no more ... than"), and closing it
// would need per-call-site identity rather than a total.
func TestProbeBudget_ProgressAddsNoSubprocess(t *testing.T) {
	baseProbes, baseFFmpegs := countJobSubprocesses(t, false)
	liveProbes, liveFFmpegs := countJobSubprocesses(t, true)
	t.Logf("one done job, progress collection off -> on: ffprobe %d -> %d, ffmpeg %d -> %d (long-run ceiling %d / %d)",
		baseProbes, liveProbes, baseFFmpegs, liveFFmpegs, probeBudgetFFprobe, probeBudgetFFmpeg)

	// The criterion, measured on both sides.
	if liveProbes > baseProbes {
		t.Errorf("one job spawned %d ffprobe subprocesses with progress collection on and %d with it off — progress collection must not add a probe",
			liveProbes, baseProbes)
	}
	if liveFFmpegs > baseFFmpegs {
		t.Errorf("one job spawned %d ffmpeg subprocesses with progress collection on and %d with it off — progress collection must not add a subprocess",
			liveFFmpegs, baseFFmpegs)
	}

	// The long-run ceiling, so a sequence of individually-innocent changes cannot walk
	// the per-job cost upwards a probe at a time.
	if liveProbes > probeBudgetFFprobe || liveFFmpegs > probeBudgetFFmpeg {
		t.Errorf("one job spawned %d ffprobe / %d ffmpeg subprocesses, over the recorded ceiling of %d / %d",
			liveProbes, liveFFmpegs, probeBudgetFFprobe, probeBudgetFFmpeg)
	}
	// ...and the ceiling is held to the measurement, so it stays a number somebody can
	// still reproduce rather than one somebody once wrote down.
	if baseProbes != probeBudgetFFprobe || baseFFmpegs != probeBudgetFFmpeg {
		t.Errorf("the recorded per-job baseline is stale: measured %d ffprobe / %d ffmpeg with progress collection off, the constants say %d / %d. Re-measure and move the constants in the same commit that moved the cost.",
			baseProbes, baseFFmpegs, probeBudgetFFprobe, probeBudgetFFmpeg)
	}
}

// TestEngine_ReportsLiveProgressWithTheSourceDuration is the wiring proof: a real encode
// driven through ProcessFile reaches the Observer as PROGRESS events (no state
// transition), each carrying a real position and the source duration it is measured
// against. It is what makes the dashboard's figure a measurement rather than a guess.
func TestEngine_ReportsLiveProgressWithTheSourceDuration(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	src := filepath.Join(root, "movie.mkv")
	mkH264Long(t, ffmpeg, src, "8M") // 10s, so the encode is long enough to report

	srcDur, ok := probe.New(ffmpeg, ffprobe).DurationSec(context.Background(), src)
	if !ok {
		t.Fatal("fixture has no readable duration")
	}

	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, nil)
	var c collector
	eng.Observer = c.observe
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	var progress []Event
	for _, ev := range c.snapshot() {
		if ev.Progress != nil {
			progress = append(progress, ev)
		}
	}
	if len(progress) == 0 {
		t.Fatal("a real encode produced no progress events — an in-flight job stays as illegible as it was")
	}
	for _, ev := range progress {
		if ev.Status != store.Encoding {
			t.Errorf("a progress event must carry the state the job is still IN, got %q", ev.Status)
		}
		if ev.Outcome != nil {
			t.Error("a progress event must carry no Outcome — it is not a terminal transition")
		}
		if ev.Path != src {
			t.Errorf("progress event path = %q, want the source %q", ev.Path, src)
		}
		if ev.Progress.DurationSec == nil {
			t.Fatal("progress carries no source duration — there is nothing to measure it against")
		}
		if math.Abs(*ev.Progress.DurationSec-srcDur) > 0.01 {
			t.Errorf("progress duration = %v, want the source duration %v", *ev.Progress.DurationSec, srcDur)
		}
		if ev.Progress.PositionSec < 0 {
			t.Errorf("negative reported position %v", ev.Progress.PositionSec)
		}
	}
}

// TestEngine_FailedEncodeWithProgressStillRecordsTheError is the engine half of AC6: the
// job a failing encoder produces is the same job it produced before progress collection
// existed — failed, source untouched, with the encoder's error text as the reason.
func TestEngine_FailedEncodeWithProgressStillRecordsTheError(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	src := filepath.Join(root, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	before := md5f(t, src)

	d := t.TempDir()
	// A fake ffmpeg that emits a valid progress report and THEN fails.
	fake := progressFake(t, d, "engine-failing", oneReport(500_000, "continue"),
		"", "Conversion failed! the encoder gave up", 4)
	cfg := baseCfg(root)
	prober := probe.New(ffmpeg, ffprobe)
	ts := newTestStore(t, root)
	eng := New(cfg, prober, FFmpegEncoder{FFmpeg: fake, Cfg: cfg, Probe: prober}, ts, discardLogger())
	var c collector
	eng.Observer = c.observe

	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	if md5f(t, src) != before {
		t.Fatal("source modified by a failed encode")
	}
	if nTemp(t, root) != 0 {
		t.Error("a failed encode left a temp behind")
	}
	if !ledgerHas(t, ts, store.Failed, "movie.mkv") {
		t.Fatal("expected a failed row")
	}
	rows, err := ts.List(context.Background(), []store.Status{store.Failed}, 0)
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	reason := ""
	for _, r := range rows {
		if r.Path == src {
			reason = r.Outcome.Reason
		}
	}
	if !strings.Contains(reason, "Conversion failed!") {
		t.Errorf("the failed row lost the encoder's error text: %q", reason)
	}
	if !strings.Contains(reason, "exit status 4") {
		t.Errorf("the failed row lost the encoder's exit status: %q", reason)
	}
	// Progress really was collected on that run, so the assertions above are not
	// vacuously true of a run where collection never happened.
	sawProgress := false
	for _, ev := range c.snapshot() {
		if ev.Progress != nil {
			sawProgress = true
		}
	}
	if !sawProgress {
		t.Error("no progress was collected on the failing run — the proof would be vacuous")
	}
}

// ---- what a failure COSTS: the class, the park, and the retry that survives ----
//
// `max_failures` buys re-attempts, and a re-attempt is only worth an encode when the
// next one could come out differently. These cases pin both halves of that: a verdict
// that is a pure function of (source, configuration, ffmpeg build) costs ONE encode, and
// everything else still costs up to the bound, as it always has.

// failedRow returns the recorded failed row for path, and fails the test if there is
// none. The class and the composed reason are both on it, so both are asserted from the
// row an operator would actually read rather than from a log line.
func failedRow(t *testing.T, ts *testStore, path string) store.Job {
	t.Helper()
	rows, err := ts.List(context.Background(), []store.Status{store.Failed}, 0)
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	for _, r := range rows {
		if r.Path == path {
			return r
		}
	}
	t.Fatalf("no failed row for %s", path)
	return store.Job{}
}

// biggerThanItsSource writes an HEVC clip that is the same length as src and LARGER than
// it: 720p lossless against a 240p source, so it is bigger whatever x265 does with the
// bitrate. It is written OUTSIDE the library root, so a scan of that root still has
// exactly one file to offer.
func biggerThanItsSource(t *testing.T, ffmpeg, src string) string {
	t.Helper()
	big := filepath.Join(t.TempDir(), "big.mkv")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=1280x720:rate=10",
		"-c:v", "libx265", "-x265-params", "lossless=1:log-level=error", "-pix_fmt", "yuv420p", "--", big)
	if probe.FileSize(big) <= probe.FileSize(src) {
		t.Fatalf("fixture drifted: the 'bigger' replacement (%d B) is not bigger than the source (%d B), "+
			"so the size gate is not what rejects it", probe.FileSize(big), probe.FileSize(src))
	}
	return big
}

// TestFailure_SizeIncreaseParksOnFirstOccurrence.
//
// A file that cannot be beaten under the current configuration - an already-efficient
// encode, a grain-heavy master, anything at a bitrate the CRF cannot undercut - is
// rejected by the size gate, and every retry re-runs the same encode to produce the same
// rejection. On a feature-length source at preset slow that is hours per attempt, three
// times, across however many such files a library holds.
//
// So the FIRST occurrence parks it: fail_count reaches max_failures in the same write
// that records the failure, and the next pass does not invoke the encoder at all. The
// row still carries the gate's own text, with the finality in front of it, so an
// operator reading the dashboard is not left wondering whether it will be tried again.
func TestFailure_SizeIncreaseParksOnFirstOccurrence(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "300k")
	before := md5f(t, src)
	big := biggerThanItsSource(t, ffmpeg, src)

	calls := 0
	enc := EncoderFunc(func(ctx context.Context, in, out string, _ *probe.VideoProps) error {
		calls++
		b, err := os.ReadFile(big)
		if err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	})

	ts := newTestStore(t, d)
	cfg := baseCfg(d)
	cfg.MaxFailures = 3
	prober := probe.New(ffmpeg, ffprobe)
	scan := func() {
		eng := New(cfg, prober, enc, ts, discardLogger())
		if err := eng.RunOneshot(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	scan()
	if calls != 1 {
		t.Fatalf("the first pass invoked the encoder %d times, want 1", calls)
	}
	if !ledgerHas(t, ts, store.Failed, "movie.mkv") {
		t.Fatal("the first pass recorded no failed row")
	}
	if got := failCount(t, ts, "movie.mkv"); got != cfg.MaxFailures {
		t.Fatalf("after ONE deterministic failure fail_count = %d, want max_failures = %d - "+
			"the remaining attempts would each pay a full encode to reach the identical verdict",
			got, cfg.MaxFailures)
	}

	row := failedRow(t, ts, src)
	if row.Outcome.FailureClass != store.FailureDeterministic {
		t.Errorf("recorded class = %q, want %q", row.Outcome.FailureClass, store.FailureDeterministic)
	}
	// The reason carries BOTH: what rejected the encode, and that the verdict is final.
	if !strings.Contains(row.Outcome.Reason, "size-increase reject") {
		t.Errorf("the recorded reason lost the gate's own text, so an operator cannot see WHY "+
			"it was rejected without the logs: %q", row.Outcome.Reason)
	}
	lower := strings.ToLower(row.Outcome.Reason)
	if !strings.Contains(lower, "final") || !strings.Contains(lower, "retr") {
		t.Errorf("the recorded reason does not say the verdict is final, so an operator cannot see "+
			"that it will NOT be tried again: %q", row.Outcome.Reason)
	}

	// The park is the ordinary one. No new status, and the file is refused by the same
	// attempt bound that parks an exhausted transient failure.
	if row.Status != store.Failed {
		t.Errorf("terminal status = %q, want %q - a deterministic park introduces no new status",
			row.Status, store.Failed)
	}

	// The second pass: the file is not claimed, so the encoder is never reached.
	scan()
	if calls != 1 {
		t.Fatalf("the second pass invoked the encoder again (%d calls total) - the park bought nothing", calls)
	}
	if got := failCount(t, ts, "movie.mkv"); got != cfg.MaxFailures {
		t.Errorf("the second pass moved fail_count to %d, want it left at %d", got, cfg.MaxFailures)
	}
	if md5f(t, src) != before {
		t.Error("the source was modified by a rejected encode")
	}
	if nTemp(t, d) != 0 {
		t.Error("a rejected encode left a temp behind")
	}
}

// TestFailure_EncodeErrorStillRetries is the control that stops the change above from
// being "park everything".
//
// An encode error is not a verdict about the source: a full disk, an OOM-killed ffmpeg,
// a transient I/O error are all conditions of the RUN, and the next attempt genuinely
// may differ. Such a failure therefore costs one attempt of the bound, exactly as every
// failure did before any of them were told apart, and the file is parked only when the
// bound is actually exhausted.
func TestFailure_EncodeErrorStillRetries(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "3M")
	before := md5f(t, src)

	calls := 0
	enc := EncoderFunc(func(ctx context.Context, in, out string, _ *probe.VideoProps) error {
		calls++
		return errFake
	})
	ts := newTestStore(t, d)
	cfg := baseCfg(d)
	cfg.MaxFailures = 3
	prober := probe.New(ffmpeg, ffprobe)
	scan := func() {
		eng := New(cfg, prober, enc, ts, discardLogger())
		if err := eng.RunOneshot(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	for attempt := 1; attempt <= cfg.MaxFailures; attempt++ {
		scan()
		if calls != attempt {
			t.Fatalf("attempt %d: the encoder ran %d times in total, want %d - a transient failure "+
				"must still be retried", attempt, calls, attempt)
		}
		if got := failCount(t, ts, "movie.mkv"); got != attempt {
			t.Fatalf("attempt %d: fail_count = %d, want %d - a transient failure must spend ONE "+
				"attempt of the bound, not all of it", attempt, got, attempt)
		}
	}

	// Only now, with the bound genuinely exhausted, is it parked.
	scan()
	if calls != cfg.MaxFailures {
		t.Errorf("the file was attempted again after max_failures (%d encoder calls)", calls)
	}
	if got := failCount(t, ts, "movie.mkv"); got != cfg.MaxFailures {
		t.Errorf("after max_failures fail_count = %d, want %d", got, cfg.MaxFailures)
	}

	row := failedRow(t, ts, src)
	if row.Outcome.FailureClass != store.FailureTransient {
		t.Errorf("recorded class = %q, want %q - an encode error is not a verdict about the source",
			row.Outcome.FailureClass, store.FailureTransient)
	}
	// It records the error and nothing more: a transient failure must not tell an
	// operator the verdict is final, because it is not.
	if !strings.Contains(row.Outcome.Reason, errFake.Error()) {
		t.Errorf("the recorded reason lost the encoder's error: %q", row.Outcome.Reason)
	}
	if strings.Contains(strings.ToLower(row.Outcome.Reason), "final") {
		t.Errorf("a retryable failure's reason claims the verdict is final: %q", row.Outcome.Reason)
	}
	if md5f(t, src) != before {
		t.Error("the source was modified across the retries")
	}
}

// TestFailure_AnUnclassifiedFailureIsRetriedNotParked is the fail-safe direction, and it
// is deliberately asymmetric: the cost of a wrong "may differ" is CPU, and the cost of a
// wrong "final" is a file parked at its first failure that nobody revisits.
//
// So a terminal failure carrying NO class - which is what a gate rejection added later
// without one produces, and what every row written before the class existed carries -
// must be recorded as retryable and must actually be retried. The row is written through
// the same store call the engine's recording site uses, with the class left unset.
func TestFailure_AnUnclassifiedFailureIsRetriedNotParked(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "3M")

	ts := newTestStore(t, d)
	cfg := baseCfg(d)
	cfg.MaxFailures = 3
	ctx := context.Background()
	key := probe.Fingerprint(src)

	if ok, err := ts.Claim(ctx, src, key, "w0", cfg.MaxFailures, store.DecisionInputs{}); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}
	// A rejection this build's classifier says nothing about.
	if err := ts.Finish(ctx, src, key, store.Failed,
		&store.Outcome{Reason: "a gate rejection this build does not classify"}, cfg.MaxFailures); err != nil {
		t.Fatalf("seed finish: %v", err)
	}

	if got := failCount(t, ts, "movie.mkv"); got != 1 {
		t.Fatalf("an unclassified failure spent fail_count = %d of the bound, want 1 - an "+
			"unrecognised rejection must cost CPU, never a file nobody revisits", got)
	}
	if got := failedRow(t, ts, src).Outcome.FailureClass; got != store.FailureTransient {
		t.Errorf("an unclassified failure reads back as %q, want %q", got, store.FailureTransient)
	}

	// And it is genuinely retried: the next pass claims the file and reaches the encoder.
	calls := 0
	enc := EncoderFunc(func(ctx context.Context, in, out string, _ *probe.VideoProps) error {
		calls++
		return errFake
	})
	prober := probe.New(ffmpeg, ffprobe)
	eng := New(cfg, prober, enc, ts, discardLogger())
	if err := eng.RunOneshot(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("the next pass reached the encoder %d times, want 1 - an unclassified failure "+
			"was treated as parked", calls)
	}
}

// finishBlockedStore is a store whose terminal write can be made to fail on demand,
// leaving everything else real. It is the only way to drive the one window that matters
// here: the park was DECIDED and could not be made durable.
type finishBlockedStore struct {
	*testStore
	blocking bool
	blocked  int
}

func (s *finishBlockedStore) Finish(ctx context.Context, path, fingerprint string, st store.Status, o *store.Outcome, maxFailures int) error {
	if s.blocking {
		s.blocked++
		return errors.New("simulated store failure: the terminal row could not be written")
	}
	return s.testStore.Finish(ctx, path, fingerprint, st, o, maxFailures)
}

// TestFailure_AParkThatCouldNotBeWrittenIsNotAPark.
//
// The park is a durable fact or it is nothing. If the write that records it fails, the
// process must not behave as though the file were parked: nothing on disk was touched,
// so a later pass meeting the same file is free to attempt it again, and it is a later
// pass - not this one - that gets to decide. A park that existed only in a process that
// has since exited would be a file quietly dropped from the library.
func TestFailure_AParkThatCouldNotBeWrittenIsNotAPark(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "300k")
	before := md5f(t, src)
	big := biggerThanItsSource(t, ffmpeg, src)

	calls := 0
	enc := EncoderFunc(func(ctx context.Context, in, out string, _ *probe.VideoProps) error {
		calls++
		b, err := os.ReadFile(big)
		if err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	})

	ts := newTestStore(t, d)
	blocked := &finishBlockedStore{testStore: ts, blocking: true}
	cfg := baseCfg(d)
	cfg.MaxFailures = 3
	prober := probe.New(ffmpeg, ffprobe)
	scan := func() {
		eng := New(cfg, prober, enc, blocked, discardLogger())
		if err := eng.RunOneshot(context.Background()); err != nil {
			t.Fatalf("a failed terminal write must not take the run down: %v", err)
		}
	}

	scan()
	if blocked.blocked == 0 {
		t.Fatal("the terminal write was never attempted, so this proves nothing about it failing")
	}
	// The source is byte-for-byte intact and nothing was left behind.
	if md5f(t, src) != before {
		t.Error("the source changed when the park could not be recorded")
	}
	if codecOf(t, ffprobe, src) != "h264" {
		t.Error("the source was swapped for the rejected encode")
	}
	if nTemp(t, d) != 0 {
		t.Error("a temp was left behind when the park could not be recorded")
	}
	// And NOTHING is parked: no durable record of the park exists.
	if got := failCount(t, ts, "movie.mkv"); got != 0 {
		t.Errorf("fail_count = %d after a park that was never written, want 0", got)
	}

	// A later pass is free to attempt the file again - and now that the store is
	// healthy, that attempt is the one that parks it.
	blocked.blocking = false
	scan()
	if calls != 2 {
		t.Fatalf("the encoder ran %d times in total, want 2 - a park that was never recorded "+
			"must not hold the file out of a later pass", calls)
	}
	if got := failCount(t, ts, "movie.mkv"); got != cfg.MaxFailures {
		t.Errorf("fail_count = %d after the park was recorded for real, want %d", got, cfg.MaxFailures)
	}
}

// ---- the multi-video-stream guard, the cover-art carry, and the vocabulary ----

// TestProcessFile_SkipsAMultiVideoStreamSource is the refusal half of the source-shape
// guard, and it covers BOTH ways a source can fail it: a real second moving-picture
// stream, and a probe that could not establish the shape at all.
//
// The two belong in one test because they are one decision. Every property the pipeline
// derives - codec, bitrate, field order, pixel format, colour, HDR class, and both sides
// of the VMAF comparison - is read from v:0, so a second moving picture would be
// re-encoded against a description of a different stream and would enter no decision in
// front of the swap. An answer the probe could not establish is the same position with
// less information, so it takes the same refusal rather than a guess.
//
// Each arm asserts the whole directory is unchanged, not merely that no temp was left:
// the guard fires BEFORE any encode is started, so nothing was written and nothing was
// deleted, and a listing taken either side is what says so.
func TestProcessFile_SkipsAMultiVideoStreamSource(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	t.Run("a second moving-picture stream", func(t *testing.T) {
		d := t.TempDir()
		src := filepath.Join(d, "movie.mkv")
		mkTwoVideoStreams(t, ffmpeg, ffprobe, src, "2M")
		before, listBefore := md5f(t, src), dirListing(t, d)

		ts := run(t, ffmpeg, ffprobe, d, nil, nil)

		if !ledgerHas(t, ts, store.Skipped, "movie.mkv") {
			t.Fatalf("a source carrying two moving-picture streams was not skipped")
		}
		if got := skipReason(t, ts, "movie.mkv"); got != SkipMultiVideoStream {
			t.Errorf("skip reason = %q, want %q", got, SkipMultiVideoStream)
		}
		if md5f(t, src) != before {
			t.Error("the source changed under a guard that runs before any encode starts")
		}
		if got := dirListing(t, d); !equalStrings(got, listBefore) {
			t.Errorf("the directory changed: before %v, after %v - the guard must write no temp "+
				"and delete nothing", listBefore, got)
		}
	})

	t.Run("the probe could not establish the stream shape", func(t *testing.T) {
		d := t.TempDir()
		src := filepath.Join(d, "movie.mkv")
		// ONE video stream, so anything that skips this file skipped it for the
		// indeterminate answer and not for its shape.
		mkH264(t, ffmpeg, src, "8M")
		before, listBefore := md5f(t, src), dirListing(t, d)

		fake := delegatingFFprobe(t, t.TempDir(), ffprobe, "stream=index:stream_disposition=attached_pic", "")
		ts := run(t, ffmpeg, fake, d, nil, nil)

		if !ledgerHas(t, ts, store.Skipped, "movie.mkv") {
			t.Fatalf("a source whose stream shape could not be established was not skipped")
		}
		if got := skipReason(t, ts, "movie.mkv"); got != SkipMultiVideoStream {
			t.Errorf("skip reason = %q, want %q - an indeterminate probe answer takes the same "+
				"refusal rather than a guess", got, SkipMultiVideoStream)
		}
		if md5f(t, src) != before {
			t.Error("the source changed on an indeterminate probe answer")
		}
		if got := dirListing(t, d); !equalStrings(got, listBefore) {
			t.Errorf("the directory changed: before %v, after %v", listBefore, got)
		}
	})

	// The anti-vacuity control for the arm above: the IDENTICAL single-stream source,
	// probed by a working ffprobe, transcodes. Without it "an indeterminate answer skips"
	// would be satisfied by a guard that skipped every file it ever saw.
	t.Run("the same source with a working probe still transcodes", func(t *testing.T) {
		d := t.TempDir()
		src := filepath.Join(d, "movie.mkv")
		mkH264(t, ffmpeg, src, "8M")
		ts := run(t, ffmpeg, ffprobe, d, nil, nil)
		if !ledgerHas(t, ts, store.Done, "movie.mkv") {
			t.Fatalf("the control source did not transcode, so the indeterminate-probe arm "+
				"proves nothing: reason=%q", skipReason(t, ts, "movie.mkv"))
		}
	})
}

// TestProcessFile_StreamCopiesAttachedCoverArt is the one criterion in this change that
// makes a previously-refused class of file ELIGIBLE for the irreversible swap, so it
// proves the bound rather than asserting it.
//
// An MP4 carrying artwork carries it as a video stream, and `-c:v <codec>` applies to it:
// today that encode mostly fails outright at the mux, and where it succeeds it leaves a
// one-frame HEVC stream where a JPEG used to be. After this change the file transcodes -
// so two things have to be true at once, and both are asserted here:
//
//  1. The file went through the UNCHANGED FULL VERIFY GATE. Nothing is skipped and
//     nothing is relaxed for it, so this run turns the VMAF gate ON and measures with
//     real libvmaf - the costliest gate, the one a shortcut would be most tempted to
//     drop - and then asserts the recorded row carries what that gate MEASURED. A row
//     with a mean, a worst frame, a chroma figure, a model and a comparison format on it
//     is a row whose perceptual gate really ran.
//  2. The cover art survived as BYTES. The cover stream is extracted from the source
//     before the run and from the replacement after it, with `-c copy` both times, and
//     the two byte slices must be equal. A passing swap is not evidence the picture
//     survived; only the bytes are.
func TestProcessFile_StreamCopiesAttachedCoverArt(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	side := t.TempDir() // extraction scratch, outside the library root
	src := filepath.Join(d, "movie.mp4")
	mkMP4WithCoverArt(t, ffmpeg, ffprobe, src, "8M")

	wantCover := extractVideoStream(t, ffmpeg, src, 1, filepath.Join(side, "source-cover.jpg"))

	ts := run(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) {
		// In-place: an attached picture is an MP4 shape, so the output container is the
		// source's own rather than the tests' default mkv.
		c.ContainerExt = "source"
		// EVERY gate, including the one that costs a second full decode.
		c.VmafEnable = boolPtr(true)
		c.MinVmaf, c.VmafMinPool, c.VmafMinChroma = 90, 50, 25
	})

	if !ledgerHas(t, ts, store.Done, "movie.mp4") {
		t.Fatalf("the cover-art source did not transcode: reason=%q", skipReason(t, ts, "movie.mp4"))
	}
	if got := codecOf(t, ffprobe, src); got != "hevc" {
		t.Fatalf("the file at the source path is %q, want hevc - no swap happened, so there is "+
			"nothing to say about what survived it", got)
	}

	// (1) The whole gate ran: the perceptual measurement is on the row.
	rows, err := ts.List(context.Background(), []store.Status{store.Done}, 0)
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	var done store.Outcome
	for _, r := range rows {
		if r.Path == src {
			done = r.Outcome
		}
	}
	if done.VmafMean == nil || done.VmafMin == nil || done.VmafChroma == nil {
		t.Errorf("the done row carries no VMAF measurement (%+v) - the cover-art path must run the "+
			"UNCHANGED gate, the perceptual one included", done)
	}
	if done.VmafModel == "" || done.VmafPixFmt == "" || done.VmafChromaMetric == "" {
		t.Errorf("the done row is missing the model (%q), the comparison format (%q) or the chroma "+
			"metric (%q) that a real measurement records", done.VmafModel, done.VmafPixFmt, done.VmafChromaMetric)
	}

	// The output still carries both streams, and the cover is still an attached picture.
	assertVideoStreamShape(t, ffprobe, src, []bool{false, true})

	// (2) The cover art survived BYTE FOR BYTE.
	gotCover := extractVideoStream(t, ffmpeg, src, 1, filepath.Join(side, "output-cover.jpg"))
	if !bytes.Equal(gotCover, wantCover) {
		t.Errorf("the cover art changed across the transcode: %d bytes in, %d bytes out - an "+
			"attached picture must be CARRIED, never re-encoded", len(wantCover), len(gotCover))
	}
}

// TestSkipVocabularyIsDocumented makes the guard enumeration a mechanical obligation
// rather than a habit: every token in the engine's closed skip vocabulary must appear in
// the shipped enumeration an operator looks a `reason` up in.
//
// It reads the vocabulary out of the PACKAGE SOURCE rather than from a list maintained
// beside it, because a hand-kept list is the thing that goes stale: a token added as a
// constant and forgotten in the list would ship undocumented and this test would still
// pass. Parsing the declarations means the only way to add a token is to add a constant,
// and the only way to add a constant without reding this test is to document it.
func TestSkipVocabularyIsDocumented(t *testing.T) {
	root, err := docscheck.RepoRoot(".")
	if err != nil {
		t.Fatalf("locate the repository root: %v", err)
	}

	vocabulary := skipVocabulary(t, filepath.Join(root, "internal", "engine"))
	// Anti-vacuity: a parse that found nothing would satisfy every loop below.
	if len(vocabulary) < 10 {
		t.Fatalf("only %d Skip* constant(s) were parsed out of the engine package (%v) - the "+
			"vocabulary is not being read, so nothing below is being checked", len(vocabulary), vocabulary)
	}
	if got := vocabulary["SkipMultiVideoStream"]; got != "multi-video-stream" {
		t.Errorf("SkipMultiVideoStream = %q, want %q - the guard's reason is drawn from the "+
			"vocabulary, not written as a literal at the call site", got, "multi-video-stream")
	}

	row := skippedReasonRow(t, filepath.Join(root, "docs", "api-reference.md"))
	for name, tok := range vocabulary {
		if !strings.Contains(row, "`"+tok+"`") {
			t.Errorf("%s = %q is in the engine's skip vocabulary but is NOT named in the guard "+
				"enumeration in docs/api-reference.md - a token no operator can look up must not ship",
				name, tok)
		}
	}
}

// skipVocabulary parses every non-test Go file in dir and returns the Skip* string
// constants it declares, keyed by constant name.
func skipVocabulary(t *testing.T, dir string) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}
	out := map[string]string{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.CONST {
					continue
				}
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range vs.Names {
						if !strings.HasPrefix(name.Name, "Skip") || i >= len(vs.Values) {
							continue
						}
						lit, ok := vs.Values[i].(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							continue
						}
						v, uerr := strconv.Unquote(lit.Value)
						if uerr != nil {
							t.Fatalf("%s: cannot read the value of %s: %v", dir, name.Name, uerr)
						}
						out[name.Name] = v
					}
				}
			}
		}
	}
	return out
}

// skippedReasonRow returns the one row of the shipped record reference that enumerates
// the skip guards. It FAILS rather than returning "" when that row is not there: a
// missing enumeration must red this test, not silently satisfy every lookup against an
// empty string.
func skippedReasonRow(t *testing.T, doc string) string {
	t.Helper()
	b, err := os.ReadFile(doc)
	if err != nil {
		t.Fatalf("read %s: %v", doc, err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "| `reason` |") && strings.Contains(line, "skipped") {
			return line
		}
	}
	t.Fatalf("%s carries no skipped-`reason` row - the guard enumeration this gate checks against "+
		"is gone, so nothing is enumerated", doc)
	return ""
}

// TestDecodeOK_FailsWhenAnAdditionalVideoStreamIsDamaged is the decode-integrity gate at
// its new width. The fixture is built so that the FIRST video stream decodes perfectly
// and the second does not, which is exactly the file a check reading `0:v:0` reports as
// clean - the precondition below asserts that, so this test states in its own body what
// it reds against.
func TestDecodeOK_FailsWhenAnAdditionalVideoStreamIsDamaged(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	p := func(n string) string { return filepath.Join(d, n) }
	ctx := context.Background()

	// Two elementary streams, one of which is then damaged in the middle. Damaging an
	// elementary stream BEFORE the mux is what puts the corruption in a known stream;
	// flipping bytes in a finished container hits whichever stream they happened to
	// belong to.
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=320x240:rate=10", "-c:v", "libx264", "-preset", "ultrafast",
		"-b:v", "2M", "-bsf:v", "h264_mp4toannexb", "-f", "h264", "--", p("first.h264"))
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "mandelbrot=size=320x240:rate=10", "-t", "2", "-c:v", "libx264", "-preset", "ultrafast",
		"-b:v", "2M", "-bsf:v", "h264_mp4toannexb", "-f", "h264", "--", p("second.h264"))

	raw, err := os.ReadFile(p("second.h264"))
	if err != nil {
		t.Fatalf("read the second elementary stream: %v", err)
	}
	if len(raw) < 16384 {
		t.Fatalf("the second elementary stream is %d bytes - too small to damage its middle", len(raw))
	}
	damagedBytes := append([]byte(nil), raw...)
	for i := len(damagedBytes) / 2; i < len(damagedBytes)/2+4096; i++ {
		damagedBytes[i] ^= 0xff
	}
	if err := os.WriteFile(p("second-damaged.h264"), damagedBytes, 0o644); err != nil {
		t.Fatalf("write the damaged elementary stream: %v", err)
	}

	mux := func(second, out string) {
		ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
			"-i", p("first.h264"), "-i", second, "-map", "0:v", "-map", "1:v", "-c", "copy", "--", out)
	}
	mux(p("second-damaged.h264"), p("damaged.mkv"))
	mux(p("second.h264"), p("clean.mkv"))
	assertVideoStreamShape(t, ffprobe, p("damaged.mkv"), []bool{false, false})
	assertVideoStreamShape(t, ffprobe, p("clean.mkv"), []bool{false, false})

	// The precondition, and the whole point: decoding the FIRST stream alone - the check
	// this gate used to be - reports the damaged file as clean.
	firstStreamOnly := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-nostdin", "-v", "error",
		"-xerror", "-err_detect", "+explode", "-i", p("damaged.mkv"), "-map", "0:v:0", "-f", "null", "-")
	if err := firstStreamOnly.Run(); err != nil {
		t.Fatalf("the damaged fixture's FIRST stream does not decode either (%v) - a narrower check "+
			"would have caught this file, so it proves nothing about decoding every stream", err)
	}

	pr := probe.New(ffmpeg, ffprobe)
	if pr.DecodeOK(ctx, p("damaged.mkv")) {
		t.Error("DecodeOK accepted a file whose SECOND video stream does not decode - the check must " +
			"decode every video stream the output carries, not the first alone")
	}
	// Anti-vacuity: the identically-built file with an undamaged second stream passes, so
	// the rejection above is attributable to the damage and not to the wider map.
	if !pr.DecodeOK(ctx, p("clean.mkv")) {
		t.Error("DecodeOK rejected a two-stream file that is not damaged at all")
	}
}

// equalStrings reports whether two string slices hold the same elements in the same
// order (the directory listings compared above).
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
