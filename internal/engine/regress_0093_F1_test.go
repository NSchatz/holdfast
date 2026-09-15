package engine

// Regression probe for the targeted-submission gate: the record-based hold-backs a
// submission reads are a snapshot taken ONCE, when the queue's pool starts, and never
// refreshed. A scan takes a fresh one on every pass (RunOneshot stores loadHoldBacks), so a
// hold-back recorded after the daemon started reaches the scan route and not the submission
// route - and a deployment running with the periodic scan off, which is the deployment this
// endpoint exists to enable, never takes another pass at all.
//
// The fixture is the one the suite already uses for the hold-back criterion (a parked job
// naming a source and a replacement). The ONLY thing changed is the ORDER: the incident is
// recorded after the queue has started, which is when a swap actually parks one.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

func TestRegress0093F1SubmissionMissesAHoldBackParkedAfterTheQueueStarted(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	parked := filepath.Join(root, "parked.mkv")
	replacement := filepath.Join(root, "replacement.mkv")
	mkH264(t, ffmpeg, parked, "8M")
	mkH264(t, ffmpeg, replacement, "8M")

	ts := newTestStore(t, root)
	cfg := baseCfg(root)
	prober := probe.New(ffmpeg, ffprobe)
	enc := gatedFFmpeg(ffmpeg, cfg, prober)
	eng := New(cfg, prober, enc, ts, discardLogger())

	// The daemon's order: the queue's pool is started first (cmd/holdfast runs
	// subs.Run(ctx) at startup), so its snapshot is taken before anything is parked.
	subs := eng.NewSubmissions(1, 8)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); subs.Run(ctx) }()
	deadline := time.Now().Add(30 * time.Second)
	for eng.held.Load() == nil {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("the queue never published a hold-back snapshot")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// NOW the swap parks. Both recorded paths are hold-backs from this instant.
	if err := ts.RecordSwapIncident(context.Background(), store.SwapIncident{
		SourcePath:        parked,
		SourceFingerprint: probe.Fingerprint(parked),
		ReplacementPath:   replacement,
		Outcome:           store.Indeterminate,
		SwapError:         "simulated: the swap outcome could not be established",
	}); err != nil {
		t.Fatalf("RecordSwapIncident: %v", err)
	}

	// The control: a snapshot taken NOW - which is what every scan pass takes - holds the
	// replacement path back. Without this the case could pass for want of a hold-back.
	if _, ok := eng.loadHoldBacks(context.Background()).held(replacement); !ok {
		t.Fatal("a freshly loaded snapshot does not hold the replacement back, so this fixture " +
			"establishes nothing about the stale one")
	}

	if !subs.Offer(replacement) {
		t.Fatal("the queue would not take the path")
	}
	for len(subs.Results()) < 1 {
		if time.Now().After(deadline.Add(5 * time.Minute)) {
			cancel()
			<-done
			t.Fatal("the submission was never processed")
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-done

	r := subs.Results()[0]
	if r.Claimed {
		t.Errorf("a submission got a held-back path (%s) past the pipeline's door; a scan running at "+
			"the same moment holds it back, so the two routes do not agree", replacement)
	}
	if n := enc.n.Load(); n != 0 {
		t.Errorf("the encoder ran %d time(s) on a held-back replacement; those bytes are what a parked "+
			"incident is keeping for an operator", n)
	}
	if row, ok := terminalRowFor(t, ts, replacement); ok {
		t.Errorf("a held-back path carries a terminal row (%s/%q); a scan records nothing for it",
			row.Status, row.Outcome.Reason)
	}
}
