package main

// `run`'s exit-code contract, declared ONCE (cli L1, L5).
//
// The exit code is what a caller acts on with stderr discarded, so the set of codes and
// what each means is a contract rather than an implementation detail. It is declared here,
// in one place, and it has exactly two readers: the `run --help` text a caller discovers
// the surface from, and the check that holds the two in agreement. A code returned by the
// command and absent from this table therefore fails a test rather than shipping as a
// number nobody documented.
//
// The numbers are per-repo (cli L1a) and these are holdfast's. 0, 1 and 2 are what `run`
// has always spent and are unchanged; 3 is added here because a POLICY REFUSAL is a third
// thing a caller treats differently. It is not 1, because 1 means holdfast could not run
// and a caller retries that after fixing the host; it is not 2, because 2 means the
// invocation was malformed and a caller fixes its own argv. A `--file` path this
// configuration would never act on is neither: the invocation was well formed, the host
// is fine, and the answer is no. 3 is the first number free after the two already spent,
// it is clear of the region POSIX fixes (126 not-executable, 127 not-found, 128+ signal),
// and the Go tools a holdfast caller sits beside answer on 1 and 2 alone, so nothing in
// the ecosystem gives 3 a conflicting meaning.

import (
	"fmt"
	"strings"
)

const (
	// exitOK is the run doing what it was asked to do.
	exitOK = 0
	// exitError is holdfast being unable to run: an unreadable or invalid config, a
	// startup refusal, a capability that is not there, a job store it cannot open.
	exitError = 1
	// exitUsage is a malformed invocation: an unknown flag, a missing --config, a
	// --limit that is not a count.
	exitUsage = 2
	// exitRefused is a run refused by policy: it was well formed and holdfast could have
	// run it, and the answer is no. Nothing was probed, encoded or mutated.
	exitRefused = 3
)

// runExitCode is one code `run` can return, with the meaning a caller keys off.
type runExitCode struct {
	Code    int
	Meaning string
}

// runExitCodes IS the table. Both the help text and its check read this slice, so the two
// cannot disagree and a code added to the command without a row here has nowhere to hide.
var runExitCodes = []runExitCode{
	{exitOK, "the run finished: every file it offered reached an outcome and the ledger records each one"},
	{exitError, "holdfast could not run: an unreadable or invalid config, a startup refusal (storage, " +
		"scratch directory, a missing binary, an encoder or VMAF capability), or a job store it could not open"},
	{exitUsage, "the invocation was wrong: an unknown flag, no --config, or a --limit that is not a count"},
	{exitRefused, "the run was refused: --file named a path this configuration would never act on. " +
		"Nothing was probed, encoded or mutated, and nothing was created under the state directory"},
}

// runExitCodeTable renders the table for `run --help`, one indented line per code.
func runExitCodeTable() string {
	var b strings.Builder
	for _, c := range runExitCodes {
		fmt.Fprintf(&b, "  %d  %s\n", c.Code, c.Meaning)
	}
	return b.String()
}
