package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/NSchatz/holdfast/internal/store"
)

// TestHistoryRowJSON_CarriesTheDecidingProfile.
//
// A library root carries its own encoder, crf, bitrate floor and VMAF floors, so a
// terminal row has to say WHICH profile judged the file - the configuration has several
// answers and only the row knows which one applied. Both facts are published, and both
// are published in ONE place: `holdfast export` writes rows by calling the same
// HistoryRowJSON the API serves them through, so the two cannot come to disagree about
// the row's shape.
//
// Asserted on the RAW BYTES rather than through a struct, for the reason the codec row
// established: a missing key and an explicit null both decode to the zero value, so a
// test that read a decoded field could not tell "not recorded" from "recorded as an
// empty string" - which is the whole distinction the outcome schema exists to keep.
func TestHistoryRowJSON_CarriesTheDecidingProfile(t *testing.T) {
	decided := store.Job{
		Path:        "/mnt/tv/ep.mkv",
		Fingerprint: "1:1",
		Status:      store.Done,
		Outcome: store.Outcome{
			Encoder:  "cpu",
			Decision: store.Decision{LibraryRoot: "/mnt/tv", ProfileDigest: "0123456789abcdef"},
		},
	}
	// A row no profile decided, or one an earlier build wrote: both fields must arrive as
	// an explicit null a client can tell from "" and from a fabricated root.
	unattributed := store.Job{
		Path:        "/mnt/tv/old.mkv",
		Fingerprint: "2:2",
		Status:      store.Skipped,
		Outcome:     store.Outcome{Reason: "restored-original"},
	}

	got := rawRow(t, decided)
	assertJSONString(t, got, "library_root", "/mnt/tv")
	assertJSONString(t, got, "profile_digest", "0123456789abcdef")

	got = rawRow(t, unattributed)
	assertJSONNull(t, got, "library_root")
	assertJSONNull(t, got, "profile_digest")

	// The API and the export ship the SAME BYTES for the same row, because they are the
	// same function. Asserted rather than assumed: an export that grew its own projection
	// is exactly how the two come to disagree, and nothing else in the build would notice.
	h := newHarness(t, "")
	mustClaim(t, h.st, decided.Path, decided.Fingerprint)
	if err := h.st.Finish(context.Background(), decided.Path, decided.Fingerprint, store.Done, &decided.Outcome, 3); err != nil {
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
	served := rowFor(t, body.History, decided.Path)
	exported := rawRow(t, jobFromStore(t, h, decided.Path))
	for _, key := range []string{"library_root", "profile_digest"} {
		if _, ok := served[key]; !ok {
			t.Fatalf("/api/history does not publish %q on a terminal row", key)
		}
		if string(served[key]) != string(exported[key]) {
			t.Errorf("the API and the export disagree about %q: %s vs %s", key, served[key], exported[key])
		}
	}
	assertJSONString(t, served, "library_root", "/mnt/tv")
	assertJSONString(t, served, "profile_digest", "0123456789abcdef")
}

// rowFor picks the published row for path out of a history response.
func rowFor(t *testing.T, rows []map[string]json.RawMessage, path string) map[string]json.RawMessage {
	t.Helper()
	for _, r := range rows {
		var p string
		if err := json.Unmarshal(r["path"], &p); err != nil {
			t.Fatalf("unmarshal row path: %v", err)
		}
		if p == path {
			return r
		}
	}
	t.Fatalf("no published row for %s", path)
	return nil
}

// jobFromStore reads one terminal row back out of the harness's store, so the export half
// of the comparison above walks the same rows `holdfast export` does rather than re-using
// the literal the API was seeded from.
func jobFromStore(t *testing.T, h *harness, path string) store.Job {
	t.Helper()
	var out store.Job
	found := false
	if err := h.st.EachTerminal(context.Background(), func(j store.Job) error {
		if j.Path == path {
			out, found = j, true
		}
		return nil
	}); err != nil {
		t.Fatalf("EachTerminal: %v", err)
	}
	if !found {
		t.Fatalf("the store holds no terminal row for %s", path)
	}
	return out
}

func rawRow(t *testing.T, j store.Job) map[string]json.RawMessage {
	t.Helper()
	line, err := HistoryRowJSON(j)
	if err != nil {
		t.Fatalf("HistoryRowJSON: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("unmarshal row: %v", err)
	}
	return m
}

func assertJSONString(t *testing.T, m map[string]json.RawMessage, key, want string) {
	t.Helper()
	raw, ok := m[key]
	if !ok {
		t.Fatalf("the published row carries no %q key at all", key)
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%s = %s, which is not a string: %v", key, raw, err)
	}
	if got != want {
		t.Errorf("%s = %q, want %q", key, got, want)
	}
}

func assertJSONNull(t *testing.T, m map[string]json.RawMessage, key string) {
	t.Helper()
	raw, ok := m[key]
	if !ok {
		t.Fatalf("%q is absent from the row rather than null - a client cannot tell a fact "+
			"nobody recorded from a field that has gone away", key)
	}
	if string(raw) != "null" {
		t.Errorf("%s = %s, want an explicit null (never \"\" and never a fabricated root)", key, raw)
	}
}
