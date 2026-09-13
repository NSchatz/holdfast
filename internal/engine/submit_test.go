package engine

// The targeted-submission suite (S0093). What it has to establish is narrow and it is all
// about EQUIVALENCE: a file submitted by name reaches the same guards, the same claim and
// the same recorded verdict a whole-library scan would have reached on it. A submission
// route that quietly got its own answer to any of those questions would be a
// network-reachable path into a pipeline that ends in the deletion of an original.
//
// So every case here compares the submission against the scan rather than against a value
// written into the test, wherever the comparison is possible at all.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// ---- harness -----------------------------------------------------------------

// submitDeadline bounds the wait for a drained queue. It is a wall clock on a test that
// would otherwise hang, never a gate: nothing here passes because it expired.
const submitDeadline = 5 * time.Minute

// engineOver builds an Engine over an EXISTING store, which the shared helpers do not do.
// The re-opening cases need two configurations answering over one ledger, which is the
// whole shape of "the configuration moved under a row that was already there".
func engineOver(t *testing.T, ffmpeg, ffprobe, root string, ts *testStore, enc Encoder, mutate func(*config.Config)) *Engine {
	t.Helper()
	cfg := baseCfg(root)
	if mutate != nil {
		mutate(&cfg)
	}
	prober := probe.New(ffmpeg, ffprobe)
	if enc == nil {
		enc = FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}
	}
	return New(cfg, prober, enc, ts, discardLogger())
}

// submit judges every path, enqueues the ones that pass, drains the queue until each has
// been processed, then stops the pool. It fails the test on a path the eligibility
// decision refuses: a case that means to exercise a refusal asserts on Judge directly.
func submit(t *testing.T, eng *Engine, paths ...string) *Submissions {
	t.Helper()
	subs := eng.NewSubmissions(len(paths), 64)
	for _, p := range paths {
		resolved, rule, detail, ok := subs.Judge(p)
		if !ok {
			t.Fatalf("Judge(%s) refused an eligible path: %s - %s", p, rule, detail)
		}
		if !subs.Offer(resolved) {
			t.Fatalf("Offer(%s): the queue would not take it", resolved)
		}
	}
	drain(t, subs, len(paths))
	return subs
}

// drain runs the pool until want submissions have been processed, then stops it and joins.
func drain(t *testing.T, subs *Submissions, want int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); subs.Run(ctx) }()
	deadline := time.Now().Add(submitDeadline)
	for len(subs.Results()) < want {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("only %d of %d submissions were processed inside %s", len(subs.Results()), want, submitDeadline)
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-done
}

// terminalRowFor returns the terminal row recorded for an exact path, and whether there is
// one. It is keyed on the PATH rather than on a fingerprint, because the point of most of
// these cases is whether a row moved at all - and it reports an ABSENCE rather than
// failing on one, which the package's existing rowFor cannot do.
func terminalRowFor(t *testing.T, ts *testStore, path string) (store.Job, bool) {
	t.Helper()
	rows, err := ts.List(context.Background(), []store.Status{
		store.Done, store.Skipped, store.Failed, store.WouldTranscode,
		store.Indeterminate, store.AppliedDespiteError,
	}, 0)
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	for _, r := range rows {
		if r.Path == path {
			return r, true
		}
	}
	return store.Job{}, false
}

// gatedEncoder counts encodes and can hold one open, which is what makes "at most once
// concurrently" and "nothing was encoded" assertable rather than inferred.
type gatedEncoder struct {
	inner   Encoder
	n       atomic.Int64
	started chan string
	release chan struct{}
}

// Encode holds the encode open until the test releases it. It deliberately does NOT watch
// ctx: the whole point of the shutdown case is that work already in flight when the
// cancellation lands is carried to completion rather than abandoned, and a gate that
// unblocked on the cancellation would abandon it for the code under test. The wall clock
// is a backstop against a case that fails before releasing, never a gate.
func (c *gatedEncoder) Encode(ctx context.Context, in, out string, props *probe.VideoProps) error {
	c.n.Add(1)
	if c.started != nil {
		select {
		case c.started <- in:
		default:
		}
	}
	if c.release != nil {
		select {
		case <-c.release:
		case <-time.After(submitDeadline):
		}
	}
	return c.inner.Encode(ctx, in, out, props)
}

func gatedFFmpeg(ffmpeg string, cfg config.Config, prober *probe.Prober) *gatedEncoder {
	return &gatedEncoder{inner: FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}}
}

// errSubmitFixtureEncode is the encode failure the max_failures case drives. It is a
// fixture, not a condition under test: what the case is about is the failure COUNT.
var errSubmitFixtureEncode = errors.New("simulated: the encoder failed")

// ---- AC4a: ONE eligibility decision ------------------------------------------

// The rules that decide whether a file is eligible for the pipeline are ONE decision, read
// by the scan's enumeration and by a targeted submission alike. A video extension added, a
// working-file name form added or a library root added must reach both routes without a
// second edit to a parallel copy - and the only way to establish that mechanically is to
// exercise the decision through BOTH routes and require them to agree, file by file, under
// more than one configuration.
//
// It is not a test about a helper function. It walks a real tree with the real enumeration
// and asks the real submission decision about every path in it, including the ones neither
// route should take.
func TestEligibilityIsOneDecisionForScanAndSubmission(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	// Every file in the tree, and every file just outside it. Nothing here is a real
	// video: enumeration decides from the name and the submission decision resolves and
	// stats, so neither opens one.
	inTree := []string{
		"film.mkv",                               // an ordinary source
		"clip.MKV",                               // the extension test is case-insensitive
		"episode.mp4",                            // eligible only under the wider video_exts
		"notes.txt",                              // no configured video extension
		"noext",                                  // no extension at all
		"film.__transcoding__.mkv",               // a work-in-progress temp holdfast wrote
		"film.0.__transcoding__.mkv",             // the n-suffixed temp form
		"film.__holdfast-replacement__.mkv",      // a replacement holdfast retained
		"film.abc123.__undo__.mkv",               // a retained original, by name
		"sub/nested.mkv",                         // a source one directory down
		"sub/.holdfast-undo/x.dead.__undo__.mkv", // the retention area's own file
		"sub/.holdfast-undo/plain.mkv",           // an ordinary name INSIDE the retention area
	}
	for _, rel := range inTree {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "elsewhere.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Two configurations that differ in ONE rule. If the two routes read one decision,
	// widening video_exts moves episode.mp4 in BOTH; if either route held its own copy,
	// they would disagree here.
	for _, tc := range []struct {
		name string
		exts []string
	}{
		{"video_exts: mkv", []string{"mkv"}},
		{"video_exts: mkv, mp4", []string{"mkv", "mp4"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseCfg(root)
			cfg.VideoExts = tc.exts
			eng := New(cfg, probe.New("", ""), EncoderFunc(nil), newTestStore(t, root), discardLogger())

			enumerated, _ := eng.enumerate()
			sort.Strings(enumerated)

			// Ask the submission decision about every path the tree holds, plus the one
			// outside it, and collect the ones it accepts.
			var accepted []string
			el := eng.Eligibility()
			for _, rel := range inTree {
				p := filepath.Join(root, rel)
				if resolved, bad := el.Judge(p); bad == nil {
					accepted = append(accepted, resolved)
				}
			}
			if _, bad := el.Judge(filepath.Join(outside, "elsewhere.mkv")); bad == nil {
				accepted = append(accepted, "A PATH OUTSIDE EVERY ROOT WAS ACCEPTED")
			}
			sort.Strings(accepted)

			if strings.Join(enumerated, "\n") != strings.Join(accepted, "\n") {
				t.Fatalf("the scan and a targeted submission disagree about what is eligible.\n"+
					"the scan enumerates:\n  %s\na submission accepts:\n  %s",
					strings.Join(enumerated, "\n  "), strings.Join(accepted, "\n  "))
			}

			// Anti-vacuity, both ways: two sets that agree because both are empty, or
			// because both are everything, would establish nothing at all.
			if len(enumerated) == 0 {
				t.Fatal("both routes took nothing, so their agreement is vacuous")
			}
			if len(enumerated) == len(inTree) {
				t.Fatal("both routes took every file in the tree, including the working files - " +
					"their agreement says nothing about the rules")
			}
		})
	}

	// And the rule really did move: mp4 is in one configuration's answer and not the
	// other's. Without this the two subtests could both be reading a frozen list.
	cfgNarrow, cfgWide := baseCfg(root), baseCfg(root)
	cfgNarrow.VideoExts, cfgWide.VideoExts = []string{"mkv"}, []string{"mkv", "mp4"}
	mp4 := filepath.Join(root, "episode.mp4")
	if _, bad := NewEligibility(cfgNarrow).Judge(mp4); bad == nil {
		t.Fatal("episode.mp4 was accepted under video_exts: [mkv], so widening the rule proves nothing")
	}
	if _, bad := NewEligibility(cfgWide).Judge(mp4); bad != nil {
		t.Fatalf("episode.mp4 was refused under video_exts: [mkv mp4]: %v", bad)
	}
}

// ---- AC4: no weaker gate than a scan -----------------------------------------

// A submitted file records the SAME terminal status and the SAME reason a whole-library
// scan records for it under the same configuration. The comparison is made against a scan
// that really ran, over an identical tree, rather than against statuses written into the
// test: a change that moved both routes together would still be caught by the criteria
// that pin individual verdicts, and a change that moved only one is caught here.
func TestTargetedSubmissionReachesTheSameVerdictAsAScan(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	// Three fixtures reaching three different verdicts, so the comparison is not about
	// one code path: a skip at a guard, a failure with no readable video stream, and a
	// real encode through the gates to a swap.
	fixtures := func(root string) {
		mkHevc(t, ffmpeg, filepath.Join(root, "already.mkv"), "2M")
		if err := os.WriteFile(filepath.Join(root, "broken.mkv"), []byte("not a video"), 0o644); err != nil {
			t.Fatal(err)
		}
		mkH264(t, ffmpeg, filepath.Join(root, "good.mkv"), "8M")
	}

	scanned := t.TempDir()
	fixtures(scanned)
	scanStore := run(t, ffmpeg, ffprobe, scanned, nil, nil)

	submitted := t.TempDir()
	fixtures(submitted)
	subStore := newTestStore(t, submitted)
	eng := engineOver(t, ffmpeg, ffprobe, submitted, subStore, nil, nil)
	names := []string{"already.mkv", "broken.mkv", "good.mkv"}
	paths := make([]string, 0, len(names))
	for _, n := range names {
		paths = append(paths, filepath.Join(submitted, n))
	}
	submit(t, eng, paths...)

	for _, n := range names {
		want, ok := terminalRowFor(t, scanStore, filepath.Join(scanned, n))
		if !ok {
			t.Fatalf("the SCAN recorded no terminal row for %s, so there is nothing to compare against", n)
		}
		got, ok := terminalRowFor(t, subStore, filepath.Join(submitted, n))
		if !ok {
			t.Fatalf("the SUBMISSION recorded no terminal row for %s; a scan recorded %s/%q",
				n, want.Status, want.Outcome.Reason)
		}
		if got.Status != want.Status || got.Outcome.Reason != want.Outcome.Reason {
			t.Errorf("%s: a submission recorded %s/%q; the scan recorded %s/%q. A submitted file must "+
				"reach the same verdict a scan reaches",
				n, got.Status, got.Outcome.Reason, want.Status, want.Outcome.Reason)
		}
		// The recorded decision inputs are part of the verdict: they are what decides
		// whether the next configuration change re-opens this row, so a submission that
		// recorded a different set would leave the file answerable by a different future.
		if strings.Join(got.Outcome.DecisionInputs.Keys(), ",") != strings.Join(want.Outcome.DecisionInputs.Keys(), ",") {
			t.Errorf("%s: a submission recorded decision inputs %v; the scan recorded %v",
				n, got.Outcome.DecisionInputs.Keys(), want.Outcome.DecisionInputs.Keys())
		}
	}

	// Anti-vacuity: the three fixtures really did reach three different verdicts, so the
	// comparison above is over a spread and not over one repeated answer.
	seen := map[store.Status]bool{}
	for _, n := range names {
		r, _ := terminalRowFor(t, scanStore, filepath.Join(scanned, n))
		seen[r.Status] = true
	}
	if len(seen) < 3 {
		t.Fatalf("the fixtures reached %d distinct verdicts, not 3: %v", len(seen), seen)
	}
}

// A submitted file the pipeline holds back stays held back. The hold-back is a property of
// the RECORD (a parked job's two recorded paths, and any recorded replacement path still
// excluded), so it is invisible to the eligibility decision and can only be honoured by
// going through the same door a scan's worker goes through.
func TestTargetedSubmissionRespectsTheHoldBacks(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	parked := filepath.Join(root, "parked.mkv")
	replacement := filepath.Join(root, "replacement.mkv")
	ordinary := filepath.Join(root, "ordinary.mkv")
	mkH264(t, ffmpeg, parked, "8M")
	mkH264(t, ffmpeg, replacement, "8M")
	mkH264(t, ffmpeg, ordinary, "8M")

	ts := newTestStore(t, root)
	ctx := context.Background()
	// A parked job naming both of its files. This is the record-based hold-back, and it
	// is what withholds the two paths from the sweep, the scan and the workers alike.
	if err := ts.RecordSwapIncident(ctx, store.SwapIncident{
		SourcePath:        parked,
		SourceFingerprint: probe.Fingerprint(parked),
		ReplacementPath:   replacement,
		Outcome:           store.Indeterminate,
		SwapError:         "simulated: the swap outcome could not be established",
	}); err != nil {
		t.Fatalf("RecordSwapIncident: %v", err)
	}

	cfg := baseCfg(root)
	prober := probe.New(ffmpeg, ffprobe)
	enc := gatedFFmpeg(ffmpeg, cfg, prober)
	eng := New(cfg, prober, enc, ts, discardLogger())

	subs := submit(t, eng, parked, replacement, ordinary)

	for _, p := range []string{parked, replacement} {
		if row, ok := terminalRowFor(t, ts, p); ok && row.Status != store.Indeterminate {
			t.Errorf("a held-back path %s was given a terminal row (%s/%q); a submission must leave it "+
				"exactly as a scan leaves it", p, row.Status, row.Outcome.Reason)
		}
		for _, r := range subs.Results() {
			if r.Path == p && r.Claimed {
				t.Errorf("a held-back path %s got past the pipeline's door", p)
			}
		}
	}
	// Anti-vacuity: the SAME submission, in the same queue, did process the file nothing
	// holds back - so "nothing happened" is a finding about the hold-backs and not about a
	// queue that never ran.
	if _, ok := terminalRowFor(t, ts, ordinary); !ok {
		t.Fatal("the ordinary file was not processed either, so this proves nothing about hold-backs")
	}
	if enc.n.Load() != 1 {
		t.Errorf("the encoder ran %d times; only the file nothing holds back should have reached it", enc.n.Load())
	}
}

// The same file submitted twice, and submitted while a scan is already working on it, is
// processed at most once concurrently and never produces a second encode of one source.
// store.Claim is what makes that true - it is the cross-worker mutual-exclusion guard -
// and this establishes that the submission route inherits it rather than routing around it.
func TestTargetedSubmissionIsMutuallyExclusiveWithAScan(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	t.Run("the same file submitted twice in quick succession", func(t *testing.T) {
		root := t.TempDir()
		film := filepath.Join(root, "film.mkv")
		mkH264(t, ffmpeg, film, "8M")

		ts := newTestStore(t, root)
		cfg := baseCfg(root)
		prober := probe.New(ffmpeg, ffprobe)
		enc := gatedFFmpeg(ffmpeg, cfg, prober)
		enc.started, enc.release = make(chan string, 4), make(chan struct{})
		eng := New(cfg, prober, enc, ts, discardLogger())

		// Two workers and the same path twice, so both are in the pool at once.
		subs := eng.NewSubmissions(2, 8)
		for i := 0; i < 2; i++ {
			if !subs.Offer(film) {
				t.Fatal("the queue would not take the path")
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); subs.Run(ctx) }()

		// Hold the first encode open, so the second submission meets a claim that is
		// live rather than one that has already been released.
		select {
		case <-enc.started:
		case <-time.After(submitDeadline):
			cancel()
			<-done
			t.Fatal("no encode started")
		}
		close(enc.release)
		for len(subs.Results()) < 2 {
			time.Sleep(2 * time.Millisecond)
		}
		cancel()
		<-done

		if n := enc.n.Load(); n != 1 {
			t.Errorf("the encoder ran %d times for one source; a second encode of the same source "+
				"is exactly what the claim exists to prevent", n)
		}
		claims := 0
		for _, r := range subs.Results() {
			if r.Claimed {
				claims++
			}
		}
		if claims != 1 {
			t.Errorf("%d of 2 submissions got past the door; exactly one may", claims)
		}
	})

	t.Run("submitted while a scan is already working on it", func(t *testing.T) {
		root := t.TempDir()
		film := filepath.Join(root, "film.mkv")
		mkH264(t, ffmpeg, film, "8M")

		ts := newTestStore(t, root)
		cfg := baseCfg(root)
		prober := probe.New(ffmpeg, ffprobe)
		enc := gatedFFmpeg(ffmpeg, cfg, prober)
		enc.started, enc.release = make(chan string, 4), make(chan struct{})
		eng := New(cfg, prober, enc, ts, discardLogger())

		// The SCAN goes first and is held inside the encoder, so its claim is live.
		scanCtx, scanCancel := context.WithCancel(context.Background())
		defer scanCancel()
		scanDone := make(chan struct{})
		go func() { defer close(scanDone); _ = eng.RunOneshot(scanCtx) }()
		select {
		case <-enc.started:
		case <-time.After(submitDeadline):
			t.Fatal("the scan never reached the encoder")
		}

		// Now submit the very file the scan is encoding.
		subs := eng.NewSubmissions(1, 8)
		if !subs.Offer(film) {
			t.Fatal("the queue would not take the path")
		}
		drain(t, subs, 1)

		close(enc.release)
		<-scanDone

		if n := enc.n.Load(); n != 1 {
			t.Errorf("the encoder ran %d times; a submission must not produce a second encode of a "+
				"source a scan is already encoding", n)
		}
		if subs.Results()[0].Claimed {
			t.Error("the submission got past the door while a scan held the claim")
		}
	})
}

// ---- AC5 / AC5a: terminal rows, and not a requeue -----------------------------

// A done or skipped row whose recorded decision inputs STILL match the configuration in
// force holds its file out, and a submission leaves it exactly where it is. The submission
// reports that it reached such a row, which is the only place that fact can be reported:
// the endpoint answered before the file was ever looked at.
func TestTargetedSubmissionHonoursATerminalRowThatStillHolds(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	film := filepath.Join(root, "already.mkv")
	mkHevc(t, ffmpeg, film, "2M")

	ts := newTestStore(t, root)
	cfg := baseCfg(root)
	prober := probe.New(ffmpeg, ffprobe)
	enc := gatedFFmpeg(ffmpeg, cfg, prober)
	eng := New(cfg, prober, enc, ts, discardLogger())

	// A scan first, so the row is one the pipeline itself wrote under this configuration.
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	before, ok := terminalRowFor(t, ts, film)
	if !ok || before.Status != store.Skipped {
		t.Fatalf("the scan left %s at %s, not skipped; there is no terminal row to honour", film, before.Status)
	}

	subs := submit(t, eng, film)

	after, _ := terminalRowFor(t, ts, film)
	if after.Status != before.Status || after.Outcome.Reason != before.Outcome.Reason ||
		after.UpdatedAt != before.UpdatedAt {
		t.Errorf("the submission moved a row that still holds: %s/%q at %d became %s/%q at %d",
			before.Status, before.Outcome.Reason, before.UpdatedAt,
			after.Status, after.Outcome.Reason, after.UpdatedAt)
	}
	if enc.n.Load() != 0 {
		t.Errorf("the encoder ran %d times on a file a terminal row holds out", enc.n.Load())
	}
	r := subs.Results()[0]
	if !r.HeldByTerminalRow() {
		t.Errorf("the submission did not report reaching a row that holds the file out "+
			"(before=%q claimed=%v)", r.Before, r.Claimed)
	}
}

// A terminal row whose recorded decision inputs no longer match is offered to the pipeline
// exactly as a file nobody has ever seen. The re-opening rule lives in store.Claim and the
// submission must INHERIT it - so this moves a real configuration key under a real row and
// requires the submission to notice, with no code in the submission path that knows what a
// decision input is.
func TestTargetedSubmissionReopensARowWhoseInputsMoved(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	film := filepath.Join(root, "film.mkv")
	mkH264(t, ffmpeg, film, "8M")

	ts := newTestStore(t, root)
	// A bitrate floor above the fixture skips it, and records min_bitrate_kbps as the
	// input that decided it.
	high := engineOver(t, ffmpeg, ffprobe, root, ts, nil, func(c *config.Config) { c.MinBitrateKbps = 100000 })
	if err := high.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	before, ok := terminalRowFor(t, ts, film)
	if !ok || before.Status != store.Skipped || before.Outcome.Reason != SkipLowBitrate {
		t.Fatalf("the scan left %s at %s/%q, not a low-bitrate skip", film, before.Status, before.Outcome.Reason)
	}

	// The floor moves. The row now records a value the configuration no longer holds.
	low := engineOver(t, ffmpeg, ffprobe, root, ts, nil, func(c *config.Config) { c.MinBitrateKbps = 0 })
	subs := submit(t, low, film)

	if !subs.Results()[0].Claimed {
		t.Fatal("the submission did not get past the door, so the row was not re-opened")
	}
	after, _ := terminalRowFor(t, ts, film)
	if after.Status == store.Skipped && after.Outcome.Reason == SkipLowBitrate {
		t.Errorf("the row still reads %s/%q: the file was never re-offered to the guards",
			after.Status, after.Outcome.Reason)
	}
}

// Three rows are never re-opened however far the configuration has moved, because what
// holds each of them out is not a configuration question: indeterminate,
// applied-despite-error, and a skip recording the restored-original guard. A submission
// leaves all three held out.
//
// Each is seeded recording NO decision inputs, which is the condition that re-opens ANY
// other terminal row - so a submission that got past the door here would have got past it
// because the rule was bypassed, not because the row was re-derivable.
func TestTargetedSubmissionLeavesTheNeverReopenedRowsAlone(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	seed := func(t *testing.T, ts *testStore, path string, status store.Status, reason string) {
		t.Helper()
		ctx := context.Background()
		fp := probe.Fingerprint(path)
		ok, err := ts.Claim(ctx, path, fp, "seed", 3, store.DecisionInputs{})
		if err != nil || !ok {
			t.Fatalf("seeding Claim(%s): ok=%v err=%v", path, ok, err)
		}
		if err := ts.Finish(ctx, path, fp, status, &store.Outcome{Reason: reason}, 3); err != nil {
			t.Fatalf("seeding Finish(%s): %v", path, err)
		}
	}

	for _, tc := range []struct {
		name     string
		status   store.Status
		reason   string
		reopened bool
	}{
		{"indeterminate", store.Indeterminate, "simulated: outcome could not be established", false},
		{"applied-despite-error", store.AppliedDespiteError, "simulated: the rename took effect", false},
		{"restored-original", store.Skipped, store.GuardRestoredOriginal, false},
		// The control. Same seeding, same absent decision inputs, an ordinary guard: this
		// one MUST be re-offered, or the three above prove nothing.
		{"an ordinary skip (the control)", store.Skipped, SkipLowBitrate, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			film := filepath.Join(dir, "film.mkv")
			mkH264(t, ffmpeg, film, "8M")
			ts := newTestStore(t, dir)
			seed(t, ts, film, tc.status, tc.reason)
			before, _ := terminalRowFor(t, ts, film)

			cfg := baseCfg(dir)
			prober := probe.New(ffmpeg, ffprobe)
			enc := gatedFFmpeg(ffmpeg, cfg, prober)
			eng := New(cfg, prober, enc, ts, discardLogger())
			subs := submit(t, eng, film)

			claimed := subs.Results()[0].Claimed
			if claimed != tc.reopened {
				t.Fatalf("the submission %s a %s row; want %s",
					claimedWord(claimed), tc.status, claimedWord(tc.reopened))
			}
			if tc.reopened {
				return
			}
			after, _ := terminalRowFor(t, ts, film)
			if after.Status != before.Status || after.Outcome.Reason != before.Outcome.Reason {
				t.Errorf("a row that is never re-opened moved: %s/%q became %s/%q",
					before.Status, before.Outcome.Reason, after.Status, after.Outcome.Reason)
			}
			if enc.n.Load() != 0 {
				t.Errorf("the encoder ran %d times on a file one of the three never-re-opened rows holds out",
					enc.n.Load())
			}
		})
	}
}

func claimedWord(b bool) string {
	if b {
		return "offered to the pipeline"
	}
	return "left held out"
}

// A submission is NOT a requeue. `holdfast requeue` is a local command by ratified
// operator decision precisely because it clears what a terminal row recorded about the
// configuration its decision was taken under, and resets the failure count that parks a
// row at max_failures. Putting either of those on a network-reachable endpoint would be
// reversing that decision by accident, so this pins that it did not happen.
func TestTargetedSubmissionIsNotARequeue(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	t.Run("a row parked at max_failures stays parked", func(t *testing.T) {
		root := t.TempDir()
		film := filepath.Join(root, "film.mkv")
		mkH264(t, ffmpeg, film, "8M")
		ts := newTestStore(t, root)

		// Fail it up to the bound, exactly as repeated scans would.
		cfg := baseCfg(root)
		cfg.MaxFailures = 2
		prober := probe.New(ffmpeg, ffprobe)
		broken := EncoderFunc(func(context.Context, string, string, *probe.VideoProps) error {
			return errSubmitFixtureEncode
		})
		eng := New(cfg, prober, broken, ts, discardLogger())
		for i := 0; i < cfg.MaxFailures; i++ {
			if err := eng.RunOneshot(context.Background()); err != nil {
				t.Fatalf("RunOneshot: %v", err)
			}
		}
		if got := failCount(t, ts, "film.mkv"); got != cfg.MaxFailures {
			t.Fatalf("fail_count is %d, not the %d that parks the row", got, cfg.MaxFailures)
		}
		before, _ := terminalRowFor(t, ts, film)

		enc := gatedFFmpeg(ffmpeg, cfg, prober)
		good := New(cfg, prober, enc, ts, discardLogger())
		subs := submit(t, good, film)

		if subs.Results()[0].Claimed {
			t.Error("a submission re-opened a row parked at max_failures; that is what `holdfast requeue " +
				"--failed` is for, and it is a LOCAL command by ratified operator decision")
		}
		if got := failCount(t, ts, "film.mkv"); got != cfg.MaxFailures {
			t.Errorf("the submission moved the failure count from %d to %d", cfg.MaxFailures, got)
		}
		after, _ := terminalRowFor(t, ts, film)
		if after.Status != before.Status {
			t.Errorf("a parked row moved from %s to %s", before.Status, after.Status)
		}
		if enc.n.Load() != 0 {
			t.Errorf("the encoder ran %d times on a parked row", enc.n.Load())
		}
	})

	t.Run("what a terminal row recorded is not cleared", func(t *testing.T) {
		root := t.TempDir()
		film := filepath.Join(root, "film.mkv")
		mkH264(t, ffmpeg, film, "8M")
		ts := newTestStore(t, root)

		eng := engineOver(t, ffmpeg, ffprobe, root, ts, nil, func(c *config.Config) { c.MinBitrateKbps = 100000 })
		if err := eng.RunOneshot(context.Background()); err != nil {
			t.Fatalf("RunOneshot: %v", err)
		}
		before, ok := terminalRowFor(t, ts, film)
		if !ok || !before.Outcome.DecisionInputs.Recorded() || len(before.Outcome.DecisionInputs.Keys()) == 0 {
			t.Fatalf("the scan recorded no decision inputs on %s, so there is nothing to preserve", film)
		}

		submit(t, eng, film)

		after, _ := terminalRowFor(t, ts, film)
		if !after.Outcome.DecisionInputs.Recorded() {
			t.Fatal("the submission CLEARED what the row recorded about the configuration its decision " +
				"was taken under - that is a requeue, and requeue is a local command")
		}
		if strings.Join(after.Outcome.DecisionInputs.Keys(), ",") != strings.Join(before.Outcome.DecisionInputs.Keys(), ",") {
			t.Errorf("the recorded decision inputs moved from %v to %v",
				before.Outcome.DecisionInputs.Keys(), after.Outcome.DecisionInputs.Keys())
		}
		for _, k := range before.Outcome.DecisionInputs.Keys() {
			wantV, _ := before.Outcome.DecisionInputs.Value(k)
			gotV, _ := after.Outcome.DecisionInputs.Value(k)
			if gotV != wantV {
				t.Errorf("the recorded value of %q moved from %q to %q", k, wantV, gotV)
			}
		}
	})
}

// ---- the queue's own shutdown contract ---------------------------------------

// The engine half of the shutdown criterion the server surface grades: work already in
// flight is joined, and submissions that were never started are dropped without a ledger
// row - nothing looked at them, so a row for one would be a record of a decision nobody
// took.
func TestSubmissionShutdownJoinsInFlightWorkAndDropsTheRest(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	var paths []string
	for _, n := range []string{"a.mkv", "b.mkv", "c.mkv"} {
		p := filepath.Join(root, n)
		mkH264(t, ffmpeg, p, "8M")
		paths = append(paths, p)
	}

	ts := newTestStore(t, root)
	cfg := baseCfg(root)
	prober := probe.New(ffmpeg, ffprobe)
	enc := gatedFFmpeg(ffmpeg, cfg, prober)
	enc.started, enc.release = make(chan string, 4), make(chan struct{})
	eng := New(cfg, prober, enc, ts, discardLogger())

	subs := eng.NewSubmissions(1, 8) // one worker, so the other two must wait
	for _, p := range paths {
		if !subs.Offer(p) {
			t.Fatal("the queue would not take a path")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { defer close(runDone); subs.Run(ctx) }()

	var inFlight string
	select {
	case inFlight = <-enc.started:
	case <-time.After(submitDeadline):
		cancel()
		<-runDone
		t.Fatal("no submission reached the encoder")
	}

	// Shutdown lands with one file mid-encode and two still queued.
	cancel()
	waitDone := make(chan struct{})
	var once sync.Once
	go func() { subs.Wait(); once.Do(func() { close(waitDone) }) }()
	select {
	case <-waitDone:
		t.Fatal("Wait returned while a submission was still in flight; a caller would then close the " +
			"store handle out from under it")
	case <-time.After(200 * time.Millisecond):
	}

	close(enc.release)
	select {
	case <-waitDone:
	case <-time.After(submitDeadline):
		t.Fatal("Wait never returned after the in-flight work finished")
	}
	<-runDone

	if got := len(subs.Results()); got != 1 {
		t.Fatalf("%d submissions were processed; only the one already in flight should have been", got)
	}
	if subs.Results()[0].Path != inFlight {
		t.Fatalf("the processed submission was %s, not the one in flight (%s)", subs.Results()[0].Path, inFlight)
	}
	for _, p := range paths {
		if p == inFlight {
			continue
		}
		if row, ok := terminalRowFor(t, ts, p); ok {
			t.Errorf("a submission that was never started carries a ledger row (%s/%q): nothing looked at "+
				"that file, so there is no decision to record", row.Status, row.Outcome.Reason)
		}
	}
}
