package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/store"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newStore opens a temp SQLite store and seeds it with a couple of rows so the read
// endpoints have something to return.
func newStore(t *testing.T) *store.SQLite {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	// one active (encoding) + one terminal (done) row
	mustClaim(t, st, "/lib/active.mkv", "1:1")
	if err := st.Advance(ctx, "/lib/active.mkv", "1:1", store.Encoding); err != nil {
		t.Fatal(err)
	}
	mustClaim(t, st, "/lib/done.mkv", "2:2")
	if err := st.Finish(ctx, "/lib/done.mkv", "2:2", store.Done); err != nil {
		t.Fatal(err)
	}
	return st
}

func mustClaim(t *testing.T, st *store.SQLite, path, fp string) {
	t.Helper()
	ok, err := st.Claim(context.Background(), path, fp, "w0", 3)
	if err != nil || !ok {
		t.Fatalf("Claim(%s): ok=%v err=%v", path, ok, err)
	}
}

// harness bundles a wired Server with control seams for the fake scanner.
type harness struct {
	srv  *Server
	ctrl *Controller
	hub  *Hub
	// scanStarted receives once per scan invocation; scanRelease gates each scan's
	// completion (buffered so a test can pre-fill it for auto-completing scans).
	scanStarted chan struct{}
	scanRelease chan struct{}
}

func newHarness(t *testing.T, token string) *harness {
	t.Helper()
	st := newStore(t)
	h := &harness{
		scanStarted: make(chan struct{}, 8),
		scanRelease: make(chan struct{}, 8),
	}
	scan := func(ctx context.Context) error {
		h.scanStarted <- struct{}{}
		select {
		case <-h.scanRelease:
		case <-ctx.Done():
		}
		return nil
	}
	ctx := context.Background()
	h.ctrl = NewController(ctx, scan, discard())
	h.hub = NewHub(st, h.ctrl, discard())
	h.ctrl.SetOnChange(h.hub.Trigger)
	cfg := config.Config{ServerAuthToken: token}
	h.srv = New(ctx, cfg, st, h.ctrl, h.hub, nil, nil, discard())
	return h
}

func TestReadEndpoints(t *testing.T) {
	h := newHarness(t, "")
	ts := httptest.NewServer(h.srv)
	defer ts.Close()

	t.Run("summary", func(t *testing.T) {
		var got controlState
		getJSON(t, ts.URL+"/api/summary", &got)
		if got.Summary[string(store.Done)] != 1 || got.Summary[string(store.Encoding)] != 1 {
			t.Fatalf("summary counts wrong: %+v", got.Summary)
		}
		if got.Paused || got.Scanning {
			t.Fatalf("fresh server should be neither paused nor scanning: %+v", got)
		}
	})

	t.Run("queue excludes terminal", func(t *testing.T) {
		var got struct {
			Queue []jobDTO `json:"queue"`
		}
		getJSON(t, ts.URL+"/api/queue", &got)
		if len(got.Queue) != 1 || got.Queue[0].Path != "/lib/active.mkv" {
			t.Fatalf("queue should hold only the active row, got %+v", got.Queue)
		}
	})

	t.Run("history holds terminal", func(t *testing.T) {
		var got struct {
			History []jobDTO `json:"history"`
		}
		getJSON(t, ts.URL+"/api/history", &got)
		if len(got.History) != 1 || got.History[0].Path != "/lib/done.mkv" {
			t.Fatalf("history should hold the done row, got %+v", got.History)
		}
	})
}

func TestAuth_MutatingEndpoints(t *testing.T) {
	t.Run("no token configured disables control (403)", func(t *testing.T) {
		h := newHarness(t, "") // no token
		ts := httptest.NewServer(h.srv)
		defer ts.Close()
		code, _ := post(t, ts.URL+"/api/rescan", "")
		if code != http.StatusForbidden {
			t.Fatalf("rescan with no token configured: code %d, want 403", code)
		}
	})

	t.Run("token configured, missing/bad bearer (401)", func(t *testing.T) {
		h := newHarness(t, "secret")
		ts := httptest.NewServer(h.srv)
		defer ts.Close()
		if code, _ := post(t, ts.URL+"/api/rescan", ""); code != http.StatusUnauthorized {
			t.Fatalf("rescan with no bearer: code %d, want 401", code)
		}
		if code, _ := post(t, ts.URL+"/api/rescan", "wrong"); code != http.StatusUnauthorized {
			t.Fatalf("rescan with wrong bearer: code %d, want 401", code)
		}
	})

	t.Run("token configured, correct bearer (202)", func(t *testing.T) {
		h := newHarness(t, "secret")
		h.scanRelease <- struct{}{} // let the scan auto-complete
		ts := httptest.NewServer(h.srv)
		defer ts.Close()
		code, _ := post(t, ts.URL+"/api/rescan", "secret")
		if code != http.StatusAccepted {
			t.Fatalf("rescan with correct bearer: code %d, want 202", code)
		}
		select {
		case <-h.scanStarted:
		case <-time.After(2 * time.Second):
			t.Fatal("scan never started after an accepted rescan")
		}
	})
}

func TestRescan_RefusedWhileScanning(t *testing.T) {
	h := newHarness(t, "secret") // scanRelease left empty: the first scan blocks
	ts := httptest.NewServer(h.srv)
	defer ts.Close()

	if code, _ := post(t, ts.URL+"/api/rescan", "secret"); code != http.StatusAccepted {
		t.Fatalf("first rescan: want 202, got %d", code)
	}
	select {
	case <-h.scanStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first scan never started")
	}
	// A second rescan while the first is still running is refused with 409.
	if code, _ := post(t, ts.URL+"/api/rescan", "secret"); code != http.StatusConflict {
		t.Fatalf("overlapping rescan: want 409, got %d", code)
	}
	h.scanRelease <- struct{}{} // release the first scan
}

func TestPauseRefusesRescan(t *testing.T) {
	h := newHarness(t, "secret")
	ts := httptest.NewServer(h.srv)
	defer ts.Close()

	if code, _ := post(t, ts.URL+"/api/pause", "secret"); code != http.StatusOK {
		t.Fatalf("pause: want 200, got %d", code)
	}
	if !h.ctrl.Paused() {
		t.Fatal("controller not paused after POST /api/pause")
	}
	// Rescan while paused is refused (409) and starts no scan.
	if code, _ := post(t, ts.URL+"/api/rescan", "secret"); code != http.StatusConflict {
		t.Fatalf("rescan while paused: want 409, got %d", code)
	}
	select {
	case <-h.scanStarted:
		t.Fatal("a scan started while paused")
	case <-time.After(200 * time.Millisecond):
	}
	if code, _ := post(t, ts.URL+"/api/resume", "secret"); code != http.StatusOK {
		t.Fatalf("resume: want 200, got %d", code)
	}
	if h.ctrl.Paused() {
		t.Fatal("controller still paused after resume")
	}
}

func TestHub_BroadcastsSnapshotOnEvent(t *testing.T) {
	h := newHarness(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.hub.Run(ctx)

	sub, unsub := h.hub.Subscribe()
	defer unsub()

	h.hub.Observe(engine.Event{Path: "/lib/x.mkv", Status: store.Encoding})

	select {
	case data := <-sub:
		var snap snapshot
		if err := json.Unmarshal(data, &snap); err != nil {
			t.Fatalf("bad snapshot JSON: %v", err)
		}
		// The snapshot is rebuilt from the store, which has the seeded rows.
		if snap.Summary[string(store.Done)] != 1 {
			t.Fatalf("broadcast snapshot missing seeded rows: %+v", snap.Summary)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no snapshot broadcast after an event")
	}
}

func TestHub_BytesReclaimedAccumulates(t *testing.T) {
	h := newHarness(t, "")
	h.hub.Observe(engine.Event{Status: store.Done, BytesReclaimed: 100})
	h.hub.Observe(engine.Event{Status: store.Done, BytesReclaimed: 250})
	h.hub.Observe(engine.Event{Status: store.Skipped}) // 0 — must not change the total
	if got := h.hub.BytesReclaimed(); got != 350 {
		t.Fatalf("bytes reclaimed = %d, want 350", got)
	}
}

func TestSSEEndpoint_StreamsInitialSnapshot(t *testing.T) {
	h := newHarness(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.hub.Run(ctx)
	ts := httptest.NewServer(h.srv)
	defer ts.Close()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	// Read the initial frame: an "event: snapshot" line followed by a "data:" line.
	gotEvent, gotData := readSSEFrame(t, resp.Body, 2*time.Second)
	if gotEvent != "snapshot" {
		t.Fatalf("first SSE event = %q, want snapshot", gotEvent)
	}
	var snap snapshot
	if err := json.Unmarshal([]byte(gotData), &snap); err != nil {
		t.Fatalf("SSE data not valid snapshot JSON: %v (%q)", err, gotData)
	}
	if snap.Summary[string(store.Done)] != 1 {
		t.Fatalf("initial SSE snapshot missing seeded rows: %+v", snap.Summary)
	}
}

// --- tiny HTTP helpers -------------------------------------------------------

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

func post(t *testing.T, url, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// readSSEFrame reads one event/data pair from an SSE body within timeout.
func readSSEFrame(t *testing.T, body io.Reader, timeout time.Duration) (event, data string) {
	t.Helper()
	type frame struct{ event, data string }
	done := make(chan frame, 1)
	var once sync.Once
	go func() {
		sc := bufio.NewScanner(body)
		var ev, da string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event:"):
				ev = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				da = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			case line == "" && da != "":
				once.Do(func() { done <- frame{ev, da} })
				return
			}
		}
	}()
	select {
	case f := <-done:
		return f.event, f.data
	case <-time.After(timeout):
		t.Fatal("timed out reading an SSE frame")
		return "", ""
	}
}
