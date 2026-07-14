package engine

// TRANSCODE-7: proof that the engine's Observer emits the job-state transitions the
// API/SSE hub relies on, and that the Paused hook only ever DELAYS work (never
// touches a source). Real-ffmpeg, fail-loud like the rest of the safety suite.

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/store"
)

// collector is a concurrency-safe Observer that records every Event (workers emit
// from multiple goroutines under the pool).
type collector struct {
	mu     sync.Mutex
	events []Event
}

func (c *collector) observe(ev Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
}

func (c *collector) snapshot() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Event, len(c.events))
	copy(out, c.events)
	return out
}

// hasStatus reports whether any collected event reached status s.
func hasStatus(evs []Event, s store.Status) bool {
	for _, e := range evs {
		if e.Status == s {
			return true
		}
	}
	return false
}

func TestObserver_EmitsTransitionsAndReclaimedBytesOnSwap(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	src := filepath.Join(root, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M") // a fat H.264 source: real libx265 shrinks it well below 8 Mbit

	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) {
		c.MinBitrateKbps = 0
	})
	col := &collector{}
	eng.Observer = col.observe
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	evs := col.snapshot()
	for _, want := range []store.Status{store.Probing, store.Encoding, store.Verifying, store.Done} {
		if !hasStatus(evs, want) {
			t.Fatalf("observer never saw a %q event; got %+v", want, evs)
		}
	}

	// Exactly one event must carry the reclaimed-bytes payload (the post-swap Done),
	// and it must be positive — the whole point of the transcode. The generic Done
	// emit from the finish wrapper carries 0, so summing is double-count-free.
	var byteEvents, doneEvents int
	var totalReclaimed int64
	for _, e := range evs {
		if e.Status == store.Done {
			doneEvents++
		}
		if e.BytesReclaimed != 0 {
			byteEvents++
			totalReclaimed += e.BytesReclaimed
			if e.Status != store.Done {
				t.Fatalf("BytesReclaimed set on a non-Done event: %+v", e)
			}
			if e.Worker == "" {
				t.Fatalf("Done byte event missing worker: %+v", e)
			}
			// TRANSCODE-8: the rich Done event carries the encode duration for the
			// metrics histogram. A real libx265 encode always takes measurable time.
			if e.EncodeDuration <= 0 {
				t.Fatalf("Done event missing a positive EncodeDuration: %+v", e)
			}
		}
	}
	// Done must be emitted EXACTLY once (the rich event) — the store-only finish on
	// the swap path does not emit, so a metrics consumer won't double-count done.
	if doneEvents != 1 {
		t.Fatalf("want exactly 1 Done event, got %d (%+v)", doneEvents, evs)
	}
	if byteEvents != 1 {
		t.Fatalf("want exactly 1 byte-carrying event, got %d (%+v)", byteEvents, evs)
	}
	if totalReclaimed <= 0 {
		t.Fatalf("reclaimed bytes must be positive, got %d", totalReclaimed)
	}
}

func TestObserver_DoneEventCarriesVmafScoreWhenEnabled(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	src := filepath.Join(root, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")

	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) {
		c.MinBitrateKbps = 0
		c.VmafEnable = boolPtr(true) // turn the VMAF gate on so a score is measured
		c.MinVmaf = 80               // a real re-encode of a testsrc clip scores well above this
	})
	col := &collector{}
	eng.Observer = col.observe
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	var found bool
	for _, e := range col.snapshot() {
		if e.Status == store.Done && e.BytesReclaimed != 0 {
			found = true
			if e.VmafScore <= 0 || e.VmafScore > 100 {
				t.Fatalf("Done event VmafScore out of (0,100]: %v", e.VmafScore)
			}
		}
	}
	if !found {
		t.Fatalf("no rich Done event observed; events: %+v", col.snapshot())
	}
}

func TestObserver_EmitsSkip(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	src := filepath.Join(root, "already.mkv")
	mkHevc(t, ffmpeg, src, "1M") // already the target codec → Skipped, no encode

	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) {
		c.MinBitrateKbps = 0
	})
	col := &collector{}
	eng.Observer = col.observe
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	evs := col.snapshot()
	if !hasStatus(evs, store.Skipped) {
		t.Fatalf("a same-codec source must emit a Skipped event; got %+v", evs)
	}
	if hasStatus(evs, store.Encoding) {
		t.Fatalf("a skipped source must never reach Encoding; got %+v", evs)
	}
}

func TestPaused_LeavesSourcesUntouchedAndUnclaimed(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	src := filepath.Join(root, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	before := md5f(t, src)

	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) {
		c.MinBitrateKbps = 0
	})
	eng.Paused = func() bool { return true } // paused from the outset

	col := &collector{}
	eng.Observer = col.observe
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	// Paused ⇒ no file was fed to a worker ⇒ no claim, no encode, no event, and the
	// source is byte-for-byte intact. Pause DELAYS; it never touches a file.
	if evs := col.snapshot(); len(evs) != 0 {
		t.Fatalf("paused scan emitted events (did work): %+v", evs)
	}
	rows, err := eng.Store.List(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("paused scan created store rows (claimed work): %+v", rows)
	}
	if after := md5f(t, src); after != before {
		t.Fatalf("paused scan mutated the source: %s != %s", before, after)
	}
	if !exists(src) {
		t.Fatalf("paused scan deleted the source")
	}
}
