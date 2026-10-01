package health

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/store"
)

func record(t *testing.T, st *store.SQLite, sweep int64, path string, res store.HealthResult, reason string, at time.Time) {
	t.Helper()
	if err := st.RecordHealthCheck(context.Background(), store.HealthCheck{SweepID: sweep, Path: path, Size: 1000,
		MtimeNS: 1, CheckedAt: at, Result: res, Reason: reason}); err != nil {
		t.Fatal(err)
	}
}

// TestBuildReport_NoSweepYet: nothing recorded is null, not zero.
func TestBuildReport_NoSweepYet(t *testing.T) {
	st := openLedger(t, filepath.Join(t.TempDir(), "jobs.db"))
	defer func() { _ = st.Close() }()
	for _, tc := range []struct {
		interval time.Duration
		live     Live
		enabled  bool
		state    string
	}{
		{0, Live{State: StateOff}, false, "off"},
		{168 * time.Hour, Live{State: StateIdle}, true, "idle"},
	} {
		r, err := BuildReport(context.Background(), st, tc.interval, tc.live)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(r)
		want := fmt.Sprintf(`{"enabled":%v,"interval_hours":%d,"state":%q,"next_due_at":null,"current":null,"last_completed":null}`,
			tc.enabled, int(tc.interval/time.Hour), tc.state)
		if string(b) != want {
			t.Errorf("got  %s\nwant %s", b, want)
		}
	}
}

// TestBuildReport_ACurrentAndALastCompletedSweep: the sweep under way is counted from its
// checks, the finished one from its row, each carries its problems and nothing else, the
// next due time is withheld while a sweep is under way, and the waiting reason is shown.
func TestBuildReport_ACurrentAndALastCompletedSweep(t *testing.T) {
	ctx := context.Background()
	st := openLedger(t, filepath.Join(t.TempDir(), "jobs.db"))
	defer func() { _ = st.Close() }()
	t0 := time.Unix(1_790_000_000, 0)

	first, err := st.StartHealthSweep(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	record(t, st, first.ID, "/lib/a.mkv", store.HealthOK, "", t0)
	record(t, st, first.ID, "/lib/b.mkv", store.HealthCorrupt, "File ended prematurely", t0.Add(time.Minute))
	record(t, st, first.ID, "/lib/c.mkv", store.HealthUnreadable, "not a regular file", t0.Add(2*time.Minute))
	if err := st.FinishHealthSweep(ctx, first.ID, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	r, err := BuildReport(ctx, st, 24*time.Hour, Live{State: StateIdle})
	if err != nil {
		t.Fatal(err)
	}
	if r.Current != nil || r.LastCompleted == nil || r.NextDueAt == nil || *r.NextDueAt != t0.Add(25*time.Hour).Unix() {
		t.Fatalf("idle report %+v", r)
	}
	lc := r.LastCompleted
	if lc.ID != first.ID || lc.StartedAt != t0.Unix() || lc.FinishedAt == nil || *lc.FinishedAt != t0.Add(time.Hour).Unix() ||
		lc.Checked != 3 || lc.OK != 1 || lc.Corrupt != 1 || lc.Unreadable != 1 || lc.ProblemsTruncated {
		t.Errorf("last completed %+v", lc)
	}
	if len(lc.Problems) != 2 || lc.Problems[0] != (Problem{Path: "/lib/b.mkv", Result: "corrupt",
		Reason: "File ended prematurely", CheckedAt: t0.Add(time.Minute).Unix(), Size: 1000}) ||
		lc.Problems[1].Path != "/lib/c.mkv" || lc.Problems[1].Result != "unreadable" {
		t.Errorf("problems %+v", lc.Problems)
	}

	second, err := st.StartHealthSweep(ctx, t0.Add(25*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	record(t, st, second.ID, "/lib/a.mkv", store.HealthCorrupt, "Invalid data", t0.Add(25*time.Hour))
	record(t, st, second.ID, "/lib/d.mkv", store.HealthOK, "", t0.Add(25*time.Hour))
	r, err = BuildReport(ctx, st, 24*time.Hour, Live{State: StateWaiting, Why: "outside run window 01:00-05:00"})
	if err != nil {
		t.Fatal(err)
	}
	if r.State != "waiting" || r.Waiting != "outside run window 01:00-05:00" || r.NextDueAt != nil {
		t.Errorf("waiting report %+v", r)
	}
	c := r.Current
	if c == nil || c.ID != second.ID || c.FinishedAt != nil || c.Checked != 2 || c.OK != 1 || c.Corrupt != 1 ||
		len(c.Problems) != 1 || c.Problems[0].Path != "/lib/a.mkv" {
		t.Errorf("current %+v", c)
	}
	if r.LastCompleted == nil || r.LastCompleted.ID != first.ID {
		t.Errorf("last completed %+v", r.LastCompleted)
	}
	// The waiting reason is shown only while waiting.
	r, _ = BuildReport(ctx, st, 24*time.Hour, Live{State: StateRunning, Why: "stale"})
	if r.Waiting != "" {
		t.Errorf("a running sweep reports waiting %q", r.Waiting)
	}
}

// TestBuildReport_TheProblemListIsBounded: at MaxProblemsListed the list is cut and says so.
func TestBuildReport_TheProblemListIsBounded(t *testing.T) {
	ctx := context.Background()
	st := openLedger(t, filepath.Join(t.TempDir(), "jobs.db"))
	defer func() { _ = st.Close() }()
	t0 := time.Unix(1_790_000_000, 0)
	for _, n := range []int{MaxProblemsListed, MaxProblemsListed + 1} {
		sw, err := st.StartHealthSweep(ctx, t0)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < n; i++ {
			record(t, st, sw.ID, fmt.Sprintf("/lib/%04d.mkv", i), store.HealthCorrupt, "x", t0)
		}
		r, err := BuildReport(ctx, st, time.Hour, Live{State: StateRunning})
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Current.Problems) != MaxProblemsListed || r.Current.ProblemsTruncated != (n > MaxProblemsListed) ||
			r.Current.Corrupt != int64(n) {
			t.Errorf("%d problems: listed %d truncated=%v counted %d", n, len(r.Current.Problems),
				r.Current.ProblemsTruncated, r.Current.Corrupt)
		}
		if err := st.FinishHealthSweep(ctx, sw.ID, t0); err != nil {
			t.Fatal(err)
		}
	}
}

// TestLedger_AStartedSweepPrunesTheChecksOfEverySweepOlderThanTheLastFinished: the ledger
// keeps the sweep under way and the last finished one, and nothing older.
func TestLedger_AStartedSweepPrunesTheChecksOfEverySweepOlderThanTheLastFinished(t *testing.T) {
	ctx := context.Background()
	st := openLedger(t, filepath.Join(t.TempDir(), "jobs.db"))
	defer func() { _ = st.Close() }()
	t0 := time.Unix(1_790_000_000, 0)
	var ids []int64
	for i := 0; i < 3; i++ {
		sw, err := st.StartHealthSweep(ctx, t0)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, sw.ID)
		record(t, st, sw.ID, "/lib/a.mkv", store.HealthCorrupt, "x", t0)
		if err := st.FinishHealthSweep(ctx, sw.ID, t0); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.FinishHealthSweep(ctx, ids[2], t0); err == nil {
		t.Error("finishing a finished sweep again was accepted")
	}
	for i, want := range []int64{0, 1, 1} {
		c, _ := st.HealthCountsOf(ctx, ids[i])
		if c.Corrupt != want {
			t.Errorf("sweep %d keeps %d check(s), want %d", ids[i], c.Corrupt, want)
		}
	}
	last, _, _ := st.LastFinishedHealthSweep(ctx)
	if last.ID != ids[2] || last.Counts.Corrupt != 1 {
		t.Errorf("the last finished sweep is %+v", last)
	}
}
