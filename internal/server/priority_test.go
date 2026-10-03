package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/secret"
	"github.com/NSchatz/holdfast/internal/store"
)

// A job row's `priority`: what the configuration's priority is for the file, an explicit
// null where that is not known, on every projection, and never anything but a display.

func intp(v int) *int { return &v }

// priorityConfig is three roots: one whose priority comes from the root alone, one whose
// rules band on the source's height and name priorities, and one that names none.
func priorityConfig() config.Config {
	return config.Config{
		Roots: []config.Root{
			{Path: "/mnt/tv", Clean: "/mnt/tv", Priority: intp(3)},
			{Path: "/mnt/films", Clean: "/mnt/films", Priority: intp(5), Profile: config.Profile{Rules: config.Rules{
				{When: config.Band{MaxSourceHeight: intp(576)}, Priority: intp(50)},
				{When: config.Band{MinSourceHeight: intp(2000)}},
				{When: config.Band{MinSourceHeight: intp(1000)}, Priority: intp(-7)},
			}}},
			{Path: "/mnt/plain", Clean: "/mnt/plain"},
		},
		EncodeProfiles: []config.EncodeProfile{{Name: "anime", Match: "**/anime/**", Priority: intp(20)}},
	}
}

// priorityRow is one fixture row and the priority it must be served with (nil: null).
type priorityRow struct {
	path   string
	height *int
	want   *int
}

var priorityRows = []priorityRow{
	// A root-level priority needs no height: the row records none and is still answered.
	{"/mnt/tv/show/e01.mkv", nil, intp(3)},
	{"/mnt/tv/show/e02.mkv", intp(1080), intp(3)},
	// An encode profile's priority is ahead of the root's.
	{"/mnt/tv/anime/e01.mkv", nil, intp(20)},
	// A banded rule's priority: the band the recorded height falls in.
	{"/mnt/films/sd.mkv", intp(480), intp(50)},
	{"/mnt/films/sd-edge.mkv", intp(576), intp(50)},
	{"/mnt/films/hd.mkv", intp(1080), intp(-7)},
	// 2160 is decided by a rule that names no priority, and 720 by no rule: the root's.
	{"/mnt/films/uhd.mkv", intp(2160), intp(5)},
	{"/mnt/films/720.mkv", intp(720), intp(5)},
	// The same root with NO recorded height: the band is not known, so neither is the
	// priority. Never the root's 5, and never the 50 a height of 0 would fall in.
	{"/mnt/films/unprobed.mkv", nil, nil},
	{"/mnt/films/zero.mkv", intp(0), nil},
	// A root that names no priority: 0 is the priority, and it is not null.
	{"/mnt/plain/a.mkv", nil, intp(0)},
	// Under no configured root: null. "/mnt/tv-old" begins with a root's text and is not in it.
	{"/elsewhere/a.mkv", intp(1080), nil},
	{"/mnt/tv-old/a.mkv", nil, nil},
}

// seedPriorityRows writes each fixture row twice: once terminal (the history's) and once
// pending under a `.q` suffix (the queue's).
func seedPriorityRows(l *pagingLedger) {
	for i, r := range priorityRows {
		for _, row := range []struct{ path, status string }{{r.path, "done"}, {r.path + ".q", "pending"}} {
			var height any
			if r.height != nil {
				height = *r.height
			}
			l.insert(row.path, "a:a", row.status, pagingBase-int64(i), int64(i+1))
			l.flush()
			if _, err := l.raw.Exec(`UPDATE jobs SET source_height = ? WHERE path = ?`, height, row.path); err != nil {
				l.t.Fatalf("seed height: %v", err)
			}
		}
	}
}

// priorityServer serves a ledger through a hub with the given resolver (nil: none wired).
func priorityServer(t *testing.T, l *pagingLedger, resolve PriorityResolver) (*httptest.Server, *Hub) {
	t.Helper()
	l.flush()
	ctrl := NewController(context.Background(), func(context.Context) error { return nil }, discard())
	hub := NewHub(l.st, ctrl, discard())
	if resolve != nil {
		hub.SetPriority(resolve)
	}
	cfg := priorityConfig()
	ts := httptest.NewServer(New(context.Background(), cfg, secret.NewValue("control"), secret.Value{}, l.st, ctrl, hub, nil, discard()))
	t.Cleanup(ts.Close)
	return ts, hub
}

// rowsOf decodes one array of job rows out of a body, keeping every field raw so a null is
// told from a number and an absent key from both.
func rowsOf(t *testing.T, body []byte, key string) map[string]map[string]json.RawMessage {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(top[key], &rows); err != nil {
		t.Fatalf("decode %s: %v", key, err)
	}
	out := map[string]map[string]json.RawMessage{}
	for _, r := range rows {
		var path string
		if err := json.Unmarshal(r["path"], &path); err != nil {
			t.Fatal(err)
		}
		out[path] = r
	}
	return out
}

// requirePriority asserts one served row carries the key, as the wanted number or as null.
func requirePriority(t *testing.T, where, path string, row map[string]json.RawMessage, want *int) {
	t.Helper()
	if row == nil {
		t.Errorf("%s: no row for %s", where, path)
		return
	}
	raw, present := row["priority"]
	if !present {
		t.Errorf("%s: %s carries no priority key; not known is an explicit null", where, path)
		return
	}
	wantText := "null"
	if want != nil {
		wantText = fmt.Sprint(*want)
	}
	if string(raw) != wantText {
		t.Errorf("%s: %s has priority %s, want %s", where, path, raw, wantText)
	}
}

func getWithToken(t *testing.T, url, token string) []byte {
	t.Helper()
	auth := ""
	if token != "" {
		auth = "Bearer " + token
	}
	resp, body := get(t, url, "", auth)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", url, resp.StatusCode, body)
	}
	return []byte(body)
}

// everyProjection reads the fixture rows off each surface that serves job rows.
func everyProjection(t *testing.T, ts *httptest.Server, hub *Hub) map[string]map[string]map[string]json.RawMessage {
	t.Helper()
	snap, err := hub.SnapshotJSON(context.Background())
	if err != nil {
		t.Fatalf("SnapshotJSON: %v", err)
	}
	return map[string]map[string]map[string]json.RawMessage{
		"GET /api/queue":            rowsOf(t, getWithToken(t, ts.URL+"/api/queue", ""), "queue"),
		"GET /api/history":          rowsOf(t, getWithToken(t, ts.URL+"/api/history", ""), "history"),
		"GET /api/history?status":   rowsOf(t, getWithToken(t, ts.URL+"/api/history?status=done&limit=100", ""), "history"),
		"the SSE snapshot queue":    rowsOf(t, snap, "queue"),
		"the SSE snapshot history":  rowsOf(t, snap, "history"),
		"GET /api/search (control)": rowsOf(t, getWithToken(t, ts.URL+"/api/search?path=/", "control"), "results"),
	}
}

// suffixOf is the path a projection's rows carry for a fixture row: the pending copy on the
// queue, the terminal one everywhere else.
func suffixOf(where string) string {
	if where == "GET /api/queue" || where == "the SSE snapshot queue" {
		return ".q"
	}
	return ""
}

func TestPriority_EveryRowCarriesWhatTheConfigurationAnswersForIt(t *testing.T) {
	l := newPagingLedger(t)
	seedPriorityRows(l)
	ts, hub := priorityServer(t, l, ConfigPriority(priorityConfig()))

	for where, rows := range everyProjection(t, ts, hub) {
		for _, r := range priorityRows {
			path := r.path + suffixOf(where)
			requirePriority(t, where, path, rows[path], r.want)
		}
	}
}

// With no resolver wired - the zero value, and what every caller of NewHub before this
// field got - every row's priority is null and the key is still there.
func TestPriority_NoResolverServesNullOnEveryRow(t *testing.T) {
	l := newPagingLedger(t)
	seedPriorityRows(l)
	ts, hub := priorityServer(t, l, nil)

	for where, rows := range everyProjection(t, ts, hub) {
		for _, r := range priorityRows {
			path := r.path + suffixOf(where)
			requirePriority(t, where, path, rows[path], nil)
		}
	}

	// The export's row is projected with no hub at all: the same explicit null.
	line, err := HistoryRowJSON(store.Job{Path: "/mnt/tv/show/e01.mkv", Status: store.Done})
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]json.RawMessage
	if err := json.Unmarshal(line, &row); err != nil {
		t.Fatal(err)
	}
	requirePriority(t, "HistoryRowJSON", "/mnt/tv/show/e01.mkv", row, nil)
}

// The resolver itself, one case at a time, against Config.PriorityOf for the cases it
// answers: the field is what the queue's own ordering reads, or it is null.
func TestConfigPriority_IsPriorityOfOrNull(t *testing.T) {
	cfg := priorityConfig()
	resolve := ConfigPriority(cfg)
	for _, r := range priorityRows {
		got := resolve(r.path, r.height)
		if (got == nil) != (r.want == nil) || (got != nil && *got != *r.want) {
			t.Errorf("%s (height %v): got %v, want %v", r.path, deref(r.height), deref(got), deref(r.want))
		}
		if got == nil {
			continue
		}
		root, ok := cfg.RootFor(r.path)
		if !ok {
			t.Errorf("%s: a priority was answered for a path under no root", r.path)
			continue
		}
		height := 0
		if r.height != nil {
			height = *r.height
		}
		if want := cfg.PriorityOf(root, r.path, height); *got != want {
			t.Errorf("%s: the row says %d and Config.PriorityOf says %d", r.path, *got, want)
		}
	}
	// A height of 1 is a height: the least one that is recorded.
	if got := resolve("/mnt/films/tiny.mkv", intp(1)); got == nil || *got != 50 {
		t.Errorf("a recorded height of 1 answered %v, want the band's 50", deref(got))
	}
	if got := resolve("/mnt/films/neg.mkv", intp(-1)); got != nil {
		t.Errorf("a height of -1 is not a recorded height and answered %d", *got)
	}
}

func deref(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// Display only: reading the rows writes nothing, and no request can set the field.
func TestPriority_IsADisplayAndNothingSetsIt(t *testing.T) {
	l := newPagingLedger(t)
	seedPriorityRows(l)
	ts, hub := priorityServer(t, l, ConfigPriority(priorityConfig()))

	before, err := l.st.List(context.Background(), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	everyProjection(t, ts, hub)
	// A request that names a priority, by every route a caller might try, moves nothing.
	for _, try := range []struct{ method, path string }{
		{http.MethodPost, "/api/queue"}, {http.MethodPut, "/api/queue"}, {http.MethodPatch, "/api/queue"},
		{http.MethodPost, "/api/history"}, {http.MethodPost, "/api/priority"}, {http.MethodPut, "/api/priority"},
		{http.MethodPatch, "/api/priority"}, {http.MethodPost, "/api/queue/priority"},
	} {
		req, _ := http.NewRequest(try.method, ts.URL+try.path+"?priority=999",
			strings.NewReader(`{"path":"/mnt/tv/show/e01.mkv","priority":999}`))
		req.Header.Set("Authorization", "Bearer control")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s answered %d: no route takes a priority", try.method, try.path, resp.StatusCode)
		}
	}
	for where, rows := range everyProjection(t, ts, hub) {
		path := "/mnt/tv/show/e01.mkv" + suffixOf(where)
		requirePriority(t, where, path, rows[path], intp(3))
	}
	after, err := l.st.List(context.Background(), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Error("reading the rows' priority changed the ledger")
	}
}
