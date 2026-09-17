package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/store"
)

// The whole-ledger figure set and what it costs (S0096).
//
// Every figure here is computed over EVERY matching row, so each one costs what the
// operator's library costs. Bounding them to one refresh per interval is only honest if
// the frame SAYS how old the value it is serving is, and only safe if a figure served
// from the cache is the same figure a fresh read would have produced. Both are graded.

// clockedHub is a Hub over a counting double whose clock a test drives. The returned
// advance moves the hub's clock forward without sleeping through a 30s interval.
//
// The clock is behind a mutex because the hub reads it from its own Run goroutine: a
// plain variable here would be a data race in every test that starts one, which under
// -race is a failure about the test rather than about the code.
func clockedHub(t *testing.T) (*Hub, *countingStore, func(time.Duration)) {
	t.Helper()
	hub, cs := countingHub(t)
	c := &testClock{at: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
	hub.now = c.now
	return hub, cs, c.advance
}

type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// [AC-3] WHEN frames are published inside one refresh interval THE SYSTEM SHALL run at
// most one refresh of the whole-ledger figure set (the six aggregates and the two row
// totals) in that interval, however many frames are published, and no other frame in it
// SHALL issue a whole-ledger query.
//
// "At most one" is graded at BOTH ends: the frames inside one interval must cost exactly
// one refresh, and crossing the interval must cost another - a cache that never refreshed
// again would satisfy the first half and publish a figure frozen at startup.
func TestSnapshot_WholeLedgerFiguresRefreshAtMostOncePerInterval(t *testing.T) {
	hub, cs, advance := clockedHub(t)
	ctx := context.Background()

	// 40 frames spread across ONE interval. The ledger is read for the first of them and
	// for none of the other 39.
	for i := 0; i < 40; i++ {
		if _, err := hub.buildSnapshot(ctx, true); err != nil {
			t.Fatalf("buildSnapshot: %v", err)
		}
		advance(ledgerFigureInterval / 50)
	}
	if cs.aggregates != 1 {
		t.Errorf("40 frames inside one interval ran %d aggregate refreshes, want 1", cs.aggregates)
	}
	if cs.countRows != 2 {
		t.Errorf("40 frames inside one interval ran %d row-total reads, want 2 (the queue and history totals, once)",
			cs.countRows)
	}
	// And the frames that did not refresh still paid for what a frame is FOR: the badge
	// counts and the two capped views are live on every one of them.
	if cs.summary != 40 || cs.list != 80 {
		t.Errorf("the per-frame reads were cached too: summary=%d (want 40) list=%d (want 80)", cs.summary, cs.list)
	}

	// Past the interval, the next frame refreshes - exactly once more, not once per frame.
	cs.reset()
	advance(ledgerFigureInterval)
	for i := 0; i < 5; i++ {
		if _, err := hub.buildSnapshot(ctx, true); err != nil {
			t.Fatalf("buildSnapshot after the interval: %v", err)
		}
	}
	if cs.aggregates != 1 || cs.countRows != 2 {
		t.Errorf("crossing the interval ran %d aggregate refreshes and %d row-total reads over 5 frames, want 1 and 2",
			cs.aggregates, cs.countRows)
	}
}

// [AC-3] The read endpoints share the same set and the same interval: polling /api/summary
// in a loop must not put the per-call scan back that the stream no longer pays.
func TestSnapshot_WholeLedgerFiguresRefreshAtMostOncePerInterval_AcrossEveryPublisher(t *testing.T) {
	hub, cs, advance := clockedHub(t)
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		aggregatesOf(hub.ledgerFigures(ctx, true))              // what /api/summary publishes
		rowTotalOf(hub.ledgerFigures(ctx, true).QueueTotal, 1)  // what /api/queue publishes
		if _, err := hub.buildSnapshot(ctx, true); err != nil { // what a subscriber is sent
			t.Fatalf("buildSnapshot: %v", err)
		}
		advance(ledgerFigureInterval / 100)
	}
	if cs.wholeLedgerReads() != 3 {
		t.Errorf("60 published figures inside one interval cost %d whole-ledger reads, want 3 "+
			"(one Aggregates and two CountRows): the read endpoints do not share the set",
			cs.wholeLedgerReads())
	}
}

// [AC-4] WHEN a whole-ledger figure is published THE SYSTEM SHALL state the age of the
// value it is serving in that figure's own published coverage envelope, so no published
// figure's freshness is unstated and a cached value is never published as though it had
// been computed for that frame.
//
// Asserted on the RAW BYTES beside the figure it belongs to, because that is where the
// criterion lives: the age has to be IN the figure's own envelope, and a decoder that
// found it at the top of the frame would report the same Go value for a shape a client
// could not use to tell one figure's freshness from another's.
func TestSnapshot_WholeLedgerFigureStatesItsAge(t *testing.T) {
	hub, _, advance := clockedHub(t)
	ctx := context.Background()

	// Frame one: computed for this frame, so every figure is zero seconds old.
	data, err := hub.SnapshotJSON(ctx)
	if err != nil {
		t.Fatalf("SnapshotJSON: %v", err)
	}
	for name, age := range figureAges(t, data) {
		if age == nil {
			t.Errorf("%s states no age: a client cannot tell a value read for this frame from one read an interval ago", name)
			continue
		}
		if *age != 0 {
			t.Errorf("%s was read for this frame and states an age of %ds, want 0", name, *age)
		}
	}

	// Frame two, 12 seconds later and inside the interval: the SAME values are served,
	// and every one of them says so.
	advance(12 * time.Second)
	data, err = hub.SnapshotJSON(ctx)
	if err != nil {
		t.Fatalf("SnapshotJSON: %v", err)
	}
	for name, age := range figureAges(t, data) {
		if age == nil {
			t.Fatalf("%s states no age on a cached frame", name)
		}
		if *age != 12 {
			t.Errorf("%s is being served a value read 12s ago but states an age of %ds", name, *age)
		}
	}
}

// [AC-4] A figure that is NOT being served a value states no age: an unavailable figure
// has no value for an age to be the age of, and a 0 there would read as "computed for
// this frame" - the exact overclaim the field exists to prevent.
func TestSnapshot_WholeLedgerFigureStatesItsAge_UnavailableStatesNone(t *testing.T) {
	hub, cs, _ := clockedHub(t)
	cs.aggErr = errors.New("simulated: the aggregate could not be read")
	cs.countErr = errors.New("simulated: the total could not be counted")

	data, err := hub.SnapshotJSON(context.Background())
	if err != nil {
		t.Fatalf("SnapshotJSON: %v", err)
	}
	for name, age := range figureAges(t, data) {
		if age != nil {
			t.Errorf("%s could not be read at all yet states an age of %ds", name, *age)
		}
	}
	if !strings.Contains(string(data), `"available":false`) {
		t.Fatalf("no figure reported itself unavailable: %s", data)
	}
}

// [AC-5] IF a refresh of a whole-ledger figure fails THEN THE SYSTEM SHALL publish either
// the last good value WITH its age or that figure as unavailable carrying the existing
// fixed unavailable text, SHALL never publish a value whose stated age is younger than the
// value actually is, and SHALL record the failure at `warn`.
//
// This build takes the first branch where it can: a value that read cleanly is worth more
// to an operator than a blank card, PROVIDED the frame says how old it is. The age is the
// assertion that matters - a cache that re-stamped the value on the failed refresh would
// publish a stale figure as fresh, which is worse than publishing nothing.
func TestSnapshot_FailedRefreshKeepsLastGoodValueOrReportsUnavailable(t *testing.T) {
	hub, cs, advance := clockedHub(t)
	ctx := context.Background()
	logged := captureWarnings(hub)

	// One clean refresh, so there is a last good value to keep.
	if _, err := hub.SnapshotJSON(ctx); err != nil {
		t.Fatalf("SnapshotJSON: %v", err)
	}

	// Now every read fails, and the interval has passed so the next frame retries.
	cs.aggErr = errors.New("simulated: the aggregate could not be read")
	cs.countErr = errors.New("simulated: the total could not be counted")
	advance(ledgerFigureInterval)
	advance(5 * time.Second)

	data, err := hub.SnapshotJSON(ctx)
	if err != nil {
		t.Fatalf("SnapshotJSON over a failing ledger: %v", err)
	}
	if cs.aggregates != 2 {
		t.Fatalf("the failing refresh did not run: %d aggregate reads", cs.aggregates)
	}

	var frame struct {
		Aggregates map[string]json.RawMessage `json:"aggregates"`
		QueueTotal json.RawMessage            `json:"queue_total"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	for name, age := range figureAges(t, data) {
		if age == nil {
			// The other admissible branch: unavailable, with the fixed text.
			continue
		}
		if *age != int64((ledgerFigureInterval+5*time.Second)/time.Second) {
			t.Errorf("%s is serving a value read %v ago but states an age of %ds - a stale value "+
				"published as though the failed refresh had produced it",
				name, ledgerFigureInterval+5*time.Second, *age)
		}
	}
	// A served figure keeps the value it had, not an emptied one.
	var outcomes breakdownDTO
	if err := json.Unmarshal(frame.Aggregates["outcomes"], &outcomes); err != nil {
		t.Fatalf("unmarshal outcomes: %v", err)
	}
	switch {
	case outcomes.Available:
		if len(outcomes.Buckets) == 0 {
			t.Error("outcomes reports itself available with no buckets: the last good value was lost")
		}
		if outcomes.AgeSeconds == nil {
			t.Error("outcomes is serving the last good value without stating its age")
		}
	default:
		if outcomes.Unavailable != aggregateUnavailable {
			t.Errorf("an unavailable figure ships %q, want the fixed text %q", outcomes.Unavailable, aggregateUnavailable)
		}
	}

	// And the failure was RECORDED: which figure, which read, and what happens next.
	warnings := logged()
	if len(warnings) == 0 {
		t.Fatal("a failed refresh recorded nothing: an operator has no way to learn why a figure stopped moving")
	}
	for _, want := range []string{"outcomes", "queue_total"} {
		if !anyContains(warnings, want) {
			t.Errorf("no warning names the figure %q: %v", want, warnings)
		}
	}
	if !anyContains(warnings, "every terminal row") {
		t.Errorf("no warning names the read that failed: %v", warnings)
	}
	if !anyContains(warnings, "serving the last value") && !anyContains(warnings, "unavailable") {
		t.Errorf("no warning says what happens next: %v", warnings)
	}
}

// [AC-5] The unhappy path with no last good value at all: a ledger that has never read
// cleanly publishes the figure as unavailable with the existing fixed text, and never a
// fabricated zero-aged value.
func TestSnapshot_FailedRefreshKeepsLastGoodValueOrReportsUnavailable_WithNothingGoodYet(t *testing.T) {
	hub, cs, _ := clockedHub(t)
	cs.aggErr = errors.New("simulated: the aggregate could not be read")

	data, err := hub.SnapshotJSON(context.Background())
	if err != nil {
		t.Fatalf("SnapshotJSON: %v", err)
	}
	var frame struct {
		Aggregates map[string]json.RawMessage `json:"aggregates"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	for name, raw := range frame.Aggregates {
		var b breakdownDTO
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatalf("unmarshal %s: %v", name, err)
		}
		if b.Available {
			t.Errorf("%s reports itself available over a ledger that has never read cleanly", name)
		}
		if b.Unavailable != aggregateUnavailable {
			t.Errorf("%s ships %q, want the fixed text", name, b.Unavailable)
		}
		if b.AgeSeconds != nil {
			t.Errorf("%s states an age of %d with no value to be the age of", name, *b.AgeSeconds)
		}
		if b.Covers == "" {
			t.Errorf("%s states no covers: an unavailable figure still names the set it would have covered", name)
		}
	}
}

// [AC-13] WHEN the same ledger state is published from a cached value and from a fresh
// read THE SYSTEM SHALL publish the same figure: the same buckets or the same
// min/mean/max, the same `covers`, the same `counted` and the same `excluded`.
//
// Over a REAL store, seeded over both caps, so the figures have buckets, spreads and a
// non-zero exclusion count to disagree about. The age is the one field allowed to differ:
// it is the fact that the value came from the cache.
func TestSnapshot_CachedFigureEqualsAFreshOne(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	seedOverCap(t, st)

	ctrl := NewController(context.Background(), nil, discard())
	hub := NewHub(st, ctrl, discard())
	at := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	hub.now = func() time.Time { return at }
	ctx := context.Background()

	fresh := snapshotOf(t, hub) // the refresh
	at = at.Add(9 * time.Second)
	cached := snapshotOf(t, hub) // served from the cache, no read at all

	// A second hub over the same unchanged ledger reads every figure fresh. It is the
	// control: what the cached frame is being compared AGAINST is a real read, not the
	// first frame's own memory of one.
	control := NewHub(st, NewController(ctx, nil, discard()), discard())
	controlSnap := snapshotOf(t, control)

	for _, c := range []struct {
		name           string
		got, want, ref spreadDTO
	}{
		{"size_ratio", cached.Aggregates.SizeRatio, fresh.Aggregates.SizeRatio, controlSnap.Aggregates.SizeRatio},
		{"encode_ms", cached.Aggregates.EncodeMs, fresh.Aggregates.EncodeMs, controlSnap.Aggregates.EncodeMs},
		{"vmaf_mean", cached.Aggregates.VmafMean, fresh.Aggregates.VmafMean, controlSnap.Aggregates.VmafMean},
		{"vmaf_min", cached.Aggregates.VmafMin, fresh.Aggregates.VmafMin, controlSnap.Aggregates.VmafMin},
	} {
		if c.want.Counted == 0 {
			t.Fatalf("%s counted nothing in the fresh frame: the fixture proves nothing", c.name)
		}
		assertSameSpread(t, c.name+" (cached against the frame that read it)", c.got, c.want)
		assertSameSpread(t, c.name+" (cached against an independent fresh read)", c.got, c.ref)
	}
	for _, c := range []struct {
		name           string
		got, want, ref breakdownDTO
	}{
		{"outcomes", cached.Aggregates.Outcomes, fresh.Aggregates.Outcomes, controlSnap.Aggregates.Outcomes},
		{"skips_by_guard", cached.Aggregates.SkipsByGuard, fresh.Aggregates.SkipsByGuard, controlSnap.Aggregates.SkipsByGuard},
	} {
		if len(c.want.Buckets) == 0 {
			t.Fatalf("%s has no buckets in the fresh frame: the fixture proves nothing", c.name)
		}
		assertSameBreakdown(t, c.name+" (cached against the frame that read it)", c.got, c.want)
		assertSameBreakdown(t, c.name+" (cached against an independent fresh read)", c.got, c.ref)
	}
	for _, c := range []struct {
		name           string
		got, want, ref rowTotalDTO
	}{
		{"queue_total", cached.QueueTotal, fresh.QueueTotal, controlSnap.QueueTotal},
		{"history_total", cached.HistoryTotal, fresh.HistoryTotal, controlSnap.HistoryTotal},
	} {
		if c.want.Count == nil || *c.want.Count == 0 {
			t.Fatalf("%s counted nothing in the fresh frame: the fixture proves nothing", c.name)
		}
		if c.got.Count == nil || *c.got.Count != *c.want.Count || *c.got.Count != *c.ref.Count {
			t.Errorf("%s: cached count %v, fresh %v, independent %v", c.name, c.got.Count, c.want.Count, c.ref.Count)
		}
		if c.got.Covers != c.want.Covers {
			t.Errorf("%s: cached covers %q, fresh %q", c.name, c.got.Covers, c.want.Covers)
		}
	}

	// The one field that MUST differ: the cached frame says the value is 9 seconds old.
	if cached.Aggregates.SizeRatio.AgeSeconds == nil || *cached.Aggregates.SizeRatio.AgeSeconds != 9 {
		t.Errorf("the cached frame does not state its age: %v", cached.Aggregates.SizeRatio.AgeSeconds)
	}
}

// --- helpers -----------------------------------------------------------------

// figureAges pulls the age out of every whole-ledger figure's OWN envelope in a frame,
// keyed by figure. A nil value is an explicit JSON null - "no value is being served" -
// and a missing key is a failure, because an unstated freshness is the defect.
func figureAges(t *testing.T, frame []byte) map[string]*int64 {
	t.Helper()
	var f struct {
		Aggregates   map[string]json.RawMessage `json:"aggregates"`
		QueueTotal   json.RawMessage            `json:"queue_total"`
		HistoryTotal json.RawMessage            `json:"history_total"`
	}
	if err := json.Unmarshal(frame, &f); err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	out := map[string]*int64{}
	take := func(name string, raw json.RawMessage) {
		if len(raw) == 0 {
			t.Fatalf("the frame carries no %s figure at all", name)
		}
		var env struct {
			AgeSeconds *int64 `json:"age_seconds"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("unmarshal %s: %v", name, err)
		}
		if !strings.Contains(string(raw), `"age_seconds"`) {
			t.Fatalf("%s carries no age_seconds key: its freshness is unstated", name)
		}
		out[name] = env.AgeSeconds
	}
	if len(f.Aggregates) != 6 {
		t.Fatalf("the frame carries %d aggregates, want 6", len(f.Aggregates))
	}
	for name, raw := range f.Aggregates {
		take(name, raw)
	}
	take("queue_total", f.QueueTotal)
	take("history_total", f.HistoryTotal)
	return out
}

func assertSameSpread(t *testing.T, what string, got, want spreadDTO) {
	t.Helper()
	got.AgeSeconds, want.AgeSeconds = nil, nil
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: a cached figure is not the figure a fresh read produces\n got %+v\nwant %+v", what, got, want)
	}
}

func assertSameBreakdown(t *testing.T, what string, got, want breakdownDTO) {
	t.Helper()
	got.AgeSeconds, want.AgeSeconds = nil, nil
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: a cached figure is not the figure a fresh read produces\n got %+v\nwant %+v", what, got, want)
	}
}

// captureWarnings points the hub's logger at a buffer and returns a reader for the lines
// it recorded, so a test can assert what a degraded refresh told the operator. The lines
// are structured records (O1), so they are captured as the JSON the daemon emits.
func captureWarnings(hub *Hub) func() []string {
	buf := &lockedBuffer{}
	hub.log = slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return func() []string {
		lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
		if len(lines) == 1 && lines[0] == "" {
			return nil
		}
		return lines
	}
}

// lockedBuffer is a bytes.Buffer a logger and a test may touch from different goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func anyContains(lines []string, want string) bool {
	for _, l := range lines {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}
