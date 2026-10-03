package store

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// The lease ledger's reporting reads: a capped listing, newest grant first in a total
// order, live and ended alike, and a count of every row - neither of which writes anything.

func leaseKeys(ls []Lease) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = fmt.Sprintf("%s#%d", l.Path, l.Epoch)
	}
	return out
}

func TestRecentLeases_NewestGrantFirstThenPathThenEpochDescending(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	at := time.Unix(1800000000, 0)

	if got, err := s.RecentLeases(ctx, 10); err != nil || len(got) != 0 {
		t.Fatalf("an empty ledger listed %d leases, err %v", len(got), err)
	}
	if total := s.CountLeases(ctx); total.Err != nil || total.Count != 0 || total.Coverage.Set == "" {
		t.Fatalf("an empty ledger counted %+v", total)
	}

	// /lib/b is granted three times in ONE second (each ended before the next), and /lib/a
	// and /lib/c in that same second: only path and epoch order them.
	grant(t, s, "/lib/old.mkv", "l-old", at.Add(-time.Hour))
	grant(t, s, "/lib/c.mkv", "l-c", at)
	grant(t, s, "/lib/b.mkv", "l-b1", at)
	endLease(t, s, "l-b1", LeaseFailed, at)
	grant(t, s, "/lib/b.mkv", "l-b2", at)
	endLease(t, s, "l-b2", LeaseExpired, at)
	grant(t, s, "/lib/b.mkv", "l-b3", at)
	grant(t, s, "/lib/a.mkv", "l-a", at)
	grant(t, s, "/lib/new.mkv", "l-new", at.Add(time.Second))
	endLease(t, s, "l-new", LeaseCompleted, at.Add(2*time.Second))

	want := []string{"/lib/new.mkv#1", "/lib/a.mkv#1", "/lib/b.mkv#3", "/lib/b.mkv#2", "/lib/b.mkv#1", "/lib/c.mkv#1", "/lib/old.mkv#1"}
	all, err := s.RecentLeases(ctx, 100)
	if err != nil {
		t.Fatalf("RecentLeases: %v", err)
	}
	if got := leaseKeys(all); !reflect.DeepEqual(got, want) {
		t.Fatalf("listed\n %v\nwant\n %v", got, want)
	}
	// Live and ended alike, each row whole.
	states := map[string]LeaseState{}
	for _, l := range all {
		states[l.ID] = l.State
	}
	wantStates := map[string]LeaseState{"l-old": LeaseGranted, "l-c": LeaseGranted, "l-b1": LeaseFailed,
		"l-b2": LeaseExpired, "l-b3": LeaseGranted, "l-a": LeaseGranted, "l-new": LeaseCompleted}
	if !reflect.DeepEqual(states, wantStates) {
		t.Fatalf("states %v, want %v", states, wantStates)
	}
	read, ok, err := s.GetLease(ctx, "l-new")
	if err != nil || !ok || all[0] != read {
		t.Fatalf("the listed row is not the row GetLease reads:\n %+v\n %+v (ok=%v err=%v)", all[0], read, ok, err)
	}

	// The cap takes the newest, and the count is over every row whatever the cap.
	for limit := 1; limit <= len(want); limit++ {
		got, err := s.RecentLeases(ctx, limit)
		if err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if keys := leaseKeys(got); !reflect.DeepEqual(keys, want[:limit]) {
			t.Errorf("limit %d listed %v, want %v", limit, keys, want[:limit])
		}
	}
	total := s.CountLeases(ctx)
	if total.Err != nil || total.Count != int64(len(want)) {
		t.Fatalf("CountLeases = %+v, want %d", total, len(want))
	}
	if total.Coverage.Set != "every lease in the ledger" {
		t.Errorf("the total covers %q", total.Coverage.Set)
	}
}

func TestRecentLeases_RefusesAListingOfNoRows(t *testing.T) {
	s := openTest(t)
	grant(t, s, "/lib/a.mkv", "l-a", time.Unix(1800000000, 0))
	for _, limit := range []int{0, -1} {
		if got, err := s.RecentLeases(context.Background(), limit); err == nil || got != nil {
			t.Errorf("limit %d listed %d rows, err %v; want a refusal", limit, len(got), err)
		}
	}
	if got, err := s.RecentLeases(context.Background(), 1); err != nil || len(got) != 1 {
		t.Errorf("limit 1 listed %d rows, err %v", len(got), err)
	}
}

// A read that fails says so: the listing returns its error, and the count carries its own
// and never a zero that reads as "no lease".
func TestRecentLeases_AFailedReadIsReportedAndTheCountIsNeverAZeroTotal(t *testing.T) {
	s := openTest(t)
	grant(t, s, "/lib/a.mkv", "l-a", time.Unix(1800000000, 0))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := s.RecentLeases(ctx, 5); err == nil || got != nil {
		t.Errorf("a cancelled listing returned %d rows, err %v", len(got), err)
	}
	total := s.CountLeases(ctx)
	if total.Err == nil {
		t.Fatalf("a cancelled count reported no error: %+v", total)
	}
	if total.Count != 0 || total.Coverage.Set == "" {
		t.Errorf("a failed count carries %+v; it must still name its set", total)
	}
}

// Both reads work through the ledger's read-only door, which refuses every write, and
// neither changes a row.
func TestRecentLeases_ReadThroughTheReadOnlyDoorAndChangeNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	at := time.Unix(1800000000, 0)
	grant(t, s, "/lib/a.mkv", "l-a", at)
	grant(t, s, "/lib/b.mkv", "l-b", at.Add(time.Second))
	endLease(t, s, "l-b", LeaseFailed, at.Add(2*time.Second))

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	t.Cleanup(func() { _ = ro.Close() })
	ctx := context.Background()
	before, err := s.RecentLeases(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		got, err := ro.RecentLeases(ctx, 10)
		if err != nil {
			t.Fatalf("read %d through the read-only door: %v", i, err)
		}
		if !reflect.DeepEqual(got, before) {
			t.Fatalf("read %d differs:\n %+v\n %+v", i, got, before)
		}
		if total := ro.CountLeases(ctx); total.Err != nil || total.Count != 2 {
			t.Fatalf("count %d through the read-only door: %+v", i, total)
		}
	}
	after, err := s.RecentLeases(ctx, 10)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("the listing changed the ledger (err %v)", err)
	}
}
