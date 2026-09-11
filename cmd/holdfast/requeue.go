package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/store"
)

// `holdfast requeue` offers a file the engine has already answered back to the pipeline.
//
// A terminal row is a decision, and most decisions re-derive themselves: the scan
// re-opens a done or skipped row whose recorded inputs no longer match the configuration
// in front of it, so lowering min_bitrate_kbps or moving the encoder to a different target
// codec reaches the files it should. This command is for the rest - the rows no
// configuration key can reason about:
//
//	holdfast requeue --config c.yaml /library/film.mkv     # one file
//	holdfast requeue --config c.yaml --guard low-bitrate   # every row that guard skipped
//	holdfast requeue --config c.yaml --failed              # every row parked at max_failures
//
// It is LOCAL by design and is never an HTTP endpoint, for the reason `restore` is not
// one: it changes what the engine will do to a media file, and a mutating endpoint opens
// an authorization question the read-and-control API does not answer. That is a ratified
// operator decision, not a convenience.
//
// Every refusal exits NON-ZERO having changed nothing, and re-opening nothing is a
// refusal: an operator who is told a requeue succeeded will not come back to check, so a
// command that reported success against an empty match set would be worse than one that
// did not exist.

const requeueUsage = `holdfast requeue - offer a file the engine has already answered back to the pipeline

  holdfast requeue --config <file> <path>              re-open one file's terminal row
  holdfast requeue --config <file> --guard <token>     re-open every row that guard skipped
  holdfast requeue --config <file> --failed            re-open every row parked at max_failures

Flags:
  --config   path to the YAML config file (required)
  --guard    a skip guard's token; --guard '' is not a selector
  --failed   select every row whose attempts have reached max_failures

ONE selector per run. A path, --guard and --failed name three different sets, so two of
them together is a refusal rather than a guess at which one was meant.

Re-opening is not re-encoding: the guards run again, and a file that reaches the same
verdict reaches it in microseconds with nothing encoded and nothing written beside it.
A row parked at max_failures is the exception, and it is held by its attempt count rather
than by a verdict: re-opening one - by name or with --failed - hands the file its attempts
back, so whatever failed that encode will be attempted again.

A scan already re-opens a row whose recorded decision inputs no longer match the
configuration, so this is for what that cannot reach. Three rows are never re-opened, by
this command or by any configuration change: a job parked indeterminate, a swap recorded
applied-despite-error, and a file an operator restored through the undo window.
`

func cmdRequeue(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("requeue", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(stderr, requeueUsage)
		fmt.Fprintf(stderr, "\nGuards this build recognises:\n  %s\n", strings.Join(engine.SkipGuards, "\n  "))
	}
	guard := fs.String("guard", "", "a skip guard's token")
	failed := fs.Bool("failed", false, "every row parked at max_failures")

	cfg, code := loadConfig(fs, args, stderr)
	if cfg == nil {
		return code
	}
	rest := fs.Args()
	if len(rest) > 1 {
		fmt.Fprintf(stderr, "holdfast: requeue takes at most one path (got %d)\n", len(rest))
		return 2
	}

	sel := engine.RequeueSelector{Guard: *guard, Failed: *failed}
	if len(rest) == 1 {
		sel.Path = absoluteLedgerPath(rest[0])
	}
	if sel.Empty() {
		fmt.Fprintf(stderr, "holdfast: %v\n\n", engine.ErrNoSelector)
		fs.Usage()
		return 2
	}
	if given := sel.Given(); len(given) > 1 {
		fmt.Fprintf(stderr, "holdfast: %v\n\n", engine.ErrTooManySelectors{Given: given})
		fs.Usage()
		return 2
	}
	if sel.Guard != "" && !engine.KnownGuard(sel.Guard) {
		fmt.Fprintf(stderr, "holdfast: %v\n", engine.ErrUnknownGuard{Token: sel.Guard})
		return 2
	}

	// The ledger is OPENED, never created. `requeue` against a fresh install has nothing
	// to re-open, and a command that answered by writing an empty database into the state
	// directory would be creating the thing it was asked about.
	dbPath := filepath.Join(effectiveStateDir(cfg), "jobs.db")
	if _, err := os.Stat(dbPath); err != nil {
		fmt.Fprintf(stderr, "holdfast: there is no ledger at %s yet, so there is nothing to re-open "+
			"(run `holdfast run` or `holdfast serve` first)\n", dbPath)
		return 1
	}
	st, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "holdfast: opening job store: %v\n", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	return reportRequeue(context.Background(), st, sel, cfg, stdout, stderr)
}

// reportRequeue runs one requeue and says what it did, in the order an operator needs it:
// what was left alone and why FIRST, because that is the part they did not ask for, then
// what was re-opened and the count.
func reportRequeue(ctx context.Context, st store.Store, sel engine.RequeueSelector, cfg *config.Config, stdout, stderr io.Writer) int {
	res, err := engine.Requeue(ctx, st, sel, cfg.MaxFailures)
	for _, p := range res.Protected {
		fmt.Fprintf(stdout, "left alone: %s\n  %s\n", p.Path, p.Why)
	}
	if err != nil {
		var nothing engine.ErrNothingReopened
		if errors.As(err, &nothing) {
			fmt.Fprintf(stderr, "holdfast: %v\n", err)
			return 1
		}
		fmt.Fprintf(stderr, "holdfast: requeue failed: %v\n", err)
		return 1
	}
	for _, p := range res.Reopened {
		fmt.Fprintf(stdout, "re-opened: %s\n", p)
	}
	fmt.Fprintf(stdout, "re-opened %d row(s); the next scan runs the guards over them again.\n", len(res.Reopened))
	if res.AttemptsCleared > 0 {
		fmt.Fprintf(stdout, "%d of them were parked at max_failures and got their attempts back; "+
			"whatever failed those encodes will be attempted again.\n", res.AttemptsCleared)
	}
	if len(res.Protected) > 0 {
		fmt.Fprintf(stdout, "left %d row(s) alone (named above).\n", len(res.Protected))
	}
	return 0
}

// reportDecisionInputs writes the two counts a run and `validate` owe an operator: how
// many terminal rows record decision inputs that differ from the configuration in front of
// them, and how many record none at all. Both are rows the next scan will offer to the
// pipeline again, so the re-derivation is announced rather than discovered as a burst of
// unexplained activity.
//
// It writes one line per figure through fmt so `validate` can print it, and the caller
// that has a logger logs it too.
func decisionInputsReport(ctx context.Context, st store.Store, cfg *config.Config) (store.DecisionInputsSurvey, error) {
	return st.SurveyDecisionInputs(ctx, engine.DecisionInputsFor(*cfg))
}

// decisionInputsLines renders the survey as the operator-facing sentences both callers
// print, so `validate` and a daemon cannot describe the same ledger differently.
//
// BOTH FIGURES ARE ALWAYS NUMBERS, including when they are zero. "Nothing has moved" is
// the answer an operator most wants to be able to trust, and a sentence that only appears
// when there is something to report is indistinguishable from a report that was not
// attempted - so the zero is printed rather than summarised away. The one case with no
// figures is an empty ledger, where the two counts would describe a set that is not there.
func decisionInputsLines(s store.DecisionInputsSurvey) []string {
	if s.Reopening() == 0 && s.Matching == 0 {
		return []string{"the ledger holds no terminal row a configuration change could re-open"}
	}
	lines := []string{
		fmt.Sprintf("%d terminal row(s) were taken under a configuration that has since moved", s.Moved),
		fmt.Sprintf("%d terminal row(s) record no decision inputs at all (written before holdfast recorded them)", s.NotRecorded),
	}
	if s.Reopening() == 0 {
		return append(lines, fmt.Sprintf("so the next scan re-opens none of them: every one of the %d "+
			"terminal row(s) a configuration change could re-open was taken under the configuration in "+
			"force", s.Matching))
	}
	return append(lines, fmt.Sprintf("the next scan offers those %d file(s) to the guards again; that is a "+
		"re-decision, not a re-encode - a file that reaches the same verdict reaches it with nothing "+
		"encoded", s.Reopening()))
}

// absoluteLedgerPath turns what the operator typed into the path the ledger is keyed by.
// Library roots are absolute (Validate refuses anything else), so every recorded path is
// too - and an operator standing in their library and typing `ep.mkv` would otherwise be
// told nothing matches about a file that certainly does. An unresolvable path falls back
// to the input, so the refusal still names something they recognise rather than an error
// about the current directory.
func absoluteLedgerPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}
