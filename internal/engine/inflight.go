package engine

import (
	"context"
	"path/filepath"
	"sync"

	"github.com/NSchatz/holdfast/internal/store"
)

// What the jobs in flight in THIS PROCESS hold, so that several of them on one drive cannot
// act on one another's files (S0163).
//
// A working file beside a source is named by the source's STEM and the OUTPUT container's
// extension (tempPath). Two sources in one directory that share a stem and encode to one
// container - `ep.mp4` and `ep.avi` under `container_ext: mkv` - therefore construct the
// same name, and a picker that cleared whatever sat at its candidate would remove the other
// job's file while that job's encoder was still writing it. Every extra worker is another
// concurrent transcode-and-delete pipeline, and a job whose gates measured a file its own
// encoder did not write is the one outcome here that no gate can catch.
//
// The same two sources share a swap TARGET too (`ep.mkv`). The pre-swap check that nothing
// is at the target and the rename that puts something there are two steps, and two jobs
// between them at once would both pass the check; the second rename would then replace the
// first job's replacement after the first job had removed its source.
//
// Both sets are PROCESS-wide rather than per Engine: a working file and a target are
// filesystem objects, and two engines in one process share the filesystem. Another PROCESS
// is kept off a working file by its owner record instead (tempowner.go): pickTempPath skips a
// candidate whose record a live owner holds.

// workingPaths is the set of working paths the jobs in flight in this process have picked.
var workingPaths = struct {
	mu   sync.Mutex
	held map[string]bool
}{held: map[string]bool{}}

// holdWorkingPath claims path for the job that picked it and reports false when another job
// in flight already holds it. The claim lasts until releaseWorkingPath.
func holdWorkingPath(path string) bool {
	path = filepath.Clean(path)
	workingPaths.mu.Lock()
	defer workingPaths.mu.Unlock()
	if workingPaths.held[path] {
		return false
	}
	workingPaths.held[path] = true
	return true
}

// releaseWorkingPath gives a picked working path back. The job calls it on every way out,
// after whatever that way did to the file: removed it, renamed it onto the source, or left
// it orphaned for a sweep.
func releaseWorkingPath(path string) {
	workingPaths.mu.Lock()
	defer workingPaths.mu.Unlock()
	delete(workingPaths.held, filepath.Clean(path))
}

// workingPathHeld reports whether a job in flight in this process holds path.
func workingPathHeld(path string) bool {
	workingPaths.mu.Lock()
	defer workingPaths.mu.Unlock()
	return workingPaths.held[filepath.Clean(path)]
}

// targetLock is one swap target's lock and the number of jobs holding or waiting for it.
type targetLock struct {
	mu   sync.Mutex
	refs int
}

// swapTargets holds the locks of the swap targets jobs in this process are between their
// pre-swap target check and the end of their swap. An entry exists only while a job holds
// or waits for it, so the map never grows with the library.
var swapTargets = struct {
	mu    sync.Mutex
	locks map[string]*targetLock
}{locks: map[string]*targetLock{}}

// lockSwapTarget serializes, per target path, the stretch from the pre-swap check that no
// file sits at final to the end of the swap that puts one there, and returns the unlock. A
// job that finds another job inside that stretch for the same target waits for it; its own
// check then sees the other job's replacement and refuses to clobber it.
//
// It is taken only for a container-changing swap (final != source): an in-place swap's
// target is its own source, which the claim already gives to one job at a time.
func (e *Engine) lockSwapTarget(final string) (unlock func()) {
	final = filepath.Clean(final)
	swapTargets.mu.Lock()
	l := swapTargets.locks[final]
	if l == nil {
		l = &targetLock{}
		swapTargets.locks[final] = l
	}
	l.refs++
	swapTargets.mu.Unlock()

	if !l.mu.TryLock() {
		e.Log.Info("waiting for another job in flight to finish its swap onto the same target "+
			"before checking that target", "target", final)
		if e.hookTargetWait != nil {
			e.hookTargetWait(final)
		}
		l.mu.Lock()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Unlock()
			swapTargets.mu.Lock()
			if l.refs--; l.refs == 0 {
				delete(swapTargets.locks, final)
			}
			swapTargets.mu.Unlock()
		})
	}
}

// liveClaims counts, per store, the jobs of this process that are past a successful claim
// and have not yet returned. A pass starts by resetting the ledger rows a previous process
// left active (Store.RecoverStale), and a row a LIVE job holds is active too: resetting it
// would hand that job's source to the next claimant while its first job is still encoding
// it. Every pool of a daemon - the scan, the submission queue, the watch - and every engine in
// one process can be mid-job when a pass starts, so the reset waits for a pass that starts
// with none of them in flight on its store.
//
// gate closes the window between the count and the reset: every claim is taken holding it
// for reading, and the reset decides holding it for writing, so no claim lands between the
// decision and the UPDATE.
var liveClaims = struct {
	gate sync.RWMutex
	mu   sync.Mutex
	live map[store.Store]int
}{live: map[store.Store]int{}}

// takeClaim runs claim against the engine's store under the claim gate and, where it
// claims, counts the job live until the returned leave runs.
func (e *Engine) takeClaim(claim func(st store.Store) (bool, error)) (claimed bool, leave func(), err error) {
	st := e.Store
	liveClaims.gate.RLock()
	defer liveClaims.gate.RUnlock()
	claimed, err = claim(st)
	if err != nil || !claimed {
		return claimed, func() {}, err
	}
	liveClaims.mu.Lock()
	liveClaims.live[st]++
	liveClaims.mu.Unlock()
	var once sync.Once
	return true, func() {
		once.Do(func() {
			liveClaims.mu.Lock()
			defer liveClaims.mu.Unlock()
			if liveClaims.live[st]--; liveClaims.live[st] <= 0 {
				delete(liveClaims.live, st)
			}
		})
	}, nil
}

// recoverStale resets the rows a previous process left active - the mark of a crashed run -
// unless jobs of THIS process are in flight on the same store, whose rows are theirs. The
// rows it leaves are reset by the first pass that starts with nothing in flight; until then
// Claim reads them as held, which only ever delays a file and never admits one twice.
func (e *Engine) recoverStale(ctx context.Context) {
	liveClaims.gate.Lock()
	defer liveClaims.gate.Unlock()
	liveClaims.mu.Lock()
	n := liveClaims.live[e.Store]
	liveClaims.mu.Unlock()
	if n > 0 {
		e.Log.Info("not resetting active ledger rows at the start of this pass: jobs of this process are "+
			"in flight on this ledger and their rows are theirs; a pass that starts with none in flight resets "+
			"what a previous process left", "jobs_in_flight", n)
		return
	}
	if _, err := e.Store.RecoverStale(ctx); err != nil {
		// Fail safe: a stuck "active" row means one file is skipped this pass (Claim treats
		// it as held), never a false completion, so log and continue.
		e.Log.Warn("recover stale jobs failed (continuing)", "err", err)
	}
}
