package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// `holdfast resolve` is the WAY OUT of the parked state (FILESYSTEM-1).
//
// A swap whose outcome holdfast could not establish leaves a job parked with both files
// intact, and nothing in the engine will ever touch either of them again on its own.
// That is the right fail-safe and it would be an unforgivable one to ship without an
// exit, because a state with no exit is a state that silently withdraws files from a
// library forever. This command is that exit, and it is deliberately a first-class
// subcommand rather than a hand-edit of jobs.db: an operator resolving an incident is
// exercising judgement holdfast does not have, not repairing a database.
//
// Two shapes, and the first one is the important one:
//
//	holdfast resolve --config c.yaml                 # list every parked job
//	holdfast resolve --config c.yaml --id 3          # REPORT one: both recorded paths,
//	                                                 # and what is at each RIGHT NOW
//	holdfast resolve --config c.yaml --id 3 \        # ... and record the determination
//	    --determination swap-was-applied \
//	    --replacement delete
//
// The report is never conditional on observing either file. A recorded path with
// nothing at it, or one that cannot be inspected, is REPORTED AS THAT - naming which of
// the two it is - and the job is still resolvable on instruction. Refusing to act
// because a file an operator already deleted by hand is missing would trap the job in
// exactly the state this command exists to leave.

// removeFile is the ONE deletion this program performs on an operator's explicit
// instruction, behind a seam so a test can prove what happens when it does NOT succeed -
// the unhappy path of the only licensed removal in holdfast is not one to leave
// unexercised. Production is os.Remove.
var removeFile = os.Remove

// resolveUsage is printed for -h and for a malformed invocation.
const resolveUsage = `holdfast resolve - report and resolve a job whose swap outcome could not be established

  holdfast resolve --config <file>                      list every parked job
  holdfast resolve --config <file> --id <n>             report one parked job
  holdfast resolve --config <file> --id <n> \
      --determination swap-was-applied|source-is-intact \
      --replacement retain|delete                       record the determination

Flags:
  --config          path to the YAML config file (required)
  --id              the parked job's id, as the list and the run log print it
  --determination   what you determined actually happened to the rename
  --replacement     what to do with the retained replacement file:
                      retain  keep it, and keep it out of enumeration
                      delete  remove it (ordered only AFTER the resolution is durable)

The source path is always kept in place: it re-enters enumeration and later runs treat
it normally. Under "the swap was applied" the file there is the replacement and the
ordinary already-at-target-codec guard skips it; under "the source is intact" the swap
never happened, so a later run is free to encode and swap it again.
`

func cmdResolve(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("resolve", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(stderr, resolveUsage) }
	id := fs.Int64("id", 0, "the parked job's id")
	determination := fs.String("determination", "", "swap-was-applied | source-is-intact")
	replacement := fs.String("replacement", "", "retain | delete")

	cfg, code := loadConfig(fs, args, stderr)
	if cfg == nil {
		return code
	}

	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "holdfast: %v\n", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	if *id == 0 {
		return listParked(ctx, st, stdout, stderr)
	}
	return resolveOne(ctx, st, *id, *determination, *replacement, stdout, stderr)
}

// openStore opens the job store the configured state directory holds, using the same
// defaulting buildEngine applies so `resolve` can never open a DIFFERENT database from
// the one the run wrote to.
func openStore(cfg *config.Config) (store.Store, error) {
	stateDir := cfg.StateDir
	if stateDir == "" {
		stateDir = "state"
	}
	st, err := store.Open(filepath.Join(stateDir, "jobs.db"))
	if err != nil {
		return nil, fmt.Errorf("opening job store: %w", err)
	}
	return st, nil
}

func listParked(ctx context.Context, st store.Store, stdout, stderr io.Writer) int {
	parked, err := st.ParkedIncidents(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "holdfast: reading parked jobs: %v\n", err)
		return 1
	}
	if len(parked) == 0 {
		fmt.Fprintln(stdout, "no parked jobs")
		return 0
	}
	fmt.Fprintf(stdout, "%d parked job(s):\n", len(parked))
	for _, in := range parked {
		fmt.Fprintf(stdout, "\n  id %d\n", in.ID)
		fmt.Fprintf(stdout, "    source      %s\n", in.SourcePath)
		fmt.Fprintf(stdout, "    replacement %s\n", in.ReplacementPath)
		fmt.Fprintf(stdout, "    recorded    %s\n", in.SwapError)
	}
	fmt.Fprintln(stdout, "\nrun `holdfast resolve --config <file> --id <n>` to see what is at each path now.")
	return 0
}

// observation is what is at ONE recorded path right now: its current attributes, or the
// fact that there is no file there, or the fact that it could not be inspected -
// naming which of the two it is. It is never an empty string standing in for any of them.
type observation struct {
	Text string
	// Absent and Uninspectable are distinct on purpose. "Somebody deleted it" and
	// "holdfast cannot look at it" call for opposite responses from an operator, and
	// collapsing them into "no attributes" hides which one happened.
	Absent        bool
	Uninspectable bool
}

func observe(path string) observation {
	attrs, err := probe.StatAttributes(path)
	switch {
	case err == nil:
		return observation{Text: attrs.String()}
	case errors.Is(err, os.ErrNotExist):
		return observation{Text: "ABSENT (no file at this path)", Absent: true}
	default:
		return observation{Text: "UNINSPECTABLE (" + err.Error() + ")", Uninspectable: true}
	}
}

func resolveOne(ctx context.Context, st store.Store, id int64, determination, replacement string, stdout, stderr io.Writer) int {
	in, ok, err := st.IncidentByID(ctx, id)
	if err != nil {
		fmt.Fprintf(stderr, "holdfast: reading job %d: %v\n", id, err)
		return 1
	}
	// A job that is not parked - already resolved, or never indeterminate, or simply
	// not there. Reported DISTINCTLY from the absent-file case above, because they are
	// opposite situations, and nothing is touched: no file, no record.
	if !ok || !in.Parked() {
		fmt.Fprintf(stderr, "holdfast: id %d is not a parked job", id)
		switch {
		case !ok:
			fmt.Fprintln(stderr, " (no job with that id) - nothing was changed")
		case in.Resolution != "":
			fmt.Fprintf(stderr, " (already resolved as %q) - nothing was changed\n", in.Resolution)
		default:
			fmt.Fprintf(stderr, " (its recorded outcome is %q) - nothing was changed\n", in.Outcome)
		}
		return 1
	}

	// The REPORT. It comes before anything else and it is unconditional: both recorded
	// paths, and for each of them either the attributes the file there has right now or
	// the fact that it is absent or uninspectable.
	obsSource := observe(in.SourcePath)
	obsRepl := observe(in.ReplacementPath)
	fmt.Fprintf(stdout, "parked job %d\n", in.ID)
	fmt.Fprintf(stdout, "  recorded:    %s\n", in.SwapError)
	fmt.Fprintf(stdout, "  storage:     %s\n", storageText(in))
	fmt.Fprintf(stdout, "  source path        %s\n", in.SourcePath)
	fmt.Fprintf(stdout, "    recorded before the swap  %s\n", in.SourceAttrs)
	fmt.Fprintf(stdout, "    now                       %s\n", obsSource.Text)
	fmt.Fprintf(stdout, "  replacement path   %s\n", in.ReplacementPath)
	fmt.Fprintf(stdout, "    recorded before the swap  %s\n", in.ReplacementAttrs)
	fmt.Fprintf(stdout, "    now                       %s\n", obsRepl.Text)

	if determination == "" {
		fmt.Fprintln(stdout, "\nno determination given - nothing was changed.")
		fmt.Fprintln(stdout, "re-run with --determination swap-was-applied|source-is-intact and --replacement retain|delete")
		return 0
	}

	det := store.Determination(determination)
	if !det.Valid() {
		fmt.Fprintf(stderr, "holdfast: %q is not a determination (want swap-was-applied or source-is-intact) - nothing was changed\n", determination)
		return 2
	}

	// A disposition is required for EACH of the two paths, and a resolution that leaves
	// either without one is refused. The source's is fixed by the contract: under both
	// determinations it is kept-in-place, which means exactly what it says - the path
	// re-enters enumeration and no hold-back is placed on it.
	dispSource := store.KeptInPlace
	dispRepl, code := replacementDisposition(replacement, obsRepl, stderr)
	if code != 0 {
		return code
	}

	// AC15k, and the ordering is the whole point: a durable resolution record is the
	// PRECONDITION for removing anything. The record goes in FIRST, so no file is ever
	// removed while the resolution that authorised it could still be lost. If the store
	// cannot be written, nothing is deleted or moved, the failure is reported with both
	// paths, the determination and both dispositions, and the job stays PARKED with
	// this command re-invocable - a store failure costs a repeated instruction and
	// never an unrecorded deletion.
	res := store.Resolution{
		Determination:          det,
		By:                     "operator",
		ObservedSource:         obsSource.Text,
		ObservedReplacement:    obsRepl.Text,
		DispositionSource:      dispSource,
		DispositionReplacement: dispRepl,
	}
	if err := st.ResolveIncident(ctx, id, res); err != nil {
		fmt.Fprintf(stderr, "holdfast: COULD NOT PERSIST the resolution - nothing was deleted or moved and the job is STILL PARKED\n")
		fmt.Fprintf(stderr, "  source path             %s\n", in.SourcePath)
		fmt.Fprintf(stderr, "  replacement path        %s\n", in.ReplacementPath)
		fmt.Fprintf(stderr, "  determination           %s\n", det)
		fmt.Fprintf(stderr, "  disposition (source)      %s\n", dispSource)
		fmt.Fprintf(stderr, "  disposition (replacement) %s\n", dispRepl)
		fmt.Fprintf(stderr, "  store error             %v\n", err)
		fmt.Fprintf(stderr, "re-run this command once the job store is writable and it will resolve.\n")
		return 1
	}

	fmt.Fprintf(stdout, "\nrecorded: %s (by operator)\n", det)
	fmt.Fprintf(stdout, "  source path        %s -> %s\n", in.SourcePath, dispSource)
	fmt.Fprintf(stdout, "  replacement path   %s -> %s\n", in.ReplacementPath, dispRepl)

	// Only now, with the record durable, is a removal licensed.
	if dispRepl == store.Deleted {
		if err := removeFile(in.ReplacementPath); err != nil {
			// The licensed removal did not succeed. The record must not go on claiming
			// a deletion that did not happen: the disposition is corrected back to
			// retained-excluded, which keeps the surviving file OUT of enumeration for
			// as long as the record lives, and the failure is reported. The job stays
			// RESOLVED - the operator's determination is recorded, durable and correct;
			// it is only the removal that failed - so re-running with --replacement
			// delete is not the remedy, removing the file by hand is.
			fmt.Fprintf(stderr, "holdfast: the resolution is recorded, but REMOVING the replacement failed: %v\n", err)
			if amendErr := st.AmendReplacementDisposition(ctx, id, store.RetainedExcluded, err.Error()); amendErr != nil {
				fmt.Fprintf(stderr, "holdfast: could not correct the recorded disposition either: %v\n", amendErr)
				fmt.Fprintf(stderr, "  the file at %s is still there and is still held out of enumeration by its name\n", in.ReplacementPath)
				return 1
			}
			fmt.Fprintf(stderr, "  the file at %s is still there; its disposition is now %s, so it stays out of enumeration\n",
				in.ReplacementPath, store.RetainedExcluded)
			return 1
		}
		fmt.Fprintf(stdout, "  removed %s\n", in.ReplacementPath)
	}
	return 0
}

// replacementDisposition turns the operator's instruction into the disposition that is
// actually recorded.
//
// An ABSENT file is recorded absent whatever was asked, because `deleted` and
// `retained-excluded` both assert something about a file that is not there. That is not
// overriding the operator: there was nothing to dispose of, and saying so is the honest
// record. An UNINSPECTABLE path is NOT treated as absent - holdfast could not look, which
// is not the same as looking and finding nothing - so the instruction stands.
func replacementDisposition(instruction string, obs observation, stderr io.Writer) (store.Disposition, int) {
	if obs.Absent {
		return store.Absent, 0
	}
	switch instruction {
	case "retain":
		return store.RetainedExcluded, 0
	case "delete":
		return store.Deleted, 0
	case "":
		fmt.Fprintln(stderr, "holdfast: --replacement is required with --determination "+
			"(retain | delete) - a resolution that leaves a path without a disposition is not accepted; nothing was changed")
		return "", 2
	default:
		fmt.Fprintf(stderr, "holdfast: %q is not a replacement disposition (want retain or delete) - nothing was changed\n", instruction)
		return "", 2
	}
}

func storageText(in store.SwapIncident) string {
	if in.StorageType == "" {
		return in.StorageClass
	}
	return in.StorageClass + " (" + in.StorageType + ")"
}
