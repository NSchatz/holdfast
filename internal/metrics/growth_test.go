package metrics

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// Retention DISABLED, seen through the metrics that already exist (LEDGER-5).
//
// Criterion 4: IF pruning is disabled THEN THE SYSTEM SHALL keep every row and SHALL
// report the table's growth through the metrics it already exposes.
//
// Criterion 18: WHEN the metrics endpoint is scraped after this change THE SYSTEM SHALL
// publish every metric name it publishes today, none renamed and none removed.
//
// The two belong together because the first is only true if the second is: "the metrics it
// already exposes" is a promise about names, and a renamed metric breaks every dashboard
// built on it SILENTLY - the reason this repository froze its metric namespace at the
// rename and has a script that fails if a pre-rename identifier reappears.
//
// The gauge is the right instrument here and no new one was added. holdfast_queue_depth is
// read from store.Summary AT SCRAPE TIME, over every status including the terminal ones,
// so a table that grows is a gauge that grows with no wiring at all - which is exactly what
// criterion 4 asks to be demonstrated rather than assumed.

// holdfastMetrics is the frozen set of metric names this build publishes. Adding to it is
// a deliberate act; renaming or removing one is a silent break for every dashboard and
// alert an operator has already built, which is why the assertion is on the SET and not
// on "at least these".
var holdfastMetrics = []string{
	"holdfast_bytes_reclaimed_total",
	"holdfast_encode_duration_seconds",
	"holdfast_files_total",
	"holdfast_queue_depth",
	"holdfast_vmaf_score",
}

var metricNameRe = regexp.MustCompile(`(?m)^# TYPE (holdfast_[a-z_0-9]+) `)

func publishedMetricNames(t *testing.T, m *Metrics) []string {
	t.Helper()
	var names []string
	for _, hit := range metricNameRe.FindAllStringSubmatch(scrape(t, m), -1) {
		names = append(names, hit[1])
	}
	sort.Strings(names)
	return names
}

// queueDepth reads holdfast_queue_depth{state="..."} out of a scrape. It parses the
// exposition text rather than reaching into the collector, because the criterion is about
// what a SCRAPE reports - which is the only thing an operator's Prometheus ever sees.
func queueDepth(t *testing.T, body, state string) int {
	t.Helper()
	want := `holdfast_queue_depth{state="` + state + `"} `
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, want) {
			v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, want)), 64)
			if err != nil {
				t.Fatalf("holdfast_queue_depth{state=%q} is not a number: %q", state, line)
			}
			return int(v)
		}
	}
	return 0 // absent means no row in that state, which the gauge reports by omission
}

// seedSkipped writes terminal rows for files UNDER root that are not there. root is the
// library the scan below lists, so these are the rows a churning library leaves behind -
// history for files it has finished with, and the rows a retention exists to bound.
func seedSkipped(t *testing.T, st *store.SQLite, root string, from, to int) {
	t.Helper()
	ctx := context.Background()
	for i := from; i < to; i++ {
		p := filepath.Join(root, "history"+strconv.Itoa(i)+".mkv")
		ok, err := st.Claim(ctx, p, "fp", "w0", 3)
		if err != nil || !ok {
			t.Fatalf("seed claim %s: ok=%v err=%v", p, ok, err)
		}
		if err := st.Finish(ctx, p, "fp", store.Skipped, &store.Outcome{Reason: engine.SkipLowBitrate}); err != nil {
			t.Fatalf("seed finish %s: %v", p, err)
		}
	}
}

// scanWithRetention runs one real engine scan over root with the given retention. root is
// EMPTY because the criterion is about the LEDGER, not about encoding: the scan is here
// only because it is what triggers the retention pass. It is the SAME directory the rows
// were seeded under, which is what makes them prunable - the pass lists it and the files
// are not there.
func scanWithRetention(t *testing.T, st store.Store, root string, rows int) {
	t.Helper()
	cfg := config.Config{
		LibraryRoots:         []string{root},
		VideoExts:            []string{"mkv"},
		Encoder:              "cpu",
		MaxFailures:          3,
		HistoryRetentionRows: rows,
	}
	eng := engine.New(cfg, probe.New("ffmpeg", "ffprobe"), nil, st,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
}

// --- criterion 4 ------------------------------------------------------------------------

func TestGrowth_WithRetentionDisabledEveryRowIsKeptAndTheGaugeShowsTheTableGrowing(t *testing.T) {
	st := openStore(t)
	m := New(st)
	root := t.TempDir()

	// The shipped default: history_retention_rows unset, which resolves to 0.
	var cfg config.Config
	if cfg.RetentionEnabled() {
		t.Fatal("the zero-value configuration enables retention; the shipped default must be disabled")
	}

	seedSkipped(t, st, root, 0, 40)
	scanWithRetention(t, st, root, cfg.HistoryRetentionRows)
	if got := queueDepth(t, scrape(t, m), "skipped"); got != 40 {
		t.Fatalf("holdfast_queue_depth{state=\"skipped\"} reads %d after 40 rows and a scan with retention disabled", got)
	}

	// The table GROWS, and the gauge grows with it. No new metric, no new wiring: the
	// figure is read from the store on every scrape.
	seedSkipped(t, st, root, 40, 140)
	scanWithRetention(t, st, root, cfg.HistoryRetentionRows)
	body := scrape(t, m)
	if got := queueDepth(t, body, "skipped"); got != 140 {
		t.Fatalf("holdfast_queue_depth{state=\"skipped\"} reads %d after the table grew to 140", got)
	}

	seedSkipped(t, st, root, 140, 400)
	scanWithRetention(t, st, root, cfg.HistoryRetentionRows)
	if got := queueDepth(t, scrape(t, m), "skipped"); got != 400 {
		t.Fatalf("holdfast_queue_depth{state=\"skipped\"} reads %d after the table grew to 400", got)
	}
}

// The anti-vacuity half: the gauge is not simply counting up regardless. Enable retention
// on the identical fixture and the same scrape reports the bounded table - which is what
// makes the reading above a measurement of the ledger rather than of the seeding loop.
func TestGrowth_TheGaugeFollowsTheTableDownWhenRetentionIsEnabled(t *testing.T) {
	st := openStore(t)
	m := New(st)
	root := t.TempDir()

	seedSkipped(t, st, root, 0, 400)
	if got := queueDepth(t, scrape(t, m), "skipped"); got != 400 {
		t.Fatalf("holdfast_queue_depth{state=\"skipped\"} reads %d before the prune, want 400", got)
	}
	scanWithRetention(t, st, root, 25)
	if got := queueDepth(t, scrape(t, m), "skipped"); got != 25 {
		t.Fatalf("holdfast_queue_depth{state=\"skipped\"} reads %d after a scan with history_retention_rows: 25; "+
			"if this is still 400 the reading above cannot fail and proves nothing", got)
	}
}

// --- criterion 18 -------------------------------------------------------------------------

func TestGrowth_TheMetricNamesPublishedTodayAreStillPublishedAndNoneWasRenamed(t *testing.T) {
	st := openStore(t)
	m := New(st)

	// Something in every series, so nothing is absent merely for want of a data point.
	seedSkipped(t, st, t.TempDir(), 0, 3)
	m.Observe(doneEvent(1024, 1000, 97.5))
	m.Observe(engine.Event{Status: store.Skipped})
	m.Observe(engine.Event{Status: store.Failed})

	got := publishedMetricNames(t, m)
	want := append([]string(nil), holdfastMetrics...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the published holdfast_* metric set is\n  %v\nwant\n  %v\n"+
			"a renamed or removed metric silently breaks every dashboard and alert built on it",
			got, want)
	}

	// And the queue-depth gauge still carries its `state` label over every status,
	// terminal ones included - which is the whole of how growth is visible.
	//
	// The fixture puts a real row in each of the other six states first, and it has to: the
	// gauge reports an empty state by OMISSION, so over a table holding only skipped rows
	// this loop would be asking whether six series that SHOULD be absent are absent, and
	// answering yes whatever the collector did.
	seedOneRowInEveryOtherState(t, st)

	body := scrape(t, m)
	for _, c := range []struct {
		state string
		rows  int
	}{
		{"pending", 1}, {"probing", 1}, {"encoding", 1}, {"verifying", 1},
		{"done", 1}, {"skipped", 3}, {"failed", 1},
	} {
		if !strings.Contains(body, `holdfast_queue_depth{state="`+c.state+`"`) {
			t.Errorf("holdfast_queue_depth carries no series for state %q, which holds %d row(s)", c.state, c.rows)
			continue
		}
		if got := queueDepth(t, body, c.state); got != c.rows {
			t.Errorf("holdfast_queue_depth{state=%q} reads %d, want the %d seeded row(s)", c.state, got, c.rows)
		}
	}
}

// seedOneRowInEveryOtherState leaves exactly one row in each of pending, probing, encoding,
// verifying, done and failed, beside whatever skipped rows the caller seeded.
//
// Order is load-bearing. Claim is the only way to create a row and it creates it PROBING,
// and RecoverStale is the only way back to pending - and it resets EVERY active row - so the
// pending one has to be made and reset before the three that must stay active exist.
func seedOneRowInEveryOtherState(t *testing.T, st *store.SQLite) {
	t.Helper()
	ctx := context.Background()
	claim := func(path string) {
		t.Helper()
		ok, err := st.Claim(ctx, path, "fp", "w0", 3)
		if err != nil || !ok {
			t.Fatalf("seed claim %s: ok=%v err=%v", path, ok, err)
		}
	}

	claim("/lib/state-pending.mkv")
	if _, err := st.RecoverStale(ctx); err != nil {
		t.Fatalf("seed RecoverStale: %v", err)
	}

	claim("/lib/state-probing.mkv") // left where Claim puts it

	for _, c := range []struct {
		path string
		to   store.Status
	}{
		{"/lib/state-encoding.mkv", store.Encoding},
		{"/lib/state-verifying.mkv", store.Verifying},
	} {
		claim(c.path)
		if err := st.Advance(ctx, c.path, "fp", c.to); err != nil {
			t.Fatalf("seed advance %s to %s: %v", c.path, c.to, err)
		}
	}

	for _, c := range []struct {
		path string
		to   store.Status
	}{
		{"/lib/state-done.mkv", store.Done},
		{"/lib/state-failed.mkv", store.Failed},
	} {
		claim(c.path)
		if err := st.Finish(ctx, c.path, "fp", c.to, &store.Outcome{Encoder: "cpu"}); err != nil {
			t.Fatalf("seed finish %s as %s: %v", c.path, c.to, err)
		}
	}
}

func TestGrowth_ARetentionPassPublishesNoNewMetricOfItsOwn(t *testing.T) {
	// Criterion 4 is met THROUGH the metrics that exist. A prune that added its own
	// counter would be a new name to freeze at the first tag for no gain, and the spec
	// puts adding one out of scope.
	st := openStore(t)
	m := New(st)
	root := t.TempDir()
	seedSkipped(t, st, root, 0, 50)
	before := publishedMetricNames(t, m)

	scanWithRetention(t, st, root, 5)
	after := publishedMetricNames(t, m)
	if strings.Join(before, ",") != strings.Join(after, ",") {
		t.Errorf("a retention pass changed the published metric set from\n  %v\nto\n  %v", before, after)
	}
}
