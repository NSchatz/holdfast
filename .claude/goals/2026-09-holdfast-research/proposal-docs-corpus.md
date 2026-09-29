# P6 - should the documentation corpus skip `.claude/`? (I16)

## What this decides

Whether `internal/corpus` keeps walking `.claude/`, so that the program's own Markdown (the
brief, the goal ledgers, these proposals and `NEEDS-OWNER.md`) stays input to the mechanical
documentation checks, or skips it, so that program files can neither satisfy a presence check
nor trip an absence check. Review finding M4 raised it and inferred row I16 parked it here. Until
the owner decides, the brief's workaround stands (§4): program Markdown avoids the vocabulary
the absence checks count, and every new statement or metric is proven present in `README.md` or
`docs/` by `git grep`, never by a ledger line.

## Where things stand

Read on `main` at `30d245f`, 2026-09-29.

- `corpus.Markdown` (`internal/corpus/corpus.go:39`) walks every `.md` file under the
  repository root and skips only `.git`, `vendor` and `node_modules`
  (`internal/corpus/corpus.go:47`). It walks the FILESYSTEM, not `git ls-files`, so an
  untracked Markdown file in a checkout is in the set too. `corpus.Documents`
  (`internal/corpus/corpus.go:77`) is that set plus `config.example.yaml`.
- Two kinds of check read the set:
  - **Presence checks**, where any file in the set can satisfy the rule: every published
    Prometheus metric name appears in some document (`internal/docscheck/docscheck_test.go:23`
    reads `corpus.Markdown`), the anchored posture statements exist with their clauses
    (`CheckDynamicHDR`, `internal/docscheck/dynamichdr.go:110`; the interlacing and enumeration
    statements; the stream-selection statement in `internal/startup/stream_docs_test.go:29`).
  - **Absence checks**, where any file in the set can break the rule: at most one document may
    make a claim about picture-size reduction (`CheckDownscaleStatedOnce`,
    `internal/docscheck/downscale.go:219`), a retired claim about one source kind may not come
    back (`CheckRetired`, `internal/docscheck/interlacing.go:307`), and no document may claim
    that a scratch location spares the source drive work (`CheckNoSourceDriveClaim` over
    `ScratchCorpus`, `internal/startup/scratchdocs.go:140` and `:169`).
- `.claude/` holds 29 Markdown-or-text files today (the brief, 15 goal files, the research, this
  ledger and `NEEDS-OWNER.md`), and the program adds a ledger per goal. All of it is public
  and rendered by GitHub, and `README.md` now links the brief as the plan of record (R6), but
  none of it states what the build does: it states what the owner decided and what goals will
  build.
- The mechanical guards that are NOT corpus-based keep scanning `.claude/` whatever this
  decides: the rename guard (`scripts/check-pins.sh` section 4), `make secret-scan` and
  `make identity-scan` all read every tracked file.

The two failure modes the current walk has:

1. **False green.** A presence check is satisfied by a program file. A new metric named only in
   a ledger's evidence table, or a posture clause quoted in a proposal, passes the check while
   `README.md` and `docs/` say nothing: the reader the check protects is told nothing.
2. **False red, and prose written around it.** An absence check counts a program file that
   describes a planned feature in the words a shipped claim would use, and the gate goes red on
   a plan. The workaround is what the brief does today: write the counted vocabulary only
   inside code spans and never restate a statement the checks own. It works, and it costs every
   goal attention, and it fails the day a ledger quotes a PR body.

## Options

- **(a) Skip `.claude/` in `corpus.Markdown`.** One more directory name beside `.git`,
  `vendor` and `node_modules` (`internal/corpus/corpus.go:47`), and one test in
  `internal/corpus/corpus_test.go` that a Markdown file under `.claude/` (at the top and nested)
  is not in the set while one under `docs/` still is. Costs: a small change to a package inside
  the mutation domain (the new test must kill the mutants of the changed walk); a document
  meant for readers must not be put under `.claude/` (a directory named for an agent tool
  holds none today); the brief's §4 workaround can retire, and later goals write program
  Markdown freely. Presence checks are then satisfied only by `README.md`, `docs/`,
  `config.example.yaml` and any other shipped document. Benefit: removes both failure modes
  at their root.
- **(b) Keep the walk as it is.** No code change. Costs: both failure modes stay; every goal's
  program Markdown keeps the vocabulary discipline, and a presence check can go green on a
  ledger (the `git grep -- README.md docs/` proof in §4 is a convention, not a check).
- **(c) Split the set by kind of check.** Presence checks read only shipped documents
  (`README.md`, `docs/`, `config.example.yaml`); absence checks keep reading everything,
  `.claude/` included. Costs: two corpus functions and every caller re-pointed (docscheck's
  metric and statement checks, the stream-selection test, `ScratchCorpus`), each with a test;
  the false-red half stays, so the vocabulary discipline stays. Benefit: a stray contradicting
  claim anywhere in the repository, plans included, still fails.
- **(d) Skip every dot-directory.** Like (a), but also `.github/` and any future one. Costs:
  broader than the problem; `.github/` carries no Markdown today, and a pull-request template
  added there later would silently leave the corpus. Benefit: none over (a) today.

## Recommendation

Adopt option (a): `internal/corpus` skips `.claude/` exactly as it skips `.git`, `vendor` and
`node_modules`, with a test that proves a `.claude/` document is out and a `docs/` document is
in, and the corpus's doc comment and `docs/test-mass.md` (which describes the corpus) say so.
The checks exist to grade what a stranger is told about the build, and the program's files are
decisions and plans; letting them satisfy a presence check is the false green M4 named, and
making them trip an absence check taxes every goal for no reader's benefit. Goal 2 carries it,
as a carried item with its own test, and the §4 vocabulary rule is lifted in the same PR. Until
the owner approves, nothing changes and the §4 rule holds.

## Test plan

- `internal/corpus`: a temporary tree with `docs/a.md`, `.claude/goals/b.md`, `.claude/c.md`
  and `x/.claude/d.md`; `Markdown` returns `docs/a.md` only among them; the existing depth and
  VCS-exclusion cases keep passing. The test fails if the `.claude` case is deleted.
- `internal/docscheck` and `internal/startup`: the corpus-based tests keep passing on the real
  tree, and each bite test keeps failing on its old wording (they build their inputs in memory,
  so the change cannot weaken them).
- A regression proof for the false green: a presence test fed a corpus in which only a
  `.claude/` file names a metric reports it undocumented.
- `make mutation-diff REF=origin/main` under the heavy lock before the PR is pushed; the PR's
  `mutation` job green.

## Claims re-verified

| Claim | Source (read 2026-09-29) | Outcome |
|---|---|---|
| The walk skips only `.git`, `vendor`, `node_modules` | `internal/corpus/corpus.go:47` at `30d245f` | confirmed |
| The walk is of the filesystem, not of tracked files | `internal/corpus/corpus.go:39-61` (`filepath.WalkDir`) | confirmed |
| The metric-name presence check reads `corpus.Markdown` | `internal/docscheck/docscheck_test.go:23` | confirmed |
| The scratch-claim absence check reads the same walk | `internal/startup/scratchdocs.go:169-175` | confirmed |
| The rename guard, secret scan and identity scan read every tracked file, not the corpus | `scripts/check-pins.sh` section 4 (`git grep`), `scripts/secret-scan.sh`, `scripts/identity-scan.sh` | confirmed |
| `.github/` holds no Markdown today | `git ls-files .github` at `30d245f` | confirmed |

## Sources

- `internal/corpus/corpus.go`, `internal/corpus/corpus_test.go` at `30d245f`. Read 2026-09-29.
- `internal/docscheck/docscheck_test.go`, `dynamichdr.go`, `downscale.go`, `interlacing.go` at
  `30d245f`. Read 2026-09-29.
- `internal/startup/scratchdocs.go`, `internal/startup/stream_docs_test.go` at `30d245f`. Read
  2026-09-29.
- The brief `.claude/goals/2026-09-holdfast.md` §4 (program Markdown is scanned too), I16, and
  §23 finding M4; `review-brief-v1.md` finding M4. Read 2026-09-29.
