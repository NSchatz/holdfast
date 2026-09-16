package metrics

import (
	"context"
	"testing"

	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/store"
)

// TestMetrics_AfterADryPassTheDepthGaugeReadsZeroForEveryActiveState grades [AC-9], the
// instrumentation half: once the pass is over, holdfast_queue_depth reads 0 for every active
// state, `probing` included, and the decisions are counted as the terminal facts they are.
//
// `probing` on this gauge means CLAIMED AND NOT YET DECIDED, so a decided file counted there
// keeps an operator's "work in hand" alert firing for ever over a library nothing is working
// on. The figure is READ THROUGH A SCRAPE, which is the only thing an operator's Prometheus
// ever sees, and this gauge spells zero by OMISSION: it is computed from store.Summary at
// scrape time and Summary names only the statuses holding a row, so a state with nothing in it
// has no sample and queueDepth reads it as the 0 it is.
//
// The claim is asserted VISIBLE first, at 1, on the same gauge through the same reader. Without
// that this would be asking whether four series that should be absent are absent, and
// answering yes whatever the collector did.
func TestMetrics_AfterADryPassTheDepthGaugeReadsZeroForEveryActiveState(t *testing.T) {
	st := openStore(t)
	m := New(st)
	ctx := context.Background()

	candidate, guarded := "/lib/candidate.mkv", "/lib/already.mkv"
	if ok, err := st.Claim(ctx, candidate, "9:9", "w0", 3, store.DecisionInputs{}); err != nil || !ok {
		t.Fatalf("Claim(%s): ok=%v err=%v", candidate, ok, err)
	}
	if got := queueDepth(t, scrape(t, m), string(store.Probing)); got != 1 {
		t.Fatalf("holdfast_queue_depth{state=\"probing\"} reads %d while one file is claimed and not yet "+
			"decided, want 1. If the gauge cannot see a claim, the reading after the pass below proves "+
			"nothing", got)
	}
	if err := st.Finish(ctx, candidate, "9:9", store.WouldTranscode,
		&store.Outcome{SourceCodec: "h264", TargetPath: candidate}, 3); err != nil {
		t.Fatalf("Finish(would-transcode): %v", err)
	}
	if ok, err := st.Claim(ctx, guarded, "7:7", "w0", 3, store.DecisionInputs{}); err != nil || !ok {
		t.Fatalf("Claim(%s): ok=%v err=%v", guarded, ok, err)
	}
	if err := st.Finish(ctx, guarded, "7:7", store.Skipped,
		&store.Outcome{Reason: engine.SkipAlreadyTargetCodec}, 3); err != nil {
		t.Fatalf("Finish(skipped): %v", err)
	}

	body := scrape(t, m)
	for _, state := range []store.Status{store.Pending, store.Probing, store.Encoding, store.Verifying} {
		if got := queueDepth(t, body, string(state)); got != 0 {
			t.Errorf("holdfast_queue_depth{state=%q} reads %d once the pass is over, want 0. A dry pass "+
				"decided every file it claimed, so nothing is in progress", state, got)
		}
	}
	// And the decisions are reported as themselves, so "nothing in progress" is not "nothing
	// happened".
	for _, c := range []struct {
		state store.Status
		want  int
	}{{store.WouldTranscode, 1}, {store.Skipped, 1}} {
		if got := queueDepth(t, body, string(c.state)); got != c.want {
			t.Errorf("holdfast_queue_depth{state=%q} reads %d, want %d", c.state, got, c.want)
		}
	}
}
