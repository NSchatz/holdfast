package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/NSchatz/holdfast/internal/store"
)

// TestHistoryRowJSON_CarriesDeinterlaceFields grades [AC-13] of
// S0107-holdfast-interlacing-decision: a terminal row published through `holdfast export`
// states whether a deinterlace was applied and which filter and parameters produced it, as
// explicit fields, so the transformation is visible rather than inferable.
//
// A deinterlace is the one transformation this tool performs that cannot be read back from
// the replacement. Every other knob changes how the same picture was encoded; this removes
// the fields the source carried, and the source is then deleted. If the row does not say
// so, nothing does.
//
// The three rows below are the three states the pair can be in, and they are asserted on
// the RAW BYTES because a missing key and an explicit null both decode to the same zero
// value - which is exactly the distinction this pair exists to keep.
func TestHistoryRowJSON_CarriesDeinterlaceFields(t *testing.T) {
	yes, no := true, false
	deinterlaced := store.Job{
		Path:        "/mnt/tv/broadcast.mkv",
		Fingerprint: "1:1",
		Status:      store.Done,
		Outcome: store.Outcome{
			Encoder:           "cpu",
			VmafPixFmt:        "yuv420p10le",
			Deinterlaced:      &yes,
			DeinterlaceFilter: "yadif=mode=send_frame:parity=auto:deint=all",
		},
	}
	plain := store.Job{
		Path:        "/mnt/tv/progressive.mkv",
		Fingerprint: "2:2",
		Status:      store.Done,
		Outcome:     store.Outcome{Encoder: "cpu", Deinterlaced: &no},
	}
	unmeasured := store.Job{
		Path:        "/mnt/tv/old.mkv",
		Fingerprint: "3:3",
		Status:      store.Done,
		Outcome:     store.Outcome{Encoder: "cpu"},
	}

	got := rawRow(t, deinterlaced)
	assertJSONBool(t, got, "deinterlaced", true)
	assertJSONString(t, got, "deinterlace_filter", "yadif=mode=send_frame:parity=auto:deint=all")

	// A job this build ran and did not deinterlace says so OUTRIGHT. It is the statement
	// that makes the null below mean something.
	got = rawRow(t, plain)
	assertJSONBool(t, got, "deinterlaced", false)
	assertJSONNull(t, got, "deinterlace_filter")

	got = rawRow(t, unmeasured)
	assertJSONNull(t, got, "deinterlaced")
	assertJSONNull(t, got, "deinterlace_filter")

	// The API and the export ship the SAME BYTES for the same row, because they are the
	// same function. Asserted rather than assumed: the projection IS the HTTP surface, and
	// an export that grew its own copy is how the two come to disagree.
	h := newHarness(t, "")
	mustClaim(t, h.st, deinterlaced.Path, deinterlaced.Fingerprint)
	if err := h.st.Finish(context.Background(), deinterlaced.Path, deinterlaced.Fingerprint,
		store.Done, &deinterlaced.Outcome, 3); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	ts := httptest.NewServer(h.srv)
	defer ts.Close()

	var body struct {
		History []map[string]json.RawMessage `json:"history"`
	}
	if err := json.Unmarshal([]byte(getRaw(t, ts.URL+"/api/history")), &body); err != nil {
		t.Fatalf("unmarshal /api/history: %v", err)
	}
	served := rowFor(t, body.History, deinterlaced.Path)
	exported := rawRow(t, jobFromStore(t, h, deinterlaced.Path))
	for _, key := range []string{"deinterlaced", "deinterlace_filter"} {
		if _, ok := served[key]; !ok {
			t.Fatalf("/api/history does not publish %q on a terminal row", key)
		}
		if string(served[key]) != string(exported[key]) {
			t.Errorf("the API and the export disagree about %q: %s vs %s", key, served[key], exported[key])
		}
	}
	assertJSONBool(t, served, "deinterlaced", true)
	assertJSONString(t, served, "deinterlace_filter", "yadif=mode=send_frame:parity=auto:deint=all")
}

// assertJSONBool reads one key as a JSON boolean, failing when it is absent, null or of
// another type - each of which is a different defect and none of which is the value.
func assertJSONBool(t *testing.T, m map[string]json.RawMessage, key string, want bool) {
	t.Helper()
	raw, ok := m[key]
	if !ok {
		t.Fatalf("the published row carries no %q key at all", key)
	}
	var got bool
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%s = %s, which is not a boolean: %v", key, raw, err)
	}
	if got != want {
		t.Errorf("%s = %v, want %v", key, got, want)
	}
}
