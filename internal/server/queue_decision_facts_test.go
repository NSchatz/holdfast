package server

// S0171 on the wire: an encoding or verifying row is served with the decision facts its
// ledger row records, and those facts are never counted into a figure about finished work.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/NSchatz/holdfast/internal/store"
)

// decisionKeys are the six decision facts as the wire names them.
var decisionKeys = []string{"source_bytes", "source_codec", "source_width", "source_height", "library_root", "profile_digest"}

// emptyStore opens a store with no rows in it.
func emptyLedger(t *testing.T) *store.SQLite {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// admit claims path and admits it to the encoder with d, then moves it on to status.
func admitRow(t *testing.T, st *store.SQLite, path, fp string, d store.DecisionFacts, status store.Status) {
	t.Helper()
	ctx := context.Background()
	mustClaim(t, st, path, fp)
	if err := st.AdmitToEncoder(ctx, path, fp, d); err != nil {
		t.Fatalf("AdmitToEncoder(%s): %v", path, err)
	}
	if status != store.Encoding {
		if err := st.Advance(ctx, path, fp, status); err != nil {
			t.Fatalf("Advance(%s, %s): %v", path, status, err)
		}
	}
}

// TestS0171_AC5_TheQueueServesExactlyTheRecordedDecisionFacts grades [AC-5]: /api/queue
// and the SSE snapshot serve an encoding or verifying row with exactly the decision facts
// its ledger row records, and serve them identically once no file exists at the row's path.
//
// The encoding row's path is a REAL file to begin with, so "identically when no file
// exists" is a comparison of two responses about one row rather than an assumption about a
// path that never existed.
func TestS0171_AC5_TheQueueServesExactlyTheRecordedDecisionFacts(t *testing.T) {
	st := emptyLedger(t)
	onDisk := filepath.Join(t.TempDir(), "running.mkv")
	if err := os.WriteFile(onDisk, []byte("a source that is on disk while its job encodes"), 0o600); err != nil {
		t.Fatal(err)
	}
	admitRow(t, st, onDisk, "1:1", store.DecisionFacts{
		Source:      store.SourceFacts{Codec: "h264", Width: factPx(1920), Height: factPx(1080)},
		SourceBytes: i64p(4_000_000),
		Decision:    store.Decision{LibraryRoot: "/lib/tv", ProfileDigest: "0123456789abcdef"},
	}, store.Encoding)
	admitRow(t, st, "/gone/verifying.mkv", "2:2", store.DecisionFacts{
		Source:      store.SourceFacts{Codec: "mpeg2video"},
		SourceBytes: i64p(7),
		Decision:    store.Decision{LibraryRoot: "/lib/films", ProfileDigest: "fedcba9876543210"},
	}, store.Verifying)
	// A probing row has not decided, so it carries none of them.
	mustClaim(t, st, "/gone/probing.mkv", "3:3")

	h := newHarnessOn(t, st, "", "")
	ts := httptest.NewServer(h.srv)
	defer ts.Close()

	want := map[string]map[string]string{
		onDisk: {
			"status": `"encoding"`, "source_bytes": "4000000", "source_codec": `"h264"`,
			"source_width": "1920", "source_height": "1080",
			"library_root": `"/lib/tv"`, "profile_digest": `"0123456789abcdef"`,
		},
		"/gone/verifying.mkv": {
			"status": `"verifying"`, "source_bytes": "7", "source_codec": `"mpeg2video"`,
			"source_width": "null", "source_height": "null",
			"library_root": `"/lib/films"`, "profile_digest": `"fedcba9876543210"`,
		},
		"/gone/probing.mkv": {
			"status": `"probing"`, "source_bytes": "null", "source_codec": "null",
			"source_width": "null", "source_height": "null",
			"library_root": "null", "profile_digest": "null",
		},
	}
	check := func(where string, rows map[string]map[string]json.RawMessage) {
		t.Helper()
		for path, w := range want {
			row, ok := rows[path]
			if !ok {
				t.Errorf("%s: %s is not in the queue", where, path)
				continue
			}
			wantRawKeys(t, where+" "+path, row, w)
		}
	}

	withFile := rawRowsByPath(t, getRaw(t, ts.URL+"/api/queue"), "queue")
	check("/api/queue", withFile)
	check("the SSE snapshot", rawRowsByPath(t, snapshotRaw(t, h.hub), "queue"))

	// The file goes; the ledger does not change; the row is served as it was.
	if err := os.Remove(onDisk); err != nil {
		t.Fatal(err)
	}
	withoutFile := rawRowsByPath(t, getRaw(t, ts.URL+"/api/queue"), "queue")
	check("/api/queue with no file at the path", withoutFile)
	check("the SSE snapshot with no file at the path", rawRowsByPath(t, snapshotRaw(t, h.hub), "queue"))
	for _, key := range append([]string{"status", "updated_at"}, decisionKeys...) {
		if !bytes.Equal(withFile[onDisk][key], withoutFile[onDisk][key]) {
			t.Errorf("%s was served as %s with the file there and %s without it", key,
				withFile[onDisk][key], withoutFile[onDisk][key])
		}
	}
}

// TestS0171_AC8_ActiveRowsCarryingASizeMoveNoFigureAboutFinishedWork grades [AC-8]: the
// lifetime reclaimed total, the size-ratio aggregate and the per-status counts are identical
// between a ledger whose active rows carry a source size and one whose active rows carry
// none. Only done rows ever contribute a size.
//
// The two ledgers hold the same done rows and the same active rows in the same states; the
// only difference between them is whether the active rows were admitted with facts.
func TestS0171_AC8_ActiveRowsCarryingASizeMoveNoFigureAboutFinishedWork(t *testing.T) {
	seed := func(t *testing.T, facts bool) *harness {
		t.Helper()
		st := emptyLedger(t)
		ctx := context.Background()
		for i, sizes := range [][2]int64{{1000, 400}, {2000, 500}} {
			path := filepath.Join("/lib/done", string(rune('a'+i))+".mkv")
			mustClaim(t, st, path, "d:d")
			if err := st.Finish(ctx, path, "d:d", store.Done, &store.Outcome{
				Encoder: "cpu", SourceBytes: i64p(sizes[0]), OutputBytes: i64p(sizes[1]),
				Decision: store.Decision{LibraryRoot: "/lib", ProfileDigest: "digest"},
			}, 3); err != nil {
				t.Fatal(err)
			}
		}
		d := store.DecisionFacts{}
		if facts {
			// Sizes large enough that any figure they leaked into would move visibly.
			d = store.DecisionFacts{
				Source:      store.SourceFacts{Codec: "h264", Width: factPx(1920), Height: factPx(1080)},
				SourceBytes: i64p(9_000_000_000),
				Decision:    store.Decision{LibraryRoot: "/lib", ProfileDigest: "digest"},
			}
		}
		admitRow(t, st, "/lib/active/encoding.mkv", "e:e", d, store.Encoding)
		admitRow(t, st, "/lib/active/verifying.mkv", "v:v", d, store.Verifying)
		return newHarnessOn(t, st, "", "")
	}
	figures := func(t *testing.T, h *harness) (summary, snap map[string]json.RawMessage) {
		t.Helper()
		ts := httptest.NewServer(h.srv)
		defer ts.Close()
		if err := json.Unmarshal([]byte(getRaw(t, ts.URL+"/api/summary")), &summary); err != nil {
			t.Fatalf("unmarshal /api/summary: %v", err)
		}
		if err := json.Unmarshal([]byte(snapshotRaw(t, h.hub)), &snap); err != nil {
			t.Fatalf("unmarshal the snapshot: %v", err)
		}
		return summary, snap
	}
	sizeRatio := func(t *testing.T, doc map[string]json.RawMessage) string {
		t.Helper()
		var agg map[string]json.RawMessage
		if err := json.Unmarshal(doc["aggregates"], &agg); err != nil {
			t.Fatalf("unmarshal aggregates: %v", err)
		}
		if len(agg["size_ratio"]) == 0 {
			t.Fatal("the response carries no size_ratio aggregate")
		}
		return string(agg["size_ratio"])
	}

	withSummary, withSnap := figures(t, seed(t, true))
	bareSummary, bareSnap := figures(t, seed(t, false))

	// Anti-vacuity: the active rows of the first ledger really do carry the size.
	if rows := rawRowsByPath(t, rawDocument(t, withSnap), "queue"); string(rows["/lib/active/encoding.mkv"]["source_bytes"]) != "9000000000" ||
		string(rows["/lib/active/verifying.mkv"]["source_bytes"]) != "9000000000" {
		t.Fatalf("precondition: the active rows do not carry their size: %v", rows)
	}
	if rows := rawRowsByPath(t, rawDocument(t, bareSnap), "queue"); string(rows["/lib/active/encoding.mkv"]["source_bytes"]) != "null" {
		t.Fatalf("precondition: the control's active row carries a size: %v", rows)
	}

	for _, doc := range []struct {
		where      string
		with, bare map[string]json.RawMessage
	}{
		{"/api/summary", withSummary, bareSummary},
		{"the SSE snapshot", withSnap, bareSnap},
	} {
		if got := string(doc.with["bytes_reclaimed_lifetime"]); got != "2100" {
			t.Errorf("%s: bytes_reclaimed_lifetime = %s, want 2100 (the two done rows' 600 + 1500)", doc.where, got)
		}
		for _, key := range []string{"bytes_reclaimed_lifetime", "summary"} {
			if !bytes.Equal(doc.with[key], doc.bare[key]) {
				t.Errorf("%s: %s is %s where active rows carry a size and %s where they carry none",
					doc.where, key, doc.with[key], doc.bare[key])
			}
		}
		with, bare := sizeRatio(t, doc.with), sizeRatio(t, doc.bare)
		if with != bare {
			t.Errorf("%s: the size_ratio aggregate is %s where active rows carry a size and %s where they carry none",
				doc.where, with, bare)
		}
	}

	var counts map[string]int
	if err := json.Unmarshal(withSummary["summary"], &counts); err != nil {
		t.Fatal(err)
	}
	if counts["done"] != 2 || counts["encoding"] != 1 || counts["verifying"] != 1 {
		t.Errorf("per-status counts = %v, want 2 done, 1 encoding, 1 verifying", counts)
	}
}

// mustJSON renders a decoded document back to the bytes rawRows reads.
func rawDocument(t *testing.T, doc map[string]json.RawMessage) string {
	t.Helper()
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
