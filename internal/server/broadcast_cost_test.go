package server

import (
	"context"
	"sync"
	"testing"

	"github.com/NSchatz/holdfast/internal/store"
)

// What a frame COSTS, and when it is paid (S0096).
//
// The hub rebuilt a full store-derived snapshot on every broadcast, at one broadcast per
// second per running encode, whether or not anybody was subscribed - roughly ten queries,
// several of them scans of the whole jobs table, on the single serialized connection the
// engine writes every job transition through. The tests here grade WHEN a figure is
// computed. Not one of them asserts anything about what a figure SAYS: that is
// TestSnapshot_CachedFigureEqualsAFreshOne's subject and the existing aggregate suites'.

// countingStore counts the store calls a frame makes. It stands in for the store, which is
// OUTSIDE the hub's boundary (testing T3) - the hub itself runs for real - and it is the
// only way "issues no whole-ledger query" is observable at all: the query either happened
// or it did not, and nothing in the published frame says which.
//
// Every method returns a usable value rather than a zero, because the assertions are about
// the CALLS and a frame that failed to build would stop making them for the wrong reason.
type countingStore struct {
	store.Store // every method these tests do not exercise; a call to one panics, loudly

	mu sync.Mutex

	summary    int
	list       int
	countRows  int
	aggregates int
	held       int
	reclaimed  int

	// aggErr, when set, is the error EVERY aggregate reports on its own figure - the
	// store's own per-figure failure discipline, which is how a refresh fails without
	// failing the frame.
	aggErr error
	// countErr does the same for the two row totals.
	countErr error
}

func (c *countingStore) Summary(context.Context) (map[store.Status]int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.summary++
	return map[store.Status]int{store.Done: 2, store.Pending: 1}, nil
}

func (c *countingStore) List(context.Context, []store.Status, int) ([]store.Job, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.list++
	return nil, nil
}

func (c *countingStore) CountRows(_ context.Context, _ []store.Status) store.RowTotal {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.countRows++
	if c.countErr != nil {
		return store.RowTotal{Coverage: store.Coverage{Set: "every matching row in the ledger"}, Err: c.countErr}
	}
	return store.RowTotal{Coverage: store.Coverage{Set: "every matching row in the ledger"}, Count: 7}
}

func (c *countingStore) HeldByUndoWindow(context.Context) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held++
	return 0, nil
}

func (c *countingStore) ReclaimedTotal(context.Context) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reclaimed++
	return 0, nil
}

func (c *countingStore) Aggregates(context.Context) store.Aggregates {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.aggregates++
	mk := func(set string) store.Breakdown {
		b := store.Breakdown{Coverage: store.Coverage{Set: set}, Err: c.aggErr}
		if c.aggErr == nil {
			b.Buckets = []store.Bucket{{Key: "done", Count: 3}}
			b.Counted, b.Excluded = 3, 1
		}
		return b
	}
	sp := func(set string, at float64) store.Spread {
		s := store.Spread{Coverage: store.Coverage{Set: set}, Err: c.aggErr}
		if c.aggErr == nil {
			v := at
			s.Min, s.Mean, s.Max = &v, &v, &v
			s.Counted, s.Excluded = 4, 2
		}
		return s
	}
	return store.Aggregates{
		Outcomes:     mk("every terminal row in the ledger"),
		SkipsByGuard: mk("every skipped row in the ledger"),
		SizeRatio:    sp("every done row in the ledger", 0.35),
		EncodeMs:     sp("every done row in the ledger", 30000),
		VmafMean:     sp("every done row in the ledger", 96.5),
		VmafMin:      sp("every done row in the ledger", 91.5),
	}
}

// reads is every store call this double has counted: the whole cost of a frame.
func (c *countingStore) reads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.summary + c.list + c.countRows + c.aggregates + c.held
}

// wholeLedgerReads is the cost the refresh interval bounds: the six aggregates (one
// Aggregates call) and the two row totals. Summary and the two capped List reads are
// deliberately NOT here - they are what a progress frame is FOR and they stay on every
// frame.
func (c *countingStore) wholeLedgerReads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.aggregates + c.countRows
}

func (c *countingStore) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.summary, c.list, c.countRows, c.aggregates, c.held, c.reclaimed = 0, 0, 0, 0, 0, 0
}

// countingHub builds a real Hub over the counting double, with the baseline read the
// constructor makes already discounted.
func countingHub(t *testing.T) (*Hub, *countingStore) {
	t.Helper()
	cs := &countingStore{}
	ctrl := NewController(context.Background(), nil, discard())
	hub := NewHub(cs, ctrl, discard())
	cs.reset()
	return hub, cs
}

// [AC-1] WHEN the hub broadcasts and no subscriber is registered THE SYSTEM SHALL return
// without building a snapshot and without issuing any store query.
//
// The number that matters is ZERO, and it is zero over EVERY store call rather than only
// the expensive ones: a frame nobody receives is a frame nobody needed, and the cheapest
// query on it still queues on the connection the engine's writes go through.
func TestBroadcast_BuildsNothingWithNoSubscribers(t *testing.T) {
	hub, cs := countingHub(t)

	for i := 0; i < 25; i++ {
		hub.broadcast(context.Background())
	}

	if got := cs.reads(); got != 0 {
		t.Fatalf("an unsubscribed broadcast issued %d store reads, want 0 "+
			"(summary=%d list=%d count_rows=%d aggregates=%d held=%d)",
			got, cs.summary, cs.list, cs.countRows, cs.aggregates, cs.held)
	}
}

// [AC-1] The zero-subscriber return must be the SUBSCRIBER COUNT and not some other
// quiescence: with a subscriber attached, the same broadcast reads the store.
//
// Without this the criterion is passable by a hub that never broadcasts at all.
func TestBroadcast_BuildsNothingWithNoSubscribers_StillBuildsForASubscriber(t *testing.T) {
	hub, cs := countingHub(t)

	_, cancel := hub.Subscribe(context.Background())
	defer cancel()
	cs.reset() // the subscription's own first frame is AC-2's subject, not this one

	hub.broadcast(context.Background())

	if cs.summary == 0 || cs.list == 0 {
		t.Fatalf("a broadcast with one subscriber read nothing: summary=%d list=%d", cs.summary, cs.list)
	}
}

// [AC-2] WHEN a subscriber attaches THE SYSTEM SHALL build that subscriber's first frame
// on demand at that moment, so subscribing after an idle period is never served a frame
// built before the subscription existed.
//
// "At that moment" is graded two ways, because either alone is passable by the wrong
// implementation: the store is read DURING Subscribe (a cached frame reads nothing), and
// the frame is WAITING on the returned channel when Subscribe comes back (a hub that read
// the store and threw the result away would pass the first check alone).
func TestSubscribe_InitialFrameIsBuiltOnDemand(t *testing.T) {
	hub, cs := countingHub(t)

	// An idle stretch first: nobody is watching, so nothing has been built and there is
	// no frame anywhere for a new subscriber to be handed.
	for i := 0; i < 10; i++ {
		hub.broadcast(context.Background())
	}
	if got := cs.reads(); got != 0 {
		t.Fatalf("the idle stretch was not idle: %d store reads", got)
	}

	ch, cancel := hub.Subscribe(context.Background())
	defer cancel()

	if cs.summary == 0 {
		t.Error("Subscribe read no summary: the first frame was not built at subscribe time")
	}
	if cs.aggregates == 0 {
		t.Error("Subscribe read no aggregates: the first frame was not built at subscribe time")
	}
	select {
	case data := <-ch:
		if len(data) == 0 {
			t.Fatal("the seeded first frame is empty")
		}
	default:
		t.Fatal("Subscribe returned with no frame waiting: a client attaching after an idle " +
			"period would render nothing until the engine next transitioned")
	}
}

// [AC-2] The unhappy path: a store the first frame cannot be read from must still open the
// stream. A subscription that failed because a figure could not be read would cost a
// client every LATER frame too, which is the one thing the per-figure failure discipline
// exists to prevent.
func TestSubscribe_InitialFrameIsBuiltOnDemand_SurvivesAFailedBuild(t *testing.T) {
	hub, _ := countingHub(t)
	hub.store = failingSummaryStore{&countingStore{}}

	ch, cancel := hub.Subscribe(context.Background())
	defer cancel()

	select {
	case <-ch:
		t.Fatal("a frame arrived from a store whose summary cannot be read")
	default:
	}

	// The subscription is live: a later broadcast, over a store that reads, reaches it.
	hub.store = &countingStore{}
	hub.broadcast(context.Background())
	select {
	case data := <-ch:
		if len(data) == 0 {
			t.Fatal("the later frame is empty")
		}
	default:
		t.Fatal("the subscription was not registered: a failed first build closed the stream")
	}
}

// failingSummaryStore fails the one read buildSnapshot cannot survive, and reads
// everything else normally - so a frame that fails here failed for the reason the test
// names and not because the double is thin.
type failingSummaryStore struct{ *countingStore }

func (failingSummaryStore) Summary(context.Context) (map[store.Status]int, error) {
	return nil, errSummaryUnreadable
}

var errSummaryUnreadable = errStatic("simulated: the summary could not be read")

type errStatic string

func (e errStatic) Error() string { return string(e) }
