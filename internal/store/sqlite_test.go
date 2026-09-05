package store

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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

// The recorded proof is DURABLE — it survives closing and reopening the database. This
// is the point of the phase: before it, the sizes lived only in an in-process counter,
// so an operator's "reclaimed" figure reset to 0 on every daemon bounce. Deriving a
// lifetime total from these columns is TRANSCODE-14's; persisting them so it CAN be is
// this phase's, and this is the proof that it is.
func TestOutcome_SurvivesAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	ctx := context.Background()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if ok, err := s.Claim(ctx, "/a/one.mkv", "fp", "w0", 3); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if err := s.Finish(ctx, "/a/one.mkv", "fp", Done, &Outcome{
		Encoder: "cpu", VmafMean: f64(97.25), VmafMin: f64(88.5), VmafModel: "version=vmaf_v0.6.1",
		SourceBytes: i64(1000), OutputBytes: i64(400), EncodeMs: i64(12_345),
	}); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()

	rows, err := s2.List(ctx, []Status{Done}, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("List after reopen: rows=%d err=%v", len(rows), err)
	}
	o := rows[0].Outcome
	if o.SourceBytes == nil || *o.SourceBytes != 1000 || o.OutputBytes == nil || *o.OutputBytes != 400 {
		t.Errorf("sizes did not survive the reopen: src=%v out=%v", o.SourceBytes, o.OutputBytes)
	}
	if o.VmafMean == nil || *o.VmafMean != 97.25 || o.VmafMin == nil || *o.VmafMin != 88.5 || o.VmafModel != "version=vmaf_v0.6.1" {
		t.Errorf("the fidelity proof did not survive the reopen: %+v", o)
	}
	if o.Encoder != "cpu" || o.EncodeMs == nil || *o.EncodeMs != 12_345 {
		t.Errorf("encoder/duration did not survive the reopen: %+v", o)
	}
}

// A RETRY must not serve the previous attempt's proof.
//
// Claim is the transition that begins a new attempt, and only a Failed row can reach it
// with an outcome already attached — an outcome describing an encode that was REJECTED
// and whose temp was deleted. If Claim left it there, the row would sit in probing/
// encoding/verifying (for hours, on a real film) still carrying that dead encode's
// failure reason and, far worse, its VMAF score. /api/queue projects the same columns as
// /api/history, so an operator would watch an in-flight file advertise a fidelity number
// belonging to a file that no longer exists. That is a fabricated score — precisely what
// this schema exists to prevent — so Claim clears the columns.
func TestClaim_RetryClearsThePreviousAttemptsOutcome(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	if ok, err := s.Claim(ctx, "/a/movie.mkv", "fp1", "w0", 3); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	// A VMAF rejection: the row records why, and what it measured.
	if err := s.Finish(ctx, "/a/movie.mkv", "fp1", Failed, &Outcome{
		Reason:  "VMAF worst-frame below floor (min=41.00 < vmaf_min_pool=60.00)",
		Encoder: "cpu", VmafMean: f64(87.5), VmafMin: f64(41.0), VmafModel: "version=vmaf_v0.6.1",
		EncodeMs: i64(12_345),
	}); err != nil {
		t.Fatalf("Finish(failed): %v", err)
	}

	// Retry it (failed is retryable under MaxFailures) and inspect the row MID-FLIGHT.
	if ok, err := s.Claim(ctx, "/a/movie.mkv", "fp1", "w1", 3); err != nil || !ok {
		t.Fatalf("re-claim: ok=%v err=%v", ok, err)
	}
	if err := s.Advance(ctx, "/a/movie.mkv", "fp1", Encoding); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	rows, err := s.List(ctx, []Status{Encoding}, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("List: rows=%d err=%v", len(rows), err)
	}
	o := rows[0].Outcome
	if o.Reason != "" {
		t.Errorf("an in-flight retry still carries the previous attempt's reason %q", o.Reason)
	}
	if o.VmafMean != nil || o.VmafMin != nil {
		t.Errorf("an in-flight retry is advertising the REJECTED encode's VMAF score (mean=%v min=%v) — that encode was deleted",
			o.VmafMean, o.VmafMin)
	}
	if o.VmafModel != "" || o.Encoder != "" || o.EncodeMs != nil {
		t.Errorf("an in-flight retry still carries stale outcome data: %+v", o)
	}
	// fail_count is retry ACCOUNTING, not proof of an outcome — it must survive.
	if rows[0].FailCount != 1 {
		t.Errorf("fail_count = %d, want 1 — clearing the outcome must not reset retry accounting", rows[0].FailCount)
	}
}

// --- TRANSCODE-14: lifetime reclaimed total ----------------------------------

// ReclaimedTotal sums (source - output) over done rows that recorded BOTH sizes, and
// is what makes the dashboard's reclaimed figure durable across restarts.
func TestReclaimedTotal_SumsDoneRowsWithBothSizes(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	// Two real reclaims: 3 MB and 1.5 MB.
	done := func(path string, src, out int64) {
		if ok, err := s.Claim(ctx, path, "fp", "w0", 3); err != nil || !ok {
			t.Fatalf("claim %s: ok=%v err=%v", path, ok, err)
		}
		if err := s.Finish(ctx, path, "fp", Done, &Outcome{SourceBytes: i64(src), OutputBytes: i64(out)}); err != nil {
			t.Fatalf("finish %s: %v", path, err)
		}
	}
	done("/a/one.mkv", 5_000_000, 2_000_000)
	done("/a/two.mkv", 4_000_000, 2_500_000)

	// A done row with NO sizes (a pre-outcome-columns row) must contribute 0, never be
	// read as a 0-byte-reclaimed row — and never crash the SUM on a NULL.
	if ok, err := s.Claim(ctx, "/a/legacy.mkv", "fp", "w0", 3); err != nil || !ok {
		t.Fatalf("claim legacy: ok=%v err=%v", ok, err)
	}
	if err := s.Finish(ctx, "/a/legacy.mkv", "fp", Done, nil); err != nil {
		t.Fatalf("finish legacy: %v", err)
	}
	// A skipped row is not a reclaim and must not count.
	if _, err := s.RecordSkip(ctx, "/a/skip.mkv", "fp", "low-bitrate"); err != nil {
		t.Fatalf("record skip: %v", err)
	}

	total, err := s.ReclaimedTotal(ctx)
	if err != nil {
		t.Fatalf("ReclaimedTotal: %v", err)
	}
	if want := int64(3_000_000 + 1_500_000); total != want {
		t.Errorf("ReclaimedTotal = %d, want %d", total, want)
	}
}

func TestReclaimedTotal_EmptyStoreIsZero(t *testing.T) {
	s := openTest(t)
	total, err := s.ReclaimedTotal(context.Background())
	if err != nil {
		t.Fatalf("ReclaimedTotal: %v", err)
	}
	if total != 0 {
		t.Errorf("ReclaimedTotal on empty store = %d, want 0", total)
	}
}

// --- TRANSCODE-14: RecordSkip / ClearSkip (the hardlink guard's report path) --

// RecordSkip writes a skipped row a pre-Claim guard can show in the UI, and reports
// whether it actually recorded one — so a caller emits/counts the skip exactly once.
func TestRecordSkip_InsertsThenIsIdempotent(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	changed, err := s.RecordSkip(ctx, "/a/seed.mkv", "fp", "hardlinked")
	if err != nil {
		t.Fatalf("RecordSkip: %v", err)
	}
	if !changed {
		t.Fatal("first RecordSkip must report changed=true")
	}
	st, _, exists, err := s.Get(ctx, "/a/seed.mkv", "fp")
	if err != nil || !exists || st != Skipped {
		t.Fatalf("after RecordSkip: status=%q exists=%v err=%v, want skipped", st, exists, err)
	}
	rows, _ := s.List(ctx, []Status{Skipped}, 0)
	if len(rows) != 1 || rows[0].Outcome.Reason != "hardlinked" {
		t.Fatalf("reason not recorded: rows=%+v", rows)
	}

	// A second call on the already-skipped row is a no-op: changed=false, so the caller
	// does not re-emit the skip on every scan.
	changed, err = s.RecordSkip(ctx, "/a/seed.mkv", "fp", "hardlinked")
	if err != nil {
		t.Fatalf("RecordSkip 2: %v", err)
	}
	if changed {
		t.Error("second RecordSkip on an existing skip must report changed=false")
	}
}

// RecordSkip must NEVER clobber a row that already carries a real terminal outcome:
// a mutable guard's report is worth less than measured proof.
func TestRecordSkip_DoesNotClobberARealOutcome(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if ok, err := s.Claim(ctx, "/a/movie.mkv", "fp", "w0", 3); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	proof := &Outcome{Encoder: "cpu", VmafMean: f64(97.0), VmafMin: f64(90.0),
		SourceBytes: i64(5_000_000), OutputBytes: i64(2_000_000)}
	if err := s.Finish(ctx, "/a/movie.mkv", "fp", Done, proof); err != nil {
		t.Fatalf("finish: %v", err)
	}

	changed, err := s.RecordSkip(ctx, "/a/movie.mkv", "fp", "hardlinked")
	if err != nil {
		t.Fatalf("RecordSkip: %v", err)
	}
	if changed {
		t.Error("RecordSkip over a done row must not change it (changed=true)")
	}
	rows, _ := s.List(ctx, []Status{Done}, 0)
	if len(rows) != 1 || rows[0].Outcome.VmafMean == nil || *rows[0].Outcome.VmafMean != 97.0 {
		t.Fatalf("done proof was clobbered: rows=%+v", rows)
	}
	if rows[0].Status != Done {
		t.Errorf("status = %q, want done (untouched)", rows[0].Status)
	}
}

// ClearSkip deletes ONLY a skipped row whose reason matches — the re-evaluation half of
// a mutable guard. It must leave a done/failed/other-skip row alone.
func TestClearSkip_RemovesMatchingSkipOnly(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	if _, err := s.RecordSkip(ctx, "/a/seed.mkv", "fp", "hardlinked"); err != nil {
		t.Fatalf("record hardlink skip: %v", err)
	}
	if _, err := s.RecordSkip(ctx, "/a/small.mkv", "fp", "low-bitrate"); err != nil {
		t.Fatalf("record low-bitrate skip: %v", err)
	}

	// Clearing "hardlinked" removes the seed's skip so it re-enters the normal path.
	if err := s.ClearSkip(ctx, "/a/seed.mkv", "fp", "hardlinked"); err != nil {
		t.Fatalf("ClearSkip: %v", err)
	}
	if _, _, exists, _ := s.Get(ctx, "/a/seed.mkv", "fp"); exists {
		t.Error("hardlinked skip should have been cleared")
	}
	// A different reason must be untouched by a hardlinked clear.
	if _, _, exists, _ := s.Get(ctx, "/a/small.mkv", "fp"); !exists {
		t.Error("ClearSkip('hardlinked') must not delete a low-bitrate skip")
	}
	// Clearing a non-existent row is a no-op, not an error.
	if err := s.ClearSkip(ctx, "/a/nope.mkv", "fp", "hardlinked"); err != nil {
		t.Errorf("ClearSkip on absent row: %v", err)
	}
}

// --- DASH-7: whole-ledger aggregates -----------------------------------------
//
// The reporting caps the API ships with (500 queue rows, 200 history rows) are the
// thing these tests exist to defeat. Every ledger they seed is DELIBERATELY larger
// than both, because an aggregate that agrees with the whole table on 3 rows and on
// 300,000 rows looks identical until the table is bigger than the view.

const (
	// The API's reporting caps, restated here as the thing to exceed. They live in
	// internal/server (a store test must not import it), so the seeds below are sized
	// against these and the server-side test asserts the real ones.
	testQueueCap   = 500
	testHistoryCap = 200
)

// seedTerminal claims path and finishes it with the given status and outcome, so a test
// can seed a row carrying exactly the facts (or the absences) it wants to aggregate.
func seedTerminal(t *testing.T, s *SQLite, path string, st Status, o *Outcome) {
	t.Helper()
	ctx := context.Background()
	ok, err := s.Claim(ctx, path, "fp", "w0", 3)
	if err != nil || !ok {
		t.Fatalf("seedTerminal Claim(%s): ok=%v err=%v", path, ok, err)
	}
	if err := s.Finish(ctx, path, "fp", st, o); err != nil {
		t.Fatalf("seedTerminal Finish(%s): %v", path, err)
	}
}

func closeToF(t *testing.T, what string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Errorf("%s = nil, want %v", what, want)
		return
	}
	if math.Abs(*got-want) > 1e-9*math.Max(1, math.Abs(want)) {
		t.Errorf("%s = %v, want %v", what, *got, want)
	}
}

func bucketsOf(b Breakdown) map[string]int64 {
	m := make(map[string]int64, len(b.Buckets))
	for _, x := range b.Buckets {
		m[x.Key] = x.Count
	}
	return m
}

// THE criterion of this phase: an aggregate is over every matching row in the table,
// not over the capped rows the queue and history views ship. The ledger seeded here
// holds MORE terminal rows than the history cap and MORE non-terminal rows than the
// queue cap, so a figure derived from either view is arithmetically unable to match -
// the test states the capped values it must NOT equal, alongside the whole-table values
// it must.
func TestAggregates_ComputedOverEveryRowNotTheCappedViews(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	const (
		nDone    = testHistoryCap + 30 // 230 terminal-done rows
		nSkipped = testHistoryCap + 5  // 205 skipped rows
		nQueued  = testQueueCap + 5    // 505 non-terminal rows
	)

	// Done rows carry a deterministic, non-degenerate spread of every recorded fact,
	// and the expected values are accumulated here rather than restated as constants -
	// a hand-copied expectation is a second implementation to get wrong.
	var (
		ratioSum, meanSum, minSum float64
		ratioLo, ratioHi          = math.Inf(1), math.Inf(-1)
		msSum, msLo, msHi         = int64(0), int64(math.MaxInt64), int64(math.MinInt64)
		vmeanLo, vmeanHi          = math.Inf(1), math.Inf(-1)
		vminLo, vminHi            = math.Inf(1), math.Inf(-1)
	)
	for i := 0; i < nDone; i++ {
		src := int64(1_000_000 + i*1_000)
		out := int64(300_000 + i*100)
		ms := int64(60_000 + i*37)
		vmean := 90.0 + float64(i%100)/10.0
		vmin := 60.0 + float64(i%80)/10.0
		model := "version=vmaf_v0.6.1"

		ratio := float64(out) / float64(src)
		ratioSum += ratio
		ratioLo, ratioHi = math.Min(ratioLo, ratio), math.Max(ratioHi, ratio)
		msSum += ms
		if ms < msLo {
			msLo = ms
		}
		if ms > msHi {
			msHi = ms
		}
		meanSum += vmean
		vmeanLo, vmeanHi = math.Min(vmeanLo, vmean), math.Max(vmeanHi, vmean)
		minSum += vmin
		vminLo, vminHi = math.Min(vminLo, vmin), math.Max(vminHi, vmin)

		seedTerminal(t, s, "/lib/done"+strconv.Itoa(i)+".mkv", Done, &Outcome{
			Encoder: "cpu", VmafMean: &vmean, VmafMin: &vmin, VmafModel: model,
			SourceBytes: &src, OutputBytes: &out, EncodeMs: &ms,
		})
	}

	guards := []string{"low-bitrate", "already-at-target-codec", "hardlinked"}
	wantGuards := map[string]int64{}
	for i := 0; i < nSkipped; i++ {
		g := guards[i%len(guards)]
		wantGuards[g]++
		seedTerminal(t, s, "/lib/skip"+strconv.Itoa(i)+".mkv", Skipped, &Outcome{Reason: g})
	}
	for i := 0; i < nQueued; i++ {
		p := "/lib/queued" + strconv.Itoa(i) + ".mkv"
		if ok, err := s.Claim(ctx, p, "fp", "w0", 3); err != nil || !ok {
			t.Fatalf("Claim(%s): ok=%v err=%v", p, ok, err)
		}
	}

	// The views really are capped - this is the premise, not an aside. If these ever
	// stopped truncating, the test below would pass for the wrong reason.
	hist, err := s.List(ctx, []Status{Done, Skipped, Failed}, testHistoryCap)
	if err != nil {
		t.Fatalf("List history: %v", err)
	}
	queue, err := s.List(ctx, []Status{Pending, Probing, Encoding, Verifying}, testQueueCap)
	if err != nil {
		t.Fatalf("List queue: %v", err)
	}
	if len(hist) != testHistoryCap || len(queue) != testQueueCap {
		t.Fatalf("seed did not exceed the caps: history=%d queue=%d", len(hist), len(queue))
	}

	// Timed and LOGGED, not asserted: the figures are recomputed on the snapshot path,
	// which shares one serialized connection with the engine's writes, so what this
	// costs is a fact worth having in the test output. A threshold here would be a
	// clock-dependent flake, and a flaky test is one that gets deleted later by someone
	// who no longer knows what it was for.
	started := time.Now()
	a := s.Aggregates(ctx)
	t.Logf("six aggregates over %d rows took %v", nDone+nSkipped+nQueued, time.Since(started))

	if a.Outcomes.Err != nil {
		t.Fatalf("outcome counts: %v", a.Outcomes.Err)
	}
	got := bucketsOf(a.Outcomes)
	if got[string(Done)] != nDone || got[string(Skipped)] != nSkipped {
		t.Errorf("outcome counts = %v, want done=%d skipped=%d (the WHOLE table, not the %d-row history view)",
			got, nDone, nSkipped, testHistoryCap)
	}
	if a.Outcomes.Counted != nDone+nSkipped {
		t.Errorf("outcome counted = %d, want %d", a.Outcomes.Counted, nDone+nSkipped)
	}

	if a.SkipsByGuard.Err != nil {
		t.Fatalf("skips by guard: %v", a.SkipsByGuard.Err)
	}
	for g, want := range wantGuards {
		if got := bucketsOf(a.SkipsByGuard)[g]; got != want {
			t.Errorf("skips by guard %q = %d, want %d (counted over every skipped row)", g, got, want)
		}
	}

	if a.SizeRatio.Err != nil {
		t.Fatalf("size ratio: %v", a.SizeRatio.Err)
	}
	if a.SizeRatio.Counted != nDone {
		t.Errorf("size ratio counted = %d, want %d", a.SizeRatio.Counted, nDone)
	}
	closeToF(t, "size ratio mean", a.SizeRatio.Mean, ratioSum/float64(nDone))
	closeToF(t, "size ratio min", a.SizeRatio.Min, ratioLo)
	closeToF(t, "size ratio max", a.SizeRatio.Max, ratioHi)

	if a.EncodeMs.Err != nil {
		t.Fatalf("encode ms: %v", a.EncodeMs.Err)
	}
	closeToF(t, "encode ms mean", a.EncodeMs.Mean, float64(msSum)/float64(nDone))
	closeToF(t, "encode ms min", a.EncodeMs.Min, float64(msLo))
	closeToF(t, "encode ms max", a.EncodeMs.Max, float64(msHi))

	if a.VmafMean.Err != nil || a.VmafMin.Err != nil {
		t.Fatalf("vmaf spreads: mean=%v min=%v", a.VmafMean.Err, a.VmafMin.Err)
	}
	closeToF(t, "vmaf mean spread mean", a.VmafMean.Mean, meanSum/float64(nDone))
	closeToF(t, "vmaf mean spread min", a.VmafMean.Min, vmeanLo)
	closeToF(t, "vmaf mean spread max", a.VmafMean.Max, vmeanHi)
	closeToF(t, "vmaf worst spread mean", a.VmafMin.Mean, minSum/float64(nDone))
	closeToF(t, "vmaf worst spread min", a.VmafMin.Min, vminLo)
	closeToF(t, "vmaf worst spread max", a.VmafMin.Max, vminHi)

	// And every figure states the set it covers, so the number is never published bare.
	for name, cov := range map[string]Coverage{
		"outcomes": a.Outcomes.Coverage, "skips": a.SkipsByGuard.Coverage,
		"size ratio": a.SizeRatio.Coverage, "encode ms": a.EncodeMs.Coverage,
		"vmaf mean": a.VmafMean.Coverage, "vmaf min": a.VmafMin.Coverage,
	} {
		if cov.Set == "" {
			t.Errorf("%s publishes no coverage set - a figure whose set is unstated reads as covering everything", name)
		}
		if cov.Window != "" {
			t.Errorf("%s claims window %q, but it is computed over its whole matching set", name, cov.Window)
		}
	}
}

// A row that recorded no value for an aggregate is EXCLUDED and COUNTED, never read as
// a zero. Reading an absent VMAF as 0 would drag a published fidelity figure down with
// a measurement nobody took; reading an absent size as 0 would invent a 100%-reclaim.
func TestAggregates_ExcludeRowsWithNoRecordedValueAndCountThem(t *testing.T) {
	s := openTest(t)

	src, out, ms := int64(1000), int64(400), int64(2000)
	mean, worst := 96.0, 80.0
	seedTerminal(t, s, "/lib/measured-a.mkv", Done, &Outcome{
		SourceBytes: &src, OutputBytes: &out, EncodeMs: &ms, VmafMean: &mean, VmafMin: &worst,
	})
	src2, out2, ms2 := int64(2000), int64(1000), int64(4000)
	mean2, worst2 := 98.0, 90.0
	seedTerminal(t, s, "/lib/measured-b.mkv", Done, &Outcome{
		SourceBytes: &src2, OutputBytes: &out2, EncodeMs: &ms2, VmafMean: &mean2, VmafMin: &worst2,
	})
	// The shape of every row written before the outcome columns existed: done, and
	// carrying nothing.
	seedTerminal(t, s, "/lib/unmeasured.mkv", Done, nil)

	a := s.Aggregates(context.Background())

	for _, tc := range []struct {
		name string
		sp   Spread
		mean float64
	}{
		{"size ratio", a.SizeRatio, (0.4 + 0.5) / 2},
		{"encode ms", a.EncodeMs, 3000},
		{"vmaf mean", a.VmafMean, 97},
		{"vmaf min", a.VmafMin, 85},
	} {
		if tc.sp.Err != nil {
			t.Fatalf("%s: %v", tc.name, tc.sp.Err)
		}
		if tc.sp.Counted != 2 {
			t.Errorf("%s counted = %d, want 2 (the rows that recorded a value)", tc.name, tc.sp.Counted)
		}
		if tc.sp.Excluded != 1 {
			t.Errorf("%s excluded = %d, want 1 - an excluded row must be REPORTED, not silently dropped", tc.name, tc.sp.Excluded)
		}
		closeToF(t, tc.name+" mean", tc.sp.Mean, tc.mean)
	}

	// A skipped row that recorded no guard is excluded from the breakdown rather than
	// filed under a bucket that would look like a guard which exists.
	seedTerminal(t, s, "/lib/skip-known.mkv", Skipped, &Outcome{Reason: "low-bitrate"})
	seedTerminal(t, s, "/lib/skip-unknown.mkv", Skipped, nil)
	b := s.Aggregates(context.Background()).SkipsByGuard
	if b.Err != nil {
		t.Fatalf("skips by guard: %v", b.Err)
	}
	if b.Counted != 1 || b.Excluded != 1 {
		t.Errorf("skip breakdown counted/excluded = %d/%d, want 1/1", b.Counted, b.Excluded)
	}
	if len(b.Buckets) != 1 || b.Buckets[0].Key != "low-bitrate" {
		t.Errorf("skip buckets = %+v, want exactly the recorded guard", b.Buckets)
	}
}

// When NO row contributed a value, the figure is "no data" - nil, and never a zero or an
// average of zero. A ledger of unmeasured swaps must not read as a ledger of perfectly
// measured zeros.
func TestAggregates_NothingRecordedIsNoDataNotZero(t *testing.T) {
	s := openTest(t)
	for i := 0; i < 3; i++ {
		seedTerminal(t, s, "/lib/old"+strconv.Itoa(i)+".mkv", Done, nil)
	}

	a := s.Aggregates(context.Background())
	for name, sp := range map[string]Spread{
		"size ratio": a.SizeRatio, "encode ms": a.EncodeMs,
		"vmaf mean": a.VmafMean, "vmaf min": a.VmafMin,
	} {
		if sp.Err != nil {
			t.Fatalf("%s: %v", name, sp.Err)
		}
		if sp.Counted != 0 || sp.Excluded != 3 {
			t.Errorf("%s counted/excluded = %d/%d, want 0/3", name, sp.Counted, sp.Excluded)
		}
		if sp.Min != nil || sp.Mean != nil || sp.Max != nil {
			t.Errorf("%s reported %v/%v/%v with nothing recorded - an unmeasured aggregate must be nil, never 0",
				name, sp.Min, sp.Mean, sp.Max)
		}
	}

	// An empty ledger: no buckets at all, and a zero count that says so.
	empty := openTest(t).Aggregates(context.Background())
	if empty.Outcomes.Err != nil || len(empty.Outcomes.Buckets) != 0 || empty.Outcomes.Counted != 0 {
		t.Errorf("empty ledger outcomes = %+v, want no buckets and a zero count", empty.Outcomes)
	}
	if empty.SizeRatio.Mean != nil {
		t.Errorf("empty ledger size ratio mean = %v, want nil", *empty.SizeRatio.Mean)
	}
}

// A skip breakdown is keyed by the GUARD that fired and counted over EVERY skipped row -
// including the ones past the history view's cap, which is where an operator's intuition
// about "which guard is holding my library back" actually lives.
func TestAggregates_SkipBreakdownCoversEverySkippedRow(t *testing.T) {
	s := openTest(t)
	guards := map[string]int{
		"low-bitrate":             120,
		"already-at-target-codec": 60,
		"hardlinked":              25,
		"dolby-vision":            5,
	}
	total := 0
	for g, n := range guards {
		for i := 0; i < n; i++ {
			seedTerminal(t, s, "/lib/"+g+strconv.Itoa(i)+".mkv", Skipped, &Outcome{Reason: g})
			total++
		}
	}
	if total <= testHistoryCap {
		t.Fatalf("seed of %d skipped rows does not exceed the %d-row history cap", total, testHistoryCap)
	}

	b := s.Aggregates(context.Background()).SkipsByGuard
	if b.Err != nil {
		t.Fatalf("skips by guard: %v", b.Err)
	}
	if b.Counted != int64(total) {
		t.Errorf("skip breakdown counted %d of %d skipped rows", b.Counted, total)
	}
	got := bucketsOf(b)
	for g, want := range guards {
		if got[g] != int64(want) {
			t.Errorf("guard %q = %d, want %d", g, got[g], want)
		}
	}
	// Ordered biggest first, so the page's ordering is the store's and does not wobble
	// between snapshots.
	for i := 1; i < len(b.Buckets); i++ {
		if b.Buckets[i-1].Count < b.Buckets[i].Count {
			t.Errorf("buckets are not ordered by count: %+v", b.Buckets)
			break
		}
	}
}

// The constraint this phase was given, pinned as a test: the aggregates must not depend
// on a SQL function whose presence is a property of the BUILD. median() and percentile()
// need SQLite 3.51.0 compiled with -DSQLITE_ENABLE_PERCENTILE (or a loadable extension
// before that), so a query naming one resolves on some builds and fails at runtime on
// others - on a user's image, in the surface whose whole job is to be trustworthy.
//
// This test asks the PINNED build directly whether median() resolves and then asserts
// the aggregates are fully computed either way. Whichever answer the build gives, the
// figures must not depend on it.
func TestAggregates_DoNotDependOnAVersionGatedSQLFunction(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	var v sql.NullFloat64
	err := s.db.QueryRowContext(ctx, `SELECT median(x) FROM (SELECT 1 AS x UNION ALL SELECT 2)`).Scan(&v)
	if err != nil {
		t.Logf("this build has no median(): %v - which is exactly why the aggregates use MIN/AVG/MAX", err)
	} else {
		t.Logf("this build resolves median() (= %v), but the aggregates still must not require it", v.Float64)
	}

	src, out, ms := int64(1000), int64(500), int64(9000)
	mean, worst := 95.5, 77.25
	seedTerminal(t, s, "/lib/one.mkv", Done, &Outcome{
		SourceBytes: &src, OutputBytes: &out, EncodeMs: &ms, VmafMean: &mean, VmafMin: &worst,
	})

	a := s.Aggregates(ctx)
	for name, e := range map[string]error{
		"outcomes": a.Outcomes.Err, "skips": a.SkipsByGuard.Err, "size ratio": a.SizeRatio.Err,
		"encode ms": a.EncodeMs.Err, "vmaf mean": a.VmafMean.Err, "vmaf min": a.VmafMin.Err,
	} {
		if e != nil {
			t.Errorf("%s failed against the pinned SQLite build: %v", name, e)
		}
	}
	closeToF(t, "size ratio mean", a.SizeRatio.Mean, 0.5)
	closeToF(t, "encode ms mean", a.EncodeMs.Mean, 9000)
	closeToF(t, "vmaf mean", a.VmafMean.Mean, 95.5)
	closeToF(t, "vmaf worst", a.VmafMin.Mean, 77.25)
}

// A read failure is reported PER FIGURE, not as one error for the report. Aggregates
// returns no error at all, by design: a single error return is what would let one
// unreadable figure travel up the snapshot path and blank the live page. A failed
// figure must also carry NO value - an unreadable aggregate that reported 0 rows and a
// 0 mean would be indistinguishable from a real, empty ledger.
func TestAggregates_ReportFailurePerFigureAndNeverAsAZero(t *testing.T) {
	s := openTest(t)
	if err := s.Close(); err != nil { // every query below now fails
		t.Fatalf("Close: %v", err)
	}

	a := s.Aggregates(context.Background())
	for name, b := range map[string]Breakdown{"outcomes": a.Outcomes, "skips": a.SkipsByGuard} {
		if b.Err == nil {
			t.Errorf("%s reported no error against a closed database", name)
		}
		if b.Counted != 0 || len(b.Buckets) != 0 {
			t.Errorf("%s reported data (%d/%+v) it could not read", name, b.Counted, b.Buckets)
		}
		if b.Coverage.Set == "" {
			t.Errorf("%s lost its coverage statement on failure", name)
		}
	}
	for name, sp := range map[string]Spread{
		"size ratio": a.SizeRatio, "encode ms": a.EncodeMs,
		"vmaf mean": a.VmafMean, "vmaf min": a.VmafMin,
	} {
		if sp.Err == nil {
			t.Errorf("%s reported no error against a closed database", name)
		}
		if sp.Min != nil || sp.Mean != nil || sp.Max != nil || sp.Counted != 0 {
			t.Errorf("%s published a figure it could not read: %+v", name, sp)
		}
	}
}
