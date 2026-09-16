package server

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/store"
)

// What the read surface says once a DRY PASS is over.
//
// The queue view is the page an operator reads to answer "is anything being worked on". A
// file the pass has finished deciding appearing there says a worker is still examining a file
// nothing is examining - for the whole duration of a `serve` run with dry_run on, over every
// eligible file in the library.

// TestDryPass_TheQueueViewCarriesNoRowForAFileThePassDecided grades [AC-9], the HTTP half:
// once the pass is over, /api/queue carries no row for any file it decided.
//
// Both verdicts a dry pass can write are seeded, through Claim and Finish exactly as the
// engine writes them, so each row really has been through `probing` on its way to its
// terminal state. The harness's own in-flight row is left in place and asserted PRESENT: a
// queue view that had simply stopped serving rows would otherwise pass this.
func TestDryPass_TheQueueViewCarriesNoRowForAFileThePassDecided(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()

	candidate := "/lib/candidate.mkv"
	seedCandidate(t, h.st, candidate, "9:9", &store.Outcome{
		SourceCodec: "h264", SourceBytes: i64p(4_000_000), TargetPath: "/lib/candidate.mkv",
	})
	guarded := "/lib/already.mkv"
	mustClaim(t, h.st, guarded, "7:7")
	if err := h.st.Finish(ctx, guarded, "7:7", store.Skipped,
		&store.Outcome{Reason: engine.SkipAlreadyTargetCodec}, 3); err != nil {
		t.Fatalf("Finish(skipped): %v", err)
	}

	ts := httptest.NewServer(h.srv)
	defer ts.Close()

	var got struct {
		Queue []jobDTO `json:"queue"`
	}
	getJSON(t, ts.URL+"/api/queue", &got)

	for _, decided := range []string{candidate, guarded} {
		if hasPath(got.Queue, decided) {
			t.Errorf("/api/queue carries %s, a file the pass has finished deciding. The queue view is "+
				"what says work is in hand, and a decided file there is a worker examining a file "+
				"nothing is examining: %+v", decided, got.Queue)
		}
	}
	// Anti-vacuity: the view does still serve a file genuinely in flight.
	if !hasPath(got.Queue, "/lib/active.mkv") {
		t.Fatalf("/api/queue carries no row for the genuinely in-flight file either, so the assertions "+
			"above hold whatever the projection does: %+v", got.Queue)
	}
}
