package engine

import (
	"context"
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
// The same account is taken at the scratch pre-check (scratchRoomFor), against the scratch
// filesystem, so both per-job checks count what the other jobs in flight on the filesystem
// they write to have reserved (S0163).
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

// roomHolds is the counted half: the bytes the in-flight jobs that passed a per-job
// free-space check hold against the filesystem their working file is written on - the
// scratch filesystem when scratch_dir is set, the source's own otherwise (S0163). Both
// checks keep one account, so a scratch directory on a library's filesystem counts the jobs
// encoding beside their sources there, and they count it. The zero value is ready.
//
// It lives on the Engine, so a `serve` process that keeps one engine across passes keeps
// one account across them too. That is why every hold is released on every way out of
// its job: a hold that leaked would hold every later job on that filesystem waiting until
// a restart.
type roomHolds struct {
	mu   sync.Mutex
	held map[string]uint64 // filesystem identity -> bytes held by in-flight jobs on it
	// freed is closed, and forgotten, whenever a hold is given back, so every job waiting
	// for room wakes and checks again.
	freed chan struct{}
}

// release gives back one job's hold and wakes every job waiting for room.
func (r *roomHolds) release(fs string, size uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.held[fs] -= size
	if r.held[fs] == 0 {
		delete(r.held, fs)
	}
	if r.freed != nil {
		close(r.freed)
		r.freed = nil
	}
}

// filesystemOf names the filesystem holding dir, falling back to the shared unknown
// identity when it cannot be named.
//
// The fallback is recorded at info and not warn. The job itself proceeds on exactly the
// terms it would have otherwise; only the account is coarser, and in the direction that
// counts sooner. The same failure usually breaks the free-space lookup too, and that one
// already says, at warn, that this job runs unchecked.
func (e *Engine) filesystemOf(dir string) string {
	read := diskfree.ID
	if e.fsID != nil {
		read = e.fsID
	}
	id, err := read(dir)
	if err != nil {
		e.Log.Info("could not establish which filesystem holds this directory, so its jobs are counted "+
			"together with every other job whose filesystem could not be established",
			"dir", dir, "err", err)
		return unknownFilesystem
	}
	return id
}

// roomCheck is one per-job free-space check: the directory the job's working file is
// written in, and the words its refusal and its failed lookup are said in.
type roomCheck struct {
	dir string
	// refuse is the failure for a source that does not fit even with nothing reserved.
	refuse func(avail, size uint64) error
	// lookupFailed says, once per job, that the free space could not be established.
	lookupFailed func(err error)
}

// reserveRoom is the reservation both per-job checks take, BEFORE the encoder writes a byte:
// it refuses a job whose source will not fit in the free space the check reads, waits while
// it fits only in that space less what the jobs already in flight on the same filesystem
// have reserved, and otherwise holds the source's size against that filesystem until the
// returned release runs. The caller runs it on every way out of the job.
//
// The comparison and the hold are one step under one lock, so two workers cannot both
// pass on the same free bytes. The lookup is taken OUTSIDE the lock: a statfs on a
// network mount can hang, and one hung mount must not stall every other worker's check.
// A figure measured a moment before the lock is taken is as current as one measured
// inside it, because what the lock protects is the count, and the count is read inside.
//
// WAITING, NOT FAILING, is the ruling for contention (S0163 D3). A source that fits in the
// free space but not beside the other jobs' reservations is not a file that cannot be done:
// a sibling finishing clears the condition, and a failure would count toward max_failures
// and could park the file for a state that lasted minutes. So the job is held without
// encoding and without a row written, says so once at info (observability O5), and checks
// again - re-reading the free space - each time a hold is given back. A cancelled pass
// returns it with the context's error, which records nothing against the file. A source
// that does not fit even with nothing reserved fails exactly as it always did, because no
// sibling finishing will ever make room for it.
//
// A lookup that FAILS does not fail the job: a write that then fails is an ordinary encode
// failure, which leaves the source untouched. The job still holds its bytes, because it is
// still going to write them.
func (e *Engine) reserveRoom(ctx context.Context, source string, sourceBytes int64, c roomCheck) (release func(), err error) {
	var size uint64
	if sourceBytes > 0 {
		size = uint64(sourceBytes)
	}
	fs := e.filesystemOf(c.dir)
	warned, waited := false, false
	for {
		avail, lookupErr := e.free(c.dir)
		if lookupErr != nil && !warned {
			warned = true
			c.lookupFailed(lookupErr)
		}

		e.room.mu.Lock()
		others := e.room.held[fs]
		if lookupErr == nil && avail < size {
			e.room.mu.Unlock()
			return nil, c.refuse(avail, size)
		}
		// avail >= size here, so avail-size cannot wrap.
		if lookupErr == nil && avail-size < others {
			if e.room.freed == nil {
				e.room.freed = make(chan struct{})
			}
			freed := e.room.freed
			e.room.mu.Unlock()
			if !waited {
				waited = true
				e.Log.Info("waiting for room: this job's source fits in the free space its filesystem reports, "+
					"but not beside what the jobs already in flight there have reserved, so it is held without "+
					"encoding until one of them ends, and then checked again",
					"file", source, "dir", c.dir, "filesystem", fs, "free_bytes", avail,
					"reserved_bytes", others, "source_bytes", size)
				if e.hookRoomWait != nil {
					e.hookRoomWait(source)
				}
			}
			select {
			case <-freed:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if e.room.held == nil {
			e.room.held = make(map[string]uint64)
		}
		e.room.held[fs] += size
		e.room.mu.Unlock()
		var once sync.Once
		return func() { once.Do(func() { e.room.release(fs, size) }) }, nil
	}
}

// sourceRoomFor is the check for a job whose working file goes BESIDE ITS SOURCE, taken
// against the filesystem holding the source's directory (see reserveRoom).
func (e *Engine) sourceRoomFor(ctx context.Context, dir, source string, sourceBytes int64) (release func(), err error) {
	return e.reserveRoom(ctx, source, sourceBytes, roomCheck{
		dir: dir,
		refuse: func(avail, size uint64) error {
			return fmt.Errorf("not enough room beside the source in %s for %s: %d byte(s) available on that "+
				"filesystem, the source is %d byte(s). Free space on that filesystem to let it through",
				dir, source, avail, size)
		},
		lookupFailed: func(err error) {
			e.Log.Warn("could not establish the free space beside the source, so this job continues without the "+
				"pre-check (a write that fails there is an ordinary encode failure, and leaves the source untouched)",
				"dependency", "free-space lookup", "attempted", "statfs of the source's directory", "dir", dir,
				"file", source, "err", err, "next", "continue with the encode")
		},
	})
}
