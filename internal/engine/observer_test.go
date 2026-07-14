package engine

// TRANSCODE-7: proof that the engine's Observer emits the job-state transitions the
// API/SSE hub relies on, and that the Paused hook only ever DELAYS work (never
// touches a source). Real-ffmpeg, fail-loud like the rest of the safety suite.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
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
		if n := e.BytesReclaimed(); n != 0 {
			byteEvents++
			totalReclaimed += n
			if e.Status != store.Done {
				t.Fatalf("BytesReclaimed set on a non-Done event: %+v", e)
			}
			if e.Worker == "" {
				t.Fatalf("Done byte event missing worker: %+v", e)
			}
			// TRANSCODE-8: the rich Done event carries the encode duration for the
			// metrics histogram — since TRANSCODE-13 on its Outcome, which is the same
			// value the ledger stores. A real libx265 encode always takes measurable time.
			if e.Outcome == nil || e.Outcome.EncodeMs == nil || *e.Outcome.EncodeMs <= 0 {
				t.Fatalf("Done event missing a positive EncodeMs: %+v", e.Outcome)
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

// TRANSCODE-13. The engine computes everything needed to PROVE a swap was safe and,
// before this phase, kept none of it: the VMAF mean survived only as a metric, and the
// worst-frame min, the model that produced them, both file sizes and the encode
// duration were all computed and thrown away.
//
// This drives a REAL libx265 encode with the REAL VMAF gate on and asserts the Done
// event carries the whole proof — and, crucially, that the SAME proof is what landed in
// the ledger. (The event and the store row are the same *store.Outcome value by
// construction; this pins that they stay so, because a fidelity number the dashboard
// shows but the database does not have is worth nothing after a restart.)
func TestObserver_DoneEventAndLedgerCarryTheFullProof(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	src := filepath.Join(root, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	srcSize := probe.FileSize(src)

	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) {
		c.MinBitrateKbps = 0
		c.VmafEnable = boolPtr(true) // turn the VMAF gate on so a score is measured
		c.MinVmaf = 80               // a real re-encode of a testsrc clip scores well above this
		c.VmafMinPool = 40           // and its worst frame clears this comfortably
	})
	col := &collector{}
	eng.Observer = col.observe
	// Capture the engine's own logs: the DONE line is the operator-facing report of what
	// a swap was worth, and the scores on it are *float64 now. slog formats an unknown
	// type with %v, and a pointer's %v is its ADDRESS — so handing it the pointer prints
	// `vmaf=0xc000012120` and silently guts the line. Asserted below.
	var logs bytes.Buffer
	eng.Log = slog.New(slog.NewTextHandler(&logs, nil))

	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	var done *Event
	for i, e := range col.snapshot() {
		if e.Status == store.Done {
			done = &col.snapshot()[i]
		}
	}
	if done == nil {
		t.Fatalf("no Done event observed; events: %+v", col.snapshot())
	}
	o := done.Outcome
	if o == nil {
		t.Fatal("Done event carries no Outcome — the proof was computed and thrown away")
	}

	// The VMAF pair AND the model that produced it. The min is the whole point: it is
	// the worst-frame floor TRANSCODE-11 shipped, and it was being discarded.
	if o.VmafMean == nil || *o.VmafMean <= 0 || *o.VmafMean > 100 {
		t.Errorf("VmafMean not recorded or out of (0,100]: %v", o.VmafMean)
	}
	if o.VmafMin == nil || *o.VmafMin <= 0 || *o.VmafMin > 100 {
		t.Errorf("VmafMin not recorded or out of (0,100]: %v", o.VmafMin)
	}
	if o.VmafMin != nil && o.VmafMean != nil && *o.VmafMin > *o.VmafMean {
		t.Errorf("worst frame (%v) cannot beat the mean (%v)", *o.VmafMin, *o.VmafMean)
	}
	// A score without its model is not an interpretable number — that is the honesty
	// constraint the whole fidelity track rests on, so the model must be there.
	if o.VmafModel == "" {
		t.Error("VmafModel not recorded — a VMAF score without its model cannot be honestly displayed")
	}

	// The encoder, both sizes, and the duration.
	if o.Encoder != "cpu" {
		t.Errorf("Encoder = %q, want %q", o.Encoder, "cpu")
	}
	if o.SourceBytes == nil || *o.SourceBytes != srcSize {
		t.Errorf("SourceBytes = %v, want the pre-encode size %d", o.SourceBytes, srcSize)
	}
	if o.OutputBytes == nil || *o.OutputBytes <= 0 || *o.OutputBytes >= srcSize {
		t.Errorf("OutputBytes = %v, want a positive size strictly smaller than %d", o.OutputBytes, srcSize)
	}
	if o.EncodeMs == nil || *o.EncodeMs <= 0 {
		t.Errorf("EncodeMs = %v, want positive", o.EncodeMs)
	}
	if o.Reason != "" {
		t.Errorf("Reason = %q, want empty on a Done (a success needs no excuse)", o.Reason)
	}
	if got, want := done.BytesReclaimed(), srcSize-*o.OutputBytes; got != want {
		t.Errorf("BytesReclaimed() = %d, want %d (derived from the recorded sizes)", got, want)
	}

	// The DONE log line reports the real numbers, not pointer addresses.
	line := logs.String()
	if strings.Contains(line, "vmaf=0x") || strings.Contains(line, "vmaf_min=0x") {
		t.Errorf("the DONE log line is printing POINTER ADDRESSES instead of VMAF scores:\n%s", line)
	}
	if !strings.Contains(line, fmt.Sprintf("vmaf=%v", *o.VmafMean)) {
		t.Errorf("the DONE log line does not carry the measured VMAF mean %v:\n%s", *o.VmafMean, line)
	}

	// And the ledger kept it — not just the live event. This is the actual bug: the
	// dashboard could not show fidelity because the store had none of this.
	rows, err := eng.Store.List(context.Background(), []store.Status{store.Done}, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 done row, got %d (%+v)", len(rows), rows)
	}
	got := rows[0].Outcome
	if got.VmafMean == nil || *got.VmafMean != *o.VmafMean ||
		got.VmafMin == nil || *got.VmafMin != *o.VmafMin ||
		got.VmafModel != o.VmafModel || got.Encoder != o.Encoder ||
		got.SourceBytes == nil || *got.SourceBytes != *o.SourceBytes ||
		got.OutputBytes == nil || *got.OutputBytes != *o.OutputBytes ||
		got.EncodeMs == nil || *got.EncodeMs != *o.EncodeMs {
		t.Errorf("the ledger row does not carry the same proof as the event:\n row=%+v\n ev =%+v", got, *o)
	}

}

// TRANSCODE-13: a SKIPPED job must record WHICH GUARD skipped it. Before this phase an
// operator saw the bare word "skipped" and had to go read the logs to find out which of
// eight guards fired.
func TestObserver_SkippedJobRecordsWhichGuardFired(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	src := filepath.Join(root, "thin.mkv")
	mkH264(t, ffmpeg, src, "80k") // well under the min-bitrate guard

	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) {
		c.MinBitrateKbps = 5000 // guarantees the low-bitrate guard fires
	})
	col := &collector{}
	eng.Observer = col.observe
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	rows, err := eng.Store.List(context.Background(), []store.Status{store.Skipped}, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 skipped row, got %d (%+v)", len(rows), rows)
	}
	if got := rows[0].Outcome.Reason; got != SkipLowBitrate {
		t.Errorf("skipped row reason = %q, want %q", got, SkipLowBitrate)
	}
	// A skip records no fidelity data — nothing was encoded, so there is nothing to
	// measure. It must read as "not recorded", never as a zero score.
	if o := rows[0].Outcome; o.VmafMean != nil || o.VmafMin != nil || o.SourceBytes != nil {
		t.Errorf("a skipped row must record no measurements, got %+v", o)
	}

	// The live event carries the same reason.
	var sawReason bool
	for _, e := range col.snapshot() {
		if e.Status == store.Skipped && e.Outcome != nil && e.Outcome.Reason == SkipLowBitrate {
			sawReason = true
		}
	}
	if !sawReason {
		t.Errorf("no Skipped event carried the guard reason; events: %+v", col.snapshot())
	}
}

// TRANSCODE-13: a FAILED job must record the ERROR that failed it. The README documents
// /api/history as returning terminal jobs "with reason" — this is the half of that claim
// the engine has to supply.
func TestObserver_FailedJobRecordsTheError(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	src := filepath.Join(root, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	before := md5f(t, src)

	// A deterministic encoder failure — the safety suite's standard seam, so the test
	// asserts the failure PATH rather than depending on codec luck.
	enc := EncoderFunc(func(ctx context.Context, in, out string) error { return errFake })
	eng := buildEngine(t, ffmpeg, ffprobe, root, enc, func(c *config.Config) {
		c.MinBitrateKbps = 0
	})
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	// The invariant, restated here because this test writes a failure path: a failed
	// encode leaves the source byte-for-byte intact.
	if md5f(t, src) != before {
		t.Fatal("source modified on a failed encode")
	}

	rows, err := eng.Store.List(context.Background(), []store.Status{store.Failed}, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 failed row, got %d (%+v)", len(rows), rows)
	}
	o := rows[0].Outcome
	if !strings.Contains(o.Reason, errFake.Error()) {
		t.Errorf("failed row reason = %q, want it to carry the encoder's error %q", o.Reason, errFake)
	}
	// The encoder is attributable on a failure too.
	if o.Encoder != "cpu" {
		t.Errorf("failed row encoder = %q, want %q", o.Encoder, "cpu")
	}
	// Nothing was verified, so there is no fidelity to record — "not recorded", not 0.
	if o.VmafMean != nil || o.SourceBytes != nil {
		t.Errorf("a failed-at-encode row must record no measurements, got %+v", o)
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
