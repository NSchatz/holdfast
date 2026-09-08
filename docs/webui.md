# The dashboard: how it is built and how it is graded

`holdfast serve` ships **one** self-contained `index.html`, embedded in the binary with
`go:embed` and served at `/` under a tight Content Security Policy. That has not changed.
What changed (WEBUI-10) is that the page's JavaScript is no longer 446 lines inside the
HTML file: it lives in source modules, a Go generator inlines them back into the one
embedded document, and the behaviour those modules describe is under two suites.

## Layout

```
internal/webui/
  index.html                GENERATED and COMMITTED. The file go:embed puts in the binary.
  src/
    index.html.tmpl         the page shell: markup, plus one marker for the stylesheet
                            and one for the script
    tokens.css              the ONE committed token file: every colour and every length
                            the surface paints, in both themes, with each pair's measured
                            contrast ratio recorded beside it
    dashboard.css           the rules, which declare no colour and no length of their own
    js/modules.txt          the modules, in the order they are concatenated
    js/10-constants.js      the closed vocabularies the page reads off the wire
    js/20-derive.js         the VALUE DERIVATIONS. No DOM reference at all
    js/30-dom.js            createElement + textContent, and nothing else
    js/40-cells.js          one cell renderer per column
    js/50-rows.js           the rows, cloned from the document's own <template>s
    js/60-aggregates.js     the whole-ledger cards
    js/70-render.js         one snapshot in, the page it describes out
    js/55-states.js         the three states every view owes: loading, empty, unreadable
    js/80-wire.js           the controls, the ticker and the SSE stream. The only module
                            that RUNS anything at load time
    test/                   the derivation unit suite (node's built-in test runner)
  gen/                      the generator, Go and stdlib only
  gen/genindex/             its command
  e2e/                      the rendered graders (Playwright). TEST ONLY - see below
    fixtureserver/          a Go command that mounts the REAL webui.HandlerFor
    fixtures/               the snapshots the scenarios serve
    driver.mjs              the one engine driver, for the Go graders that operate a browser
    specs/probe.mjs         the measuring script, which decides nothing
    specs/graders.mjs       the predicates, which measure nothing
    specs/*.spec.mjs        the cases, and mutations.spec.mjs which defeats every grader
```

The modules are plain scripts sharing one top-level scope, concatenated into a single
inline `<script>` - not ES modules. That is deliberate: the served document must resolve
nothing at load time, and `import`/`export` would make it fetch.

## The build

`internal/webui/index.html` is generated **and committed**. Nothing regenerates it behind
a build, and no build step depends on it being regenerated:

| command | what it does |
|---|---|
| `make webui-gen` | rewrite `internal/webui/index.html` from `internal/webui/src`. The only writer of that file |
| `make webui-stale` | fail if the committed document is not what the sources generate. Part of `make check` |
| `make webui-check` | the dashboard's two suites in REQUIRED mode (see below) |

The generator is **Go and the standard library only**. There is no JavaScript runtime, no
bundler, no registry package, no lockfile and no network in the build path, so `make
build` is unchanged and the container image gains no stage and no tool. `make check`
proves the committed document is current rather than regenerating it, which keeps the
build a `go build`.

Failure is total, never partial. Every source is read and checked before a byte is
written - a missing module, an unreadable one, an unterminated string or comment,
unbalanced brackets, a manifest entry that is not a module, a shell missing its marker -
and the message names the offending file. The write itself goes through a temp file in
the destination directory and is renamed into place, so a generator that fails leaves the
committed document exactly as it found it.

## The three suites, and the runtimes each needs

All three are reachable from `go test ./internal/webui/...`.

**The derivation units** run in **node's built-in test runner** (`node --test`), which
needs node and nothing else - the runner, the assertions and the module loader are all
part of the runtime, so the suite introduces no registry package and no lockfile. They
exercise `internal/webui/src/js/20-derive.js` one input at a time, including every way a
value can be absent. Those functions touch no DOM, which is what makes that possible;
`internal/webui/src/test/load.js` evaluates the modules in `node:vm` and hands the names
back.

**The rendered graders** load the SERVED document in a **real browser engine** and read
what it rendered: computed style after the whole cascade, real layout geometry, the text
`innerText` says a reader can see, a hit test at each subject's own centre, and the
browser's own report of every policy refusal. They need chromium (or chrome) on PATH.
Anything about what the page SHOWS is decided there and never by matching HTML or CSS
source text, because a text grader cannot decide what a rule applies to, what wins the
cascade, or what is shown rather than merely built.

**The Playwright graders** (`internal/webui/e2e`) hold every criterion that needs the
browser OPERATED rather than the page read - the three things no expression evaluated
inside the page can do:

- emulate the operating system's colour-scheme preference, so the theme under test is set
  at the ENGINE and never by a class, an attribute or a stylesheet injected into the page.
  A grader that injects the theme is grading its own fixture;
- read the **accessibility tree** the engine computed. An accessible name is the engine's
  own answer over labels, ARIA, native semantics and content - not a property of markup
  that anything can reconstruct by inspection;
- dispatch **real key presses**. A `KeyboardEvent` constructed inside the page is
  untrusted and moves focus nowhere, so tab order is not observable from the document.

They grade the **served document**: `e2e/fixtureserver` mounts the real
`webui.HandlerFor`, so what a spec loads is the same bytes and the same
Content-Security-Policy `holdfast serve` puts on the wire. There is one reader of that
document in this repository, deliberately.

The layering is the point, and it is why every grader can be proved: `probe.mjs` MEASURES
and decides nothing, `graders.mjs` DECIDES and measures nothing, the spec files drive the
world. Because a grader is a pure function of a snapshot, `mutations.spec.mjs` can run
every one of them against a document deliberately built to defeat it and fail the run if
the grader stays silent. The sharpest of those is the policy grader, which asserts a list
is EMPTY: a dead instrument and a clean page report the same nothing, and only a page the
engine must refuse can tell the two apart.

The measuring script moved onto the runner VERBATIM - rewriting the thing that does the
measuring would have put every grader's verdict in doubt at the same moment, with nothing
left to check it against - and no grader was deleted until its replacement was proved to
bite.

**Where the dependency reaches**, which is the load-bearing half: the BUILD PATH and the
SHIPPED PAGE take none of it. `internal/webui/gen` is still Go and the standard library
alone, `make build` is still a plain `go build`, the image gains no stage and no tool, and
the served document still resolves nothing at load time.
`TestBuild_TheTestOnlyDependencyCannotReachTheBuiltArtifact` proves that of the
generator's imports, the generated document, the Dockerfile and `go.mod` rather than
asking for it on trust. Lifecycle scripts are disabled in a committed `.npmrc`, and the
runner uses the browser the machine already has - `HOLDFAST_BROWSER` or one on PATH -
rather than downloading its own.

**One engine driver, and only one.** The PROSE graders in the second suite ask the same
three engine-only questions - the preference, the accessibility tree with each name's
sources, and an evaluation the served policy would refuse inside the page - and their
decisions are several hundred lines of Go that read committed records, so they stay in Go
and reach the engine through `internal/webui/e2e/driver.mjs`: one browser, one JSON command
per line. Two drivers would be two answers to "what did the engine say" every time they
disagreed, which is why the hand-written one this repository used to carry is gone rather
than kept beside the runner that answers the same questions.

**Skip or fail.** `make check` is this repository's gate and stays green on a machine with
no browser and no node: the suites skip, naming the runtime they wanted, exactly as the
docker gate does. That idiom's one failure mode is a suite that skips everywhere and
reports "ok" forever, so `make webui-check` sets `HOLDFAST_WEBUI_REQUIRED=1`, which turns
a missing runtime into a failure, and `scripts/webui-check.sh` additionally fails if
anything skipped or if any of the three halves did not execute. The Playwright half's Go
wrapper reads the runner's own JSON report and refuses a run that skipped a case or
executed too few, so "it ran" and "it decided something" are separate claims and both are
checked. CI runs `make webui-check` on every pull request, after proving both runtimes are
present and installing the graders' runner with `npm ci` from the committed lockfile.

**Running them by hand.** From `internal/webui/e2e`: `npm ci` once, then `npx playwright
test`. The fixture server is started for you. `--project=engine` is the convention set
(it drives its own theme and viewport); `--project=dark-wide|light-wide|dark-narrow` are
the specs that read the page as the project presents it.

## What the graders will not let you change quietly

- The served Content Security Policy. It is asserted byte for byte AND by a rule about
  policies: no `unsafe-eval`, no host, scheme, nonce or hash source, no `img-src`, no
  default Trusted Types policy, no directive outside the served set. The rule is proved
  against a table of policies that each widen it by exactly one thing.
- The render idiom. No module may assign a string to an HTML sink (`innerHTML`,
  `outerHTML`, `insertAdjacentHTML`, `document.write` or an equivalent). The sweep covers
  every source module and the generated document, and is proved against a module that
  does.
- Self-containment. The rendered page must issue no request to any origin but the server
  that served it, and must carry no element or style naming an off-origin URL. A
  hyperlink is not in that set: the AGPL section 13 source offer in the footer is a
  navigation target the reader chooses, not a resource the page loads.
- Absence. A fact nobody recorded renders as "not recorded" or "unavailable", never as 0,
  NaN or "undefined" - the store's own invariant, carried to the screen.
- The cap notices. Each capped table states the total the SERVER reported for it
  (`queue_total` / `history_total`), never one the page derived: the summary roll-up that
  used to produce those figures is gone from `js/20-derive.js` outright, along with the
  per-table status lists that fed it, so there is nothing left to derive one from. A
  response whose total could not be read is shown as unavailable **with no figure in its
  place** - the notice carries no digit at all, because a number beside the word "capped"
  is read as the total whatever the sentence around it says. Nor does it claim the view IS
  capped: cappedness is the comparison `total > shown`, so an unreadable total takes that
  answer with it, and an absent total field answers the same way as an unreadable one.
  Both are graded in the browser against a document mutated to derive its own total, to
  hide the notice, to print a number where the unavailability belongs, and to say nothing
  at all.

## The page's shape, and its figures (DASH-9)

The page is ordered by the two questions an operator has, in the order they ask them.
**Right now** comes first in the document and first on the screen - the live badges, the
counts, the controls, the filter and the queue - and **What it has done to your library**
comes after it: the whole-ledger figures and the recent history. Each region is under its
own heading, and the graders decide that order from document position AND from the
rendered top edge of each region, not from the markup.

Each whole-ledger figure is **drawn as well as stated**. A distribution (Outcomes, Skips
by guard) draws one bar per bucket, sized against the largest count in that same figure; a
spread (Replacement size, Encode time, VMAF pooled mean, VMAF worst frame) puts its
minimum, mean and maximum on one scale. Four rules govern every one of them, and each is
graded in the browser against a document deliberately mutated to defeat it:

- **Built, never fetched.** A drawing is a shell cloned from a `<template>` in the page's
  own markup, with one geometry attribute set per mark. The SVG namespace comes from the
  HTML parser reading that template, so no module names a namespace URI either. No
  library, no font, no image, no `data:` URI, no `package.json`, no lockfile: the response
  policy is `default-src 'none'` and `img-src`, `font-src` and `media-src` all fall back
  to it, so a dependency here would not be a heavier page, it would be a broken one.
- **The drawing is never the sole carrier of a number.** Every value a figure encodes is
  rendered as text in the same card, in the order the marks are drawn: label and count per
  bar, and the three named values of a spread. Remove every drawing from the rendered
  document and the cards still read - which is exactly what one grader does.
- **Nothing means anything by colour alone.** Every mark of every figure is one token
  (`--mark`), and the distinctions are position, length, tick height and the label beside
  the mark. Elsewhere on the page a status dot, a count chip, a badge, the connection state
  and an unavailable figure each pair their colour with rendered text, and the three
  terminal outcomes are given three different dot shapes as well. The grader forces every
  colour on the page to one value and requires the same distinctions to still be readable.
- **3:1 against what is behind it.** Every mark, scale, tick, status dot and figure
  boundary is measured from the browser's computed styles by WCAG 2.2's own relative
  luminance ratio, and the Go side recomputes each ratio rather than trusting the page's.
  `--border` (4.15:1 on the page, 3.82:1 on a card face) draws every boundary a reader has
  to find; `--line` remains for decorative separators, where the floor does not apply.

The value-to-geometry arithmetic behind the drawings (`readBuckets`, `bucketProportions`,
`spreadPositions`) lives in `js/20-derive.js` with the rest of the derivations, so it is
exercised input by input in node. Nothing in the browser computes a STATISTIC: the server
already did that over the whole ledger, and these turn a published number into a length or
a position and nothing else. A figure with nothing to draw draws nothing - an unavailable
one, one no row contributed to, and a spread whose ends coincide or never arrived all keep
their card and their words while drawing no mark that could be read as a measured zero.
