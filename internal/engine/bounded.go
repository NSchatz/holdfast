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
	"strconv"
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

	// LimitEncodes is how many files may REACH AN ENCODE in this run (S0174): the job
	// entered the encoding state, or, under dry_run, was recorded as would-transcode. A
	// skip, a hold-back, a claim refusal or a free-space refusal before the encoder does
	// not spend it. Zero or less is no encode bound. It and Limit are both upper limits,
	// and whichever is reached first stops the offer.
	LimitEncodes int
}

// bounded reports whether this run is bounded at all, which is the question the
// whole-library passes turn on.
func (b Bound) bounded() bool { return b.File != "" || b.Limit > 0 || b.LimitEncodes > 0 }

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
	which, value := boundName(b)
	// A run carrying BOTH count bounds also gives each its own field, so either is read
	// off the one record without parsing the joined name (S0174 AC-8, AC-10). A run carrying
	// one gives none, so a `--limit` record is the record it always was.
	counts := []any{}
	if b.File == "" && b.Limit > 0 && b.LimitEncodes > 0 {
		counts = append(counts, boundLimit, b.Limit, boundLimitEncodes, b.LimitEncodes)
	}
	sweep, sweepRemoves := staleTempSweepOwnerChecked, "only temps whose recorded owner is provably dead"
	if e.owners == nil {
		sweep, sweepRemoves = "skipped", "nothing: this engine keeps no owner records, so no temp's owner can be proved dead"
	}
	fields := []any{
		"bounded", true,
		"bound", which,
		"bound_value", value,
		"stale_temp_sweep", sweep,
		"stale_temp_sweep_removes", sweepRemoves,
		"ledger_retention_pass", "skipped",
		"why_skipped", "this pass did not list the whole library, and the retention pass concludes " +
			"from an absence that only a whole-library pass is evidence for",
	}
	e.Log.Info("bounded run: this pass carries a bound, so it does not list the whole library",
		append(fields, counts...)...)
}

// The names a bound record gives each bound. `limit` counts terminal outcomes and
// `limit_encodes` counts files that reached an encode (S0174).
const (
	boundLimit        = "limit"
	boundLimitEncodes = "limit_encodes"
)

// boundName is the `bound` and `bound_value` fields of the bound record. A file bound wins
// over both counts, exactly as it wins in the pass. A run carrying both counts names both,
// joined with a comma in the order `limit,limit_encodes`, with their values joined the
// same way; a run carrying one names that one alone, so a `--limit` record reads exactly as
// it did before the encode bound existed.
func boundName(b Bound) (string, any) {
	switch {
	case b.File != "":
		return "file", b.File
	case b.Limit > 0 && b.LimitEncodes > 0:
		return boundLimit + "," + boundLimitEncodes, strconv.Itoa(b.Limit) + "," + strconv.Itoa(b.LimitEncodes)
	case b.LimitEncodes > 0:
		return boundLimitEncodes, b.LimitEncodes
	default:
		return boundLimit, b.Limit
	}
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
		pictures := append(picturesBeside(temp), subtitleTempsBeside(temp)...)
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
			if e.decideTemp(ctx, p, sweepBounded, v) {
				removed++
			}
		}
		// The record goes with its temp. A record whose temp is not there is left exactly
		// as it is: nothing is removed on its account, and the next job to pick that path
		// takes it over.
		r := e.decideTemp(ctx, temp, sweepBounded, v)
		if r {
			removed++
		}
		v.close(r)
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
	release, err := e.holdLocalSlot(ctx, false)
	if err != nil {
		return err
	}
	defer release()
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

// budget is a pass's count bounds while it is running: how many files may still reach a
// terminal outcome (Limit), how many may still reach an encode (LimitEncodes, S0174), and
// how many are being decided right now.
//
// The terminal bound counts what was RECORDED, never what was offered, because the
// criterion counts decisions: a file the claim turns away because a terminal row already
// holds it recorded nothing, took no decision and must not spend the bound. And it counts
// what is IN FLIGHT beside it, because a file handed to a worker is a terminal outcome this
// pass has already committed to - a bound that only counted completions would be overshot
// by every worker holding a file when the last slot was recorded.
//
// The encode bound is the same admission over a different event: a file in flight that has
// not yet reached an encode MIGHT reach one, so it holds a slot until it either does (and
// its slot becomes a spent encode) or returns without one (and its slot is handed back).
// Which file reached an encode is known per file, from the slot it carries on its context
// (see track and reachedEncode), so an encode that is still running is counted once - as
// spent - and never a second time as merely in flight, which would serialise the pool on
// every running encode for no gain in exactness.
//
// A nil *budget is an unbounded pass: every method answers so, and none of them blocks.
type budget struct {
	e     *Engine
	limit int
	// limitEncodes is the encode bound; zero is none.
	limitEncodes int

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
	// encodes is how many files of THIS pass have reached an encode, in flight or
	// returned; inFlightEncoded is how many of the in-flight ones already have.
	encodes         int
	inFlightEncoded int
}

// budgetFor builds this pass's count bounds, or nil where there is none.
func (e *Engine) budgetFor(b Bound) *budget {
	if b.Limit <= 0 && b.LimitEncodes <= 0 {
		return nil
	}
	bud := &budget{e: e, start: e.terminal.Load()}
	if b.Limit > 0 {
		bud.limit = b.Limit
	}
	if b.LimitEncodes > 0 {
		bud.limitEncodes = b.LimitEncodes
	}
	bud.cond = sync.NewCond(&bud.mu)
	return bud
}

// recorded is how many terminal outcomes THIS pass has recorded so far.
func (b *budget) recorded() int64 { return b.e.terminal.Load() - b.start }

// met reports that a bound is REACHED: this pass has recorded its full count of terminal
// outcomes, or carried its full count of files to an encode. It only ever becomes true, so
// it is safe to stop an enumeration on, and it never waits on a file - it is the question
// asked before each directory is listed, where waiting would be paying for nothing.
func (b *budget) met() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.metLocked()
}

func (b *budget) metLocked() bool {
	return b.limitMet() || b.encodesMetLocked()
}

// limitMet is the terminal bound reached; false where there is no terminal bound.
func (b *budget) limitMet() bool { return b.limit > 0 && b.recorded() >= int64(b.limit) }

// encodesMetLocked is the encode bound reached; false where there is no encode bound.
func (b *budget) encodesMetLocked() bool { return b.limitEncodes > 0 && b.encodes >= b.limitEncodes }

// roomLocked reports whether one more file can be handed out without either bound being
// overshot whatever every file in flight turns out to do.
func (b *budget) roomLocked() bool {
	if b.limit > 0 && b.recorded()+int64(b.inFlight) >= int64(b.limit) {
		return false
	}
	if b.limitEncodes > 0 && b.encodes+(b.inFlight-b.inFlightEncoded) >= b.limitEncodes {
		return false
	}
	return true
}

// admit reports whether this pass may offer another file, taking a slot when it may.
//
// It BLOCKS in exactly one state: no bound is yet reached, and every remaining slot of a
// bound is with a file being decided right now. Waiting there is what makes the two halves
// of the criterion hold together - the pass spends no more than the bound, and a library
// holding fewer files than the bound is still processed to the end - because a file in
// flight either spends its slot (records an outcome, reaches an encode) or hands it back,
// and which of those it did is not knowable until it does. Every slot is released by the
// worker that took it, and every encode reached broadcasts, so this wait is always woken.
func (b *budget) admit() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for {
		if b.metLocked() {
			return false
		}
		if b.roomLocked() {
			b.inFlight++
			return true
		}
		b.cond.Wait()
	}
}

// reportReached states what each count bound actually reached, once the pass is over.
//
// It is `info` WHETHER OR NOT a bound was met, and that is the criterion rather than a
// preference (S0100 AC-5, S0174 AC-5, observability O3): a library holding fewer files than
// the bound has been processed completely, so the run is complete and no human has to act.
// A record at `error` there would train an operator to ignore the level that means they
// must.
func (b *budget) reportReached() {
	if b == nil {
		return
	}
	got := b.recorded()
	e := b.e
	b.mu.Lock()
	encodes, encodesMet := b.encodes, b.encodesMetLocked()
	b.mu.Unlock()
	limitMet := b.limitMet()

	if b.limit > 0 {
		switch {
		case limitMet:
			e.Log.Info("bounded run: the bound was reached", "bound", boundLimit, "bound_value", b.limit,
				"terminal_outcomes_recorded", got)
		case encodesMet:
			e.Log.Info("bounded run: the bound was not reached, because the encode bound was reached first",
				"bound", boundLimit, "bound_value", b.limit, "terminal_outcomes_recorded", got,
				"bound_reached", false, "stopped_by", boundLimitEncodes)
		default:
			e.Log.Info("bounded run: every eligible file was processed before the bound was reached, "+
				"which is a complete run over a library holding fewer of them than the bound asked for",
				"bound", boundLimit, "bound_value", b.limit, "terminal_outcomes_recorded", got,
				"bound_reached", false)
		}
	}
	if b.limitEncodes > 0 {
		switch {
		case encodesMet:
			e.Log.Info("bounded run: the encode bound was reached", "bound", boundLimitEncodes,
				"bound_value", b.limitEncodes, "encodes_reached", encodes, "bound_reached", true)
		case limitMet:
			e.Log.Info("bounded run: the encode bound was not reached, because the terminal-outcome bound "+
				"was reached first", "bound", boundLimitEncodes, "bound_value", b.limitEncodes,
				"encodes_reached", encodes, "bound_reached", false, "stopped_by", boundLimit)
		default:
			e.Log.Info("bounded run: every eligible file was processed before the encode bound was reached, "+
				"which is a complete run over a library holding fewer files that reach an encode than the "+
				"bound asked for", "bound", boundLimitEncodes, "bound_value", b.limitEncodes,
				"encodes_reached", encodes, "bound_reached", false)
		}
	}
}

// release hands a slot back once the file that took it has been decided, or once it was
// never handed out. The broadcast is what wakes an enumeration waiting in admit.
func (b *budget) release() { b.releaseSlot(nil) }

// releaseSlot is release for a file that carried slot s: a slot whose file reached an
// encode leaves the in-flight count as a spent encode, which it already is.
func (b *budget) releaseSlot(s *encodeSlot) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.inFlight--
	if s != nil && s.encoded {
		b.inFlightEncoded--
	}
	b.mu.Unlock()
	b.cond.Broadcast()
}

// encodeSlot is one in-flight file of a pass under an encode bound. It rides on the
// context the file is processed under, so the pipeline can say "this file reached an
// encode" at the point it does without a second parameter on every function in between.
type encodeSlot struct {
	b       *budget
	encoded bool // guarded by b.mu
}

type encodeSlotKey struct{}

// track gives one file's processing context its slot. Without an encode bound there is
// nothing to track and ctx comes back unchanged.
func (b *budget) track(ctx context.Context) (context.Context, *encodeSlot) {
	if b == nil || b.limitEncodes <= 0 {
		return ctx, nil
	}
	s := &encodeSlot{b: b}
	return context.WithValue(ctx, encodeSlotKey{}, s), s
}

// reachedEncode records that the file processed under ctx has REACHED AN ENCODE (S0174):
// its job entered the encoding state, or, under dry_run, it was decided would-transcode.
// It counts once per file however often it is called, and is a no-op for a file carrying
// no slot - an unbounded pass, a `--file` run, a submission through serve.
func reachedEncode(ctx context.Context) {
	s, _ := ctx.Value(encodeSlotKey{}).(*encodeSlot)
	if s == nil {
		return
	}
	b := s.b
	b.mu.Lock()
	if !s.encoded {
		s.encoded = true
		b.encodes++
		b.inFlightEncoded++
	}
	b.mu.Unlock()
	// Room is unchanged - the slot moved from "might" to "did" - but the bound may now be
	// reached, and an enumeration waiting in admit should stop offering at once.
	b.cond.Broadcast()
}
