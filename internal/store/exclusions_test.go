package store

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// tempDB is a database file that does not exist yet, so a case can open it, close it and
// open it AGAIN - which is the only way to prove a record survives the handle that wrote it.
func tempDB(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "jobs.db")
}

// The two reads and the one writing of runtime state this phase adds: a path search over
// the WHOLE ledger, and the withheld-path record an operator's surface creates and removes.
//
// Both are graded through the production methods rather than through SQL written here: a
// test that spelled the query again would agree with the store today and drift from it the
// first time either moved, which is the failure this repository refuses everywhere else.

// seedDoneRows writes n done rows under root, oldest transition first, and returns their
// paths in the order they were written.
func seedDoneRows(t *testing.T, s *SQLite, root string, n int) []string {
	t.Helper()
	ctx := context.Background()
	var paths []string
	for i := 0; i < n; i++ {
		p := root + "/" + strconv.Itoa(i) + ".mkv"
		key := strconv.Itoa(i) + ":" + strconv.Itoa(i)
		if ok, err := s.Claim(ctx, p, key, "w0", 3, DecisionInputs{}); err != nil || !ok {
			t.Fatalf("Claim %s: ok=%v err=%v", p, ok, err)
		}
		if err := s.Finish(ctx, p, key, Done, &Outcome{Encoder: "cpu"}, 3); err != nil {
			t.Fatalf("Finish %s: %v", p, err)
		}
		paths = append(paths, p)
	}
	return paths
}

// The search reaches every matching row, not only the ones a capped read would ship, and
// the count it reports is over the whole ledger rather than over what it returned.
func TestSearchPath_ReachesTheWholeLedgerAndReportsTheCount(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seedDoneRows(t, s, "/lib/films", 40)
	seedDoneRows(t, s, "/lib/shows", 10)

	rows, total, err := s.SearchPath(ctx, []Status{Done}, "/lib/films/", 5)
	if err != nil {
		t.Fatalf("SearchPath: %v", err)
	}
	if len(rows) != 5 {
		t.Errorf("the search returned %d rows under a limit of 5", len(rows))
	}
	if total.Err != nil {
		t.Errorf("the total could not be read: %v", total.Err)
	}
	if total.Count != 40 {
		t.Errorf("the search counted %d matching rows, want 40 - the count must be over the ledger and not over what it shipped", total.Count)
	}
	if total.Coverage.Set == "" {
		t.Error("the total states no set, so a reader cannot tell what it counted")
	}
	// Uncapped, it really does reach all of them.
	all, _, err := s.SearchPath(ctx, []Status{Done}, "/lib/films/", 0)
	if err != nil {
		t.Fatalf("SearchPath (uncapped): %v", err)
	}
	if len(all) != 40 {
		t.Errorf("an uncapped search returned %d rows, want 40", len(all))
	}
	for _, j := range all {
		if !strings.Contains(j.Path, "/lib/films/") {
			t.Errorf("the search returned a row that does not match the term: %q", j.Path)
		}
	}
}

// The term is TEXT. A `_` and a `%` in it match those characters, never a wildcard, so an
// operator searching for one file is never told a row exists for another.
func TestSearchPath_TheTermIsTextAndNotAPattern(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for i, p := range []string{"/lib/the_wire.mkv", "/lib/the-wire.mkv", "/lib/100%.mkv", "/lib/1000.mkv"} {
		key := strconv.Itoa(i) + ":0"
		if ok, err := s.Claim(ctx, p, key, "w0", 3, DecisionInputs{}); err != nil || !ok {
			t.Fatalf("Claim %s: ok=%v err=%v", p, ok, err)
		}
		if err := s.Finish(ctx, p, key, Done, &Outcome{}, 3); err != nil {
			t.Fatalf("Finish %s: %v", p, err)
		}
	}
	for _, tc := range []struct{ term, want string }{
		{"the_wire", "/lib/the_wire.mkv"},
		{"100%", "/lib/100%.mkv"},
	} {
		rows, total, err := s.SearchPath(ctx, []Status{Done}, tc.term, 0)
		if err != nil {
			t.Fatalf("SearchPath(%q): %v", tc.term, err)
		}
		if len(rows) != 1 || rows[0].Path != tc.want {
			t.Errorf("searching for %q returned %d row(s) (%v), want exactly %q - the term was taken as a pattern",
				tc.term, len(rows), pathsOf(rows), tc.want)
		}
		if total.Count != 1 {
			t.Errorf("searching for %q counted %d, want 1", tc.term, total.Count)
		}
	}
}

func pathsOf(rows []Job) []string {
	out := make([]string, 0, len(rows))
	for _, j := range rows {
		out = append(out, j.Path)
	}
	return out
}

// A withholding survives the handle that recorded it, is idempotent, and is removable -
// which is what keeps a wrongly recorded one from silently stopping work for ever.
func TestExclusions_AreDurableIdempotentAndRemovable(t *testing.T) {
	path := tempDB(t)
	ctx := context.Background()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	added, err := s.ExcludePath(ctx, "/lib/films/a.mkv")
	if err != nil || !added {
		t.Fatalf("ExcludePath: added=%v err=%v", added, err)
	}
	again, err := s.ExcludePath(ctx, "/lib/films/a.mkv")
	if err != nil {
		t.Fatalf("ExcludePath (again): %v", err)
	}
	if again {
		t.Error("recording the same path twice reported a new record; it is one withholding, not two")
	}
	_ = s.Close()

	// A NEW handle on the same file: the record is the daemon's own state and outlives the
	// process that wrote it.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()

	held, err := s2.PathIsExcluded(ctx, "/lib/films/a.mkv")
	if err != nil || !held {
		t.Fatalf("PathIsExcluded after a restart: held=%v err=%v", held, err)
	}
	list, err := s2.ExcludedPaths(ctx)
	if err != nil {
		t.Fatalf("ExcludedPaths: %v", err)
	}
	if len(list) != 1 || list[0].Path != "/lib/films/a.mkv" {
		t.Fatalf("the withheld paths read back as %+v, want exactly the one recorded", list)
	}
	if list[0].CreatedAt == 0 {
		t.Error("the withholding records no time, so nobody can tell how long a file has been held out")
	}
	if !list[0].Stamp.Recognised() {
		t.Errorf("the withholding carries no recognised schema stamp: %s", list[0].Stamp)
	}

	// Another path is untouched by either operation.
	if held, err := s2.PathIsExcluded(ctx, "/lib/films/b.mkv"); err != nil || held {
		t.Errorf("a path nobody withheld reads as withheld: held=%v err=%v", held, err)
	}

	gone, err := s2.UnexcludePath(ctx, "/lib/films/a.mkv")
	if err != nil || !gone {
		t.Fatalf("UnexcludePath: gone=%v err=%v", gone, err)
	}
	if held, err := s2.PathIsExcluded(ctx, "/lib/films/a.mkv"); err != nil || held {
		t.Errorf("the path is still withheld after the record was removed: held=%v err=%v", held, err)
	}
	if again, err := s2.UnexcludePath(ctx, "/lib/films/a.mkv"); err != nil || again {
		t.Errorf("removing a withholding that was not there reported a change: again=%v err=%v", again, err)
	}
	list, err = s2.ExcludedPaths(ctx)
	if err != nil {
		t.Fatalf("ExcludedPaths after removal: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("the withheld paths still hold %+v after the only record was removed", list)
	}
}

// The withholding table is its own table and touches no job row: recording one must not
// move, clear or create anything in the ledger the engine reads its decisions from.
func TestExclusions_TouchNoJobRow(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seedDoneRows(t, s, "/lib/films", 3)

	before, err := s.List(ctx, nil, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if _, err := s.ExcludePath(ctx, "/lib/films/0.mkv"); err != nil {
		t.Fatalf("ExcludePath: %v", err)
	}
	if _, err := s.UnexcludePath(ctx, "/lib/films/1.mkv"); err != nil {
		t.Fatalf("UnexcludePath: %v", err)
	}
	after, err := s.List(ctx, nil, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(before) != len(after) {
		t.Fatalf("the ledger went from %d rows to %d across two withholding operations", len(before), len(after))
	}
	for i := range before {
		if before[i].Path != after[i].Path || before[i].Status != after[i].Status ||
			before[i].FailCount != after[i].FailCount || before[i].Outcome.Reason != after[i].Outcome.Reason {
			t.Errorf("the row %q moved across a withholding operation: %+v -> %+v", before[i].Path, before[i], after[i])
		}
	}
}
