package engine

import (
	"context"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// What the FEED declines to spend a worker on (S0095 AC-9).
//
// A pause stops the feed where it stands and the files it never handed out stay pending, so
// the scan after a resume starts at the top of the configured order again and walks back
// over everything already decided. Claim turns each of those away - that is what a terminal
// row is for and nothing here changes it - but the walk costs an attribute read and a write
// transaction per file, on the single write connection the workers are using, before the
// answer comes back "no". On a library that is mostly done, that is the whole pass.
//
// So the feed asks the cheap question first: would Claim refuse this row? It is asked of the
// STORE, through the store's own predicates, so this is not a second opinion about what is
// terminal. Two rules keep it from ever being one:
//
//   - IT ONLY EVER DECLINES WHERE IT CAN PROVE A REFUSAL. An unresolvable profile, an
//     unreadable fingerprint, a store that will not answer - each of them OFFERS the file and
//     lets ProcessFile and Claim decide it, which is the behaviour of the build before this
//     existed. A wrong hold-back leaves a file unprocessed; a wrong offer costs one claim
//     that is refused.
//   - IT DECIDES NOTHING ABOUT THE FILE. Nothing is written, no row moves, no verdict is
//     reached and no event is emitted. A declined candidate is simply not handed to a worker
//     this pass, and the next scan asks again.
//
// WHAT IT COSTS. One indexed read per candidate, and - only for a candidate that already has
// a row a refusal could come from - one attribute read to confirm the file is still the one
// that row describes. The pass stays linear in the number of candidates (performance PB4):
// there is no nesting, and the per-path profile resolution is memoized on the pair it
// actually depends on rather than recomputed per file.
type feedHoldOut struct {
	eng *Engine
	// inputs resolves the decision inputs in force for one path WITHOUT touching the file,
	// or reports that they cannot be resolved without one. nil disables the hold-out.
	inputs func(path string) (store.DecisionInputs, bool)
}

// newFeedHoldOut builds the hold-out for one pass. The resolution it captures is a snapshot
// of the configuration in force, exactly as the pass's hold-back snapshot is: a pass reads
// its own configuration end to end.
func (e *Engine) newFeedHoldOut() *feedHoldOut {
	if e.Store == nil {
		return &feedHoldOut{eng: e}
	}
	return &feedHoldOut{eng: e, inputs: feedInputsPerPath(e.Cfg)}
}

// declines reports whether the feed should pass over this candidate rather than hand it to a
// worker. Every answer it cannot prove is false.
func (f *feedHoldOut) declines(ctx context.Context, path string) bool {
	if f == nil || f.inputs == nil {
		return false
	}
	current, ok := f.inputs(path)
	if !ok {
		return false
	}
	held, err := f.eng.Store.TerminalHolds(ctx, path, current, mutableGuardSkips...)
	if err != nil {
		// The store is the authority and it did not answer, so nothing is concluded from
		// the silence: the file goes to a worker and the claim asks the same question where
		// a failure is already handled. Named, attempted, next (observability O4).
		f.eng.Log.Warn("the ledger could not be asked whether this file's recorded outcome still "+
			"holds, so the file is offered and the claim decides it as it always has",
			"dependency", "store", "attempted", "read the terminal rows recorded for this path",
			"next", "offer the file to a worker; nothing about it is decided here",
			"file", path, "err", err)
		return false
	}
	if len(held) == 0 {
		return false
	}

	// There IS a row a refusal could come from, so - and only now - the one question left is
	// whether the file on disk is still the file that row describes. A row is keyed by the
	// source's size and modification time, so a file that has been re-downloaded or edited
	// keys to a row that does not exist and is claimed exactly as an unseen file is. This
	// read is what keeps that true; without it the hold-out would hold a CHANGED file out of
	// the pipeline for ever, which is the one failure it must not have.
	fi, err := f.eng.stat(path)
	if err != nil {
		// Nothing at the other end of this path, or this process may not look. The door says
		// so per file, in the words it has always said it in, so this says nothing and
		// offers.
		return false
	}
	now := probe.AttributesOf(fi).String()
	for _, fingerprint := range held {
		if fingerprint == now {
			f.eng.Log.Info("not offering (its recorded outcome still holds under the configuration in "+
				"force, so a worker would be turned away at the claim); the row is re-opened by a "+
				"configuration change it can reason about, or by the local `holdfast requeue`",
				"file", path)
			return true
		}
	}
	return false
}

// feedInputsPerPath is the per-path decision-input resolution the FEED may use: the same
// reading DecisionInputsForJob makes for a claim, for every path whose profile can be
// resolved from the path alone.
//
// It REFUSES to answer for a root that bands its files on the source height, and that
// refusal is the whole of why this is not DecisionInputsPerPath. That resolution answers
// with the root's own profile where a band needs a height nobody read, which is right for a
// survey - a count an operator reads, erring toward "this row has moved" - and wrong here,
// where the same error in the other direction is a file held out of the pipeline by a rule
// the scan would have resolved differently. A banded root is therefore never declined at
// all: its files go to a worker and the claim decides them, which costs exactly what it
// cost before this existed.
//
// The resolution is memoized on (library root, encode profile name), which is the whole of
// what it depends on once the height is out of it, so a library-sized pass costs a handful
// of resolutions and one match per candidate. The closure is stateful and is called from
// the enumeration's one goroutine.
func feedInputsPerPath(cfg config.Config) func(path string) (store.DecisionInputs, bool) {
	roots := cfg.RootProfiles()
	cached := map[string]store.DecisionInputs{}
	return func(path string) (store.DecisionInputs, bool) {
		var root config.Root
		var rooted bool
		for _, r := range roots {
			if r.Contains(path) {
				root, rooted = r, true
				break
			}
		}
		// A path under no configured root is one the enumeration should never have produced.
		// It is offered rather than reasoned about: an unexplained path is not one to take a
		// hold-back decision on.
		if !rooted {
			return store.DecisionInputs{}, false
		}
		if root.Profile.Rules.NeedsSourceHeight() {
			return store.DecisionInputs{}, false
		}
		prof := root.Profile.WithRules(0)
		ts := cfg.TranscodeIn(prof, path)
		key := root.Clean + "\x00" + ts.Profile
		if in, ok := cached[key]; ok {
			return in, true
		}
		in := DecisionInputsForJob(cfg, prof, ts)
		cached[key] = in
		return in, true
	}
}
