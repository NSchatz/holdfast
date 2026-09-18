# Mutation testing

Line coverage proves that a line EXECUTED. It proves nothing about whether anything
asserted on it: a suite can run every statement of the swap path and assert nothing about
what it did. This repository deletes a source file once a replacement has passed its
gates, and the gates are the tests, so "the suite ran" and "the suite would have noticed"
are not the same claim and only the second one is worth anything here.

A mutation run measures the second. It introduces a deliberate fault into one line, runs
the tests of the package that line is in, and records whether they failed. A fault the
tests notice is KILLED; one they run straight past is LIVED.

The runner is [gremlins](https://gremlins.dev). The Makefile owns its pin, the way it owns
`staticcheck` and `govulncheck`, and it is reached with `go run <module>@<version>`: it is
not a requirement of this module and does not appear in `go.mod`.

## The floor

<!-- mutation-floor -->
The floor is **70%**, and a run below it exits non-zero naming the measured score and the
floor.

It lives in `.gremlins.yaml`, as `unleash.threshold.efficacy`, and nowhere else. The
workflow does not carry it, the Makefile does not carry it, and neither run passes it on a
command line. `make mutation-shape` reads it out of that file and out of this document and
refuses to pass if the two disagree.

The remedy for a run below the floor is an assertion that kills a surviving mutant. It is
never a lower floor, and it is never a wider exclusion list: a floor moved to fit the
number it is measuring is not a floor.

## The score the floor is applied to

    mutation score = KILLED / (KILLED + LIVED)

That is the percentage of the faults the suite RAN that it also CAUGHT. Mutants the
compiler rejects (NOT VIABLE) are outside the calculation, and so are mutants on lines no
test reaches at all (NOT COVERED).

The other figure the runner reports, mutant coverage, is
`(KILLED + LIVED) / (KILLED + LIVED + NOT_COVERED)`. It measures REACH rather than
assertion, which is what the line-coverage figure already claims, and it is deliberately
NOT enforced here. One floor, one figure: a red run means the assertions are thin, and it
never means something else.

A mutant whose test run exceeded its time budget is TIMED OUT, and the runner puts it in
neither figure. The published report carries the count, because a run with many of them
measured the machine as much as it measured the suite. `.gremlins.yaml` sets the timeout
budget as a generous multiple of the coverage run, so that a busy machine does not quietly
shrink the number of mutants the score is computed over.

## The mutation domain

The domain is the module MINUS the list below. Every Go file outside these paths is
mutable, including files in packages that carry no tests at all: those score NOT COVERED,
which the floor's figure ignores, and they begin to count on the day someone tests them.

An excluded path is an UNMEASURED path. That is the honest weak point of this gate, which
is why the list is short, why every entry carries its reason here, and why
`make mutation-shape` refuses to let this table drift away from `.gremlins.yaml`.

An entry earns its place one of two ways: the package's tests need the pinned ffmpeg,
which the mutation job does not install, or the suite takes long enough that re-running it
once per mutant does not finish. The timings below are wall-clock `go test` measurements
of the package's own suite.

<!-- mutation-exclusions -->
| excluded path | why it is excluded |
|---|---|
| `^internal/engine/` | Real libx265 encodes behind the real verify gate. The Makefile records this suite at 524 to 568 seconds ALONE under `-race`, and a mutant re-runs it. |
| `^internal/vmaf/` | Drives libvmaf through the pinned ffmpeg: the instrument the no-loss verdict is measured with, and not something a mutation job should install in order to mutate. |
| `^cmd/holdfast/` | Drives real oneshot runs end to end - a real encode, a real VMAF measurement, a real swap - so it needs the pinned ffmpeg for the same reason `internal/engine` does. |
| `^internal/store/` | 143 seconds: sqlite migrations and ledger-scale fixtures, once per mutant. |
| `^internal/server/` | 97 seconds: the HTTP surface's own suite, once per mutant. |
| `^internal/metrics/` | 23 seconds: collector registration and scrape fixtures, once per mutant. |

What that leaves measured is the decision logic: configuration and profile selection, the
encoder matrix, skip and holdback rules, scheduling windows, the secret resolver and the
secret scanner, notification routing, disk accounting, filesystem classification and the
startup checks.

## The two runs

**On every pull request, diff-scoped.** Only the files inside the domain that the pull
request changed, measured against the merge base with its base branch, are mutated. That
is seconds of work, which is what makes it reasonable to bind every pull request. A small
diff is a harsh sample on purpose: one survivor out of two mutants is a 50% score and a
red run, and the remedy is a test that kills it.

A pull request that changes no Go file inside the domain reports that no mutant was in
scope and passes. Nothing to mutate is not a score of zero, and the published report says
`"in_scope": false` with a null score rather than inventing one.

**On a schedule, unscoped.** `03:23 UTC every Saturday`, against the default branch, in
`.github/workflows/mutation.yml`. This is the run the floor is a statement about: the
whole domain, every mutable line in it, re-running a package's suite once per mutant. It
is scheduled rather than gated because it is the run that can take a long time, and it
publishes its machine-readable report as an artifact of the run, red or green.

Both runs take the floor and the domain from `.gremlins.yaml`, through
`scripts/mutation.sh`. There is one definition of what a mutation run is.

## A red scheduled run

The run stays RED, and it also opens ONE issue on this repository, assigned to the
repository owner, carrying:

- the measured score, or the failure that prevented one,
- the floor,
- the URL of the failing run.

The issue carries a stable marker in its body, and the next consecutive failure UPDATES
that issue instead of opening a second one: a weekly job that files a fresh issue every
Saturday teaches its reader to close them unread. The job requests `issues: write` for
itself, because this repository's default workflow permission is read.

Nothing about the notification rescues the run. If the issue cannot be filed, that step
fails too, and the run was already red.

## Reproducing either run

    make mutation-diff REF=origin/main     # what a pull request runs
    make mutation-full                     # what the schedule runs
    make mutation-shape                    # the hermetic agreement gate, also part of `make check`
    make mutation-selftest                 # proves the gate still bites

`REF` is any git reference: the diff scope is taken against the merge base with it. The
unscoped run needs no network beyond the module proxy and no ffmpeg, because nothing in
the domain needs one.

Both runs write `mutation-report.json`: the mode, the reference, the floor, the score, the
mutant counts, and every file that was mutated with what became of its mutants. The file
is gitignored. It is the record of one run on one machine, and the run publishes it.

## What this gate cannot tell you

It cannot tell you that the excluded packages assert anything. `internal/engine` is the
most safety-critical code in this repository and it is the largest thing outside the
domain; what covers it is the fixture suite, the verify gates and the image smoke test,
not this score.

It cannot tell you that a KILLED mutant was killed by a MEANINGFUL assertion. A test that
fails because a panic escaped counts the same as one that checked a value.

And it cannot be read as coverage. A high score over a domain that excludes six packages
is a statement about those packages that remain, which is why the exclusion list is in
this document rather than only in a configuration file.
