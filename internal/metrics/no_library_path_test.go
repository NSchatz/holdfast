package metrics

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/store"
)

// TestExposition_NamesNoLibraryPath is the PREMISE the /metrics decision rests on.
//
// /metrics is deliberately not gated by server_read_token or by anything else: its
// reachability is governed by metrics_enable alone, because a scrape credential is the
// one thing a Prometheus deployment most often cannot supply and gating it would break
// every existing scrape on upgrade. That is only safe while the exposition carries
// AGGREGATES and names no file - counters by outcome, a byte total, two histograms, and a
// queue-depth gauge labelled by state. The moment a metric gains a path label, or a
// filename reaches a Help string, an ungated /metrics becomes the very disclosure
// server_read_token exists to close, on a surface nobody would think to re-check.
//
// So the premise is asserted here, where a later metric breaks it, rather than argued in
// a comment beside the route. The ledger is seeded with paths whose every component is
// distinctive enough that a leak cannot be mistaken for ordinary exposition text - a bare
// "holiday.mkv" could plausibly appear in a Help example; "s0101-library-leak-canary"
// cannot.
func TestExposition_NamesNoLibraryPath(t *testing.T) {
	const dir = "/mnt/s0101-library-leak-canary"
	paths := []string{
		dir + "/season-one/episode-eleven.mkv",
		dir + "/a-film-nobody-should-see-named.mkv",
		dir + "/failed-encode.mkv",
	}

	st := openStore(t)
	m := New(st, nil)
	ctx := context.Background()

	// One row per terminal outcome that carries a figure, so the seeding exercises the
	// counter, the byte total and both histograms rather than a single code path.
	claim := func(p string) {
		t.Helper()
		ok, err := st.Claim(ctx, p, "fp", "w0", 3, store.DecisionInputs{})
		if err != nil || !ok {
			t.Fatalf("Claim(%s): ok=%v err=%v", p, ok, err)
		}
	}
	claim(paths[0])
	if err := st.Finish(ctx, paths[0], "fp", store.Done, &store.Outcome{Reason: "done"}, 3); err != nil {
		t.Fatalf("Finish(done): %v", err)
	}
	claim(paths[1])
	if err := st.Finish(ctx, paths[1], "fp", store.Skipped, &store.Outcome{Reason: engine.SkipLowBitrate}, 3); err != nil {
		t.Fatalf("Finish(skipped): %v", err)
	}
	claim(paths[2])
	if err := st.Finish(ctx, paths[2], "fp", store.Failed, &store.Outcome{Reason: "ffmpeg exited 1"}, 3); err != nil {
		t.Fatalf("Finish(failed): %v", err)
	}

	// The observer path too: these are the events the daemon feeds the collectors, and
	// they carry an Outcome, which is where a path would most plausibly arrive.
	m.Observe(doneEvent(4096, 2*time.Second, 96.5))
	m.Observe(engine.Event{Status: store.Skipped, Path: paths[1]})
	m.Observe(engine.Event{Status: store.Failed, Path: paths[2]})
	m.Observe(engine.Event{Status: store.Encoding, Path: paths[0]})

	body := scrape(t, m)

	// Anti-vacuity: the scrape must actually carry this build's metrics, or "no path
	// appears" would be true of an empty string.
	for _, want := range []string{
		"holdfast_files_total", "holdfast_bytes_reclaimed_total",
		"holdfast_encode_duration_seconds", "holdfast_vmaf_score", "holdfast_queue_depth",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("the exposition does not carry %s, so this assertion proves nothing:\n%s", want, body)
		}
	}
	// And it must reflect the seeded ledger, so the queue-depth collector really did read
	// the rows whose paths must not appear.
	if !strings.Contains(body, `holdfast_queue_depth{state="done"} 1`) {
		t.Fatalf("the queue-depth gauge did not read the seeded rows, so nothing here saw a path at all:\n%s", body)
	}

	// The assertion itself: no seeded path, and no component of one, anywhere in the
	// exposition - not in a label, not in a Help string, not in a comment line.
	for _, p := range paths {
		if strings.Contains(body, p) {
			t.Errorf("the /metrics exposition names the library path %q. /metrics is served with NO "+
				"credential, so this is every path in the library published to anyone who can reach "+
				"the port. Either drop the label or gate the route - the two decisions travel "+
				"together.\n%s", p, body)
		}
	}
	for _, fragment := range []string{dir, "episode-eleven", "a-film-nobody-should-see-named", "failed-encode"} {
		if strings.Contains(body, fragment) {
			t.Errorf("the /metrics exposition carries the path fragment %q, which is a media path in "+
				"pieces and discloses the same thing:\n%s", fragment, body)
		}
	}
}
