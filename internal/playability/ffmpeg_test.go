package playability

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The checks against the pinned ffmpeg, over files it wrote: what an encoder writes passes,
// and a file whose cold start or segment does not decode fails while its linear decode - the
// decode-integrity gate's - passes.

// tools finds the pinned ffmpeg and ffprobe, failing loud rather than skipping: a skipped
// safety proof is a false green (docs/development.md).
func tools(t *testing.T) Checker {
	t.Helper()
	c := Checker{FFmpeg: os.Getenv("HOLDFAST_FFMPEG"), FFprobe: os.Getenv("HOLDFAST_FFPROBE")}
	if c.FFmpeg == "" {
		c.FFmpeg = "ffmpeg"
	}
	if c.FFprobe == "" {
		c.FFprobe = "ffprobe"
	}
	for _, b := range []string{c.FFmpeg, c.FFprobe} {
		if _, err := exec.LookPath(b); err != nil {
			t.Fatalf("::error:: %q not found - the playability fixtures require the pinned ffmpeg (set HOLDFAST_FFMPEG/FFPROBE): %v", b, err)
		}
	}
	return c
}

// fixtures are written once per test binary: every case reads them and none writes them.
var (
	fixturesOnce sync.Once
	fixturesDir  string
	fixturesErr  error
)

// The fixtures, each a few seconds of 160x128 at 24 fps with a keyframe every second.
const (
	openGOP   = "open.mkv"   // libx265 open GOP: CRA keyframes, RASL pictures
	closedGOP = "closed.mkv" // libx265 closed GOP: IDR keyframes only
	openMP4   = "open.mp4"   // the open-GOP stream in MP4
	openTS    = "open.ts"    // the open-GOP stream in MPEG-TS, which has no keyframe index
	closedMP4 = "closed.mp4" // the closed-GOP stream in MP4
	h264      = "h264.mkv"   // libx264 as holdfast runs it: closed GOP
	av1       = "av1.mkv"    // libsvtav1
	noBFrames = "nob.mkv"    // libx265 with no B pictures: nothing reordered
	crafted   = "crafted.mp4"
	craftedMK = "crafted.mkv"
)

// craftedAt is the presentation time, in the closed-GOP stream's MP4 ticks, of the trailing
// picture the crafted fixtures flag as a keyframe in place of the IDR at 6 s.
var craftedAt int64

func fixture(t *testing.T, name string) string {
	t.Helper()
	c := tools(t)
	fixturesOnce.Do(func() { fixturesDir, fixturesErr = writeFixtures(c) })
	if fixturesErr != nil {
		t.Fatal(fixturesErr)
	}
	return filepath.Join(fixturesDir, name)
}

func TestMain(m *testing.M) {
	code := m.Run()
	if fixturesDir != "" {
		_ = os.RemoveAll(fixturesDir)
	}
	os.Exit(code)
}

func run(ffmpeg string, args ...string) error {
	full := append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)
	if out, err := exec.Command(ffmpeg, full...).CombinedOutput(); err != nil {
		return errors.New(strings.Join(args, " ") + ": " + err.Error() + ": " + string(out))
	}
	return nil
}

func writeFixtures(c Checker) (string, error) {
	dir, err := os.MkdirTemp("", "playability-")
	if err != nil {
		return "", err
	}
	p := func(n string) string { return filepath.Join(dir, n) }
	src := func(sec string) []string {
		return []string{"-f", "lavfi", "-i", "testsrc2=duration=" + sec + ":size=160x128:rate=24"}
	}
	x265 := func(out, sec, params string) error {
		return run(c.FFmpeg, append(src(sec), "-c:v", "libx265", "-preset", "ultrafast", "-x265-params",
			"log-level=error:keyint=24:min-keyint=24:scenecut=0:"+params, "-pix_fmt", "yuv420p10le", out)...)
	}
	steps := []func() error{
		func() error { return x265(p(openGOP), "8", "open-gop=1:bframes=4") },
		func() error { return x265(p(closedGOP), "8", "open-gop=0:bframes=4") },
		func() error { return x265(p(noBFrames), "4", "bframes=0") },
		func() error { return run(c.FFmpeg, "-i", p(openGOP), "-c", "copy", p(openMP4)) },
		func() error { return run(c.FFmpeg, "-i", p(openGOP), "-c", "copy", p(openTS)) },
		func() error { return run(c.FFmpeg, "-i", p(closedGOP), "-c", "copy", p(closedMP4)) },
		func() error {
			return run(c.FFmpeg, append(src("4"), "-c:v", "libx264", "-preset", "ultrafast", "-g", "24",
				"-pix_fmt", "yuv420p", p(h264))...)
		},
		func() error {
			return run(c.FFmpeg, append(src("4"), "-c:v", "libsvtav1", "-preset", "12", "-g", "24",
				"-svtav1-params", "log-level=1", p(av1))...)
		},
		// The crafted file: the closed-GOP stream in MP4, its sync-sample table rewritten so
		// the IDR at 6 s is no longer flagged and the fifth picture after it in decode order, a
		// trailing picture, is. Every picture is the encoder's own and decodes from the start.
		func() error { return x265(p("crafted-src.mkv"), "10", "open-gop=0:bframes=4") },
		func() error { return run(c.FFmpeg, "-i", p("crafted-src.mkv"), "-c", "copy", p(crafted)) },
		func() error {
			at, err := moveSyncSample(p(crafted), 6*24+1, 6*24+6)
			craftedAt = at
			return err
		},
		func() error { return run(c.FFmpeg, "-i", p(crafted), "-c", "copy", p(craftedMK)) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return dir, err
		}
	}
	return dir, nil
}

// moveSyncSample rewrites an MP4's stss so the sync sample numbered from (1-based) is
// numbered to instead, and returns the presentation time of the sample to in the track's
// timescale, read back with ffprobe.
func moveSyncSample(path string, from, to uint32) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	stss, ok := descend(b, "moov", "trak", "mdia", "minf", "stbl", "stss")
	if !ok || len(stss) < 8 {
		return 0, errors.New("no stss in " + path)
	}
	n := binary.BigEndian.Uint32(stss[4:])
	moved := false
	for i := uint32(0); i < n; i++ {
		e := stss[8+4*i:]
		if binary.BigEndian.Uint32(e) == from {
			binary.BigEndian.PutUint32(e, to)
			moved = true
		}
	}
	if !moved {
		return 0, errors.New("sample is not a sync sample")
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return 0, err
	}
	s, err := Checker{FFprobe: envOr("HOLDFAST_FFPROBE", "ffprobe")}.ReadStream(context.Background(), path)
	if err != nil {
		return 0, err
	}
	return s.Packets[to-1].PTS, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// linearDecodeOK is the decode-integrity gate's own decode (probe.Prober.DecodeOK): every
// video stream from the first frame, -xerror -err_detect +explode, the exit status read.
func linearDecodeOK(t *testing.T, ffmpeg, file string) bool {
	t.Helper()
	return exec.Command(ffmpeg, "-hide_banner", "-nostdin", "-v", "error", "-xerror", "-err_detect", "+explode",
		"-i", file, "-map", "0:v", "-f", "null", "-").Run() == nil
}

// TestCheck_EveryShapeAnEncoderWritesPasses: every check over every shape of stream holdfast's
// encoders write - open and closed GOPs, reordered and not, HEVC, H.264 and AV1, in Matroska,
// MP4 and MPEG-TS - passes, with a cold start at EVERY keyframe.
func TestCheck_EveryShapeAnEncoderWritesPasses(t *testing.T) {
	c := tools(t)
	for _, name := range []string{openGOP, closedGOP, openMP4, openTS, h264, av1, noBFrames} {
		t.Run(name, func(t *testing.T) {
			if err := c.Check(context.Background(), fixture(t, name), nil, Options{ColdStarts: 1000, Segments: true}); err != nil {
				t.Fatalf("a file an encoder wrote failed: %v", err)
			}
		})
	}
}

// TestReadStream_TheFixturesShape pins the fixtures the other cases lean on: the open-GOP
// stream's keyframes after the first carry leading pictures in every container, and the
// closed-GOP one's and the one with no B pictures carry none. A drift in the encoder that took
// any of this away would leave the passes above proving less.
func TestReadStream_TheFixturesShape(t *testing.T) {
	c := tools(t)
	for _, tc := range []struct {
		name          string
		keys, leading int
		tb            Rational
	}{
		{openGOP, 8, 7, Rational{1, 1000}},
		{closedGOP, 8, 0, Rational{1, 1000}},
		{openMP4, 8, 7, Rational{1, 16000}},
		{openTS, 8, 7, Rational{1, 90000}},
		{noBFrames, 4, 0, Rational{1, 1000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := c.ReadStream(context.Background(), fixture(t, tc.name))
			if err != nil {
				t.Fatal(err)
			}
			keys := s.Keyframes()
			leading := 0
			for _, k := range keys {
				if s.Leads(k) {
					leading++
				}
			}
			if len(keys) != tc.keys || leading != tc.leading || s.TimeBase != tc.tb {
				t.Errorf("keys=%d leading=%d tb=%v, want %d %d %v", len(keys), leading, s.TimeBase, tc.keys, tc.leading, tc.tb)
			}
			if len(s.Packets) != 24*tc.keys {
				t.Errorf("%d packets, want %d", len(s.Packets), 24*tc.keys)
			}
		})
	}
}

// TestColdStart_AKeyframeFlaggedOnATrailingPictureFails is the fixture issue #200 asked for:
// a file the decode-integrity gate passes - every picture decodes from the first frame - but
// one of whose container keyframes is not a picture a decoder can start at. A cold start
// there fails, naming the keyframe and the decoder's own error, in MP4 and in Matroska.
func TestColdStart_AKeyframeFlaggedOnATrailingPictureFails(t *testing.T) {
	c := tools(t)
	for _, name := range []string{crafted, craftedMK} {
		t.Run(name, func(t *testing.T) {
			file := fixture(t, name)
			if !linearDecodeOK(t, c.FFmpeg, file) {
				t.Fatal("fixture drifted: the linear decode fails, so this proves nothing a linear decode does not")
			}
			err := c.Check(context.Background(), file, nil, Options{ColdStarts: 1000})
			var f *Failure
			if !errors.As(err, &f) {
				t.Fatalf("a cold start at a trailing picture flagged as a keyframe passed: %v", err)
			}
			at := "the keyframe at " + seconds(float64(craftedAt)/16000)
			if f.Check != "cold start" || f.At != at || !strings.Contains(f.Reason, "Could not find ref") {
				t.Errorf("failure %+v, want a cold start at %q naming the missing reference", f, at)
			}
			if strings.Contains(f.Reason, " @ 0x") {
				t.Errorf("the reason carries a memory address: %q", f.Reason)
			}
		})
	}
}

// TestSegments_AFragmentStartingAtATrailingPictureFails: the same crafted file, cut into
// fragmented MP4 by the remux. The flagged trailing picture is the first keyframe 6 s in, so
// the second segment starts at it, and decoded on its own it fails, with the cold starts off.
func TestSegments_AFragmentStartingAtATrailingPictureFails(t *testing.T) {
	c := tools(t)
	err := c.Check(context.Background(), fixture(t, crafted), nil, Options{Segments: true})
	var f *Failure
	if !errors.As(err, &f) {
		t.Fatalf("a segment starting at a trailing picture passed: %v", err)
	}
	if f.Check != "segment" || !strings.HasPrefix(f.At, "segment 2 (decode time 6.") || f.Reason == "" {
		t.Errorf("failure %+v, want segment 2 from about 6 s", f)
	}
}

// TestSegments_ADamagedFragmentFails: bytes overwritten in a stream's media data are found by
// the segment that carries them, named by its decode time. Each stream is 8 s with a keyframe
// every second. In the closed-GOP stream every keyframe is decoded on the second, so the second
// segment starts at the keyframe exactly 6 s past the first; in the open-GOP stream a keyframe
// is decoded ahead of the leading pictures before it, and the first one decoded 6 s or more past
// the first segment's start is the one at 6.875 s. The MP4 copies are damaged inside their one
// mdat, so the container itself still reads.
func TestSegments_ADamagedFragmentFails(t *testing.T) {
	c := tools(t)
	for _, tc := range []struct {
		name, file string
		from, to   float64 // fractions of the media data
		wantAt     string
	}{
		{"open GOPs, the first segment", openMP4, 0.33, 0.5, "segment 1 (decode time 0.000 s)"},
		{"open GOPs, the second segment", openMP4, 0.88, 0.97, "segment 2 (decode time 6.875 s)"},
		{"closed GOPs, the second segment", closedMP4, 0.88, 0.97, "segment 2 (decode time 6.000 s)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := os.ReadFile(fixture(t, tc.file))
			if err != nil {
				t.Fatal(err)
			}
			mdat, ok := child(b, "mdat")
			if !ok {
				t.Fatal("no mdat")
			}
			for off := int(float64(len(mdat)) * tc.from); off < int(float64(len(mdat))*tc.to); off += len(mdat) / 50 {
				for i := off; i < off+128 && i < len(mdat); i++ {
					mdat[i] ^= 0x5a
				}
			}
			damaged := filepath.Join(t.TempDir(), "damaged.mp4")
			if err := os.WriteFile(damaged, b, 0o644); err != nil {
				t.Fatal(err)
			}
			err = c.Segments(context.Background(), damaged)
			var f *Failure
			if !errors.As(err, &f) || f.Check != "segment" || f.At != tc.wantAt {
				t.Fatalf("got %v, want a failure at %s", err, tc.wantAt)
			}
		})
	}
}

// TestReadGOP_TheNALUnitTypesOfEachStream: the open-GOP stream's keyframes after the first are
// CRA pictures and it carries RASL pictures; the closed-GOP stream is closed; the crafted
// file's moved keyframe is a trailing picture; and the stream with no B pictures, coded with
// libx265's default open GOP, has CRA keyframes and no RASL.
func TestReadGOP_TheNALUnitTypesOfEachStream(t *testing.T) {
	c := tools(t)
	for _, tc := range []struct{ name, want string }{
		{openGOP, "7 of its 8 keyframes are not IDR or BLA pictures (the first, at 1.000 s, is CRA); 21 RASL pictures (the first at 0.917 s)"},
		{closedGOP, ""},
		{crafted, "1 of its 10 keyframes are not IDR or BLA pictures (the first, at 6.167 s, is TRAIL_N)"},
		{noBFrames, "3 of its 4 keyframes are not IDR or BLA pictures (the first, at 1.000 s, is CRA)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := fixture(t, tc.name)
			s, err := c.ReadStream(context.Background(), file)
			if err != nil {
				t.Fatal(err)
			}
			types, err := c.ReadGOP(context.Background(), file)
			if err != nil {
				t.Fatal(err)
			}
			if got := ClosedGOP(s, types); got != tc.want {
				t.Errorf("ClosedGOP = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestChecks_ABinaryThatCannotRunIsUnrunNotAFailure: a missing ffmpeg or ffprobe says nothing
// about the file, and every check reports it as an *Unrun.
func TestChecks_ABinaryThatCannotRunIsUnrunNotAFailure(t *testing.T) {
	file := fixture(t, closedGOP)
	real := tools(t)
	missing := filepath.Join(t.TempDir(), "no-such-binary")
	s, err := real.ReadStream(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	broken := Checker{FFmpeg: missing, FFprobe: missing}
	for name, run := range map[string]func() error{
		"packets":     func() error { _, err := broken.ReadStream(context.Background(), file); return err },
		"cold starts": func() error { return broken.ColdStarts(context.Background(), file, s, 4) },
		"segments":    func() error { return broken.Segments(context.Background(), file) },
		"gop":         func() error { _, err := broken.ReadGOP(context.Background(), file); return err },
		"check":       func() error { return broken.Check(context.Background(), file, nil, Options{Segments: true}) },
	} {
		t.Run(name, func(t *testing.T) {
			err := run()
			var u *Unrun
			if !errors.As(err, &u) || IsFailure(err) {
				t.Fatalf("got %v, want an *Unrun", err)
			}
		})
	}
}

// TestChecks_ACancelledContextIsUnrun: a check cancelled under it ran nothing to its end.
func TestChecks_ACancelledContextIsUnrun(t *testing.T) {
	c := tools(t)
	file := fixture(t, closedGOP)
	s, err := c.ReadStream(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, err := range map[string]error{
		"cold starts": c.ColdStarts(ctx, file, s, 4),
		"segments":    c.Segments(ctx, file),
	} {
		var u *Unrun
		if !errors.As(err, &u) || !errors.Is(err, context.Canceled) {
			t.Errorf("%s: got %v, want an *Unrun carrying the cancellation", name, err)
		}
	}
}

// TestChecks_AFileThatIsNotVideoFails: a file that is not media fails each check as a verdict
// about the file.
func TestChecks_AFileThatIsNotVideoFails(t *testing.T) {
	c := tools(t)
	junk := filepath.Join(t.TempDir(), "junk.mkv")
	if err := os.WriteFile(junk, []byte(strings.Repeat("not a video ", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadStream(context.Background(), junk); !IsFailure(err) || !strings.Contains(err.Error(), "could not list") {
		t.Errorf("packets: %v", err)
	}
	if err := c.Segments(context.Background(), junk); !IsFailure(err) || !strings.Contains(err.Error(), "could not be remuxed") {
		t.Errorf("segments: %v", err)
	}
	if _, err := c.ReadGOP(context.Background(), junk); !IsFailure(err) || !strings.Contains(err.Error(), "could not be copied out") {
		t.Errorf("gop: %v", err)
	}
}
