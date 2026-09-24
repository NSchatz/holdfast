package engine

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// The S0157 criteria. ffmpeg is outside holdfast's boundary, so where a criterion needs an
// encode that holds a known amount of resident memory, FFmpegEncoder.FFmpeg points at this
// test binary re-executed as a stand-in (s0157Helper): it maps and touches real memory, so
// the kernel reports it in /proc exactly as it would an ffmpeg's, and then holds, exits, or
// execs the real ffmpeg on the argv it was handed. The watchdog, the encoder's exec path and
// the engine's failure branch all run for real, over a real fixture and a real prober.

// s0157HelperEnv carries the path of the stand-in's script. It is set only for the test that
// uses the stand-in; every other invocation of this binary runs the suite.
const s0157HelperEnv = "HOLDFAST_S0157_FFMPEG_STANDIN"

func TestMain(m *testing.M) {
	if script := os.Getenv(s0157HelperEnv); script != "" {
		os.Exit(s0157Helper(script))
	}
	os.Exit(m.Run())
}

// s0157Script is what the stand-in does when it is run in ffmpeg's place.
type s0157Script struct {
	// EncodeFirst runs the real ffmpeg on the argv to completion before anything else, so the
	// working file is a complete, valid encode.
	EncodeFirst bool `json:"encode_first"`
	// Grow is how many bytes of resident memory to map and touch.
	Grow int64 `json:"grow"`
	// Hold is how long to hold that memory; "" holds until the process is ended.
	Hold string `json:"hold"`
	// Then is what follows the hold: "exec" the real ffmpeg on the argv, "exit0" or "exit1".
	Then string `json:"then"`
	// IgnoreTerm ignores SIGTERM; TermExit0 exits 0 on it.
	IgnoreTerm bool `json:"ignore_term"`
	TermExit0  bool `json:"term_exit0"`
	// FFmpeg is the real ffmpeg; Marks is the file each step appends a timestamp to.
	FFmpeg string `json:"ffmpeg"`
	Marks  string `json:"marks"`
}

func s0157Helper(scriptPath string) int {
	raw, err := os.ReadFile(scriptPath)
	if err != nil {
		return 2
	}
	var s s0157Script
	if err := json.Unmarshal(raw, &s); err != nil {
		return 2
	}
	mark := func(what string) {
		f, err := os.OpenFile(s.Marks, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%s %d\n", what, time.Now().UnixNano())
			_ = f.Close()
		}
	}
	if s.IgnoreTerm {
		signal.Ignore(syscall.SIGTERM)
	}
	if s.TermExit0 {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGTERM)
		go func() {
			<-c
			mark("term")
			os.Exit(0)
		}()
	}
	args := os.Args[1:]
	if s.EncodeFirst {
		cmd := exec.Command(s.FFmpeg, args...)
		if fd3 := os.NewFile(3, "progress"); fd3 != nil {
			if _, err := fd3.Stat(); err == nil {
				cmd.ExtraFiles = []*os.File{fd3}
			}
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			_, _ = os.Stderr.Write(out)
			return 1
		}
	}
	mark("grow")
	if s.Grow > 0 {
		b, err := syscall.Mmap(-1, 0, int(s.Grow), syscall.PROT_READ|syscall.PROT_WRITE,
			syscall.MAP_ANON|syscall.MAP_PRIVATE)
		if err != nil {
			return 3
		}
		for i := 0; i < len(b); i += 4096 {
			b[i] = 1
		}
	}
	mark("grown")
	if s.Hold == "" {
		for {
			time.Sleep(time.Hour)
		}
	}
	d, _ := time.ParseDuration(s.Hold)
	time.Sleep(d)
	switch s.Then {
	case "exec":
		_ = syscall.Exec(s.FFmpeg, append([]string{s.FFmpeg}, args...), os.Environ())
		return 1
	case "exit1":
		return 1
	}
	return 0
}

// s0157Limit is the cgroup memory limit the watched cases are run under, and s0157Threshold
// is 85% of it rounded down. The stand-in grows by the whole limit to cross it and by
// s0157Small to stay well below it.
const (
	s0157Limit     = int64(768) << 20
	s0157Threshold = int64(684510412)
	s0157Small     = int64(64) << 20
)

// s0157CgroupRoot writes a fixture cgroup hierarchy holding the given files at its root.
func s0157CgroupRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if strings.HasSuffix(name, "/") {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// s0157Bound derives the run's bound from a fixture cgroup whose memory.max is limit, the
// way a run derives it, and fails unless it is armed at the expected threshold.
func s0157Bound(t *testing.T, limit int64, extra map[string]string) MemoryBound {
	t.Helper()
	files := map[string]string{"memory.max": strconv.FormatInt(limit, 10) + "\n"}
	for k, v := range extra {
		files[k] = v
	}
	p := DeriveMemoryWatch(s0157CgroupRoot(t, files))
	if !p.Bound.Armed() || p.Bound.Limit != limit {
		t.Fatalf("a memory.max of %d derived %+v (err %v), want an armed bound at that limit", limit, p.Bound, p.Err)
	}
	return p.Bound
}

// s0157Rig is one library holding one fixture source, an engine over it and what it
// recorded: its structured log, its events, and the stand-in's marks.
type s0157Rig struct {
	root, src, marks string
	ffmpeg, ffprobe  string
	cfg              config.Config
	store            *testStore
	eng              *Engine
	logs             *lockedBuffer

	mu       sync.Mutex
	events   []Event
	failedAt time.Time
}

// s0157Setup builds a rig. script nil runs the real ffmpeg; otherwise the stand-in, with
// the script's FFmpeg and Marks filled in.
func s0157Setup(t *testing.T, script *s0157Script, bound MemoryBound, mutate func(*config.Config)) *s0157Rig {
	t.Helper()
	ffmpeg, ffprobe := tools(t)
	r := &s0157Rig{root: t.TempDir(), ffmpeg: ffmpeg, ffprobe: ffprobe, logs: &lockedBuffer{}}
	r.src = filepath.Join(r.root, "movie.mkv")
	mkH264(t, ffmpeg, r.src, "8M")
	r.cfg = baseCfg(r.root)
	if mutate != nil {
		mutate(&r.cfg)
	}
	bin := ffmpeg
	if script != nil {
		bin = s0157StandIn(t, script)
		r.marks = script.Marks
	}
	r.store = newTestStore(t, r.root)
	prober := probe.New(ffmpeg, ffprobe)
	enc := FFmpegEncoder{FFmpeg: bin, Cfg: r.cfg, Probe: prober, Memory: bound}
	r.eng = New(r.cfg, prober, enc, r.store, slog.New(slog.NewJSONHandler(r.logs, nil)))
	r.eng.Observer = func(ev Event) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.events = append(r.events, ev)
		if ev.Status == store.Failed && r.failedAt.IsZero() {
			r.failedAt = time.Now()
		}
	}
	return r
}

// s0157StandIn writes script for the stand-in and returns the binary to run in ffmpeg's
// place: this test binary.
func s0157StandIn(t *testing.T, script *s0157Script) string {
	t.Helper()
	ffmpeg, _ := tools(t)
	real, err := exec.LookPath(ffmpeg)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script.FFmpeg = real
	script.Marks = filepath.Join(dir, "marks")
	b, err := json.Marshal(script)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "script.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(s0157HelperEnv, path)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return self
}

// process runs one ProcessFile under a context that reaps a stuck child if the test fails.
func (r *s0157Rig) process(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := r.eng.ProcessFile(ctx, "w0", r.src); err != nil {
		t.Fatalf("ProcessFile: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("the encode was still running at the test's own deadline")
	}
}

// markTimes is every time the stand-in recorded what.
func (r *s0157Rig) markTimes(t *testing.T, what string) []time.Time {
	t.Helper()
	b, err := os.ReadFile(r.marks)
	if err != nil {
		return nil
	}
	var out []time.Time
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		name, ns, ok := strings.Cut(line, " ")
		if !ok || name != what {
			continue
		}
		n, err := strconv.ParseInt(ns, 10, 64)
		if err != nil {
			t.Fatalf("bad mark %q", line)
		}
		out = append(out, time.Unix(0, n))
	}
	return out
}

// row is the store's row for the source, whatever its status.
func (r *s0157Rig) row(t *testing.T) store.Job {
	t.Helper()
	rows, err := r.store.List(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	for _, j := range rows {
		if j.Path == r.src {
			return j
		}
	}
	t.Fatalf("no row for %s", r.src)
	return store.Job{}
}

// failedEvent is the one failed event the run emitted.
func (r *s0157Rig) failedEvent(t *testing.T) Event {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var failed []Event
	for _, ev := range r.events {
		if ev.Status == store.Failed {
			failed = append(failed, ev)
		}
	}
	if len(failed) != 1 {
		t.Fatalf("%d failed events, want exactly 1: %+v", len(failed), r.events)
	}
	return failed[0]
}

// records is every structured log record the engine wrote whose msg is msg.
func (r *s0157Rig) records(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(strings.NewReader(r.logs.String()))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("the engine wrote a line that is not a JSON record: %s", sc.Text())
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

const s0157AbortMsg = "FAIL (encode aborted for memory, source untouched)"

// s0157Identity is what AC-4 holds a source to: its bytes, its size and its mtime.
type s0157Identity struct {
	sum   [32]byte
	size  int64
	mtime time.Time
}

func s0157Identify(t *testing.T, path string) s0157Identity {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return s0157Identity{sum: sha256.Sum256(b), size: fi.Size(), mtime: fi.ModTime()}
}

// s0157ReasonRSS is the resident figure an abort reason reports.
var s0157ReasonRSS = regexp.MustCompile(`resident memory reached (\d+) bytes`)

// assertMemoryAbort grades AC-3 on a finished rig: the row is failed, the event is
// attributed to the encode gate, the reason says the encode was aborted for memory with the
// resident bytes, the limit and the threshold, and the failure came within ten seconds of
// the stand-in starting to grow - which is at or before the crossing.
func (r *s0157Rig) assertMemoryAbort(t *testing.T) {
	t.Helper()
	j := r.row(t)
	if j.Status != store.Failed {
		t.Fatalf("status = %q, want %q: %s", j.Status, store.Failed, j.Outcome.Reason)
	}
	if ev := r.failedEvent(t); ev.Gate != GateEncode {
		t.Errorf("the failed event names gate %q, want %q", ev.Gate, GateEncode)
	}
	reason := j.Outcome.Reason
	if !strings.Contains(reason, "aborted for memory") {
		t.Errorf("the reason does not say the encode was aborted for memory: %q", reason)
	}
	m := s0157ReasonRSS.FindStringSubmatch(reason)
	if m == nil {
		t.Fatalf("the reason gives no resident figure: %q", reason)
	}
	if rss, _ := strconv.ParseInt(m[1], 10, 64); rss < s0157Threshold {
		t.Errorf("the reason reports %d resident bytes, below the threshold %d it was aborted at", rss, s0157Threshold)
	}
	for _, want := range []int64{s0157Limit, s0157Threshold} {
		if !strings.Contains(reason, strconv.FormatInt(want, 10)+" bytes") {
			t.Errorf("the reason does not give %d bytes: %q", want, reason)
		}
	}
	grow := r.markTimes(t, "grow")
	if len(grow) != 1 {
		t.Fatalf("the stand-in started growing %d times, want 1", len(grow))
	}
	r.mu.Lock()
	took := r.failedAt.Sub(grow[0])
	r.mu.Unlock()
	if took > 10*time.Second {
		t.Errorf("the encode was recorded failed %v after its memory began to grow, want within 10s", took)
	}
}

// assertSourceUntouched grades AC-4: the same sha256, size and mtime, the same codec (no
// swap), and no working file or temp anywhere under dirs.
func (r *s0157Rig) assertSourceUntouched(t *testing.T, before s0157Identity, dirs ...string) {
	t.Helper()
	if after := s0157Identify(t, r.src); after != before {
		t.Errorf("the source changed across the abort: before %+v, after %+v", before, after)
	}
	if got := codecOf(t, r.ffprobe, r.src); got != "h264" {
		t.Errorf("the file at the source path is %q, want the untouched h264 source", got)
	}
	for _, d := range append([]string{r.root}, dirs...) {
		entries, err := os.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if filepath.Join(d, e.Name()) != r.src {
				t.Errorf("%s was left in %s after the abort", e.Name(), d)
			}
		}
		if n := nTemp(t, d); n != 0 {
			t.Errorf("%d temp file(s) left under %s", n, d)
		}
	}
}

// s0157Bounds are AC-1's ceilings, per option.
var s0157Bounds = map[string]int64{
	"-max_muxing_queue_size":       128,
	"-muxing_queue_data_threshold": 52428800,
	"-thread_queue_size":           8,
}

// assertBounds fails unless argv carries each AC-1 option, unqualified by a stream
// specifier (so it applies to every output stream), with a positive value no greater than
// its ceiling, after `-i <src>` and before the output path.
func assertBounds(t *testing.T, argv []string, src string) {
	t.Helper()
	in := -1
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-i" && argv[i+1] == src {
			in = i
		}
	}
	if in < 0 || len(argv) < 2 || argv[len(argv)-2] != "--" {
		t.Fatalf("argv is not `... -i %s ... -- <out>`: %q", src, argv)
	}
	for opt, ceiling := range s0157Bounds {
		at := -1
		for i, a := range argv {
			if a == opt {
				at = i
			}
		}
		if at < 0 {
			t.Errorf("argv carries no %s: %q", opt, argv)
			continue
		}
		if at <= in+1 || at >= len(argv)-2 {
			t.Errorf("%s is at %d, want after -i <source> (at %d) and before the output path: %q", opt, at, in, argv)
		}
		v, err := strconv.ParseInt(argv[at+1], 10, 64)
		if err != nil || v <= 0 || v > ceiling {
			t.Errorf("%s %q, want a positive value no greater than %d", opt, argv[at+1], ceiling)
		}
	}
}

// TestS0157_AC1_EveryEncodeArgvCarriesTheMuxQueueBounds grades AC-1 on the argv the
// production encoder assembled, for a re-encode and for a remux-only job.
func TestS0157_AC1_EveryEncodeArgvCarriesTheMuxQueueBounds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config.Config)
	}{
		{"re-encode", nil},
		{"remux-only", func(c *config.Config) { c.RemuxOnly = boolPtr(true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ffmpeg, ffprobe := tools(t)
			root := t.TempDir()
			src := filepath.Join(root, "movie.mkv")
			mkH264(t, ffmpeg, src, "8M")
			cfg := baseCfg(root)
			if tc.mutate != nil {
				tc.mutate(&cfg)
			}
			argv := newArgvLog()
			prober := probe.New(ffmpeg, ffprobe)
			run(t, ffmpeg, ffprobe, root, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober,
				argvObserver: argv.record}, tc.mutate)
			assertBounds(t, argv.forSource(t, src), src)
		})
	}
}

// TestS0157_AC2_BoundedEncodesOfRealFixturesReachDone grades AC-2: with the AC-1 bounds in
// the argv, a video, audio and subtitle source in the default container reaches done
// carrying every stream the intended map names, and the MP4 cover-art source, kept in its
// own container, reaches done with its cover carried byte for byte.
func TestS0157_AC2_BoundedEncodesOfRealFixturesReachDone(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	t.Run("video, audio and subtitle", func(t *testing.T) {
		root := t.TempDir()
		src := filepath.Join(root, "movie.mkv")
		mkSourceWithStreams(t, ffmpeg, src, audioStream("eng"), subtitleStream("eng"))
		cfg := baseCfg(root)
		eng := buildEngine(t, ffmpeg, ffprobe, root, nil, nil)
		plan := planFor(t, eng, src, cfg.RootProfiles()[0].Profile)
		intended := countByType(plan.Intended())
		if intended["video"] != 1 || intended["audio"] != 1 || intended["subtitle"] != 1 {
			t.Fatalf("the intended map names %v, want one video, one audio and one subtitle stream", intended)
		}
		argv := newArgvLog()
		prober := probe.New(ffmpeg, ffprobe)
		ts := run(t, ffmpeg, ffprobe, root, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober,
			argvObserver: argv.record}, nil)
		assertBounds(t, argv.forSource(t, src), src)
		if !ledgerHas(t, ts, store.Done, "movie.mkv") {
			t.Fatalf("the source did not reach done: %q", skipReason(t, ts, "movie.mkv"))
		}
		got := countByType(streamsOf(t, eng, src))
		if !reflect.DeepEqual(got, intended) {
			t.Errorf("the output carries %v, want the intended %v", got, intended)
		}
	})
	t.Run("MP4 cover art", func(t *testing.T) {
		root, side := t.TempDir(), t.TempDir()
		src := filepath.Join(root, "movie.mp4")
		mkMP4WithCoverArt(t, ffmpeg, ffprobe, src, "8M")
		want := extractVideoStream(t, ffmpeg, src, 1, filepath.Join(side, "source-cover.jpg"))
		mutate := func(c *config.Config) { c.ContainerExt = "source" }
		cfg := baseCfg(root)
		mutate(&cfg)
		argv := newArgvLog()
		prober := probe.New(ffmpeg, ffprobe)
		ts := run(t, ffmpeg, ffprobe, root, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober,
			argvObserver: argv.record}, mutate)
		assertBounds(t, argv.forSource(t, src), src)
		if !ledgerHas(t, ts, store.Done, "movie.mp4") {
			t.Fatalf("the cover-art source did not reach done: %q", skipReason(t, ts, "movie.mp4"))
		}
		assertVideoStreamShape(t, ffprobe, src, []bool{false, true})
		if got := extractVideoStream(t, ffmpeg, src, 1, filepath.Join(side, "output-cover.jpg")); string(got) != string(want) {
			t.Errorf("the cover art changed: %d bytes in, %d bytes out", len(want), len(got))
		}
	})
}

// TestS0157_AC3_AC4_AC13_AnEncodePastTheThresholdIsAbortedAndTheSourceIsUntouched grades
// AC-3 (terminated within the window, failed at the encode gate with the figures), AC-4 (the
// source, the working file and the temps, with scratch_dir unset and set) and AC-13's abort
// record.
func TestS0157_AC3_AC4_AC13_AnEncodePastTheThresholdIsAbortedAndTheSourceIsUntouched(t *testing.T) {
	for _, scratch := range []bool{false, true} {
		t.Run(fmt.Sprintf("scratch_dir set %v", scratch), func(t *testing.T) {
			var scratchDir string
			var mutate func(*config.Config)
			if scratch {
				scratchDir = t.TempDir()
				mutate = func(c *config.Config) { c.ScratchDir = scratchDir }
			}
			r := s0157Setup(t, &s0157Script{Grow: s0157Limit}, s0157Bound(t, s0157Limit, nil), mutate)
			before := s0157Identify(t, r.src)
			r.process(t)

			r.assertMemoryAbort(t)
			var extra []string
			if scratch {
				extra = append(extra, scratchDir)
			}
			r.assertSourceUntouched(t, before, extra...)

			recs := r.records(t, s0157AbortMsg)
			if len(recs) != 1 {
				t.Fatalf("%d %q records, want exactly 1", len(recs), s0157AbortMsg)
			}
			rec := recs[0]
			if rec["level"] != "WARN" || rec["file"] != r.src {
				t.Errorf("the abort record is %v for %v, want WARN for %s", rec["level"], rec["file"], r.src)
			}
			if rss, _ := rec["rss_bytes"].(float64); int64(rss) < s0157Threshold {
				t.Errorf("the abort record carries rss_bytes %v, want at least %d", rec["rss_bytes"], s0157Threshold)
			}
			if lim, _ := rec["memory_limit_bytes"].(float64); int64(lim) != s0157Limit {
				t.Errorf("the abort record carries memory_limit_bytes %v, want %d", rec["memory_limit_bytes"], s0157Limit)
			}
			if th, _ := rec["memory_threshold_bytes"].(float64); int64(th) != s0157Threshold {
				t.Errorf("the abort record carries memory_threshold_bytes %v, want %d", rec["memory_threshold_bytes"], s0157Threshold)
			}
		})
	}
}

// TestS0157_AC5_AnAbortedEncodeThatExitsZeroIsStillFailed grades AC-5: the stand-in first
// writes a complete, valid encode, then crosses the threshold and exits 0 when asked to stop.
// Read off the exit status alone, that is a success the verify gates would accept.
func TestS0157_AC5_AnAbortedEncodeThatExitsZeroIsStillFailed(t *testing.T) {
	r := s0157Setup(t, &s0157Script{EncodeFirst: true, Grow: s0157Limit, TermExit0: true},
		s0157Bound(t, s0157Limit, nil), nil)
	before := s0157Identify(t, r.src)
	r.process(t)
	if len(r.markTimes(t, "term")) != 1 {
		t.Fatalf("the stand-in did not exit through its SIGTERM handler, so this did not exercise an exit 0")
	}
	r.assertMemoryAbort(t)
	r.assertSourceUntouched(t, before)
}

// TestS0157_AC6_AnEncodeThatIgnoresTerminationIsKilledInsideTheWindow grades AC-6.
func TestS0157_AC6_AnEncodeThatIgnoresTerminationIsKilledInsideTheWindow(t *testing.T) {
	r := s0157Setup(t, &s0157Script{Grow: s0157Limit, IgnoreTerm: true}, s0157Bound(t, s0157Limit, nil), nil)
	before := s0157Identify(t, r.src)
	r.process(t)
	r.assertMemoryAbort(t)
	r.assertSourceUntouched(t, before)
}

// TestS0157_AC7_MemoryAbortsAreTransientAndBoundedByMaxFailures grades AC-7: each abort is
// recorded transient, and after max_failures of them the file is not claimed again.
func TestS0157_AC7_MemoryAbortsAreTransientAndBoundedByMaxFailures(t *testing.T) {
	r := s0157Setup(t, &s0157Script{Grow: s0157Limit}, s0157Bound(t, s0157Limit, nil),
		func(c *config.Config) { c.MaxFailures = 2 })
	for pass, want := range []int{1, 2, 2} {
		if err := r.eng.RunOneshot(context.Background()); err != nil {
			t.Fatalf("pass %d: %v", pass+1, err)
		}
		if got := failCount(t, r.store, "movie.mkv"); got != want {
			t.Fatalf("pass %d: fail count %d, want %d", pass+1, got, want)
		}
		if j := r.row(t); j.Outcome.FailureClass != store.FailureTransient || !strings.Contains(j.Outcome.Reason, "aborted for memory") {
			t.Fatalf("pass %d: class %q reason %q, want a transient memory abort", pass+1, j.Outcome.FailureClass, j.Outcome.Reason)
		}
	}
	if n := len(r.markTimes(t, "grow")); n != 2 {
		t.Errorf("the encoder ran %d times over three passes, want 2: a file past max_failures was claimed again", n)
	}
}

// TestS0157_AC8_CgroupUsageAtTheThresholdDoesNotAbort grades AC-8: the cgroup's own usage,
// page cache and tmpfs included, is at the limit, and the encode's own resident memory stays
// well below the threshold. The encode runs to done.
func TestS0157_AC8_CgroupUsageAtTheThresholdDoesNotAbort(t *testing.T) {
	full := strconv.FormatInt(s0157Limit, 10) + "\n"
	bound := s0157Bound(t, s0157Limit, map[string]string{
		"memory.current": full,
		"memory.stat":    "anon 1048576\nfile " + full,
	})
	r := s0157Setup(t, &s0157Script{Grow: s0157Small, Hold: "3s", Then: "exec"}, bound, nil)
	r.process(t)
	if j := r.row(t); j.Status != store.Done {
		t.Fatalf("status = %q, want %q: %s", j.Status, store.Done, j.Outcome.Reason)
	}
	if recs := r.records(t, s0157AbortMsg); len(recs) != 0 {
		t.Errorf("an encode below the threshold was aborted: %v", recs)
	}
}

// TestS0157_AC9_AWatchedEncodeBelowTheThresholdMatchesAnUnwatchedOne grades AC-9: the same
// fixture, at the same path, processed once watched under a limit far above its peak and once
// unwatched, reaches the same terminal state with the same outcome, times excepted.
func TestS0157_AC9_AWatchedEncodeBelowTheThresholdMatchesAnUnwatchedOne(t *testing.T) {
	watched := s0157Setup(t, nil, s0157Bound(t, int64(16)<<30, nil), nil)
	original, err := os.ReadFile(watched.src)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(watched.src)
	if err != nil {
		t.Fatal(err)
	}
	watched.process(t)
	first := watched.row(t)

	// The same bytes, the same mtime and the same path, into a fresh store and an unwatched
	// encoder.
	if err := os.WriteFile(watched.src, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(watched.src, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	ts := newTestStore(t, watched.root)
	prober := probe.New(watched.ffmpeg, watched.ffprobe)
	eng := New(watched.cfg, prober, FFmpegEncoder{FFmpeg: watched.ffmpeg, Cfg: watched.cfg, Probe: prober},
		ts, discardLogger())
	if err := eng.ProcessFile(context.Background(), "w0", watched.src); err != nil {
		t.Fatalf("ProcessFile: %v", err)
	}
	rows, err := ts.List(context.Background(), nil, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("the unwatched run left %d rows (%v)", len(rows), err)
	}
	second := rows[0]

	if first.Status != store.Done || second.Status != first.Status {
		t.Fatalf("watched %q, unwatched %q; want both %q (%s / %s)", first.Status, second.Status, store.Done,
			first.Outcome.Reason, second.Outcome.Reason)
	}
	a, b := first.Outcome, second.Outcome
	a.EncodeMs, b.EncodeMs = nil, nil
	if !reflect.DeepEqual(a, b) {
		t.Errorf("the recorded outcomes differ:\nwatched   %+v\nunwatched %+v", a, b)
	}
}

// TestS0157_AC10_ACancelledEncodeIsAnInterruptionNotAMemoryAbort grades AC-10: a context
// cancelled during a watched encode takes the interrupted branch - working file removed, the
// row left active for RecoverStale, nothing failed, and no memory-abort record.
func TestS0157_AC10_ACancelledEncodeIsAnInterruptionNotAMemoryAbort(t *testing.T) {
	r := s0157Setup(t, &s0157Script{Grow: s0157Small}, s0157Bound(t, s0157Limit, nil), nil)
	key := probe.Fingerprint(r.src)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			if b, _ := os.ReadFile(r.marks); strings.Contains(string(b), "grown ") {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		// Past at least one sample of a running, watched encode.
		time.Sleep(1500 * time.Millisecond)
		cancel()
	}()
	_ = r.eng.ProcessFile(ctx, "w0", r.src)

	status, fails, exists, err := r.store.Get(context.Background(), r.src, key)
	if err != nil || !exists {
		t.Fatalf("Get: status %q exists %v err %v", status, exists, err)
	}
	if !status.Active() || fails != 0 {
		t.Errorf("the interrupted job is %q with %d failures, want an active row with none", status, fails)
	}
	r.mu.Lock()
	for _, ev := range r.events {
		if ev.Status == store.Failed {
			t.Errorf("an interruption emitted a failed event: %+v", ev)
		}
	}
	r.mu.Unlock()
	if recs := r.records(t, s0157AbortMsg); len(recs) != 0 {
		t.Errorf("an interruption was recorded as a memory abort: %v", recs)
	}
	if n := nTemp(t, r.root); n != 0 {
		t.Errorf("%d working file(s) left after the interruption", n)
	}
	if n, err := r.store.RecoverStale(context.Background()); err != nil || n != 1 {
		t.Errorf("RecoverStale reset %d row(s) (%v), want 1", n, err)
	}
}

// TestS0157_AC11_AnUnreadableSampleDoesNotAbort grades AC-11: a sample that cannot be read
// decides nothing, and ffmpeg's own exit decides the encode.
func TestS0157_AC11_AnUnreadableSampleDoesNotAbort(t *testing.T) {
	t.Run("the reader", func(t *testing.T) {
		proc := t.TempDir()
		write := func(pid, body string) {
			if err := os.MkdirAll(filepath.Join(proc, pid), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(proc, pid, "status"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		write("10", "Name:\tffmpeg\nVmRSS:\t    2048 kB\nThreads:\t4\n")
		write("11", "Name:\tffmpeg\nState:\tZ (zombie)\nThreads:\t1\n")
		write("12", "VmRSS:\tlots kB\n")
		write("13", "VmRSS:\t2048 MB\n")
		if rss, ok := vmRSS(proc, 10); !ok || rss != 2048*1024 {
			t.Errorf("a readable VmRSS read as %d, %v; want %d, true", rss, ok, 2048*1024)
		}
		for _, pid := range []int{11, 12, 13, 14} {
			if rss, ok := vmRSS(proc, pid); ok {
				t.Errorf("pid %d read as %d bytes, want no sample", pid, rss)
			}
		}
		// A process that has exited and been reaped has no entry at all.
		cmd := exec.Command("true")
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
		if rss, ok := vmRSS("/proc", cmd.Process.Pid); ok {
			t.Errorf("a reaped process read as %d bytes, want no sample", rss)
		}
	})
	t.Run("the encode", func(t *testing.T) {
		for _, tc := range []struct {
			then    string
			wantErr bool
		}{{"exit0", false}, {"exit1", true}} {
			t.Run(tc.then, func(t *testing.T) {
				ffmpeg, ffprobe := tools(t)
				dir := t.TempDir()
				src := filepath.Join(dir, "movie.mkv")
				mkH264(t, ffmpeg, src, "8M")
				bin := s0157StandIn(t, &s0157Script{Grow: s0157Limit, Hold: "3s", Then: tc.then})
				enc := FFmpegEncoder{FFmpeg: bin, Cfg: baseCfg(dir), Probe: probe.New(ffmpeg, ffprobe),
					Memory: s0157Bound(t, s0157Limit, nil), procRoot: t.TempDir()}
				err := enc.Encode(context.Background(), src, filepath.Join(dir, "out.mkv"), nil)
				var mem *MemoryAbortError
				if errors.As(err, &mem) {
					t.Fatalf("an encode whose samples could not be read was aborted: %v", err)
				}
				if (err != nil) != tc.wantErr {
					t.Errorf("Encode = %v, want an error %v: ffmpeg's own exit decides", err, tc.wantErr)
				}
			})
		}
	})
}

// TestS0157_AC12_NoLimitRunsEveryEncodeUnwatched grades AC-12's engine half: a cgroup that
// says max, names no memory.max, or holds an unreadable or malformed one derives no bound,
// and an encode under no bound runs past any size to ffmpeg's own exit.
func TestS0157_AC12_NoLimitRunsEveryEncodeUnwatched(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"max":        {"memory.max": "max\n"},
		"absent":     {},
		"cgroup v1":  {"memory/memory.limit_in_bytes": "1048576\n"},
		"malformed":  {"memory.max": "garbage\n"},
		"unreadable": {"memory.max/": ""},
	} {
		if p := DeriveMemoryWatch(s0157CgroupRoot(t, files)); p.Bound != (MemoryBound{}) {
			t.Errorf("%s: derived %+v, want no bound", name, p.Bound)
		}
	}
	ffmpeg, ffprobe := tools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	bin := s0157StandIn(t, &s0157Script{Grow: s0157Limit, Hold: "2500ms", Then: "exit0"})
	enc := FFmpegEncoder{FFmpeg: bin, Cfg: baseCfg(dir), Probe: probe.New(ffmpeg, ffprobe)}
	if err := enc.Encode(context.Background(), src, filepath.Join(dir, "out.mkv"), nil); err != nil {
		t.Errorf("an unwatched encode did not run to ffmpeg's own exit: %v", err)
	}
}
