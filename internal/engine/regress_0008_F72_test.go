package engine

// regress_0008_F72 - S0008-holdfast-swap-1, impl gate ordinal 2, finding F72 (advisory).
//
// THE CLAIM. loadHoldBacks logs and CONTINUES when the store reads that build the two
// record-based hold-backs fail, leaving the run with none of them - while AC15c is
// unconditional ("SHALL NOT encode, swap, delete or re-queue anything at either path
// until it is resolved").
//
// THE RULING TAKEN. The trade stands: refusing to scan would turn a store hiccup into a
// stopped library, and the alternative to a degraded run is no run at all. What was
// missing is that the report said "continuing" without naming the guarantee now standing
// on something else, and that the something else was argued rather than tested. This
// file tests it. With BOTH record reads failing - the real shape of an unreadable store -
// the two halves of AC15c fall back to:
//
//   - the RETAINED REPLACEMENT: held back by its NAME, which needs no record at all.
//     That is what the retained marker exists for and it is untouched by a store error.
//   - the PARKED SOURCE: Claim refuses an `indeterminate` row, so the file is not
//     encoded, swapped, deleted or re-queued. That refusal is keyed on path+fingerprint,
//     so it covers a parked source whose bytes have not moved - the residue is one
//     rewritten since it was parked, which the log now names rather than implying the
//     criterion is fully enforced.
//
// RED without the fix's log change, and red if either fallback is ever removed.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// unreadableRecords wraps a real store and refuses exactly the two reads that build the
// record-based hold-backs. Everything else behaves normally, which is what a store that
// cannot be read at that moment looks like from the engine's side.
type unreadableRecords struct {
	store.Store
	err error
}

func (u unreadableRecords) ParkedIncidents(context.Context) ([]store.SwapIncident, error) {
	return nil, u.err
}

func (u unreadableRecords) ExcludedReplacementPaths(context.Context) ([]string, error) {
	return nil, u.err
}

func TestHoldBacks_AStoreReadErrorNamesTheCriterionAndTheFallbacksHold(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	srcMD5 := md5f(t, src)

	real := newTestStore(t, d)
	cfg := baseCfg(d)
	prober := probe.New(ffmpeg, ffprobe)

	// Park a job for real: a failed swap on storage that is not local.
	first := New(cfg, prober, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}, real, discardLogger())
	first.renameFn = failingRename(errSwap)
	first.fsLookup = lookups("nfs")
	if err := first.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	in := onlyIncident(t, real)
	if !in.Parked() {
		t.Fatalf("precondition: the job is not parked: %+v", in)
	}
	retained := in.ReplacementPath
	if !exists(retained) {
		t.Fatalf("precondition: no retained replacement at %s; dir holds %v", retained, lsDir(t, d))
	}
	retainedMD5 := md5f(t, retained)

	// The next run cannot read either record.
	logs := &capturedLog{}
	blind := unreadableRecords{Store: real, err: errors.New("simulated: the job store cannot be read")}
	next := New(cfg, prober, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}, blind, logs.logger())
	if err := next.RunOneshot(context.Background()); err != nil {
		t.Fatalf("degraded RunOneshot: %v", err)
	}

	// The hold-backs really were lost - otherwise the fallbacks below are not what is
	// keeping the files intact and this test proves nothing.
	if _, held := next.heldBack(src); held {
		t.Fatal("precondition: the record-based hold-back survived the store error, so the fallbacks are not under test")
	}

	// AC15c's two paths, both still untouched. Asserted by looking at the files.
	if !exists(src) || md5f(t, src) != srcMD5 {
		t.Error("the parked job's source was encoded, swapped or deleted while the records could not be read")
	}
	if !exists(retained) || md5f(t, retained) != retainedMD5 {
		t.Error("the parked job's replacement was encoded, swapped or deleted while the records could not be read")
	}
	if got := codecOf(t, ffprobe, src); got != "h264" {
		t.Errorf("the parked source is %q, want the untouched h264 source", got)
	}
	if row, ok := jobRow(t, real, retained, probe.Fingerprint(retained)); ok {
		t.Errorf("the retained replacement was offered to the pipeline (row %q)", row.Status)
	}
	// The row for the parked source is still the parked one: Claim refused it rather
	// than beginning a new attempt.
	if row, ok := jobRow(t, real, src, probe.Fingerprint(src)); !ok || row.Status != store.Indeterminate {
		t.Errorf("the parked row is %q (present=%v), want %q - Claim did not refuse it", row.Status, ok, store.Indeterminate)
	}

	// And it was REPORTED as what it is: the criterion whose record-based enforcement is
	// gone, named, with what is still holding each half.
	out := logs.String()
	for _, want := range []string{"AC15c", "AC15d", "CONTINUING WITHOUT", "Claim", "NAME"} {
		if !strings.Contains(out, want) {
			t.Errorf("the degraded run's report never mentions %q; log:\n%s", want, out)
		}
	}
}
