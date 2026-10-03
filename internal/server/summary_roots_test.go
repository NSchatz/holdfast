package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/metrics"
	"github.com/NSchatz/holdfast/internal/secret"
	"github.com/NSchatz/holdfast/internal/store"
)

// The per-root sizing figures of GET /api/summary (S0169).
//
// Every case runs the REAL handler over a real SQLite store. A double stands in only for
// what is outside the server's boundary - a store read that fails, a filesystem that
// hangs - and never for the handler (testing T3).

// --- the fixture ---------------------------------------------------------------

// warnLog is an io.Writer a logger can write to from several goroutines.
type warnLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *warnLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// warns is every warn-level record written so far, one decoded JSON object each.
func (b *warnLog) warns(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if rec["level"] == "WARN" {
			out = append(out, rec)
		}
	}
	return out
}

// sizing is a real Server and Hub over a store, with the log captured.
type sizing struct {
	srv  *Server
	hub  *Hub
	ts   *httptest.Server
	logs *warnLog
}

// newSizing wires a server whose configuration names roots, reading through reads.
func newSizing(t *testing.T, reads store.Store, roots []string, readToken string, mx http.Handler) *sizing {
	t.Helper()
	logs := &warnLog{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ctx := context.Background()
	ctrl := NewController(ctx, func(context.Context) error { return nil }, discard())
	hub := NewHub(reads, ctrl, log)
	srv := New(ctx, config.Config{LibraryRoots: roots}, secret.Value{}, secret.NewValue(readToken),
		reads, ctrl, hub, mx, log)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &sizing{srv: srv, hub: hub, ts: ts, logs: logs}
}

// emptyStore opens a store holding no row at all.
func emptyStore(t *testing.T) *store.SQLite {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// summaryBody answers GET /api/summary and returns the status and the raw body.
func (s *sizing) summaryBody(t *testing.T) (int, string) {
	t.Helper()
	resp, err := http.Get(s.ts.URL + "/api/summary")
	if err != nil {
		t.Fatalf("GET /api/summary: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /api/summary: %v", err)
	}
	return resp.StatusCode, string(b)
}

// summary answers GET /api/summary, requires 200, and decodes it.
func (s *sizing) summary(t *testing.T) (controlState, string) {
	t.Helper()
	code, body := s.summaryBody(t)
	if code != http.StatusOK {
		t.Fatalf("GET /api/summary = %d, want 200:\n%s", code, body)
	}
	var got controlState
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode /api/summary: %v\n%s", err, body)
	}
	return got, body
}

// finishRow records one terminal row through the store's own write path.
func finishRow(t *testing.T, st *store.SQLite, path string, status store.Status, root string, source, output *int64) {
	t.Helper()
	fp := "fp:" + path
	mustClaim(t, st, path, fp)
	if err := st.Finish(context.Background(), path, fp, status,
		&store.Outcome{Decision: store.Decision{LibraryRoot: root}, SourceBytes: source, OutputBytes: output}, 3); err != nil {
		t.Fatalf("Finish(%s, %s): %v", status, path, err)
	}
}

// retain records one live retention through the store's own write path.
func retain(t *testing.T, st *store.SQLite, source string, size int64) {
	t.Helper()
	if err := st.Retain(context.Background(), store.Retained{
		SourcePath: source, SwappedPath: source, RetainedPath: source + ".retained",
		SourceBytes: size, SwappedFingerprint: "1:1", RetainedAt: 100, ExpiresAt: 200,
	}); err != nil {
		t.Fatalf("Retain(%s): %v", source, err)
	}
}

// want asserts one figure: a value, or null when v is nil.
func want(t *testing.T, name string, got *int64, v *int64) {
	t.Helper()
	switch {
	case v == nil && got != nil:
		t.Errorf("%s = %d, want null", name, *got)
	case v != nil && got == nil:
		t.Errorf("%s = null, want %d", name, *v)
	case v != nil && *got != *v:
		t.Errorf("%s = %d, want %d", name, *got, *v)
	}
}

// rootKeys is every field Definitions lists for one root, and unattributedKeys for the
// unattributed totals.
var (
	rootKeys = []string{"root", "candidate_files", "candidate_excluded", "candidate_bytes",
		"projection_basis_files", "projected_savings_bytes", "bytes_held_by_undo_window", "free_bytes"}
	unattributedKeys = []string{"candidate_files", "candidate_excluded", "candidate_bytes",
		"bytes_held_by_undo_window"}
)

func objectOf(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("not a JSON object: %v\n%s", err, raw)
	}
	return m
}

func assertExactKeys(t *testing.T, where string, got map[string]json.RawMessage, wantKeys []string) {
	t.Helper()
	if len(got) != len(wantKeys) {
		t.Errorf("%s carries %d fields, want exactly %d (%v): %v", where, len(got), len(wantKeys), wantKeys, got)
	}
	for _, k := range wantKeys {
		if _, ok := got[k]; !ok {
			t.Errorf("%s carries no %q field", where, k)
		}
	}
}

// --- AC-1 ----------------------------------------------------------------------

// TestS0169_AC1_RootsInConfigurationOrderBesideEveryOldField grades [AC-1]: N roots give N
// entries in CONFIGURATION order, each named by its cleaned path and carrying every field,
// plus the unattributed object and the top-level held figure, and every field the summary
// carried before keeps its name, type and value.
func TestS0169_AC1_RootsInConfigurationOrderBesideEveryOldField(t *testing.T) {
	st := newStore(t) // one encoding row, one done row
	base := t.TempDir()
	// Configuration order is deliberately NOT sorted order, and one root is written with a
	// trailing slash so "cleaned path" is graded rather than assumed.
	tv, films, music := filepath.Join(base, "tv"), filepath.Join(base, "films"), filepath.Join(base, "music")
	for _, d := range []string{tv, films, music} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s := newSizing(t, st, []string{tv + "/", films, music + "/./"}, "", nil)
	// The control state is not at its zero value, so "keeps its value" is graded on it.
	s.srv.ctrl.Pause()

	_, body := s.summary(t)
	top := objectOf(t, json.RawMessage(body))

	var roots []json.RawMessage
	if err := json.Unmarshal(top["roots"], &roots); err != nil {
		t.Fatalf("roots is not an array: %v\n%s", err, top["roots"])
	}
	if len(roots) != 3 {
		t.Fatalf("roots has %d entries for 3 configured roots:\n%s", len(roots), top["roots"])
	}
	for i, wantRoot := range []string{tv, films, music} {
		entry := objectOf(t, roots[i])
		assertExactKeys(t, "roots["+strconv.Itoa(i)+"]", entry, rootKeys)
		var got string
		if err := json.Unmarshal(entry["root"], &got); err != nil || got != wantRoot {
			t.Errorf("roots[%d].root = %s, want the cleaned path %q in configuration order", i, entry["root"], wantRoot)
		}
	}
	assertExactKeys(t, "roots_unattributed", objectOf(t, top["roots_unattributed"]), unattributedKeys)
	if string(top["bytes_held_by_undo_window"]) != "0" {
		t.Errorf("top-level bytes_held_by_undo_window = %s, want 0 over an empty retention table",
			top["bytes_held_by_undo_window"])
	}

	// Every field the summary carried before, by name, with the value the hub's own frame
	// states for the same ledger.
	snap := snapshotOf(t, s.hub)
	frame, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	frameKeys := objectOf(t, frame)
	for _, k := range []string{"summary", "bytes_reclaimed_session", "bytes_reclaimed_lifetime",
		"paused", "scanning", "aggregates"} {
		got, ok := top[k]
		if !ok {
			t.Errorf("the summary no longer carries %q", k)
			continue
		}
		if !sameJSON(t, got, frameKeys[k]) {
			t.Errorf("%q = %s, want %s (the value the frame states)", k, got, frameKeys[k])
		}
	}
	if string(top["paused"]) != "true" || string(top["scanning"]) != "false" {
		t.Errorf("paused = %s, scanning = %s, want true and false", top["paused"], top["scanning"])
	}
	var counts map[string]int
	if err := json.Unmarshal(top["summary"], &counts); err != nil {
		t.Fatalf("summary is not a map of integers: %v", err)
	}
	if counts["done"] != 1 || counts["encoding"] != 1 || len(counts) != 2 {
		t.Errorf("summary = %v, want done 1 and encoding 1", counts)
	}
	if len(top) != 9 {
		t.Errorf("the summary carries %d top-level fields, want the 6 it had and 3 added: %v", len(top), top)
	}
}

// sameJSON compares two JSON values ignoring every `age_seconds`, which is a property of
// when each was read and not of the figure.
func sameJSON(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	age := regexp.MustCompile(`"age_seconds":\d+`)
	return age.ReplaceAllString(string(a), `"age_seconds":0`) == age.ReplaceAllString(string(b), `"age_seconds":0`)
}

// --- AC-2 ----------------------------------------------------------------------

// TestS0169_AC2_CandidatesAreTheSizedDryRunRowsOfThatRoot grades [AC-2]: candidate_files
// counts the root's sized would-transcode rows, candidate_bytes sums them, the unsized
// ones are counted in candidate_excluded, and no other status contributes anything.
func TestS0169_AC2_CandidatesAreTheSizedDryRunRowsOfThatRoot(t *testing.T) {
	st := emptyStore(t)
	base := t.TempDir()
	a, b := filepath.Join(base, "a"), filepath.Join(base, "b")
	ctx := context.Background()

	// A pending row first: RecoverStale returns the claimed row to pending.
	mustClaim(t, st, a+"/pending.mkv", "p:p")
	if _, err := st.RecoverStale(ctx); err != nil {
		t.Fatalf("RecoverStale: %v", err)
	}

	finishRow(t, st, a+"/c1.mkv", store.WouldTranscode, a, i64p(1000), nil)
	finishRow(t, st, a+"/c2.mkv", store.WouldTranscode, a, i64p(234), nil)
	finishRow(t, st, a+"/c3.mkv", store.WouldTranscode, a, i64p(0), nil) // a real zero is sized
	finishRow(t, st, a+"/unsized1.mkv", store.WouldTranscode, a, nil, nil)
	finishRow(t, st, a+"/unsized2.mkv", store.WouldTranscode, a, nil, nil)
	// Every other status under the same root, each with a size that must be counted nowhere.
	finishRow(t, st, a+"/done.mkv", store.Done, a, i64p(70_000), i64p(30_000))
	finishRow(t, st, a+"/skipped.mkv", store.Skipped, a, i64p(80_000), nil)
	finishRow(t, st, a+"/failed.mkv", store.Failed, a, i64p(90_000), nil)
	// And another root's candidate, which is not this root's.
	finishRow(t, st, b+"/c.mkv", store.WouldTranscode, b, i64p(5), nil)

	sum, err := st.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []store.Status{store.Pending, store.Done, store.Skipped, store.Failed} {
		if sum[status] != 1 {
			t.Fatalf("the fixture holds %d %s rows, want 1: %v", sum[status], status, sum)
		}
	}

	got, _ := newSizing(t, st, []string{a, b}, "", nil).summary(t)

	want(t, "a.candidate_files", got.Roots[0].CandidateFiles, i64p(3))
	want(t, "a.candidate_bytes", got.Roots[0].CandidateBytes, i64p(1234))
	want(t, "a.candidate_excluded", got.Roots[0].CandidateExcluded, i64p(2))
	want(t, "b.candidate_files", got.Roots[1].CandidateFiles, i64p(1))
	want(t, "b.candidate_bytes", got.Roots[1].CandidateBytes, i64p(5))
	want(t, "b.candidate_excluded", got.Roots[1].CandidateExcluded, i64p(0))
	want(t, "unattributed.candidate_files", got.RootsUnattributed.CandidateFiles, i64p(0))
	want(t, "unattributed.candidate_bytes", got.RootsUnattributed.CandidateBytes, i64p(0))
	want(t, "unattributed.candidate_excluded", got.RootsUnattributed.CandidateExcluded, i64p(0))
}

// --- AC-3 ----------------------------------------------------------------------

const pib = int64(1) << 50

// TestS0169_AC3_ProjectionIsBytesWeightedFlooredAndExactAtAPebibyte grades [AC-3].
//
// The expected figure is worked by hand, not by the code under test. Root a holds
// candidate_bytes c = 3 PiB, and two done rows whose sources sum to B = 2 PiB = 2^51 and
// whose savings sum to S = 2^50 + 1. So
//
//	c x S / B = 3 x 2^50 x (2^50 + 1) / 2^51 = (3 x 2^50 + 3) / 2 = 3 x 2^49 + 1.5
//
// and the floor is 3 x 2^49 + 1 = 1688849860263937. c x S is about 2^101.6, which no
// 64-bit product holds, so an implementation that multiplies in int64 cannot get it.
func TestS0169_AC3_ProjectionIsBytesWeightedFlooredAndExactAtAPebibyte(t *testing.T) {
	st := emptyStore(t)
	base := t.TempDir()
	a, b, c := filepath.Join(base, "a"), filepath.Join(base, "b"), filepath.Join(base, "c")

	for i := 0; i < 3; i++ {
		finishRow(t, st, a+"/c"+strconv.Itoa(i)+".mkv", store.WouldTranscode, a, i64p(pib), nil)
	}
	finishRow(t, st, a+"/d1.mkv", store.Done, a, i64p(pib), i64p(pib/2))
	finishRow(t, st, a+"/d2.mkv", store.Done, a, i64p(pib), i64p(pib/2-1))
	// Done rows that record only ONE size are not basis rows.
	finishRow(t, st, a+"/d3.mkv", store.Done, a, i64p(pib), nil)
	finishRow(t, st, a+"/d4.mkv", store.Done, a, nil, i64p(pib))
	// Root b has candidates and no basis at all; root c has a basis and no candidate.
	finishRow(t, st, b+"/c.mkv", store.WouldTranscode, b, i64p(pib), nil)
	finishRow(t, st, b+"/d.mkv", store.Done, b, i64p(pib), nil)
	finishRow(t, st, c+"/d.mkv", store.Done, c, i64p(4*pib), i64p(pib))

	got, body := newSizing(t, st, []string{a, b, c}, "", nil).summary(t)

	want(t, "a.candidate_bytes", got.Roots[0].CandidateBytes, i64p(3*pib))
	want(t, "a.projection_basis_files", got.Roots[0].ProjectionBasisFiles, i64p(2))
	want(t, "a.projected_savings_bytes", got.Roots[0].ProjectedSavingsBytes, i64p(1688849860263937))

	want(t, "b.projection_basis_files", got.Roots[1].ProjectionBasisFiles, i64p(0))
	want(t, "b.projected_savings_bytes", got.Roots[1].ProjectedSavingsBytes, nil)
	if !strings.Contains(body, `"projection_basis_files":0,"projected_savings_bytes":null`) {
		t.Errorf("a root with no basis does not ship an explicit null projection beside basis 0:\n%s", body)
	}

	// A basis with nothing to project onto is a real zero: nothing is a candidate.
	want(t, "c.projection_basis_files", got.Roots[2].ProjectionBasisFiles, i64p(1))
	want(t, "c.projected_savings_bytes", got.Roots[2].ProjectedSavingsBytes, i64p(0))
}

// TestS0169_AC3_ProjectedSavingsArithmetic grades [AC-3]'s arithmetic at its edges, each
// against a figure worked by hand.
func TestS0169_AC3_ProjectedSavingsArithmetic(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		candidate, saved, basis int64
		want                    *int64
	}{
		{name: "exact", candidate: 100, saved: 3, basis: 4, want: i64p(75)},
		{name: "floored", candidate: 10, saved: 1, basis: 3, want: i64p(3)},
		// floor(-10/3) is -4; truncation toward zero would say -3.
		{name: "floor of a negative saving", candidate: 10, saved: -1, basis: 3, want: i64p(-4)},
		{name: "a basis of one byte", candidate: 7, saved: 1, basis: 1, want: i64p(7)},
		{name: "a basis summing to nothing", candidate: 100, saved: 0, basis: 0, want: nil},
		{name: "a negative basis", candidate: 100, saved: 5, basis: -5, want: nil},
		// 2 x MaxInt64 does not fit the wire's integer, and is not reported as another number.
		{name: "a quotient past int64", candidate: math.MaxInt64, saved: 4, basis: 2, want: nil},
		{name: "the largest quotient int64 holds", candidate: math.MaxInt64, saved: 2, basis: 2, want: i64p(math.MaxInt64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want(t, "projectedSavings", projectedSavings(tc.candidate, tc.saved, tc.basis), tc.want)
		})
	}
}

// --- AC-4 ----------------------------------------------------------------------

var heldGauge = regexp.MustCompile(`(?m)^holdfast_bytes_held_by_undo_window (\S+)$`)

// TestS0169_AC4_HeldBytesSplitByRootAddUpToTheGauge grades [AC-4]: the per-root held
// figures and the unattributed one sum to the top-level figure, which is the value the
// gauge reports over the same store, and a restored retention is counted by none of them.
func TestS0169_AC4_HeldBytesSplitByRootAddUpToTheGauge(t *testing.T) {
	st := emptyStore(t)
	base := t.TempDir()
	a, b := filepath.Join(base, "lib"), filepath.Join(base, "films")
	ctx := context.Background()

	retain(t, st, a+"/show/one.mkv", 1_000)
	retain(t, st, a+"/two.mkv", 20_000)
	retain(t, st, b+"/three.mkv", 300_000)
	// Restored: its bytes went back to the library, and nothing holds them.
	retain(t, st, a+"/restored.mkv", 4_000_000)
	if err := st.MarkRestored(ctx, a+"/restored.mkv", 150); err != nil {
		t.Fatalf("MarkRestored: %v", err)
	}
	// Under no configured root - and one whose path merely STARTS with a root's spelling,
	// which is not under it: containment is on a path boundary.
	retain(t, st, filepath.Join(base, "elsewhere", "four.mkv"), 50_000_000)
	retain(t, st, a+"2/five.mkv", 600_000_000)

	s := newSizing(t, st, []string{a, b}, "", metrics.New(st, nil).Handler())
	got, _ := s.summary(t)

	want(t, "a.bytes_held_by_undo_window", got.Roots[0].BytesHeldByUndoWindow, i64p(21_000))
	want(t, "b.bytes_held_by_undo_window", got.Roots[1].BytesHeldByUndoWindow, i64p(300_000))
	want(t, "unattributed.bytes_held_by_undo_window", got.RootsUnattributed.BytesHeldByUndoWindow, i64p(650_000_000))
	want(t, "bytes_held_by_undo_window", got.BytesHeldByUndoWindow, i64p(650_321_000))

	resp, err := http.Get(s.ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	exposition, _ := io.ReadAll(resp.Body)
	m := heldGauge.FindSubmatch(exposition)
	if m == nil {
		t.Fatalf("/metrics carries no holdfast_bytes_held_by_undo_window sample:\n%s", exposition)
	}
	gauge, err := strconv.ParseFloat(string(m[1]), 64)
	if err != nil {
		t.Fatalf("gauge value %q: %v", m[1], err)
	}
	if got.BytesHeldByUndoWindow == nil || float64(*got.BytesHeldByUndoWindow) != gauge {
		t.Errorf("the summary's held figure and the gauge disagree over one store: gauge %v", gauge)
	}
	sum := *got.Roots[0].BytesHeldByUndoWindow + *got.Roots[1].BytesHeldByUndoWindow +
		*got.RootsUnattributed.BytesHeldByUndoWindow
	if float64(sum) != gauge {
		t.Errorf("the per-root held figures and the unattributed one sum to %d, the gauge reads %v", sum, gauge)
	}
	// And the frame's figure, which the summary now matches.
	if frame := snapshotOf(t, s.hub).BytesHeldByUndoWindow; frame == nil || *frame != *got.BytesHeldByUndoWindow {
		t.Errorf("the SSE snapshot's held figure differs from the summary's")
	}
}

// --- AC-5 ----------------------------------------------------------------------

// TestS0169_AC5_FreeBytesIsWhatTheFilesystemHasForThisProcess grades [AC-5]: free_bytes is
// the bytes available to an unprivileged process on the filesystem holding the root,
// within 64 MiB of an independent statfs(2) f_bavail x f_bsize taken in the same test.
func TestS0169_AC5_FreeBytesIsWhatTheFilesystemHasForThisProcess(t *testing.T) {
	root := t.TempDir()
	s := newSizing(t, emptyStore(t), []string{root}, "", nil)

	got, _ := s.summary(t)

	var fs syscall.Statfs_t
	if err := syscall.Statfs(root, &fs); err != nil {
		t.Fatalf("statfs: %v", err)
	}
	independent := int64(fs.Bavail) * int64(fs.Bsize)
	if got.Roots[0].FreeBytes == nil {
		t.Fatalf("free_bytes is null for a real directory; warns: %v", s.logs.warns(t))
	}
	diff := *got.Roots[0].FreeBytes - independent
	if diff < 0 {
		diff = -diff
	}
	if diff > 64<<20 {
		t.Errorf("free_bytes = %d, an independent statfs says %d: %d bytes apart, more than 64 MiB",
			*got.Roots[0].FreeBytes, independent, diff)
	}
	if independent > 64<<20 && *got.Roots[0].FreeBytes <= 0 {
		t.Errorf("free_bytes = %d on a filesystem with %d available", *got.Roots[0].FreeBytes, independent)
	}
	if n := len(s.logs.warns(t)); n != 0 {
		t.Errorf("a summary whose every read succeeded logged %d warn records: %v", n, s.logs.warns(t))
	}
}

// --- AC-8, AC-9: a failing store read -------------------------------------------

// failingReads is a real store whose two sizing reads can be failed independently. It
// doubles for the store, which is outside the server's boundary; the handler is real.
type failingReads struct {
	*store.SQLite
	rootErr, heldErr atomic.Pointer[error]
	rootCalls        atomic.Int64
}

func (f *failingReads) RootTotals(ctx context.Context) ([]store.RootTotal, error) {
	f.rootCalls.Add(1)
	if err := f.rootErr.Load(); err != nil {
		return nil, *err
	}
	return f.SQLite.RootTotals(ctx)
}

func (f *failingReads) HeldBySource(ctx context.Context) ([]store.HeldSource, error) {
	if err := f.heldErr.Load(); err != nil {
		return nil, *err
	}
	return f.SQLite.HeldBySource(ctx)
}

// sizedFixture seeds two roots with candidates, a basis and a live retention each, plus an
// unattributed candidate and retention, so no figure is zero by accident.
func sizedFixture(t *testing.T) (st *store.SQLite, a, b string) {
	t.Helper()
	st = emptyStore(t)
	base := t.TempDir()
	a, b = filepath.Join(base, "a"), filepath.Join(base, "b")
	for _, d := range []string{a, b} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, root := range []string{a, b} {
		finishRow(t, st, root+"/c.mkv", store.WouldTranscode, root, i64p(8_000), nil)
		finishRow(t, st, root+"/u.mkv", store.WouldTranscode, root, nil, nil)
		finishRow(t, st, root+"/d.mkv", store.Done, root, i64p(4_000), i64p(1_000))
		retain(t, st, root+"/held.mkv", 500)
	}
	finishRow(t, st, "/gone/c.mkv", store.WouldTranscode, "/gone", i64p(77), nil)
	retain(t, st, "/gone/held.mkv", 9)
	return st, a, b
}

// nullFields counts how many times `"name":null` appears in body.
func nullFields(body, name string) int { return strings.Count(body, `"`+name+`":null`) }

// TestS0169_AC8_AFailedLedgerReadNullsOnlyTheCandidateAndProjectionFigures grades [AC-8].
func TestS0169_AC8_AFailedLedgerReadNullsOnlyTheCandidateAndProjectionFigures(t *testing.T) {
	st, a, b := sizedFixture(t)
	reads := &failingReads{SQLite: st}
	boom := errors.New("disk I/O error")
	reads.rootErr.Store(&boom)
	s := newSizing(t, reads, []string{a, b}, "", nil)

	got, body := s.summary(t)

	// Never 0: asserted on the raw bytes, where a null and a zero are different things.
	for name, n := range map[string]int{
		"candidate_files": 3, "candidate_bytes": 3, "candidate_excluded": 3, // two roots and unattributed
		"projected_savings_bytes": 2, "projection_basis_files": 2,
	} {
		if got := nullFields(body, name); got != n {
			t.Errorf("%d %s fields are null, want all %d:\n%s", got, name, n, body)
		}
	}
	for i := range got.Roots {
		want(t, "held", got.Roots[i].BytesHeldByUndoWindow, i64p(500))
		if got.Roots[i].FreeBytes == nil {
			t.Errorf("roots[%d].free_bytes went null with the ledger read", i)
		}
	}
	want(t, "unattributed held", got.RootsUnattributed.BytesHeldByUndoWindow, i64p(9))
	want(t, "top-level held", got.BytesHeldByUndoWindow, i64p(1009))
	if got.Summary["would-transcode"] != 5 || got.Summary["done"] != 2 || !got.Aggregates.Outcomes.Available {
		t.Errorf("a pre-existing field moved with the failed read: summary %v", got.Summary)
	}

	warns := s.logs.warns(t)
	if len(warns) != 1 {
		t.Fatalf("%d warn records, want exactly 1: %v", len(warns), warns)
	}
	w := warns[0]
	if w["dependency"] != "job store" || w["read"] != "RootTotals" ||
		!strings.Contains(w["msg"].(string), "unavailable") || !strings.Contains(w["err"].(string), "disk I/O error") {
		t.Errorf("the warn record does not name the dependency, the read and that the figures are "+
			"reported unavailable: %v", w)
	}
}

// TestS0169_AC8_TheLedgerReadIsBoundedByTheRefreshInterval grades the bound [AC-8]'s read
// sits behind: the whole-ledger read is issued at most once per interval however often the
// summary is polled, a failed read is retried once the interval has passed, and a failed
// refresh never republishes an older value as current.
func TestS0169_AC8_TheLedgerReadIsBoundedByTheRefreshInterval(t *testing.T) {
	st, a, b := sizedFixture(t)
	reads := &failingReads{SQLite: st}
	s := newSizing(t, reads, []string{a, b}, "", nil)
	var mu sync.Mutex
	now := time.Unix(1_700_000_000, 0)
	s.hub.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }

	first, _ := s.summary(t)
	want(t, "a.candidate_bytes", first.Roots[0].CandidateBytes, i64p(8_000))
	want(t, "a.projected_savings_bytes", first.Roots[0].ProjectedSavingsBytes, i64p(6_000))

	// A new candidate lands; inside the interval the figure is the cached one.
	finishRow(t, st, a+"/c2.mkv", store.WouldTranscode, a, i64p(2_000), nil)
	advance(ledgerFigureInterval - time.Second)
	for i := 0; i < 5; i++ {
		got, _ := s.summary(t)
		want(t, "a.candidate_bytes inside the interval", got.Roots[0].CandidateBytes, i64p(8_000))
	}
	if n := reads.rootCalls.Load(); n != 1 {
		t.Fatalf("6 summaries inside one interval issued %d whole-ledger reads, want 1", n)
	}

	// At the interval the read runs again, and this one fails: null, not the old value.
	boom := errors.New("database is locked")
	reads.rootErr.Store(&boom)
	advance(time.Second)
	got, _ := s.summary(t)
	want(t, "a.candidate_bytes after a failed refresh", got.Roots[0].CandidateBytes, nil)
	want(t, "a.projection_basis_files after a failed refresh", got.Roots[0].ProjectionBasisFiles, nil)
	if n := reads.rootCalls.Load(); n != 2 {
		t.Fatalf("the read at the interval boundary: %d reads in total, want 2", n)
	}

	// The failed attempt spends its interval: no retry per request.
	reads.rootErr.Store(nil)
	advance(ledgerFigureInterval - time.Second)
	got, _ = s.summary(t)
	want(t, "a.candidate_bytes inside the failed interval", got.Roots[0].CandidateBytes, nil)
	if n := reads.rootCalls.Load(); n != 2 {
		t.Fatalf("a failed read was retried inside its interval: %d reads, want 2", n)
	}

	advance(time.Second)
	got, _ = s.summary(t)
	want(t, "a.candidate_bytes once a later refresh reads it", got.Roots[0].CandidateBytes, i64p(10_000))
	want(t, "a.candidate_files once a later refresh reads it", got.Roots[0].CandidateFiles, i64p(2))
	if len(s.logs.warns(t)) != 1 {
		t.Errorf("one failed refresh logged %d warn records, want 1", len(s.logs.warns(t)))
	}
}

// TestS0169_AC9_AFailedHeldReadNullsEveryHeldFigureAndNothingElse grades [AC-9].
func TestS0169_AC9_AFailedHeldReadNullsEveryHeldFigureAndNothingElse(t *testing.T) {
	st, a, b := sizedFixture(t)
	reads := &failingReads{SQLite: st}
	boom := errors.New("no such table: retained_originals")
	reads.heldErr.Store(&boom)
	s := newSizing(t, reads, []string{a, b}, "", nil)

	got, body := s.summary(t)

	// The top-level figure, two roots and the unattributed total: four nulls and no zero.
	if n := nullFields(body, "bytes_held_by_undo_window"); n != 4 {
		t.Errorf("%d bytes_held_by_undo_window fields are null, want all 4:\n%s", n, body)
	}
	for i := range got.Roots {
		want(t, "candidate_files", got.Roots[i].CandidateFiles, i64p(1))
		want(t, "candidate_bytes", got.Roots[i].CandidateBytes, i64p(8_000))
		want(t, "candidate_excluded", got.Roots[i].CandidateExcluded, i64p(1))
		want(t, "projection_basis_files", got.Roots[i].ProjectionBasisFiles, i64p(1))
		want(t, "projected_savings_bytes", got.Roots[i].ProjectedSavingsBytes, i64p(6_000))
		if got.Roots[i].FreeBytes == nil {
			t.Errorf("roots[%d].free_bytes went null with the held read", i)
		}
	}
	want(t, "unattributed.candidate_bytes", got.RootsUnattributed.CandidateBytes, i64p(77))
	if got.Summary["would-transcode"] != 5 {
		t.Errorf("a pre-existing field moved with the failed read: summary %v", got.Summary)
	}

	warns := s.logs.warns(t)
	if len(warns) != 1 {
		t.Fatalf("%d warn records, want exactly 1: %v", len(warns), warns)
	}
	w := warns[0]
	if w["dependency"] != "job store" || w["figure"] != "bytes_held_by_undo_window" ||
		!strings.Contains(w["msg"].(string), "unavailable") ||
		!strings.Contains(w["err"].(string), "retained_originals") {
		t.Errorf("the warn record does not name the dependency and the unavailable figure: %v", w)
	}
}

// --- AC-10, AC-11: a failing or hanging filesystem ------------------------------

// TestS0169_AC10_ARootThatCannotBeInspectedHasNullFreeBytesAlone grades [AC-10], against
// the real filesystem: a root whose directory does not exist.
func TestS0169_AC10_ARootThatCannotBeInspectedHasNullFreeBytesAlone(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "not-mounted")
	s := newSizing(t, emptyStore(t), []string{base, missing, base + "/"}, "", nil)

	got, body := s.summary(t)

	if got.Roots[0].FreeBytes == nil || got.Roots[2].FreeBytes == nil {
		t.Errorf("a readable root lost its free_bytes because another root could not be read:\n%s", body)
	}
	want(t, "missing.free_bytes", got.Roots[1].FreeBytes, nil)
	if n := nullFields(body, "free_bytes"); n != 1 {
		t.Errorf("%d free_bytes fields are null, want exactly the missing root's:\n%s", n, body)
	}
	want(t, "missing.candidate_files", got.Roots[1].CandidateFiles, i64p(0))
	want(t, "missing.bytes_held_by_undo_window", got.Roots[1].BytesHeldByUndoWindow, i64p(0))

	warns := s.logs.warns(t)
	if len(warns) != 1 {
		t.Fatalf("%d warn records, want exactly 1: %v", len(warns), warns)
	}
	w := warns[0]
	if w["root"] != missing || !strings.Contains(w["read"].(string), "free space") ||
		!strings.Contains(w["msg"].(string), "unavailable") {
		t.Errorf("the warn record does not name the root, the read and that the figure is "+
			"reported unavailable: %v", w)
	}
}

// hungMount is a free-space read that blocks for one root until the test ends, the way
// statfs(2) blocks on a network mount that has gone away.
type hungMount struct {
	hung    string
	release chan struct{}
	calls   atomic.Int64
}

func newHungMount(t *testing.T, hung string) *hungMount {
	h := &hungMount{hung: hung, release: make(chan struct{})}
	t.Cleanup(func() { close(h.release) })
	return h
}

func (h *hungMount) read(path string) (uint64, error) {
	if path == h.hung {
		h.calls.Add(1)
		<-h.release
		return 0, errors.New("released")
	}
	return 4096, nil
}

// TestS0169_AC11_AHungMountIsBoundedAtTwoSeconds grades [AC-11] at the SHIPPED bound: a
// free-space read that never returns leaves that root's free_bytes null, every other
// root's read, and the response complete within 3 seconds - and not before the 2 second
// bound, which is what gives a slow mount its chance to answer.
func TestS0169_AC11_AHungMountIsBoundedAtTwoSeconds(t *testing.T) {
	s := newSizing(t, emptyStore(t), []string{"/mnt/ok", "/mnt/hung", "/mnt/hung-too"}, "", nil)
	mount := newHungMount(t, "/mnt/hung")
	also := newHungMount(t, "/mnt/hung-too")
	s.srv.free.read = func(path string) (uint64, error) {
		if path == also.hung {
			return also.read(path)
		}
		return mount.read(path)
	}

	start := time.Now()
	got, _ := s.summary(t)
	elapsed := time.Since(start)

	if elapsed >= 3*time.Second {
		t.Errorf("the summary took %v with two hung mounts, want under 3s", elapsed)
	}
	if elapsed < freeSpaceTimeout {
		t.Errorf("the summary gave up on the mount after %v, before its %v bound", elapsed, freeSpaceTimeout)
	}
	if freeSpaceTimeout != 2*time.Second {
		t.Errorf("the free-space bound is %v, want 2s", freeSpaceTimeout)
	}
	want(t, "ok.free_bytes", got.Roots[0].FreeBytes, i64p(4096))
	want(t, "hung.free_bytes", got.Roots[1].FreeBytes, nil)
	want(t, "hung-too.free_bytes", got.Roots[2].FreeBytes, nil)

	warns := s.logs.warns(t)
	if len(warns) != 2 {
		t.Fatalf("%d warn records, want one per hung root: %v", len(warns), warns)
	}
	for _, w := range warns {
		if !strings.Contains(w["err"].(string), "did not return within its bound") {
			t.Errorf("the warn record does not say the read timed out: %v", w)
		}
	}
}

// TestS0169_AC11_AHungReadIsNotRepeatedByLaterRequests grades [AC-11]'s cost: a read that
// hangs cannot be cancelled, so a polled summary must not start another one per request.
// Later requests join the read in flight, and once its bound has passed they are answered
// at once rather than each waiting the bound again.
func TestS0169_AC11_AHungReadIsNotRepeatedByLaterRequests(t *testing.T) {
	const bound = time.Second
	s := newSizing(t, emptyStore(t), []string{"/mnt/hung"}, "", nil)
	mount := newHungMount(t, "/mnt/hung")
	s.srv.free.read = mount.read
	s.srv.free.timeout = bound

	start := time.Now()
	got, _ := s.summary(t)
	first := time.Since(start)
	want(t, "hung.free_bytes", got.Roots[0].FreeBytes, nil)
	if first < bound {
		t.Errorf("the first request gave up after %v, before the configured %v bound", first, bound)
	}
	if first >= freeSpaceTimeout {
		t.Errorf("the first request took %v: the configured %v bound was not the one applied", first, bound)
	}

	// Each of these would take the whole bound again if it waited on the read afresh.
	for i := 0; i < 5; i++ {
		start := time.Now()
		got, _ := s.summary(t)
		if d := time.Since(start); d >= bound*8/10 {
			t.Errorf("request %d waited %v on a read whose %v bound had already passed", i, d, bound)
		}
		want(t, "hung.free_bytes", got.Roots[0].FreeBytes, nil)
	}
	if n := mount.calls.Load(); n != 1 {
		t.Errorf("6 requests started %d reads of a hung mount, want 1", n)
	}
}

// TestS0169_AC11_FreeSpaceReadsAreSharedAndThenReadAgain grades the other side of that
// rule: concurrent requests share one read and all get its answer, a finished read is not
// reused (the next request reads the filesystem again), and the seam's failures and
// out-of-range answers are null rather than a number.
func TestS0169_AC11_FreeSpaceReadsAreSharedAndThenReadAgain(t *testing.T) {
	var calls atomic.Int64
	gate := make(chan struct{})
	answer := atomic.Uint64{}
	answer.Store(1 << 30)
	f := &freeSpace{timeout: 5 * time.Second, read: func(string) (uint64, error) {
		calls.Add(1)
		<-gate
		return answer.Load(), nil
	}}

	results := make(chan int64, 4)
	for i := 0; i < 4; i++ {
		go func() {
			v, err := f.bytes(context.Background(), "/mnt/slow")
			if err != nil {
				v = -1
			}
			results <- v
		}()
	}
	// Let all four arrive before the read is allowed to answer.
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	for i := 0; i < 4; i++ {
		if v := <-results; v != 1<<30 {
			t.Errorf("a concurrent caller got %d, want the shared read's 1 GiB", v)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("4 concurrent callers started %d reads, want 1", n)
	}

	// Finished: the next caller reads again and sees the new answer.
	answer.Store(2 << 30)
	if v, err := f.bytes(context.Background(), "/mnt/slow"); err != nil || v != 2<<30 {
		t.Errorf("after the read finished, bytes = %d, %v; want a fresh read's 2 GiB", v, err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("a finished read was reused: %d reads, want 2", n)
	}

	// A different root is a different read.
	if _, err := f.bytes(context.Background(), "/mnt/other"); err != nil || calls.Load() != 3 {
		t.Errorf("a second root did not get its own read: calls %d, err %v", calls.Load(), err)
	}

	// The largest figure the wire holds is served; one past it is refused, not wrapped.
	answer.Store(math.MaxInt64)
	if v, err := f.bytes(context.Background(), "/mnt/slow"); err != nil || v != math.MaxInt64 {
		t.Errorf("bytes at MaxInt64 = %d, %v", v, err)
	}
	answer.Store(math.MaxInt64 + 1)
	if v, err := f.bytes(context.Background(), "/mnt/slow"); !errors.Is(err, errFreeSpaceRange) || v != 0 {
		t.Errorf("bytes past MaxInt64 = %d, %v; want errFreeSpaceRange", v, err)
	}

	// A cancelled request stops waiting.
	blocked := &freeSpace{timeout: 5 * time.Second, read: func(string) (uint64, error) {
		<-make(chan struct{})
		return 0, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, err := blocked.bytes(ctx, "/mnt/hung"); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled request = %v, want context.Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("a cancelled request waited %v", time.Since(start))
	}

	// The seam's own error is the error.
	denied := errors.New("permission denied")
	failing := &freeSpace{read: func(string) (uint64, error) { return 12345, denied }}
	if v, err := failing.bytes(context.Background(), "/mnt/x"); !errors.Is(err, denied) || v != 0 {
		t.Errorf("a failed read = %d, %v; want 0 and the read's error", v, err)
	}
}

// --- AC-12 ---------------------------------------------------------------------

// TestS0169_AC12_EmptyIsZeroAndUnknownRootsAreUnattributed grades [AC-12]: an empty
// ledger and retention table read as true zeros (and a null projection, which has no
// basis), and a row recording no root, or one no longer configured, is counted in
// roots_unattributed and in no root.
func TestS0169_AC12_EmptyIsZeroAndUnknownRootsAreUnattributed(t *testing.T) {
	st := emptyStore(t)
	base := t.TempDir()
	a, b := filepath.Join(base, "a"), filepath.Join(base, "b")
	s := newSizing(t, st, []string{a, b}, "", nil)
	var mu sync.Mutex
	now := time.Unix(1_700_000_000, 0)
	s.hub.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }

	got, body := s.summary(t)
	for i := range got.Roots {
		want(t, "candidate_files", got.Roots[i].CandidateFiles, i64p(0))
		want(t, "candidate_bytes", got.Roots[i].CandidateBytes, i64p(0))
		want(t, "candidate_excluded", got.Roots[i].CandidateExcluded, i64p(0))
		want(t, "projection_basis_files", got.Roots[i].ProjectionBasisFiles, i64p(0))
		want(t, "projected_savings_bytes", got.Roots[i].ProjectedSavingsBytes, nil)
		want(t, "bytes_held_by_undo_window", got.Roots[i].BytesHeldByUndoWindow, i64p(0))
	}
	want(t, "unattributed.candidate_files", got.RootsUnattributed.CandidateFiles, i64p(0))
	want(t, "unattributed.candidate_bytes", got.RootsUnattributed.CandidateBytes, i64p(0))
	want(t, "unattributed.candidate_excluded", got.RootsUnattributed.CandidateExcluded, i64p(0))
	want(t, "unattributed.bytes_held_by_undo_window", got.RootsUnattributed.BytesHeldByUndoWindow, i64p(0))
	want(t, "bytes_held_by_undo_window", got.BytesHeldByUndoWindow, i64p(0))
	if !strings.Contains(body, `"roots_unattributed":{"candidate_files":0,"candidate_excluded":0,"candidate_bytes":0,"bytes_held_by_undo_window":0}`) {
		t.Errorf("roots_unattributed is not all zero over an empty ledger:\n%s", body)
	}

	// A row recording no root, two recording roots the configuration no longer names, and
	// one of root a's own so "in no root" is graded beside a root that does count.
	finishRow(t, st, "/old/no-root.mkv", store.WouldTranscode, "", i64p(100), nil)
	finishRow(t, st, "/removed/c.mkv", store.WouldTranscode, "/removed", i64p(20), nil)
	finishRow(t, st, "/removed/u.mkv", store.WouldTranscode, "/removed", nil, nil)
	finishRow(t, st, "/removed-too/c.mkv", store.WouldTranscode, "/removed-too", i64p(3), nil)
	finishRow(t, st, "/removed/d.mkv", store.Done, "/removed", i64p(4_000), i64p(1_000))
	finishRow(t, st, a+"/c.mkv", store.WouldTranscode, a, i64p(5_000), nil)

	mu.Lock()
	now = now.Add(ledgerFigureInterval)
	mu.Unlock()
	got, _ = s.summary(t)

	want(t, "unattributed.candidate_files", got.RootsUnattributed.CandidateFiles, i64p(3))
	want(t, "unattributed.candidate_bytes", got.RootsUnattributed.CandidateBytes, i64p(123))
	want(t, "unattributed.candidate_excluded", got.RootsUnattributed.CandidateExcluded, i64p(1))
	want(t, "a.candidate_files", got.Roots[0].CandidateFiles, i64p(1))
	want(t, "a.candidate_bytes", got.Roots[0].CandidateBytes, i64p(5_000))
	want(t, "a.candidate_excluded", got.Roots[0].CandidateExcluded, i64p(0))
	want(t, "a.projection_basis_files", got.Roots[0].ProjectionBasisFiles, i64p(0))
	want(t, "b.candidate_files", got.Roots[1].CandidateFiles, i64p(0))
	want(t, "b.candidate_bytes", got.Roots[1].CandidateBytes, i64p(0))
	want(t, "b.projection_basis_files", got.Roots[1].ProjectionBasisFiles, i64p(0))
}

// --- AC-14 ---------------------------------------------------------------------

// TestS0169_AC14_TheSchemaDescribesEveryAddedFieldAndItsNull grades [AC-14]: /api/schema
// describes the summary's 200 body with the three additions and every per-root field,
// each nullable exactly where null is a value.
func TestS0169_AC14_TheSchemaDescribesEveryAddedFieldAndItsNull(t *testing.T) {
	s := newSizing(t, emptyStore(t), []string{t.TempDir()}, "", nil)
	resp, err := http.Get(s.ts.URL + SchemaPath)
	if err != nil {
		t.Fatalf("GET %s: %v", SchemaPath, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	doc, err := ParseDocument(raw)
	if err != nil {
		t.Fatalf("ParseDocument: %v", err)
	}

	var body *Shape
	for _, e := range doc.Endpoints {
		if e.Method != http.MethodGet || e.Path != "/api/summary" {
			continue
		}
		for i := range e.Responses {
			if e.Responses[i].Status == http.StatusOK {
				body = &e.Responses[i].Body
			}
		}
	}
	if body == nil {
		t.Fatal("the surface document describes no 200 body for GET /api/summary")
	}
	field := func(sh Shape, name string) Field {
		t.Helper()
		for _, f := range sh.Fields {
			if f.Name == name {
				return f
			}
		}
		t.Fatalf("the document describes no %q field in %+v", name, sh.Fields)
		return Field{}
	}
	nullableInteger := func(where string, f Field) {
		t.Helper()
		if f.Type.Kind != kindInteger || !f.Type.Nullable || !f.Required {
			t.Errorf("%s.%s is described as %+v, want a required, nullable integer", where, f.Name, f)
		}
	}

	nullableInteger("summary", field(*body, "bytes_held_by_undo_window"))

	roots := field(*body, "roots")
	if roots.Type.Kind != kindArray || roots.Type.Elem == nil || roots.Type.Elem.Kind != kindObject || !roots.Required {
		t.Fatalf("roots is described as %+v, want a required array of objects", roots)
	}
	entry := *roots.Type.Elem
	if len(entry.Fields) != len(rootKeys) {
		t.Errorf("a root entry is described with %d fields, want %d", len(entry.Fields), len(rootKeys))
	}
	root := field(entry, "root")
	if root.Type.Kind != kindString || root.Type.Nullable || !root.Required {
		t.Errorf("roots[].root is described as %+v, want a required string that is never null", root)
	}
	for _, name := range rootKeys[1:] {
		nullableInteger("roots[]", field(entry, name))
	}

	un := field(*body, "roots_unattributed")
	if un.Type.Kind != kindObject || un.Type.Nullable || !un.Required || len(un.Type.Fields) != len(unattributedKeys) {
		t.Fatalf("roots_unattributed is described as %+v, want a required object of %d fields", un, len(unattributedKeys))
	}
	for _, name := range unattributedKeys {
		nullableInteger("roots_unattributed", field(un.Type, name))
	}

	// And a live response - nulls included - validates against that description.
	code, live := s.summaryBody(t)
	if code != http.StatusOK {
		t.Fatalf("GET /api/summary = %d", code)
	}
	violations, err := doc.ValidateResponse(http.MethodGet, "/api/summary", http.StatusOK, []byte(live))
	if err != nil || len(violations) != 0 {
		t.Errorf("a live summary does not validate against the document: %v %v", err, violations)
	}
}

// --- AC-16 ---------------------------------------------------------------------

// TestS0169_AC16_TheReadGateStillRefusesAndNamesNoRoot grades [AC-16]: with a read token
// configured, a request without it is answered 401 as before, and the body carries no
// root path and no per-root figure. The credentialled request is the control: the same
// server does serve them.
func TestS0169_AC16_TheReadGateStillRefusesAndNamesNoRoot(t *testing.T) {
	st, a, b := sizedFixture(t)
	s := newSizing(t, st, []string{a, b}, readTok, nil)

	for _, authorization := range []string{"", "Bearer not-the-token"} {
		resp, body := get(t, s.ts.URL, "/api/summary", authorization)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GET /api/summary with Authorization %q = %d, want 401", authorization, resp.StatusCode)
		}
		for _, leak := range []string{a, b, "roots", "candidate", "free_bytes", "bytes_held", "projected"} {
			if strings.Contains(body, leak) {
				t.Errorf("the 401 body carries %q:\n%s", leak, body)
			}
		}
		if strings.TrimSpace(body) != "unauthorized" {
			t.Errorf("the 401 body is %q, want the refusal it has always been", body)
		}
	}

	resp, body := get(t, s.ts.URL, "/api/summary", "Bearer "+readTok)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, a) || !strings.Contains(body, `"candidate_bytes":8000`) {
		t.Errorf("the credentialled request = %d and does not carry the per-root figures:\n%s", resp.StatusCode, body)
	}
}
