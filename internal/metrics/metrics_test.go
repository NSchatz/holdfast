package metrics

import (
	"context"
	"errors"
	"go/ast"
	"go/build"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/NSchatz/holdfast/internal/corpus"
	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/store"
)

func openStore(t *testing.T) *store.SQLite {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("/metrics status %d", rec.Code)
	}
	b, _ := io.ReadAll(rec.Body)
	return string(b)
}

// doneEvent builds the Done event the engine emits (TRANSCODE-13): the reclaimed bytes
// and the encode duration are now read off the event's Outcome — the same value handed
// to the store — rather than from fields duplicated onto the event.
func doneEvent(reclaimed int64, encode time.Duration, vmaf float64) engine.Event {
	src, out := reclaimed+1000, int64(1000)
	ms := encode.Milliseconds()
	return engine.Event{
		Status: store.Done,
		Outcome: &store.Outcome{
			SourceBytes: &src, OutputBytes: &out,
			EncodeMs: &ms, VmafMean: &vmaf, VmafModel: "version=vmaf_v0.6.1",
		},
	}
}

func TestMetrics_CountersAndHistogramsFromEvents(t *testing.T) {
	m := New(openStore(t), nil)

	m.Observe(doneEvent(1000, 2*time.Second, 96))
	m.Observe(doneEvent(500, 1*time.Second, 98))
	m.Observe(engine.Event{Status: store.Skipped})
	m.Observe(engine.Event{Status: store.Failed})
	m.Observe(engine.Event{Status: store.Encoding}) // non-terminal — must NOT be counted

	if got := testutil.ToFloat64(m.filesTotal.WithLabelValues("done")); got != 2 {
		t.Errorf("files_total{done} = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.filesTotal.WithLabelValues("skipped")); got != 1 {
		t.Errorf("files_total{skipped} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.filesTotal.WithLabelValues("failed")); got != 1 {
		t.Errorf("files_total{failed} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.bytesReclaimed); got != 1500 {
		t.Errorf("bytes_reclaimed_total = %v, want 1500", got)
	}

	// Histograms: assert the sample counts via the scrape body.
	body := scrape(t, m)
	for _, want := range []string{
		`holdfast_files_total{outcome="done"} 2`,
		`holdfast_files_total{outcome="failed"} 1`,
		`holdfast_bytes_reclaimed_total 1500`,
		`holdfast_encode_duration_seconds_count 2`,
		`holdfast_vmaf_score_count 2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics body missing %q", want)
		}
	}
}

func TestMetrics_QueueDepthReadsStore(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	// Seed: two pending-claimed (encoding) + one done.
	for _, p := range []string{"/lib/a.mkv", "/lib/b.mkv"} {
		if ok, err := st.Claim(ctx, p, "1:1", "w0", 3, store.DecisionInputs{}); err != nil || !ok {
			t.Fatalf("claim %s: %v", p, err)
		}
		if err := st.Advance(ctx, p, "1:1", store.Encoding); err != nil {
			t.Fatal(err)
		}
	}
	if ok, _ := st.Claim(ctx, "/lib/c.mkv", "2:2", "w0", 3, store.DecisionInputs{}); ok {
		_ = st.Finish(ctx, "/lib/c.mkv", "2:2", store.Done, nil, 3)
	}

	m := New(st, nil)
	body := scrape(t, m)
	if !strings.Contains(body, `holdfast_queue_depth{state="encoding"} 2`) {
		t.Errorf("queue_depth encoding gauge wrong; body:\n%s", body)
	}
	if !strings.Contains(body, `holdfast_queue_depth{state="done"} 1`) {
		t.Errorf("queue_depth done gauge wrong; body:\n%s", body)
	}
}

func TestMetrics_PrecreatedSeriesReadZero(t *testing.T) {
	m := New(openStore(t), nil)
	// Before any event the outcome series should already exist at 0 (not absent).
	body := scrape(t, m)
	if !strings.Contains(body, `holdfast_files_total{outcome="done"} 0`) {
		t.Errorf("expected pre-created done series at 0; body:\n%s", body)
	}
}

// TestMetrics_TheTwoSwapOutcomesGetTheirOwnSeries is this surface's half of the rule
// that a job parked indeterminate, or one applied despite an error, is reported AS THE
// STATE IT IS IN. Folding either into done or failed is not a rounding error: an alert
// on "a job whose outcome holdfast could not establish" is exactly the alert an operator
// wants, and it is unbuildable if the count is hidden inside another label.
func TestMetrics_TheTwoSwapOutcomesGetTheirOwnSeries(t *testing.T) {
	m := New(openStore(t), nil)

	m.Observe(engine.Event{Status: store.Indeterminate, Outcome: &store.Outcome{Reason: "could not establish"}})
	m.Observe(engine.Event{Status: store.AppliedDespiteError, Outcome: &store.Outcome{Reason: "applied anyway"}})

	body := scrape(t, m)
	for _, want := range []string{
		`holdfast_files_total{outcome="indeterminate"} 1`,
		`holdfast_files_total{outcome="applied-despite-error"} 1`,
		`holdfast_files_total{outcome="done"} 0`,
		`holdfast_files_total{outcome="failed"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics does not carry %q:\n%s", want, body)
		}
	}
	// Neither reclaimed anything: nothing was established, so nothing is claimed.
	if !strings.Contains(body, "holdfast_bytes_reclaimed_total 0") {
		t.Errorf("an unestablished outcome contributed reclaimed bytes:\n%s", body)
	}
}

var gateLabelRE = regexp.MustCompile(`holdfast_failures_total\{gate="([^"]*)"\} ([0-9.e+]+)`)

// failuresByGate reads the per-gate failure counts out of an exposition.
func failuresByGate(t *testing.T, body string) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, m := range gateLabelRE.FindAllStringSubmatch(body, -1) {
		v, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			t.Fatalf("holdfast_failures_total{gate=%q} is not a number: %q", m[1], m[2])
		}
		out[m[1]] = v
	}
	return out
}

// TestFailuresTotal_CountsByGateAndAlwaysAddsUp grades [AC-4] and [AC-5] of S0104: a failed
// file is counted under the gate that rejected it, from the closed gate vocabulary, and a
// failure attributable to none of them is counted under `other` rather than dropped.
//
// The adding-up is the criterion with teeth. "Which gate is rejecting encodes" is only
// answerable if the per-gate counts account for every failure there was: a failure that
// fell through the classification and was not counted at all would leave the two counters
// disagreeing, and an operator reading the difference as "the rest were fine" would be
// reading a hole in the instrument as a fact about their library.
func TestFailuresTotal_CountsByGateAndAlwaysAddsUp(t *testing.T) {
	m := New(openStore(t), nil)

	// [AC-4] Every gate reads 0 before anything fails, the fallback included: an alert on
	// chroma-floor rejections has to be writable before the first one.
	body := scrape(t, m)
	for _, gate := range engine.GateVocabulary {
		if !strings.Contains(body, `holdfast_failures_total{gate="`+gate+`"} 0`) {
			t.Errorf("the gate %q has no pre-created series:\n%s", gate, body)
		}
	}

	// [AC-4] Each of these is a rejection an operator acts on differently, and each is
	// counted as itself. The two chroma rejections are the case the whole label exists for:
	// folded into a single "vmaf" they would be indistinguishable from a mean-floor reject.
	for _, gate := range []string{
		engine.GateProbe,
		engine.GateVmafChroma,
		engine.GateVmafChroma,
		engine.GateSwap,
	} {
		m.Observe(engine.Event{Status: store.Failed, Gate: gate, Outcome: &store.Outcome{Reason: "…"}})
	}

	// [AC-5] A failure attributed to nothing, and one carrying a token from a newer build.
	// Neither may vanish, and neither may become a label value of its own.
	m.Observe(engine.Event{Status: store.Failed, Outcome: &store.Outcome{Reason: "ffmpeg exited 1"}})
	m.Observe(engine.Event{Status: store.Failed, Gate: "a-gate-from-a-newer-build"})

	body = scrape(t, m)
	got := failuresByGate(t, body)
	for gate, want := range map[string]float64{
		engine.GateProbe:      1,
		engine.GateVmafChroma: 2,
		engine.GateSwap:       1,
		engine.GateOther:      2,
		engine.GateVmafMean:   0,
	} {
		if got[gate] != want {
			t.Errorf("holdfast_failures_total{gate=%q} = %v, want %v:\n%s", gate, got[gate], want, body)
		}
	}

	// [AC-4] The label set is exactly the vocabulary: `gate` is bounded the way `guard` is.
	allowed := map[string]bool{}
	for _, gate := range engine.GateVocabulary {
		allowed[gate] = true
	}
	for gate := range got {
		if !allowed[gate] {
			t.Errorf("holdfast_failures_total carries the label value %q, which is not in the closed "+
				"gate vocabulary - a failure's error text is unbounded and must never reach a label",
				gate)
		}
	}

	// [AC-5] The identity: every failure that happened is in the per-gate counts, once.
	var sum float64
	for _, v := range got {
		sum += v
	}
	failed := testutil.ToFloat64(m.filesTotal.WithLabelValues(string(store.Failed)))
	if sum != failed {
		t.Errorf("the per-gate counts sum to %v but holdfast_files_total{outcome=\"failed\"} is %v. "+
			"A failure that is counted in one and not the other makes the difference between them "+
			"read as a set of failures nothing rejected:\n%s", sum, failed, body)
	}
	if failed == 0 {
		t.Fatal("nothing failed, so the identity above is 0 == 0 and proves nothing")
	}
}

// TestVmafFloors_ObserveOnlyWhatWasMeasured grades [AC-6] and [AC-7] of S0104: the worst
// frame and the chroma figure are observed into their own histograms over the population
// holdfast_vmaf_score already covers, and a figure that was NOT measured is observed
// nowhere - never as a zero.
//
// Zero is the trap. 0.0 is a legal value for both: a destroyed frame and an obliterated
// colour plane. So a job whose gate never ran (a remux, a disabled gate, a measurement
// that failed) must contribute NOTHING, because contributing 0.0 would put the most
// alarming reading this instrument can take into the distribution on behalf of a
// measurement nobody made - and the alert that fires on it would be about nothing.
func TestVmafFloors_ObserveOnlyWhatWasMeasured(t *testing.T) {
	m := New(openStore(t), nil)

	measured := func(mean, min, chroma float64) engine.Event {
		ev := doneEvent(1024, time.Second, mean)
		ev.Outcome.VmafMin, ev.Outcome.VmafChroma = &min, &chroma
		ev.Outcome.VmafChromaMetric = "psnr_cbcr"
		return ev
	}

	// [AC-6] Two measured encodes: both floors follow the mean's population.
	m.Observe(measured(97.5, 88.0, 41.2))
	m.Observe(measured(96.0, 62.0, 33.0))

	// [AC-7] A job the gate did not measure. It reaches Done with a mean and no floors -
	// exactly the shape a remux records - and neither histogram may take a sample for it.
	unmeasured := doneEvent(1024, time.Second, 98.0)
	unmeasured.Outcome.VmafMin, unmeasured.Outcome.VmafChroma = nil, nil
	m.Observe(unmeasured)

	body := scrape(t, m)
	for _, want := range []string{
		"holdfast_vmaf_min_count 2",
		"holdfast_vmaf_chroma_count 2",
		"holdfast_vmaf_score_count 3",
		// The sums say the figures went in as themselves, so a build that observed the
		// right NUMBER of samples with the wrong values still reds.
		"holdfast_vmaf_min_sum 150",
		"holdfast_vmaf_chroma_sum 74.2",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the exposition does not carry %q. The unmeasured job must contribute to the "+
				"mean's population and to neither floor:\n%s", want, body)
		}
	}

	// And the zero it must not have observed: with two real samples above 20 dB, a
	// fabricated 0.0 would show up in the lowest bucket of each histogram.
	for _, forbidden := range []string{
		`holdfast_vmaf_min_bucket{le="20"} 1`,
		`holdfast_vmaf_chroma_bucket{le="20"} 1`,
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("%q: an unmeasured figure was observed as 0.0. 0 is a destroyed frame and an "+
				"obliterated colour plane, not an absent measurement:\n%s", forbidden, body)
		}
	}
}

// readSeam is a store whose two SCRAPE-TIME reads can be failed independently. It doubles
// for the store, which is outside this package's boundary, and never for a collector,
// which is the subject: both collectors under test are the real ones.
//
// A failing read returns the error a store returns when it cannot answer, which is the
// only thing either collector is allowed to see.
type readSeam struct {
	store.Store
	summaryErr error
	heldErr    error
	held       int64
}

func (s *readSeam) Summary(context.Context) (map[store.Status]int, error) {
	if s.summaryErr != nil {
		return nil, s.summaryErr
	}
	return map[store.Status]int{store.Encoding: 2}, nil
}

func (s *readSeam) HeldByUndoWindow(context.Context) (int64, error) {
	if s.heldErr != nil {
		return 0, s.heldErr
	}
	return s.held, nil
}

// TestBytesHeldByUndoWindow_IsReadAtScrapeTime grades [AC-8] of S0104: the undo window's
// held bytes are a GAUGE read from the store on every scrape, the way the queue depth
// already is, and not a figure captured once when the metric set was built.
//
// The distinction is the whole value of the series. The figure it reports is the answer to
// "why has free space not gone up after all those transcodes", and a copy taken at startup
// would answer that question with the number from before the run.
func TestBytesHeldByUndoWindow_IsReadAtScrapeTime(t *testing.T) {
	// Against the REAL store first: the gauge is wired to the store this daemon runs on and
	// reports its own answer, which on an empty ledger is nothing held.
	live := New(openStore(t), nil)
	if body := scrape(t, live); !strings.Contains(body, "holdfast_bytes_held_by_undo_window 0") {
		t.Errorf("the gauge is absent, or does not read 0, against an empty real store:\n%s", body)
	}

	// Then the seam, where the figure MOVES between scrapes. A gauge read at scrape time
	// follows it; one captured at construction reports the first number for ever.
	seam := &readSeam{held: 4096}
	m := New(seam, nil)
	if body := scrape(t, m); !strings.Contains(body, "holdfast_bytes_held_by_undo_window 4096") {
		t.Fatalf("the gauge does not report the store's figure:\n%s", body)
	}
	seam.held = 8192
	if body := scrape(t, m); !strings.Contains(body, "holdfast_bytes_held_by_undo_window 8192") {
		t.Errorf("the gauge still reports the first figure after the store's answer changed, so it "+
			"is not read at scrape time:\n%s", body)
	}
}

// TestCollect_OmitsOnlyTheUnreadableGauge grades [AC-9] of S0104: when a scrape-time store
// read fails, the gauge that read would have filled is omitted and NOTHING ELSE IS - the
// other scrape-time gauge included - and the scrape is still answered 200.
//
// The two gauges fail independently or they do not fail independently. One collector
// holding both would return on the first error and silently drop the series it had not
// reached yet, so a store hiccup on the undo-window query would take the queue depth with
// it - and a monitoring surface that loses a series it did not have to lose reports an
// outage it does not have. Omitting one series from an otherwise good scrape is the
// degradation this endpoint is documented to take; failing the whole response is not.
func TestCollect_OmitsOnlyTheUnreadableGauge(t *testing.T) {
	const undo = "holdfast_bytes_held_by_undo_window"
	const depth = `holdfast_queue_depth{state="encoding"} 2`

	// Anti-vacuity: with both reads answering, BOTH gauges are in the exposition. Without
	// this, "the other one survived" would be true of a build that never emitted either.
	both := scrape(t, New(&readSeam{held: 4096}, nil))
	if !strings.Contains(both, undo+" 4096") || !strings.Contains(both, depth) {
		t.Fatalf("both reads answered but the exposition is missing a gauge, so neither case below "+
			"proves anything:\n%s", both)
	}

	t.Run("the undo-window read fails", func(t *testing.T) {
		// scrape fatals on any status but 200, which is half of what this criterion asks.
		body := scrape(t, New(&readSeam{heldErr: errors.New("store: query failed")}, nil))
		if strings.Contains(body, undo) {
			t.Errorf("the undo-window gauge is in the exposition after its read FAILED - a fabricated "+
				"figure is worse than an absent one on the series that says how much space is held:\n%s", body)
		}
		if !strings.Contains(body, depth) {
			t.Errorf("the queue-depth gauge went missing with it. The two reads are independent: a "+
				"failure of one must cost exactly one series:\n%s", body)
		}
		if !strings.Contains(body, "holdfast_files_total") {
			t.Errorf("the counters went missing too, so a store hiccup emptied the whole scrape:\n%s", body)
		}
	})

	t.Run("the queue-depth read fails", func(t *testing.T) {
		body := scrape(t, New(&readSeam{summaryErr: errors.New("store: query failed"), held: 4096}, nil))
		if strings.Contains(body, "holdfast_queue_depth") {
			t.Errorf("the queue-depth gauge is in the exposition after its read FAILED:\n%s", body)
		}
		if !strings.Contains(body, undo+" 4096") {
			t.Errorf("the undo-window gauge went missing with the queue depth, so the two are not "+
				"independent - the failure they must not share is exactly this one:\n%s", body)
		}
	})
}

// skipConstants is the ORACLE for the two criteria the next test grades: the Skip*
// constants internal/engine DECLARES, read out of that package's source and resolved to
// their values.
//
// It is read from the source rather than from engine.SkipVocabulary on purpose. A list
// asserted against itself proves nothing: the label set below is built FROM that list, so
// an oracle that was also that list would agree with any drift in it - including a guard
// added as a constant and forgotten in the list, which is the one failure this test exists
// to catch. Parsing the declarations means the only way to add a token is to add a
// constant, and a constant added without a bucket for it reds here.
//
// It is emphatically NOT engine.SkipGuards either. That slice calls itself the closed
// vocabulary and is a DIFFERENT, smaller set - the tokens `requeue --guard` accepts, which
// deliberately omit the mutable guards (hardlinked, undo-retention-failed,
// operator-excluded) because a requeue has nothing to re-open for a verdict the next pass
// clears by itself. Those guards fire in ordinary operation, so an oracle built on
// SkipGuards would agree that live guards belong in the unclassified bucket, and
// implementation and oracle would share one wrong set.
//
// The values are resolved by TYPE-CHECKING the package rather than by reading string
// literals, because one constant is not a literal at all
// (SkipRestoredOriginal = store.GuardRestoredOriginal): a reader that quietly kept only
// the literals would drop it, see one token fewer than the package declares, and go on
// passing. A vocabulary that can narrow in silence is not a closed vocabulary.
func skipConstants(t *testing.T) map[string]string {
	t.Helper()
	root, err := corpus.RepoRoot(".")
	if err != nil {
		t.Fatalf("locate the repository root: %v", err)
	}
	dir := filepath.Join(root, "internal", "engine")

	// The file set comes from go/build rather than from a directory listing, so the build
	// constraints are applied: this package carries a _linux.go and an _other.go declaring
	// the same helper, and parsing both would fail on a redeclaration that does not exist
	// in any real build.
	bp, err := build.ImportDir(dir, 0)
	if err != nil {
		t.Fatalf("read the engine package's file list: %v", err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range bp.GoFiles {
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, f)
	}
	conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	pkg, err := conf.Check("github.com/NSchatz/holdfast/internal/engine", fset, files, nil)
	if err != nil {
		t.Fatalf("type-check internal/engine: %v", err)
	}

	out := map[string]string{}
	for _, name := range pkg.Scope().Names() {
		if !strings.HasPrefix(name, "Skip") {
			continue
		}
		c, ok := pkg.Scope().Lookup(name).(*types.Const)
		if !ok {
			continue // a Skip-prefixed function or type is not part of the vocabulary
		}
		if c.Val().Kind() != constant.String {
			t.Fatalf("%s is a %s constant, not a string - the skip vocabulary is a wire format of strings",
				name, c.Val().Kind())
		}
		out[name] = constant.StringVal(c.Val())
	}
	return out
}

var skipLabelRE = regexp.MustCompile(`holdfast_skips_total\{guard="([^"]*)"\}`)

// exposedGuards is every value the guard label carries in one exposition.
func exposedGuards(body string) map[string]bool {
	out := map[string]bool{}
	for _, m := range skipLabelRE.FindAllStringSubmatch(body, -1) {
		out[m[1]] = true
	}
	return out
}

// sortedKeys renders a set in a stable order, so a failure names it the same way twice.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestSkipsTotal_UsesOnlyTheClosedVocabulary grades [AC-1], [AC-2] and [AC-3] of S0104:
// holdfast_skips_total is labelled by the guard the engine recorded, every member of the
// closed skip vocabulary is exposed at zero before anything has happened, and a reason
// outside that vocabulary is counted under `unclassified` rather than becoming a label
// value of its own.
//
// The BOUND on the label set is the point of it. `guard` is the only label on this
// counter and its values are a closed vocabulary, so the number of series is known in
// advance and nothing the engine meets on disk can move it. A counter that took its label
// from whatever a row happened to say would grow a series per distinct string.
//
// Pre-creating the whole vocabulary is the other half: an alert on "files are being
// skipped as exotic-pixel-format since that config change" has to be writable BEFORE the
// first such skip, and a series that appears only at first use cannot be alerted on until
// it is already too late.
func TestSkipsTotal_UsesOnlyTheClosedVocabulary(t *testing.T) {
	declared := skipConstants(t)
	// Anti-vacuity: an oracle that parsed nothing would satisfy every loop below.
	if len(declared) < 10 {
		t.Fatalf("only %d Skip* constant(s) were read out of internal/engine (%v) - the vocabulary "+
			"is not being read, so nothing below is being checked", len(declared), declared)
	}

	m := New(openStore(t), nil)
	body := scrape(t, m)

	// [AC-2] Every member of the vocabulary reads 0, not absent, before any event.
	for name, tok := range declared {
		if !strings.Contains(body, `holdfast_skips_total{guard="`+tok+`"} 0`) {
			t.Errorf("%s = %q is in the engine's skip vocabulary but holdfast_skips_total exposes no "+
				"series for it at 0. A guard with no series cannot be alerted on until it has already "+
				"fired.\n%s", name, tok, body)
		}
	}

	// [AC-2], the half a near-miss would fail: the MUTABLE guards. engine.SkipGuards is the
	// requeue vocabulary and omits them, and it is the identifier in this repository that
	// advertises itself as the closed one - so a counter built on it would expose no series
	// for guards that fire in ordinary operation and would bucket them as unclassified.
	requeueable := map[string]bool{}
	for _, tok := range engine.SkipGuards {
		requeueable[tok] = true
	}
	var mutable []string
	for _, tok := range declared {
		if !requeueable[tok] {
			mutable = append(mutable, tok)
		}
	}
	if len(mutable) == 0 {
		t.Fatal("every declared guard is in SkipGuards, so this case can no longer tell the two " +
			"sets apart and it proved nothing")
	}
	for _, tok := range mutable {
		if !strings.Contains(body, `holdfast_skips_total{guard="`+tok+`"} 0`) {
			t.Errorf("the mutable guard %q has no series. It is NOT in engine.SkipGuards, which is "+
				"the requeue vocabulary and not this one: this counter is built on the Skip* CONSTANT "+
				"SET, and a guard that fires in ordinary operation must have a bucket of its own", tok)
		}
	}

	// [AC-3] The fallback is exposed too, so counting an unrecognised reason never creates a
	// label value that was not already there.
	if !strings.Contains(body, `holdfast_skips_total{guard="unclassified"} 0`) {
		t.Errorf("the unclassified fallback has no pre-created series, so the first unrecognised "+
			"reason would CREATE a label value:\n%s", body)
	}

	// [AC-1] A real guard is counted under its own token - including one of the mutable
	// guards, which is exactly what a SkipGuards-shaped implementation gets wrong.
	m.Observe(engine.Event{Status: store.Skipped, Outcome: &store.Outcome{Reason: engine.SkipHardlinked}})
	m.Observe(engine.Event{Status: store.Skipped, Outcome: &store.Outcome{Reason: engine.SkipLowBitrate}})

	// [AC-3] Three ways of arriving with no usable guard: a token this build has never heard
	// of, an empty reason, and no outcome at all.
	m.Observe(engine.Event{Status: store.Skipped, Outcome: &store.Outcome{Reason: "a-guard-from-a-newer-build"}})
	m.Observe(engine.Event{Status: store.Skipped, Outcome: &store.Outcome{Reason: ""}})
	m.Observe(engine.Event{Status: store.Skipped})

	body = scrape(t, m)
	for _, want := range []string{
		`holdfast_skips_total{guard="hardlinked"} 1`,
		`holdfast_skips_total{guard="low-bitrate"} 1`,
		`holdfast_skips_total{guard="unclassified"} 3`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the exposition does not carry %q:\n%s", want, body)
		}
	}

	// [AC-3] The label set is EXACTLY the vocabulary plus the fallback, after those events
	// as before them: nothing an engine event carried became a label value.
	want := map[string]bool{"unclassified": true}
	for _, tok := range declared {
		want[tok] = true
	}
	got := exposedGuards(body)
	if len(got) == 0 {
		t.Fatalf("no holdfast_skips_total series at all, so the bound below is vacuous:\n%s", body)
	}
	for tok := range got {
		if !want[tok] {
			t.Errorf("holdfast_skips_total carries the label value %q, which is neither in the closed "+
				"vocabulary nor the fallback. The bound on this label is what keeps the number of "+
				"series knowable in advance.\nexposed: %v\nallowed: %v",
				tok, sortedKeys(got), sortedKeys(want))
		}
	}
	for tok := range want {
		if !got[tok] {
			t.Errorf("holdfast_skips_total exposes no series for %q, which is in the closed "+
				"vocabulary.\nexposed: %v", tok, sortedKeys(got))
		}
	}
}
