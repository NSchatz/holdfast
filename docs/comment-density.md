# Comment density

`make comment-density` measures how much of each Go file in this repository is comment
prose and refuses a file that is over the ceiling. It is a prerequisite of `make check`,
so CI and the release workflow run it too.

This document is the record the ceiling was derived from. It exists so the threshold is
auditable rather than asserted: a gate whose number nobody can re-derive is a number the
next person moves.

## What is counted

The Go parser decides what a comment is, so the count is by tokens and not by pattern.
`//` inside a URL, a raw string full of comment-looking lines, a `/*` inside a string
literal: none of those is prose, and a counter that matched patterns would score the
files that are mostly code as the ones to gut.

- Eligible file: a `.go` file that is not under a `testdata/` directory, does not carry
  the `Code generated ... DO NOT EDIT.` marker ahead of its package clause, and whose
  prose plus code lines total at least thirty. Below that floor one doc comment swings
  the ratio further than any judgement about the file could.
- Prose line: a line spanned by a comment and carrying no code.
- Code line: any other non-blank line. **A line carrying both code and a trailing comment
  counts as code**, because Go's trailing-comment idiom is ordinary code with a note on
  it. This is a deliberate divergence from the umbrella's Python counter, where the whole
  line is prose.
- Ratio of a file: prose / (prose + code). Blank lines outside a comment are in neither
  tally.
- P90: the ratio of the eligible file at nearest rank `ceil(0.90 * N)` with the eligible
  files sorted by ratio ascending.

A file that cannot be read or cannot be parsed is a failure naming the path, never a
skip and never a zero, and a walk that finds no eligible file at all is a failure too. An
unmeasured file that scores nothing is how this gate would rot into a permanent green.

## The thresholds, and the measurement they come from

The pre-trim measurement, taken over the whole tree with the gate present and no comment
yet deleted:

| number | value |
|---|---|
| eligible files | 159 |
| module-wide aggregate ratio | 24.9% prose (14586 prose lines, 43922 code lines) |
| maximum per-file ratio | 71.3% - `internal/store/store.go` (507 prose, 204 code) |
| P90 | 49.97% - `internal/engine/engine.go` (736 prose, 737 code) |

Deriving the ceiling C and the warn band W from that P90:

```
P90                              = 49.97%
rounded UP to a multiple of 5pp  = 50%
clamped to [30%, 50%]            = 50%   -> C = 50%
W = C - 10pp                     = 40%   -> W = 40%
```

The 50% cap is the umbrella's own ceiling and is there so a prose-heavy tree cannot
ratchet itself loose; the 30% floor is there so a comment-light tree does not get a gate
that reds on its next legitimate doc comment.

C and W were derived once, from the measurement above, and are now constants in
`internal/commentdensity`. They are never re-derived from a later measurement: a
threshold that follows the tree is a threshold that follows the drift it exists to catch.
Widening C to make a file fit is not available either. The escape hatch is the exemption
list beside the thresholds, capped at three entries, each naming a path and a reason, and
each entry checked on every run so that an exemption cannot outlive its file or the
reason it was granted for.

The P90 does not depend on the gate's own two files: at N = 157, N = 158 and N = 159 the
nearest rank lands on `internal/engine/engine.go` either way.

## Files that were over C before the trim

Fourteen, listed prose-heaviest first, with the ratio the pre-trim measurement gave them:

| file | pre-trim ratio | prose | code |
|---|---|---|---|
| `internal/store/store.go` | 71.3% | 507 | 204 |
| `internal/startup/startup.go` | 67.7% | 90 | 43 |
| `internal/engine/event.go` | 67.2% | 45 | 22 |
| `internal/vmaf/format.go` | 62.9% | 39 | 23 |
| `internal/engine/progress.go` | 59.0% | 111 | 77 |
| `internal/sourceoffer/sourceoffer.go` | 56.8% | 104 | 79 |
| `internal/fsclass/fsclass.go` | 56.3% | 125 | 97 |
| `internal/vmaf/vmaf.go` | 54.3% | 152 | 128 |
| `internal/store/migrate.go` | 54.2% | 240 | 203 |
| `scripts/release-shape-gate/regress_0046_F27_test.go` | 53.5% | 53 | 46 |
| `internal/webui/webui.go` | 53.1% | 76 | 67 |
| `internal/engine/verify.go` | 52.4% | 131 | 119 |
| `internal/startup/classify.go` | 52.3% | 58 | 53 |
| `internal/engine/swap.go` | 51.6% | 432 | 406 |

That set is exactly what was trimmed. No file the measurement did not flag was touched,
and nothing but comment text was: no `//go:` or `//lint:` directive was removed, and no
comment recording a safety invariant, a `rename-guard-allow` marker, or a rationale
nobody could re-derive from the code. What went was restatement and narrated history -
the same fact said three times, and the account of what the code used to do, which git
already holds.

## Post-trim

The same four numbers, re-measured after the trim: NOT YET MEASURED.
