package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
	"github.com/NSchatz/holdfast/internal/vmaf"
)

// Fixtures and harnesses shared by the stream-selection cases in encode_test.go (what the
// encode carries) and verify_test.go (what the gate checks). No test lives here: this file
// is the real multi-stream sources those tests need and the seams they read.

// streamSpec is one non-video stream a fixture source carries: what kind it is, what
// language the container tags it with, and whether the container marks it as commentary.
// An EMPTY language writes no tag at all - which Matroska then stores as the undefined
// code, exactly as a real remux of an untagged track does.
type streamSpec struct {
	kind     string // "audio" or "subtitle"
	language string
	comment  bool
}

func audioStream(lang string) streamSpec { return streamSpec{kind: "audio", language: lang} }

func commentaryAudio(lang string) streamSpec {
	return streamSpec{kind: "audio", language: lang, comment: true}
}

func subtitleStream(lang string) streamSpec { return streamSpec{kind: "subtitle", language: lang} }

// mkSourceWithStreams writes an h264 clip carrying one video stream plus the given
// non-video streams, in the order given, with REAL language tags and REAL dispositions -
// so what the selection reads is what a container actually carries and not a struct a test
// filled in.
func mkSourceWithStreams(t *testing.T, ffmpeg, path string, specs ...streamSpec) {
	t.Helper()
	srt := filepath.Join(filepath.Dir(path), "subs-"+filepath.Base(path)+".srt")
	needSubs := false
	for _, s := range specs {
		if s.kind == "subtitle" {
			needSubs = true
		}
	}
	if needSubs {
		if err := os.WriteFile(srt, []byte("1\n00:00:00,000 --> 00:00:01,000\nHello\n\n"), 0o644); err != nil {
			t.Fatalf("write the subtitle fixture: %v", err)
		}
		defer func() { _ = os.Remove(srt) }()
	}

	args := []string{"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=duration=2:size=320x240:rate=10"}
	for i, s := range specs {
		if s.kind == "audio" {
			args = append(args, "-f", "lavfi", "-i",
				fmt.Sprintf("sine=frequency=%d:duration=2", 300+80*i))
			continue
		}
		args = append(args, "-f", "srt", "-i", srt)
	}
	args = append(args, "-map", "0:v")
	for i := range specs {
		args = append(args, "-map", fmt.Sprintf("%d:0", i+1))
	}
	args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-b:v", "8M",
		"-pix_fmt", "yuv420p", "-c:a", "aac", "-c:s", "copy")
	nAudio, nSubs := 0, 0
	for _, s := range specs {
		spec := ""
		if s.kind == "audio" {
			spec = fmt.Sprintf("a:%d", nAudio)
			nAudio++
		} else {
			spec = fmt.Sprintf("s:%d", nSubs)
			nSubs++
		}
		if s.language != "" {
			args = append(args, "-metadata:s:"+spec, "language="+s.language)
		}
		if s.comment {
			args = append(args, "-disposition:"+spec, "comment")
		}
	}
	args = append(args, "--", path)
	ff(t, ffmpeg, args...)
}

// planFor derives the intended stream map for a file exactly as ProcessFile derives it -
// from the source's own probe and the profile that decides it - so a test driving
// verifyOutput directly is handing the gate the same value the engine would.
func planFor(t *testing.T, eng *Engine, path string, prof config.Profile) *StreamPlan {
	t.Helper()
	streams, ok := eng.Probe.Streams(context.Background(), path)
	if !ok {
		t.Fatalf("ffprobe could not enumerate the streams of %s, so no intended map can be derived", path)
	}
	return DeriveStreamPlan(streams, prof, eng.Probe.VideoCodec(context.Background(), path))
}

// streamsOf is the file's streams as ffprobe reports them, for a test that asserts on what
// an output actually carries.
func streamsOf(t *testing.T, eng *Engine, path string) []probe.Stream {
	t.Helper()
	streams, ok := eng.Probe.Streams(context.Background(), path)
	if !ok {
		t.Fatalf("ffprobe could not enumerate the streams of %s", path)
	}
	return streams
}

// videoHash is the hash of a file's video bitstream, joined across its streams. It is what
// "the video was really copied" is asserted with, read through the same probe the identity
// check reads it through.
func videoHash(t *testing.T, ffmpeg, path string) string {
	t.Helper()
	hashes, ok := probe.New(ffmpeg, "").VideoStreamHashes(context.Background(), path)
	if !ok {
		t.Fatalf("the video streams of %s could not be hashed", path)
	}
	return strings.Join(hashes, "+")
}

// countByType counts a file's streams per ffprobe codec_type.
func countByType(streams []probe.Stream) map[string]int {
	n := map[string]int{}
	for _, s := range streams {
		n[s.Type]++
	}
	return n
}

// languagesOfType lists the language tags of one type's streams, in container order, with
// an untagged stream reported as "".
func languagesOfType(streams []probe.Stream, typ string) []string {
	var out []string
	for _, s := range streams {
		if s.Type == typ {
			out = append(out, s.Language)
		}
	}
	return out
}

// planLog records the intended stream map each stage was handed, which is the SEAM AC-6 is
// graded at: the encode announces the map its argv is built from and the gate announces the
// map it checked against, so a test can ask whether they were ONE derivation - and get the
// answer NO if anything ever derives a second one.
type planLog struct {
	mu   sync.Mutex
	seen map[string][]*StreamPlan
}

func newPlanLog() *planLog { return &planLog{seen: map[string][]*StreamPlan{}} }

func (l *planLog) record(stage string, plan *StreamPlan) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen[stage] = append(l.seen[stage], plan)
}

func (l *planLog) only(t *testing.T, stage string) *StreamPlan {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	plans := l.seen[stage]
	if len(plans) != 1 {
		t.Fatalf("stage %q read %d intended maps, want exactly 1", stage, len(plans))
	}
	return plans[0]
}

// selectionRun is one whole pass over one library root with the REAL production encoder,
// carrying every seam these criteria are graded through.
type selectionRun struct {
	eng   *Engine
	store *testStore
	argv  *argvLog
	plans *planLog
	score *int
}

// runSelection builds an engine over cfg with the real encoder and runs one oneshot pass,
// recording the argv the encoder assembled, the intended map each stage read, and how many
// times the VMAF gate measured anything.
//
// enc overrides the production encoder where a test needs an output the real one cannot
// produce; nil uses the real FFmpegEncoder, which is what every criterion about the argv
// requires.
func runSelection(t *testing.T, ffmpeg, ffprobe string, cfg config.Config, enc Encoder) selectionRun {
	t.Helper()
	argv := newArgvLog()
	plans := newPlanLog()
	prober := probe.New(ffmpeg, ffprobe)
	ts := newTestStore(t, filepath.Dir(cfg.LibraryRoots[0]))
	if enc == nil {
		enc = FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober, argvObserver: argv.record}
	}
	eng := New(cfg, prober, enc, ts, discardLogger())
	eng.planObserver = plans.record
	calls := 0
	// The measurement is supplied through the seam so a test can tell "the gate did not
	// run" from "the gate ran and libvmaf was not there". A run that reaches this has
	// measured something; a remux-only run must never reach it.
	eng.vmafScore = func(context.Context, vmaf.Request) (vmaf.Result, error) {
		calls++
		return passing(), nil
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	return selectionRun{eng: eng, store: ts, argv: argv, plans: plans, score: &calls}
}

// blindToStreamList is an ffprobe that answers every question the real one does EXCEPT the
// stream-list probe, which it refuses for any path matching pathSub ("" being every path).
//
// It is how the unenumerable-shape paths are driven for real rather than asserted about: a
// container ffprobe cannot read is not something a test can build, but a probe that will
// not answer about one is exactly the state such a container produces - and every guard
// above the stream list still gets its real answer, so the file reaches the point where the
// intended map would have been derived.
func blindToStreamList(t *testing.T, dir, realFFprobe, pathSub string) string {
	t.Helper()
	return delegatingFFprobe(t, dir, realFFprobe, probe.StreamEntries, pathSub)
}

// selectionCfg loads a configuration for one root with the engine-suite defaults these
// criteria need: nothing is skipped for being small, and the perceptual gate is ON, so a
// test that observes it not running has observed the SKIP rather than a gate that was off.
// extra is whatever the root's own entry carries, already indented.
func selectionCfg(t *testing.T, root string, extra string) config.Config {
	t.Helper()
	return profileCfg(t, "library_roots:\n  - path: "+root+"\n"+extra+`
preset: ultrafast
crf: 28
min_bitrate_kbps: 0
vmaf_enable: true
min_vmaf: 95
vmaf_min_pool: 60
vmaf_min_chroma: 30
`)
}

// selectionProfile resolves ONE root's profile from real YAML, so a case graded at the
// derivation is derived from the configuration an operator writes rather than from a
// struct a test filled in.
func selectionProfile(t *testing.T, extra string) config.Profile {
	t.Helper()
	cfg := selectionCfg(t, t.TempDir(), extra)
	return cfg.RootProfiles()[0].Profile
}

// doneRow is the terminal row this run recorded for path, asserting it is DONE. A case
// about what an encode carried proves nothing if the encode was rejected, and a status
// checked in one place is a message that names the rejection when it was.
func (r selectionRun) doneRow(t *testing.T, path string) store.Outcome {
	t.Helper()
	j := r.rowFor(t, path)
	if j.Status != store.Done {
		t.Fatalf("status = %q for %s, want %q: %s", j.Status, path, store.Done, j.Outcome.Reason)
	}
	return j.Outcome
}

// rowFor is the terminal row this run recorded for path, whatever its status.
func (r selectionRun) rowFor(t *testing.T, path string) store.Job {
	t.Helper()
	rows, err := r.store.List(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	for _, j := range rows {
		if j.Path == path {
			return j
		}
	}
	var seen []string
	for _, j := range rows {
		seen = append(seen, j.Path+" ("+string(j.Status)+")")
	}
	t.Fatalf("no row for %s (rows: %v)", path, seen)
	return store.Job{}
}

// argvHas reports whether args carries the given consecutive tokens.
func argvHas(args []string, want ...string) bool {
	for i := 0; i+len(want) <= len(args); i++ {
		if strings.Join(args[i:i+len(want)], "\x00") == strings.Join(want, "\x00") {
			return true
		}
	}
	return false
}
