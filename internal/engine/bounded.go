package engine

// The bounded run (S0100): one named file, or a count of decisions.
//
// `holdfast run` had exactly one shape - scan every configured root and act on everything
// it finds - so the first act an operator takes with a delete-capable tool was to aim it
// at the whole library. A bound is how a person watches the real pipeline decide a real
// file, every guard, gate and swap intact, before handing it the library.
//
// What a bound is NOT is a rehearsal. The per-file pipeline is the identical one: the
// door, the hold-backs, the profile resolution, the guards, the claim, the encode, every
// gate and the swap, all through ProcessFile, which is the only door into any of it. A
// bounded run therefore destroys a source exactly as a full run can, one file at a time,
// which is the point of it.
//
// Three things a bound changes, and nothing else:
//
//   - WHAT IS OFFERED. A file bound offers exactly the named path; a count bound offers
//     until that many files have reached a terminal outcome.
//   - THE WHOLE-LIBRARY PASSES ARE NOT RUN AS THEY ARE ON A SCAN. The ledger retention pass
//     decides a row's file is gone, reasoning from "this pass listed the whole library",
//     which a bounded pass did not, and its mistake is an irreversible delete of audit
//     history that no re-run restores - so a bounded run does not run it at all. The
//     whole-library stale-temp sweep reasons the same way, deciding a temp is orphaned on
//     finding it in a listing, and a bounded run does not run that either. What it runs
//     instead is the OWNER-CHECKED sweep: before any file is offered it removes each temp
//     whose owner record (tempowner.go) shows its owner provably dead, found from those
//     records rather than from a listing, so a bounded run killed by SIGKILL or the OOM
//     killer leaves nothing a later bounded run cannot reach. An owner record is evidence
//     about the TEMP, not about the library, which is why the old reason for skipping the
//     sweep does not apply to it. A temp whose owner it cannot prove dead - no record, as
//     every temp an older build wrote has none; a record it cannot read; storage where a
//     lock is not evidence - it leaves, and names at `warn` wherever it sees one.
//   - IT SAYS SO. Which bound, its value, what became of the stale-temp sweep and that the
//     retention pass was skipped, as structured fields, because "the run I just watched did
//     less than a scan does" is exactly the thing an operator must not have to infer.

import (
	"context"
	"errors"
	"os"
	"sync"
)

// Bound is the bound on one run: a single named file, a count of terminal outcomes, or
// neither, which is an ordinary whole-library pass.
//
// The two are not exclusive and the file wins. A caller that passes both has asked for one
// file and for at most N of them, and one file satisfies both readings; treating the
// combination as an invocation error would refuse a caller that asked for nothing
// contradictory.
type Bound struct {
	// File is the ONE path this run carries to a terminal outcome, already judged
	// eligible by the caller and in the resolved form the pipeline is keyed on. Empty is
	// no file bound.
	File string

	// Limit is how many files may reach a terminal outcome in this run. Zero or less is
	// no count bound.
	Limit int
}

// bounded reports whether this run is bounded at all, which is the question the
// whole-library passes turn on.
func (b Bound) bounded() bool { return b.File != "" || b.Limit > 0 }

// RunBounded is RunOneshot under a bound. An unbounded Bound runs the ordinary pass, so a
// caller that has not decided yet has one function to call rather than a branch to write.
func (e *Engine) RunBounded(ctx context.Context, b Bound) error { return e.runPass(ctx, b) }

// staleTempSweepOwnerChecked is the bounded-run report's value for its stale-temp sweep: it
// runs, and removes only a temp whose recorded owner is provably dead.
const staleTempSweepOwnerChecked = "owner-checked"

// reportBound states what this pass is bounded by and what that costs, as fields rather
// than as a sentence, before any of it happens (AC-11, observability O1/O3). It is `info`:
// nothing here needs a human to act, and a bounded run is the operator's own request. It
// comes before the sweep; the sweep's own records say what it removed.
func (e *Engine) reportBound(b Bound) {
	which, value := "limit", any(b.Limit)
	if b.File != "" {
		which, value = "file", any(b.File)
	}
	sweep, sweepRemoves := staleTempSweepOwnerChecked, "only temps whose recorded owner is provably dead"
	if e.owners == nil {
		sweep, sweepRemoves = "skipped", "nothing: this engine keeps no owner records, so no temp's owner can be proved dead"
	}
	e.Log.Info("bounded run: this pass carries a bound, so it does not list the whole library",
		"bounded", true,
		"bound", which,
		"bound_value", value,
		"stale_temp_sweep", sweep,
		"stale_temp_sweep_removes", sweepRemoves,
		"ledger_retention_pass", "skipped",
		"why_skipped", "this pass did not list the whole library, and the retention pass concludes "+
			"from an absence that only a whole-library pass is evidence for")
}

// sweepOrphanedTemps is a bounded run's stale-temp sweep, and it runs before any file is
// offered. It reads the owner records rather than the library: each record whose owner is
// provably dead has its temp removed - the attached-picture files named after it first -
// unless a hold-back applies, and every removal is recorded with its path (cli L6). A
// record whose owner is alive, or that cannot be decided, leaves its temp where it is and
// names it. It returns the temps it decided, so the enumeration does not decide them again.
func (e *Engine) sweepOrphanedTemps(ctx context.Context) map[string]bool {
	decided := map[string]bool{}
	if e.owners == nil {
		return decided
	}
	records, err := e.owners.ownerRecords()
	if err != nil {
		e.Log.Warn("the owner records could not be listed, so this bounded run removes no temp",
			"owner_records", e.owners.dir, "operation", "list the owner-record directory", "err", err,
			"next", "every temp stays where it is for a later bounded run or an unbounded pass")
		return decided
	}
	removed := 0
	for _, rec := range records {
		if ctx.Err() != nil {
			break
		}
		v := e.owners.inspect(rec, "")
		temp := v.temp()
		if v.state == ownerNoRecord {
			// Cleared between the listing and the open: its owner finished with it.
			continue
		}
		if temp == "" {
			e.Log.Warn("leaving an owner record, and any temp it names, in place: its owner is not provably dead",
				"owner_record", rec, "why", v.why, "sweep", string(sweepBounded))
			v.close(false)
			continue
		}
		pictures := picturesBeside(temp)
		for _, p := range pictures {
			decided[p] = true
		}
		decided[temp] = true
		if v.state != ownerDead {
			// Named only where there is a temp to name: a live owner's record is written a
			// moment before its temp exists.
			for _, p := range append(pictures, temp) {
				if _, err := os.Lstat(p); err == nil {
					e.decideTemp(ctx, p, sweepBounded, v)
				}
			}
			v.close(false)
			continue
		}
		for _, p := range pictures {
			if r, _ := e.decideTemp(ctx, p, sweepBounded, v); r {
				removed++
			}
		}
		r, gone := e.decideTemp(ctx, temp, sweepBounded, v)
		if r {
			removed++
		}
		v.close(gone)
	}
	e.Log.Info("bounded run: the owner-checked stale-temp sweep is done",
		"owner_records", len(records), "temps_removed", removed)
	return decided
}

// processOne carries a single named path to a terminal outcome, through the same exported
// door a scan's worker and a targeted submission both use.
//
// Nothing is enumerated. The path was judged eligible before the run started - the same
// rules the enumeration applies, asked of a path instead of a listing entry - and
// everything the enumeration would still have said about it lives on the door itself:
// ProcessFile re-checks the hold-backs, refuses a retained replacement by name, and takes
// the claim. So the outcome recorded here is the outcome an unbounded pass over the same
// tree and the same configuration records, because it is reached by the same code.
//
// The worker label is the one a single-worker scan hands out, so a row this run writes is
// attributed exactly as a scan's would be rather than under a name only this path uses.
func (e *Engine) processOne(ctx context.Context, path string) error {
	e.Log.Info("bounded run: carrying one file to a terminal outcome", "file", path)
	if err := e.ProcessFile(ctx, "w0", path); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		// Same treatment a scan gives it: every per-file outcome is recorded inside
		// ProcessFile, so anything returned here is unexpected rather than a verdict.
		e.Log.Warn("process file error", "file", path, "err", err)
	}
	return nil
}

// budget is a count bound while a pass is running: how many files may still reach a
// terminal outcome, and how many are being decided right now.
//
// It counts what was RECORDED, never what was offered, because the criterion counts
// decisions: a file the claim turns away because a terminal row already holds it recorded
// nothing, took no decision and must not spend the bound. And it counts what is IN FLIGHT
// beside it, because a file handed to a worker is a terminal outcome this pass has already
// committed to - a bound that only counted completions would be overshot by every worker
// holding a file when the last slot was recorded.
//
// A nil *budget is an unbounded pass: every method answers so, and none of them blocks.
type budget struct {
	e     *Engine
	limit int

	// decided is every temp this bounded pass has already decided, so the enumeration does
	// not decide or name one twice: the owner-record sweep's before the scan began, and
	// each one the enumeration then lists. It is only touched on the enumeration's own
	// goroutine, after the sweep has returned.
	decided map[string]bool
	// start is the engine's terminal count when this pass began, so the pass measures
	// ITSELF rather than the lifetime of the process.
	start int64

	mu       sync.Mutex
	cond     *sync.Cond
	inFlight int
}

// budgetFor builds this pass's count bound, or nil where there is none.
func (e *Engine) budgetFor(b Bound) *budget {
	if b.Limit <= 0 {
		return nil
	}
	bud := &budget{e: e, limit: b.Limit, start: e.terminal.Load()}
	bud.cond = sync.NewCond(&bud.mu)
	return bud
}

// recorded is how many terminal outcomes THIS pass has recorded so far.
func (b *budget) recorded() int64 { return b.e.terminal.Load() - b.start }

// met reports that the bound is REACHED: this pass has recorded its full count. It only
// ever becomes true, so it is safe to stop an enumeration on, and it never blocks - it is
// the question asked before each directory is listed, where waiting would be paying for
// nothing.
func (b *budget) met() bool {
	if b == nil {
		return false
	}
	return b.recorded() >= int64(b.limit)
}

// admit reports whether this pass may offer another file, taking a slot when it may.
//
// It BLOCKS in exactly one state: the bound is not yet recorded, and every remaining slot
// is with a file being decided right now. Waiting there is what makes the two halves of
// the criterion hold together - the pass records no more than the bound, and a library
// holding fewer files than the bound is still processed to the end - because a file in
// flight either records an outcome (spending its slot) or records none (handing it back),
// and which of those it did is not knowable until it returns. Every slot is released by
// the worker that took it, so this wait is always woken.
func (b *budget) admit() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for {
		recorded := b.recorded()
		if recorded >= int64(b.limit) {
			return false
		}
		if recorded+int64(b.inFlight) < int64(b.limit) {
			b.inFlight++
			return true
		}
		b.cond.Wait()
	}
}

// reportReached states what the count bound actually reached, once the pass is over.
//
// It is `info` WHETHER OR NOT the bound was met, and that is the criterion rather than a
// preference (AC-5, observability O3): a library holding fewer files than the bound has
// been processed completely, so the run is complete and no human has to act. A record at
// `error` there would train an operator to ignore the level that means they must.
func (b *budget) reportReached() {
	if b == nil {
		return
	}
	got := b.recorded()
	e := b.e
	if got >= int64(b.limit) {
		e.Log.Info("bounded run: the bound was reached", "bound", "limit", "bound_value", b.limit,
			"terminal_outcomes_recorded", got)
		return
	}
	e.Log.Info("bounded run: every eligible file was processed before the bound was reached, "+
		"which is a complete run over a library holding fewer of them than the bound asked for",
		"bound", "limit", "bound_value", b.limit, "terminal_outcomes_recorded", got,
		"bound_reached", false)
}

// release hands a slot back once the file that took it has been decided. The broadcast is
// what wakes an enumeration waiting in admit.
func (b *budget) release() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.inFlight--
	b.mu.Unlock()
	b.cond.Broadcast()
}
