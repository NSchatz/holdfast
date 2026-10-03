package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func leaseAt(id string, at time.Time) Lease {
	return Lease{
		ID: id, Key: "fp-" + id, Node: "node-a", State: LeaseGranted,
		ExpiresAt: at.Add(time.Minute), Temp: "/lib/a.__transcoding__.mkv.holdfast-part",
		ReservedBytes: 1000, ArgsDigest: "sha-256=:args:", SourceSize: 2000,
		SourceModTime: time.Unix(1700000000, 123456789), GrantedAt: at, UpdatedAt: at,
	}
}

func grant(t *testing.T, s *SQLite, path, id string, at time.Time) Lease {
	t.Helper()
	l, err := s.GrantLease(context.Background(), path, func([]Lease, int64) (Lease, error) {
		return leaseAt(id, at), nil
	})
	if err != nil {
		t.Fatalf("GrantLease(%s, %s): %v", path, id, err)
	}
	return l
}

func endLease(t *testing.T, s *SQLite, id string, to LeaseState, at time.Time) Lease {
	t.Helper()
	l, err := s.UpdateLease(context.Background(), id, func(cur Lease) (Lease, error) {
		cur.State, cur.EndedAt, cur.UpdatedAt, cur.Reason = to, at, at, "ended by the test"
		return cur, nil
	})
	if err != nil {
		t.Fatalf("UpdateLease(%s): %v", id, err)
	}
	return l
}

// TestLease_AGrantRoundTripsEveryColumnAndNullsWhatIsNotRecorded: the row a grant writes
// reads back field for field, and the figures nothing recorded yet read back empty rather
// than as a recorded zero.
func TestLease_AGrantRoundTripsEveryColumnAndNullsWhatIsNotRecorded(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	at := time.Unix(1800000000, 0)
	got := grant(t, s, "/lib/a.mkv", "lease-1", at)
	want := leaseAt("lease-1", at)
	want.Path, want.Epoch = "/lib/a.mkv", 1
	if got != want {
		t.Fatalf("GrantLease returned\n %+v\nwant\n %+v", got, want)
	}
	read, ok, err := s.GetLease(ctx, "lease-1")
	if err != nil || !ok {
		t.Fatalf("GetLease: ok=%v err=%v", ok, err)
	}
	if read != want {
		t.Fatalf("GetLease returned\n %+v\nwant\n %+v", read, want)
	}
	var nulls int
	if err := s.db.QueryRow(`SELECT (output_digest IS NULL) + (output_bytes IS NULL) + (source_digest IS NULL)
		+ (ended_at IS NULL) FROM node_leases WHERE id = 'lease-1'`).Scan(&nulls); err != nil {
		t.Fatal(err)
	}
	if nulls != 4 {
		t.Errorf("%d of the four not-yet-recorded columns are NULL, want all 4", nulls)
	}
	var stamp int
	if err := s.db.QueryRow(`SELECT schema_version FROM node_leases WHERE id = 'lease-1'`).Scan(&stamp); err != nil {
		t.Fatal(err)
	}
	if stamp != schemaVersion() {
		t.Errorf("the row is stamped %d, want this build's schema version %d", stamp, schemaVersion())
	}
	if _, ok, err := s.GetLease(ctx, "no-such-lease"); err != nil || ok {
		t.Errorf("GetLease of an unknown id: ok=%v err=%v, want absent and no error", ok, err)
	}
}

// TestLease_EpochRisesByOneAtEveryGrantOfTheSamePath: the fencing token is assigned by the
// ledger inside the grant transaction, from the rows it keeps - terminal ones included -
// and is per path. Whatever the decision put in Path or Epoch is overwritten.
func TestLease_EpochRisesByOneAtEveryGrantOfTheSamePath(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	at := time.Unix(1800000000, 0)

	var sawLast []int64
	var sawLive []int
	grantSeeing := func(path, id string) Lease {
		l, err := s.GrantLease(ctx, path, func(live []Lease, last int64) (Lease, error) {
			sawLast, sawLive = append(sawLast, last), append(sawLive, len(live))
			l := leaseAt(id, at)
			l.Path, l.Epoch = "/somewhere/else.mkv", 99
			return l, nil
		})
		if err != nil {
			t.Fatalf("GrantLease(%s): %v", id, err)
		}
		return l
	}
	first := grantSeeing("/lib/a.mkv", "a1")
	if first.Epoch != 1 || first.Path != "/lib/a.mkv" {
		t.Fatalf("the first grant carries epoch %d path %q, want 1 and the granted path", first.Epoch, first.Path)
	}
	endLease(t, s, "a1", LeaseExpired, at)
	second := grantSeeing("/lib/a.mkv", "a2")
	other := grantSeeing("/lib/b.mkv", "b1")
	endLease(t, s, "a2", LeaseFailed, at)
	third := grantSeeing("/lib/a.mkv", "a3")
	if second.Epoch != 2 || third.Epoch != 3 || other.Epoch != 1 {
		t.Errorf("epochs: a2=%d a3=%d b1=%d, want 2, 3 and 1", second.Epoch, third.Epoch, other.Epoch)
	}
	if want := []int64{0, 1, 0, 2}; !equalInt64s(sawLast, want) {
		t.Errorf("the decisions saw last epochs %v, want %v", sawLast, want)
	}
	// Live rows of EVERY path are handed to the decision: none, none (a1 ended), a2, b1.
	if want := []int{0, 0, 1, 1}; !equalInts(sawLive, want) {
		t.Errorf("the decisions saw %v live leases, want %v", sawLive, want)
	}
	// The UNIQUE (path, epoch) constraint is the schema's own statement of the rule.
	if _, err := s.db.Exec(`INSERT INTO node_leases (id, path, job_key, node, epoch, state, expires_at, temp_path,
		reserved_bytes, args_digest, source_size, source_mtime_ns, upload_attempts, reason, granted_at, updated_at)
		VALUES ('dup', '/lib/a.mkv', 'k', 'n', 3, 'granted', 0, 't', 0, 'd', 0, 0, 0, '', 0, 0)`); err == nil {
		t.Error("a second row at an epoch the path already carries was accepted")
	}
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestLease_ARefusedDecisionWritesNothing: an error from the decision is returned as it
// is, and neither a grant nor an update leaves a trace.
func TestLease_ARefusedDecisionWritesNothing(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	at := time.Unix(1800000000, 0)
	refused := errors.New("refused by the decision")

	if _, err := s.GrantLease(ctx, "/lib/a.mkv", func([]Lease, int64) (Lease, error) {
		return leaseAt("never", at), refused
	}); !errors.Is(err, refused) {
		t.Fatalf("GrantLease returned %v, want the decision's own error", err)
	}
	if _, ok, _ := s.GetLease(ctx, "never"); ok {
		t.Fatal("a refused grant wrote a row")
	}
	if _, err := s.GrantLease(ctx, "/lib/a.mkv", func([]Lease, int64) (Lease, error) {
		return Lease{State: LeaseGranted}, nil
	}); err == nil {
		t.Error("a grant with no id was written")
	}
	if _, err := s.GrantLease(ctx, "/lib/a.mkv", func([]Lease, int64) (Lease, error) {
		l := leaseAt("bad-state", at)
		l.State = "leased"
		return l, nil
	}); err == nil {
		t.Error("a grant in a state outside the vocabulary was written")
	}

	before := grant(t, s, "/lib/a.mkv", "kept", at)
	got, err := s.UpdateLease(ctx, "kept", func(cur Lease) (Lease, error) {
		cur.State, cur.Reason = LeaseFailed, "should not be written"
		return cur, refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("UpdateLease returned %v, want the decision's own error", err)
	}
	if got != before {
		t.Errorf("a refused update returned %+v, want the row as it was read %+v", got, before)
	}
	if read, _, _ := s.GetLease(ctx, "kept"); read != before {
		t.Errorf("a refused update changed the row: %+v", read)
	}
	if _, err := s.UpdateLease(ctx, "kept", func(cur Lease) (Lease, error) {
		cur.State = "leased"
		return cur, nil
	}); err == nil {
		t.Error("an update to a state outside the vocabulary was written")
	}
	if _, err := s.UpdateLease(ctx, "no-such-lease", func(cur Lease) (Lease, error) {
		t.Error("the decision ran for a lease that does not exist")
		return cur, nil
	}); !errors.Is(err, ErrNoLease) {
		t.Errorf("UpdateLease of an unknown id returned %v, want ErrNoLease", err)
	}
}

// TestLease_AnUpdateWritesTheDecisionAndNeverTheGrantsOwnColumns: what a decision returns
// is what the row then holds, except the columns that are the grant's.
func TestLease_AnUpdateWritesTheDecisionAndNeverTheGrantsOwnColumns(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	at := time.Unix(1800000000, 0)
	later := at.Add(90 * time.Second)
	grant(t, s, "/lib/a.mkv", "lease-1", at)

	got, err := s.UpdateLease(ctx, "lease-1", func(cur Lease) (Lease, error) {
		cur.ID, cur.Path, cur.Key, cur.Node, cur.Epoch = "other", "/lib/other.mkv", "other-key", "other-node", 7
		cur.GrantedAt = later
		cur.State, cur.ExpiresAt, cur.Temp = LeaseCompleted, later, "/lib/renamed.part"
		cur.ReservedBytes, cur.ArgsDigest, cur.SourceSize = 5, "sha-256=:other:", 6
		cur.SourceModTime = time.Unix(1700000001, 5)
		cur.OutputDigest, cur.OutputBytes, cur.SourceDigest = "sha-256=:out:", 1234, "sha-256=:src:"
		cur.UploadAttempts, cur.Reason, cur.UpdatedAt, cur.EndedAt = 2, "because", later, later
		return cur, nil
	})
	if err != nil {
		t.Fatalf("UpdateLease: %v", err)
	}
	want := Lease{
		ID: "lease-1", Path: "/lib/a.mkv", Key: "fp-lease-1", Node: "node-a", Epoch: 1, GrantedAt: at,
		State: LeaseCompleted, ExpiresAt: later, Temp: "/lib/renamed.part", ReservedBytes: 5,
		ArgsDigest: "sha-256=:other:", SourceSize: 6, SourceModTime: time.Unix(1700000001, 5),
		OutputDigest: "sha-256=:out:", OutputBytes: 1234, SourceDigest: "sha-256=:src:",
		UploadAttempts: 2, Reason: "because", UpdatedAt: later, EndedAt: later,
	}
	if got != want {
		t.Errorf("UpdateLease returned\n %+v\nwant\n %+v", got, want)
	}
	if read, _, _ := s.GetLease(ctx, "lease-1"); read != want {
		t.Errorf("the row reads back\n %+v\nwant\n %+v", read, want)
	}
	if _, ok, _ := s.GetLease(ctx, "other"); ok {
		t.Error("an update moved the row to another id")
	}
}

// TestLease_LiveLeasesListsGrantedAndUploadedOnlyOldestFirst.
func TestLease_LiveLeasesListsGrantedAndUploadedOnlyOldestFirst(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	at := time.Unix(1800000000, 0)
	grant(t, s, "/lib/c.mkv", "c", at.Add(2*time.Second))
	grant(t, s, "/lib/a.mkv", "a", at)
	grant(t, s, "/lib/b.mkv", "b", at.Add(time.Second))
	for id, st := range map[string]LeaseState{"d": LeaseCompleted, "e": LeaseFailed, "f": LeaseExpired} {
		grant(t, s, "/lib/"+id+".mkv", id, at)
		endLease(t, s, id, st, at)
	}
	if _, err := s.UpdateLease(ctx, "b", func(cur Lease) (Lease, error) {
		cur.State = LeaseUploaded
		return cur, nil
	}); err != nil {
		t.Fatal(err)
	}
	live, err := s.LiveLeases(ctx)
	if err != nil {
		t.Fatalf("LiveLeases: %v", err)
	}
	var ids []string
	for _, l := range live {
		ids = append(ids, l.ID)
	}
	if len(ids) != 3 || ids[0] != "a" || ids[1] != "b" || ids[2] != "c" {
		t.Errorf("LiveLeases returned %v, want [a b c]", ids)
	}
	for st, want := range map[LeaseState]bool{LeaseGranted: true, LeaseUploaded: true,
		LeaseCompleted: false, LeaseFailed: false, LeaseExpired: false, "leased": false} {
		if st.Live() != want {
			t.Errorf("%q.Live() = %v, want %v", st, st.Live(), want)
		}
	}
	if LeaseState("leased").Valid() || !LeaseExpired.Valid() || !LeaseGranted.Valid() {
		t.Error("Valid does not describe the closed vocabulary")
	}
}

// TestLease_PruneKeepsLiveRowsYoungRowsAndTheNewestRowOfEveryPath: the bounded rule. What
// goes is a terminal row that ended before the cut and is not its path's newest.
func TestLease_PruneKeepsLiveRowsYoungRowsAndTheNewestRowOfEveryPath(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	old := time.Unix(1800000000, 0)
	cut := old.Add(time.Hour)
	young := cut.Add(time.Hour)

	// /lib/a.mkv: three old terminal grants; only the newest survives.
	for _, id := range []string{"a1", "a2", "a3"} {
		grant(t, s, "/lib/a.mkv", id, old)
		endLease(t, s, id, LeaseExpired, old)
	}
	// /lib/b.mkv: an old terminal grant under a LIVE one; the old one goes, the live stays.
	grant(t, s, "/lib/b.mkv", "b1", old)
	endLease(t, s, "b1", LeaseFailed, old)
	grant(t, s, "/lib/b.mkv", "b2", old)
	// /lib/c.mkv: a terminal grant that ended exactly AT the cut and one after it, under a
	// newer one; neither ended before the cut, so both stay.
	grant(t, s, "/lib/c.mkv", "c1", old)
	endLease(t, s, "c1", LeaseCompleted, cut)
	grant(t, s, "/lib/c.mkv", "c2", old)
	endLease(t, s, "c2", LeaseCompleted, young)
	grant(t, s, "/lib/c.mkv", "c3", old)
	endLease(t, s, "c3", LeaseCompleted, young)

	n, err := s.PruneLeases(ctx, cut)
	if err != nil {
		t.Fatalf("PruneLeases: %v", err)
	}
	if n != 3 {
		t.Errorf("PruneLeases deleted %d rows, want 3 (a1, a2, b1)", n)
	}
	for id, want := range map[string]bool{"a1": false, "a2": false, "a3": true, "b1": false, "b2": true,
		"c1": true, "c2": true, "c3": true} {
		if _, ok, _ := s.GetLease(ctx, id); ok != want {
			t.Errorf("after the prune, lease %s present=%v, want %v", id, ok, want)
		}
	}
	// The epoch history survives the prune: the next grant of a pruned path still counts up.
	if l := grant(t, s, "/lib/a.mkv", "a4", young); l.Epoch != 4 {
		t.Errorf("the grant after a prune carries epoch %d, want 4", l.Epoch)
	}
}

// TestLease_ConcurrentGrantsOfOnePathGetDistinctEpochs: the read of the highest epoch and
// the insert are one transaction, so racing grants cannot both read the same figure.
func TestLease_ConcurrentGrantsOfOnePathGetDistinctEpochs(t *testing.T) {
	s := openTest(t)
	at := time.Unix(1800000000, 0)
	const n = 16
	epochs := make([]int64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l, err := s.GrantLease(context.Background(), "/lib/a.mkv", func([]Lease, int64) (Lease, error) {
				return leaseAt("lease-"+string(rune('a'+i)), at), nil
			})
			if err != nil {
				t.Errorf("grant %d: %v", i, err)
				return
			}
			epochs[i] = l.Epoch
		}(i)
	}
	wg.Wait()
	seen := map[int64]bool{}
	for _, e := range epochs {
		if e < 1 || e > n || seen[e] {
			t.Fatalf("epochs %v are not the distinct figures 1..%d", epochs, n)
		}
		seen[e] = true
	}
}

// TestLease_RowsSurviveACloseAndReopen: a lease is durable, which is what lets a restarted
// server tell a live node's work from a stale one's.
func TestLease_RowsSurviveACloseAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1800000000, 0)
	want := grant(t, s, "/lib/a.mkv", "lease-1", at)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	live, err := s.LiveLeases(context.Background())
	if err != nil || len(live) != 1 || live[0] != want {
		t.Fatalf("after a reopen LiveLeases = %+v (err %v), want the one granted row %+v", live, err, want)
	}
}
