package metrics

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/store"
)

// What /metrics says about a dry run's decisions.
//
// The whole of the change is a new LABEL VALUE on a counter that already exists plus a
// gauge series the store's own GROUP BY produces. No new metric NAME: the published name
// set is frozen, because a renamed or added-then-renamed metric breaks every dashboard
// built on it silently, and the frozen set is asserted here as a SET.

// TestMetrics_ADryRunsDecisionsAreCountedUnderTheirOwnOutcome. Counted as themselves, and
// counted as DECISIONS: nothing was encoded, so nothing rides along in the reclaimed
// bytes, the encode-duration histogram or the VMAF distribution.
func TestMetrics_ADryRunsDecisionsAreCountedUnderTheirOwnOutcome(t *testing.T) {
	m := New(openStore(t))

	src := int64(4_000_000)
	for i := 0; i < 3; i++ {
		m.Observe(engine.Event{
			Status:  store.WouldTranscode,
			Outcome: &store.Outcome{SourceCodec: "h264", SourceBytes: &src},
		})
	}

	body := scrape(t, m)
	if !strings.Contains(body, `holdfast_files_total{outcome="would-transcode"} 3`) {
		t.Errorf("/metrics does not report three recorded decisions under their own outcome:\n%s",
			outcomeLines(body))
	}
	for _, notCounted := range []string{
		`holdfast_files_total{outcome="done"} 3`,
		`holdfast_files_total{outcome="skipped"} 3`,
	} {
		if strings.Contains(body, notCounted) {
			t.Errorf("a dry run's decisions were folded into another outcome (%s). \"skipped\" would "+
				"say the file does not qualify, which is the opposite of what the row records, and "+
				"\"done\" would claim a transcode that never happened", notCounted)
		}
	}
	// Nothing was encoded, so none of the encode-side instruments moved.
	for _, want := range []string{
		"holdfast_bytes_reclaimed_total 0",
		"holdfast_encode_duration_seconds_count 0",
		"holdfast_vmaf_score_count 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics is missing %q - a recorded decision must not move an encode-side "+
				"instrument, because nothing encoded anything", want)
		}
	}
}

// TestMetrics_TheCandidateSeriesReadsZeroBeforeTheFirstDryRun. An alert on the candidate
// count has to be buildable BEFORE anybody runs a dry run.
//
// Prometheus omits a series that has never been touched, and an alert on an absent series
// does not fire - it evaluates to no data. So the series is pre-created at construction,
// exactly as the other outcomes are, and reads a real 0.
func TestMetrics_TheCandidateSeriesReadsZeroBeforeTheFirstDryRun(t *testing.T) {
	m := New(openStore(t))

	body := scrape(t, m)
	if !strings.Contains(body, `holdfast_files_total{outcome="would-transcode"} 0`) {
		t.Errorf("the candidate series is absent (or not 0) on a fresh process. An alert on a series "+
			"that only appears once something has happened cannot be written before it is needed:\n%s",
			outcomeLines(body))
	}
}

// TestMetrics_TheLiveDepthGaugeReportsCandidatesAsThemselves is the second half, and the
// half where mis-grouping is destructive rather than merely untidy.
//
// The gauge is read from the STORE at scrape time, so it reports what the ledger holds
// right now. `probing` on this gauge means CLAIMED AND NOT YET DECIDED - a worker is
// looking at that file this second - and a decided candidate counted there would keep an
// operator's "work in hand" alert firing for ever over files nothing is working on.
func TestMetrics_TheLiveDepthGaugeReportsCandidatesAsThemselves(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	// Two recorded decisions and one file genuinely being examined.
	for _, p := range []string{"/lib/a.mkv", "/lib/b.mkv"} {
		if ok, err := st.Claim(ctx, p, "1:1", "w0", 3); err != nil || !ok {
			t.Fatalf("Claim(%s): ok=%v err=%v", p, ok, err)
		}
		if err := st.Finish(ctx, p, "1:1", store.WouldTranscode, &store.Outcome{SourceCodec: "h264"}); err != nil {
			t.Fatalf("Finish(%s): %v", p, err)
		}
	}
	if ok, err := st.Claim(ctx, "/lib/in-hand.mkv", "2:2", "w1", 3); err != nil || !ok {
		t.Fatalf("Claim(in-hand): ok=%v err=%v", ok, err)
	}

	body := scrape(t, New(st))
	if got := queueDepth(t, body, string(store.WouldTranscode)); got != 2 {
		t.Errorf("holdfast_queue_depth{state=\"would-transcode\"} = %d, want 2", got)
	}
	if got := queueDepth(t, body, string(store.Probing)); got != 1 {
		t.Errorf("holdfast_queue_depth{state=\"probing\"} = %d, want 1. A decided candidate counted "+
			"as probing would say a worker is examining a file nothing is examining", got)
	}
}

// TestMetrics_NoNewMetricNameIsPublished. The name set is frozen and this change does not
// widen it: a label value is free to a dashboard, a NAME is not.
//
// It is asserted as a SET rather than as "at least these", because the failure that
// matters is not only an addition - it is a rename, which reads as an addition plus a
// removal and breaks every alert already written against the old one.
func TestMetrics_NoNewMetricNameIsPublished(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	// A row in the ledger, so the depth gauge - which reports by omission when there is
	// nothing in any state - is actually published and can be compared.
	if ok, err := st.Claim(ctx, "/lib/a.mkv", "1:1", "w0", 3); err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}
	if err := st.Finish(ctx, "/lib/a.mkv", "1:1", store.WouldTranscode, &store.Outcome{SourceCodec: "h264"}); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	m := New(st)
	src := int64(1024)
	m.Observe(engine.Event{
		Status:  store.WouldTranscode,
		Outcome: &store.Outcome{SourceCodec: "h264", SourceBytes: &src},
	})

	got := publishedMetricNames(t, m)
	want := append([]string(nil), holdfastMetrics...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the published metric NAME set moved after a dry-run decision was recorded:\n"+
			"  got  %v\n  want %v\nA new label value is free to every dashboard built on this; a new "+
			"or renamed name is not", got, want)
	}
}

// outcomeLines pulls just the files_total series out of a scrape, so a failure prints the
// figures it is about rather than the whole Go runtime's instrumentation.
func outcomeLines(body string) string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "holdfast_files_total{") {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
