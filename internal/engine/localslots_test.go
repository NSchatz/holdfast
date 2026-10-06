package engine

// The engine-wide local job bound and the wait a submitted or watched file makes on Paused.
// `workers: 1` once let `serve` run two encodes at once - one from the scan, one from a
// targeted submission or the watch, each pool `workers` wide with nothing capping the total -
// at a host load above max_load, which only the scan feed consulted. Every case forces the
// overlap through a blocking encoder rather than leaving it to timing; the wall clocks below
// are backstops and the window a regressing build is given to start its second encode.

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
)

// slotWindow is how long a case waits for an encode that must NOT start. A build without the
// bound starts it within a probe of a two-second fixture, far inside this.
const slotWindow = 2 * time.Second

// errSlotFixtureEncode ends every held encode: what these cases are about is when an encode
// STARTS, so none of them pays for a real one.
var errSlotFixtureEncode = errors.New("simulated: the encoder failed")

// heldEncoder records every encode that starts, and the most in flight at once, and holds
// each one until the test releases it.
type heldEncoder struct {
	count   inflight
	started chan string
	release chan struct{}
}

func newHeldEncoder() *heldEncoder {
	return &heldEncoder{started: make(chan string, 8), release: make(chan struct{})}
}

func (h *heldEncoder) Encode(ctx context.Context, in, out string, props *probe.VideoProps) error {
	h.count.enter(in)
	defer h.count.leave()
	h.started <- in
	select {
	case <-h.release:
	case <-time.After(s0163Guard):
	}
	return errSlotFixtureEncode
}

// awaitStart is the next encode to start, failing the case if none does within the guard.
func (h *heldEncoder) awaitStart(t *testing.T, what string) string {
	t.Helper()
	select {
	case in := <-h.started:
		return in
	case <-time.After(s0163Guard):
		t.Fatalf("%s never reached its encode", what)
		return ""
	}
}

// refuteStart fails the case if an encode starts within slotWindow.
func (h *heldEncoder) refuteStart(t *testing.T, why string) {
	t.Helper()
	select {
	case in := <-h.started:
		t.Fatalf("%s started its encode %s", in, why)
	case <-time.After(slotWindow):
	}
}

// waitTaken waits until the submission queue's pool has taken every path off its channel, so
// the case knows the path is past the queue and inside the pool, not merely not yet drained.
func waitTaken(t *testing.T, subs *Submissions) {
	t.Helper()
	deadline := time.Now().Add(s0163Guard)
	for subs.Pending() > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the submission pool never took the offered path")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// offerOne judges and offers one path, failing on a refusal.
func offerOne(t *testing.T, subs *Submissions, p string) {
	t.Helper()
	resolved, rule, detail, ok := subs.Judge(p)
	if !ok {
		t.Fatalf("Judge(%s) refused an eligible path: %s - %s", p, rule, detail)
	}
	if !subs.Offer(resolved) {
		t.Fatalf("Offer(%s): the queue would not take it", resolved)
	}
}

// TestWorkersIsOneBoundAcrossEveryPool: with `workers: 1`, a file a second pool offers while
// the first pool's file is in its encode does not start until that job has ended - whichever
// two of the scan, the submission queue and the watch the files come through.
func TestWorkersIsOneBoundAcrossEveryPool(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, tc := range []struct{ name, first, second string }{
		{"scan then submission", "scan", "submission"},
		{"submission then watch", "submission", "watch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			a, b := filepath.Join(root, "a.mp4"), filepath.Join(root, "b.mp4")
			s0163Copies(t, ffmpeg, a)
			enc := newHeldEncoder()
			eng, _ := buildEngineAndStore(t, ffmpeg, ffprobe, root, enc, func(c *config.Config) { c.Workers = 1 })
			subs := eng.NewSubmissions(0, 0)
			watch := eng.NewWatches()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var running atomic.Int32
			goRun := func(f func()) {
				running.Add(1)
				go func() { defer running.Add(-1); f() }()
			}
			subsDone := make(chan struct{})
			go func() { defer close(subsDone); subs.Run(ctx) }()
			offer := func(route, p string) {
				switch route {
				case "scan":
					goRun(func() {
						if err := eng.RunOneshot(ctx); err != nil {
							t.Errorf("RunOneshot: %v", err)
						}
					})
				case "submission":
					offerOne(t, subs, p)
					waitTaken(t, subs)
				case "watch":
					goRun(func() { watch.process(ctx, "watch-w0", p) })
				}
			}

			offer(tc.first, a)
			if got := enc.awaitStart(t, "the first file"); got != a {
				t.Fatalf("the first encode was of %s, want %s", got, a)
			}
			// b is written only now, so the scan, which has listed the root already, offers
			// a alone, and b reaches the engine through the second route only.
			s0163Copies(t, ffmpeg, b)
			offer(tc.second, b)
			enc.refuteStart(t, "while the first file's job held the only worker slot")

			close(enc.release)
			if got := enc.awaitStart(t, "the second file, once the first job ended"); got != b {
				t.Fatalf("the second encode was of %s, want %s", got, b)
			}
			deadline := time.Now().Add(s0163Guard)
			subsWant := 0
			if tc.first == "submission" || tc.second == "submission" {
				subsWant = 1
			}
			for running.Load() > 0 || len(subs.Results()) < subsWant {
				if time.Now().After(deadline) {
					t.Fatal("the two jobs never finished")
				}
				time.Sleep(2 * time.Millisecond)
			}
			cancel()
			<-subsDone
			if p := enc.count.peak(); p != 1 {
				t.Fatalf("%d encodes were in flight at once under workers: 1", p)
			}
		})
	}
}

// TestSubmissionWaitsWhilePaused: a file submitted while Paused says stop is neither claimed
// nor encoded until it turns false, and then it is; a cancellation while it waits records
// nothing at all.
func TestSubmissionWaitsWhilePaused(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	a, b := filepath.Join(root, "a.mp4"), filepath.Join(root, "b.mp4")
	s0163Copies(t, ffmpeg, a, b)
	enc := newHeldEncoder()
	close(enc.release)
	eng, ts := buildEngineAndStore(t, ffmpeg, ffprobe, root, enc, func(c *config.Config) { c.Workers = 1 })
	var paused atomic.Bool
	paused.Store(true)
	eng.Paused = paused.Load
	eng.pausePoll = 5 * time.Millisecond

	noRow := func(p, when string) {
		t.Helper()
		if _, _, exists, err := ts.Get(context.Background(), p, probe.Fingerprint(p)); err != nil || exists {
			t.Fatalf("%s: %s has a ledger row (exists %v, err %v), want none", when, p, exists, err)
		}
	}

	// Waits, then runs once resumed.
	subs := eng.NewSubmissions(0, 0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); subs.Run(ctx) }()
	offerOne(t, subs, a)
	waitTaken(t, subs)
	enc.refuteStart(t, "while paused")
	noRow(a, "while paused")
	if n := len(subs.Results()); n != 0 {
		t.Fatalf("the paused submission was reported processed (%d result(s))", n)
	}
	paused.Store(false)
	if got := enc.awaitStart(t, "the submission, once resumed"); got != a {
		t.Fatalf("the encode was of %s, want %s", got, a)
	}
	deadline := time.Now().Add(s0163Guard)
	for len(subs.Results()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the resumed submission never finished")
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-done

	// Cancelled while it waits: nothing claimed, nothing recorded, nothing reported.
	paused.Store(true)
	subs = eng.NewSubmissions(0, 0)
	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() { defer close(done); subs.Run(ctx) }()
	offerOne(t, subs, b)
	waitTaken(t, subs)
	cancel()
	<-done
	if n := enc.count.callsFor(b); n != 0 {
		t.Fatalf("%s was encoded %d time(s) though the queue was cancelled while paused", b, n)
	}
	noRow(b, "after a cancellation while paused")
	if n := len(subs.Results()); n != 0 {
		t.Fatalf("a submission cancelled while paused was reported (%d result(s))", n)
	}
}

// TestAScanFileWaitingForASlotDoesNotStartOnceStopped: a file the scan feed handed out while
// it could run, then waited for the only slot behind a submission's job, does not start if a
// stop (pause, run_window, max_load) landed while it waited. It is left for the next scan.
func TestAScanFileWaitingForASlotDoesNotStartOnceStopped(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	a, b := filepath.Join(root, "a.mp4"), filepath.Join(root, "b.mp4")
	s0163Copies(t, ffmpeg, a)
	enc := newHeldEncoder()
	eng, _ := buildEngineAndStore(t, ffmpeg, ffprobe, root, enc, func(c *config.Config) { c.Workers = 1 })
	var paused atomic.Bool
	var asked atomic.Int32
	eng.Paused = func() bool { asked.Add(1); return paused.Load() }
	subs := eng.NewSubmissions(0, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subsDone := make(chan struct{})
	go func() { defer close(subsDone); subs.Run(ctx) }()
	offerOne(t, subs, a)
	if got := enc.awaitStart(t, "the submitted file"); got != a {
		t.Fatalf("the first encode was of %s, want %s", got, a)
	}

	s0163Copies(t, ffmpeg, b)
	before := asked.Load()
	scanDone := make(chan error, 1)
	go func() { scanDone <- eng.RunOneshot(ctx) }()
	// The feed asks Paused before it hands b out; past that, b's worker is at the slot.
	deadline := time.Now().Add(s0163Guard)
	for asked.Load() == before {
		if time.Now().After(deadline) {
			t.Fatal("the scan feed never asked whether it may hand a file out")
		}
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	paused.Store(true)
	close(enc.release)

	enc.refuteStart(t, "after a stop landed while it waited for the slot")
	select {
	case err := <-scanDone:
		if err != nil {
			t.Fatalf("RunOneshot: %v", err)
		}
	case <-time.After(s0163Guard):
		t.Fatal("the scan never ended")
	}
	cancel()
	<-subsDone
	if !exists(b) {
		t.Fatalf("the held-back file %s is gone", b)
	}
}
