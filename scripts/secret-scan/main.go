// Command secret-scan is the repository's secret scanner (secrets K4). It is invoked
// through scripts/secret-scan.sh - `make secret-scan`, `make secret-scan-selftest` and the
// pre-commit hook all go through that one wrapper - so there is a single definition of
// what the scan is and a single place its exit codes are decided.
//
// The three exit codes are the whole interface, and they are distinct on purpose: a caller
// RETRIES "could not run" and OBEYS "found a credential", so collapsing them into one
// non-zero code makes a broken scanner indistinguishable from a leak.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/NSchatz/holdfast/internal/secretscan"
)

// The exit codes. Documented here, printed by -h, and asserted by the self-test.
const (
	exitClean     = 0 // nothing found
	exitFound     = 3 // it ran, and a tracked file carries a credential or a forbidden name
	exitCannotRun = 4 // it could not run, so nothing is cleared
	exitUsage     = 2 // the invocation itself was wrong
)

const usage = `secret-scan - refuse a tree that carries an issued credential (secrets K4)

Usage:
  secret-scan [--staged] [--root DIR]

  --staged    scan the INDEX a commit would ship, not the worktree (the pre-commit path)
  --root DIR  the repository to scan (default: the current directory)

Exit codes:
  0  clean: every file in scope was read and none carried a credential
  3  FOUND: a tracked file carries a credential or is named like a credential store.
     Rotate the credential at its provider first, then remove it (secrets K6).
  4  COULD NOT RUN: git failed, a file in scope could not be read, or the enumeration
     was empty. Nothing is cleared by this code - a check that could not run has not
     passed. A caller RETRIES this one and OBEYS 3.
  2  the invocation was wrong (an unknown flag or a stray argument)

What it catches: high-signal vendor prefixes and forbidden filenames only. There is no
entropy heuristic, so it catches an ISSUED token and does NOT catch a password typed into
a config file, a bare base64 blob or a hand-made shared secret. See docs/secrets.md.
`

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("secret-scan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	staged := fs.Bool("staged", false, "scan the staged index rather than the worktree")
	root := fs.String("root", ".", "the repository to scan")
	if err := fs.Parse(args); err != nil {
		// -h/--help is a successful request for the usage text (which fs.Usage has just
		// printed, exit codes and all), not a bad invocation.
		if errors.Is(err, flag.ErrHelp) {
			return exitClean
		}
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "secret-scan: unexpected argument %q\n\n%s", fs.Arg(0), usage)
		return exitUsage
	}

	src := secretscan.Git{Root: *root, Staged: *staged}
	findings, err := secretscan.Scan(src, secretscan.Families())
	if err != nil {
		fmt.Fprintf(stderr, "::error::secret-scan COULD NOT RUN: %v\n", err)
		fmt.Fprintf(stderr, "secret-scan: exiting %d. Nothing has been cleared by this run.\n", exitCannotRun)
		return exitCannotRun
	}
	if len(findings) > 0 {
		fmt.Fprintf(stderr, "::error::secret-scan FOUND %d credential finding(s) in %s:\n",
			len(findings), src.Describe())
		for _, f := range findings {
			fmt.Fprintf(stderr, "  %s\n", f)
		}
		fmt.Fprintf(stderr, "secret-scan: exiting %d. ROTATE the credential at its provider BEFORE "+
			"removing it from the tree - a commit that deletes a secret without rotating it leaves a "+
			"live credential in the reflog (secrets K6).\n", exitFound)
		return exitFound
	}
	fmt.Fprintf(stdout, "secret-scan: clean - %s carried no credential of any covered family and no "+
		"forbidden filename.\n", src.Describe())
	return exitClean
}
