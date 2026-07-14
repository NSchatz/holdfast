package metrics

import (
	"context"
	"io"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/store"
)

func openStore(t *testing.T) *store.SQLite {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("/metrics status %d", rec.Code)
	}
	b, _ := io.ReadAll(rec.Body)
	return string(b)
}

// doneEvent builds the Done event the engine emits (TRANSCODE-13): the reclaimed bytes
// and the encode duration are now read off the event's Outcome — the same value handed
// to the store — rather than from fields duplicated onto the event.
func doneEvent(reclaimed int64, encode time.Duration, vmaf float64) engine.Event {
	src, out := reclaimed+1000, int64(1000)
	ms := encode.Milliseconds()
	return engine.Event{
		Status: store.Done,
		Outcome: &store.Outcome{
			SourceBytes: &src, OutputBytes: &out,
			EncodeMs: &ms, VmafMean: &vmaf, VmafModel: "version=vmaf_v0.6.1",
		},
	}
}

func TestMetrics_CountersAndHistogramsFromEvents(t *testing.T) {
	m := New(openStore(t))

	m.Observe(doneEvent(1000, 2*time.Second, 96))
	m.Observe(doneEvent(500, 1*time.Second, 98))
	m.Observe(engine.Event{Status: store.Skipped})
	m.Observe(engine.Event{Status: store.Failed})
	m.Observe(engine.Event{Status: store.Encoding}) // non-terminal — must NOT be counted

	if got := testutil.ToFloat64(m.filesTotal.WithLabelValues("done")); got != 2 {
		t.Errorf("files_total{done} = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.filesTotal.WithLabelValues("skipped")); got != 1 {
		t.Errorf("files_total{skipped} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.filesTotal.WithLabelValues("failed")); got != 1 {
		t.Errorf("files_total{failed} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.bytesReclaimed); got != 1500 {
		t.Errorf("bytes_reclaimed_total = %v, want 1500", got)
	}

	// Histograms: assert the sample counts via the scrape body.
	body := scrape(t, m)
	for _, want := range []string{
		`holdfast_files_total{outcome="done"} 2`,
		`holdfast_files_total{outcome="failed"} 1`,
		`holdfast_bytes_reclaimed_total 1500`,
		`holdfast_encode_duration_seconds_count 2`,
		`holdfast_vmaf_score_count 2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics body missing %q", want)
		}
	}
}

func TestMetrics_QueueDepthReadsStore(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	// Seed: two pending-claimed (encoding) + one done.
	for _, p := range []string{"/lib/a.mkv", "/lib/b.mkv"} {
		if ok, err := st.Claim(ctx, p, "1:1", "w0", 3); err != nil || !ok {
			t.Fatalf("claim %s: %v", p, err)
		}
		if err := st.Advance(ctx, p, "1:1", store.Encoding); err != nil {
			t.Fatal(err)
		}
	}
	if ok, _ := st.Claim(ctx, "/lib/c.mkv", "2:2", "w0", 3); ok {
		_ = st.Finish(ctx, "/lib/c.mkv", "2:2", store.Done, nil)
	}

	m := New(st)
	body := scrape(t, m)
	if !strings.Contains(body, `holdfast_queue_depth{state="encoding"} 2`) {
		t.Errorf("queue_depth encoding gauge wrong; body:\n%s", body)
	}
	if !strings.Contains(body, `holdfast_queue_depth{state="done"} 1`) {
		t.Errorf("queue_depth done gauge wrong; body:\n%s", body)
	}
}

func TestMetrics_PrecreatedSeriesReadZero(t *testing.T) {
	m := New(openStore(t))
	// Before any event the outcome series should already exist at 0 (not absent).
	body := scrape(t, m)
	if !strings.Contains(body, `holdfast_files_total{outcome="done"} 0`) {
		t.Errorf("expected pre-created done series at 0; body:\n%s", body)
	}
}
