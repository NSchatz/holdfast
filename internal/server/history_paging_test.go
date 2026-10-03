package server

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/secret"
	"github.com/NSchatz/holdfast/internal/store"
)

// GET /api/history: the status filter and the cursor (S0170). One test per acceptance
// criterion, each named for it.
//
// The fixtures are synthetic rows written through the store, with their `updated_at` then
// stamped through the test's own database handle: the subject is the endpoint, and a paging
// test that cannot place two rows in one second, or one row strictly after another, cannot
// reach the boundaries the criteria are about.

// pagingBase is the newest stamp a fixture writes. It is far enough in the past that a row
// the store stamps with the real clock sorts ahead of every fixture row.
const pagingBase = int64(1_700_000_000)

// pagingLedger is a ledger a paging test seeds and re-stamps.
type pagingLedger struct {
	t   *testing.T
	st  *store.SQLite
	raw *sql.DB
	// nextID is the next row's identity: each row carries a unique source_bytes, which is
	// what tells two rows apart on the wire where they share a path, a status and a second
	// (the wire row deliberately carries no fingerprint).
	nextID int64
	// tx is the open transaction add and pending write into, nil when none is open.
	tx *sql.Tx
}

func newPagingLedger(t *testing.T) *pagingLedger {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jobs.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return &pagingLedger{t: t, st: st, raw: raw, nextID: 1}
}

// add records one terminal row and returns its identity. The rows are written through the
// test's own handle, in one transaction a fixture commits before anything reads it: a
// fixture of several hundred rows written one durable transition at a time would spend its
// whole run waiting on the disk, and the subject here is the endpoint, not the writer.
func (l *pagingLedger) add(path, fp string, status store.Status, updatedAt int64) int64 {
	l.t.Helper()
	id := l.nextID
	l.nextID++
	l.insert(path, fp, string(status), updatedAt, id)
	return id
}

// pending records one row the history view must never serve.
func (l *pagingLedger) pending(path string) {
	l.t.Helper()
	l.insert(path, "q:q", string(store.Pending), pagingBase+1, nil)
}

func (l *pagingLedger) insert(path, fp, status string, updatedAt int64, sourceBytes any) {
	l.t.Helper()
	if l.tx == nil {
		tx, err := l.raw.Begin()
		if err != nil {
			l.t.Fatalf("begin: %v", err)
		}
		l.tx = tx
	}
	if _, err := l.tx.Exec(`INSERT INTO jobs (path, fingerprint, status, fail_count, updated_at, source_bytes)
		VALUES (?, ?, ?, 0, ?, ?)`, path, fp, status, updatedAt, sourceBytes); err != nil {
		l.t.Fatalf("insert %s %s: %v", path, fp, err)
	}
}

// flush commits what add and pending wrote.
func (l *pagingLedger) flush() {
	l.t.Helper()
	if l.tx == nil {
		return
	}
	if err := l.tx.Commit(); err != nil {
		l.t.Fatalf("commit: %v", err)
	}
	l.tx = nil
}

// retransition moves a row to another terminal status, stamped at.
func (l *pagingLedger) retransition(path, fp string, status store.Status, at int64) {
	l.t.Helper()
	l.flush()
	res, err := l.raw.Exec(`UPDATE jobs SET status = ?, updated_at = ? WHERE path = ? AND fingerprint = ?`,
		string(status), at, path, fp)
	if err != nil {
		l.t.Fatalf("retransition %s: %v", path, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		l.t.Fatalf("retransition %s %s moved %d rows, want 1", path, fp, n)
	}
}

func (l *pagingLedger) remove(path, fp string) {
	l.t.Helper()
	l.flush()
	if err := l.st.Delete(context.Background(), path, fp); err != nil {
		l.t.Fatalf("Delete(%s): %v", path, err)
	}
}

// reference is the ONE unbounded read of a status set, in the order the endpoint states:
// newest transition first, then path, then fingerprint. It is read through the test's own
// handle and not through the store, so the paged read is compared with something that is
// not itself.
func (l *pagingLedger) reference(statuses ...string) []int64 {
	l.t.Helper()
	l.flush()
	ph := make([]string, len(statuses))
	args := make([]any, len(statuses))
	for i, s := range statuses {
		ph[i], args[i] = "?", s
	}
	rows, err := l.raw.Query(`SELECT source_bytes FROM jobs WHERE status IN (`+strings.Join(ph, ",")+
		`) ORDER BY updated_at DESC, path ASC, fingerprint ASC`, args...)
	if err != nil {
		l.t.Fatalf("reference read: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			l.t.Fatalf("reference scan: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		l.t.Fatalf("reference rows: %v", err)
	}
	return out
}

func (l *pagingLedger) serve() *httptest.Server {
	l.t.Helper()
	l.flush()
	ts := httptest.NewServer(newHarnessOn(l.t, l.st, "", "").srv)
	l.t.Cleanup(ts.Close)
	return ts
}

var terminalWords = []string{"done", "skipped", "failed", "would-transcode", "indeterminate", "applied-despite-error"}

// historyWire is one page as it arrives.
type historyWire struct {
	History []struct {
		Path        string `json:"path"`
		Status      string `json:"status"`
		UpdatedAt   int64  `json:"updated_at"`
		SourceBytes *int64 `json:"source_bytes"`
	} `json:"history"`
	HistoryTotal rowTotalWire `json:"history_total"`
	NextCursor   *string      `json:"next_cursor"`
}

func (p historyWire) ids(t *testing.T) []int64 {
	t.Helper()
	out := make([]int64, 0, len(p.History))
	for _, r := range p.History {
		if r.SourceBytes == nil {
			t.Fatalf("row %s carries no source_bytes, so the fixture cannot identify it", r.Path)
		}
		out = append(out, *r.SourceBytes)
	}
	return out
}

// fetch is one GET, returning the status, the content type and the body.
func fetch(t *testing.T, rawURL string) (int, string, []byte) {
	t.Helper()
	resp, err := http.Get(rawURL)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", rawURL, err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), body
}

// page is one accepted page: a 200, decoded, with its three keys proven present.
func page(t *testing.T, base string, q url.Values) historyWire {
	t.Helper()
	u := base + "/api/history"
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	code, _, body := fetch(t, u)
	if code != http.StatusOK {
		t.Fatalf("GET %s: status %d, body %s", u, code, body)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	for _, k := range []string{"history", "history_total", "next_cursor"} {
		if _, ok := keys[k]; !ok {
			t.Fatalf("GET %s: the body carries no %q key: %s", u, k, body)
		}
	}
	if string(keys["history"]) == "null" {
		t.Fatalf("GET %s: history is null; an empty page is an empty array", u)
	}
	var out historyWire
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	return out
}

// traverse follows next_cursor from the first page until it is null and returns every row's
// identity in the order served, and the pages' sizes.
func traverse(t *testing.T, base string, q url.Values) (ids []int64, sizes []int) {
	t.Helper()
	params := url.Values{}
	for k, v := range q {
		params[k] = v
	}
	for n := 0; ; n++ {
		if n > 10_000 {
			t.Fatal("the traversal never reached a null next_cursor")
		}
		p := page(t, base, params)
		ids = append(ids, p.ids(t)...)
		sizes = append(sizes, len(p.History))
		if p.NextCursor == nil {
			return ids, sizes
		}
		params.Set("cursor", *p.NextCursor)
	}
}

func sameIDs(a, b []int64) bool {
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

// refusalWire is the 400 envelope as it arrives. Retryable is a pointer so an absent key is
// not read as the false the contract requires to be STATED.
type refusalWire struct {
	Rule       string `json:"rule"`
	Error      string `json:"error"`
	Retryable  *bool  `json:"retryable"`
	Parameters []struct {
		Parameter string `json:"parameter"`
		Rule      string `json:"rule"`
		Error     string `json:"error"`
	} `json:"parameters"`
}

// refused asserts a request was refused in the envelope and returns the per-parameter rules
// by parameter, in the order served.
func refused(t *testing.T, rawURL string) (params []string, rules map[string]string) {
	t.Helper()
	code, ctype, body := fetch(t, rawURL)
	if code != http.StatusBadRequest {
		t.Fatalf("GET %s: status %d, want 400; body %s", rawURL, code, body)
	}
	if !strings.HasPrefix(ctype, "application/json") {
		t.Fatalf("GET %s: the refusal is %q, want application/json", rawURL, ctype)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		t.Fatalf("GET %s: the refusal is not JSON: %v", rawURL, err)
	}
	if _, ok := keys["history"]; ok {
		t.Fatalf("GET %s: a refusal carries rows: %s", rawURL, body)
	}
	var got refusalWire
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	if got.Rule != "invalid-query" {
		t.Errorf("GET %s: top-level rule %q, want the stable token invalid-query", rawURL, got.Rule)
	}
	if got.Error == "" {
		t.Errorf("GET %s: the refusal says nothing in words", rawURL)
	}
	if got.Retryable == nil || *got.Retryable {
		t.Errorf("GET %s: retryable must be stated and false: %s", rawURL, body)
	}
	rules = map[string]string{}
	for _, p := range got.Parameters {
		if p.Error == "" {
			t.Errorf("GET %s: the %s entry says nothing in words", rawURL, p.Parameter)
		}
		if _, twice := rules[p.Parameter]; twice {
			t.Errorf("GET %s: %s is named twice", rawURL, p.Parameter)
		}
		params = append(params, p.Parameter)
		rules[p.Parameter] = p.Rule
		if !strings.Contains(got.Error, p.Parameter) {
			t.Errorf("GET %s: the top-level error %q does not name %s", rawURL, got.Error, p.Parameter)
		}
	}
	return params, rules
}

// --- AC-1 ------------------------------------------------------------------------------

func TestS0170_AC1_NoParametersServesTheNewestTerminalRowsAndSaysWhetherMoreFollow(t *testing.T) {
	l := newPagingLedger(t)
	const rows = historyLimit + 9
	for i := 0; i < rows; i++ {
		// Three rows a second, written in an order that is neither the path's nor the stamp's.
		k := (i * 7) % rows
		l.add(fmt.Sprintf("/lib/ac1/%04d.mkv", k), "a:a", store.Status(terminalWords[k%len(terminalWords)]), pagingBase-int64(k/3))
	}
	for i := 0; i < 5; i++ {
		l.pending(fmt.Sprintf("/lib/ac1/queued%d.mkv", i))
	}
	ts := l.serve()
	want := l.reference(terminalWords...)
	if len(want) != rows {
		t.Fatalf("the fixture holds %d terminal rows, want %d", len(want), rows)
	}

	t.Run("the cap", func(t *testing.T) {
		p := page(t, ts.URL, nil)
		if !sameIDs(p.ids(t), want[:historyLimit]) {
			t.Fatalf("the page is not the newest %d terminal rows in order", historyLimit)
		}
		for i, r := range p.History {
			if !store.Status(r.Status).Terminal() {
				t.Fatalf("row %d (%s) is %s: the history view served a non-terminal row", i, r.Path, r.Status)
			}
			if i == 0 {
				continue
			}
			prev := p.History[i-1]
			if prev.UpdatedAt < r.UpdatedAt || (prev.UpdatedAt == r.UpdatedAt && prev.Path > r.Path) {
				t.Fatalf("rows %d and %d are out of order: (%d, %s) before (%d, %s)",
					i-1, i, prev.UpdatedAt, prev.Path, r.UpdatedAt, r.Path)
			}
		}
		requireAvailable(t, "history_total", p.HistoryTotal, rows, historyLimit)
		if p.NextCursor == nil || *p.NextCursor == "" {
			t.Fatalf("%d terminal rows exist and %d were served, and next_cursor is null", rows, len(p.History))
		}
	})

	t.Run("fewer under limit", func(t *testing.T) {
		p := page(t, ts.URL, url.Values{"limit": {"4"}})
		if !sameIDs(p.ids(t), want[:4]) {
			t.Fatalf("limit=4 served %d rows, not the newest four", len(p.History))
		}
		requireAvailable(t, "history_total", p.HistoryTotal, rows, 4)
		if p.NextCursor == nil {
			t.Fatal("rows remain after four and next_cursor is null")
		}
	})

	t.Run("a limit the clamp does not accept keeps the cap", func(t *testing.T) {
		for _, limit := range []string{"0", "-3", "201", "many", strconv.Itoa(historyLimit)} {
			if p := page(t, ts.URL, url.Values{"limit": {limit}}); len(p.History) != historyLimit {
				t.Errorf("limit=%s served %d rows, want the cap of %d", limit, len(p.History), historyLimit)
			}
		}
		if p := page(t, ts.URL, url.Values{"limit": {"199"}}); len(p.History) != 199 {
			t.Errorf("limit=199 served %d rows", len(p.History))
		}
	})

	t.Run("nothing follows", func(t *testing.T) {
		small := newPagingLedger(t)
		small.add("/lib/one.mkv", "a:a", store.Done, pagingBase)
		small.add("/lib/two.mkv", "a:a", store.Failed, pagingBase)
		small.pending("/lib/three.mkv")
		p := page(t, small.serve().URL, nil)
		if len(p.History) != 2 {
			t.Fatalf("served %d rows, want the two terminal ones", len(p.History))
		}
		if p.NextCursor != nil {
			t.Fatalf("every terminal row was served and next_cursor is %q, want null", *p.NextCursor)
		}
		requireAvailable(t, "history_total", p.HistoryTotal, 2, historyLimit)
	})
}

// --- AC-2 ------------------------------------------------------------------------------

func TestS0170_AC2_AStatusFilterServesOnlyThoseRowsAndTotalsTheWholeFilteredSet(t *testing.T) {
	l := newPagingLedger(t)
	counts := map[string]int{}
	for i := 0; i < 60; i++ {
		status := terminalWords[i%len(terminalWords)]
		counts[status]++
		l.add(fmt.Sprintf("/lib/ac2/%03d.mkv", i), "a:a", store.Status(status), pagingBase-int64(i/4))
	}
	l.pending("/lib/ac2/queued.mkv")
	ts := l.serve()

	t.Run("one status", func(t *testing.T) {
		p := page(t, ts.URL, url.Values{"status": {"failed"}, "limit": {"3"}})
		if !sameIDs(p.ids(t), l.reference("failed")[:3]) {
			t.Fatal("status=failed did not serve the three newest failed rows in order")
		}
		for _, r := range p.History {
			if r.Status != "failed" {
				t.Fatalf("status=failed served a %s row (%s)", r.Status, r.Path)
			}
		}
		requireAvailable(t, "history_total", p.HistoryTotal, int64(counts["failed"]), 3)
		if !strings.Contains(p.HistoryTotal.Covers, "failed") || strings.Contains(p.HistoryTotal.Covers, "done") {
			t.Errorf("covers = %q: it must name the requested status and no other", p.HistoryTotal.Covers)
		}
	})

	t.Run("a union, in either spelling", func(t *testing.T) {
		want := l.reference("done", "failed")
		p := page(t, ts.URL, url.Values{"status": {"done,failed"}, "limit": {"7"}})
		if !sameIDs(p.ids(t), want[:7]) {
			t.Fatal("status=done,failed did not serve the seven newest matching rows in order")
		}
		for _, r := range p.History {
			if r.Status != "done" && r.Status != "failed" {
				t.Fatalf("status=done,failed served a %s row (%s)", r.Status, r.Path)
			}
		}
		requireAvailable(t, "history_total", p.HistoryTotal, int64(counts["done"]+counts["failed"]), 7)
		if int64(len(p.History)) == *p.HistoryTotal.Count {
			t.Fatal("the fixture caps nothing, so the count could be the rows returned")
		}
		for _, name := range []string{"done", "failed"} {
			if !strings.Contains(p.HistoryTotal.Covers, name) {
				t.Errorf("covers = %q does not name %s", p.HistoryTotal.Covers, name)
			}
		}
		if strings.Contains(p.HistoryTotal.Covers, "skipped") {
			t.Errorf("covers = %q names a status nobody asked for", p.HistoryTotal.Covers)
		}

		_, _, joined := fetch(t, ts.URL+"/api/history?limit=7&status=done,failed")
		for _, spelling := range []string{
			"status=done&status=failed", "status=failed&status=done", "status=failed,done",
			"status=done,failed&status=done",
		} {
			code, _, body := fetch(t, ts.URL+"/api/history?limit=7&"+spelling)
			if code != http.StatusOK {
				t.Fatalf("%s: status %d", spelling, code)
			}
			if string(body) != string(joined) {
				t.Errorf("%s and status=done,failed are one set and answered differently:\n%s\n%s", spelling, body, joined)
			}
		}
	})

	t.Run("every terminal status is accepted", func(t *testing.T) {
		for _, name := range terminalWords {
			p := page(t, ts.URL, url.Values{"status": {name}})
			if len(p.History) != counts[name] {
				t.Errorf("status=%s served %d rows, want %d", name, len(p.History), counts[name])
			}
			requireAvailable(t, "history_total for "+name, p.HistoryTotal, int64(counts[name]), historyLimit)
		}
	})
}

// --- AC-3 ------------------------------------------------------------------------------

// seedTraversal writes the AC-3 ledger: 660 terminal rows, 150 to a second, of which 600 are
// done or failed, and one path that two rows share under two fingerprints in one second -
// placed at positions 199 and 200 of the unfiltered order, so a page boundary of 50 and of
// 200 both fall BETWEEN them and only the fingerprint decides which side each is on.
func seedTraversal(l *pagingLedger) {
	const rows = 660
	for i := 0; i < rows; i++ {
		k := (i * 13) % rows // written out of order
		status := store.Done
		switch {
		case k%11 == 0:
			status = store.Skipped
		case k%2 == 1:
			status = store.Failed
		}
		path, fp := fmt.Sprintf("/lib/ac3/%04d.mkv", k), "a:a"
		if k == 200 {
			path, fp = fmt.Sprintf("/lib/ac3/%04d.mkv", 199), "b:b"
		}
		l.add(path, fp, status, pagingBase-int64(k/150))
	}
	for i := 0; i < 20; i++ {
		l.pending(fmt.Sprintf("/lib/ac3/queued%02d.mkv", i))
	}
}

func TestS0170_AC3_FollowingTheCursorServesEveryRowExactlyOnceInOrder(t *testing.T) {
	l := newPagingLedger(t)
	seedTraversal(l)
	ts := l.serve()

	var shared int
	l.flush()
	if err := l.raw.QueryRow(`SELECT COUNT(*) FROM jobs WHERE path = '/lib/ac3/0199.mkv'`).Scan(&shared); err != nil || shared != 2 {
		t.Fatalf("the fixture holds %d rows at the shared path (err %v), want 2", shared, err)
	}

	for _, tc := range []struct {
		name      string
		statuses  []string
		query     url.Values
		wantRows  int
		wantPages []int
	}{
		{"unfiltered, default page", terminalWords, nil, 660, []int{200, 200, 200, 60}},
		{"unfiltered, limit=50", terminalWords, url.Values{"limit": {"50"}}, 660, nil},
		{"filtered, default page", []string{"done", "failed"}, url.Values{"status": {"done,failed"}}, 600, []int{200, 200, 200}},
		{"filtered, limit=50", []string{"done", "failed"}, url.Values{"status": {"failed", "done"}, "limit": {"50"}}, 600, nil},
		{"one status, limit=50", []string{"skipped"}, url.Values{"status": {"skipped"}, "limit": {"50"}}, 60, []int{50, 10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := l.reference(tc.statuses...)
			if len(want) != tc.wantRows {
				t.Fatalf("the fixture holds %d matching rows, want %d", len(want), tc.wantRows)
			}
			got, sizes := traverse(t, ts.URL, tc.query)
			if len(sizes) < 2 {
				t.Fatalf("the traversal was %d page(s); the fixture pages nothing", len(sizes))
			}
			seen := map[int64]int{}
			for _, id := range got {
				seen[id]++
			}
			for _, id := range want {
				switch seen[id] {
				case 1:
				case 0:
					t.Errorf("row %d was never served", id)
				default:
					t.Errorf("row %d was served %d times", id, seen[id])
				}
			}
			if !sameIDs(got, want) {
				t.Fatalf("the traversal served %d rows and the one unbounded read %d, or in another order", len(got), len(want))
			}
			if tc.wantPages != nil && fmt.Sprint(sizes) != fmt.Sprint(tc.wantPages) {
				t.Errorf("page sizes %v, want %v", sizes, tc.wantPages)
			}
		})
	}
}

// --- AC-4 ------------------------------------------------------------------------------

func TestS0170_AC4_TheLastPageSaysItIsTheLast(t *testing.T) {
	l := newPagingLedger(t)
	for i := 0; i < 10; i++ {
		l.add(fmt.Sprintf("/lib/ac4/%02d.mkv", i), "a:a", store.Done, pagingBase) // one second
	}
	for i := 0; i < 4; i++ {
		l.add(fmt.Sprintf("/lib/ac4/skip%02d.mkv", i), "a:a", store.Skipped, pagingBase)
	}
	ts := l.serve()
	filter := url.Values{"status": {"done"}}

	for _, tc := range []struct {
		limit string
		sizes []int
	}{
		{"5", []int{5, 5}}, // an exact multiple: two full pages and no third
		{"10", []int{10}},  // one full page that is also the last
		{"9", []int{9, 1}}, // one row remains: a cursor, then the row
		{"3", []int{3, 3, 3, 1}},
		{"11", []int{10}},
	} {
		q := url.Values{"status": filter["status"], "limit": {tc.limit}}
		_, sizes := traverse(t, ts.URL, q)
		if fmt.Sprint(sizes) != fmt.Sprint(tc.sizes) {
			t.Errorf("limit=%s: pages of %v, want %v - a full last page must carry next_cursor null, "+
				"and a page with rows after it must carry a cursor", tc.limit, sizes, tc.sizes)
		}
	}

	// Unfiltered, the same rule: 14 terminal rows in pages of 7.
	if _, sizes := traverse(t, ts.URL, url.Values{"limit": {"7"}}); fmt.Sprint(sizes) != "[7 7]" {
		t.Errorf("unfiltered limit=7: pages of %v, want [7 7]", sizes)
	}

	first := page(t, ts.URL, url.Values{"status": {"done"}, "limit": {"9"}})
	if first.NextCursor == nil {
		t.Fatal("one matching row remains after nine and next_cursor is null")
	}
	last := page(t, ts.URL, url.Values{"status": {"done"}, "limit": {"9"}, "cursor": {*first.NextCursor}})
	if len(last.History) != 1 || last.NextCursor != nil {
		t.Fatalf("the last page carries %d rows and next_cursor %v, want one row and null", len(last.History), last.NextCursor)
	}
}

// --- AC-5 ------------------------------------------------------------------------------

func TestS0170_AC5_AMovingLedgerNeitherRepeatsNorDropsAnUntouchedRow(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query url.Values
		set   []string
	}{
		{"unfiltered", url.Values{"limit": {"20"}}, terminalWords},
		{"filtered", url.Values{"limit": {"20"}, "status": {"done,skipped"}}, []string{"done", "skipped"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newPagingLedger(t)
			type row struct {
				path string
				id   int64
			}
			var rows []row
			for i := 0; i < 90; i++ {
				status := store.Done
				if i%3 == 0 {
					status = store.Skipped
				}
				p := fmt.Sprintf("/lib/ac5/%03d.mkv", i)
				rows = append(rows, row{p, l.add(p, "a:a", status, pagingBase-int64(i/8))})
			}
			ts := l.serve()
			before := l.reference(tc.set...)
			pathOf := map[int64]string{}
			for _, r := range rows {
				pathOf[r.id] = r.path
			}

			params := url.Values{}
			for k, v := range tc.query {
				params[k] = v
			}
			first := page(t, ts.URL, params)
			served := first.ids(t)
			if first.NextCursor == nil {
				t.Fatal("the fixture fits one page")
			}

			// Between two page reads, each change stamped strictly newer than every row
			// served so far (the newest served stamp is pagingBase).
			newer := pagingBase + 100
			touched := map[int64]bool{}
			// 1. new terminal rows are recorded.
			fresh := []int64{
				l.add("/lib/ac5/new-a.mkv", "a:a", store.Done, newer),
				l.add("/lib/ac5/new-b.mkv", "a:a", store.Skipped, newer+1),
			}
			// 2. a row already served is re-transitioned.
			again := served[3]
			l.retransition(pathOf[again], "a:a", store.Skipped, newer+2)
			touched[again] = true
			// 3. a row not yet served is re-transitioned.
			unserved := before[len(served):]
			moved := unserved[5]
			l.retransition(pathOf[moved], "a:a", store.Done, newer+3)
			touched[moved] = true
			// 4. a row not yet served is deleted.
			gone := unserved[9]
			l.remove(pathOf[gone], "a:a")
			touched[gone] = true

			params.Set("cursor", *first.NextCursor)
			rest, _ := traverse(t, ts.URL, params)
			all := append(append([]int64(nil), served...), rest...)

			seen := map[int64]int{}
			for _, id := range all {
				seen[id]++
			}
			for id, n := range seen {
				if n > 1 {
					t.Errorf("row %d (%s) was served %d times in one traversal", id, pathOf[id], n)
				}
			}
			for _, id := range before {
				if !touched[id] && seen[id] != 1 {
					t.Errorf("untouched row %d (%s) was served %d times, want exactly once", id, pathOf[id], seen[id])
				}
			}
			for _, id := range fresh {
				if seen[id] != 0 {
					t.Errorf("row %d was recorded after the traversal passed its place and was served in it", id)
				}
			}
			if seen[gone] != 0 {
				t.Errorf("the deleted row %d was served", gone)
			}
			if len(all) != len(before)-2 {
				t.Errorf("the traversal served %d rows, want %d: the %d it started over, less the one deleted "+
					"and the one that moved ahead of the cursor", len(all), len(before)-2, len(before))
			}
		})
	}
}

// --- AC-6 ------------------------------------------------------------------------------

func TestS0170_AC6_ACursorWhoseRowWasDeletedContinuesFromThatPosition(t *testing.T) {
	l := newPagingLedger(t)
	paths := map[int64]string{}
	for i := 0; i < 30; i++ {
		p := fmt.Sprintf("/lib/ac6/%02d.mkv", i)
		paths[l.add(p, "a:a", store.Done, pagingBase-int64(i/4))] = p
	}
	ts := l.serve()
	want := l.reference("done")

	first := page(t, ts.URL, url.Values{"limit": {"10"}})
	if first.NextCursor == nil {
		t.Fatal("the fixture fits one page")
	}
	minted := first.ids(t)[9]
	l.remove(paths[minted], "a:a")

	code, _, body := fetch(t, ts.URL+"/api/history?limit=10&cursor="+url.QueryEscape(*first.NextCursor))
	if code != http.StatusOK {
		t.Fatalf("a cursor whose row is gone answered %d: %s", code, body)
	}
	next := page(t, ts.URL, url.Values{"limit": {"10"}, "cursor": {*first.NextCursor}})
	if !sameIDs(next.ids(t), want[10:20]) {
		t.Fatalf("the page after a deleted position is %v, want the ten rows that follow it %v "+
			"(and never the newest rows again)", next.ids(t), want[10:20])
	}
}

// --- AC-7 ------------------------------------------------------------------------------

func TestS0170_AC7_AStatusOutsideTheTerminalVocabularyIsRefused(t *testing.T) {
	l := newPagingLedger(t)
	l.add("/lib/ac7/a.mkv", "a:a", store.Done, pagingBase)
	l.pending("/lib/ac7/q.mkv")
	ts := l.serve()

	for _, query := range []string{
		"status=bogus",               // an unknown word
		"status=pending",             // a status the queue serves
		"status=encoding",            //
		"status=",                    // an empty element
		"status=done,",               // an empty element after a good one
		"status=,done",               //
		"status=done&status=",        // repeated, one empty
		"status=done&status=nope",    // repeated, one unknown
		"status=Done",                // the vocabulary is exact
		"status=%20done",             // and untrimmed
		"status=done,failed,probing", // a non-terminal one among good ones
		"status=verifying&limit=5",
	} {
		t.Run(query, func(t *testing.T) {
			params, rules := refused(t, ts.URL+"/api/history?"+query)
			if len(params) != 1 || params[0] != "status" {
				t.Fatalf("the refusal names %v, want exactly status", params)
			}
			if rules["status"] != "status-not-terminal" {
				t.Errorf("status was refused as %q, want the stable token status-not-terminal", rules["status"])
			}
		})
	}

	// The refusal quotes what it refused, bounded.
	_, _, body := fetch(t, ts.URL+"/api/history?status="+strings.Repeat("x", 500))
	if len(body) > 1500 {
		t.Errorf("a 500-byte status produced a %d-byte refusal: the echo is not bounded", len(body))
	}
	if !strings.Contains(string(body), strings.Repeat("x", 64)) || strings.Contains(string(body), strings.Repeat("x", 65)) {
		t.Errorf("the refusal must quote the first 64 bytes of the value it refused and no more: %s", body)
	}
	_, _, body = fetch(t, ts.URL+"/api/history?status=pending")
	if !strings.Contains(string(body), `\"pending\"`) {
		t.Errorf("the refusal does not quote the value it refused: %s", body)
	}
	for _, name := range terminalWords {
		if !strings.Contains(string(body), name) {
			t.Errorf("the refusal does not state the vocabulary (%s missing): %s", name, body)
		}
	}
}

// --- AC-8 ------------------------------------------------------------------------------

// cursorOf encodes a token the way the server does, from JSON a test wrote. In that JSON
// the path and the fingerprint are base64 (L2xpYi9hYzgvMDUubWt2 is /lib/ac8/05.mkv, YTph is a:a): the
// token carries them as bytes, since a path need not be UTF-8.
func cursorOf(raw string) string { return base64.RawURLEncoding.EncodeToString([]byte(raw)) }

func TestS0170_AC8_ACursorThisServerCannotDecodeIsRefused(t *testing.T) {
	l := newPagingLedger(t)
	for i := 0; i < 12; i++ {
		l.add(fmt.Sprintf("/lib/ac8/%02d.mkv", i), "a:a", store.Done, pagingBase-int64(i))
	}
	ts := l.serve()
	first := page(t, ts.URL, url.Values{"limit": {"5"}})
	if first.NextCursor == nil {
		t.Fatal("the fixture fits one page")
	}
	good := *first.NextCursor
	// The fixture's own idea of a well-formed token is accepted, so each refusal below is
	// of the ONE thing that case changes.
	wellFormed := cursorOf(`{"v":1,"u":1699999990,"p":"L2xpYi9hYzgvMDUubWt2","f":"YTph","s":""}`)
	if code, _, body := fetch(t, ts.URL+"/api/history?cursor="+wellFormed); code != http.StatusOK {
		t.Fatalf("a well-formed token answered %d: %s", code, body)
	}

	for name, token := range map[string]string{
		"garbage":                "!!!not-a-token!!!",
		"empty":                  "",
		"truncated":              good[:len(good)/2],
		"truncated by one":       good[:len(good)-1],
		"padded base64":          good + "=",
		"standard alphabet":      "+/+/",
		"not JSON":               cursorOf("these are words"),
		"a JSON array":           cursorOf(`[1,2,3]`),
		"an unknown field":       cursorOf(`{"v":1,"u":1699999990,"p":"L2xpYi9hYzgvMDUubWt2","f":"YTph","s":"","x":1}`),
		"another version":        cursorOf(`{"v":2,"u":1699999990,"p":"L2xpYi9hYzgvMDUubWt2","f":"YTph","s":""}`),
		"no version":             cursorOf(`{"u":1699999990,"p":"L2xpYi9hYzgvMDUubWt2","f":"YTph","s":""}`),
		"no path":                cursorOf(`{"v":1,"u":1699999990,"p":"","f":"YTph","s":""}`),
		"a negative stamp":       cursorOf(`{"v":1,"u":-1,"p":"L2xpYi9hYzgvMDUubWt2","f":"YTph","s":""}`),
		"a stamp that is words":  cursorOf(`{"v":1,"u":"then","p":"L2xpYi9hYzgvMDUubWt2","f":"YTph","s":""}`),
		"two objects":            cursorOf(`{"v":1,"u":1699999990,"p":"L2xpYi9hYzgvMDUubWt2","f":"YTph","s":""}{"v":1}`),
		"an object then garbage": cursorOf(`{"v":1,"u":1699999990,"p":"L2xpYi9hYzgvMDUubWt2","f":"YTph","s":""} x`),
	} {
		t.Run(name, func(t *testing.T) {
			params, rules := refused(t, ts.URL+"/api/history?limit=5&cursor="+url.QueryEscape(token))
			if len(params) != 1 || params[0] != "cursor" {
				t.Fatalf("the refusal names %v, want exactly cursor", params)
			}
			if rules["cursor"] != "cursor-undecodable" {
				t.Errorf("cursor was refused as %q, want the stable token cursor-undecodable", rules["cursor"])
			}
		})
	}

	t.Run("a stamp of zero is a position", func(t *testing.T) {
		zero := cursorOf(`{"v":1,"u":0,"p":"L2xpYi9hYzgvMDUubWt2","f":"YTph","s":""}`)
		p := page(t, ts.URL, url.Values{"cursor": {zero}})
		if len(p.History) != 0 || p.NextCursor != nil {
			t.Fatalf("nothing is older than the epoch: got %d rows", len(p.History))
		}
	})

	t.Run("given twice", func(t *testing.T) {
		q := "cursor=" + url.QueryEscape(good) + "&cursor=" + url.QueryEscape(good)
		_, rules := refused(t, ts.URL+"/api/history?"+q)
		if rules["cursor"] != "cursor-undecodable" {
			t.Errorf("two cursors were refused as %q, want cursor-undecodable", rules["cursor"])
		}
	})

	t.Run("status and cursor both invalid are named in one response", func(t *testing.T) {
		params, rules := refused(t, ts.URL+"/api/history?status=pending&cursor=garbage")
		if fmt.Sprint(params) != "[status cursor]" {
			t.Fatalf("the one response names %v, want status and cursor", params)
		}
		if rules["status"] != "status-not-terminal" || rules["cursor"] != "cursor-undecodable" {
			t.Errorf("rules = %v", rules)
		}
	})

	t.Run("an invalid status beside a readable cursor names only the status", func(t *testing.T) {
		params, _ := refused(t, ts.URL+"/api/history?status=pending&cursor="+url.QueryEscape(good))
		if fmt.Sprint(params) != "[status]" {
			t.Fatalf("the refusal names %v: with no readable status there is no set to hold the cursor to", params)
		}
	})
}

// --- AC-9 ------------------------------------------------------------------------------

func TestS0170_AC9_ACursorIsBoundToTheStatusSetItWasMintedUnder(t *testing.T) {
	l := newPagingLedger(t)
	for i := 0; i < 40; i++ {
		l.add(fmt.Sprintf("/lib/ac9/%02d.mkv", i), "a:a", store.Status(terminalWords[i%3]), pagingBase-int64(i/5))
	}
	ts := l.serve()
	mint := func(status ...string) string {
		q := url.Values{"limit": {"4"}}
		if len(status) > 0 {
			q["status"] = status
		}
		p := page(t, ts.URL, q)
		if p.NextCursor == nil {
			t.Fatalf("status %v fits one page", status)
		}
		return *p.NextCursor
	}
	unfiltered, doneFailed, done := mint(), mint("done,failed"), mint("done")

	for _, tc := range []struct {
		name, cursor, query string
	}{
		{"minted unfiltered, presented with a filter", unfiltered, "status=done"},
		{"minted under a filter, presented unfiltered", doneFailed, ""},
		{"minted under two, presented under one", doneFailed, "status=done"},
		{"minted under one, presented under two", done, "status=done,failed"},
		{"minted under one, presented under another", done, "status=failed"},
		{"minted unfiltered, presented with every status named", unfiltered, "status=" + strings.Join(terminalWords, ",")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params, rules := refused(t, ts.URL+"/api/history?limit=4&"+tc.query+"&cursor="+url.QueryEscape(tc.cursor))
			if fmt.Sprint(params) != "[cursor]" {
				t.Fatalf("the refusal names %v, want exactly cursor", params)
			}
			if rules["cursor"] != "cursor-filter-mismatch" {
				t.Errorf("cursor was refused as %q, want cursor-filter-mismatch, a token of its own", rules["cursor"])
			}
		})
	}

	want := l.reference("done", "failed")[4:8]
	for _, spelling := range []string{
		"status=done,failed", "status=failed,done", "status=done&status=failed",
		"status=failed&status=done", "status=failed,done,failed",
	} {
		t.Run("accepted: "+spelling, func(t *testing.T) {
			code, _, body := fetch(t, ts.URL+"/api/history?limit=4&"+spelling+"&cursor="+url.QueryEscape(doneFailed))
			if code != http.StatusOK {
				t.Fatalf("the same set in another spelling answered %d: %s", code, body)
			}
			var p historyWire
			if err := json.Unmarshal(body, &p); err != nil {
				t.Fatal(err)
			}
			if !sameIDs(p.ids(t), want) {
				t.Fatalf("the continued page is %v, want %v", p.ids(t), want)
			}
		})
	}
	t.Run("accepted: unfiltered under unfiltered", func(t *testing.T) {
		p := page(t, ts.URL, url.Values{"limit": {"4"}, "cursor": {unfiltered}})
		if !sameIDs(p.ids(t), l.reference(terminalWords...)[4:8]) {
			t.Fatal("an unfiltered cursor did not continue the unfiltered traversal")
		}
	})
}

// --- AC-10 -----------------------------------------------------------------------------

func TestS0170_AC10_AFilterThatMatchesNothingIsAnEmptyPageAndNotARefusal(t *testing.T) {
	l := newPagingLedger(t)
	l.add("/lib/ac10/a.mkv", "a:a", store.Done, pagingBase)
	l.pending("/lib/ac10/q.mkv")
	ts := l.serve()

	code, _, body := fetch(t, ts.URL+"/api/history?status=indeterminate")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		t.Fatal(err)
	}
	if string(keys["history"]) != "[]" {
		t.Errorf("history = %s, want an empty array", keys["history"])
	}
	if string(keys["next_cursor"]) != "null" {
		t.Errorf("next_cursor = %s, want null", keys["next_cursor"])
	}
	var p historyWire
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	requireAvailable(t, "history_total", p.HistoryTotal, 0, historyLimit)
	var age struct {
		HistoryTotal struct {
			Age *int64 `json:"age_seconds"`
		} `json:"history_total"`
	}
	if err := json.Unmarshal(body, &age); err != nil {
		t.Fatal(err)
	}
	if age.HistoryTotal.Age == nil || *age.HistoryTotal.Age != 0 {
		t.Errorf("a filtered total is read for the response, so its age is 0: got %v", age.HistoryTotal.Age)
	}
}

// --- AC-11 -----------------------------------------------------------------------------

func TestS0170_AC11_AnUnreadableFilteredTotalStillShipsThePageAndItsCursor(t *testing.T) {
	l := newPagingLedger(t)
	for i := 0; i < 12; i++ {
		l.add(fmt.Sprintf("/lib/ac11/%02d.mkv", i), "a:a", store.Failed, pagingBase-int64(i/3))
	}
	want := l.reference("failed")
	if len(want) != 12 {
		t.Fatalf("the fixture holds %d failed rows", len(want))
	}

	broken := countFailingStore{SQLite: l.st}
	ctrl := NewController(context.Background(), func(context.Context) error { return nil }, discard())
	hub := NewHub(broken, ctrl, discard())
	ts := httptest.NewServer(New(context.Background(), configZero(), secret.Value{}, secret.Value{}, broken, ctrl, hub, nil, discard()))
	defer ts.Close()

	q := url.Values{"status": {"failed"}, "limit": {"5"}}
	code, _, body := fetch(t, ts.URL+"/api/history?"+q.Encode())
	if code != http.StatusOK {
		t.Fatalf("an unreadable total answered %d: %s", code, body)
	}
	if !strings.Contains(string(body), `"count":null`) {
		t.Errorf("an unreadable total must go out as an explicit null count: %s", body)
	}
	if !strings.Contains(string(body), `"age_seconds":null`) {
		t.Errorf("an unreadable total has no value for an age to be the age of: %s", body)
	}
	first := page(t, ts.URL, q)
	if !sameIDs(first.ids(t), want[:5]) {
		t.Fatal("an unreadable total cost the caller its rows")
	}
	requireUnavailable(t, "history_total", first.HistoryTotal)
	if first.HistoryTotal.Cap != 5 {
		t.Errorf("cap = %d, want the limit applied", first.HistoryTotal.Cap)
	}
	if first.NextCursor == nil {
		t.Fatal("rows remain and next_cursor is null")
	}
	got, sizes := traverse(t, ts.URL, q)
	if !sameIDs(got, want) || fmt.Sprint(sizes) != "[5 5 2]" {
		t.Fatalf("the traversal under an unreadable total served pages of %v", sizes)
	}
}

// --- AC-12 -----------------------------------------------------------------------------

func TestS0170_AC12_TheReadGateAnswersBeforeAnyParameterIsValidated(t *testing.T) {
	h := newHarnessWith(t, "control-token", "read-token")
	ts := httptest.NewServer(h.srv)
	defer ts.Close()

	for _, query := range []string{"status=bogus", "cursor=garbage", "status=&cursor="} {
		for _, auth := range []string{"", "Bearer wrong"} {
			resp, body := get(t, ts.URL, "/api/history?"+query, auth)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s with Authorization %q answered %d, want 401 before any validation: %s",
					query, auth, resp.StatusCode, body)
			}
			if strings.Contains(body, "invalid-query") || strings.Contains(body, "parameters") {
				t.Errorf("an unauthenticated caller was told about its parameters: %s", body)
			}
		}
		for _, token := range []string{"read-token", "control-token"} {
			resp, body := get(t, ts.URL, "/api/history?"+query, "Bearer "+token)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%s with a valid token answered %d, want the 400: %s", query, resp.StatusCode, body)
			}
		}
	}
}

// --- AC-13 -----------------------------------------------------------------------------

func TestS0170_AC13_TheSurfaceDocumentDescribesTheRefusalAndTheCursor(t *testing.T) {
	l := newPagingLedger(t)
	for i := 0; i < 6; i++ {
		l.add(fmt.Sprintf("/lib/ac13/%02d.mkv", i), "a:a", store.Done, pagingBase)
	}
	ts := l.serve()

	code, _, raw := fetch(t, ts.URL+SchemaPath)
	if code != http.StatusOK {
		t.Fatalf("GET %s: %d", SchemaPath, code)
	}
	doc, err := ParseDocument(raw)
	if err != nil {
		t.Fatalf("the served document does not parse: %v", err)
	}
	byStatus := map[int]Response{}
	for _, ep := range doc.Endpoints {
		if ep.Method == http.MethodGet && ep.Path == "/api/history" {
			for _, r := range ep.Responses {
				byStatus[r.Status] = r
			}
		}
	}
	field := func(sh Shape, name string) (Field, bool) {
		for _, f := range sh.Fields {
			if f.Name == name {
				return f, true
			}
		}
		return Field{}, false
	}

	ok, found := byStatus[http.StatusOK]
	if !found {
		t.Fatal("the document declares no 200 for GET /api/history")
	}
	next, found := field(ok.Body, "next_cursor")
	if !found || !next.Required || next.Type.Kind != kindString || !next.Type.Nullable {
		t.Errorf("the 200 body must declare next_cursor as a required, nullable string: %+v (declared %v)", next, found)
	}

	bad, found := byStatus[http.StatusBadRequest]
	if !found {
		t.Fatal("the document declares no 400 for GET /api/history")
	}
	if bad.MediaType != mediaJSON || bad.Body.Kind != kindObject {
		t.Fatalf("the 400 is declared as %q / %s, want a JSON object", bad.MediaType, bad.Body.Kind)
	}
	for name, kind := range map[string]string{"rule": kindString, "error": kindString, "retryable": kindBoolean, "parameters": kindArray} {
		f, found := field(bad.Body, name)
		if !found || !f.Required || f.Type.Kind != kind {
			t.Errorf("the 400 body must declare %s as a required %s: %+v (declared %v)", name, kind, f, found)
		}
	}
	params, _ := field(bad.Body, "parameters")
	if params.Type.Elem == nil {
		t.Fatal("parameters declares no element")
	}
	for _, name := range []string{"parameter", "rule", "error"} {
		if f, found := field(*params.Type.Elem, name); !found || f.Type.Kind != kindString || !f.Required {
			t.Errorf("a parameters entry must declare %s as a required string", name)
		}
	}

	// Derived from the types the handler encodes: the LIVE bodies validate against the
	// document, with no field undeclared and none missing.
	for _, tc := range []struct {
		query  string
		status int
	}{
		{"limit=2", http.StatusOK},
		{"status=done&limit=6", http.StatusOK},
		{"status=bogus&cursor=", http.StatusBadRequest},
	} {
		code, _, body := fetch(t, ts.URL+"/api/history?"+tc.query)
		if code != tc.status {
			t.Fatalf("%s answered %d, want %d", tc.query, code, tc.status)
		}
		violations, err := doc.ValidateResponse(http.MethodGet, "/api/history", code, body)
		if err != nil {
			t.Fatalf("%s: %v", tc.query, err)
		}
		if len(violations) > 0 {
			t.Errorf("%s: the live body disagrees with the document: %v", tc.query, violations)
		}
	}
}

// AC-3, for a path that is not UTF-8. A file name is whatever bytes the filesystem holds,
// and the ledger keeps them. A cursor that carried the path as a JSON string would rewrite
// each such byte to U+FFFD: a position that sorts after its row skips the rows between,
// and one that sorts before its row serves that row on every page and never ends.
func TestS0170_AC3_APathThatIsNotUTF8IsAPositionLikeAnyOther(t *testing.T) {
	l := newPagingLedger(t)
	const at = 1_700_000_000
	for i, path := range []string{
		"/lib/a.mkv",
		"/lib/caf\xe9.mkv", "/lib/caf\xea.mkv", // below U+FFFD's first byte: a rewritten position skips forward
		"/lib/caf\xf5.mkv", "/lib/caf\xf6.mkv", // above it: a rewritten position falls back behind its own row
		"/lib/z.mkv",
	} {
		l.add(path, "fp"+strconv.Itoa(i), store.Done, at)
	}
	ts := l.serve()
	want := l.reference("done")
	if len(want) != 6 {
		t.Fatalf("the fixture holds %d rows, want 6", len(want))
	}
	for _, limit := range []string{"1", "2", "5"} {
		got, sizes := traverse(t, ts.URL, url.Values{"limit": {limit}})
		if !sameIDs(got, want) {
			t.Errorf("limit=%s served rows %v over pages %v, want every row once in the ledger's order %v",
				limit, got, sizes, want)
		}
	}
}
