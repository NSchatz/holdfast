package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
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

// The dashboard's reclaimed figure must be DURABLE: a lifetime total that survives a
// restart, not a per-process counter that resets to 0. The Hub reads a baseline from
// the store's already-recorded done rows at construction, and the live figure is that
// baseline plus this process's reclaims. (TRANSCODE-14.)
func TestHub_ReclaimedLifetimeIsBaselinePlusSession(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	// Seed a prior-session done row worth 3,000,000 bytes reclaimed.
	mustClaim(t, st, "/lib/old.mkv", "9:9")
	src, out := int64(5_000_000), int64(2_000_000)
	if err := st.Finish(ctx, "/lib/old.mkv", "9:9", store.Done,
		&store.Outcome{SourceBytes: &src, OutputBytes: &out}); err != nil {
		t.Fatal(err)
	}

	ctrl := NewController(ctx, func(context.Context) error { return nil }, discard())
	hub := NewHub(st, ctrl, discard()) // reads the 3,000,000 baseline here

	if got := hub.ReclaimedLifetime(); got != 3_000_000 {
		t.Fatalf("lifetime at startup = %d, want 3,000,000 (the baseline)", got)
	}

	// This process reclaims another 500,000. Lifetime is baseline + session; session
	// alone is only this run's number.
	hub.Observe(doneEvent(500_000))
	if got := hub.ReclaimedLifetime(); got != 3_500_000 {
		t.Errorf("lifetime after a reclaim = %d, want 3,500,000", got)
	}
	if got := hub.BytesReclaimed(); got != 500_000 {
		t.Errorf("session = %d, want 500,000 (this run only)", got)
	}

	// It rides the snapshot the SSE stream and /api/summary both serve.
	data, err := hub.SnapshotJSON(ctx)
	if err != nil {
		t.Fatalf("SnapshotJSON: %v", err)
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if snap.BytesReclaimedLifetime != 3_500_000 {
		t.Errorf("snapshot lifetime = %d, want 3,500,000", snap.BytesReclaimedLifetime)
	}
	if snap.BytesReclaimedSession != 500_000 {
		t.Errorf("snapshot session = %d, want 500,000", snap.BytesReclaimedSession)
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

// --- the whole-ledger aggregates (DASH-7) ------------------------------------

// aggStore is the seam for the failure-isolation criteria: a real store whose aggregate
// report is post-processed, so a test can make exactly ONE figure unreadable and watch
// what happens to the rest of the snapshot. Everything else is the real SQLite store, so
// the summary/queue/history the test asserts on are genuinely read from a database.
type aggStore struct {
	*store.SQLite
	breakOne func(store.Aggregates) store.Aggregates
}

func (s aggStore) Aggregates(ctx context.Context) store.Aggregates {
	return s.breakOne(s.SQLite.Aggregates(ctx))
}

// seedOverCap fills st with more terminal rows than historyLimit and more non-terminal
// rows than queueLimit, and returns what the WHOLE table then holds. This is the shape
// of ledger every figure on the page has to survive: the views truncate, the figures
// must not.
func seedOverCap(t *testing.T, st *store.SQLite) (done, skipped, queued int) {
	t.Helper()
	ctx := context.Background()
	done, skipped, queued = historyLimit+11, historyLimit+7, queueLimit+3

	for i := 0; i < done; i++ {
		p := "/lib/over/done" + strconv.Itoa(i) + ".mkv"
		mustClaim(t, st, p, "d:d")
		src, out, ms := int64(4_000_000+i), int64(1_000_000+i), int64(30_000+i)
		mean, worst := 95.0+float64(i%10)/10.0, 70.0+float64(i%20)/10.0
		if err := st.Finish(ctx, p, "d:d", store.Done, &store.Outcome{
			Encoder: "cpu", VmafMean: &mean, VmafMin: &worst, VmafModel: "version=vmaf_v0.6.1",
			SourceBytes: &src, OutputBytes: &out, EncodeMs: &ms,
		}); err != nil {
			t.Fatalf("seed done: %v", err)
		}
	}
	for i := 0; i < skipped; i++ {
		p := "/lib/over/skip" + strconv.Itoa(i) + ".mkv"
		mustClaim(t, st, p, "s:s")
		if err := st.Finish(ctx, p, "s:s", store.Skipped, &store.Outcome{Reason: engine.SkipLowBitrate}); err != nil {
			t.Fatalf("seed skipped: %v", err)
		}
	}
	for i := 0; i < queued; i++ {
		mustClaim(t, st, "/lib/over/queued"+strconv.Itoa(i)+".mkv", "q:q")
	}
	return done, skipped, queued
}

func snapshotOf(t *testing.T, hub *Hub) snapshot {
	t.Helper()
	data, err := hub.SnapshotJSON(context.Background())
	if err != nil {
		t.Fatalf("SnapshotJSON: %v", err)
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	return snap
}

// --- live progress on the wire (S0030) ---------------------------------------
//
// The page these fields feed is the only place an operator can answer "is this moving?".
// So the bar is not "a number appears" — it is that an unmeasured figure is visibly
// unmeasured, a measured one is in range, and a finished job stops carrying either.

// progressEvent builds the live progress report the engine emits for a running encode.
// dur is the source duration the position is measured against; nil is UNKNOWN.
func progressEvent(path string, positionSec float64, dur *float64) engine.Event {
	return engine.Event{
		Path:     path,
		Status:   store.Encoding,
		Progress: &engine.Progress{PositionSec: positionSec, DurationSec: dur},
	}
}

// snapshotRaw is snapshotOf's counterpart for the assertions that must read the RAW
// bytes: the explicit-null convention only exists on the wire, and decoding into a
// struct would erase the very distinction those tests are pinning.
func snapshotRaw(t *testing.T, hub *Hub) string {
	t.Helper()
	data, err := hub.SnapshotJSON(context.Background())
	if err != nil {
		t.Fatalf("SnapshotJSON: %v", err)
	}
	return string(data)
}

func queueRowFor(t *testing.T, snap snapshot, path string) jobDTO {
	t.Helper()
	for _, j := range snap.Queue {
		if j.Path == path {
			return j
		}
	}
	t.Fatalf("%s is not in the queue: %+v", path, snap.Queue)
	return jobDTO{}
}

// TestSnapshot_ActiveJobWithNoProgressIsUnknownNotZero is AC3 and AC10 together. A job
// whose encoder has reported nothing yet is still shown as running — it just has no
// figure — and "no figure" goes out as an explicit null. A zero here would read as "0%
// encoded", a measurement nobody took, on the page an operator uses to decide whether
// the tool is stuck.
func TestSnapshot_ActiveJobWithNoProgressIsUnknownNotZero(t *testing.T) {
	h := newHarness(t, "")

	snap := snapshotOf(t, h.hub)
	row := queueRowFor(t, snap, "/lib/active.mkv")
	if row.Status != string(store.Encoding) {
		t.Errorf("status = %q, want encoding — an unmeasured job is still a RUNNING job", row.Status)
	}
	if row.ProgressSeconds != nil || row.ProgressDuration != nil || row.ProgressFraction != nil {
		t.Errorf("a job whose encoder reported nothing is carrying a figure: %+v", row)
	}

	raw := snapshotRaw(t, h.hub)
	for _, want := range []string{
		`"progress_seconds":null`, `"progress_duration_seconds":null`, `"progress_fraction":null`,
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("unreported progress must serialize as %s\nbody: %s", want, raw)
		}
	}
	for _, banned := range []string{`"progress_fraction":0`, `"progress_seconds":0`} {
		if strings.Contains(raw, banned) {
			t.Errorf("unreported progress went out as %s — a fabricated zero\nbody: %s", banned, raw)
		}
	}
	// The elapsed basis rides the same frame (AC1's "derived from the timestamp on the
	// wire": the page needs the server's own clock to read updated_at against).
	if snap.Now <= 0 {
		t.Errorf("snapshot carries no server clock (now=%d) — elapsed would have to be guessed from the client's", snap.Now)
	}
	if row.UpdatedAt <= 0 {
		t.Errorf("the active row carries no transition timestamp (updated_at=%d)", row.UpdatedAt)
	}
}

// TestSnapshot_ReportedProgressRidesTheQueue is the positive case: once the encoder has
// reported a position, the queue row carries it, the duration it is measured against,
// and the two divided.
func TestSnapshot_ReportedProgressRidesTheQueue(t *testing.T) {
	h := newHarness(t, "")
	dur := 400.0
	h.hub.Observe(progressEvent("/lib/active.mkv", 100, &dur))

	row := queueRowFor(t, snapshotOf(t, h.hub), "/lib/active.mkv")
	if row.ProgressSeconds == nil || *row.ProgressSeconds != 100 {
		t.Fatalf("progress_seconds = %v, want 100", row.ProgressSeconds)
	}
	if row.ProgressDuration == nil || *row.ProgressDuration != 400 {
		t.Fatalf("progress_duration_seconds = %v, want 400", row.ProgressDuration)
	}
	if row.ProgressFraction == nil || *row.ProgressFraction != 0.25 {
		t.Fatalf("progress_fraction = %v, want 0.25", row.ProgressFraction)
	}

	// GET /api/queue serves the same picture as the stream — a client that polls must
	// not see a different running job than one that watches.
	ts := httptest.NewServer(h.srv)
	defer ts.Close()
	var got struct {
		Queue []jobDTO `json:"queue"`
		Now   int64    `json:"now"`
	}
	getJSON(t, ts.URL+"/api/queue", &got)
	if len(got.Queue) != 1 || got.Queue[0].ProgressFraction == nil || *got.Queue[0].ProgressFraction != 0.25 {
		t.Errorf("/api/queue does not carry the live figure the stream does: %+v", got.Queue)
	}
	if got.Now <= 0 {
		t.Errorf("/api/queue carries no server clock (now=%d)", got.Now)
	}
}

// TestSnapshot_UnknownSourceDurationPublishesNoFraction is AC5. Some containers report
// no duration at all (MPEG-TS is the standing example, and the probe layer refuses to
// coerce that to 0). With no length to measure against there is no fraction to compute,
// and computing one anyway is precisely the invented figure this surface must not carry.
func TestSnapshot_UnknownSourceDurationPublishesNoFraction(t *testing.T) {
	h := newHarness(t, "")

	for _, tc := range []struct {
		name string
		dur  *float64
	}{
		{"unknown duration", nil},
		{"zero duration", func() *float64 { z := 0.0; return &z }()},
		{"negative duration", func() *float64 { n := -5.0; return &n }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.hub.Observe(progressEvent("/lib/active.mkv", 90, tc.dur))
			row := queueRowFor(t, snapshotOf(t, h.hub), "/lib/active.mkv")
			if row.ProgressFraction != nil {
				t.Errorf("a %s published a fraction of %v — there is nothing to measure against", tc.name, *row.ProgressFraction)
			}
			if row.ProgressDuration != nil {
				t.Errorf("a %s published a duration of %v", tc.name, *row.ProgressDuration)
			}
			raw := snapshotRaw(t, h.hub)
			if !strings.Contains(raw, `"progress_fraction":null`) {
				t.Errorf("an unmeasurable fraction must serialize as null\nbody: %s", raw)
			}
		})
	}
}

// TestSnapshot_PositionOutOfRangeIsClamped is AC7. An encoder legitimately reports a
// position at or slightly past the source duration on its final report (a container's
// duration is itself an approximation), and ffmpeg emits negative timestamps during
// pre-roll on some inputs. Neither may reach the page as "103% encoded" or a negative.
func TestSnapshot_PositionOutOfRangeIsClamped(t *testing.T) {
	h := newHarness(t, "")
	dur := 120.0

	h.hub.Observe(progressEvent("/lib/active.mkv", 126, &dur)) // past the end
	row := queueRowFor(t, snapshotOf(t, h.hub), "/lib/active.mkv")
	if row.ProgressFraction == nil || *row.ProgressFraction != 1 {
		t.Errorf("a position past the source duration gave fraction %v, want exactly 1 (fully encoded)", row.ProgressFraction)
	}
	if row.ProgressSeconds == nil || *row.ProgressSeconds != 120 {
		t.Errorf("progress_seconds = %v, want it clamped to the 120s duration beside it", row.ProgressSeconds)
	}

	h.hub.Observe(progressEvent("/lib/active.mkv", -3, &dur)) // pre-roll
	row = queueRowFor(t, snapshotOf(t, h.hub), "/lib/active.mkv")
	if row.ProgressSeconds == nil || *row.ProgressSeconds != 0 {
		t.Errorf("a negative position gave progress_seconds %v, want 0", row.ProgressSeconds)
	}
	if row.ProgressFraction == nil || *row.ProgressFraction != 0 {
		t.Errorf("a negative position gave fraction %v, want 0", row.ProgressFraction)
	}
}

// TestSnapshot_TerminalJobStopsReportingProgress is AC8. The moment a job finishes it
// leaves the queue, its live entry is dropped, and no history row carries a progress
// figure — so nothing on a finished row can keep moving.
func TestSnapshot_TerminalJobStopsReportingProgress(t *testing.T) {
	h := newHarness(t, "")
	dur := 200.0
	h.hub.Observe(progressEvent("/lib/active.mkv", 50, &dur))
	if h.hub.liveProgressCount() != 1 {
		t.Fatalf("live progress entries = %d, want 1 before the job finishes", h.hub.liveProgressCount())
	}

	// The engine finishes the job: the store row goes terminal and a terminal event is
	// emitted, exactly as ProcessFile does.
	if err := h.st.Finish(context.Background(), "/lib/active.mkv", "1:1", store.Done, nil); err != nil {
		t.Fatal(err)
	}
	h.hub.Observe(engine.Event{Path: "/lib/active.mkv", Status: store.Done})

	if got := h.hub.liveProgressCount(); got != 0 {
		t.Errorf("live progress entries = %d after the job went terminal, want 0", got)
	}
	snap := snapshotOf(t, h.hub)
	for _, j := range snap.Queue {
		if j.Path == "/lib/active.mkv" {
			t.Errorf("a finished job is still in the queue: %+v", j)
		}
	}
	for _, j := range snap.History {
		if j.ProgressSeconds != nil || j.ProgressDuration != nil || j.ProgressFraction != nil {
			t.Errorf("a terminal history row carries a live progress figure: %+v", j)
		}
	}
}

// TestSnapshot_ProgressReachesAConnectedClientWithNoTransition is AC9: an encode that
// advances with no state change still pushes a fresh frame down the stream a client is
// already holding open. Without this the figure would only ever move when the job
// changed state, which is the illegibility this whole item exists to fix.
func TestSnapshot_ProgressReachesAConnectedClientWithNoTransition(t *testing.T) {
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

	// Frame 1: the initial snapshot, before any progress has been reported.
	_, first := readSSEFrame(t, resp.Body, 2*time.Second)
	var before snapshot
	if err := json.Unmarshal([]byte(first), &before); err != nil {
		t.Fatalf("initial frame: %v", err)
	}
	if queueRowFor(t, before, "/lib/active.mkv").ProgressFraction != nil {
		t.Fatal("the initial frame already carries a figure — the test would prove nothing")
	}

	// The encode advances. No transition: the job is still `encoding` in the store.
	dur := 300.0
	h.hub.Observe(progressEvent("/lib/active.mkv", 150, &dur))

	// Frame 2 arrives on the SAME connection, with no request from the client.
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, data := readSSEFrame(t, resp.Body, 2*time.Second)
		var snap snapshot
		if err := json.Unmarshal([]byte(data), &snap); err != nil {
			t.Fatalf("later frame: %v", err)
		}
		row := queueRowFor(t, snap, "/lib/active.mkv")
		if row.Status != string(store.Encoding) {
			t.Fatalf("the job changed state (%q) — this must prove delivery WITHOUT a transition", row.Status)
		}
		if row.ProgressFraction != nil {
			if *row.ProgressFraction != 0.5 {
				t.Errorf("delivered fraction = %v, want 0.5", *row.ProgressFraction)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the advanced figure never reached the already-connected client")
		}
	}
}

// TestSnapshot_NoActiveJobsPublishNoLiveProgress is AC13: with nothing running there are
// no live entries at all — including any left behind by a job that finished under a
// different path than it started under (a container-changing swap renames the file, so
// the Done event names the POST-swap path).
func TestSnapshot_NoActiveJobsPublishNoLiveProgress(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()

	// A stale entry under the PRE-swap name, of the kind a rename would strand.
	dur := 60.0
	h.hub.Observe(progressEvent("/lib/active.mkv", 30, &dur))
	h.hub.Observe(progressEvent("/lib/gone.mp4", 10, &dur))

	// Everything finishes; the queue empties.
	if err := h.st.Finish(ctx, "/lib/active.mkv", "1:1", store.Done, nil); err != nil {
		t.Fatal(err)
	}

	snap := snapshotOf(t, h.hub)
	if len(snap.Queue) != 0 {
		t.Fatalf("queue is not empty: %+v", snap.Queue)
	}
	if got := h.hub.liveProgressCount(); got != 0 {
		t.Errorf("live progress entries = %d with no active job, want 0 — a stranded entry would grow without bound", got)
	}
	raw := snapshotRaw(t, h.hub)
	if strings.Contains(raw, `"progress_fraction":0.`) || strings.Contains(raw, `"progress_fraction":1`) {
		t.Errorf("a snapshot with no active jobs published a progress figure\nbody: %s", raw)
	}
}

// TestSnapshot_LiveProgressIsDroppedOnAnyExitFromEncoding is the general rule AC8's
// terminal case is one instance of, and the rule the spec states from the other side:
// "A progress percentage for the verify/VMAF phase" is out of scope and "the verifying
// state is covered by elapsed alone".
//
// A progress figure is a measurement taken BY A PROCESS, so it is live exactly while that
// process is running. The moment a job leaves `encoding` — terminal or not — the encoder
// that produced the figure has exited and the figure describes nothing that is still
// happening. Publishing it anyway would freeze a percentage next to a state it does not
// describe for the whole verify/VMAF phase, which on a feature-length source is the
// longest stretch an operator watches.
//
// Both routes out of `encoding` are covered, because the hub may only ever see one of
// them: the transition EVENT, and the STORE alone (the events channel drops when it is
// full, and RecoverStale returns a job to pending with no event at all).
func TestSnapshot_LiveProgressIsDroppedOnAnyExitFromEncoding(t *testing.T) {
	ctx := context.Background()
	dur := 400.0

	for _, tc := range []struct {
		name      string
		status    store.Status
		announced bool
	}{
		{"verifying, transition observed", store.Verifying, true},
		{"verifying, store only (the event was coalesced away)", store.Verifying, false},
		{"back to pending, store only (RecoverStale after a crash)", store.Pending, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "")
			h.hub.Observe(progressEvent("/lib/active.mkv", 300, &dur))
			if h.hub.liveProgressCount() != 1 {
				t.Fatal("there was no live entry to drop — the case would prove nothing")
			}

			if err := h.st.Advance(ctx, "/lib/active.mkv", "1:1", tc.status); err != nil {
				t.Fatalf("Advance to %s: %v", tc.status, err)
			}
			if tc.announced {
				h.hub.Observe(engine.Event{Path: "/lib/active.mkv", Status: tc.status, Worker: "w0"})
			}

			row := queueRowFor(t, snapshotOf(t, h.hub), "/lib/active.mkv")
			if row.Status != string(tc.status) {
				t.Fatalf("fixture is in %q, not %q", row.Status, tc.status)
			}
			if row.ProgressSeconds != nil || row.ProgressDuration != nil || row.ProgressFraction != nil {
				t.Errorf("a %s row still carries the finished encode's figure: seconds=%v duration=%v fraction=%v",
					tc.status, row.ProgressSeconds, row.ProgressDuration, row.ProgressFraction)
			}
			if got := h.hub.liveProgressCount(); got != 0 {
				t.Errorf("the hub holds %d live entries for a job whose encoder has exited, want 0", got)
			}
			// And it is absent the way this repo says absent: an explicit null, never a 0.
			raw := snapshotRaw(t, h.hub)
			for _, want := range []string{
				`"progress_seconds":null`, `"progress_duration_seconds":null`, `"progress_fraction":null`,
			} {
				if !strings.Contains(raw, want) {
					t.Errorf("a %s row does not carry %s\nbody: %s", tc.status, want, raw)
				}
			}
		})
	}
}

// The criterion this phase exists for. Against a ledger holding MORE terminal rows than
// the history view ships and MORE non-terminal rows than the queue view ships, every
// published figure equals the whole-table value - while the rows on the wire are still
// capped exactly as before. Both halves matter: an aggregate that matched by shipping
// more rows would have fixed the number by breaking the payload.
func TestSnapshot_AggregatesAreWholeLedgerWhileRowsStayCapped(t *testing.T) {
	h := newHarness(t, "")
	done, skipped, _ := seedOverCap(t, h.st)
	// newStore seeds one done row and one active row on top of the over-cap seed.
	done++

	snap := snapshotOf(t, h.hub)

	if len(snap.Queue) != queueLimit {
		t.Errorf("queue shipped %d rows, want the unchanged cap of %d", len(snap.Queue), queueLimit)
	}
	if len(snap.History) != historyLimit {
		t.Errorf("history shipped %d rows, want the unchanged cap of %d", len(snap.History), historyLimit)
	}

	counts := map[string]int64{}
	for _, b := range snap.Aggregates.Outcomes.Buckets {
		counts[b.Key] = b.Count
	}
	if counts[string(store.Done)] != int64(done) || counts[string(store.Skipped)] != int64(skipped) {
		t.Errorf("outcome counts = %v, want done=%d skipped=%d - the whole table, not the %d rows shipped",
			counts, done, skipped, len(snap.History))
	}
	if got := snap.Aggregates.SkipsByGuard.Buckets; len(got) != 1 || got[0].Key != engine.SkipLowBitrate || got[0].Count != int64(skipped) {
		t.Errorf("skip breakdown = %+v, want %d rows under %q", got, skipped, engine.SkipLowBitrate)
	}
	if snap.Aggregates.SizeRatio.Counted != int64(done-1) {
		t.Errorf("size ratio counted %d rows, want the %d done rows that recorded sizes",
			snap.Aggregates.SizeRatio.Counted, done-1)
	}
	// The one done row newStore seeds carries no outcome at all - excluded and counted,
	// never folded in as a zero.
	if snap.Aggregates.SizeRatio.Excluded != 1 {
		t.Errorf("size ratio excluded = %d, want 1 (the outcome-less done row)", snap.Aggregates.SizeRatio.Excluded)
	}
	if snap.Aggregates.EncodeMs.Counted != int64(done-1) || snap.Aggregates.VmafMean.Counted != int64(done-1) {
		t.Errorf("encode/vmaf counted %d/%d, want %d each",
			snap.Aggregates.EncodeMs.Counted, snap.Aggregates.VmafMean.Counted, done-1)
	}
}

// Every published figure states the set it covers, and a figure that is NOT over its
// whole set has to say what bounds it. Nothing published today is bounded - which is the
// assertion, not an omission: an unstated window is exactly the failure mode the phase
// names, so `window` is on the wire for every figure and empty for every figure, and the
// projection is proved to carry a bound when one exists.
func TestSnapshot_EveryAggregateStatesTheSetItCovers(t *testing.T) {
	h := newHarness(t, "")
	snap := snapshotOf(t, h.hub)
	a := snap.Aggregates

	for name, cov := range map[string][2]string{
		"outcomes":       {a.Outcomes.Covers, a.Outcomes.Window},
		"skips_by_guard": {a.SkipsByGuard.Covers, a.SkipsByGuard.Window},
		"size_ratio":     {a.SizeRatio.Covers, a.SizeRatio.Window},
		"encode_ms":      {a.EncodeMs.Covers, a.EncodeMs.Window},
		"vmaf_mean":      {a.VmafMean.Covers, a.VmafMean.Window},
		"vmaf_min":       {a.VmafMin.Covers, a.VmafMin.Window},
	} {
		if cov[0] == "" {
			t.Errorf("%s ships no `covers` - a figure whose set is unstated will be read as covering the whole library", name)
		}
		if !strings.Contains(cov[0], "ledger") {
			t.Errorf("%s covers %q, which does not name the ledger it is over", name, cov[0])
		}
		if cov[1] != "" {
			t.Errorf("%s claims window %q but is computed over its whole matching set", name, cov[1])
		}
	}

	// A bounded figure states its bound ALONGSIDE the figure, on the wire, in the same
	// object. This drives the projection with a windowed aggregate to prove the
	// statement travels rather than being dropped on the way out.
	windowed := (&Hub{log: discard()}).spread("windowed", store.Spread{
		Coverage: store.Coverage{Set: "done rows in the ledger", Window: "the most recent 200 rows"},
		Counted:  5,
	})
	if windowed.Window != "the most recent 200 rows" || windowed.Covers != "done rows in the ledger" {
		t.Errorf("a bounded figure lost its window on the wire: %+v", windowed)
	}
	body, err := json.Marshal(windowed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"window":"the most recent 200 rows"`) {
		t.Errorf("the window is not stated alongside the figure: %s", body)
	}
}

// A figure nobody could compute is "no data", on the wire, as explicit nulls - never 0,
// never an average of 0, and never an empty breakdown that reads as a set of zero
// counts. Asserted on the raw bytes, because the distinction only exists there:
// decoding into a struct turns null and 0 into the same Go value, and a client that
// read 0 would publish a fabricated library-wide statistic.
func TestSnapshot_AnAggregateWithNoDataIsNullNotZero(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	// A done row with NO recorded outcome: the shape of every row written before the
	// outcome columns existed.
	mustClaim(t, st, "/lib/unmeasured.mkv", "1:1")
	if err := st.Finish(ctx, "/lib/unmeasured.mkv", "1:1", store.Done, nil); err != nil {
		t.Fatal(err)
	}

	ctrl := NewController(ctx, func(context.Context) error { return nil }, discard())
	hub := NewHub(st, ctrl, discard())
	data, err := hub.SnapshotJSON(ctx)
	if err != nil {
		t.Fatalf("SnapshotJSON: %v", err)
	}
	body := string(data)

	for _, want := range []string{`"min":null`, `"mean":null`, `"max":null`, `"excluded":1`} {
		if !strings.Contains(body, want) {
			t.Errorf("an unmeasured aggregate must ship %s\nbody: %s", want, body)
		}
	}
	if strings.Contains(body, `"mean":0`) || strings.Contains(body, `"min":0`) {
		t.Errorf("an unmeasured aggregate went out as a zero - that is a claim about a library nobody measured\nbody: %s", body)
	}
}

// One unreadable figure must cost exactly itself. buildSnapshot returns an error on any
// store read failure and broadcast then SKIPS the frame, so an aggregate read on that
// path would blank the live page for every subscriber the moment one query failed. This
// breaks a single aggregate and asserts the summary, the queue and the history still
// ship, the other figures still compute, and the broadcast still fires.
func TestSnapshot_OneUnreadableAggregateStillShipsEverythingElse(t *testing.T) {
	real := newStore(t)
	broken := aggStore{SQLite: real, breakOne: func(a store.Aggregates) store.Aggregates {
		a.VmafMean.Err = errors.New("simulated aggregate read failure")
		a.VmafMean.Counted, a.VmafMean.Mean = 0, nil
		return a
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl := NewController(ctx, func(context.Context) error { return nil }, discard())
	hub := NewHub(broken, ctrl, discard())
	srv := New(ctx, config.Config{}, broken, ctrl, hub, nil, nil, discard())

	snap := snapshotOf(t, hub)
	if snap.Summary[string(store.Done)] != 1 || len(snap.Queue) != 1 || len(snap.History) != 1 {
		t.Fatalf("a failing aggregate suppressed the snapshot's rows: summary=%v queue=%d history=%d",
			snap.Summary, len(snap.Queue), len(snap.History))
	}
	if snap.Aggregates.VmafMean.Available {
		t.Error("the broken figure is marked available")
	}
	if snap.Aggregates.VmafMean.Unavailable == "" {
		t.Error("the broken figure ships no unavailable statement, so the page cannot say why it is blank")
	}
	if snap.Aggregates.VmafMean.Mean != nil {
		t.Errorf("an unreadable figure published a value: %v", *snap.Aggregates.VmafMean.Mean)
	}
	for name, ok := range map[string]bool{
		"outcomes": snap.Aggregates.Outcomes.Available, "skips_by_guard": snap.Aggregates.SkipsByGuard.Available,
		"size_ratio": snap.Aggregates.SizeRatio.Available, "encode_ms": snap.Aggregates.EncodeMs.Available,
		"vmaf_min": snap.Aggregates.VmafMin.Available,
	} {
		if !ok {
			t.Errorf("%s went unavailable because a DIFFERENT figure failed", name)
		}
	}

	// The broadcast still fires - a subscriber gets the frame, it is not skipped.
	go hub.Run(ctx)
	sub, unsub := hub.Subscribe()
	defer unsub()
	hub.Observe(engine.Event{Path: "/lib/x.mkv", Status: store.Encoding})
	select {
	case data := <-sub:
		var got snapshot
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("broadcast frame is not a snapshot: %v", err)
		}
		if got.Summary[string(store.Done)] != 1 {
			t.Errorf("the broadcast frame lost its summary: %+v", got.Summary)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no broadcast after an event - one failing aggregate skipped the frame for every subscriber")
	}

	// And the read endpoints answer too, rather than 500ing on the failed figure.
	ts := httptest.NewServer(srv)
	defer ts.Close()
	var state controlState
	getJSON(t, ts.URL+"/api/summary", &state)
	if state.Summary[string(store.Done)] != 1 {
		t.Errorf("/api/summary lost its counts to a failing aggregate: %+v", state)
	}
	if state.Aggregates.VmafMean.Available || !state.Aggregates.SizeRatio.Available {
		t.Errorf("/api/summary reported the wrong figures as unavailable: %+v", state.Aggregates)
	}
	var q struct {
		Queue []jobDTO `json:"queue"`
	}
	getJSON(t, ts.URL+"/api/queue", &q)
	if len(q.Queue) != 1 {
		t.Errorf("/api/queue lost its rows to a failing aggregate: %+v", q.Queue)
	}
}

// The new fields widen what the unauthenticated read surface says about the LIBRARY and
// must widen nothing about a FILE. Two halves: the endpoints carrying them still need no
// authorization (the localhost bind stays the control, exactly as before), and the
// aggregates object contains no per-file datum - no path, no worker, no fingerprint, and
// no key outside the published aggregate vocabulary.
func TestSnapshot_AggregatesAddNoAuthorizationAndNoPerFileDatum(t *testing.T) {
	h := newHarness(t, "a-token-is-configured") // control IS gated; reads must not be
	ctx := context.Background()
	mustClaim(t, h.st, "/lib/a-very-distinctive-path.mkv", "9:9")
	src, out := int64(9_000_000), int64(3_000_000)
	if err := h.st.Finish(ctx, "/lib/a-very-distinctive-path.mkv", "9:9", store.Done,
		&store.Outcome{Encoder: "cpu", SourceBytes: &src, OutputBytes: &out}); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(h.srv)
	defer ts.Close()

	// No Authorization header anywhere below.
	for _, path := range []string{"/api/summary", "/api/events"} {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		code := resp.StatusCode
		_ = resp.Body.Close()
		if code != http.StatusOK {
			t.Errorf("GET %s with no token = %d, want 200 - the aggregates must not add an authorization requirement", path, code)
		}
	}

	// Peel the aggregates object out of the summary response and inspect it on its own.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(getRaw(t, ts.URL+"/api/summary")), &raw); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	aggRaw, ok := raw["aggregates"]
	if !ok {
		t.Fatal("/api/summary carries no aggregates")
	}
	agg := string(aggRaw)
	for _, forbidden := range []string{"a-very-distinctive-path", "/lib/", ".mkv", `"path"`, `"worker"`, `"fingerprint"`} {
		if strings.Contains(agg, forbidden) {
			t.Errorf("the aggregates expose %q - a per-file datum has leaked into a library-wide figure\n%s", forbidden, agg)
		}
	}

	// Stronger than a substring hunt: every key inside every aggregate is from the
	// published vocabulary, so a future field cannot arrive here unnoticed.
	var members map[string]map[string]json.RawMessage
	if err := json.Unmarshal(aggRaw, &members); err != nil {
		t.Fatalf("decode aggregates: %v", err)
	}
	allowed := map[string]bool{
		"available": true, "unavailable": true, "covers": true, "window": true,
		"counted": true, "excluded": true, "min": true, "mean": true, "max": true, "buckets": true,
	}
	for name, fields := range members {
		for k := range fields {
			if !allowed[k] {
				t.Errorf("aggregate %q ships unpublished key %q", name, k)
			}
		}
	}
	// A breakdown's bucket keys are the closed vocabularies the API already ships per
	// row (a status, or an engine guard token) - never anything file-specific.
	var breakdown struct {
		Buckets []bucketDTO `json:"buckets"`
	}
	if err := json.Unmarshal(members["skips_by_guard"]["buckets"], &breakdown.Buckets); err != nil && len(members["skips_by_guard"]["buckets"]) > 4 {
		t.Fatalf("decode buckets: %v", err)
	}
	for _, b := range breakdown.Buckets {
		if strings.ContainsAny(b.Key, "/.") {
			t.Errorf("skip bucket key %q looks like a path, not a guard token", b.Key)
		}
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
