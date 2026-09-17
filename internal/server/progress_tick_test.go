package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/store"
)

// The progress tick, which is the frame the daemon publishes most (S0096).
//
// At `workers: 1` the transitions are seconds or minutes apart while a running encoder
// reports its position once a second, so the overwhelming majority of frames are driven by
// a batch that carries nothing but progress. None of those reports moves a row, so none of
// them can move a whole-ledger figure - and a frame that nonetheless rebuilt the figure set
// would pay a scan of the operator's whole library, once a second, for a value it already
// had.

// [AC-6] WHEN the only engine event since the last broadcast is a progress report THE
// SYSTEM SHALL publish a frame that issues no whole-ledger query, AND every figure in that
// frame SHALL keep its envelope and its availability - no figure becomes unavailable, null
// or absent because the frame was a progress frame.
//
// The second clause is the one with teeth. Suppressing the query is easy to do wrongly by
// dropping the figures from the frame, which would draw six unavailable cards once a
// second on a ledger nothing is wrong with. So the frame a progress tick publishes is
// compared FIELD BY FIELD against the frame before it, and every figure has to be the same
// figure.
func TestSnapshot_ProgressTickDoesNotRunTheAggregates(t *testing.T) {
	hub, cs, advance := clockedHub(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go hub.Run(ctx)
	sub, unsub := hub.Subscribe(ctx)
	defer unsub()
	before := frameFrom(t, sub, "the subscription's own first frame")

	// Everything from here is a progress report and nothing else, and the clock crosses
	// the refresh interval several times over while they arrive - so a frame that
	// refreshed "when the interval is up" would refresh, and this asserts it does not.
	cs.reset()
	for i := 0; i < 5; i++ {
		pos := float64(i)
		hub.Observe(engine.Event{
			Path:     "/lib/running.mkv",
			Status:   store.Encoding,
			Progress: &engine.Progress{PositionSec: pos},
		})
		advance(ledgerFigureInterval)
		after := frameFrom(t, sub, "the frame a progress report published")

		if aggs, rows := cs.counts(); aggs != 0 || rows != 0 {
			t.Fatalf("a progress tick ran %d aggregate refreshes and %d row-total reads, want 0 and 0",
				aggs, rows)
		}
		assertFiguresUndowngraded(t, before, after)
		before = after
	}
}

// [AC-6] The complement, and what stops the criterion being passable by a hub that simply
// stopped refreshing: a batch carrying a real transition DOES refresh once the interval is
// up. The rule is about what a progress report can move, not about never reading again.
func TestSnapshot_ProgressTickDoesNotRunTheAggregates_ATransitionStillRefreshes(t *testing.T) {
	hub, cs, advance := clockedHub(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go hub.Run(ctx)
	sub, unsub := hub.Subscribe(ctx)
	defer unsub()
	frameFrom(t, sub, "the subscription's own first frame")

	cs.reset()
	advance(ledgerFigureInterval)
	hub.Observe(engine.Event{Path: "/lib/done.mkv", Status: store.Done})
	frameFrom(t, sub, "the frame a transition published")

	if aggs, rows := cs.counts(); aggs != 1 || rows != 2 {
		t.Errorf("a transition past the interval ran %d aggregate refreshes and %d row-total reads, want 1 and 2",
			aggs, rows)
	}
}

// frameFrom takes the next frame off a subscription and decodes it.
func frameFrom(t *testing.T, sub <-chan []byte, what string) snapshot {
	t.Helper()
	select {
	case data := <-sub:
		var snap snapshot
		if err := json.Unmarshal(data, &snap); err != nil {
			t.Fatalf("%s is not a snapshot: %v", what, err)
		}
		return snap
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never arrived", what)
		return snapshot{}
	}
}

// assertFiguresUndowngraded holds every whole-ledger figure in one frame against the frame
// before it: same availability, same envelope, same value. The age is the one field
// allowed to move, because time passed.
func assertFiguresUndowngraded(t *testing.T, before, after snapshot) {
	t.Helper()
	for _, c := range []struct {
		name     string
		was, now spreadDTO
	}{
		{"size_ratio", before.Aggregates.SizeRatio, after.Aggregates.SizeRatio},
		{"encode_ms", before.Aggregates.EncodeMs, after.Aggregates.EncodeMs},
		{"vmaf_mean", before.Aggregates.VmafMean, after.Aggregates.VmafMean},
		{"vmaf_min", before.Aggregates.VmafMin, after.Aggregates.VmafMin},
	} {
		if !c.was.Available {
			t.Fatalf("%s was already unavailable before the progress tick: the fixture proves nothing", c.name)
		}
		if !c.now.Available {
			t.Errorf("%s went unavailable because the frame was a progress frame", c.name)
			continue
		}
		if c.now.Covers != c.was.Covers || c.now.Window != c.was.Window {
			t.Errorf("%s lost its envelope on a progress frame: covers %q->%q window %q->%q",
				c.name, c.was.Covers, c.now.Covers, c.was.Window, c.now.Window)
		}
		if c.now.Counted != c.was.Counted || c.now.Excluded != c.was.Excluded {
			t.Errorf("%s changed its counts on a progress frame: counted %d->%d excluded %d->%d",
				c.name, c.was.Counted, c.now.Counted, c.was.Excluded, c.now.Excluded)
		}
		if !sameFloat(c.now.Mean, c.was.Mean) || !sameFloat(c.now.Min, c.was.Min) || !sameFloat(c.now.Max, c.was.Max) {
			t.Errorf("%s changed its value on a progress frame: %+v -> %+v", c.name, c.was, c.now)
		}
		if c.now.AgeSeconds == nil {
			t.Errorf("%s stopped stating its age on a progress frame", c.name)
		}
	}
	for _, c := range []struct {
		name     string
		was, now breakdownDTO
	}{
		{"outcomes", before.Aggregates.Outcomes, after.Aggregates.Outcomes},
		{"skips_by_guard", before.Aggregates.SkipsByGuard, after.Aggregates.SkipsByGuard},
	} {
		if !c.was.Available {
			t.Fatalf("%s was already unavailable before the progress tick: the fixture proves nothing", c.name)
		}
		if !c.now.Available {
			t.Errorf("%s went unavailable because the frame was a progress frame", c.name)
			continue
		}
		if len(c.now.Buckets) != len(c.was.Buckets) || c.now.Counted != c.was.Counted {
			t.Errorf("%s lost its buckets on a progress frame: %+v -> %+v", c.name, c.was, c.now)
		}
		if c.now.Covers != c.was.Covers {
			t.Errorf("%s lost its covers on a progress frame: %q -> %q", c.name, c.was.Covers, c.now.Covers)
		}
		if c.now.AgeSeconds == nil {
			t.Errorf("%s stopped stating its age on a progress frame", c.name)
		}
	}
	for _, c := range []struct {
		name     string
		was, now rowTotalDTO
	}{
		{"queue_total", before.QueueTotal, after.QueueTotal},
		{"history_total", before.HistoryTotal, after.HistoryTotal},
	} {
		if !c.was.Available {
			t.Fatalf("%s was already unavailable before the progress tick: the fixture proves nothing", c.name)
		}
		if !c.now.Available {
			t.Errorf("%s went unavailable because the frame was a progress frame", c.name)
			continue
		}
		if c.now.Count == nil || c.was.Count == nil || *c.now.Count != *c.was.Count {
			t.Errorf("%s changed its count on a progress frame: %v -> %v", c.name, c.was.Count, c.now.Count)
		}
		if c.now.Cap != c.was.Cap || c.now.Covers != c.was.Covers {
			t.Errorf("%s lost its envelope on a progress frame: %+v -> %+v", c.name, c.was, c.now)
		}
	}
}

func sameFloat(a, b *float64) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}
