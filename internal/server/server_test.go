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
	if err := st.Finish(ctx, "/lib/done.mkv", "2:2", store.Done, nil); err != nil {
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
	// st is the harness's store, exposed so a test can seed extra rows (the outcome
	// tests need failed/skipped rows that newStore does not create).
	st *store.SQLite
	// scanStarted receives once per scan invocation; scanRelease gates each scan's
	// completion (buffered so a test can pre-fill it for auto-completing scans).
	scanStarted chan struct{}
	scanRelease chan struct{}
}

func newHarness(t *testing.T, token string) *harness {
	t.Helper()
	st := newStore(t)
	h := &harness{
		st:          st,
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

// doneEvent builds the Done event the engine emits for a swap that reclaimed n bytes.
// Since TRANSCODE-13 the reclaimed figure is DERIVED from the two sizes the Outcome
// records rather than carried as its own field, so a test states both.
func doneEvent(reclaimed int64) engine.Event {
	src, out := reclaimed+1000, int64(1000)
	return engine.Event{
		Status:  store.Done,
		Outcome: &store.Outcome{SourceBytes: &src, OutputBytes: &out},
	}
}

func TestHub_BytesReclaimedAccumulates(t *testing.T) {
	h := newHarness(t, "")
	h.hub.Observe(doneEvent(100))
	h.hub.Observe(doneEvent(250))
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

// --- the persisted proof on the wire (TRANSCODE-13) --------------------------

// The README documents `GET /api/history` as returning terminal jobs
// "(done/skipped/failed, with reason)". Until TRANSCODE-13 there was no reason field at
// all — the doc was an overclaim. This asserts the claim is now TRUE for both kinds of
// row that have one: a FAILED job (the error) and a SKIPPED job (which guard fired).
func TestHistoryEndpoint_ReturnsAReasonForFailedAndSkipped(t *testing.T) {
	h := newHarness(t, "")
	st := h.st
	ctx := context.Background()

	mustClaim(t, st, "/lib/broke.mkv", "3:3")
	if err := st.Finish(ctx, "/lib/broke.mkv", "3:3", store.Failed,
		&store.Outcome{Reason: "decode-integrity check failed (output does not fully decode)", Encoder: "cpu"}); err != nil {
		t.Fatal(err)
	}
	mustClaim(t, st, "/lib/thin.mkv", "4:4")
	if err := st.Finish(ctx, "/lib/thin.mkv", "4:4", store.Skipped,
		&store.Outcome{Reason: engine.SkipLowBitrate}); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(h.srv)
	defer ts.Close()

	var got struct {
		History []jobDTO `json:"history"`
	}
	getJSON(t, ts.URL+"/api/history", &got)

	byPath := make(map[string]jobDTO, len(got.History))
	for _, j := range got.History {
		byPath[j.Path] = j
	}

	failed, ok := byPath["/lib/broke.mkv"]
	if !ok {
		t.Fatalf("failed job absent from history: %+v", got.History)
	}
	if !strings.Contains(failed.Reason, "decode-integrity") {
		t.Errorf("failed job reason = %q, want the gate error that rejected it", failed.Reason)
	}
	if failed.Encoder != "cpu" {
		t.Errorf("failed job encoder = %q, want %q", failed.Encoder, "cpu")
	}

	skipped, ok := byPath["/lib/thin.mkv"]
	if !ok {
		t.Fatalf("skipped job absent from history: %+v", got.History)
	}
	if skipped.Reason != engine.SkipLowBitrate {
		t.Errorf("skipped job reason = %q, want the guard token %q — an operator must not have to read the logs to learn WHICH guard fired",
			skipped.Reason, engine.SkipLowBitrate)
	}
}

// A done row carries the fidelity proof, and an UNRECORDED field goes out as an explicit
// JSON `null` — never as 0. Asserted on the raw bytes, because that distinction only
// exists on the wire: decoding into a struct would turn both into the same Go value, and
// a client that reads 0 for "vmaf_min" would render a fabricated fidelity score for a
// swap nobody measured. That is the exact overclaim this whole track exists to prevent.
func TestHistoryEndpoint_UnrecordedOutcomeIsNullNotZero(t *testing.T) {
	h := newHarness(t, "")
	st := h.st
	ctx := context.Background()

	mean, min := 97.25, 88.5
	src, out, ms := int64(5_000_000), int64(2_000_000), int64(12_345)
	mustClaim(t, st, "/lib/proved.mkv", "5:5")
	if err := st.Finish(ctx, "/lib/proved.mkv", "5:5", store.Done, &store.Outcome{
		Encoder: "cpu", VmafMean: &mean, VmafMin: &min, VmafModel: "version=vmaf_v0.6.1",
		SourceBytes: &src, OutputBytes: &out, EncodeMs: &ms,
	}); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(h.srv)
	defer ts.Close()

	body := getRaw(t, ts.URL+"/api/history")

	// The proved row carries the whole thing — including the model, without which the
	// score is not an interpretable number.
	for _, want := range []string{
		`"vmaf_mean":97.25`, `"vmaf_min":88.5`, `"vmaf_model":"version=vmaf_v0.6.1"`,
		`"source_bytes":5000000`, `"output_bytes":2000000`, `"encode_ms":12345`, `"encoder":"cpu"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("history body missing %s\nbody: %s", want, body)
		}
	}
	// /lib/done.mkv was seeded by newStore with a nil outcome — the shape of every row
	// written before this phase. It must serialize as null, not 0.
	for _, want := range []string{`"vmaf_mean":null`, `"vmaf_min":null`, `"source_bytes":null`, `"encode_ms":null`} {
		if !strings.Contains(body, want) {
			t.Errorf("an unrecorded outcome must serialize as %s (a 0 would be a fabricated measurement)\nbody: %s", want, body)
		}
	}
	if strings.Contains(body, `"vmaf_mean":0`) || strings.Contains(body, `"vmaf_min":0`) {
		t.Errorf("an unrecorded VMAF must NEVER go out as 0\nbody: %s", body)
	}
}

// An in-flight retry must not advertise the PREVIOUS attempt's fidelity score on
// /api/queue. The queue and history views share one projection, so a stale outcome left
// on a re-claimed row would be served next to a file that is still encoding — a score
// belonging to an encode that was rejected and deleted.
func TestQueueEndpoint_InFlightRetryCarriesNoStaleProof(t *testing.T) {
	h := newHarness(t, "")
	st := h.st
	ctx := context.Background()

	mean, min := 87.5, 41.0
	mustClaim(t, st, "/lib/retry.mkv", "7:7")
	if err := st.Finish(ctx, "/lib/retry.mkv", "7:7", store.Failed, &store.Outcome{
		Reason: "VMAF worst-frame below floor", Encoder: "cpu", VmafMean: &mean, VmafMin: &min,
	}); err != nil {
		t.Fatal(err)
	}
	// Retry: claim it again and put it in flight.
	mustClaim(t, st, "/lib/retry.mkv", "7:7")
	if err := st.Advance(ctx, "/lib/retry.mkv", "7:7", store.Encoding); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(h.srv)
	defer ts.Close()

	var got struct {
		Queue []jobDTO `json:"queue"`
	}
	getJSON(t, ts.URL+"/api/queue", &got)

	var found bool
	for _, j := range got.Queue {
		if j.Path != "/lib/retry.mkv" {
			continue
		}
		found = true
		if j.VmafMean != nil || j.VmafMin != nil {
			t.Errorf("an encoding job is advertising the rejected attempt's VMAF (mean=%v min=%v)", j.VmafMean, j.VmafMin)
		}
		if j.Reason != "" {
			t.Errorf("an encoding job still carries the previous failure's reason: %q", j.Reason)
		}
	}
	if !found {
		t.Fatalf("the retried job is not in the queue: %+v", got.Queue)
	}
}

func getRaw(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return string(b)
}
