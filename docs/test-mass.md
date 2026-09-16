# Test mass

How much of this repository is test code, what that code takes as its subject, and what
about it is still unmeasured. Every figure below is produced by `scripts/test-mass.sh` and
is re-derivable at the commit it names:

```sh
./scripts/test-mass.sh          # print the figures for this tree
./scripts/test-mass.sh -check   # re-derive them and compare against this document
```

`-check` exits non-zero and names the first figure that differs. It is wired into nothing:
`make check` does not run it, CI does not run it, and there is no threshold anywhere here. A
ratio is a figure, not a gate. The one thing `-check` can catch is this document going stale
against the tree, which is the failure a recorded number actually has.

## The measurement

<!-- test-mass:begin - written by scripts/test-mass.sh; re-derive with scripts/test-mass.sh -check -->
```text
commit e09a81963a367461c658cb209e7a30ccc3d5121e
module github.com/NSchatz/holdfast
production-lines 14489
test-lines 38414
test-to-production 2.65
package cmd/holdfast 6143
package internal/config 2184
package internal/corpus 78
package internal/diskfree 81
package internal/encoder 127
package internal/engine 12770
package internal/fsclass 139
package internal/hdr 222
package internal/heapmeasure 105
package internal/logging 34
package internal/metrics 502
package internal/notify 383
package internal/probe 634
package internal/schedule 374
package internal/secret 418
package internal/secretscan 234
package internal/server 4141
package internal/sourceoffer 248
package internal/startup 2901
package internal/store 5801
package internal/vmaf 895
```
<!-- test-mass:end -->

## What it counts, exactly

- **The set** is every `*.go` file git tracks under the module root. An untracked file
  cannot move a figure, and the set is reproducible from the commit named above.
- **A counted line** is one that, with leading whitespace removed, is neither empty nor
  beginning with `//`. So blank lines and comment lines are out, and the ratio is between
  bodies of code rather than between file sizes.
- **`/* */` block comments are not recognised.** There are none in this tree; a scanner for
  them mistakes the glob patterns that ARE here (`**/4K/**` carries both markers), and a
  figure that miscounts real lines is worse than one that would miscount a comment style
  nothing uses.
- **Test lines** are the counted lines of files named `*_test.go`; **production lines** are
  the rest. `test-to-production` is the first divided by the second.
- **Shell is not counted.** `scripts/*-selftest.sh` are test code by subject and several are
  large, but the figure above is about Go source, and a mixed count would be a number nobody
  could reproduce without agreeing on how to weigh a shell line against a Go one. Where a
  retired shell file mattered, the register below gives its raw line count.

The commit the block names is the commit the figures were taken at. A document cannot carry
the hash of the commit that contains it, so `-check` verifies instead that no counted file
differs between that commit and the tree it is run in, and refuses to compare anything when
one does.

## What this repository retired, and who grades it now

Each row took PRESENTATION OR PROCESS as its subject - the text of shipped Markdown, the
shape of a workflow file, the comment text of an example config - rather than the
transcode-and-delete behaviour this repository exists to be trusted about. The umbrella's
own fleet-wide `just lint` owns prose, document shape and release shape across every
repository it holds, and `testing` T2 rules that a second copy of that grading inside a
product suite is mass every stage session pays to read on every fix loop. That clause is why
each row below is gone from here, and `just lint` is where the grading lives now. Retirement
is decided by what a test takes as its SUBJECT and never by the directory it sat in, and two
of the things the first row's package carried took holdfast's own Go source as their
subject. The section after the table names them and says where each is asserted now.

| retired | what it graded | lines |
|---|---|---|
| `internal/docscheck` (package, 57 of the 58 test functions it carried, in `agreement_test.go`, `differentiator_test.go`, `docscheck_test.go`, `links_test.go`, `non_goal_library_manager_test.go`, `read_token_test.go`, `readme_plan_test.go`, `swap_metadata_test.go`) | the repository's Markdown corpus: that each shipped document carried a fixed anchor, that the statement under it said the clauses a table required, that documents agreed with each other, and that links resolved | 3,707 raw, less the 177 of `regress_0082_f7_test.go`, which is not one of them |
| `scripts/release-shape-gate` (Go command, 82 test functions in `expr_test.go`, `regress_0046_F22_test.go`, `regress_0046_F24_test.go`, `regress_0046_F26_test.go`, `regress_0046_F27_test.go`, `regress_0046_F29_test.go`, `regress_0065_test.go`, `version_test.go`, `workflow_test.go`) | the shape of `.github/workflows/release.yml`: which job held which permission, the order of the publishing steps, whether the operator runbook named every step, and whether each value a step was handed named the planning logic's output | 6,812 raw, of which 79 survive as `scripts/compose-image-ref` |
| `scripts/release-shape-selftest.sh` | that the release-shape gate still bit, by defeating each of its assertions against a mutated copy of the repository | 1,809 raw |
| `internal/config.TestConfigExample_DocumentsTheReadToken` | the comment text of `config.example.yaml` - that the shipped example spelled out what the read token gates and how to supply it | 39 raw |
| `make release-shape`, `make release-shape-selftest`, and the CI step that ran the self-test | the invocations that put the two above on the gate | - |

## What did not retire, and where it is asserted now

`testing` T2 reaches prose, styling, comment density, document shape and release shape. It
does not reach an assertion about holdfast's own Go source, and the umbrella's `just lint`
cannot own one: it grades what every repository in the fleet has in common, not this
package's vocabulary. Two things the retired package carried were of that kind: a test
function whose subject was `internal/engine`'s vocabulary, and the direct assertions a
corpus test made on the two helpers that were relocated rather than deleted. Neither is
gone. Each is asserted now in the package whose behaviour it is about.

- **Every terminal skip token is requeueable.** `internal/docscheck/regress_0082_f7_test.go`
  (177 raw) parsed `internal/engine`'s declarations and asserted that every terminal `Skip*`
  constant the package declares is in `engine.SkipGuards`, apart from the three mutable
  guards `SkipGuards` is documented as omitting. A token outside that list is answered
  `ErrUnknownGuard`, so `requeue --guard <token>` - the only lever over a verdict no
  configuration key can move - cannot reach it, and the exclusion is permanent and silent.
  The assertion is `internal/engine`'s `TestSkipGuards_EveryTerminalSkipTokenIsRequeueable`
  (96 raw), which reads the same declarations and checks membership against the running
  `SkipGuards`.
- **The corpus helper's own behaviour.** `internal/docscheck/docscheck_test.go` asserted
  `RepoRoot` and `Corpus` directly, inside a function that graded the shipped corpus with
  them. The function is one of the 57 and retired with the package; the assertions on the
  two helpers did not, because the helpers did not: they are `internal/corpus` now, and one
  of them is on a production path, since `internal/startup`'s `ScratchCorpus` reads the set
  `Markdown` produces. They are `internal/corpus/corpus_test.go` (96 raw): the root is the
  module root from any directory inside it, and the corpus is a walk that reaches a document
  at depth and leaves a vendored or VCS tree out.

Two things that look like graders and are NOT, so they stayed:

- `scripts/compose-image-ref` reads the one image reference `docker-compose.yml` names. It
  is a release capability rather than a check: `scripts/resolve-compose-image.sh`, which the
  release workflow runs, asks it for that reference and resolves it against the registry.
- `internal/corpus` locates the repository root and walks its Markdown. It is a helper two
  protected suites and `internal/startup` use, and a helper is not grading.

`internal/startup`'s document-text checks (`CheckCostStatement`, `CheckLocalSet`,
`CheckScratchStatement`, `CheckNoSourceDriveClaim`) are off-target by the same standard, and
they are still here: they have callers in `cmd/holdfast` PRODUCTION code, so retiring them
changes what the binary does at startup. That is a separate change with its own blast
radius, and it is not this one.

## What is not measured here: assertion quality

**holdfast reports LINE coverage only.** `make check` runs `go test -covermode=atomic`, and
what that produces is a statement-execution figure per package. Line coverage proves
EXECUTION, not ASSERTION: a test that runs a function and asserts nothing about what it
returned covers every line it touched, and a suite can sit at high coverage while missing
most of the faults a mutation would introduce.

Nothing here closes that gap. **`S0133-holdfast-mutation-score-floor` is the item that
measures assertion quality**, and it owns the mutation runner and whatever floor it sets.
This document defines no mutation-score threshold, adds no mutation runner, and neither does
anything else in this change: the gap is stated so that the coverage figures are read for
what they are.
