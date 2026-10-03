package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// The paged ledger read (S0170): the order is TOTAL, "strictly after a position" names an
// exact set at every level of that order, and the page says whether a row follows it.

// pageRow is one fixture row, in the order the paged read must return it.
type pageRow struct {
	at       int64
	path, fp string
	status   Status
}

func seedPage(t *testing.T, s *SQLite, rows []pageRow) {
	t.Helper()
	// Written in REVERSE, so nothing about the order can come from insertion.
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		if _, err := s.db.Exec(`INSERT INTO jobs (path, fingerprint, status, fail_count, updated_at) VALUES (?, ?, ?, 0, ?)`,
			r.path, r.fp, string(r.status), r.at); err != nil {
			t.Fatalf("seed %v: %v", r, err)
		}
	}
}

func rowKeys(jobs []Job) string {
	out := make([]string, len(jobs))
	for i, j := range jobs {
		out[i] = fmt.Sprintf("%d|%s|%s", j.UpdatedAt, j.Path, j.Fingerprint)
	}
	return strings.Join(out, "\n")
}

func wantKeys(rows []pageRow) string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = fmt.Sprintf("%d|%s|%s", r.at, r.path, r.fp)
	}
	return strings.Join(out, "\n")
}

// pagedOrder is nine rows in the paged order: newest second first, then path, then
// fingerprint. It holds a tie at every level - three rows in one second, two of them at one
// path - so each comparison in the position clause decides at least one boundary.
var pagedOrder = []pageRow{
	{300, "/lib/b.mkv", "1:1", Done},
	{300, "/lib/c.mkv", "1:1", Failed},
	{200, "/lib/a.mkv", "1:1", Done},
	{200, "/lib/a.mkv", "2:2", Skipped},
	{200, "/lib/a.mkv", "3:3", Done},
	{200, "/lib/d.mkv", "0:0", Failed},
	{100, "/lib/a.mkv", "9:9", Done},
	{100, "/lib/z.mkv", "1:1", Pending},
	{50, "/lib/0.mkv", "1:1", Done},
}

func TestListPage_TheOrderIsTotalAndEveryBoundaryIsExact(t *testing.T) {
	s := openTest(t)
	seedPage(t, s, pagedOrder)
	ctx := context.Background()

	all, more, err := s.ListPage(ctx, nil, nil, 100)
	if err != nil {
		t.Fatalf("ListPage: %v", err)
	}
	if more {
		t.Error("every row was returned and the page says another follows")
	}
	if got, want := rowKeys(all), wantKeys(pagedOrder); got != want {
		t.Fatalf("the unbounded read is out of order.\n got:\n%s\nwant:\n%s", got, want)
	}

	// A page starting strictly after EACH position is exactly the rows that follow it:
	// never the row itself again, and never a row skipped.
	for i, r := range pagedOrder {
		after := &PagePosition{UpdatedAt: r.at, Path: r.path, Fingerprint: r.fp}
		got, more, err := s.ListPage(ctx, nil, after, 100)
		if err != nil {
			t.Fatalf("after row %d: %v", i, err)
		}
		if more {
			t.Errorf("after row %d: the rest fits the page and it says another follows", i)
		}
		if g, w := rowKeys(got), wantKeys(pagedOrder[i+1:]); g != w {
			t.Errorf("after row %d (%v):\n got:\n%s\nwant:\n%s", i, r, g, w)
		}
	}

	// Every page size walks the whole set once, and says "more" exactly while rows remain.
	for limit := 1; limit <= len(pagedOrder)+1; limit++ {
		var walked []Job
		var after *PagePosition
		for {
			got, more, err := s.ListPage(ctx, nil, after, limit)
			if err != nil {
				t.Fatalf("limit %d: %v", limit, err)
			}
			if len(got) > limit {
				t.Fatalf("limit %d returned %d rows", limit, len(got))
			}
			walked = append(walked, got...)
			if remaining := len(walked) < len(pagedOrder); more != remaining {
				t.Fatalf("limit %d after %d rows: more = %v, and rows remain = %v", limit, len(walked), more, remaining)
			}
			if !more {
				break
			}
			if len(got) != limit {
				t.Fatalf("limit %d: a page that says more follows carries %d rows", limit, len(got))
			}
			pos := PositionOf(got[len(got)-1])
			after = &pos
		}
		if g, w := rowKeys(walked), wantKeys(pagedOrder); g != w {
			t.Errorf("limit %d walked:\n%s\nwant:\n%s", limit, g, w)
		}
	}
}

func TestListPage_APositionWithNoRowContinuesFromThatPlace(t *testing.T) {
	s := openTest(t)
	seedPage(t, s, pagedOrder)
	// A place between rows 3 and 4, where no row stands.
	got, _, err := s.ListPage(context.Background(), nil, &PagePosition{UpdatedAt: 200, Path: "/lib/a.mkv", Fingerprint: "2:9"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if g, w := rowKeys(got), wantKeys(pagedOrder[4:6]); g != w {
		t.Errorf("got:\n%s\nwant:\n%s", g, w)
	}
}

func TestListPage_FiltersToTheStatusSet(t *testing.T) {
	s := openTest(t)
	seedPage(t, s, pagedOrder)
	ctx := context.Background()
	var want []pageRow
	for _, r := range pagedOrder {
		if r.status == Done || r.status == Skipped {
			want = append(want, r)
		}
	}
	got, more, err := s.ListPage(ctx, []Status{Done, Skipped}, nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !more {
		t.Error("six rows match, four were returned, and the page says nothing follows")
	}
	if g, w := rowKeys(got), wantKeys(want[:4]); g != w {
		t.Fatalf("first page:\n%s\nwant:\n%s", g, w)
	}
	pos := PositionOf(got[3])
	rest, more, err := s.ListPage(ctx, []Status{Done, Skipped}, &pos, 4)
	if err != nil {
		t.Fatal(err)
	}
	if more {
		t.Error("the last two matching rows were returned and the page says another follows")
	}
	if g, w := rowKeys(rest), wantKeys(want[4:]); g != w {
		t.Fatalf("second page:\n%s\nwant:\n%s", g, w)
	}
	for _, j := range append(got, rest...) {
		if j.Status != Done && j.Status != Skipped {
			t.Errorf("a %s row was returned under a done,skipped filter", j.Status)
		}
	}

	// A page of exactly the matching rows is the last page.
	exact, more, err := s.ListPage(ctx, []Status{Failed}, nil, 2)
	if err != nil || len(exact) != 2 || more {
		t.Errorf("two failed rows, limit 2: %d rows, more=%v, err=%v", len(exact), more, err)
	}
	none, more, err := s.ListPage(ctx, []Status{Indeterminate}, nil, 2)
	if err != nil || len(none) != 0 || more {
		t.Errorf("no matching row: %d rows, more=%v, err=%v", len(none), more, err)
	}
}

func TestListPage_RefusesAPageOfNoRows(t *testing.T) {
	s := openTest(t)
	seedPage(t, s, pagedOrder)
	for _, limit := range []int{0, -1} {
		if got, more, err := s.ListPage(context.Background(), nil, nil, limit); err == nil || got != nil || more {
			t.Errorf("limit %d: %d rows, more=%v, err=%v; want a refusal", limit, len(got), more, err)
		}
	}
	if got, _, err := s.ListPage(context.Background(), nil, nil, 1); err != nil || len(got) != 1 {
		t.Errorf("limit 1: %d rows, err=%v", len(got), err)
	}
}

func TestListPage_ReportsAReadThatFails(t *testing.T) {
	s := openTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, more, err := s.ListPage(ctx, nil, nil, 5); err == nil || got != nil || more {
		t.Errorf("a cancelled read returned %d rows, more=%v, err=%v", len(got), more, err)
	}
}
