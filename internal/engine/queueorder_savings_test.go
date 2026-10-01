package engine

// `queue_order: savings_per_hour` (S0164) and queue PRIORITY, where they meet the
// enumeration. Every case names the acceptance criterion it grades (testing T1); the
// priority cases name the property in docs/design/queue-order.md#priority they grade.
//
// THE SEAM. The ordering's one probe per candidate goes through Engine.orderFactsFn, which a
// case replaces with a book of facts keyed by path: that is how a case supplies a 21.6 Mbps
// 45-minute source without writing seven gigabytes, and how it COUNTS what the ordering asks
// of the prober. The real prober is exercised by
// TestQueueOrder_AC12_RealClipsAreOrderedByTheirProbedBitrate, with no double at all. Every
// two-candidate ordering case names its files so that PATH ORDER IS THE OPPOSITE of the
// expected order, so the path tie-break cannot pass it.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/queuekey"
	"github.com/NSchatz/holdfast/internal/store"
)

// ---- the seam ---------------------------------------------------------------------

// factsBook stands in for the ordering's probe: the facts each path answers with, and a
// count of every call per path.
type factsBook struct {
	mu        sync.Mutex
	facts     map[string]sourceFacts
	calls     map[string]int
	bitrateOf map[string]bool
}

func newFactsBook() *factsBook {
	return &factsBook{facts: map[string]sourceFacts{}, calls: map[string]int{}, bitrateOf: map[string]bool{}}
}

// facts is an established source: a video stream, its bitrate, dimensions and duration.
func facts(kbps, width, height int, durationSec float64) sourceFacts {
	return sourceFacts{video: true, dims: true, source: queuekey.Source{
		VideoKbps: kbps, Width: width, Height: height, DurationSec: durationSec}}
}

func (b *factsBook) probe(_ context.Context, path string, withBitrate bool) sourceFacts {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls[path]++
	b.bitrateOf[path] = withBitrate
	f := b.facts[path] // an absent path answers as an unreadable file: no video stream
	if !withBitrate {
		f.source.VideoKbps = 0
	}
	return f
}

func (b *factsBook) total() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, c := range b.calls {
		n += c
	}
	return n
}

// savingsLibrary writes each file (one byte each unless sized) under a fresh root, gives
// each its facts in the book, and returns the root and an engine over it under order.
func savingsLibrary(t *testing.T, order string, files map[string]sourceFacts) (string, *Engine, *factsBook) {
	t.Helper()
	root := t.TempDir()
	book := newFactsBook()
	for rel, f := range files {
		p := filepath.Join(root, rel)
		mustWrite(t, p)
		book.facts[p] = f
	}
	eng := orderingEngine(t, root, order)
	eng.orderFactsFn = book.probe
	return root, eng, book
}

// rel turns enumerated full paths into paths relative to root, for readable messages.
func relAll(t *testing.T, root string, paths []string) []string {
	t.Helper()
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		r, err := filepath.Rel(root, p)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// ---- AC-3 to AC-6: what goes first ------------------------------------------------

// TestQueueOrder_AC3_TheHigherBitrateSourceGoesFirst: one picture size and length, two
// bitrates - the higher one carries more excess over what the encode produces.
func TestQueueOrder_AC3_TheHigherBitrateSourceGoesFirst(t *testing.T) {
	root, eng, _ := savingsLibrary(t, config.QueueOrderSavingsPerHour, map[string]sourceFacts{
		"a/lower.mkv":  facts(8_000, 1920, 1080, 3600),
		"b/higher.mkv": facts(20_000, 1920, 1080, 3600),
	})
	got := enumerated(t, eng, root)
	if want := []string{"b/higher.mkv", "a/lower.mkv"}; !slices.Equal(got, want) {
		t.Errorf("savings_per_hour offered %v, want %v", got, want)
	}
}

// TestQueueOrder_AC4_TheLowerResolutionSourceGoesFirstAtOneBitrate: one bitrate and length,
// two picture sizes - the same bitrate over fewer pixels is more excess per pixel and less
// work to encode and verify.
func TestQueueOrder_AC4_TheLowerResolutionSourceGoesFirstAtOneBitrate(t *testing.T) {
	root, eng, _ := savingsLibrary(t, config.QueueOrderSavingsPerHour, map[string]sourceFacts{
		"a/1080p.mkv": facts(8_000, 1920, 1080, 3600),
		"b/720p.mkv":  facts(8_000, 1280, 720, 3600),
	})
	got := enumerated(t, eng, root)
	if want := []string{"b/720p.mkv", "a/1080p.mkv"}; !slices.Equal(got, want) {
		t.Errorf("savings_per_hour offered %v, want %v", got, want)
	}
}

// TestQueueOrder_AC5_TheOperatorsCase is the operator's own case (S0164, the operator's
// report quoted in the umbrella spec): a 1920x1080 source at 21.6 Mbps lasting 45 minutes
// (about 7.3 GB) and one at 6.1 Mbps lasting 180 minutes (about 8.2 GB). savings_per_hour
// offers the 45-minute one first; largest offers the 180-minute one first. The files are
// really those sizes (sparse), so `largest` reads what a real scan reads.
func TestQueueOrder_AC5_TheOperatorsCase(t *testing.T) {
	files := map[string]sourceFacts{
		"a/long-180min.mkv": facts(6_100, 1920, 1080, 180*60),
		"b/short-45min.mkv": facts(21_600, 1920, 1080, 45*60),
	}
	sizes := map[string]int64{
		"a/long-180min.mkv": 6_100 * 1000 / 8 * 180 * 60,  // 8,235,000,000 bytes
		"b/short-45min.mkv": 21_600 * 1000 / 8 * 45 * 60, // 7,290,000,000 bytes
	}
	for _, tc := range []struct {
		order string
		want  []string
	}{
		{config.QueueOrderSavingsPerHour, []string{"b/short-45min.mkv", "a/long-180min.mkv"}},
		{config.QueueOrderLargest, []string{"a/long-180min.mkv", "b/short-45min.mkv"}},
	} {
		t.Run(tc.order, func(t *testing.T) {
			root, eng, _ := savingsLibrary(t, tc.order, files)
			for rel, n := range sizes {
				if err := os.Truncate(filepath.Join(root, rel), n); err != nil {
					t.Fatal(err)
				}
			}
			got := enumerated(t, eng, root)
			if !slices.Equal(got, tc.want) {
				t.Errorf("queue_order %s offered %v, want %v", tc.order, got, tc.want)
			}
		})
	}
}

// TestQueueOrder_AC6_ANonPositiveSavingGoesAfterEveryPositiveOneAndIsStillOffered: a source
// at or below what the encode is expected to produce (3,231.56 kbit/s for 1080p under the
// default model) is offered after every source with a positive saving, never dropped, and
// in path order among its kind.
func TestQueueOrder_AC6_ANonPositiveSavingGoesAfterEveryPositiveOneAndIsStillOffered(t *testing.T) {
	root, eng, _ := savingsLibrary(t, config.QueueOrderSavingsPerHour, map[string]sourceFacts{
		"a/at-the-model.mkv":   facts(3_000, 1920, 1080, 3600),
		"b/below-the-model.mkv": facts(1_000, 1920, 1080, 3600),
		"c/barely-positive.mkv": facts(3_300, 1920, 1080, 3600),
		"d/positive.mkv":        facts(5_000, 1920, 1080, 3600),
	})
	got := enumerated(t, eng, root)
	want := []string{"d/positive.mkv", "c/barely-positive.mkv", "a/at-the-model.mkv", "b/below-the-model.mkv"}
	if !slices.Equal(got, want) {
		t.Errorf("savings_per_hour offered %v, want %v", got, want)
	}

	// The same through a configured bitrate target: a 4,000 kbit/s source under a 5,000
	// kbit/s target is expected to grow, so it goes after a source that saves anything.
	cfg := baseCfg(root)
	cfg.QueueOrder = config.QueueOrderSavingsPerHour
	cfg.BitrateKbps = 5_000
	eng2 := toollessEngine(t, cfg, newTestStore(t, root), discardLogger())
	eng2.orderFactsFn = eng.orderFactsFn
	got = enumerated(t, eng2, root)
	want = []string{"a/at-the-model.mkv", "b/below-the-model.mkv", "c/barely-positive.mkv", "d/positive.mkv"}
	if !slices.Equal(got, want) {
		t.Errorf("under bitrate_kbps 5000 (every source saves nothing) savings_per_hour offered %v, want "+
			"every candidate in path order: %v", got, want)
	}
}

// ---- AC-7: a key that cannot be read ----------------------------------------------

// TestQueueOrder_AC7_AnUnreadableKeyIsOfferedLastWithOneWarnEach: a probe that found no
// video stream (a failed or timed-out probe, a vanished file), unestablished dimensions,
// and a zero duration or bitrate each put the candidate after every candidate whose key was
// read, in path order among them, with ONE warn record per file naming the file, the
// order, the operation and what happens next.
func TestQueueOrder_AC7_AnUnreadableKeyIsOfferedLastWithOneWarnEach(t *testing.T) {
	noDims := facts(9_000, 0, 0, 3600)
	noDims.dims = false
	root, eng, _ := savingsLibrary(t, config.QueueOrderSavingsPerHour, map[string]sourceFacts{
		"z/readable.mkv":     facts(9_000, 1920, 1080, 3600),
		"y/also-readable.mkv": facts(4_000, 1920, 1080, 3600),
		"e/no-video.mkv":      {},
		"d/no-dimensions.mkv": noDims,
		"c/zero-duration.mkv": facts(9_000, 1920, 1080, 0),
		"b/zero-bitrate.mkv":  facts(0, 1920, 1080, 3600),
	})
	vanished := filepath.Join(root, "a/vanished.mkv")
	mustWrite(t, vanished) // listed, then gone before the probe: the book has no facts for it

	logs := &bytes.Buffer{}
	eng.Log = slog.New(slog.NewJSONHandler(logs, nil))
	got := enumerated(t, eng, root)
	want := []string{"z/readable.mkv", "y/also-readable.mkv",
		"a/vanished.mkv", "b/zero-bitrate.mkv", "c/zero-duration.mkv", "d/no-dimensions.mkv", "e/no-video.mkv"}
	if !slices.Equal(got, want) {
		t.Errorf("savings_per_hour offered\n  %v\nwant\n  %v", got, want)
	}

	warns := map[string]int{}
	for _, rec := range records(t, logs) {
		if rec["level"] != "WARN" {
			continue
		}
		file, _ := rec["file"].(string)
		warns[file]++
		for _, k := range []string{"queue_order", "operation", "next", "err"} {
			if rec[k] == nil || rec[k] == "" {
				t.Errorf("the warn for %s carries no %q: %v", file, k, rec)
			}
		}
		if rec["queue_order"] != config.QueueOrderSavingsPerHour {
			t.Errorf("the warn for %s names queue_order %v", file, rec["queue_order"])
		}
	}
	for _, rel := range want[2:] {
		if n := warns[filepath.Join(root, rel)]; n != 1 {
			t.Errorf("%s: %d warn records, want exactly 1", rel, n)
		}
	}
	if len(warns) != 5 {
		t.Errorf("%d files were warned about, want the 5 unreadable ones: %v", len(warns), warns)
	}
}

// records decodes a JSON log into one map per record.
func records(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("a log line is not JSON: %v: %s", err, line)
		}
		out = append(out, m)
	}
	return out
}

// ---- AC-8: total, deterministic, and what plan reports ----------------------------

// TestQueueOrder_AC8_TwoPassesAgreeTiesBreakOnThePathAndPlanReportsTheSameSequence: on the
// coverage branch, where `movies/zulu.mkv` ARRIVES before `movies/sub/alpha.mkv`, two
// candidates with equal keys break on the full path ascending; two passes agree; and the
// read-only plan pass reports the sequence the scan works in.
func TestQueueOrder_AC8_TwoPassesAgreeTiesBreakOnThePathAndPlanReportsTheSameSequence(t *testing.T) {
	twin := facts(9_000, 1920, 1080, 3600)
	files := map[string]sourceFacts{
		"movies/zulu.mkv":      twin,
		"movies/sub/alpha.mkv": twin,
		"movies/mike.mkv":      facts(20_000, 1920, 1080, 600),
		"movies/sub/echo.mkv":  facts(4_000, 1920, 1080, 600),
	}
	root, _, book := savingsLibrary(t, config.QueueOrderSavingsPerHour, files)
	covered := func() *Engine {
		eng := coveredOrderingEngine(t, root, config.QueueOrderSavingsPerHour, "movies", "movies/sub")
		eng.orderFactsFn = book.probe
		return eng
	}
	want := []string{"movies/mike.mkv", "movies/sub/alpha.mkv", "movies/zulu.mkv", "movies/sub/echo.mkv"}
	first := enumerated(t, covered(), root)
	second := enumerated(t, covered(), root)
	if !slices.Equal(first, want) {
		t.Errorf("first pass offered %v, want %v (equal keys break on the full path ascending)", first, want)
	}
	if !slices.Equal(first, second) {
		t.Errorf("two passes over an unchanged library offered %v then %v", first, second)
	}

	eng := covered()
	pass := eng.Plan(context.Background(), PlanOptions{})
	var planned []string
	for _, f := range pass.Files {
		planned = append(planned, f.Path)
	}
	if got := relAll(t, root, planned); !slices.Equal(got, want) {
		t.Errorf("plan reported the sequence %v, the scan works in %v", got, want)
	}
}

// ---- AC-9: membership --------------------------------------------------------------

// TestQueueOrder_AC9_EveryOrderOffersTheSameFilesOnce: under each of the six values the same
// files are offered, each exactly once, over a library holding a candidate whose key cannot
// be read, one whose saving is not positive, and one a skip guard later skips (a hard link,
// which the hardlink guard skips); and the plan pass reaches the same verdict per file under
// savings_per_hour as under path. The plan's published document is graded in
// cmd/holdfast (TestPlan_AC9_SavingsPerHourPublishesWhatPathPublishes).
func TestQueueOrder_AC9_EveryOrderOffersTheSameFilesOnce(t *testing.T) {
	root, _, book := savingsLibrary(t, config.QueueOrderPath, map[string]sourceFacts{
		"a/positive.mkv":     facts(9_000, 1920, 1080, 3600),
		"b/non-positive.mkv": facts(1_000, 1920, 1080, 3600),
		"c/unreadable.mkv":   {},
		"d/hardlinked.mkv":   facts(9_000, 1920, 1080, 3600),
	})
	if err := os.Link(filepath.Join(root, "d/hardlinked.mkv"), filepath.Join(root, "d/hardlinked-twin.mkv")); err != nil {
		t.Fatal(err)
	}
	book.facts[filepath.Join(root, "d/hardlinked-twin.mkv")] = facts(9_000, 1920, 1080, 3600)

	verdicts := func(order string) ([]string, map[string]string) {
		eng := orderingEngine(t, root, order)
		eng.orderFactsFn = book.probe
		got := enumerated(t, eng, root)
		v := map[string]string{}
		for _, f := range eng.Plan(context.Background(), PlanOptions{}).Files {
			v[f.Path] = fmt.Sprintf("guard=%q unreadable=%v", f.Guard, f.Unreadable)
		}
		return got, v
	}
	want, wantVerdicts := verdicts(config.QueueOrderPath)
	if len(want) != 5 {
		t.Fatalf("the fixture enumerates %v, want five files", want)
	}
	if !strings.Contains(wantVerdicts[filepath.Join(root, "d/hardlinked.mkv")], SkipHardlinked) {
		t.Fatalf("the fixture's hard link is not skipped by the hardlink guard: %v", wantVerdicts)
	}
	slices.Sort(want)
	for _, order := range config.QueueOrders {
		got, v := verdicts(order)
		sorted := slices.Clone(got)
		slices.Sort(sorted)
		if !slices.Equal(sorted, want) || len(got) != len(want) {
			t.Errorf("queue_order %s offered %v, want each of %v exactly once", order, got, want)
		}
		if !reflect.DeepEqual(v, wantVerdicts) {
			t.Errorf("queue_order %s: plan verdicts\n  %v\nwhere path gives\n  %v", order, v, wantVerdicts)
		}
	}
}

// ---- AC-10: the key reading writes nothing ----------------------------------------

// TestQueueOrder_AC10_ReadingKeysChangesNoFileAndNoRow: every candidate's bytes and
// modification time, and the store's rows, are the same immediately before and after the
// key-reading phase.
func TestQueueOrder_AC10_ReadingKeysChangesNoFileAndNoRow(t *testing.T) {
	root, base, book := savingsLibrary(t, config.QueueOrderSavingsPerHour, map[string]sourceFacts{
		"a/one.mkv": facts(9_000, 1920, 1080, 3600),
		"b/two.mkv": {},
	})
	st := newTestStore(t, root)
	eng := toollessEngine(t, base.Cfg, st, discardLogger())
	eng.orderFactsFn = book.probe
	ctx := context.Background()
	eng.EnsureHoldBacks(ctx)
	seeded := filepath.Join(root, "a/one.mkv")
	if _, err := st.Claim(ctx, seeded, probe.Fingerprint(seeded), "seed", 3, store.DecisionInputs{}); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	type state struct {
		sum   string
		mtime time.Time
	}
	snapshot := func() (map[string]state, string) {
		files := map[string]state{}
		for _, rel := range []string{"a/one.mkv", "b/two.mkv"} {
			p := filepath.Join(root, rel)
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			files[rel] = state{md5f(t, p), fi.ModTime()}
		}
		rows, err := st.List(ctx, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(rows)
		return files, string(b)
	}
	beforeFiles, beforeRows := snapshot()
	if got := enumerated(t, eng, root); len(got) != 2 {
		t.Fatalf("enumerated %v", got)
	}
	afterFiles, afterRows := snapshot()
	if !reflect.DeepEqual(beforeFiles, afterFiles) {
		t.Errorf("reading keys changed a file: %v -> %v", beforeFiles, afterFiles)
	}
	if beforeRows != afterRows {
		t.Errorf("reading keys changed the store:\n%s\n->\n%s", beforeRows, afterRows)
	}
}

// ---- AC-11: what the ordering costs ------------------------------------------------

// TestQueueOrder_AC11_SavingsProbesEachCandidateOnceAndNoOtherOrderProbesAtAll counts the
// ordering's probes through its seam: exactly one per candidate under savings_per_hour (and
// no attribute read for ordering), none at all under the five other values; `path` reads no
// attribute either (and hands the first file out before the listing finishes:
// TestScan_ThePathOrderFeedsAWorkerBeforeTheLibraryIsListed, unchanged).
func TestQueueOrder_AC11_SavingsProbesEachCandidateOnceAndNoOtherOrderProbesAtAll(t *testing.T) {
	files := map[string]sourceFacts{
		"a/one.mkv":   facts(9_000, 1920, 1080, 3600),
		"b/two.mkv":   facts(4_000, 1280, 720, 1800),
		"c/three.mkv": {},
	}
	for _, order := range config.QueueOrders {
		t.Run(order, func(t *testing.T) {
			root, eng, book := savingsLibrary(t, order, files)
			var stats atomic.Int64
			countingStat(eng, &stats)
			if got := enumerated(t, eng, root); len(got) != len(files) {
				t.Fatalf("enumerated %v", got)
			}
			if order != config.QueueOrderSavingsPerHour {
				if n := book.total(); n != 0 {
					t.Errorf("queue_order %s took %d ordering probes, want 0", order, n)
				}
				if order == config.QueueOrderPath && stats.Load() != 0 {
					t.Errorf("queue_order path read %d attributes, want 0", stats.Load())
				}
				return
			}
			for rel := range files {
				if n := book.calls[filepath.Join(root, rel)]; n != 1 {
					t.Errorf("%s was probed %d times for ordering, want exactly 1", rel, n)
				}
			}
			if n := stats.Load(); n != 0 {
				t.Errorf("savings_per_hour also read %d attributes for ordering, want 0", n)
			}
		})
	}
}

// ---- AC-12: real media -------------------------------------------------------------

// TestQueueOrder_AC12_RealClipsAreOrderedByTheirProbedBitrate generates two clips with the
// pinned ffmpeg at one resolution and duration and two FORCED, well-separated bitrates
// (S0164 verdict F1), named so path order is the opposite of the expected order, and orders
// them through the engine's real prober - no double. The bitrates the probe saw are logged.
func TestQueueOrder_AC12_RealClipsAreOrderedByTheirProbedBitrate(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	mk := func(name, kbps string) string {
		p := filepath.Join(root, name)
		// Noise makes the picture expensive enough that the encoder has to spend the rate it
		// is given; minrate/maxrate/bufsize pin it there.
		ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
			"-i", "testsrc2=duration=3:size=320x240:rate=24,noise=alls=60:allf=t",
			"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
			"-b:v", kbps+"k", "-minrate", kbps+"k", "-maxrate", kbps+"k", "-bufsize", kbps+"k",
			"-x264-params", "nal-hrd=cbr", "--", p)
		return p
	}
	high := mk("b-high.mkv", "3000")
	low := mk("a-low.mkv", "400")

	cfg := baseCfg(root)
	cfg.QueueOrder = config.QueueOrderSavingsPerHour
	prober := probe.New(ffmpeg, ffprobe)
	eng := New(cfg, prober, refusingEncoder{t}, newTestStore(t, root), discardLogger())
	eng.EnsureHoldBacks(context.Background())

	hk := prober.VideoProps(context.Background(), high).BitrateKbps()
	lk := prober.VideoProps(context.Background(), low).BitrateKbps()
	t.Logf("probed bitrates: %s %d kbit/s, %s %d kbit/s", filepath.Base(high), hk, filepath.Base(low), lk)
	if hk < 3*lk {
		t.Fatalf("the clips came out at %d and %d kbit/s, not well separated: the case would grade noise", hk, lk)
	}
	got := enumerated(t, eng, root)
	if want := []string{"b-high.mkv", "a-low.mkv"}; !slices.Equal(got, want) {
		t.Errorf("savings_per_hour over real clips offered %v, want %v", got, want)
	}
}

// ---- AC-13: a stop while keys are read ----------------------------------------------

// TestQueueOrder_AC13_AStopWhileKeysAreReadStopsProbingAndOffersNothing: a scan paused after
// the second probe starts no further probe, hands no file to a worker in that pass, and
// reports as observed no directory it never listed.
func TestQueueOrder_AC13_AStopWhileKeysAreReadStopsProbingAndOffersNothing(t *testing.T) {
	files := map[string]sourceFacts{}
	for _, rel := range []string{"a/1.mkv", "a/2.mkv", "a/3.mkv", "b/4.mkv", "b/5.mkv"} {
		files[rel] = facts(9_000, 1920, 1080, 3600)
	}
	root, _, book := savingsLibrary(t, config.QueueOrderSavingsPerHour, files)
	cfg := baseCfg(root)
	cfg.QueueOrder = config.QueueOrderSavingsPerHour
	spent := &spentAWorker{Store: newTestStore(t, root)}
	eng := toollessEngine(t, cfg, spent, discardLogger())
	eng.Coverage = []string{filepath.Join(root, "a"), filepath.Join(root, "b")}
	eng.orderFactsFn = book.probe
	eng.Paused = func() bool { return book.total() >= 2 }

	ctx := context.Background()
	eng.EnsureHoldBacks(ctx)
	observed, err := eng.scanOnce(ctx, eng.passListings(), nil)
	if err != nil {
		t.Fatalf("scanOnce: %v", err)
	}
	if n := book.total(); n != 2 {
		t.Errorf("%d probes were taken, want 2: no probe may start after the stop is observed", n)
	}
	if got := spent.spent(); len(got) != 0 {
		t.Errorf("a worker was handed %v in a pass stopped while keys were read", got)
	}
	if observed[filepath.Join(root, "b")] {
		t.Errorf("directory b was never listed and is reported observed: %v", observed)
	}
}

// ---- AC-14: progress -----------------------------------------------------------------

// TestQueueOrder_AC14_KeyReadingReportsProgressAndACountBeforeTheFirstOffer reads 2,500
// synthetic candidates (one in ten unreadable): a progress record at least once per 1,000
// candidates, and one info record stating how many keys were read and how many could not be,
// written before the first candidate is offered.
func TestQueueOrder_AC14_KeyReadingReportsProgressAndACountBeforeTheFirstOffer(t *testing.T) {
	const candidates, directories = 2_500, 25
	const lib = "/lib"
	dirs := make([]string, directories)
	for i := range dirs {
		dirs[i] = fmt.Sprintf("%s/d%02d", lib, i)
	}
	cfg := config.Config{LibraryRoots: []string{lib}, VideoExts: []string{"mkv"},
		QueueOrder: config.QueueOrderSavingsPerHour}
	logs := &lockedBuffer{}
	eng := New(cfg, probe.New("", ""), nil, nil, slog.New(slog.NewJSONHandler(logs, nil)))
	eng.Coverage = dirs
	eng.readDirFn = func(dir string) ([]os.DirEntry, error) {
		out := make([]os.DirEntry, candidates/directories)
		for j := range out {
			out[j] = syntheticDirEntry{name: fmt.Sprintf("f%03d.mkv", j)}
		}
		return out, nil
	}
	var n atomic.Int64
	eng.orderFactsFn = func(_ context.Context, p string, _ bool) sourceFacts {
		if n.Add(1)%10 == 0 {
			return sourceFacts{}
		}
		return facts(9_000, 1920, 1080, 3600)
	}

	var atFirstOffer string
	offered := 0
	eng.enumerateOrdered(eng.passListings(), sink{offer: func(string) bool {
		if offered == 0 {
			atFirstOffer = logs.String()
		}
		offered++
		return true
	}})
	if offered != candidates {
		t.Fatalf("offered %d, want %d", offered, candidates)
	}
	progress, done := 0, 0
	for _, rec := range records(t, bytes.NewBufferString(atFirstOffer)) {
		msg, _ := rec["msg"].(string)
		switch {
		case strings.HasPrefix(msg, "reading savings_per_hour ordering keys"):
			progress++
		case strings.HasPrefix(msg, "savings_per_hour ordering keys read"):
			done++
			if rec["keys_read"] != float64(2_250) || rec["keys_unread"] != float64(250) {
				t.Errorf("the completion record states %v read and %v unread, want 2250 and 250",
					rec["keys_read"], rec["keys_unread"])
			}
		}
	}
	if progress < candidates/1_000 {
		t.Errorf("%d progress records over %d candidates, want at least one per 1,000", progress, candidates)
	}
	if done != 1 {
		t.Errorf("%d completion records before the first offer, want exactly 1:\n%s", done, atFirstOffer)
	}
}

// ---- priority -----------------------------------------------------------------------

// priorityFixture is the synthetic library the priority cases order: two roots, a banded
// rule naming a priority, a root priority, two encode profiles naming one, and an
// unreadable file. It returns the library, an engine factory and the facts book.
func priorityFixture(t *testing.T) (string, func(order string) *Engine, *factsBook) {
	t.Helper()
	dir, roots := twoRoots(t, "films", "tv")
	films, tv := roots[0], roots[1]
	cfg := profileCfg(t, fmt.Sprintf(`
library_roots:
  - path: %s
    priority: 5
    rules:
      - when: {max_source_height: 576}
        crf: 26
        priority: 50
  - %s
encode_profiles:
  - name: anime
    match: "**/anime/**"
    priority: 20
  - name: kids
    match: "**/kids/**"
    priority: -10
min_bitrate_kbps: 0
vmaf_enable: false
`, films, tv))
	book := newFactsBook()
	for rel, f := range map[string]sourceFacts{
		"films/a-classic-sd.mkv":  facts(2_500, 720, 480, 5400), // rule 50
		"films/b-blockbuster.mkv": facts(21_600, 1920, 1080, 2700),
		"films/c-indie.mkv":       facts(6_100, 1920, 1080, 10800),
		"films/anime/d-film.mkv":  facts(8_000, 1920, 1080, 6000), // the rule does not match: profile 20
		"tv/anime/e-episode.mkv":  facts(4_000, 1280, 720, 1440),  // profile 20
		"tv/f-news.mkv":           facts(15_000, 1920, 1080, 1800),
		"tv/g-show.mkv":           facts(9_000, 1920, 1080, 2700),
		"tv/kids/h-cartoon.mkv":   facts(3_000, 720, 576, 1320), // profile -10
		"tv/i-broken.mkv":         {},                           // unreadable
	} {
		p := filepath.Join(dir, rel)
		mustWrite(t, p)
		book.facts[p] = f
	}
	build := func(order string) *Engine {
		c := cfg
		c.QueueOrder = order
		eng := toollessEngine(t, c, newTestStore(t, dir), discardLogger())
		eng.EnsureHoldBacks(context.Background())
		eng.orderFactsFn = book.probe
		return eng
	}
	return dir, build, book
}

// TestQueueOrder_FixtureQueueByPriorityThenSavingsPerHour orders the synthetic library under
// savings_per_hour and under path, asserting the FULL offered sequence of each: priority
// first (higher first), then the declared order, then the path - and the unreadable file
// last. Its log prints both sequences.
func TestQueueOrder_FixtureQueueByPriorityThenSavingsPerHour(t *testing.T) {
	dir, build, book := priorityFixture(t)
	for _, tc := range []struct {
		order  string
		want   []string
		probes int
	}{
		{config.QueueOrderSavingsPerHour, []string{
			"films/a-classic-sd.mkv",  // 50: its banded rule's
			"tv/anime/e-episode.mkv",  // 20: encode profile anime; 720p saves more per hour than d
			"films/anime/d-film.mkv",  // 20: encode profile anime, over its root's 5
			"films/b-blockbuster.mkv", // 5: the root's; 21.6 Mbps
			"films/c-indie.mkv",       // 5: the root's; 6.1 Mbps
			"tv/f-news.mkv",           // 0: 15 Mbps
			"tv/g-show.mkv",           // 0: 9 Mbps
			"tv/kids/h-cartoon.mkv",   // -10: encode profile kids
			"tv/i-broken.mkv",         // its key could not be read: last
		}, 9},
		{config.QueueOrderPath, []string{
			"films/a-classic-sd.mkv",  // 50
			"films/anime/d-film.mkv",  // 20, in traversal order
			"tv/anime/e-episode.mkv",  // 20
			"films/b-blockbuster.mkv", // 5, in traversal order
			"films/c-indie.mkv",       // 5
			"tv/f-news.mkv",           // 0, in traversal order; under path i-broken's key is its
			"tv/g-show.mkv",           //    position and its priority needs no probe, so it is
			"tv/i-broken.mkv",         //    readable and stays in traversal order
			"tv/kids/h-cartoon.mkv",   // -10
		}, 4},
	} {
		t.Run(tc.order, func(t *testing.T) {
			before := book.total()
			got := enumerated(t, build(tc.order), dir)
			t.Logf("queue_order %s with priorities, offered:\n  %s", tc.order, strings.Join(got, "\n  "))
			if !slices.Equal(got, tc.want) {
				t.Errorf("queue_order %s offered\n  %v\nwant\n  %v", tc.order, got, tc.want)
			}
			// One probe per candidate under savings_per_hour, which also carries the height
			// the banded rule's priority needs; under path, one per candidate of the root whose
			// rule priority bands on height (films, four files) and none for tv.
			if n := book.total() - before; n != tc.probes {
				t.Errorf("queue_order %s took %d ordering probes, want %d", tc.order, n, tc.probes)
			}
		})
	}
}

// TestQueueOrder_UnderPathWithNoPriorityNothingIsBufferedOrRead: with no priority anywhere,
// `path` is the enumeration's own stream - the ordering probes nothing and reads no
// attribute, under a configuration that has rules and encode profiles.
func TestQueueOrder_UnderPathWithNoPriorityNothingIsBufferedOrRead(t *testing.T) {
	dir, roots := twoRoots(t, "films")
	cfg := profileCfg(t, fmt.Sprintf(`
library_roots:
  - path: %s
    rules:
      - when: {max_source_height: 576}
        crf: 26
encode_profiles:
  - name: anime
    match: "**/anime/**"
    crf: 24
`, roots[0]))
	if cfg.PriorityConfigured() {
		t.Fatal("the fixture names a priority")
	}
	mustWrite(t, filepath.Join(dir, "films/b.mkv"))
	mustWrite(t, filepath.Join(dir, "films/a.mkv"))
	eng := toollessEngine(t, cfg, newTestStore(t, dir), discardLogger())
	book := newFactsBook()
	eng.orderFactsFn = book.probe
	var stats atomic.Int64
	countingStat(eng, &stats)
	if got := enumerated(t, eng, dir); !slices.Equal(got, []string{"films/a.mkv", "films/b.mkv"}) {
		t.Errorf("path offered %v", got)
	}
	if book.total() != 0 || stats.Load() != 0 {
		t.Errorf("path with no priority took %d probes and %d attribute reads for ordering, want 0 and 0",
			book.total(), stats.Load())
	}
}

// TestQueueOrder_AnUnreadablePriorityHeightGoesLast: under an order that needs no probe for
// its key, a banded rule's priority still needs the source height; a file whose height
// cannot be read is offered last with a warn naming that operation, never dropped.
func TestQueueOrder_AnUnreadablePriorityHeightGoesLast(t *testing.T) {
	dir, build, book := priorityFixture(t)
	broken := filepath.Join(dir, "films/b-blockbuster.mkv")
	book.facts[broken] = sourceFacts{}
	eng := build(config.QueueOrderLargest)
	logs := &bytes.Buffer{}
	eng.Log = slog.New(slog.NewJSONHandler(logs, nil))
	got := enumerated(t, eng, dir)
	if got[len(got)-1] != "films/b-blockbuster.mkv" || len(got) != 9 {
		t.Errorf("largest offered %v, want films/b-blockbuster.mkv last and all nine offered", got)
	}
	found := false
	for _, rec := range records(t, logs) {
		if rec["level"] == "WARN" && rec["file"] == broken {
			found = strings.Contains(fmt.Sprint(rec["operation"]), "source height")
		}
	}
	if !found {
		t.Errorf("no warn names %s and the source-height operation:\n%s", broken, logs.String())
	}
	for p, n := range book.calls {
		if n > 1 {
			t.Errorf("%s was probed %d times in one pass", p, n)
		}
	}
	for p, wb := range book.bitrateOf {
		if wb {
			t.Errorf("%s was probed for its bitrate under largest, where only the height is read", p)
		}
	}
}

// TestQueueOrder_PriorityIsInNoDecisionInput is the property that lets a priority be edited
// on a half-done library: the decision inputs a terminal row records, for every file and
// height, and every root's digest are identical with and without the priorities.
func TestQueueOrder_PriorityIsInNoDecisionInput(t *testing.T) {
	const body = `
library_roots:
  - path: /mnt/films%s
    rules:
      - when: {max_source_height: 576}
        crf: 26%s
encode_profiles:
  - name: anime
    match: "**/anime/**"
    crf: 24%s
`
	without := profileCfg(t, fmt.Sprintf(body, "", "", ""))
	with := profileCfg(t, fmt.Sprintf(body, "\n    priority: 5", "\n        priority: 50", "\n    priority: 20"))
	if !with.PriorityConfigured() || without.PriorityConfigured() {
		t.Fatal("the two fixtures do not differ by their priorities")
	}
	a, b := DecisionInputsPerPath(without), DecisionInputsPerPath(with)
	for _, p := range []string{"/mnt/films/x.mkv", "/mnt/films/anime/y.mkv"} {
		for _, h := range []int{480, 1080} {
			ia, _, _ := a(p, &h)
			ib, _, _ := b(p, &h)
			if !reflect.DeepEqual(ia, ib) {
				t.Errorf("%s at %d: decision inputs\n  %v\nwithout priorities,\n  %v\nwith: a priority would re-open rows",
					p, h, ia, ib)
			}
		}
	}
	if !reflect.DeepEqual(DecisionInputsFor(without), DecisionInputsFor(with)) {
		t.Error("the top-level decision inputs differ with and without priorities")
	}
	if without.RootProfiles()[0].Profile.Digest() != with.RootProfiles()[0].Profile.Digest() {
		t.Error("the root's profile digest moved with a priority")
	}
}
