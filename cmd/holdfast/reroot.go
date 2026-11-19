package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/logging"
	"github.com/NSchatz/holdfast/internal/store"
)

// `holdfast reroot` carries the ledger across a library root rename.
//
// Every path the ledger records names the library root it was recorded under. Mount the
// same filesystem at a new path, edit library_roots to match, and every row is stranded
// under a tree no scan enumerates: a file whose encode failed deterministically is encoded
// and failed again under its new name, every done and skipped verdict is taken again, and
// a retention recorded under the old path is out of the undo window's reach. This command
// rewrites the old tree's rows to the new one.
//
// It is LOCAL by design and never an HTTP endpoint, for the reason `restore` and `requeue`
// are not: it changes what the engine will do to the library's files, and a mutating
// endpoint opens an authorization question the read-and-control API does not answer.
//
// Every refusal exits NON-ZERO having rewritten nothing, and the rewrite itself is one
// transaction. The ledger is held exclusively for the whole command (store.OpenExclusive),
// so it is refused while a daemon has the ledger open, and a daemon started during it
// cannot read the ledger until it is done.

const rerootUsage = `holdfast reroot - carry the ledger across a library root rename

  holdfast reroot --config <file> --from <old root> --to <new root>           what it would rewrite
  holdfast reroot --config <file> --from <old root> --to <new root> --apply   rewrite it

Flags:
  --config     path to the YAML config file (required); --to is one of its library_roots
  --from       the root the ledger's rows were recorded under, which the config no longer names
  --to         the configured library root the same files are mounted at now
  --apply      rewrite the ledger, in one transaction; without it nothing is rewritten
  --min-match  the share of the recorded files found under --to that must still be the
               recorded file (default 0.95; above 0 and at most 1)
  --min-coverage  the share of ALL the recorded files, deleted ones included, that must be
               under --to as recorded (default 0.5; above 0 and at most 1)
  --carry-missing-retained  carry, as recorded, the retained originals that are not under
               --to when --from cannot be checked (the undo window then drops them as missing)

It is for a library that MOVED: the same filesystem mounted at a new path, with
library_roots edited to match. Every path the ledger records at or under --from (job rows,
retained originals, swap incidents, withheld paths, health checks, node leases, published
subtitle sidecars) is rewritten to the same place under --to, so the scan finds its
verdicts again and restore and the undo window's release reach the retained originals.

Stop holdfast first. The ledger must be open nowhere else, and reroot refuses while it is.

It refuses, rewriting nothing, when --to is not a configured library root; when --from is
one, or contains or lies inside one; when nothing is recorded under --from; when a
rewritten row would take a key a row under --to already holds (each is named, and nothing
is merged); and when --to is not shown to be the tree --from was. That evidence is the
files themselves, read at BOTH paths. A file still at --from must be the same file (one
device and inode) at --to: two different files is a copy, not a move, and a file only at
--from is a partial copy or the wrong --to; either refuses. Of the rest, at least one, at
least --min-match of those found under --to and at least --min-coverage of all of them
must be under --to as recorded (the same size:mtime, or for a retained original the same
size), and every retained original found there must be the recorded size. A retained
original the undo window still holds that is not under --to refuses unless --from was
checked and it is gone there too, or --carry-missing-retained acknowledges it. A file replaced
since (an *arr upgrade under the same name) and one deleted since are carried as recorded:
a row is keyed by its path AND its fingerprint, so the next scan answers a replaced file
as the new file it is.

Run it with holdfast stopped and before a build that scans the new root starts, and where
the old tree still exists, let it see that tree (bind it read-only into the container).

With --apply it first migrates a ledger an earlier build wrote, like every command that
writes the ledger, and the log records each step. A dry run changes nothing, the schema
included: it refuses a ledger an earlier build wrote rather than upgrade it. To preview
one, run --apply against a copy: HOLDFAST_STATE_DIR=<a copy of the state directory>.
`

// rerootComponent names the emitter on every log record this command writes.
const rerootComponent = "ledger.reroot"

// rerootSample bounds every list a refusal prints: enough to see the pattern, never the
// whole of a library.
const rerootSample = 10

type rerootRequest struct {
	From, To    string
	Apply       bool
	MinMatch    float64
	MinCoverage float64
	// CarryMissingRetained acknowledges live retained originals absent under --to while
	// --from cannot be checked: their records are carried as recorded.
	CarryMissingRetained bool
}

func cmdReroot(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reroot", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(stderr, rerootUsage) }
	from := fs.String("from", "", "the root the ledger's rows were recorded under")
	to := fs.String("to", "", "the configured library root the same files are mounted at now")
	apply := fs.Bool("apply", false, "rewrite the ledger; without it nothing is rewritten")
	minMatch := fs.Float64("min-match", rerootMinMatch, "the share of the recorded files found under --to that must match")
	minCoverage := fs.Float64("min-coverage", rerootMinCoverage, "the share of all the recorded files that must be under --to as recorded")
	carryMissing := fs.Bool("carry-missing-retained", false, "carry retained originals absent under --to, with --from unseen, as recorded")

	cfg, code := loadConfig(fs, args, stderr)
	if cfg == nil {
		return code
	}
	if rest := fs.Args(); len(rest) > 0 {
		fmt.Fprintf(stderr, "holdfast: reroot takes no arguments (got %q)\n\n", rest)
		fs.Usage()
		return exitUsage
	}
	if *from == "" || *to == "" {
		fmt.Fprint(stderr, "holdfast: reroot needs both --from and --to\n\n")
		fs.Usage()
		return exitUsage
	}
	if err := checkShares(*minMatch, *minCoverage); err != nil {
		fmt.Fprintf(stderr, "holdfast: %v\n\n", err)
		fs.Usage()
		return exitUsage
	}
	log := logging.To(stderr, cfg.LogLevel)
	return runReroot(context.Background(), log, cfg, rerootRequest{From: *from, To: *to, Apply: *apply,
		MinMatch: *minMatch, MinCoverage: *minCoverage, CarryMissingRetained: *carryMissing}, stdout, stderr)
}

// checkShares refuses a threshold that is not a share. NaN fails both comparisons, so it is
// refused too.
func checkShares(minMatch, minCoverage float64) error {
	for _, f := range []struct {
		flag string
		v    float64
	}{{"--min-match", minMatch}, {"--min-coverage", minCoverage}} {
		if !(f.v > 0 && f.v <= 1) {
			return fmt.Errorf("%s %v is not a share: it must be above 0 and at most 1", f.flag, f.v)
		}
	}
	return nil
}

// runReroot is the command after its flags: every refusal, the plan, the evidence and the
// rewrite, in that order.
func runReroot(ctx context.Context, log *slog.Logger, cfg *config.Config, req rerootRequest, stdout, stderr io.Writer) int {
	refuse := func(reason string, format string, args ...any) int {
		msg := fmt.Sprintf(format, args...)
		fmt.Fprintf(stderr, "holdfast: reroot refused: %s\nnothing was rewritten.\n", msg)
		log.Warn("ledger reroot refused; nothing was rewritten",
			"component", rerootComponent, "from", req.From, "to", req.To, "apply", req.Apply,
			"reason", reason, "detail", msg)
		return exitError
	}

	if err := checkShares(req.MinMatch, req.MinCoverage); err != nil {
		return refuse("threshold", "%v", err)
	}
	from, to, reason, err := rerootRoots(cfg, req.From, req.To)
	if err != nil {
		return refuse(reason, "%v", err)
	}
	req.From, req.To = from, to

	dbPath := filepath.Join(effectiveStateDir(cfg), "jobs.db")
	if _, err := os.Stat(dbPath); err != nil {
		return refuse("no-ledger", "there is no ledger at %s, so there is nothing to carry", dbPath)
	}
	// A dry run promises to change nothing, and a migration would: it stamps a schema version
	// the holdfast that wrote the ledger then refuses to open. So only --apply migrates.
	open := store.OpenExclusive
	if !req.Apply {
		open = store.OpenExclusiveCurrent
	}
	st, err := open(dbPath)
	if err != nil {
		reportMigrationRefusal(log, err)
		if errors.Is(err, store.ErrSchemaBehind) {
			return refuse("schema-behind", "%v. A dry run changes nothing, so it does not upgrade the ledger: that "+
				"would stamp a version the holdfast which wrote it then refuses to open. To preview, copy jobs.db "+
				"(and any jobs.db-wal beside it) into a scratch directory with holdfast stopped and run "+
				"HOLDFAST_STATE_DIR=<that directory> holdfast reroot ... --apply: it migrates and rewrites only the "+
				"copy. With --apply here the reroot migrates this ledger first, as restore and requeue do, and then "+
				"refuses or rewrites; after that only this build or a later one opens it", err)
		}
		if errors.Is(err, store.ErrLedgerInUse) {
			return refuse("ledger-in-use", "the ledger at %s is open in another process - a running `holdfast serve` "+
				"or `holdfast run`, or another holdfast command. A reroot rewrites the paths that process is working "+
				"on: stop it, then run reroot again", dbPath)
		}
		return refuse("ledger-unopenable", "opening the ledger at %s: %v", dbPath, err)
	}
	defer func() { _ = st.Close() }()
	reportMigrations(log, st.MigrationReport())

	plan, err := st.PlanReroot(ctx, from, to)
	if err != nil {
		return refuse("plan-failed", "reading the ledger: %v", err)
	}
	printRerootPlan(stdout, plan, req.Apply)
	if plan.Rows() == 0 {
		return refuse("nothing-recorded", "the ledger records nothing at or under %s", from)
	}
	if n := len(plan.Collisions); n > 0 {
		for i, c := range plan.Collisions {
			if i == rerootSample {
				fmt.Fprintf(stderr, "  ... and %d more\n", n-rerootSample)
				break
			}
			key := c.Path
			if c.With != "" {
				key += " (" + c.With + ")"
			}
			fmt.Fprintf(stderr, "collision: %s %s is already recorded, so %s cannot be carried to it\n", c.Table, key, c.Was)
		}
		return refuse("collision", "%d row(s) under %s would take a key a row under %s already holds (named above). "+
			"Nothing is merged: each pair is two records of one file, and which one stands is not a decision "+
			"this command takes", n, from, to)
	}

	ev := checkRerootEvidence(plan.Evidence, from, req.CarryMissingRetained)
	printRerootEvidence(stdout, ev, from, to, req.MinMatch, req.MinCoverage)
	if why, label, findings := ev.refusal(from, to, req.MinMatch, req.MinCoverage); why != "" {
		printFindings(stderr, label, findings)
		return refuse("evidence", "%s", why)
	}

	if !req.Apply {
		logReroot(log, plan, ev, req, false)
		fmt.Fprintln(stdout, "dry run: nothing was rewritten. The same command with --apply rewrites these rows in one transaction.")
		return exitOK
	}
	done, err := st.ApplyReroot(ctx, plan)
	if err != nil {
		var col *store.RerootCollisionsError
		if errors.As(err, &col) {
			return refuse("collision", "%v", err)
		}
		return refuse("apply-failed", "the rewrite was rolled back: %v", err)
	}
	logReroot(log, done, ev, req, true)
	fmt.Fprintf(stdout, "rewrote %d row(s) from %s to %s in one transaction; the next scan finds them under %s.\n",
		done.Rows(), from, to, to)
	return exitOK
}

// rerootRoots holds --from and --to against the configuration, and returns them cleaned,
// with the refusal's reason token where it refuses.
//
// --to must BE a configured root: the point is a ledger the scan reaches, and a tree the
// configuration does not name is one it never will. --from must be in no configured root,
// contain none, and lie in none: rows a scan still reaches are not stranded, and carrying
// a tree that contains a configured root would carry that root's rows with it.
func rerootRoots(cfg *config.Config, from, to string) (string, string, string, error) {
	for _, p := range []struct{ flag, v string }{{"--from", from}, {"--to", to}} {
		if !filepath.IsAbs(p.v) {
			return "", "", "paths", fmt.Errorf("%s %q is not an absolute path", p.flag, p.v)
		}
	}
	from, to = filepath.Clean(from), filepath.Clean(to)
	roots := cfg.RootProfiles()
	var names []string
	configured := false
	for _, r := range roots {
		names = append(names, r.Clean)
		if r.Clean == to {
			configured = true
		}
	}
	if !configured {
		return "", "", "to-not-a-root", fmt.Errorf("--to %s is not a configured library root (library_roots: %s); "+
			"edit library_roots to name the root's new path first", to, strings.Join(names, ", "))
	}
	for _, r := range roots {
		_, inside := store.RerootPath(from, r.Clean, r.Clean)
		_, contains := store.RerootPath(r.Clean, from, from)
		if inside || contains {
			return "", "", "from-is-a-root", fmt.Errorf("--from %s overlaps the configured library root %s: the scan "+
				"still reaches what is recorded there, so there is nothing to carry. --from is the old path, "+
				"the one library_roots no longer names", from, r.Clean)
		}
	}
	return from, to, "", nil
}

func printRerootPlan(w io.Writer, p store.RerootPlan, apply bool) {
	mode := "dry run, nothing is rewritten"
	if apply {
		mode = "--apply"
	}
	fmt.Fprintf(w, "reroot %s -> %s (%s)\n", p.From, p.To, mode)
	for _, t := range p.Tables {
		cols := make([]string, 0, len(t.Columns))
		for _, c := range t.Columns {
			cols = append(cols, fmt.Sprintf("%s %d", c.Column, c.Values))
		}
		fmt.Fprintf(w, "  %-20s %8d row(s): %s\n", t.Table, t.Rows, strings.Join(cols, ", "))
	}
	fmt.Fprintf(w, "  %-20s %8d row(s)\n", "in all", p.Rows())
}

// logReroot writes what the command did as RECORDS: one per table, every column's count an
// attribute, then one for the whole, so a log pipeline can select on the table, the column
// or the evidence rather than parse a sentence. A plan that was not applied is recorded too,
// saying so, at the same level: both are what happened.
func logReroot(log *slog.Logger, p store.RerootPlan, ev rerootEvidence, req rerootRequest, applied bool) {
	msg := "ledger reroot planned; nothing was rewritten"
	if applied {
		msg = "ledger reroot applied"
	}
	for _, t := range p.Tables {
		cols := make([]any, 0, len(t.Columns))
		for _, c := range t.Columns {
			cols = append(cols, slog.Int64(c.Column, c.Values))
		}
		log.Info(msg, "component", rerootComponent, "from", p.From, "to", p.To, "applied", applied,
			"table", t.Table, "rows", t.Rows, slog.Group("columns", cols...))
	}
	log.Info(msg, "component", rerootComponent, "from", p.From, "to", p.To, "applied", applied,
		"rows", p.Rows(), "evidence_checked", ev.Checked, "evidence_matched", ev.Matched,
		"evidence_same_file", ev.SameFile, "evidence_replaced", len(ev.Replaced), "evidence_absent", ev.Absent,
		"evidence_retained_mismatched", len(ev.RetainedMismatched),
		"evidence_identity_checked", ev.identityChecked(), "evidence_from_witnesses", ev.Witnesses,
		"evidence_retained_carried_missing", ev.CarriedMissingRetained,
		"match_ratio", ev.ratio(), "min_match", req.MinMatch,
		"coverage", ev.coverage(), "min_coverage", req.MinCoverage)
}
