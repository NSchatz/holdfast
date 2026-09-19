package main

// The bounded run at the command surface (S0100): `run --file <path>` and `run --limit N`.
//
// Everything here happens BEFORE the engine is built, and that ordering is the criterion
// rather than a convenience. A `--file` path this configuration would never act on refuses
// the whole run at the door: before the library is walked, before ffmpeg is looked for,
// before the job store is opened - so a refused run has probed nothing, encoded nothing,
// mutated nothing and created nothing in or under the state directory.
//
// The two flags are NOT configuration. Every knob this tool runs under is declared in the
// YAML file and reachable from HOLDFAST_* and from nowhere else, which is what keeps a
// library from being processed under settings nothing in git records. A bound changes no
// setting: it says which files this invocation offers to the pipeline the configuration
// already describes, and every guard, gate and swap behind it is untouched.

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/engine"
)

// classifyScope is what narrows one run's start-or-refuse classification, and the target
// that narrowed it. The zero value is a whole-library classification, which is what every
// unbounded run and every `--limit` run takes.
type classifyScope struct {
	// Root is the library root this run acts under, as configured. Empty classifies
	// every configured root.
	Root string
	// File is the `--file` target the narrowing was taken for, carried so the record
	// says which path the decision was scoped to rather than only that it was.
	File string
}

// scoped reports whether this run's classification is narrowed at all.
func (s classifyScope) scoped() bool { return s.Root != "" }

// boundFlags are `run`'s two bounded flags as declared on its flag set.
//
// --limit is read as a STRING and turned into a count here rather than declared as an int
// flag, so that "not a count" and "a count below one" are refused by the same message
// naming the flag and the value the caller actually typed.
type boundFlags struct {
	file  *string
	limit *string
}

func addBoundFlags(fs *flag.FlagSet) boundFlags {
	return boundFlags{
		file: fs.String("file", "", "carry exactly this one file to a terminal outcome, and no other"),
		limit: fs.String("limit", "",
			"stop offering files once this many have reached a terminal outcome (an integer of at least 1)"),
	}
}

// resolveBound turns the two flags into the engine's bound, or refuses the run.
//
// It returns the bound, the classification scope a `--file` run may take, and the exit
// code: exitOK to carry on, exitUsage for an invocation this command cannot read, and
// exitRefused for a well-formed invocation naming a path this configuration would never
// act on. Those two are distinct because a caller treats them differently - fix your
// argv, or accept that the answer is no - and neither is exitError, which stays what it
// has always been: holdfast could not run.
func resolveBound(cfg *config.Config, f boundFlags, stderr io.Writer) (engine.Bound, classifyScope, int) {
	var b engine.Bound
	var scope classifyScope

	// A flag is ABSENT only when it carries the empty value the flag set defaults it to.
	// Anything else was typed and must mean something: `--limit " "` is a value that is
	// not a count, never a caller asking for no bound at all, because reading it as no
	// bound would turn a request for two files into a run over the library.
	if *f.limit != "" {
		n, err := strconv.Atoi(strings.TrimSpace(*f.limit))
		if err != nil || n < 1 {
			// The same code a missing --config returns, because it is the same kind of
			// fault: the invocation could not be read, and nothing about the host or the
			// library is implicated (cli L1).
			fmt.Fprintf(stderr, "holdfast: --limit: %q is not an integer of at least 1\n", *f.limit)
			return b, scope, exitUsage
		}
		b.Limit = n
	}

	if *f.file == "" {
		return b, scope, exitOK
	}
	target := strings.TrimSpace(*f.file)
	if target == "" {
		fmt.Fprintln(stderr, "holdfast: --file: no path was given")
		return b, scope, exitUsage
	}
	// A relative path is anchored against the working directory before anything judges
	// it, exactly as `restore` and `requeue` anchor the path they are given: an operator
	// typing a path at a shell means the file at that path.
	abs := absoluteLedgerPath(target)

	// THE ONE ELIGIBILITY DECISION, and it is the enumeration's own rules asked of a path
	// instead of a listing entry (engine.Eligibility.Judge): the path is resolved first,
	// then judged against the configured roots, the path filters, the retention areas, the
	// video extensions and this tool's own working-file names. A rule added there reaches
	// this flag with no second edit, which is what stops `--file` becoming a second, laxer
	// door into a pipeline that deletes sources.
	resolved, bad := engine.NewEligibility(*cfg).Judge(abs)
	if bad != nil {
		fmt.Fprintf(stderr, "holdfast: refusing the run: --file %s: %s: %s\n", target, bad.Rule, bad.Detail)
		return b, scope, exitRefused
	}
	// Readable BY THIS PROCESS, asked here because the alternative is discovering it
	// inside the pipeline, where the file would already have a ledger row and the state
	// directory would already exist. Opening for reading creates nothing and changes
	// nothing - not the file's bytes, not its modification time.
	fh, err := os.Open(resolved)
	if err != nil {
		fmt.Fprintf(stderr, "holdfast: refusing the run: --file %s: this process cannot read %s: %v\n",
			target, resolved, err)
		return b, scope, exitRefused
	}
	_ = fh.Close()

	b.File = resolved
	scope = classifyScope{Root: rootHolding(cfg, resolved), File: resolved}
	return b, scope, exitOK
}

// rootHolding names the configured library root, as configured, that holds a resolved
// path. Judge has already established that one does, so the empty answer is unreachable
// through resolveBound - and it is the right answer anyway: no root, no narrowing.
func rootHolding(cfg *config.Config, resolved string) string {
	for _, r := range cfg.RootProfiles() {
		if r.Contains(resolved) {
			return r.Path
		}
	}
	return ""
}

// wantsHelp reports whether this invocation is asking for the help text rather than a run.
// It is answered before the flag set parses, so `run --help` prints the whole surface -
// flags, examples and the exit-code table - on STDOUT, where requested output belongs
// (cli L2, L5), rather than the flag package's own defaults on stderr.
func wantsHelp(args []string) bool {
	for _, a := range args {
		switch a {
		case "-h", "--help", "-help":
			return true
		case "--":
			return false
		}
	}
	return false
}

// runUsage is `holdfast run --help`: every flag, one runnable example per shape, and every
// code the command can return with its meaning, rendered from the one declared table
// (cli L1, L5 - see exitcodes.go).
func runUsage() string {
	return `holdfast run - one pass over the configured library roots

Usage:
  holdfast run --config <path> [--file <path>] [--limit <n>]

A run probes, guards, encodes, verifies and swaps for real: it is licensed to replace a
source with its encode and to delete the original. The two bounds below make it act on
ONE file, or on a handful, with every guard, gate and swap intact - so the pipeline can be
watched deciding a real file before it is pointed at a library.

A bounded run is narrower, never weaker. It runs neither the stale-temp sweep nor the
ledger retention pass, because both conclude from an absence that only a whole-library
pass is evidence for, and it removes no ledger row at all.

Flags:
  -config <path>   path to the YAML config file (required)
  -file <path>     carry exactly this one file to a terminal outcome, and no other. The
                   path must be one an ordinary scan of the configured roots would act on
  -limit <n>       stop offering files once n have reached a terminal outcome, where a
                   skip with a reason counts and a file merely enumerated does not

Examples:
  holdfast run --config config.yaml --file /media/tv/pilot.mkv   # one real file, end to end
  holdfast run --config config.yaml --limit 5                    # the first five decisions
  holdfast run --config config.yaml                              # the whole library

Exit codes:
` + runExitCodeTable()
}
