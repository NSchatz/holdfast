package engine

import (
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// THE CLOSED-GOP FIDELITY CHECK (docs/design/encode-plan.md#closed-gop) and THE PLAYABILITY
// GATE (docs/design/swap.md#playability), through whole jobs: a source, an encoder, every
// gate, and the row. Each red case is an output every gate before this change passed.

// gopEncoder is an encoder faithful in every field the gates before these two check - libx265
// at the test configuration's pixel format, every carried stream copied - whose GOP structure
// is what x265params say. After it writes the output, patch (where set) may rewrite it.
func gopEncoder(ffmpeg, x265params, muxer string, patch func(t *testing.T, out string)) func(t *testing.T) Encoder {
	return func(t *testing.T) Encoder {
		return EncoderFunc(func(ctx context.Context, in, out string, _ *probe.VideoProps) error {
			err := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-nostdin", "-v", "error", "-y", "-i", in,
				"-map", "0", "-c", "copy", "-c:v", "libx265", "-preset", "ultrafast", "-crf", "30",
				"-pix_fmt", "yuv420p10le", "-x265-params", "log-level=error:"+x265params,
				"-f", muxer, "--", out).Run()
			if err == nil && patch != nil {
				patch(t, out)
			}
			return err
		})
	}
}

// gopRun runs one oneshot pass over a 4 s source named name with the encoder mkEnc builds,
// and returns the source, its md5 before the run, the store and the events.
func gopRun(t *testing.T, name string, mkEnc func(t *testing.T) Encoder, mutate func(*config.Config)) (src, before string, st *testStore, events []Event) {
	t.Helper()
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	src = filepath.Join(root, name)
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=4:size=320x240:rate=12",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "8M", "-pix_fmt", "yuv420p", "--", src)
	before = md5f(t, src)
	cfg := baseCfg(root)
	cfg.ContainerExt = "source"
	if mutate != nil {
		mutate(&cfg)
	}
	st = newTestStore(t, root)
	prober := probe.New(ffmpeg, ffprobe)
	var enc Encoder = FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}
	if mkEnc != nil {
		enc = mkEnc(t)
	}
	eng := New(cfg, prober, enc, st, discardLogger())
	var mu sync.Mutex
	eng.Observer = func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	return src, before, st, events
}

// requireRejectedBy asserts the job failed at gate with class, its reason carrying every one
// of want, the source byte-identical and no working file left.
func requireRejectedBy(t *testing.T, gate string, class store.FailureClass, src, before string, st *testStore,
	events []Event, want ...string) {
	t.Helper()
	if got := md5f(t, src); got != before {
		t.Fatalf("the source changed (md5 %s -> %s): a rejected encode must leave it byte-identical", before, got)
	}
	if n := nTemp(t, filepath.Dir(src)); n != 0 {
		t.Errorf("%d temp file(s) left behind", n)
	}
	row := rowFor(t, st, src)
	if row.Status != store.Failed {
		t.Fatalf("row status %q, want %q (reason %q)", row.Status, store.Failed, row.Outcome.Reason)
	}
	var failed []Event
	for _, ev := range events {
		if ev.Status == store.Failed {
			failed = append(failed, ev)
		}
	}
	if len(failed) != 1 || failed[0].Gate != gate {
		t.Fatalf("failed events %+v, want exactly one at gate %q", failed, gate)
	}
	for _, w := range want {
		if !strings.Contains(row.Outcome.Reason, w) {
			t.Errorf("the reason does not carry %q: %q", w, row.Outcome.Reason)
		}
	}
	if row.Outcome.FailureClass != class {
		t.Errorf("failure class %q, want %q", row.Outcome.FailureClass, class)
	}
}

func requireDone(t *testing.T, src string, st *testStore) {
	t.Helper()
	if row := rowFor(t, st, src); row.Status != store.Done {
		t.Fatalf("row status %q, want %q (reason %q)", row.Status, store.Done, row.Outcome.Reason)
	}
	_, ffprobe := tools(t)
	if c := codecOf(t, ffprobe, src); c != "hevc" {
		t.Fatalf("the source was not replaced by the encode (codec %q)", c)
	}
}

func closedGOPs(c *config.Config) { no := false; c.X265OpenGOP = &no }

// openGOPParams codes a CRA with RASL pictures every second at 12 fps.
const openGOPParams = "keyint=12:min-keyint=12:scenecut=0:open-gop=1:bframes=3"

// TestClosedGOP_AnOpenGOPOutputUnderAClosedPlanIsRefused is the closed-GOP fixture: a plan that
// declares closed GOPs (x265_open_gop: false) and an encoder that wrote open ones - CRA
// keyframes and RASL pictures - in an output every other gate passes. The fidelity gate refuses
// it, naming closed-gop and what it found, deterministically, and the source is kept.
func TestClosedGOP_AnOpenGOPOutputUnderAClosedPlanIsRefused(t *testing.T) {
	ffmpeg, _ := tools(t)
	src, before, st, events := gopRun(t, "movie.mkv", gopEncoder(ffmpeg, openGOPParams, "matroska", nil), closedGOPs)
	requireRejectedBy(t, GateFidelity, store.FailureDeterministic, src, before, st, events,
		"output fidelity:", "closed-gop: want closed GOPs (x265_open_gop: false)",
		"3 of its 4 keyframes are not IDR or BLA pictures (the first, at 1.000 s, is CRA)", "RASL pictures")
}

// TestClosedGOP_TheSameOutputUnderAnOpenOrUnsetPlanPasses: the check is the plan's declaration
// and nothing else. The open-GOP output above passes every gate - the playability gate's cold
// starts at its RASL-bearing keyframes included - where the plan asks for open GOPs or says
// nothing, and the source is replaced.
func TestClosedGOP_TheSameOutputUnderAnOpenOrUnsetPlanPasses(t *testing.T) {
	ffmpeg, _ := tools(t)
	yes := true
	for name, mutate := range map[string]func(*config.Config){
		"unset": nil,
		"true":  func(c *config.Config) { c.X265OpenGOP = &yes },
	} {
		t.Run(name, func(t *testing.T) {
			src, _, st, _ := gopRun(t, "movie.mkv", gopEncoder(ffmpeg, openGOPParams, "matroska", nil), mutate)
			requireDone(t, src, st)
		})
	}
}

// TestClosedGOP_HoldfastsOwnClosedEncodePasses is the control: holdfast's own encoder under
// x265_open_gop: false writes closed GOPs, which the check passes, and the source is replaced.
func TestClosedGOP_HoldfastsOwnClosedEncodePasses(t *testing.T) {
	src, _, st, events := gopRun(t, "movie.mkv", nil, closedGOPs)
	requireDone(t, src, st)
	for _, ev := range events {
		if ev.Gate == GateFidelity || ev.Gate == GatePlayability {
			t.Fatalf("a closed-GOP encode was refused: %+v", ev)
		}
	}
}

// moveSyncSample rewrites the MP4 at path so its sync sample numbered from (1-based) is
// numbered to instead: the container then flags a trailing picture as a keyframe, and no
// longer flags the IDR before it. Every picture is the encoder's own, so a linear decode is
// untouched.
func moveSyncSample(t *testing.T, path string, from, to uint32) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stss := findBox(t, b, "moov", "trak", "mdia", "minf", "stbl", "stss")
	n := binary.BigEndian.Uint32(stss[4:])
	for i := uint32(0); i < n; i++ {
		if e := stss[8+4*i:]; binary.BigEndian.Uint32(e) == from {
			binary.BigEndian.PutUint32(e, to)
			if err := os.WriteFile(path, b, 0o644); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("sample %d is not a sync sample of %s", from, path)
}

// findBox is the body of the box at path inside b, a run of 32-bit-sized boxes.
func findBox(t *testing.T, b []byte, path ...string) []byte {
	t.Helper()
	for _, typ := range path {
		found := false
		for len(b) >= 8 {
			size := int(binary.BigEndian.Uint32(b))
			if size < 8 || size > len(b) {
				t.Fatalf("a box of %d bytes in %d", size, len(b))
			}
			if string(b[4:8]) == typ {
				b, found = b[8:size], true
				break
			}
			b = b[size:]
		}
		if !found {
			t.Fatalf("no %s box", typ)
		}
	}
	return b
}

// trailingKeyframe is the patch the playability fixture applies: the IDR two seconds in
// (sample 25, closed GOPs every second at 12 fps) is no longer flagged a keyframe, and the
// trailing picture four samples after it is.
func trailingKeyframe(t *testing.T, out string) { moveSyncSample(t, out, 25, 29) }

// closedGOPParams codes an IDR every second at 12 fps.
const closedGOPParams = "keyint=12:min-keyint=12:scenecut=0:open-gop=0:bframes=3"

// TestPlayability_AnOutputThatDoesNotStartAtAKeyframeIsRefused is the fixture issue #200 asked
// for: an output whose every picture decodes from the first frame - so the decode-integrity
// gate passes it, as it passes every gate before this one - but one of whose container
// keyframes is a trailing picture, so a player or a segmenter starting there gets a picture
// whose references it never decoded. The playability gate refuses it at that keyframe,
// deterministically, naming the decoder's error, and the source is kept.
func TestPlayability_AnOutputThatDoesNotStartAtAKeyframeIsRefused(t *testing.T) {
	ffmpeg, _ := tools(t)
	src, before, st, events := gopRun(t, "movie.mp4", gopEncoder(ffmpeg, closedGOPParams, "mp4", trailingKeyframe), nil)
	requireRejectedBy(t, GatePlayability, store.FailureDeterministic, src, before, st, events,
		"playability cold start at the keyframe at 2.", "Could not find ref", "The source is kept")
}

// TestPlayability_TheKeysTurnItOff: the same output under a root that turns both halves of the
// gate off passes every gate and replaces its source - which is also the proof that nothing
// before the playability gate refuses it.
func TestPlayability_TheKeysTurnItOff(t *testing.T) {
	ffmpeg, _ := tools(t)
	off, zero := false, 0
	src, _, st, _ := gopRun(t, "movie.mp4", gopEncoder(ffmpeg, closedGOPParams, "mp4", trailingKeyframe),
		func(c *config.Config) { c.PlayabilityColdStarts, c.PlayabilitySegments = &zero, &off })
	requireDone(t, src, st)
}

// TestPlayability_ACheckThatCannotRunIsTransient: an ffmpeg the gate cannot start says nothing
// about the output, so the rejection is transient and retried, never parked as final.
func TestPlayability_ACheckThatCannotRunIsTransient(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	out := filepath.Join(d, "out.mkv")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=duration=1:size=160x120:rate=10",
		"-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "log-level=error", "--", out)
	eng := buildEngine(t, ffmpeg, ffprobe, d, nil, nil)
	eng.Probe = probe.New(filepath.Join(d, "no-such-ffmpeg"), ffprobe)
	job := &EncodePlan{Output: out, Profile: eng.Cfg.TopLevelProfile()}
	class, err := eng.playabilityGate(context.Background(), job, eng.outputListing(out))
	if err == nil || class != store.FailureTransient || !strings.Contains(err.Error(), "could not be run") {
		t.Fatalf("class %q err %v, want a transient refusal saying the check could not be run", class, err)
	}
	// With both halves off nothing runs, so nothing is refused.
	off, zero := false, 0
	job.Profile.PlayabilityColdStarts, job.Profile.PlayabilitySegments = &zero, &off
	if class, err := eng.playabilityGate(context.Background(), job, eng.outputListing(out)); err != nil || class != "" {
		t.Fatalf("the gate turned off refused: %q %v", class, err)
	}
}

// TestPlayability_TheListingIsReadOnce: the closed-GOP check and the cold starts share one
// packet listing of the output.
func TestPlayability_TheListingIsReadOnce(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	out := filepath.Join(d, "out.mkv")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=duration=1:size=160x120:rate=10",
		"-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "log-level=error", "--", out)
	counted, log := countingWrapper(t, d, "ffprobe", ffprobe)
	eng := buildEngine(t, ffmpeg, counted, d, nil, nil)
	listing := eng.outputListing(out)
	a, errA := listing(context.Background())
	b, errB := listing(context.Background())
	if errA != nil || errB != nil || a != b || len(a.Packets) != 10 {
		t.Fatalf("listing: %v %v, %p %p", errA, errB, a, b)
	}
	if n := countCalls(t, log); n != 1 {
		t.Errorf("the listing ran ffprobe %d times, want once", n)
	}
}
