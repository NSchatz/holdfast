package playback

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/schedule"
	"github.com/NSchatz/holdfast/internal/secret"
)

// fakeKey is the resolved api key every fake Tautulli below expects. Not credential-shaped: the
// secret scanner reads this file.
const fakeKey = "SYNTHETIC-TAUTULLI-KEY"

// fakeTautulli is a Tautulli API v2 server built from the documented shapes: get_history pages
// through rows by start and length and reports recordsFiltered; get_metadata answers one rating
// key's item. It records every request.
type fakeTautulli struct {
	t        *testing.T
	mu       sync.Mutex
	rows     []map[string]any
	items    map[string]any // rating key -> get_metadata data
	history  []url.Values
	meta     map[string]int
	status   int    // non-zero: every request is answered with it
	result   string // non-empty: every answer carries this result
	body     string // non-empty: every answer is this body
	metaFail bool   // get_metadata answers 500
	noTotal  bool   // get_history omits recordsFiltered
}

func (f *fakeTautulli) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	if r.URL.Path != "/api/v2" || q.Get("apikey") != fakeKey {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if f.status != 0 {
		http.Error(w, "down", f.status)
		return
	}
	if f.body != "" {
		_, _ = w.Write([]byte(f.body))
		return
	}
	result := "success"
	if f.result != "" {
		result = f.result
	}
	var data any
	switch q.Get("cmd") {
	case "get_history":
		f.history = append(f.history, q)
		start, _ := strconv.Atoi(q.Get("start"))
		length, _ := strconv.Atoi(q.Get("length"))
		page := []map[string]any{}
		for i := start; i < start+length && i < len(f.rows); i++ {
			page = append(page, f.rows[i])
		}
		d := map[string]any{"draw": 1, "recordsTotal": len(f.rows) + 7, "data": page}
		if !f.noTotal {
			d["recordsFiltered"] = len(f.rows)
		}
		data = d
	case "get_metadata":
		if f.meta == nil {
			f.meta = map[string]int{}
		}
		f.meta[q.Get("rating_key")]++
		if f.metaFail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		item, ok := f.items[q.Get("rating_key")]
		if !ok {
			item = map[string]any{}
		}
		if item == "absent" {
			_ = json.NewEncoder(w).Encode(map[string]any{"response": map[string]any{"result": result}})
			return
		}
		data = item
	default:
		f.t.Errorf("unexpected command %q", q.Get("cmd"))
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"response": map[string]any{"result": result, "message": nil, "data": data}})
}

// newFake starts a fake Tautulli and returns the source reading it through the real client.
func newFake(t *testing.T, f *fakeTautulli) (*Tautulli, *httptest.Server) {
	t.Helper()
	f.t = f.tOr(t)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	keyFile := filepath.Join(t.TempDir(), "tautulli.key")
	if err := os.WriteFile(keyFile, []byte(fakeKey+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	ref, err := secret.ParseRef("tautulli_api_key", "file:"+keyFile)
	if err != nil {
		t.Fatal(err)
	}
	v, err := ref.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return &Tautulli{API: schedule.NewTautulli(srv.URL, ref, v).API}, srv
}

func (f *fakeTautulli) tOr(t *testing.T) *testing.T {
	if f.t != nil {
		return f.t
	}
	return t
}

// row is one history row as Tautulli serialises it.
func row(id int, started int64, ratingKey any, viewOffset any) map[string]any {
	r := map[string]any{
		"row_id": id, "reference_id": id, "started": started, "stopped": started + 600,
		"rating_key": ratingKey, "percent_complete": 23, "media_type": "movie", "live": 0,
		"platform": "Chrome", "player": "Plex Web", decisionField: "copy",
		"duration": 600, "play_duration": 600, "user": "viewer",
	}
	if viewOffset != nil {
		r["view_offset"] = viewOffset
	}
	return r
}

// item is one get_metadata answer naming the given part files.
func filmItem(duration any, files ...string) map[string]any {
	parts := []any{}
	for _, f := range files {
		parts = append(parts, map[string]any{"file": f, "file_size": "3200000000"})
	}
	return map[string]any{"rating_key": "77", "duration": duration, "media_type": "movie",
		"media_info": []any{map[string]any{"container": "mkv", "parts": parts}}}
}

const since = int64(1_790_000_000)

// TestTautulli_ReadsEveryPageInOrderWithTheDocumentedParameters: the history is read page by
// page, oldest first, ungrouped and without live sessions, from the day before since; the read
// ends when a page comes back short of the page size.
func TestTautulli_ReadsEveryPageInOrderWithTheDocumentedParameters(t *testing.T) {
	f := &fakeTautulli{items: map[string]any{"77": filmItem("8700000", "/data/media/movies/Film/Film.mkv")}}
	for i := 0; i < 5; i++ {
		f.rows = append(f.rows, row(100+i, since+int64(i), 77, int64(1000*i)))
	}
	src, _ := newFake(t, f)
	src.PageSize = 2
	rows, complete, err := src.Sessions(context.Background(), time.Unix(since, 0))
	if err != nil || !complete {
		t.Fatalf("Sessions: complete %v, err %v", complete, err)
	}
	if len(rows) != 5 || rows[0].ID != "100" || rows[4].ID != "104" {
		t.Fatalf("rows %+v", rows)
	}
	if len(f.history) != 3 {
		t.Fatalf("%d get_history requests, want 3 pages of 2", len(f.history))
	}
	wantAfter := time.Unix(since, 0).Add(-24 * time.Hour).UTC().Format("2006-01-02")
	for i, q := range f.history {
		for k, want := range map[string]string{"grouping": "0", "include_activity": "0", "order_column": "started",
			"order_dir": "asc", "after": wantAfter, "length": "2", "start": strconv.Itoa(2 * i)} {
			if q.Get(k) != want {
				t.Errorf("page %d: %s=%q, want %q", i, k, q.Get(k), want)
			}
		}
	}
	if f.meta["77"] != 1 {
		t.Errorf("the rating key's metadata was asked %d times in one read, want once", f.meta["77"])
	}
	r := rows[3]
	if r.File != "/data/media/movies/Film/Film.mkv" || r.Started != since+3 || r.Stopped == nil || *r.Stopped != since+603 ||
		r.OffsetMS == nil || *r.OffsetMS != 3000 || r.DurationMS == nil || *r.DurationMS != 8_700_000 ||
		r.Platform != "Chrome" || r.Player != "Plex Web" || r.Decision != "copy" {
		t.Errorf("row %+v", r)
	}
	// A zero page size and page bound read as the defaults.
	if src.Name() != "tautulli" || DefaultPageSize != 100 || DefaultMaxPages != 50 {
		t.Errorf("name %q, defaults %d %d", src.Name(), DefaultPageSize, DefaultMaxPages)
	}
}

// TestTautulli_TheReadEndsAtRecordsFilteredOrAtItsPageBound: a full last page ends the read when
// recordsFiltered says so, without asking for an empty page; with no total, the empty page ends
// it; and a history longer than the page bound returns the oldest rows as incomplete.
func TestTautulli_TheReadEndsAtRecordsFilteredOrAtItsPageBound(t *testing.T) {
	mk := func() *fakeTautulli {
		f := &fakeTautulli{items: map[string]any{"77": filmItem(8700000, "/m/Film.mkv")}}
		for i := 0; i < 4; i++ {
			f.rows = append(f.rows, row(i+1, since+int64(i), "77", 0))
		}
		return f
	}
	f := mk()
	src, _ := newFake(t, f)
	src.PageSize = 2
	if rows, complete, err := src.Sessions(context.Background(), time.Unix(since, 0)); err != nil || !complete || len(rows) != 4 {
		t.Fatalf("exact pages: %d rows, complete %v, %v", len(rows), complete, err)
	}
	if len(f.history) != 2 {
		t.Errorf("%d requests for 4 rows in pages of 2 with the total stated, want 2", len(f.history))
	}

	f = mk()
	f.noTotal = true
	src, _ = newFake(t, f)
	src.PageSize = 2
	if rows, complete, err := src.Sessions(context.Background(), time.Unix(since, 0)); err != nil || !complete || len(rows) != 4 {
		t.Fatalf("no total: %d rows, complete %v, %v", len(rows), complete, err)
	}
	if len(f.history) != 3 {
		t.Errorf("%d requests with no total, want 3 (the third comes back empty)", len(f.history))
	}

	f = mk()
	src, _ = newFake(t, f)
	src.PageSize, src.MaxPages = 1, 3
	rows, complete, err := src.Sessions(context.Background(), time.Unix(since, 0))
	if err != nil || complete || len(rows) != 3 || rows[2].ID != "3" {
		t.Fatalf("page bound: %d rows, complete %v, %v", len(rows), complete, err)
	}
	if len(f.history) != 3 {
		t.Errorf("%d requests under a bound of 3 pages", len(f.history))
	}
}

// TestTautulli_RowsItCannotUseAreSkippedOrKeptWithNoFile: a row that started before since, a
// live-TV row, a music or photo row and a row with no id are skipped; an id falls back to `id`;
// an item that names two files, or none, or that Tautulli no longer knows, is kept with no file;
// a missing view_offset is taken from percent_complete; numbers sent as strings are read.
func TestTautulli_RowsItCannotUseAreSkippedOrKeptWithNoFile(t *testing.T) {
	f := &fakeTautulli{items: map[string]any{
		"1": filmItem("8700000", "/m/One.mkv"),
		"2": filmItem(8700000, "/m/Two-1080p.mkv", "/m/Two-2160p.mkv"),
		"3": filmItem(8700000),
		"5": filmItem(nil, "/m/NoLength.mkv"),
	}}
	before := row(1, since-1, 1, 5)
	live := row(2, since, 1, 5)
	live["live"] = 1
	track := row(3, since, 1, 5)
	track["media_type"] = "track"
	photo := row(4, since, 1, 5)
	photo["media_type"] = "photo"
	noID := row(5, since, 1, 5)
	delete(noID, "row_id")
	byID := row(0, since+1, "1", "5000")
	delete(byID, "row_id")
	byID["id"] = "600"
	twoFiles := row(7, since+2, 2, 1)
	noFiles := row(8, since+3, 3, 1)
	unknown := row(9, since+4, 4, 1)
	fromPercent := row(10, since+5, "1", nil)
	fromPercent["percent_complete"] = "23"
	noLength := row(11, since+6, 5, nil)
	stringStarted := row(12, 0, 1, nil)
	stringStarted["started"] = strconv.FormatInt(since+7, 10)
	stringStarted["stopped"] = nil
	stringStarted["percent_complete"] = nil
	noKey := row(13, since+8, nil, 1)
	zeroStopped := row(14, since+9, 1, 0)
	zeroStopped["stopped"] = 0
	negative := row(15, since+10, 1, -5)
	negative["percent_complete"] = 0
	full := row(16, since+11, 1, nil)
	full["percent_complete"] = 100
	over := row(17, since+12, 1, nil)
	over["percent_complete"] = 150
	under := row(18, since+13, 1, nil)
	under["percent_complete"] = -1
	gone := row(19, since+14, 6, 1)
	f.rows = []map[string]any{before, live, track, photo, noID, byID, twoFiles, noFiles, unknown, fromPercent,
		noLength, stringStarted, noKey, zeroStopped, negative, full, over, under, gone}
	f.items["6"] = "absent" // answered with no data at all
	src, _ := newFake(t, f)
	rows, complete, err := src.Sessions(context.Background(), time.Unix(since, 0))
	if err != nil || !complete {
		t.Fatalf("Sessions: %v %v", complete, err)
	}
	got := map[string]Observed{}
	var ids []string
	for _, r := range rows {
		got[r.ID] = r
		ids = append(ids, r.ID)
	}
	if strings.Join(ids, ",") != "600,7,8,9,10,11,12,13,14,15,16,17,18,19" {
		t.Fatalf("kept rows %v, want 600 and 7 to 19", ids)
	}
	if r := got["14"]; r.Stopped != nil || r.OffsetMS == nil || *r.OffsetMS != 0 {
		t.Errorf("stopped 0 and view_offset 0: stopped %v, offset %v; want no stop and an offset of 0", r.Stopped, r.OffsetMS)
	}
	if r := got["15"]; r.OffsetMS == nil || *r.OffsetMS != 0 {
		t.Errorf("a negative view_offset with 0 percent: %v, want 0 from the percent", r.OffsetMS)
	}
	if r := got["16"]; r.OffsetMS == nil || *r.OffsetMS != 8_700_000 {
		t.Errorf("100 percent: %v, want the duration", r.OffsetMS)
	}
	if got["17"].OffsetMS != nil || got["18"].OffsetMS != nil {
		t.Errorf("a percent outside 0-100 was read as an offset: %v %v", got["17"].OffsetMS, got["18"].OffsetMS)
	}
	if r := got["19"]; r.File != "" || r.DurationMS != nil {
		t.Errorf("metadata with no data: %+v", r)
	}
	if r := got["600"]; r.File != "/m/One.mkv" || *r.OffsetMS != 5000 {
		t.Errorf("by id: %+v", r)
	}
	for _, id := range []string{"7", "8", "9"} {
		if got[id].File != "" {
			t.Errorf("row %s names %q, want no file", id, got[id].File)
		}
	}
	if r := got["10"]; r.OffsetMS == nil || *r.OffsetMS != 2_001_000 {
		t.Errorf("from 23 percent of 8700 s: %v, want 2001000 ms", r.OffsetMS)
	}
	if r := got["11"]; r.File != "/m/NoLength.mkv" || r.DurationMS != nil || r.OffsetMS != nil {
		t.Errorf("an item with no length: %+v", r)
	}
	if r := got["12"]; r.Started != since+7 || r.Stopped != nil || r.OffsetMS != nil {
		t.Errorf("string started, null stopped and percent: %+v", r)
	}
	if r := got["13"]; r.File != "" || r.DurationMS != nil {
		t.Errorf("no rating key: %+v", r)
	}
	if f.meta["4"] != 1 || f.meta["1"] != 1 || len(f.meta) != 6 {
		t.Errorf("metadata requests %v", f.meta)
	}
}

// TestTautulli_AFailedReadIsAFailureOfItsClassAndCarriesNoKey: every way a read can fail ends
// the read with a class and no rows, and none carries the api key.
func TestTautulli_AFailedReadIsAFailureOfItsClassAndCarriesNoKey(t *testing.T) {
	ok := func() *fakeTautulli {
		return &fakeTautulli{items: map[string]any{"77": filmItem(1, "/m/F.mkv")},
			rows: []map[string]any{row(1, since, 77, 1)}}
	}
	for _, tc := range []struct {
		name   string
		break_ func(f *fakeTautulli)
		class  string
	}{
		{"a 500", func(f *fakeTautulli) { f.status = 500 }, ClassRequest},
		{"an error result", func(f *fakeTautulli) { f.result = "error" }, ClassResult},
		{"not JSON", func(f *fakeTautulli) { f.body = "<html>" }, ClassUnparseable},
		{"no response object", func(f *fakeTautulli) { f.body = `{"data":{}}` }, ClassUnparseable},
		{"a history that is not a page", func(f *fakeTautulli) {
			f.body = `{"response":{"result":"success","data":{"data":"rows"}}}`
		}, ClassUnparseable},
		{"a row that is not an object", func(f *fakeTautulli) {
			f.body = `{"response":{"result":"success","data":{"recordsFiltered":1,"data":[7]}}}`
		}, ClassUnparseable},
		{"metadata that fails", func(f *fakeTautulli) { f.metaFail = true }, ClassRequest},
		{"metadata that is not an item", func(f *fakeTautulli) { f.items["77"] = map[string]any{"media_info": "x"} }, ClassUnparseable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := ok()
			tc.break_(f)
			src, _ := newFake(t, f)
			rows, complete, err := src.Sessions(context.Background(), time.Unix(since, 0))
			if err == nil || rows != nil || complete {
				t.Fatalf("rows %v, complete %v, err %v", rows, complete, err)
			}
			if ClassOf(err) != tc.class {
				t.Errorf("class %q, want %q (%v)", ClassOf(err), tc.class, err)
			}
			var se *SourceError
			if !errors.As(err, &se) || se.Unwrap() == nil || !strings.HasPrefix(err.Error(), tc.class+": ") {
				t.Errorf("error %v is not a SourceError naming its class", err)
			}
			if strings.Contains(err.Error(), fakeKey) || strings.Contains(err.Error(), "apikey") {
				t.Errorf("error %q carries the key", err)
			}
		})
	}
	// A cancelled read stops before it asks.
	f := ok()
	src, _ := newFake(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := src.Sessions(ctx, time.Unix(since, 0)); err == nil || len(f.history) != 0 {
		t.Errorf("a cancelled read: %v, %d requests", err, len(f.history))
	}
	if ClassOf(errors.New("plain")) != ClassRequest {
		t.Error("an error that is not a SourceError is not a request failure")
	}
}

// TestTautulli_NumbersInEveryShape: flexInt and flexFloat read a number or a numeric string and
// refuse everything else rather than reading it as 0.
func TestTautulli_NumbersInEveryShape(t *testing.T) {
	for raw, want := range map[string]int64{`2`: 2, `"2"`: 2, `" 7 "`: 7, `84.9`: 84, `"-3"`: -3, `0`: 0} {
		if n, ok := flexInt(json.RawMessage(raw)); !ok || n != want {
			t.Errorf("flexInt(%s) = %d %v, want %d", raw, n, ok, want)
		}
	}
	for raw, want := range map[string]int64{`9007199254740992`: 1 << 53, `-9007199254740992`: -(1 << 53)} {
		if n, ok := flexInt(json.RawMessage(raw)); !ok || n != want {
			t.Errorf("flexInt(%s) = %d %v, want %d: the largest exact whole number is a number", raw, n, ok, want)
		}
	}
	for _, raw := range []string{``, `null`, `""`, `"abc"`, `true`, `{}`, `[1]`, `"NaN"`, `"Inf"`, `1e300`, `-1e300`,
		`9007199254740994`, `-9007199254740994`} {
		if n, ok := flexInt(json.RawMessage(raw)); ok {
			t.Errorf("flexInt(%s) = %d, want not a number", raw, n)
		}
	}
	if f, ok := flexFloat(json.RawMessage(`"23.5"`)); !ok || f != 23.5 {
		t.Errorf("flexFloat = %g %v", f, ok)
	}
	for raw, want := range map[string]string{`"x"`: "x", `12`: "12", `null`: "", `true`: "", `" k "`: "k"} {
		if got := flexString(json.RawMessage(raw)); got != want {
			t.Errorf("flexString(%s) = %q, want %q", raw, got, want)
		}
	}
}
