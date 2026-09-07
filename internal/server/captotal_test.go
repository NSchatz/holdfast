package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/store"
)

// The total a capped response was capped AGAINST (LEDGER-5).
//
// Criterion 3: WHEN a response caps the rows it returns THE SYSTEM SHALL report the total
// it capped against, rather than leaving a client to derive it.
//
// Criterion 14: WHEN a caller asks a capped endpoint for fewer rows than its cap THE
// SYSTEM SHALL report the same total it reports at the cap, that total being the count of
// matching rows in the ledger and never the number of rows returned.
//
// Criterion 15: IF the total behind a cap cannot be read THEN THE SYSTEM SHALL still
// return the rows and state that the total is unavailable, rather than report a total of
// zero.
//
// A total that could be derived from the payload would not be worth a field. These
// fixtures therefore always seed the ledger OVER the caps, so the count and the number of
// rows returned are different numbers and a wrong implementation cannot coincide with a
// right one.

// countFailingStore makes exactly the row-count read fail, and nothing else. Every other
// method is the real SQLite store, so the rows the response ships are genuinely read from
// a database - which is the whole of what criterion 15 asserts survives.
type countFailingStore struct {
	*store.SQLite
}

func (countFailingStore) CountRows(_ context.Context, statuses []store.Status) store.RowTotal {
	return store.RowTotal{
		Coverage: store.Coverage{Set: "every row in the ledger"},
		Err:      errors.New("simulated: the ledger could not be counted"),
	}
}

// rowTotalWire is the field as it arrives, decoded loosely so a test can tell an explicit
// null from a zero - which is the distinction the whole shape exists for.
type rowTotalWire struct {
	Available   bool   `json:"available"`
	Unavailable string `json:"unavailable"`
	Covers      string `json:"covers"`
	Cap         int    `json:"cap"`
	Count       *int64 `json:"count"`
}

func requireAvailable(t *testing.T, what string, got rowTotalWire, wantCount int64, wantCap int) {
	t.Helper()
	if !got.Available {
		t.Errorf("%s reports itself unavailable: %+v", what, got)
		return
	}
	if got.Count == nil {
		t.Errorf("%s carries a null count while reporting itself available", what)
		return
	}
	if *got.Count != wantCount {
		t.Errorf("%s reports %d, want %d - the count is over the matching rows in the ledger, "+
			"never over the rows returned", what, *got.Count, wantCount)
	}
	if got.Cap != wantCap {
		t.Errorf("%s reports a cap of %d, want %d", what, got.Cap, wantCap)
	}
	if got.Covers == "" {
		t.Errorf("%s states no set; a number whose set is unstated is read as covering everything", what)
	}
	if got.Unavailable != "" {
		t.Errorf("%s is available and still carries an unavailability statement: %q", what, got.Unavailable)
	}
}

// --- criterion 3: every capped response reports its total -------------------------------

func TestCapTotal_EveryCappedResponseReportsTheTotalItCappedAgainst(t *testing.T) {
	h := newHarness(t, "")
	done, skipped, queued := seedOverCap(t, h.st)
	// newStore already seeded one active and one done row before seedOverCap ran.
	wantQueue := int64(queued + 1)
	wantHistory := int64(done + skipped + 1)

	ts := httptest.NewServer(h.srv)
	defer ts.Close()

	t.Run("GET /api/queue", func(t *testing.T) {
		var got struct {
			Queue      []jobDTO     `json:"queue"`
			QueueTotal rowTotalWire `json:"queue_total"`
		}
		getJSON(t, ts.URL+"/api/queue", &got)
		if len(got.Queue) != queueLimit {
			t.Fatalf("the response carries %d rows, want the cap of %d", len(got.Queue), queueLimit)
		}
		requireAvailable(t, "queue_total", got.QueueTotal, wantQueue, queueLimit)
		if int64(len(got.Queue)) == wantQueue {
			t.Fatal("the fixture does not cap anything, so this test could not tell a reported total " +
				"from the number of rows returned")
		}
	})

	t.Run("GET /api/history", func(t *testing.T) {
		var got struct {
			History      []jobDTO     `json:"history"`
			HistoryTotal rowTotalWire `json:"history_total"`
		}
		getJSON(t, ts.URL+"/api/history", &got)
		if len(got.History) != historyLimit {
			t.Fatalf("the response carries %d rows, want the cap of %d", len(got.History), historyLimit)
		}
		requireAvailable(t, "history_total", got.HistoryTotal, wantHistory, historyLimit)
	})

	t.Run("the SSE snapshot", func(t *testing.T) {
		// The stream and the read endpoints must not disagree about the ledger: a client
		// that polls sees exactly what a client that subscribes sees.
		snap := snapshotOf(t, h.hub)
		if len(snap.Queue) != queueLimit || len(snap.History) != historyLimit {
			t.Fatalf("the snapshot carries %d queue and %d history rows, want the caps",
				len(snap.Queue), len(snap.History))
		}
		if snap.QueueTotal.Count == nil || *snap.QueueTotal.Count != wantQueue {
			t.Errorf("the snapshot's queue_total is %v, want %d", snap.QueueTotal.Count, wantQueue)
		}
		if snap.HistoryTotal.Count == nil || *snap.HistoryTotal.Count != wantHistory {
			t.Errorf("the snapshot's history_total is %v, want %d", snap.HistoryTotal.Count, wantHistory)
		}
		if !snap.QueueTotal.Available || !snap.HistoryTotal.Available {
			t.Error("the snapshot's totals report themselves unavailable over a readable store")
		}
	})
}

func TestCapTotal_TheFieldNamesAreTheSameOnEveryResponseThatCarriesThem(t *testing.T) {
	// The names freeze at the first published tag, so the same table's total must be the
	// same key wherever it is published. This reads the RAW bytes: decoding into a struct
	// would erase exactly the difference it is looking for.
	h := newHarness(t, "")
	seedOverCap(t, h.st)
	ts := httptest.NewServer(h.srv)
	defer ts.Close()

	queueBody := getRaw(t, ts.URL+"/api/queue")
	historyBody := getRaw(t, ts.URL+"/api/history")
	snapBytes, err := h.hub.SnapshotJSON(context.Background())
	if err != nil {
		t.Fatalf("SnapshotJSON: %v", err)
	}
	snapBody := string(snapBytes)

	for _, c := range []struct{ what, body, key string }{
		{"GET /api/queue", queueBody, `"queue_total"`},
		{"GET /api/history", historyBody, `"history_total"`},
		{"the SSE snapshot", snapBody, `"queue_total"`},
		{"the SSE snapshot", snapBody, `"history_total"`},
	} {
		if !strings.Contains(c.body, c.key) {
			t.Errorf("%s does not carry %s", c.what, c.key)
		}
	}
	// And the envelope is the same shape everywhere, so a client writes one reader.
	for _, key := range []string{`"available"`, `"unavailable"`, `"covers"`, `"cap"`, `"count"`} {
		for _, c := range []struct{ what, body string }{
			{"GET /api/queue", queueBody}, {"GET /api/history", historyBody}, {"the SSE snapshot", snapBody},
		} {
			if !strings.Contains(c.body, key) {
				t.Errorf("%s does not carry %s inside its total", c.what, key)
			}
		}
	}
}

// --- criterion 14: below the cap, the same total ----------------------------------------

func TestCapTotal_AskingForFewerRowsThanTheCapReportsTheSameTotal(t *testing.T) {
	h := newHarness(t, "")
	done, skipped, _ := seedOverCap(t, h.st)
	want := int64(done + skipped + 1)

	ts := httptest.NewServer(h.srv)
	defer ts.Close()

	type historyResp struct {
		History      []jobDTO     `json:"history"`
		HistoryTotal rowTotalWire `json:"history_total"`
	}
	var atCap historyResp
	getJSON(t, ts.URL+"/api/history", &atCap)
	requireAvailable(t, "history_total at the cap", atCap.HistoryTotal, want, historyLimit)

	for _, limit := range []int{1, 5, 199, historyLimit} {
		var got historyResp
		getJSON(t, ts.URL+"/api/history?limit="+strconv.Itoa(limit), &got)
		if len(got.History) != limit {
			t.Fatalf("?limit=%d returned %d rows", limit, len(got.History))
		}
		requireAvailable(t, "history_total at ?limit="+strconv.Itoa(limit), got.HistoryTotal, want, limit)
		if got.HistoryTotal.Count == nil || *got.HistoryTotal.Count != *atCap.HistoryTotal.Count {
			t.Errorf("?limit=%d reports a total of %v while the cap reports %v; the total is a fact about "+
				"the ledger and does not move with the request", limit, got.HistoryTotal.Count, atCap.HistoryTotal.Count)
		}
		if got.HistoryTotal.Count != nil && *got.HistoryTotal.Count == int64(len(got.History)) {
			t.Errorf("?limit=%d reports a total equal to the rows it returned (%d), which is the payload's "+
				"own length rather than the ledger's count", limit, len(got.History))
		}
	}
}

func TestCapTotal_AnEmptyLedgerReportsARealZeroAndNotAnUnreadableOne(t *testing.T) {
	// A genuine zero is a real answer and must stay distinguishable from an unreadable
	// figure - which is the other half of the discipline criterion 15 sets.
	st, err := store.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctrl := NewController(context.Background(), func(context.Context) error { return nil }, discard())
	hub := NewHub(st, ctrl, discard())

	snap := snapshotOf(t, hub)
	for _, c := range []struct {
		what  string
		total rowTotalDTO
	}{{"queue_total", snap.QueueTotal}, {"history_total", snap.HistoryTotal}} {
		if !c.total.Available {
			t.Errorf("%s over an empty but readable ledger reports itself unavailable", c.what)
		}
		if c.total.Count == nil || *c.total.Count != 0 {
			t.Errorf("%s over an empty ledger is %v, want a counted 0", c.what, c.total.Count)
		}
	}
}

// --- criterion 15: an unreadable total costs the rows nothing ---------------------------

func TestCapTotal_AnUnreadableTotalStillReturnsTheRowsAndIsNeverReportedAsZero(t *testing.T) {
	h := newHarness(t, "")
	seedOverCap(t, h.st)

	// Rewire the server and hub onto a store whose ONLY broken read is the count.
	broken := countFailingStore{SQLite: h.st}
	ctrl := NewController(context.Background(), func(context.Context) error { return nil }, discard())
	hub := NewHub(broken, ctrl, discard())
	srv := New(context.Background(), configZero(), broken, ctrl, hub, nil, nil, discard())
	ts := httptest.NewServer(srv)
	defer ts.Close()

	t.Run("GET /api/queue", func(t *testing.T) {
		var got struct {
			Queue      []jobDTO     `json:"queue"`
			QueueTotal rowTotalWire `json:"queue_total"`
		}
		getJSON(t, ts.URL+"/api/queue", &got)
		if len(got.Queue) != queueLimit {
			t.Errorf("an unreadable total cost the caller its rows: %d returned, want %d", len(got.Queue), queueLimit)
		}
		requireUnavailable(t, "queue_total", got.QueueTotal)
	})

	t.Run("GET /api/history", func(t *testing.T) {
		var got struct {
			History      []jobDTO     `json:"history"`
			HistoryTotal rowTotalWire `json:"history_total"`
		}
		getJSON(t, ts.URL+"/api/history", &got)
		if len(got.History) != historyLimit {
			t.Errorf("an unreadable total cost the caller its rows: %d returned, want %d", len(got.History), historyLimit)
		}
		requireUnavailable(t, "history_total", got.HistoryTotal)
	})

	t.Run("the SSE snapshot still ships", func(t *testing.T) {
		data, err := hub.SnapshotJSON(context.Background())
		if err != nil {
			t.Fatalf("an unreadable total failed the whole snapshot: %v", err)
		}
		var snap snapshot
		if err := json.Unmarshal(data, &snap); err != nil {
			t.Fatalf("unmarshal snapshot: %v", err)
		}
		if len(snap.Queue) != queueLimit || len(snap.History) != historyLimit {
			t.Errorf("an unreadable total cost the snapshot its rows: %d queue, %d history",
				len(snap.Queue), len(snap.History))
		}
		if snap.QueueTotal.Available || snap.HistoryTotal.Available {
			t.Error("a total that could not be read reports itself available")
		}
		if snap.QueueTotal.Count != nil || snap.HistoryTotal.Count != nil {
			t.Errorf("an unreadable total carries a number: queue=%v history=%v",
				snap.QueueTotal.Count, snap.HistoryTotal.Count)
		}
		// Asserted on the RAW bytes: decoding a `0` into an *int64 and a `null` into an
		// *int64 are distinguishable, but only because the field is a pointer. This is
		// the assertion that reds if anyone ever "simplifies" it to an int.
		if !strings.Contains(string(data), `"count":null`) {
			t.Errorf("an unreadable total does not serialize its count as an explicit null: %s", data)
		}
		if strings.Contains(string(data), `"count":0`) {
			t.Errorf("an unreadable total serialized a count of 0, which claims the ledger is empty: %s", data)
		}
	})
}

func requireUnavailable(t *testing.T, what string, got rowTotalWire) {
	t.Helper()
	if got.Available {
		t.Errorf("%s reports itself available over a store that could not be counted", what)
	}
	if got.Count != nil {
		t.Errorf("%s reports a total of %d where the figure could not be read; "+
			"a zero here claims the ledger is empty beside rows the caller can see", what, *got.Count)
	}
	if got.Unavailable == "" {
		t.Errorf("%s does not state that the figure is unavailable", what)
	}
	if got.Unavailable != aggregateUnavailable {
		t.Errorf("%s states %q; an unauthenticated read surface ships the fixed text and logs the real "+
			"error, so a driver message can never leak here", what, got.Unavailable)
	}
}

// configZero is an empty config: no auth token, which disables the mutating endpoints and
// is irrelevant to every read asserted here.
func configZero() config.Config { return config.Config{} }
