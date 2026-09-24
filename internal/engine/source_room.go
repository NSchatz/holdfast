package engine

import (
	"fmt"
	"sync"

	"github.com/NSchatz/holdfast/internal/diskfree"
)

// The free-space pre-check for a job whose working file is written BESIDE ITS SOURCE
// (no scratch_dir configured), and the account of what the other jobs in flight have
// already claimed on the same filesystem.
//
// Without it a job on a near-full drive encodes for hours, dies at ENOSPC and is parked
// after max_failures. The scratch branch has always asked first (scratchRoomFor); this
// asks the same question of the filesystem the working file actually lands on, which is
// the one holding the source's directory.
//
// The room a job needs is compared against two figures, one MEASURED and one COUNTED,
// and the split is the whole design:
//
//   - Measured: the free-space lookup's figure for the source's directory, taken as it
//     stands. A retained original in the undo window is a second link to blocks that are
//     already allocated, so that figure already excludes it. Nothing is added back for
//     it, nor for anything a pending swap or release would free: a swap frees nothing
//     while the window holds the original.
//   - Counted: the source sizes of the OTHER jobs that passed this check on the same
//     filesystem and have not yet exited. A statfs cannot see bytes an encode is about to
//     write, so two workers asking at once would otherwise both pass on the same free
//     bytes. What such a job has already written is counted twice (the statfs sees it
//     too), which errs toward refusing and never toward ENOSPC.
//
// The source's size is the bar for the output, exactly as in the scratch check: gate 4
// accepts only a strictly smaller output, so a filesystem with room for the source has
// room for any output holdfast would swap in, and no output size exists before the
// encode.

// unknownFilesystem is the identity every job whose filesystem cannot be named holds
// under. They all share it, which fails toward counting: two such jobs on one device
// can never forget each other, and two on different devices merely count each other
// when they need not. The named identities carry a prefix, so none can equal it.
const unknownFilesystem = "unknown"

// roomHolds is the counted half: the bytes the in-flight jobs that passed the
// beside-the-source check hold against their filesystem. The zero value is ready.
//
// It lives on the Engine, so a `serve` process that keeps one engine across passes keeps
// one account across them too. That is why every hold is released on every way out of
// its job: a hold that leaked would refuse work on that filesystem until a restart.
type roomHolds struct {
	mu   sync.Mutex
	held map[string]uint64 // filesystem identity -> bytes held by in-flight jobs on it
}

// release gives back one job's hold.
func (r *roomHolds) release(fs string, size uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.held[fs] -= size
	if r.held[fs] == 0 {
		delete(r.held, fs)
	}
}

// filesystemOf names the filesystem holding dir, falling back to the shared unknown
// identity when it cannot be named.
//
// The fallback is recorded at info and not warn. The job itself proceeds on exactly the
// terms it would have otherwise; only the account is coarser, and in the direction that
// refuses sooner. The same failure usually breaks the free-space lookup too, and that one
// already says, at warn, that this job runs unchecked.
func (e *Engine) filesystemOf(dir string) string {
	id, err := diskfree.ID(dir)
	if err != nil {
		e.Log.Info("could not establish which filesystem holds this directory, so its jobs are counted "+
			"together with every other job whose filesystem could not be established",
			"dir", dir, "err", err)
		return unknownFilesystem
	}
	return id
}

// sourceRoomFor refuses a job whose source will not fit beside itself, BEFORE the encoder
// writes a byte, and otherwise holds the source's size against the filesystem until the
// returned release runs. The caller runs it on every way out of the job.
//
// The comparison and the hold are one step under one lock, so two workers cannot both
// pass on the same free bytes. The lookup is taken OUTSIDE the lock: a statfs on a
// network mount can hang, and one hung mount must not stall every other worker's check.
// A figure measured a moment before the lock is taken is as current as one measured
// inside it, because what the lock protects is the count, and the count is read inside.
//
// A lookup that FAILS does not fail the job, for the reason scratchRoomFor gives: a write
// that then fails is an ordinary encode failure, which leaves the source untouched. The
// job still holds its bytes, because it is still going to write them.
func (e *Engine) sourceRoomFor(dir, source string, sourceBytes int64) (release func(), err error) {
	var size uint64
	if sourceBytes > 0 {
		size = uint64(sourceBytes)
	}
	fs := e.filesystemOf(dir)
	avail, lookupErr := e.free(dir)
	if lookupErr != nil {
		e.Log.Warn("could not establish the free space beside the source, so this job continues without the "+
			"pre-check (a write that fails there is an ordinary encode failure, and leaves the source untouched)",
			"dependency", "free-space lookup", "attempted", "statfs of the source's directory", "dir", dir,
			"file", source, "err", lookupErr, "next", "continue with the encode")
	}

	e.room.mu.Lock()
	defer e.room.mu.Unlock()
	others := e.room.held[fs]
	if need := size + others; lookupErr == nil && avail < need {
		return nil, fmt.Errorf("not enough room beside the source in %s for %s: %d byte(s) available on that filesystem, "+
			"%d byte(s) needed (the source's %d byte(s) plus %d byte(s) held by other jobs in flight there). "+
			"Free space on that filesystem to let it through",
			dir, source, avail, need, size, others)
	}
	if e.room.held == nil {
		e.room.held = make(map[string]uint64)
	}
	e.room.held[fs] += size
	var once sync.Once
	return func() { once.Do(func() { e.room.release(fs, size) }) }, nil
}
