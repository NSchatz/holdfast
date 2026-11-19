package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/logging"
	"github.com/NSchatz/holdfast/internal/probe"
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

It is for a library that MOVED: the same filesystem mounted at a new path, with
library_roots edited to match. Every path the ledger records at or under --from (job rows,
retained originals, swap incidents, withheld paths, health checks, node leases, published
subtitle sidecars) is rewritten to the same place under --to, so the scan finds its
verdicts again and restore and the undo window's release reach the retained originals.

Stop holdfast first. The ledger must be open nowhere else, and reroot refuses while it is.

It refuses, rewriting nothing, when --to is not a configured library root; when --from is
one, or contains or lies inside one; when nothing is recorded under --from; when a
rewritten row would take a key a row under --to already holds (each is named, and nothing
is merged); and when --to is not shown to be the same files. That evidence is the files
themselves: of the recorded files that exist under --to, at least one, and at least
--min-match of them, must match a size:mtime the ledger recorded for that path, and every
retained original that exists there must be the size of the original it holds. A file
replaced since (an *arr upgrade under the same name) and a file deleted since are both
carried as recorded: a row is keyed by its path AND its fingerprint, so the next scan
answers a replaced file as the new file it is.

Like every command that writes the ledger, it first migrates a ledger an earlier build
wrote, and the log records each step.
`

// rerootComponent names the emitter on every log record this command writes.
const rerootComponent = "ledger.reroot"

// rerootSample bounds every list a refusal prints: enough to see the pattern, never the
// whole of a library.
const rerootSample = 10

// rerootMinMatch is --min-match's default: the share of the recorded files found under --to
// that must still be the recorded file. A live library replaces files under the same name
// (an episode upgraded in place) between the move and the reroot, so the rule is DOMINANCE
// rather than unanimity: the question is whether --to is the same filesystem as --from, and
// an unrelated tree matches almost nothing.
const rerootMinMatch = 0.95

// rerootMinMatched is the floor under the ratio: at least this many recorded files must
// match. One is enough, because a match is not a coincidence - the same relative path, the
// same size to the byte and the same modification second - and the ratio is what rules out
// the tree that matches almost nothing. A floor any higher would refuse a small library's
// honest move for want of files rather than for want of evidence.
const rerootMinMatched = 1

type rerootRequest struct {
	From, To string
	Apply    bool
	MinMatch float64
}

func cmdReroot(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reroot", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(stderr, rerootUsage) }
	from := fs.String("from", "", "the root the ledger's rows were recorded under")
	to := fs.String("to", "", "the configured library root the same files are mounted at now")
	apply := fs.Bool("apply", false, "rewrite the ledger; without it nothing is rewritten")
	minMatch := fs.Float64("min-match", rerootMinMatch, "the share of the recorded files found under --to that must match")

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
	// The NaN case fails both comparisons, so it is refused here too.
	if !(*minMatch > 0 && *minMatch <= 1) {
		fmt.Fprintf(stderr, "holdfast: --min-match %v is not a share: it must be above 0 and at most 1\n\n", *minMatch)
		fs.Usage()
		return exitUsage
	}
	log := logging.To(stderr, cfg.LogLevel)
	return runReroot(context.Background(), log, cfg,
		rerootRequest{From: *from, To: *to, Apply: *apply, MinMatch: *minMatch}, stdout, stderr)
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

	if !(req.MinMatch > 0 && req.MinMatch <= 1) {
		return refuse("min-match", "--min-match %v is not a share: it must be above 0 and at most 1", req.MinMatch)
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
	st, err := store.OpenExclusive(dbPath)
	if err != nil {
		reportMigrationRefusal(log, err)
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

	ev := checkRerootEvidence(plan.Evidence)
	printRerootEvidence(stdout, ev, to, req.MinMatch)
	if why := ev.refusal(from, to, req.MinMatch); why != "" {
		printMismatches(stderr, "retained original not the one recorded", ev.RetainedMismatched)
		return refuse("evidence", "%s", why)
	}

	if !req.Apply {
		logReroot(log, plan, ev, req.MinMatch, false)
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
	logReroot(log, done, ev, req.MinMatch, true)
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

// rerootEvidence is what the files under --to say about the plan's claim that they are the
// files the ledger recorded under --from.
//
// Replaced is the media files that exist but match no fingerprint recorded for their path:
// files replaced since the row was written. They count against the match ratio and are
// carried as recorded. RetainedMismatched is the retained originals that exist but are not
// the size of the original they hold, and ANY one refuses: a restore renames that file over
// a library file, so a retention is carried only where its file is the recorded one or is
// gone.
type rerootEvidence struct {
	Checked            int
	Matched            int
	Absent             int
	Replaced           []rerootMismatch
	RetainedMismatched []rerootMismatch
}

// ratio is the share of the recorded files found under --to that match, and 0 when none
// was found.
func (r rerootEvidence) ratio() float64 {
	found := r.Matched + len(r.Replaced)
	if found == 0 {
		return 0
	}
	return float64(r.Matched) / float64(found)
}

type rerootMismatch struct {
	Path, Recorded, Now string
}

// checkRerootEvidence stats every rewritten path the ledger records something about. A
// path that is not there is ABSENT - a file deleted since its row was written is the
// ordinary state of a ledger. A path that is there MATCHES when it is the file recorded: a
// media file matching ANY size:mtime the ledger holds for that path (a superseded row
// beside the current one records two), a retained original the size of the original it
// holds. A path that cannot be read matches nothing: it cannot vouch for anything.
//
// The stat FOLLOWS a symbolic link, as probe.Fingerprint does, and the attributes are read
// through probe.AttributesOf, the one derivation every recorded fingerprint came from.
func checkRerootEvidence(evidence []store.RerootEvidence) rerootEvidence {
	var res rerootEvidence
	for _, e := range evidence {
		res.Checked++
		fi, err := os.Stat(e.Path)
		switch {
		case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR):
			res.Absent++
		case err != nil && e.Retained:
			res.RetainedMismatched = append(res.RetainedMismatched, rerootMismatch{Path: e.Path, Recorded: recordedOf(e),
				Now: "unreadable (" + err.Error() + ")"})
		case err != nil:
			res.Replaced = append(res.Replaced, rerootMismatch{Path: e.Path, Recorded: recordedOf(e),
				Now: "unreadable (" + err.Error() + ")"})
		case e.Retained:
			if fi.Size() == e.RetainedBytes {
				res.Matched++
			} else {
				res.RetainedMismatched = append(res.RetainedMismatched, rerootMismatch{Path: e.Path, Recorded: recordedOf(e),
					Now: fmt.Sprintf("%d bytes", fi.Size())})
			}
		default:
			now := probe.AttributesOf(fi).String()
			if anyEquals(e.Fingerprints, now) {
				res.Matched++
			} else {
				res.Replaced = append(res.Replaced, rerootMismatch{Path: e.Path, Recorded: recordedOf(e), Now: now})
			}
		}
	}
	return res
}

func recordedOf(e store.RerootEvidence) string {
	if e.Retained {
		return fmt.Sprintf("a retained original of %d bytes", e.RetainedBytes)
	}
	return strings.Join(e.Fingerprints, " or ")
}

func anyEquals(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// refusal is why the evidence does not carry the plan, "" when it does.
func (r rerootEvidence) refusal(from, to string, minMatch float64) string {
	found := r.Matched + len(r.Replaced)
	switch {
	case r.Checked == 0:
		return fmt.Sprintf("the ledger records no file under %s whose size and modification time could be checked "+
			"under %s, so nothing shows %s holds the same files", from, to, to)
	case len(r.RetainedMismatched) > 0:
		return fmt.Sprintf("%d retained original(s) under %s are not the size of the original their retention "+
			"recorded (named above). A restore renames that file over a library file, so a retention is carried "+
			"only where its file is the one recorded or is gone", len(r.RetainedMismatched), to)
	case r.Matched < rerootMinMatched:
		return fmt.Sprintf("none of the %d file(s) the ledger recorded under %s exists under %s as the file it "+
			"recorded (%d found replaced, %d gone), so nothing shows %s holds the same files",
			r.Checked, from, to, len(r.Replaced), r.Absent, to)
	case r.ratio() < minMatch:
		return fmt.Sprintf("only %d of the %d recorded file(s) found under %s match what the ledger recorded "+
			"(a match ratio of %.3f, under the --min-match threshold of %g), so %s is not shown to be the same "+
			"files as %s: an unrelated tree matches almost nothing, and the same tree replaces only a few",
			r.Matched, found, to, r.ratio(), minMatch, to, from)
	}
	return ""
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

func printRerootEvidence(w io.Writer, r rerootEvidence, to string, minMatch float64) {
	fmt.Fprintf(w, "evidence: of %d file(s) the ledger recorded, %d exist under %s and match what it recorded, "+
		"%d were replaced since (carried as recorded), %d no longer exist (carried as recorded), and %d "+
		"retained original(s) are not the recorded size\n",
		r.Checked, r.Matched, to, len(r.Replaced), r.Absent, len(r.RetainedMismatched))
	fmt.Fprintf(w, "evidence: match ratio %d/%d = %.3f, threshold %g (--min-match)\n",
		r.Matched, r.Matched+len(r.Replaced), r.ratio(), minMatch)
	printMismatches(w, "replaced since (carried as recorded)", r.Replaced)
}

// printMismatches prints a bounded sample of files that are not what the ledger recorded,
// each with what it recorded and what is there now.
func printMismatches(w io.Writer, label string, list []rerootMismatch) {
	for i, m := range list {
		if i == rerootSample {
			fmt.Fprintf(w, "  ... and %d more\n", len(list)-rerootSample)
			break
		}
		fmt.Fprintf(w, "%s: %s - the ledger recorded %s, the file there is %s\n", label, m.Path, m.Recorded, m.Now)
	}
}

// logReroot writes what the command did as RECORDS: one per table, every column's count an
// attribute, then one for the whole, so a log pipeline can select on the table, the column
// or the evidence rather than parse a sentence. A plan that was not applied is recorded too,
// saying so, at the same level: both are what happened.
func logReroot(log *slog.Logger, p store.RerootPlan, ev rerootEvidence, minMatch float64, applied bool) {
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
		"evidence_replaced", len(ev.Replaced), "evidence_absent", ev.Absent,
		"evidence_retained_mismatched", len(ev.RetainedMismatched),
		"match_ratio", ev.ratio(), "min_match", minMatch)
}
