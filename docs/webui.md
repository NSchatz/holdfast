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
    dashboard.css           the whole stylesheet (it moves as a unit; the phase does not
                            split the CSS)
    js/modules.txt          the modules, in the order they are concatenated
    js/10-constants.js      the closed vocabularies the page reads off the wire
    js/20-derive.js         the VALUE DERIVATIONS. No DOM reference at all
    js/30-dom.js            createElement + textContent, and nothing else
    js/40-cells.js          one cell renderer per column
    js/50-rows.js           the rows, cloned from the document's own <template>s
    js/60-aggregates.js     the whole-ledger cards
    js/70-render.js         one snapshot in, the page it describes out
    js/80-wire.js           the controls, the ticker and the SSE stream. The only module
                            that RUNS anything at load time
    test/                   the derivation unit suite (node's built-in test runner)
  gen/                      the generator, Go and stdlib only
  gen/genindex/             its command
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

## The two suites, and the runtimes each needs

Both are reachable from `go test ./internal/webui/...`.

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

**Skip or fail.** `make check` is this repository's gate and stays green on a machine with
no browser and no node: both suites skip, naming the runtime they wanted, exactly as the
docker gate does. That idiom's one failure mode is a suite that skips everywhere and
reports "ok" forever, so `make webui-check` sets `HOLDFAST_WEBUI_REQUIRED=1`, which turns
a missing runtime into a failure, and `scripts/webui-check.sh` additionally fails if
anything skipped or if either half did not execute. CI runs `make webui-check` on every
pull request, after proving both runtimes are present.

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
