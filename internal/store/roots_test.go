package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// rootsStore opens an empty store for the per-root reads' own tests.
func rootsStore(t *testing.T) *SQLite {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// rootsRow records one terminal row through Claim and Finish.
func rootsRow(t *testing.T, s *SQLite, path string, status Status, root string, source, output *int64) {
	t.Helper()
	ctx := context.Background()
	if ok, err := s.Claim(ctx, path, "1:1", "w0", 3, DecisionInputs{}); err != nil || !ok {
		t.Fatalf("Claim(%s): ok=%v err=%v", path, ok, err)
	}
	if err := s.Finish(ctx, path, "1:1", status,
		&Outcome{Decision: Decision{LibraryRoot: root}, SourceBytes: source, OutputBytes: output}, 3); err != nil {
		t.Fatalf("Finish(%s): %v", path, err)
	}
}

func rootsInt(v int64) *int64 { return &v }

// TestS0169_AC2_RootTotalsCountsSizedAndUnsizedCandidatesPerRecordedRoot grades [AC-2],
// [AC-3] and [AC-12] of S0169 at the store: one entry per recorded root in root order, the
// rows that recorded none under "", sized candidates summed and unsized ones counted
// apart, the basis over done rows recording BOTH sizes, and no other status contributing.
func TestS0169_AC2_RootTotalsCountsSizedAndUnsizedCandidatesPerRecordedRoot(t *testing.T) {
	s := rootsStore(t)
	ctx := context.Background()

	totals, err := s.RootTotals(ctx)
	if err != nil || totals == nil || len(totals) != 0 {
		t.Fatalf("RootTotals over an empty ledger = %v, %v; want an empty, non-nil list", totals, err)
	}

	rootsRow(t, s, "/b/c1.mkv", WouldTranscode, "/b", rootsInt(10), nil)
	rootsRow(t, s, "/b/c2.mkv", WouldTranscode, "/b", rootsInt(0), nil) // a recorded zero is sized
	rootsRow(t, s, "/b/u.mkv", WouldTranscode, "/b", nil, nil)
	// A candidate recording an OUTPUT size and no source size is still unsized.
	rootsRow(t, s, "/b/u2.mkv", WouldTranscode, "/b", nil, rootsInt(4))
	rootsRow(t, s, "/b/d1.mkv", Done, "/b", rootsInt(100), rootsInt(40))
	rootsRow(t, s, "/b/d2.mkv", Done, "/b", rootsInt(50), rootsInt(20))
	rootsRow(t, s, "/b/d3.mkv", Done, "/b", rootsInt(7_000), nil)
	rootsRow(t, s, "/b/d4.mkv", Done, "/b", nil, rootsInt(9_000))
	rootsRow(t, s, "/a/c.mkv", WouldTranscode, "/a", rootsInt(3), nil)
	rootsRow(t, s, "/x/c.mkv", WouldTranscode, "", rootsInt(1), nil)
	rootsRow(t, s, "/x/d.mkv", Done, "", rootsInt(8), rootsInt(2))
	rootsRow(t, s, "/b/s.mkv", Skipped, "/b", rootsInt(9_999), rootsInt(1))
	rootsRow(t, s, "/b/f.mkv", Failed, "/b", rootsInt(9_999), rootsInt(1))
	// A root holding only rows in other statuses has no entry at all.
	rootsRow(t, s, "/z/s.mkv", Skipped, "/z", rootsInt(9_999), nil)

	totals, err = s.RootTotals(ctx)
	if err != nil {
		t.Fatalf("RootTotals: %v", err)
	}
	want := []RootTotal{
		{LibraryRoot: "", CandidateFiles: 1, CandidateBytes: 1, BasisFiles: 1, BasisSourceBytes: 8, BasisSavedBytes: 6},
		{LibraryRoot: "/a", CandidateFiles: 1, CandidateBytes: 3},
		{LibraryRoot: "/b", CandidateFiles: 2, CandidateExcluded: 2, CandidateBytes: 10,
			BasisFiles: 2, BasisSourceBytes: 150, BasisSavedBytes: 90},
	}
	if len(totals) != len(want) {
		t.Fatalf("RootTotals = %+v, want %+v", totals, want)
	}
	for i := range want {
		if totals[i] != want[i] {
			t.Errorf("RootTotals[%d] = %+v, want %+v", i, totals[i], want[i])
		}
	}

	// It is a read: the rows are what they were.
	sum, err := s.Summary(ctx)
	if err != nil || sum[WouldTranscode] != 6 || sum[Done] != 5 || sum[Skipped] != 2 || sum[Failed] != 1 {
		t.Errorf("the ledger after the read = %v, %v", sum, err)
	}
}

// TestS0169_AC3_RootTotalsHoldsPebibyteSums grades [AC-3]'s "no overflow" at the store:
// sums of rows of a pebibyte each come back exact.
func TestS0169_AC3_RootTotalsHoldsPebibyteSums(t *testing.T) {
	s := rootsStore(t)
	const pib = int64(1) << 50
	for _, name := range []string{"c1", "c2", "c3"} {
		rootsRow(t, s, "/a/"+name+".mkv", WouldTranscode, "/a", rootsInt(pib), nil)
	}
	rootsRow(t, s, "/a/d1.mkv", Done, "/a", rootsInt(pib), rootsInt(pib/2))
	rootsRow(t, s, "/a/d2.mkv", Done, "/a", rootsInt(pib), rootsInt(pib/2-1))

	totals, err := s.RootTotals(context.Background())
	if err != nil || len(totals) != 1 {
		t.Fatalf("RootTotals = %+v, %v", totals, err)
	}
	want := RootTotal{LibraryRoot: "/a", CandidateFiles: 3, CandidateBytes: 3 * pib,
		BasisFiles: 2, BasisSourceBytes: 2 * pib, BasisSavedBytes: pib + 1}
	if totals[0] != want {
		t.Errorf("RootTotals = %+v, want %+v", totals[0], want)
	}
}

// TestS0169_AC4_HeldBySourceIsExactlyTheRowsHeldByUndoWindowSums grades [AC-4] of S0169 at
// the store: the per-source read returns the live retentions and no other, source path
// ascending, and their sizes sum to HeldByUndoWindow - one predicate, not two spellings of
// "live". ListRetained, built on the same predicate, lists the same rows.
func TestS0169_AC4_HeldBySourceIsExactlyTheRowsHeldByUndoWindowSums(t *testing.T) {
	s := rootsStore(t)
	ctx := context.Background()

	sources, err := s.HeldBySource(ctx)
	if err != nil || sources == nil || len(sources) != 0 {
		t.Fatalf("HeldBySource over an empty table = %v, %v; want an empty, non-nil list", sources, err)
	}

	for path, size := range map[string]int64{"/lib/b.mkv": 7, "/lib/a.mkv": 11, "/lib/restored.mkv": 1_000, "/lib/released.mkv": 50_000} {
		if err := s.Retain(ctx, Retained{SourcePath: path, SwappedPath: path, RetainedPath: path + ".retained",
			SourceBytes: size, SwappedFingerprint: "1:1", RetainedAt: 100, ExpiresAt: 200}); err != nil {
			t.Fatalf("Retain(%s): %v", path, err)
		}
	}
	if err := s.MarkRestored(ctx, "/lib/restored.mkv", 150); err != nil {
		t.Fatalf("MarkRestored: %v", err)
	}
	if err := s.DropRetained(ctx, "/lib/released.mkv"); err != nil {
		t.Fatalf("DropRetained: %v", err)
	}

	sources, err = s.HeldBySource(ctx)
	if err != nil {
		t.Fatalf("HeldBySource: %v", err)
	}
	want := []HeldSource{{SourcePath: "/lib/a.mkv", SourceBytes: 11}, {SourcePath: "/lib/b.mkv", SourceBytes: 7}}
	if len(sources) != 2 || sources[0] != want[0] || sources[1] != want[1] {
		t.Errorf("HeldBySource = %+v, want %+v (live retentions only, source path ascending)", sources, want)
	}
	var sum int64
	for _, src := range sources {
		sum += src.SourceBytes
	}
	held, err := s.HeldByUndoWindow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if held != 18 || sum != held {
		t.Errorf("HeldByUndoWindow = %d and the per-source read sums to %d, want 18 from both", held, sum)
	}
	live, err := s.ListRetained(ctx)
	if err != nil || len(live) != 2 {
		t.Errorf("ListRetained = %d rows, %v; want the same 2 live retentions", len(live), err)
	}
	// It is a read: the restored retention is still on record as restored.
	if r, ok, err := s.GetRetained(ctx, "/lib/restored.mkv"); err != nil || !ok || r.RestoredAt == nil || *r.RestoredAt != 150 {
		t.Errorf("the restored retention after the read = %+v, %v, %v", r, ok, err)
	}
}

// TestS0169_AC8_AFailedPerRootReadIsANamedErrorAndNoFigure grades the store's half of
// [AC-8] and [AC-9] of S0169: a read that cannot be answered returns an error naming the
// read and nil, never an empty list a caller would publish as zeros.
func TestS0169_AC8_AFailedPerRootReadIsANamedErrorAndNoFigure(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	ctx := context.Background()

	if got, err := s.RootTotals(ctx); err == nil || got != nil || !strings.Contains(err.Error(), "store: root totals") {
		t.Errorf("RootTotals on a closed store = %v, %v; want nil and a named error", got, err)
	}
	if got, err := s.HeldBySource(ctx); err == nil || got != nil || !strings.Contains(err.Error(), "store: held by source") {
		t.Errorf("HeldBySource on a closed store = %v, %v; want nil and a named error", got, err)
	}
}
