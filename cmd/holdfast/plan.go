package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/logging"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/startup"
	"github.com/NSchatz/holdfast/internal/store"
)

// `holdfast plan` - what this configuration would do to this library, and what it would
// save, without running anything against it.
//
// It is the written plan an operator is owed before they point a tool that deletes
// originals at everything they own: how many files are eligible, how many were skipped and
// by WHICH guard, how many bytes are eligible, and - only where this install's own
// completed encodes support one - an estimated reclaim carried with the sample it came from
// and the spread of that sample.
//
// IT IS A READ, and that is graded rather than promised: no claim, no ledger row, no
// encode, no file created, renamed or removed anywhere, the state directory included. The
// ledger is opened through store.OpenSnapshot, which creates nothing at all, not even the
// sidecars an ordinary SQLite reader leaves behind, and a state directory with no ledger in
// it is read as "nothing is held back" rather than created.
//
// WHAT IT CONSUMES RATHER THAN RESTATES. Which files a pass covers is the engine's own
// enumeration over the startup walk's coverage set; what the guards conclude about each is
// the engine's own guard chain (engine.Plan). This command decides no coverage rule and no
// guard of its own, so the day a path filter or a guard lands, this report inherits it.
//
// AN ESTIMATE IS NOT A MEASUREMENT, and the honesty of that boundary is the point of this
// command rather than a decoration on it. A projection is derived from the size ratios of
// encodes THIS INSTALL has already completed, it names itself an estimate wherever its
// numbers appear, and where there is no such history it is REFUSED outright with the reason
// - never emitted as a zero, and never guessed at from a published average somebody else
// measured. A refused projection is not an error: the report was produced, so the exit
// status is 0 and a script branches on the refusal field rather than on the code.

const planUsage = `holdfast plan - what this configuration would do to this library, and what it would save

  holdfast plan --config config.yaml           the written report
  holdfast plan --config config.yaml --json    the same plan as one JSON document on stdout

Flags:
  --config   path to the YAML config file (required)
  --json     write the whole plan as one JSON document to stdout, and nothing else to
             stdout; every human message and every error goes to stderr

Exit codes:
  0  a report was produced. A REFUSED projection is still a report, so 0 is the ordinary
     answer on a fresh install that has never completed an encode
  1  the plan could not be made: the config would not load or would not validate, a library
     root could not be read, or the configured ffprobe cannot answer
  2  the invocation was wrong: an unknown flag, a missing --config, or a positional argument

plan is not dry_run and neither is an alias for the other. dry_run is a full daemon pass
that probes, guards and records a terminal row per file it decided without encoding - a
record of what THAT RUN decided. plan is a read that walks and projects, claiming nothing
and writing nothing - a report about the library.
`

// planOptions is the command's one flag.
type planOptions struct {
	// JSON writes the whole plan as one JSON document instead of the report.
	JSON bool
}

// planSnapshotWrap, when non-nil, wraps the probe snapshot the plan pass takes. It is the
// one test seam this command has, and it exists because "one invocation is one pass over
// the library" can only be seen from outside by COUNTING what a pass asks of the prober.
// Production leaves it nil and the engine uses the prober itself.
var planSnapshotWrap func(func(context.Context, string) *probe.VideoProps) func(context.Context, string) *probe.VideoProps

func cmdPlan(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(stderr, planUsage) }
	asJSON := fs.Bool("json", false, "write the whole plan as one JSON document instead of the report")
	cfg, code := loadConfig(fs, args, stderr)
	if cfg == nil {
		return code
	}
	if rest := fs.Args(); len(rest) > 0 {
		fmt.Fprintf(stderr, "holdfast: plan takes no positional arguments (got %q)\n\n", rest[0])
		fs.Usage()
		return 2
	}
	// An interrupted plan is a plan that was not made: the signal cancels every probe in
	// flight, and there is nothing written to undo.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runPlan(ctx, cfg, planOptions{JSON: *asJSON}, stdout, stderr)
}

// runPlan is the command without the signal wiring, so a test can cancel it in the middle
// and assert that an interrupted run left the same nothing behind a completed one does.
func runPlan(ctx context.Context, cfg *config.Config, opt planOptions, stdout, stderr io.Writer) int {
	// The start-or-refuse decision, TAKEN and then reported rather than obeyed - exactly as
	// `analyze` takes it, and through the same one construction, so a plan is bounded by the
	// same walk a run would be bounded by. Storage that is not positively local is a verdict
	// printed beside the plan rather than a reason to refuse, because this mutates nothing;
	// every other cause is a plan that cannot be made.
	res := startupDecision(cfg)
	if !res.Start && res.Row != rowStorageNotLocal {
		res.WriteRefusal(stderr)
		return 1
	}

	eng, ledger, ledgerNote, code := planEngine(ctx, cfg, res, stderr)
	if code != 0 {
		return code
	}
	if ledger != nil {
		defer func() { _ = ledger.Close() }()
	}

	pass := eng.Plan(ctx, engine.PlanOptions{Ledger: planLedgerReader(ledger), Snapshot: planSnapshot(eng)})
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, "holdfast: interrupted - no plan was made, and nothing was written")
		return 1
	}

	p := buildPlan(ctx, cfg, res, pass, ledger, ledgerNote)

	if opt.JSON {
		if err := p.writeJSON(stdout); err != nil {
			fmt.Fprintf(stderr, "holdfast: writing the plan: %v\n", err)
			return 1
		}
		return 0
	}
	p.writeReport(stdout)
	return 0
}

// planEngine builds the read-only pass: the prober it probes with, the ledger it reads
// through, and an engine holding NO writable store at all - which is the structural half of
// "this command writes nothing", since there is no handle here that could.
//
// It is one function rather than inline setup so a test can build exactly the pass the
// command runs and compare the files it covers against the ones a daemon pass covers, path
// by path, rather than against a summary of them.
func planEngine(ctx context.Context, cfg *config.Config, res startup.Result, stderr io.Writer) (
	*engine.Engine, *store.SQLite, string, int) {
	// A plan built on a probe that can answer nothing is not a plan: every file would be
	// reported as one this command could not account for, which is true and useless. That is
	// a broken environment rather than an unreadable file, so it is refused here with the
	// reason instead of being folded into the report's own unaccounted count.
	ffmpegBin := envOr("HOLDFAST_FFMPEG", "ffmpeg")
	ffprobeBin := envOr("HOLDFAST_FFPROBE", "ffprobe")
	if _, err := exec.LookPath(ffprobeBin); err != nil {
		fmt.Fprintf(stderr, "holdfast: the configured ffprobe %q could not be run: %v\n", ffprobeBin, err)
		return nil, nil, "", 1
	}
	prober := probe.New(ffmpegBin, ffprobeBin)
	if !prober.Usable(ctx) {
		fmt.Fprintf(stderr, "holdfast: the configured ffprobe %q ran but answered nothing when asked for its "+
			"own version, so nothing it says about a file is evidence about that file\n", ffprobeBin)
		return nil, nil, "", 1
	}

	// The ledger, through a handle that CANNOT write and creates nothing - not even the
	// -wal/-shm sidecars an ordinary reader leaves beside a WAL database. A state directory
	// with no ledger in it is the ordinary state of a fresh install and is never created.
	ledger, note := openPlanLedger(cfg)

	// Store nil, deliberately. The logger writes to THIS command's stderr, so --json's
	// stdout carries the document and nothing else whatever the caller wired up.
	eng := engine.New(*cfg, prober, nil, nil, logging.To(stderr, cfg.LogLevel))
	eng.SetCoverage(res.Coverage, res.Entries)
	return eng, ledger, note, 0
}

// planLedgerReader hands the pass a reader, or a genuinely nil interface where there is no
// ledger. It exists because a nil *store.SQLite stored in an interface is not a nil
// interface, and the pass's "no ledger" branch tests the interface.
func planLedgerReader(st *store.SQLite) engine.LedgerReader {
	if st == nil {
		return nil
	}
	return st
}

// planSnapshot is the function the pass probes with: the engine's own prober, through the
// test seam when one is installed.
func planSnapshot(eng *engine.Engine) func(context.Context, string) *probe.VideoProps {
	snapshot := eng.Probe.VideoProps
	if planSnapshotWrap != nil {
		return planSnapshotWrap(snapshot)
	}
	return snapshot
}

// openPlanLedger opens this install's ledger for reading, or reports why there is none to
// read. Every outcome is survivable: a plan is a report about the library, and a ledger that
// is absent or unreadable costs the record-based hold-back and the projection, both of which
// then say so, rather than the plan.
func openPlanLedger(cfg *config.Config) (*store.SQLite, string) {
	dbPath := filepath.Join(effectiveStateDir(cfg), "jobs.db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Sprintf("no ledger at %s, so no path is held back by a record and this install "+
			"has no completed encode of its own to project from", dbPath)
	}
	st, err := store.OpenSnapshot(dbPath)
	if err != nil {
		return nil, fmt.Sprintf("the ledger at %s could not be read (%v), so no record-based hold-back was "+
			"applied and no ratio could be read from it", dbPath, err)
	}
	return st, fmt.Sprintf("%s was read for the record-based hold-back and for the size ratios of this "+
		"install's own completed encodes; not one byte of it was written", dbPath)
}

// buildPlan turns one read-only pass into the report both output forms render from.
//
// The grouping is by RESOLVED PROFILE and it is the whole reason a plan can be trusted as
// arithmetic: a figure that spanned two roots configured at different settings would be a
// number about a population that will not be encoded the same way. The digest is the
// identity of those resolved values and is what a terminal row records, so the sample a
// projection is drawn from is the sample decided under the very profile being projected.
func buildPlan(ctx context.Context, cfg *config.Config, res startup.Result, pass *engine.PlanPass,
	ledger *store.SQLite, ledgerNote string) *plan {
	p := &plan{
		Command: "holdfast plan",
		Storage: storageVerdictOf(res),
		Ledger:  ledgerNote,
		Probes:  pass.Probes,
		Note: "plan wrote NOTHING: no job row, no ledger row, no claim, no encode, and every file " +
			"under every library root is byte for byte what it was. It is not dry_run and neither is " +
			"an alias for the other - dry_run is a full daemon pass that RECORDS a terminal row per " +
			"file it decided, and plan records none.",
	}
	p.Coverage = planCoverageOf(cfg, res)

	// One group per distinct resolved profile, in configuration order, each naming the roots
	// that produced it - which is the same reading `validate` prints.
	at := map[string]*planGroup{}
	for _, r := range cfg.RootProfiles() {
		d := r.Profile.Digest()
		g, ok := at[d]
		if !ok {
			g = newPlanGroup(d)
			at[d] = g
			p.Profiles = append(p.Profiles, g)
		}
		g.LibraryRoots = append(g.LibraryRoots, r.Clean)
	}

	total := newPlanGroup("every resolved profile, combined")
	total.LibraryRoots = nil
	for _, f := range pass.Files {
		g, ok := at[f.ProfileDigest]
		if !ok {
			// A covered file under no configured root. The daemon judges it by the top-level
			// profile and records no root, so this reports it under its own digest rather than
			// attributing it to a root it does not lie beneath.
			g = newPlanGroup(f.ProfileDigest)
			at[f.ProfileDigest] = g
			p.Profiles = append(p.Profiles, g)
		}
		g.add(f)
		total.add(f)
	}

	// The ratios, read ONCE for the whole report: the whole-ledger sample for the total and
	// the per-digest samples for the groups, computed over the same rows by the same
	// expression so no group's figure can disagree with the total by being derived twice.
	var overall store.Spread
	byProfile := map[string]store.Spread{}
	if ledger != nil {
		overall, byProfile = ledger.SizeRatios(ctx)
	}
	for _, g := range p.Profiles {
		g.finish(byProfile[g.Profile], "no completed encode in this install's history was decided under "+
			"this profile, so there is no size ratio of its OWN to project from")
	}
	total.finish(overall, "this install has never completed an encode, so it has no size ratio of its own "+
		"to project from and this report will not guess at one")
	p.Total = total

	sort.Strings(p.Total.LibraryRoots)
	return p
}

// planCoverageOf is what the startup walk read and what it did not. A plan is BOUNDED by it:
// a directory nobody listed holds an unknown number of files, which is not the same as
// holding none, and a total that quietly assumed otherwise would be the confident wrong
// answer this repository's fail-safe rule exists to forbid.
func planCoverageOf(cfg *config.Config, res startup.Result) planCoverage {
	roots := make([]string, 0, len(cfg.LibraryRoots))
	for _, r := range cfg.LibraryRoots {
		roots = append(roots, filepath.Clean(r))
	}
	under := func(path string) bool {
		for _, root := range roots {
			if path == root || len(path) > len(root) && path[:len(root)] == root &&
				path[len(root)] == filepath.Separator {
				return true
			}
		}
		return false
	}

	c := planCoverage{Boundary: "what is inside a directory the startup walk could not list is UNKNOWN, " +
		"never reported as absent: the eligible figures below cover the directories that were read and " +
		"no others"}
	notRead := map[string]int64{}
	for _, dir := range res.Coverage {
		if under(dir) {
			c.DirectoriesRead++
		}
	}
	// notTraversed is exactly the set of startup reports that mean A DIRECTORY WAS NOT
	// LISTED, and it is the census's own reading of them rather than a second one.
	for _, n := range res.Notices {
		if notTraversed[n.Kind] && under(n.Path) {
			notRead[string(n.Kind)]++
			c.DirectoriesNotRead++
		}
	}
	c.NotReadWhy = sortedBuckets(notRead)
	return c
}
