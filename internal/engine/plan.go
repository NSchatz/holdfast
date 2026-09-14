package engine

import (
	"context"
	"os"

	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// The read-only pass `holdfast plan` reports from.
//
// WHAT IT IS. One walk of the covered library, the daemon's OWN enumeration over it, the
// daemon's own outright refusals (Declined) and the daemon's own source-side guard chain
// over what is left - with every write taken out. No claim, no row, no encode, no temp, no
// file created, renamed or removed anywhere, including inside the state directory.
//
// WHY IT LIVES HERE rather than in the command that prints it. What `plan` reports is what
// a run WOULD do, and a report that derived that from rules of its own would be a second
// answer waiting to disagree with the first - the answer that decides whether a source is
// destroyed. So this asks the same two things the scan asks, by calling them:
// Engine.enumerate for which files a pass covers, and Engine.guardSource for what the
// guards conclude about each. A library root, a video extension, a hold-back or a guard
// added to the pipeline reaches this report with no second edit.
//
// WHAT IT DOES NOT ASK. Nothing past the guards: no encode, no gate, no swap, and no
// prediction about either. A projection is the COMMAND's business and is derived from rows
// this install already wrote, never from a property of a file nobody has encoded.

// LedgerReader is the whole of what a read-only pass asks the ledger, and every one of the
// three is a read: the two record-based hold-backs the enumeration applies, and the live
// retentions that say which of a file's extra hard links this tool holds itself.
//
// It is an interface rather than store.Store because a pass whose contract is "the state
// directory is byte-for-byte what it was" must be able to hold a handle that CANNOT write
// (store.OpenSnapshot), and because a fresh install has no ledger at all: a nil LedgerReader
// is the honest reading of that, and yields the empty hold-back set rather than an error.
type LedgerReader interface {
	ParkedIncidents(ctx context.Context) ([]store.SwapIncident, error)
	ExcludedReplacementPaths(ctx context.Context) ([]string, error)
	ListRetained(ctx context.Context) ([]store.Retained, error)
}

// PlanFile is one covered file and the verdict the guard chain reached on it.
//
// Root and ProfileDigest travel with every file because no figure in a plan may span roots
// that would be encoded differently: the digest is the identity of the resolved values that
// judged this file, and it is the same identity a terminal row records.
type PlanFile struct {
	Path  string
	Bytes int64

	// Root is the cleaned library root the file was enumerated under, empty for a file
	// under none (which the daemon judges by the top-level profile, and so does this).
	Root string
	// ProfileDigest identifies the resolved values that judged this file.
	ProfileDigest string
	// EncodeProfile is the name of the encode profile whose overrides applied, empty when
	// none matched and the root's own settings stood.
	EncodeProfile string

	// Guard is the token that stopped the file, empty when every guard passed and a run
	// would transcode it.
	Guard string
	// Unreadable marks the one verdict that is not a skip: the probe reported no video
	// stream, so this file is one the plan could not account for rather than one a guard
	// decided (AC-13's rule at the file level).
	Unreadable bool
	// Detail says why, for a file the pass could not read or probe at all.
	Detail string
}

// Eligible reports whether a run would transcode this file: every guard passed and the
// probe answered.
func (f PlanFile) Eligible() bool { return f.Guard == "" && !f.Unreadable }

// PlanDeclined is one enumerated path the pipeline refuses OUTRIGHT (Declined): a run
// claims, probes and records nothing about it, so it is in no figure of the plan either.
type PlanDeclined struct {
	Path   string
	Rule   string
	Detail string
}

// PlanPass is what one read-only pass found: every covered file with its verdict, and how
// many probe snapshots that cost.
type PlanPass struct {
	// Files is every file the enumeration covered and the pipeline did not decline, in the
	// enumeration's own order. It is the set a daemon pass covers, file for file.
	Files []PlanFile
	// Declined is every enumerated path the pipeline refuses outright, reported rather than
	// dropped: a path missing from a report about a library reads as one that is not there.
	Declined []PlanDeclined
	// Probes is how many probe snapshots the pass took. It is reported rather than assumed
	// because "one invocation is one pass over the library" is a property an operator is
	// owed evidence of, on a tool that may be pointed at a library of 300,000 files.
	Probes int
}

// PlanOptions are the read-only pass's two dependencies.
type PlanOptions struct {
	// Ledger reads the records the enumeration and the hardlink guard consult. nil is a
	// fresh install with no ledger, which holds nothing back and retains no link.
	Ledger LedgerReader
	// Snapshot takes one file's probe snapshot. nil uses the engine's own prober; a caller
	// that wants to COUNT what a pass asks of the prober supplies a wrapper around it.
	Snapshot func(ctx context.Context, path string) *probe.VideoProps
}

// Plan enumerates the covered library exactly as a scan does and runs every source-side
// guard over what it found, WRITING NOTHING.
//
// SetCoverage must have been called with a startup walk's coverage set first, exactly as a
// daemon does before its first scan: that walk is the one traversal this costs, and the
// enumeration reads its listings rather than making a second set of its own.
//
// A path the pipeline declines outright is DECLINED with its reason, and a covered file it
// cannot read or probe is UNACCOUNTED FOR with its reason. Both are published and neither is
// in any figure: a walk that stopped at the first permission denial, or that quietly dropped
// what it met, would report a confident total for a library it had only partly seen.
func (e *Engine) Plan(ctx context.Context, opt PlanOptions) *PlanPass {
	snapshot := opt.Snapshot
	if snapshot == nil {
		snapshot = e.Probe.VideoProps
	}
	// The record-based hold-backs the enumeration applies, published from the read-only
	// handle rather than read through the engine's store: a plan may not open a ledger it
	// would have to create.
	e.held.Store(holdBacksFrom(ctx, opt.Ledger, e.Log))

	pass := &PlanPass{}
	files, _ := e.enumerate()
	for _, f := range files {
		if ctx.Err() != nil {
			return pass
		}
		// Asked where ProcessFile asks it, before the hardlink guard and before anything is
		// probed, so no guard of this pass answers for a path a run never reaches.
		if rule, detail, yes := DeclinedPath(f); yes {
			pass.Declined = append(pass.Declined, PlanDeclined{Path: f, Rule: rule, Detail: detail})
			continue
		}
		pass.Files = append(pass.Files, e.planFile(ctx, f, opt.Ledger, snapshot, &pass.Probes))
	}
	return pass
}

// planFile judges one covered file the way ProcessFile judges it, minus every write: the
// same root, the same resolved settings, the same hardlink guard and the same source-side
// guard chain, off one probe snapshot.
func (e *Engine) planFile(ctx context.Context, f string, ledger LedgerReader,
	snapshot func(context.Context, string) *probe.VideoProps, probes *int) PlanFile {
	root, _ := e.rootFor(f)
	prof := root.Profile
	ts := e.Cfg.TranscodeIn(prof, f)

	pf := PlanFile{
		Path:          f,
		Root:          root.Clean,
		ProfileDigest: prof.Digest(),
		EncodeProfile: ts.Profile,
	}

	// The size the eligible-bytes figure is built from, read of the ENTRY rather than of
	// whatever it points at: a symbolic link carrying a source name is enumerated and then
	// skipped, and its bytes are the link's own - counting its target's would publish the
	// same bytes twice in the one report an operator plans disk by. DeclinedPath established
	// that a file IS here, so a failure now happened since, and is reported rather than
	// counted as zero.
	fi, err := os.Lstat(f)
	if err != nil {
		pf.Unreadable = true
		pf.Detail = errText(err)
		return pf
	}
	pf.Bytes = fi.Size()

	// Hardlink guard, asked exactly as ProcessFile asks it and before anything is probed:
	// the guard is on for this root's profile, the file carries more than one link, and
	// more of them than this tool itself holds through the undo window.
	if prof.HardlinkSkip() {
		if links := probe.NLink(f); links > 1 &&
			links > 1+heldLinksIn(ctx, f, probe.Fingerprint(f), ledger, e.Log) {
			pf.Guard = SkipHardlinked
			return pf
		}
	}

	// Counted where the snapshot is actually TAKEN, never where one might be: a guard that
	// stops a file before the probe (the symlink guard) costs nothing, and a count that
	// said otherwise would be evidence for a claim this pass does not make.
	counted := func(ctx context.Context, path string) *probe.VideoProps {
		*probes++
		return snapshot(ctx, path)
	}
	_, v := e.guardSource(ctx, f, root, ts, targetCodecFor(ts.Encoder), counted)
	switch {
	case v.failed:
		// The probe answered nothing about this file. The daemon records a failure; a plan
		// counts it as one it could not account for, which is the same statement without a
		// row behind it.
		pf.Unreadable = true
		pf.Detail = FailUnreadable
	case v.stopped():
		pf.Guard = v.guard
	}
	return pf
}
