package vmaf

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/cpuquota"
)

// comparisonFormat is the format both sides of every fixture pair below are converted to.
// It is the format the default path lands on (an 8-bit source meeting a 10-bit output), so
// the conversion the speed work touches is really exercised rather than skipped.
const comparisonFormat = "yuv420p10le"

// threadedPair builds the fixture AC-3 is measured over: a source produced from lavfi and
// an imperfect encode of it. It needs no file from an operator's media library, which is
// the criterion's own requirement, and it is produced by the gate run itself.
//
// It is deliberately not a perfect encode. A pair scoring a flat 100 would agree within
// 0.01 whatever threading did to it, and the criterion would be satisfied by a test that
// cannot fail.
func threadedPair(t *testing.T, bin string) (distorted, reference string) {
	t.Helper()
	dir := t.TempDir()
	reference = filepath.Join(dir, "ref.mp4")
	distorted = filepath.Join(dir, "dist.mp4")
	mustFF(t, bin, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=3:size=640x480:rate=30", "-c:v", "libx264", "-b:v", "6M", reference)
	mustFF(t, bin, "-hide_banner", "-loglevel", "error", "-y", "-i", reference,
		"-c:v", "libx265", "-crf", "34", "-preset", "veryfast",
		"-x265-params", "log-level=error", distorted)
	return distorted, reference
}

// derivedThreads is the count this host's own quota produces, so the criterion is measured
// at the value that will actually ship here and not only at numbers the test picked.
func derivedThreads(t *testing.T) int {
	t.Helper()
	q, err := cpuquota.Read(cpuquota.DefaultRoot)
	if err != nil {
		t.Logf("this host's CPU quota could not be read (%v), so the derived count is the "+
			"stated fallback of %d", err, cpuquota.FallbackShare)
		return cpuquota.FallbackShare
	}
	return cpuquota.Divide(q, 1)
}

// TestS0160_AC3_ThreadCountDoesNotMoveTheGateFigures is the criterion this whole item
// turns on, and the reason it is graded by measurement rather than by argument: no source
// found says in words that a thread count cannot change a VMAF score, so this build is
// asked directly.
//
// The reference is the SINGLE-THREADED measurement of the same pair on the same build,
// taken in this run and held as the figure every threaded pass is compared against. It is
// measured here rather than committed as a number because a committed figure would be a
// fact about one ffmpeg build: repinning ffmpeg would red it without anything being wrong,
// and the honest subject is the DIFFERENCE between two passes of one build, not the
// absolute value of either.
//
// What it holds: the mean, the worst-frame pool and the chroma figure all within 0.01, and
// the gate's own decision unchanged in both directions - a floor above the pair must
// reject at both thread counts and a floor below it must pass at both. If the drift ever
// exceeds 0.01 the finding is that scoring on this build is not thread-invariant. The
// tolerance is not the thing to widen: the source is deleted on the strength of this
// number, and a tolerance wide enough to swallow a gate flip licenses deleting a source on
// a figure nobody checked.
func TestS0160_AC3_ThreadCountDoesNotMoveTheGateFigures(t *testing.T) {
	bin := ffmpegBin()
	if _, err := exec.LookPath(bin); err != nil {
		t.Fatalf("::error:: ffmpeg required for the thread-invariance proof: %v", err)
	}
	dist, ref := threadedPair(t, bin)

	base := Request{Distorted: dist, Reference: ref, Subsample: 1,
		Model: "version=vmaf_v0.6.1", PixelFormat: comparisonFormat}

	single := base
	single.Threads = 1
	want, err := Score(context.Background(), bin, single)
	if err != nil {
		t.Fatalf("the single-threaded reference measurement failed: %v", err)
	}
	// Anti-vacuity: a pair that scored a flat 100 with no chroma damage would agree at any
	// thread count for reasons that have nothing to do with threading.
	if want.HarmonicMean >= 99.99 || want.Min >= 99.99 {
		t.Fatalf("the fixture is too good to measure anything (harmonic_mean=%.4f min=%.4f): "+
			"agreement within 0.01 would be satisfied by a test that cannot fail",
			want.HarmonicMean, want.Min)
	}
	t.Logf("single-threaded reference: harmonic_mean=%.6f min=%.6f chroma=%.6f (%s, %s)",
		want.HarmonicMean, want.Min, want.ChromaMin, want.ChromaMetric, want.PixelFormat)

	// The count this host derives, plus fixed counts so the proof is not vacuous on a host
	// whose quota derives 1.
	counts := map[string]int{"derived": derivedThreads(t), "2": 2, "4": 4, "8": 8}
	for name, n := range counts {
		t.Run(name+" threads", func(t *testing.T) {
			r := base
			r.Threads = n
			got, err := Score(context.Background(), bin, r)
			if err != nil {
				t.Fatalf("Score at %d threads: %v", n, err)
			}
			for _, f := range []struct {
				what       string
				want, have float64
			}{
				{"harmonic_mean", want.HarmonicMean, got.HarmonicMean},
				{"worst-frame min", want.Min, got.Min},
				{"chroma min", want.ChromaMin, got.ChromaMin},
			} {
				if d := math.Abs(f.want - f.have); d > 0.01 {
					t.Errorf("%s moved by %.6f between 1 thread (%.6f) and %d threads (%.6f): scoring "+
						"on this build is NOT thread-invariant. Report it; do not widen the tolerance - "+
						"the source is deleted on the strength of this number",
						f.what, d, f.want, n, f.have)
				}
			}
			// The gate's own decision, in both directions, so "the same decision" is not
			// satisfied by a pair that passes everything.
			for _, floors := range []struct {
				what               string
				mean, pool, chroma float64
				wantPass           bool
			}{
				{"floors below the pair", want.HarmonicMean - 1, want.Min - 1, want.ChromaMin - 1, true},
				{"floors above the pair", want.HarmonicMean + 1, want.Min + 1, want.ChromaMin + 1, false},
			} {
				a := passesFloors(want, floors.mean, floors.pool, floors.chroma)
				b := passesFloors(got, floors.mean, floors.pool, floors.chroma)
				if a != floors.wantPass {
					t.Fatalf("%s: the single-threaded reference itself %s, so this arm decides nothing",
						floors.what, verdict(a))
				}
				if a != b {
					t.Errorf("%s: the gate %s at 1 thread and %s at %d threads - the thread count "+
						"changed the VERDICT that licenses deleting a source",
						floors.what, verdict(a), verdict(b), n)
				}
			}
		})
	}
}

// passesFloors is the gate's decision written out: all three floors, and all three needed.
// It is the comparison internal/engine makes against a profile's min_vmaf, vmaf_min_pool
// and vmaf_min_chroma; the floors are parameters here because this test's subject is
// whether the DECISION moves with the thread count, not where an operator put the bar.
func passesFloors(r Result, mean, pool, chroma float64) bool {
	return r.HarmonicMean >= mean && r.Min >= pool && r.ChromaMin >= chroma
}

func verdict(pass bool) string {
	if pass {
		return "PASSED"
	}
	return "REJECTED"
}

// TestS0160_AC1_TheGraphAlwaysNamesAnExplicitThreadCount: the built graph carries
// libvmaf's part of the share it was given, spelled out, on every request - all of the
// share but the filtergraph's one thread, and never less than 1.
func TestS0160_AC1_TheGraphAlwaysNamesAnExplicitThreadCount(t *testing.T) {
	for n, libvmaf := range map[int]int{1: 1, 2: 1, 3: 2, 6: 5, 24: 23} {
		r := Request{Distorted: "d.mkv", Reference: "r.mkv", Subsample: 1,
			Model: "version=vmaf_v0.6.1", PixelFormat: comparisonFormat, Threads: n}
		graph := BuildFilter(r, "/tmp/x.json")
		// Anchored at the end of the graph, where the option is written, so a count of 16
		// cannot satisfy an assertion about 1.
		want := fmt.Sprintf("n_threads=%d", libvmaf)
		if !strings.HasSuffix(graph, want) {
			t.Errorf("the graph does not end in %q, so libvmaf would fall back to its own default "+
				"and score on about one CPU:\n%s", want, graph)
		}
	}
}

// TestS0160_AC6_TheGraphNeverNamesZeroThreads is AC-6's teeth. 0 is libvmaf's own default
// for n_threads and the library reads it as "no thread pool", so a graph carrying it says
// the opposite of what a reader would take it to say. Score refuses a request below 1
// outright; BuildFilter, which is exported and called directly, floors it.
func TestS0160_AC6_TheGraphNeverNamesZeroThreads(t *testing.T) {
	for _, n := range []int{0, -1, -32} {
		r := Request{Distorted: "d.mkv", Reference: "r.mkv", Subsample: 1,
			Model: "version=vmaf_v0.6.1", PixelFormat: comparisonFormat, Threads: n}
		graph := BuildFilter(r, "/tmp/x.json")
		if strings.HasSuffix(graph, "n_threads=0") || strings.Contains(graph, "n_threads=-") {
			t.Errorf("Threads=%d produced a graph naming a thread count libvmaf reads as its own "+
				"default rather than as a request:\n%s", n, graph)
		}
		if !strings.HasSuffix(graph, "n_threads=1") {
			t.Errorf("Threads=%d did not floor to 1:\n%s", n, graph)
		}
	}
}

// TestS0160_AC8_AnInvocationTheGateCannotRunIsRefused: a thread count libvmaf cannot act
// on and an input the filter will not accept both come back as errors carrying no Result,
// so the caller cannot read a score out of a measurement that did not happen.
func TestS0160_AC8_AnInvocationTheGateCannotRunIsRefused(t *testing.T) {
	bin := ffmpegBin()
	if _, err := exec.LookPath(bin); err != nil {
		t.Fatalf("::error:: ffmpeg required for the refusal proof: %v", err)
	}
	dist, ref := threadedPair(t, bin)

	t.Run("a thread count libvmaf would read as its own default", func(t *testing.T) {
		for _, n := range []int{0, -4} {
			res, err := Score(context.Background(), bin, Request{
				Distorted: dist, Reference: ref, Subsample: 1,
				Model: "version=vmaf_v0.6.1", PixelFormat: comparisonFormat, Threads: n})
			if err == nil {
				t.Fatalf("Threads=%d scored %+v instead of being refused", n, res)
			}
			if res != (Result{}) {
				t.Errorf("Threads=%d returned %+v beside its error", n, res)
			}
			if !strings.Contains(err.Error(), "thread count") {
				t.Errorf("the refusal does not name the thread count: %v", err)
			}
		}
	})

	t.Run("an input the filter will not accept", func(t *testing.T) {
		// A file with no video stream at all: the graph names [1:v:0] and libavfilter
		// cannot bind it, so real ffmpeg rejects the invocation the gate built.
		audio := filepath.Join(t.TempDir(), "audio.m4a")
		mustFF(t, bin, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
			"-i", "anullsrc=r=48000:cl=stereo", "-t", "1", "-c:a", "aac", audio)
		res, err := Score(context.Background(), bin, Request{
			Distorted: dist, Reference: audio, Subsample: 1,
			Model: "version=vmaf_v0.6.1", PixelFormat: comparisonFormat, Threads: 1})
		if err == nil {
			t.Fatalf("scoring against a file with no video stream returned %+v instead of an error", res)
		}
		if res != (Result{}) {
			t.Errorf("a rejected invocation returned %+v beside its error", res)
		}
	})
}

// TestS0160_AC4_TheComparisonFormatIsUnchangedByTheThreadCount: the format both streams
// are converted to, and the format recorded beside the score, are what they were - the
// conversion-side change is about how fast the conversion runs and not about where it
// lands.
func TestS0160_AC4_TheComparisonFormatIsUnchangedByTheThreadCount(t *testing.T) {
	bin := ffmpegBin()
	if _, err := exec.LookPath(bin); err != nil {
		t.Fatalf("::error:: ffmpeg required for the comparison-format proof: %v", err)
	}
	dist, ref := threadedPair(t, bin)
	named, ok := ComparisonFormat(probePixFmt(t, ref), probePixFmt(t, dist))
	if !ok {
		t.Fatal("ComparisonFormat refused the fixture pair")
	}

	for _, n := range []int{1, 4, 8} {
		r := Request{Distorted: dist, Reference: ref, Subsample: 1,
			Model: "version=vmaf_v0.6.1", PixelFormat: named, Threads: n}
		// The graph converts BOTH sides to the named format, at every thread count.
		graph := BuildFilter(r, "/tmp/x.json")
		if got := strings.Count(graph, "format="+named); got != 2 {
			t.Errorf("at %d threads the graph names format=%s %d time(s), want 2 (one per side):\n%s",
				n, named, got, graph)
		}
		got, err := Score(context.Background(), bin, r)
		if err != nil {
			t.Fatalf("Score at %d threads: %v", n, err)
		}
		if got.PixelFormat != named {
			t.Errorf("at %d threads the score records format %q, want %q", n, got.PixelFormat, named)
		}
	}
}

// TestS0160_AC10_EveryFrameIsScoredAtEveryThreadCount: with no sampling interval
// configured the gate scores every frame, and the thread count does not change which
// frames those are.
//
// It counts the frames libvmaf actually reported, not the option the graph carries: the
// claim is about what was measured, and an option can be present and ignored.
func TestS0160_AC10_EveryFrameIsScoredAtEveryThreadCount(t *testing.T) {
	bin := ffmpegBin()
	if _, err := exec.LookPath(bin); err != nil {
		t.Fatalf("::error:: ffmpeg required for the frame-count proof: %v", err)
	}
	dist, ref := threadedPair(t, bin)
	// The fixture is 3 seconds at 30 fps. Unconfigured means Subsample 0, which is what a
	// Request assembled without the key carries, and it must reach libvmaf as 1.
	const wantFrames = 90

	var first []int
	for _, n := range []int{1, 4, 8} {
		r := Request{Distorted: dist, Reference: ref, Subsample: 0,
			Model: "version=vmaf_v0.6.1", PixelFormat: comparisonFormat, Threads: n}
		if !strings.Contains(BuildFilter(r, "/tmp/x.json"), "n_subsample=1") {
			t.Fatalf("an unconfigured interval did not reach libvmaf as every frame:\n%s",
				BuildFilter(r, "/tmp/x.json"))
		}
		scored := scoredFrames(t, bin, r)
		if len(scored) != wantFrames {
			t.Errorf("at %d threads libvmaf scored %d frames, want all %d", n, len(scored), wantFrames)
		}
		if first == nil {
			first = scored
			continue
		}
		if len(scored) != len(first) {
			t.Fatalf("at %d threads libvmaf scored %d frames against %d at one thread",
				n, len(scored), len(first))
		}
		for i := range scored {
			if scored[i] != first[i] {
				t.Errorf("at %d threads the %dth scored frame is %d, was %d at one thread: the "+
					"thread count changed WHICH frames were scored", n, i, scored[i], first[i])
				break
			}
		}
	}
}

// TestS0160_AC11_TheSamplingIntervalIsOperatorConfigurationAndNothingElse: the interval
// that reaches libvmaf is exactly the one the request carries, at every thread count a
// quota could derive. A build that reached for the CPU count or the quota when choosing
// it - which would change what the worst-frame and chroma floors bound, without the
// operator asking - reds here.
func TestS0160_AC11_TheSamplingIntervalIsOperatorConfigurationAndNothingElse(t *testing.T) {
	for _, configured := range []int{1, 2, 5, 37} {
		var derived []int
		for _, cpus := range []float64{0.5, 1, 2, 4, 24, 96} {
			derived = append(derived, cpuquota.Divide(limitedQuota(cpus), 1))
		}
		for _, n := range derived {
			r := Request{Distorted: "d.mkv", Reference: "r.mkv", Subsample: configured,
				Model: "version=vmaf_v0.6.1", PixelFormat: comparisonFormat, Threads: n}
			// The trailing separator anchors the match: an interval of 10 must not satisfy
			// an assertion about 1.
			want := fmt.Sprintf("n_subsample=%d:", configured)
			if !strings.Contains(BuildFilter(r, "/tmp/x.json"), want) {
				t.Errorf("vmaf_subsample=%d at %d threads did not reach libvmaf as %q:\n%s",
					configured, n, want, BuildFilter(r, "/tmp/x.json"))
			}
		}
	}
}

// limitedQuota is a shorthand for the test above: a bandwidth limit of n CPUs.
func limitedQuota(cpus float64) cpuquota.Quota {
	return cpuquota.Quota{CPUs: cpus, Limited: true, Source: cpuquota.SourceCgroupV2}
}

// scoredFrames runs the REAL scoring graph and returns the frame numbers libvmaf reported,
// in order. It drives ffmpeg directly so the log survives the pass: Score removes its own.
func scoredFrames(t *testing.T, bin string, r Request) []int {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "vmaf.json")
	_, graphThreads := PoolThreads(r.Threads)
	out, err := exec.Command(bin, "-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-filter_complex_threads", fmt.Sprint(graphThreads),
		"-i", r.Distorted, "-i", r.Reference, "-lavfi", BuildFilter(r, logPath),
		"-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg failed at %d threads: %v\n%s", r.Threads, err, out)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading the libvmaf log: %v", err)
	}
	var parsed struct {
		Frames []struct {
			FrameNum int `json:"frameNum"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parsing the libvmaf log: %v", err)
	}
	nums := make([]int, 0, len(parsed.Frames))
	for _, f := range parsed.Frames {
		nums = append(nums, f.FrameNum)
	}
	return nums
}
