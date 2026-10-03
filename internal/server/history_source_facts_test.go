package server

// S0167 on the wire: a skipped row's history entry says what its source was.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/NSchatz/holdfast/internal/store"
)

func factPx(v int) *int { return &v }

// rawRows reads one row array out of a response body, keyed by path, with every value left
// as the bytes that were served. Decoding into a struct would erase the very distinction
// these cases grade: a missing key and an explicit null both arrive as the zero value.
func rawRowsByPath(t *testing.T, body, key string) map[string]map[string]json.RawMessage {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("unmarshal the response: %v", err)
	}
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(doc[key], &list); err != nil {
		t.Fatalf("unmarshal %q: %v", key, err)
	}
	out := map[string]map[string]json.RawMessage{}
	for _, row := range list {
		var path string
		if err := json.Unmarshal(row["path"], &path); err != nil {
			t.Fatalf("unmarshal a row's path: %v", err)
		}
		out[path] = row
	}
	return out
}

// wantRaw requires each named key to be PRESENT on the row with exactly the given bytes.
func wantRawKeys(t *testing.T, where string, row map[string]json.RawMessage, want map[string]string) {
	t.Helper()
	for key, w := range want {
		got, present := row[key]
		if !present {
			t.Errorf("%s: the key %q is missing; a fact is served as a value or an explicit null, never left out", where, key)
			continue
		}
		if string(got) != w {
			t.Errorf("%s: %s = %s, want %s", where, key, got, w)
		}
	}
}

// TestS0167_AC8_HistoryServesASkippedRowsSourceFactsOrExplicitNulls grades [AC-8]: a
// skipped row is published with the source codec, width and height it recorded, and a
// skipped row that recorded none carries an explicit JSON null for each - never a missing
// key, "" or 0.
//
// Both writers of a skipped row are covered: the one a guard uses after the claim (Finish)
// and the one the pre-claim guards use (RecordSkip).
func TestS0167_AC8_HistoryServesASkippedRowsSourceFactsOrExplicitNulls(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()

	mustClaim(t, h.st, "/lib/claimed-facts.mkv", "3:3")
	if err := h.st.Finish(ctx, "/lib/claimed-facts.mkv", "3:3", store.Skipped, &store.Outcome{
		Reason: "already-at-target-codec", SourceCodec: "hevc", SourceWidth: factPx(1920), SourceHeight: factPx(1080),
	}, 3); err != nil {
		t.Fatal(err)
	}
	mustClaim(t, h.st, "/lib/claimed-none.mkv", "4:4")
	if err := h.st.Finish(ctx, "/lib/claimed-none.mkv", "4:4", store.Skipped,
		&store.Outcome{Reason: "symlink"}, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.RecordSkip(ctx, "/lib/preclaim-facts.mkv", "5:5", "hardlinked", store.Decision{}, "",
		store.SourceFacts{Codec: "h264", Width: factPx(352), Height: factPx(288)}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.RecordSkip(ctx, "/lib/preclaim-none.mkv", "6:6", "hardlinked", store.Decision{}, "",
		store.SourceFacts{}); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(h.srv)
	defer ts.Close()
	rows := rawRowsByPath(t, getRaw(t, ts.URL+"/api/history"), "history")

	cases := map[string]map[string]string{
		"/lib/claimed-facts.mkv":  {"status": `"skipped"`, "source_codec": `"hevc"`, "source_width": "1920", "source_height": "1080"},
		"/lib/preclaim-facts.mkv": {"status": `"skipped"`, "source_codec": `"h264"`, "source_width": "352", "source_height": "288"},
		"/lib/claimed-none.mkv":   {"status": `"skipped"`, "source_codec": "null", "source_width": "null", "source_height": "null"},
		"/lib/preclaim-none.mkv":  {"status": `"skipped"`, "source_codec": "null", "source_width": "null", "source_height": "null"},
	}
	for path, want := range cases {
		row, ok := rows[path]
		if !ok {
			t.Errorf("%s is not in /api/history", path)
			continue
		}
		wantRawKeys(t, path, row, want)
	}
}
