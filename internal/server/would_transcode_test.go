package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/store"
)

// The READ SURFACE for a dry run's recorded decision.
//
// The surface is unauthenticated, so every case here is also a case about what it now says
// to anybody who can reach the port: a new per-file fact on the rows, and NOTHING new
// inside the library-wide figures.

// seedCandidate records one dry-run decision through the real store.
func seedCandidate(t *testing.T, st *store.SQLite, path, fp string, o *store.Outcome) {
	t.Helper()
	mustClaim(t, st, path, fp)
	if err := st.Finish(context.Background(), path, fp, store.WouldTranscode, o, 3); err != nil {
		t.Fatalf("Finish(would-transcode, %s): %v", path, err)
	}
}

func i64p(v int64) *int64 { return &v }

// TestSnapshot_ACandidateIsCountedAndServedAsFinishedWork is AC8 in one case: the summary
// counts it under its own key, the outcomes breakdown carries a bucket for it, the set that
// breakdown declares NAMES it, and the row is served in the terminal view rather than the
// in-flight queue.
//
// The view half is the one with teeth. The page draws the two groups apart, and the split
// is a partition of the whole status vocabulary - so a status served in neither view is a
// file an operator cannot see at all, and one served in the queue says a worker is still
// examining a file nothing is examining.
func TestSnapshot_ACandidateIsCountedAndServedAsFinishedWork(t *testing.T) {
	h := newHarness(t, "")
	seedCandidate(t, h.st, "/lib/candidate.mkv", "9:9", &store.Outcome{
		SourceCodec: "h264", SourceBytes: i64p(4_000_000),
	})

	snap := snapshotOf(t, h.hub)

	if got := snap.Summary[string(store.WouldTranscode)]; got != 1 {
		t.Errorf("the summary counts %d would-transcode rows under their own key, want 1: %+v",
			got, snap.Summary)
	}

	out := snap.Aggregates.Outcomes
	if !out.Available {
		t.Fatalf("the outcomes breakdown is unavailable: %+v", out)
	}
	var bucket *bucketDTO
	for i := range out.Buckets {
		if out.Buckets[i].Key == string(store.WouldTranscode) {
			bucket = &out.Buckets[i]
		}
	}
	if bucket == nil {
		t.Errorf("the outcomes breakdown carries no would-transcode bucket, so a dry run reads as "+
			"having concluded nothing: %+v", out.Buckets)
	} else if bucket.Count != 1 {
		t.Errorf("the would-transcode bucket counts %d, want 1", bucket.Count)
	}
	if !strings.Contains(out.Covers, string(store.WouldTranscode)) {
		t.Errorf("the set the outcomes breakdown declares does not name would-transcode, so its "+
			"scope is stated wrongly rather than merely narrowly: %q", out.Covers)
	}

	// Served in the terminal view, and in exactly one view.
	if !hasPath(snap.History, "/lib/candidate.mkv") {
		t.Errorf("the candidate row is not served in the terminal view: %+v", snap.History)
	}
	if hasPath(snap.Queue, "/lib/candidate.mkv") {
		t.Errorf("the candidate row is served in the in-flight queue; the decision is taken and "+
			"nothing is examining that file: %+v", snap.Queue)
	}
	if !strings.Contains(snap.HistoryTotal.Covers, string(store.WouldTranscode)) {
		t.Errorf("the terminal view's declared total does not name would-transcode, so the rows it "+
			"ships and the total it states them against are over different sets: %q", snap.HistoryTotal.Covers)
	}
}

// TestHistoryRow_CarriesTheCodecAndSizeWithAbsenceTellableApart is AC9, asserted on the RAW
// BYTES of the response.
//
// Decoding into a struct is exactly what would hide the defect: a missing key and an
// explicit null both arrive as the zero value, so a test that reads j.SourceCodec cannot
// tell "not recorded" from "recorded as an empty string", which is the distinction the
// whole outcome schema exists to keep.
func TestHistoryRow_CarriesTheCodecAndSizeWithAbsenceTellableApart(t *testing.T) {
	h := newHarness(t, "")
	seedCandidate(t, h.st, "/lib/recorded.mkv", "9:9", &store.Outcome{
		SourceCodec: "h264", SourceBytes: i64p(4_000_000),
	})
	// The absence row: a decision whose codec and size could not be read. Both must arrive
	// as an explicit null a client can tell apart from "" and from 0.
	seedCandidate(t, h.st, "/lib/unrecorded.mkv", "8:8", &store.Outcome{})

	ts := httptest.NewServer(h.srv)
	defer ts.Close()

	raw := getRaw(t, ts.URL+"/api/history")
	var body struct {
		History []json.RawMessage `json:"history"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("unmarshal /api/history: %v", err)
	}
	rows := map[string]map[string]json.RawMessage{}
	for _, r := range body.History {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(r, &m); err != nil {
			t.Fatalf("unmarshal row: %v", err)
		}
		var path string
		if err := json.Unmarshal(m["path"], &path); err != nil {
			t.Fatalf("unmarshal row path: %v", err)
		}
		rows[path] = m
	}

	recorded, ok := rows["/lib/recorded.mkv"]
	if !ok {
		t.Fatalf("the recorded candidate is not in the response: %v", rows)
	}
	if got := string(recorded["source_codec"]); got != `"h264"` {
		t.Errorf("source_codec on a recorded candidate is %s, want \"h264\"", got)
	}
	if got := string(recorded["source_bytes"]); got != "4000000" {
		t.Errorf("source_bytes on a recorded candidate is %s, want 4000000", got)
	}
	// Nothing encoded it, so there is no output to have a size.
	if got := string(recorded["output_bytes"]); got != "null" {
		t.Errorf("output_bytes on a candidate is %s; nothing has encoded that file, so any figure "+
			"here would be one nobody measured", got)
	}

	absent, ok := rows["/lib/unrecorded.mkv"]
	if !ok {
		t.Fatalf("the unrecorded candidate is not in the response: %v", rows)
	}
	for _, field := range []string{"source_codec", "source_bytes"} {
		got, present := absent[field]
		if !present {
			t.Errorf("%s is ABSENT from the row rather than served as an explicit absence; a client "+
				"then has to decide for itself whether the fact was unrecorded or the field went away", field)
			continue
		}
		if string(got) != "null" {
			t.Errorf("%s on a candidate that recorded neither is %s, want null. A \"\" or a 0 here is "+
				"a value, and a reader cannot tell a value from a measurement nobody took", field, got)
		}
	}
}

// TestAggregates_SayNothingNewAboutAnyIndividualFile is AC10.
//
// /metrics, the summary and the aggregates need no token, so a per-file datum added to the
// figures is published to anyone who can reach the port. The rows already carry paths -
// that is what /api/queue and /api/history are - but the library-wide figures never have,
// and adding a bucket must not be the change that starts.
func TestAggregates_SayNothingNewAboutAnyIndividualFile(t *testing.T) {
	h := newHarness(t, "")
	seedCandidate(t, h.st, "/lib/secret-holiday-video.mkv", "9:9", &store.Outcome{
		SourceCodec: "h264", SourceBytes: i64p(4_000_000),
	})

	aggs, err := json.Marshal(snapshotOf(t, h.hub).Aggregates)
	if err != nil {
		t.Fatalf("marshal aggregates: %v", err)
	}
	text := string(aggs)
	for _, leak := range []string{"secret-holiday-video", "/lib/", "9:9", "w0"} {
		if strings.Contains(text, leak) {
			t.Errorf("the aggregates object carries %q - a per-file datum on an unauthenticated "+
				"library-wide figure:\n%s", leak, text)
		}
	}
	// And it is not vacuous: the figure really did see that row.
	var found bool
	for _, b := range snapshotOf(t, h.hub).Aggregates.Outcomes.Buckets {
		if b.Key == string(store.WouldTranscode) {
			found = true
		}
	}
	if !found {
		t.Fatal("the outcomes breakdown never counted the seeded candidate, so this case proves nothing")
	}
}

// TestAggregates_AnUnreadableOutcomesFigureCostsNothingElse is AC11: the failure envelope is
// the one that already exists, and adding a bucket to that figure does not change what its
// failure costs.
//
// A zero would be the dangerous answer, not a missing one: "0 candidates" beside rows an
// operator can see with their own eyes is the page inventing the one fact it has just been
// unable to read.
func TestAggregates_AnUnreadableOutcomesFigureCostsNothingElse(t *testing.T) {
	h := newHarness(t, "")
	seedCandidate(t, h.st, "/lib/candidate.mkv", "9:9", &store.Outcome{
		SourceCodec: "h264", SourceBytes: i64p(4_000_000),
	})

	broken := aggStore{SQLite: h.st, breakOne: func(a store.Aggregates) store.Aggregates {
		a.Outcomes.Err = errors.New("simulated outcomes read failure")
		a.Outcomes.Buckets = nil
		a.Outcomes.Counted = 0
		return a
	}}
	hub := NewHub(broken, h.ctrl, discard())
	snap := snapshotOf(t, hub)

	out := snap.Aggregates.Outcomes
	if out.Available {
		t.Fatal("the broken figure reports itself available; the seam did not take")
	}
	if out.Unavailable != aggregateUnavailable {
		t.Errorf("the unreadable figure is reported as %q, want the fixed words %q",
			out.Unavailable, aggregateUnavailable)
	}
	if len(out.Buckets) != 0 || out.Counted != 0 {
		t.Errorf("an unreadable outcomes figure shipped counts anyway: %+v", out)
	}
	if strings.Contains(out.Unavailable, "simulated") {
		t.Errorf("the unauthenticated response carries the driver's own error text: %q", out.Unavailable)
	}

	// Everything else still ships, including the row and the count of it.
	if got := snap.Summary[string(store.WouldTranscode)]; got != 1 {
		t.Errorf("the summary lost its would-transcode count when one aggregate failed: %+v", snap.Summary)
	}
	if !hasPath(snap.History, "/lib/candidate.mkv") {
		t.Errorf("the candidate row was lost when one aggregate failed: %+v", snap.History)
	}
	for name, ok := range map[string]bool{
		"skips_by_guard": snap.Aggregates.SkipsByGuard.Available,
		"size_ratio":     snap.Aggregates.SizeRatio.Available,
		"encode_ms":      snap.Aggregates.EncodeMs.Available,
		"vmaf_mean":      snap.Aggregates.VmafMean.Available,
		"vmaf_min":       snap.Aggregates.VmafMin.Available,
	} {
		if !ok {
			t.Errorf("the %s figure went down with the outcomes figure", name)
		}
	}
}

func hasPath(rows []jobDTO, path string) bool {
	for _, j := range rows {
		if j.Path == path {
			return true
		}
	}
	return false
}
