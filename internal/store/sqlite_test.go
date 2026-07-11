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
	if err := s.Finish(ctx, "/a/movie.mkv", "fp1", Done); err != nil {
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
	if err := s.Finish(ctx, "/a/movie.mkv", "fp1", Skipped); err != nil {
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
		if err := s.Finish(ctx, "/a/movie.mkv", "fp1", Failed); err != nil {
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
	if err := s.Finish(ctx, "/a/done.mkv", "fp1", Done); err != nil {
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
				if err := s.Finish(ctx, path, fp, Done); err != nil {
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
