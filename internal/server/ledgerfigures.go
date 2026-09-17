package server

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/NSchatz/holdfast/internal/store"
)

// The whole-ledger figure set, and how often it is allowed to cost anything (S0096).
//
// WHAT IS IN IT. The six aggregates and the two row totals - every figure computed over
// EVERY matching row in the ledger. Each of them scans the matching set, so each costs
// what the operator's library costs and not what the response ships. They are the figures
// this file bounds.
//
// WHAT IS DELIBERATELY NOT IN IT. Summary is the badge counts and changes on a
// transition, so it stays live on every frame; the two capped List reads are what a frame
// is FOR; and the undo window's held bytes is a SUM over the rows currently retained,
// bounded by what is being held rather than by ledger history. None of the three is
// cached here, and that is stated rather than left to be discovered.
//
// WHAT THIS CHANGES AND WHAT IT DOES NOT. Only WHEN a figure is computed. A figure's
// meaning, its covers set, its counted/excluded split and its null-is-not-zero discipline
// are untouched: a cached figure IS the value the store returned, held whole and
// republished, so a cached frame and a fresh one carry the same figure. What a cached
// frame additionally states is the AGE of the value it is serving, so no published figure
// is served as though it had been computed for that frame.

// ledgerFigureInterval bounds the whole set to one refresh per interval, however many
// frames are published in it.
//
// 30s is a BOUND STATED IN THE CODE and not an operator knob: it is the interval a
// reporting figure over the whole ledger is allowed to cost a scan, and an installation
// that could set it to zero would be able to put the per-second rebuild back without
// changing a line. An operator watching the page sees a figure at most this stale, and
// the page is told exactly how stale it is.
const ledgerFigureInterval = 30 * time.Second

// figureAt is the cache's hold on ONE whole-ledger figure: the last value that read
// cleanly and when it was read.
//
// The last-good value is kept separately from the most recent read for the reason AC-5
// states: a refresh that fails must publish either the last good value WITH its age or
// the figure as unavailable, and it must never publish a value whose stated age is
// younger than the value actually is. Keeping the value and its own timestamp together is
// what makes the second half of that impossible to get wrong - the age published is
// always measured from the read that produced the value being served, never from the
// refresh that failed to replace it.
type figureAt[T any] struct {
	// value is the last value that read cleanly while good is true. While good is
	// false it is the most recent FAILED read, kept for its Coverage alone: a figure
	// reported unavailable still states which set it would have covered.
	value T
	// at is when value was read. Meaningless while good is false, and never published
	// then, because no value is being served to have an age.
	at   time.Time
	good bool
}

// record folds one refresh of this figure into the cache. A clean read replaces the value
// and its timestamp; a failed one leaves both exactly as they were, so the age of what is
// being served goes on counting up from the read that produced it.
func (f *figureAt[T]) record(v T, failed bool, at time.Time) {
	if !failed {
		f.value, f.at, f.good = v, at, true
		return
	}
	if !f.good {
		// Nothing good has ever been read, so this failed read is all there is. It is
		// kept for its Coverage: the figure ships as unavailable, still naming its set.
		f.value = v
	}
}

// read projects this figure as a frame at now publishes it.
func (f *figureAt[T]) read(now time.Time) figureReading[T] {
	r := figureReading[T]{value: f.value, served: f.good}
	if f.good {
		age := now.Sub(f.at)
		if age < 0 {
			age = 0 // a clock that went backwards never makes a value look younger
		}
		r.ageSec = int64(age / time.Second)
	}
	return r
}

// figureReading is one whole-ledger figure as a frame publishes it: the value, whether a
// value is being served at all, and how old that value is in whole seconds.
type figureReading[T any] struct {
	value  T
	ageSec int64
	served bool
}

// ledgerCache holds the whole-ledger figure set between refreshes.
type ledgerCache struct {
	// mu is held across a refresh as well as a read, which is what makes "at most one
	// refresh per interval" hold under concurrent frames: a second builder arriving
	// mid-refresh waits and then finds the set fresh, rather than starting its own.
	mu sync.Mutex

	// refreshedAt is when the last refresh ATTEMPT ran, and refreshed says whether one
	// ever has. The interval is measured from the attempt and not from the last clean
	// read, so a ledger that cannot be read is retried once per interval rather than
	// once per frame.
	refreshedAt time.Time
	refreshed   bool

	outcomes     figureAt[store.Breakdown]
	skipsByGuard figureAt[store.Breakdown]
	sizeRatio    figureAt[store.Spread]
	encodeMs     figureAt[store.Spread]
	vmafMean     figureAt[store.Spread]
	vmafMin      figureAt[store.Spread]
	queueTotal   figureAt[store.RowTotal]
	historyTotal figureAt[store.RowTotal]
}

// ledgerSet is the whole figure set as one frame publishes it.
type ledgerSet struct {
	Outcomes     figureReading[store.Breakdown]
	SkipsByGuard figureReading[store.Breakdown]
	SizeRatio    figureReading[store.Spread]
	EncodeMs     figureReading[store.Spread]
	VmafMean     figureReading[store.Spread]
	VmafMin      figureReading[store.Spread]
	QueueTotal   figureReading[store.RowTotal]
	HistoryTotal figureReading[store.RowTotal]
}

// due decides whether this frame refreshes the set.
//
// A COLD cache always refreshes, whatever the frame is. That is not an exception to the
// progress-tick rule so much as the floor under it: with nothing ever read there is no
// value to serve, and a frame that refused to read one would publish eight unavailable
// figures for a ledger that is perfectly readable - which is exactly what AC-6's second
// clause forbids a progress frame from doing. In the daemon it is unreachable anyway,
// because a subscriber's own first frame warms the set before any broadcast can happen.
func (c *ledgerCache) due(now time.Time, mayRefresh bool) bool {
	if !c.refreshed {
		return true
	}
	if !mayRefresh {
		return false
	}
	return now.Sub(c.refreshedAt) >= ledgerFigureInterval
}

// read projects every figure in the set as of now.
func (c *ledgerCache) read(now time.Time) ledgerSet {
	return ledgerSet{
		Outcomes:     c.outcomes.read(now),
		SkipsByGuard: c.skipsByGuard.read(now),
		SizeRatio:    c.sizeRatio.read(now),
		EncodeMs:     c.encodeMs.read(now),
		VmafMean:     c.vmafMean.read(now),
		VmafMin:      c.vmafMin.read(now),
		QueueTotal:   c.queueTotal.read(now),
		HistoryTotal: c.historyTotal.read(now),
	}
}

// ledgerFigures returns the whole-ledger figure set for this frame, refreshing it at most
// once per ledgerFigureInterval. mayRefresh false means this frame may not read the
// ledger at all (a progress tick), and it is honoured except over a cache that has never
// been read - see due.
func (h *Hub) ledgerFigures(ctx context.Context, mayRefresh bool) ledgerSet {
	now := h.now()
	h.figures.mu.Lock()
	defer h.figures.mu.Unlock()
	if h.figures.due(now, mayRefresh) {
		h.refreshLedgerFigures(ctx, now)
	}
	return h.figures.read(now)
}

// refreshLedgerFigures runs the whole set's reads once. Call with the cache locked.
//
// The refresh timestamp is stamped BEFORE the reads, so a refresh that fails still spends
// its interval: a ledger that cannot be read is retried once per interval and not once
// per frame, which is what keeps a broken store from costing more than a working one.
func (h *Hub) refreshLedgerFigures(ctx context.Context, now time.Time) {
	c := &h.figures
	c.refreshedAt, c.refreshed = now, true

	a := h.store.Aggregates(ctx)
	recordFigure(h.log, &c.outcomes, "outcomes",
		"the per-status count over every terminal row", a.Outcomes, a.Outcomes.Err, now)
	recordFigure(h.log, &c.skipsByGuard, "skips_by_guard",
		"the per-guard count over every skipped row", a.SkipsByGuard, a.SkipsByGuard.Err, now)
	recordFigure(h.log, &c.sizeRatio, "size_ratio",
		"the output/source size spread over every done row", a.SizeRatio, a.SizeRatio.Err, now)
	recordFigure(h.log, &c.encodeMs, "encode_ms",
		"the encode-duration spread over every done row", a.EncodeMs, a.EncodeMs.Err, now)
	recordFigure(h.log, &c.vmafMean, "vmaf_mean",
		"the pooled-mean VMAF spread over every done row", a.VmafMean, a.VmafMean.Err, now)
	recordFigure(h.log, &c.vmafMin, "vmaf_min",
		"the worst-frame VMAF spread over every done row", a.VmafMin, a.VmafMin.Err, now)

	qt := h.store.CountRows(ctx, activeAndPending)
	recordFigure(h.log, &c.queueTotal, "queue_total",
		"the count of matching rows in the ledger", qt, qt.Err, now)
	ht := h.store.CountRows(ctx, terminal)
	recordFigure(h.log, &c.historyTotal, "history_total",
		"the count of matching rows in the ledger", ht, ht.Err, now)
}

// recordFigure folds one figure's refresh into the cache and, when it failed, records
// WHAT FAILED AND WHAT HAPPENS NEXT (observability O4).
//
// It is `warn` and not `error` (O3): the process continues in a degraded state, serving
// either the last value that read cleanly or the figure as unavailable, and there is no
// action a human is being asked to take. The real error goes in the log, where an
// operator with server access can read it; the unauthenticated response is told only the
// fixed unavailable text, which is the whole of what it needs to render honestly.
func recordFigure[T any](log *slog.Logger, f *figureAt[T], figure, read string, v T, err error, now time.Time) {
	if err == nil {
		f.record(v, false, now)
		return
	}
	next := "reporting this figure as unavailable until a later refresh reads it"
	if f.good {
		next = "serving the last value that read cleanly, published with its age"
	}
	log.Warn("whole-ledger figure refresh failed (the rest of the frame still ships)",
		"figure", figure, "read", read, "next", next, "err", err)
	f.record(v, true, now)
}
