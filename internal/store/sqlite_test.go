package store

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func openTest(t *testing.T) *SQLite {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestClaim_FreshKeyClaims(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ok, err := s.Claim(ctx, "/a/movie.mkv", "fp1", "w0", 3)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !ok {
		t.Fatal("expected fresh key to claim")
	}
	st, fc, exists, err := s.Get(ctx, "/a/movie.mkv", "fp1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !exists || st != Probing || fc != 0 {
		t.Fatalf("Get after claim = status=%q failCount=%d exists=%v, want probing/0/true", st, fc, exists)
	}
}

func TestClaim_SecondClaimWhileActiveFails(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if ok, err := s.Claim(ctx, "/a/movie.mkv", "fp1", "w0", 3); err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	ok, err := s.Claim(ctx, "/a/movie.mkv", "fp1", "w1", 3)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if ok {
		t.Fatal("second claim on an active job must fail")
	}
}

func TestClaim_AfterDoneFails(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if ok, err := s.Claim(ctx, "/a/movie.mkv", "fp1", "w0", 3); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if err := s.Finish(ctx, "/a/movie.mkv", "fp1", Done, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	ok, err := s.Claim(ctx, "/a/movie.mkv", "fp1", "w0", 3)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if ok {
		t.Fatal("claim after done must fail (permanent)")
	}
}

func TestClaim_AfterSkippedFails(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if ok, err := s.Claim(ctx, "/a/movie.mkv", "fp1", "w0", 3); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if err := s.Finish(ctx, "/a/movie.mkv", "fp1", Skipped, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	ok, err := s.Claim(ctx, "/a/movie.mkv", "fp1", "w0", 3)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if ok {
		t.Fatal("claim after skipped must fail (permanent)")
	}
}

func TestClaim_FailedRetriesThenParks(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	const maxFailures = 3
	for i := 1; i <= maxFailures; i++ {
		ok, err := s.Claim(ctx, "/a/movie.mkv", "fp1", "w0", maxFailures)
		if err != nil {
			t.Fatalf("claim attempt %d: %v", i, err)
		}
		if !ok {
			t.Fatalf("claim attempt %d: expected true (fail_count=%d < max=%d)", i, i-1, maxFailures)
		}
		if err := s.Finish(ctx, "/a/movie.mkv", "fp1", Failed, nil); err != nil {
			t.Fatalf("Finish attempt %d: %v", i, err)
		}
		_, fc, _, err := s.Get(ctx, "/a/movie.mkv", "fp1")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if fc != i {
			t.Fatalf("after attempt %d: fail_count=%d want %d", i, fc, i)
		}
	}
	// Now fail_count == maxFailures: further claims must be parked (false).
	ok, err := s.Claim(ctx, "/a/movie.mkv", "fp1", "w0", maxFailures)
	if err != nil {
		t.Fatalf("Claim (parked): %v", err)
	}
	if ok {
		t.Fatal("expected parked (fail_count >= maxFailures) to refuse claim")
	}
}

func TestAdvance_Transitions(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if ok, err := s.Claim(ctx, "/a/movie.mkv", "fp1", "w0", 3); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	for _, st := range []Status{Encoding, Verifying} {
		if err := s.Advance(ctx, "/a/movie.mkv", "fp1", st); err != nil {
			t.Fatalf("Advance(%s): %v", st, err)
		}
		got, _, exists, err := s.Get(ctx, "/a/movie.mkv", "fp1")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !exists || got != st {
			t.Fatalf("after Advance(%s): status=%q exists=%v", st, got, exists)
		}
	}
}

func TestRecoverStale_ResetsActiveJobs(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if ok, err := s.Claim(ctx, "/a/movie.mkv", "fp1", "w0", 3); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if err := s.Advance(ctx, "/a/movie.mkv", "fp1", Encoding); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	n, err := s.RecoverStale(ctx)
	if err != nil {
		t.Fatalf("RecoverStale: %v", err)
	}
	if n != 1 {
		t.Fatalf("RecoverStale returned %d, want 1", n)
	}
	st, _, exists, err := s.Get(ctx, "/a/movie.mkv", "fp1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !exists || st != Pending {
		t.Fatalf("after RecoverStale: status=%q exists=%v, want pending/true", st, exists)
	}
	// Now re-claimable.
	ok, err := s.Claim(ctx, "/a/movie.mkv", "fp1", "w1", 3)
	if err != nil {
		t.Fatalf("Claim after recover: %v", err)
	}
	if !ok {
		t.Fatal("expected job to be re-claimable after RecoverStale")
	}
}

func TestRecoverStale_LeavesTerminalAndPendingAlone(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if ok, _ := s.Claim(ctx, "/a/done.mkv", "fp1", "w0", 3); !ok {
		t.Fatal("claim done.mkv")
	}
	if err := s.Finish(ctx, "/a/done.mkv", "fp1", Done, nil); err != nil {
		t.Fatal(err)
	}
	n, err := s.RecoverStale(ctx)
	if err != nil {
		t.Fatalf("RecoverStale: %v", err)
	}
	if n != 0 {
		t.Fatalf("RecoverStale reset %d jobs, want 0 (done should be untouched)", n)
	}
	st, _, _, _ := s.Get(ctx, "/a/done.mkv", "fp1")
	if st != Done {
		t.Fatalf("done job status changed to %q", st)
	}
}

func TestGet_UnseenKeyDoesNotExist(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	st, fc, exists, err := s.Get(ctx, "/never/seen.mkv", "fpX")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if exists || st != "" || fc != 0 {
		t.Fatalf("Get on unseen key = status=%q failCount=%d exists=%v, want empty/0/false", st, fc, exists)
	}
}

// ---- concurrency (run under -race) --------------------------------------------

// TestClaim_ConcurrentSameKeyExactlyOneWins hammers Claim on the SAME path+
// fingerprint from many goroutines simultaneously. Exactly one must win — this is
// the core guarantee that lets a worker pool safely fan out over a file list
// without two workers ever encoding the same source concurrently.
func TestClaim_ConcurrentSameKeyExactlyOneWins(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	const n = 32
	var wg sync.WaitGroup
	results := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := s.Claim(ctx, "/a/movie.mkv", "fp1", "w", 3)
			if err != nil {
				t.Errorf("goroutine %d: Claim: %v", i, err)
				return
			}
			results[i] = ok
		}(i)
	}
	wg.Wait()

	wins := 0
	for _, ok := range results {
		if ok {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("exactly one goroutine should have claimed the job, got %d", wins)
	}
}

// TestHammer_DifferentKeysNoDatabaseLocked runs many goroutines each doing a full
// Claim/Advance/Finish sequence on DISTINCT keys concurrently. This proves
// MaxOpenConns(1) + busy_timeout is sufficient to avoid any "database is locked"
// error under concurrent load (the failure mode this design specifically defends
// against).
func TestHammer_DifferentKeysNoDatabaseLocked(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	const workers = 16
	const perWorker = 20
	var wg sync.WaitGroup
	errCh := make(chan error, workers*perWorker)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				path := filepath.Join("/lib", "worker", strconv.Itoa(w), strconv.Itoa(i)+".mkv")
				fp := "fp-" + strconv.Itoa(w) + "-" + strconv.Itoa(i)
				worker := "w" + strconv.Itoa(w)

				ok, err := s.Claim(ctx, path, fp, worker, 3)
				if err != nil {
					errCh <- err
					continue
				}
				if !ok {
					errCh <- errFailedClaim(path)
					continue
				}
				if err := s.Advance(ctx, path, fp, Encoding); err != nil {
					errCh <- err
					continue
				}
				if err := s.Advance(ctx, path, fp, Verifying); err != nil {
					errCh <- err
					continue
				}
				if err := s.Finish(ctx, path, fp, Done, nil); err != nil {
					errCh <- err
					continue
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil && strings.Contains(err.Error(), "locked") {
			t.Fatalf("database is locked under concurrency: %v", err)
		} else if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}
}

func errFailedClaim(path string) error {
	return &claimError{path: path}
}

type claimError struct{ path string }

func (e *claimError) Error() string {
	return "claim on fresh distinct key unexpectedly failed: " + e.path
}

// ---- List + Summary (TRANSCODE-7 read model) --------------------------------

// withClock swaps the package now() seam for a deterministic counter so
// updated_at ordering is testable, restoring it on cleanup.
func withClock(t *testing.T, start int64) *int64 {
	t.Helper()
	tick := start
	prev := now
	now = func() int64 { tick++; return tick }
	t.Cleanup(func() { now = prev })
	return &tick
}

// seed claims path and drives it to a terminal status, so the row exists with a
// deterministic updated_at (each store call advances the withClock counter).
func seed(t *testing.T, s *SQLite, path, fp string, final Status) {
	t.Helper()
	ctx := context.Background()
	ok, err := s.Claim(ctx, path, fp, "w0", 3)
	if err != nil || !ok {
		t.Fatalf("seed Claim(%s): ok=%v err=%v", path, ok, err)
	}
	if final.Active() { // leave it in an active state (don't finish)
		if err := s.Advance(ctx, path, fp, final); err != nil {
			t.Fatalf("seed Advance(%s,%s): %v", path, final, err)
		}
		return
	}
	if err := s.Finish(ctx, path, fp, final, nil); err != nil {
		t.Fatalf("seed Finish(%s,%s): %v", path, final, err)
	}
}

func TestList_FilterAndOrder(t *testing.T) {
	withClock(t, 1000)
	s := openTest(t)
	ctx := context.Background()

	seed(t, s, "/lib/a.mkv", "1:1", Done)     // updated earliest
	seed(t, s, "/lib/b.mkv", "2:2", Skipped)  // then
	seed(t, s, "/lib/c.mkv", "3:3", Encoding) // active, latest

	// All rows, newest-updated first.
	all, err := s.List(ctx, nil, 0)
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("List all: want 3 rows, got %d", len(all))
	}
	if all[0].Path != "/lib/c.mkv" || all[2].Path != "/lib/a.mkv" {
		t.Fatalf("List all: wrong order: %s ... %s", all[0].Path, all[2].Path)
	}
	if all[0].Status != Encoding || all[0].Worker != "w0" {
		t.Fatalf("List all: active row lost status/worker: %+v", all[0])
	}

	// Terminal filter (done+skipped): excludes the active row.
	term, err := s.List(ctx, []Status{Done, Skipped}, 0)
	if err != nil {
		t.Fatalf("List terminal: %v", err)
	}
	if len(term) != 2 {
		t.Fatalf("List terminal: want 2, got %d (%v)", len(term), term)
	}
	for _, j := range term {
		if j.Status == Encoding {
			t.Fatalf("terminal filter leaked an active row: %+v", j)
		}
	}

	// Limit caps the result.
	one, err := s.List(ctx, nil, 1)
	if err != nil {
		t.Fatalf("List limit: %v", err)
	}
	if len(one) != 1 || one[0].Path != "/lib/c.mkv" {
		t.Fatalf("List limit 1: want just newest c.mkv, got %v", one)
	}
}

func TestList_EmptyStore(t *testing.T) {
	s := openTest(t)
	got, err := s.List(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("List empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List empty: want 0 rows, got %d", len(got))
	}
}

func TestSummary_CountsPerStatus(t *testing.T) {
	withClock(t, 2000)
	s := openTest(t)

	seed(t, s, "/lib/a.mkv", "1:1", Done)
	seed(t, s, "/lib/b.mkv", "2:2", Done)
	seed(t, s, "/lib/c.mkv", "3:3", Skipped)
	seed(t, s, "/lib/d.mkv", "4:4", Verifying) // active

	sum, err := s.Summary(context.Background())
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum[Done] != 2 {
		t.Fatalf("Summary done: want 2, got %d", sum[Done])
	}
	if sum[Skipped] != 1 {
		t.Fatalf("Summary skipped: want 1, got %d", sum[Skipped])
	}
	if sum[Verifying] != 1 {
		t.Fatalf("Summary verifying: want 1, got %d", sum[Verifying])
	}
	if _, ok := sum[Failed]; ok {
		t.Fatalf("Summary: failed should be absent (no such rows), got %d", sum[Failed])
	}
}

// --- outcome columns (TRANSCODE-13) ------------------------------------------

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

// A Done row keeps the whole proof, and it round-trips: every field written comes back
// as itself, and NULL comes back as nil rather than as a zero anyone could mistake for a
// measurement.
func TestFinish_RecordsAndRoundTripsTheOutcome(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if ok, err := s.Claim(ctx, "/a/movie.mkv", "fp1", "w0", 3); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	want := &Outcome{
		Encoder:     "cpu",
		VmafMean:    f64(97.25),
		VmafMin:     f64(88.5),
		VmafModel:   "version=vmaf_v0.6.1",
		SourceBytes: i64(5_000_000),
		OutputBytes: i64(2_000_000),
		EncodeMs:    i64(12_345),
	}
	if err := s.Finish(ctx, "/a/movie.mkv", "fp1", Done, want); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	rows, err := s.List(ctx, []Status{Done}, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("List: rows=%d err=%v", len(rows), err)
	}
	got := rows[0].Outcome
	if got.Encoder != want.Encoder || got.VmafModel != want.VmafModel {
		t.Errorf("strings: got %+v, want %+v", got, want)
	}
	if got.VmafMean == nil || *got.VmafMean != *want.VmafMean ||
		got.VmafMin == nil || *got.VmafMin != *want.VmafMin {
		t.Errorf("vmaf pair: got mean=%v min=%v", got.VmafMean, got.VmafMin)
	}
	if got.SourceBytes == nil || *got.SourceBytes != *want.SourceBytes ||
		got.OutputBytes == nil || *got.OutputBytes != *want.OutputBytes ||
		got.EncodeMs == nil || *got.EncodeMs != *want.EncodeMs {
		t.Errorf("numbers: got src=%v out=%v ms=%v", got.SourceBytes, got.OutputBytes, got.EncodeMs)
	}
	// A Done needs no excuse.
	if got.Reason != "" {
		t.Errorf("Reason = %q, want empty", got.Reason)
	}
}

// A nil outcome (and an unset field within one) stores NULL, and NULL reads back as
// "not recorded" — nil, never 0. This is the fail-safe the whole schema rests on: a
// fabricated 0.0 VMAF is a claim about a swap nobody measured.
func TestFinish_NilOutcomeReadsAsNotRecordedNotZero(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if ok, err := s.Claim(ctx, "/a/movie.mkv", "fp1", "w0", 3); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if err := s.Finish(ctx, "/a/movie.mkv", "fp1", Done, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	rows, err := s.List(ctx, []Status{Done}, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("List: rows=%d err=%v", len(rows), err)
	}
	o := rows[0].Outcome
	if o.VmafMean != nil || o.VmafMin != nil || o.SourceBytes != nil || o.OutputBytes != nil || o.EncodeMs != nil {
		t.Errorf("an unrecorded outcome must read back as nil, got %+v", o)
	}
	if o.Reason != "" || o.Encoder != "" || o.VmafModel != "" {
		t.Errorf("an unrecorded outcome must read back with empty strings, got %+v", o)
	}
}

// A retried job that finally succeeds must not carry the PREVIOUS attempt's failure
// reason next to its "done". Finish fully defines a row's proof, so the stale reason is
// cleared rather than merged forward — a "done · reason: simulated encode failure" row
// would be a lie the ledger tells forever.
func TestFinish_LaterOutcomeReplacesTheEarlierOne(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	if ok, err := s.Claim(ctx, "/a/movie.mkv", "fp1", "w0", 3); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if err := s.Finish(ctx, "/a/movie.mkv", "fp1", Failed, &Outcome{Reason: "encode blew up", Encoder: "cpu"}); err != nil {
		t.Fatalf("Finish(failed): %v", err)
	}
	// Retry (failed is retryable under MaxFailures) and succeed this time.
	if ok, err := s.Claim(ctx, "/a/movie.mkv", "fp1", "w0", 3); err != nil || !ok {
		t.Fatalf("re-claim after failure: ok=%v err=%v", ok, err)
	}
	if err := s.Finish(ctx, "/a/movie.mkv", "fp1", Done, &Outcome{
		Encoder: "cpu", SourceBytes: i64(100), OutputBytes: i64(40),
	}); err != nil {
		t.Fatalf("Finish(done): %v", err)
	}

	rows, err := s.List(ctx, []Status{Done}, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("List: rows=%d err=%v", len(rows), err)
	}
	if got := rows[0].Outcome.Reason; got != "" {
		t.Errorf("the done row still carries the failed attempt's reason %q — Finish merged instead of replacing", got)
	}
}

// The lifetime reclaimed total is derived from the stored sizes, so it survives a
// restart. Before TRANSCODE-13 the only reclaimed figure anywhere was an in-process
// counter, and the number an operator saw reset to 0 on every daemon bounce.
func TestReclaimed_IsDurableAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	ctx := context.Background()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, r := range []struct {
		path     string
		st       Status
		src, out int64
		recorded bool
	}{
		{"/a/one.mkv", Done, 1000, 400, true},    // reclaims 600
		{"/a/two.mkv", Done, 500, 200, true},     // reclaims 300
		{"/a/three.mkv", Done, 0, 0, false},      // a pre-TRANSCODE-13 row: NOT recorded
		{"/a/four.mkv", Skipped, 900, 100, true}, // skipped — reclaims nothing, ever
	} {
		if ok, err := s.Claim(ctx, r.path, "fp", "w0", 3); err != nil || !ok {
			t.Fatalf("claim %s: ok=%v err=%v", r.path, ok, err)
		}
		var o *Outcome
		if r.recorded {
			o = &Outcome{SourceBytes: i64(r.src), OutputBytes: i64(r.out)}
		}
		if err := s.Finish(ctx, r.path, "fp", r.st, o); err != nil {
			t.Fatalf("finish %s: %v", r.path, err)
		}
	}
	if got, err := s.Reclaimed(ctx); err != nil || got != 900 {
		t.Fatalf("Reclaimed = %d (err %v), want 900 (600+300; the unrecorded row contributes nothing, the skipped row is not a transcode)", got, err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The whole point: reopen the process and the number is still there.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if got, err := s2.Reclaimed(ctx); err != nil || got != 900 {
		t.Fatalf("Reclaimed after reopen = %d (err %v), want 900 — the total must survive a restart", got, err)
	}
}
