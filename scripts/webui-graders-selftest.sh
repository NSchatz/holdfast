#!/usr/bin/env bash
# Prove the dashboard's Playwright graders still BITE on the three questions that exist
# ONLY at the engine. `make webui-graders-selftest`.
#
# Those three are the reason there is a browser in this gate at all, and none of them can
# be answered by any expression the page evaluates about itself:
#
#   1. the OPERATING SYSTEM'S COLOUR-SCHEME PREFERENCE the page is rendered under. A grader
#      that injects a class, an attribute or a stylesheet is grading its own fixture;
#   2. the ACCESSIBLE NAME the engine computes for a control - its own answer over labels,
#      ARIA, native semantics and content, not a property of markup anything reconstructs;
#   3. the FOCUS a real dispatched key press moves. A KeyboardEvent constructed inside the
#      page is untrusted and moves focus nowhere, so tab order is not observable there.
#
# Each is DEFEATED here on purpose, against a mutated COPY of the tree, and the graders are
# required to go red AND to say what they saw. A case that goes red for somebody else's
# reason - a runner that could not start, a filter that matched no test - is counted as a
# failure, not a pass, because those are exactly the shapes a silent green takes.
#
# Case 0 is why the other three mean anything: the UNMUTATED copy must PASS the same three
# cases. Without it, "the graders red" would be equally true of graders that red on
# everything, which decide nothing at all.
#
# Cases 4 and 5 defeat the thing UNDER all of them: the engine itself. Every grader in this
# repository that reads what a page SHOWS is worth exactly what the browser resolution is
# worth, so an unresolvable engine is driven here for real, in both modes, and the two are
# required to answer DIFFERENTLY and to name the engine either way - a failure under
# required mode, a skip that says so on a machine that simply has no browser. Whether the
# engine a resolution DID accept can render is scripts/find-browser-selftest.sh's subject,
# and it is defeated there.
#
# Case 6 onwards are interface-craft C4 and C7, one mutation per refusal those two graders
# carry. They are here rather than beside the shipped run for the reason every case above is:
# both graders are otherwise exercised only by a run expected GREEN, and "the page draws one
# shadow depth" is equally true of a grader that decided nothing at all. Three of them mutate
# the COMMITTED RECORD rather than the page, because half of what C7 refuses is a refusal
# about the record; one mutates the HARNESS, because "every view" is the set the fixture
# server says it serves and the only honest way to hand a grader an empty view set is a server
# that really answers none.
#
# Deliberately NOT part of `make check`: the mutations belong in their own target, and
# `check` must never rewrite - or in this case re-render - the tree it is grading. CI runs
# it beside the dashboard gate. A guard nobody tries to defeat is a guard nobody knows works.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)" || { echo "::error::webui-graders selftest: mktemp failed" >&2; exit 1; }
trap 'rm -rf "$work"' EXIT

declared=24
pass=0; failed=0

repo="$work/repo"
e2e="$repo/internal/webui/e2e"
doc="$repo/internal/webui/index.html"
pristine="$work/index.html.pristine"

# --- the runtimes, named before anything is measured --------------------------------------
node="$(command -v node 2>/dev/null || true)"
[ -n "$node" ] || {
  echo "::error::webui-graders selftest: no node on PATH; the graders' runner is a node program" >&2; exit 1; }

browser="${HOLDFAST_BROWSER:-}"
if [ -n "$browser" ]; then
  # An explicit pin is the ONLY candidate. Falling back to PATH here would measure an engine
  # nobody named, which is the failure the pin exists to prevent.
  "$browser" --version >/dev/null 2>&1 || {
    echo "::error::webui-graders selftest: HOLDFAST_BROWSER names '$browser', which does not answer --version" >&2
    exit 1; }
else
  # The order scripts/find-browser.sh uses: a real vendor binary before `chromium`, which
  # on several distributions is a snap shim that hangs instead of failing. CI pins
  # HOLDFAST_BROWSER, so this branch is for a machine that named no engine.
  for b in google-chrome google-chrome-stable chrome chromium chromium-browser; do
    p="$(command -v "$b" 2>/dev/null || true)"
    [ -n "$p" ] || continue
    if "$p" --version >/dev/null 2>&1; then browser="$p"; break; fi
  done
fi
[ -n "$browser" ] || {
  echo "::error::webui-graders selftest: no browser engine found; set HOLDFAST_BROWSER or put chromium on PATH" >&2
  exit 1; }

[ -d "$here/internal/webui/e2e/node_modules/@playwright/test" ] || {
  echo "::error::webui-graders selftest: @playwright/test is not installed; run 'npm ci' in internal/webui/e2e" >&2
  exit 1; }

# The fixture server is `go run`, compiled from the COPY, so the runner needs the toolchain
# by a path that still resolves outside this repository's directory tree. A version manager
# puts a SHIM on PATH and resolves the version from the working directory, so the shim finds
# nothing once the copy is somewhere else and the server never starts - a failure that
# arrives as "no case ran", not as a wrong verdict, but a failure all the same. GOROOT is
# the toolchain's own answer to where it lives.
goroot="$(go env GOROOT 2>/dev/null || true)"
[ -x "$goroot/bin/go" ] || {
  echo "::error::webui-graders selftest: no Go toolchain found ('go env GOROOT' said '$goroot'); the fixture server is compiled from this tree" >&2
  exit 1; }

# --- the copy this runs against ----------------------------------------------------------
# The WORKING TREE, so an uncommitted change - a fix or a break - is graded as it stands,
# which is where the mistake gets made. The runner's own output is left behind: it is not
# repository content, and copying a stale trace in would only slow the copy down.
mkdir -p "$repo"
tar -C "$here" \
  --exclude=./.git \
  --exclude=./internal/webui/e2e/test-results \
  --exclude=./internal/webui/e2e/playwright-report \
  -cf - . | tar -C "$repo" -xf -

[ -r "$doc" ] || { echo "::error::webui-graders selftest: the copy has no served document" >&2; exit 1; }
cmp -s "$here/internal/webui/index.html" "$doc" \
  || { echo "::error::webui-graders selftest: the copy's document is not the working tree's - it graded the wrong thing" >&2; exit 1; }
[ -d "$e2e/node_modules/@playwright/test" ] \
  || { echo "::error::webui-graders selftest: the copy has no installed runner" >&2; exit 1; }

cp -p "$doc" "$pristine"
reset() { cp -p "$pristine" "$doc"; }

# The other two files a defeat below mutates, each with a pristine copy of its own.
#
# The state-matrix RECORD is one of them, and it has to be: half of what clause C7 refuses is
# a refusal about the record rather than about the page - a component the record does not
# name, a state it gives no entry, a not-applicable it buys without a reason, a state it
# declares away that the engine can reach. None of those can be produced by mutating a
# stylesheet, and a refusal nobody defeats is a refusal nobody knows bites.
#
# The HARNESS is the other. "Every view" is the set the fixture server says it serves, so the
# only way to hand the grader an empty view set is to make the harness say it serves nothing -
# and the copy is compiled from this tree, so a sed here is a server that really answers that.
record="$repo/docs/state-matrix.md"
harness="$repo/internal/webui/e2e/fixtureserver/main.go"
pristine_record="$work/state-matrix.md.pristine"
pristine_harness="$work/fixtureserver.go.pristine"
[ -r "$record" ] || { echo "::error::webui-graders selftest: the copy has no docs/state-matrix.md; interface-craft C7's record is what half its refusals are about" >&2; exit 1; }
cp -p "$record" "$pristine_record"
cp -p "$harness" "$pristine_harness"
reset_record() { cp -p "$pristine_record" "$record"; }
reset_harness() { cp -p "$pristine_harness" "$harness"; }
reset_all() { reset; reset_record; reset_harness; }

# Every mutation is asserted to have CHANGED the file it was aimed at. A sed that matched
# nothing would otherwise leave the pristine copy behind and the case would be graded against
# it, which is this selftest's own version of the silent green it exists to catch.
changed_file() {  # changed_file <case-name> <file> <pristine>
  if cmp -s "$3" "$2"; then
    echo "::error::webui-graders selftest: the mutation for '$1' changed nothing in $2 - that case did NOT run" >&2
    exit 1
  fi
}
changed() { changed_file "$1" "$doc" "$pristine"; }

# --- the harness --------------------------------------------------------------------------
# Each run gets a port of its own, so the runner starts the fixture server from THIS COPY
# and a run of the real suite happening beside it cannot be the one answering. A server
# built from another tree would serve the unmutated page while the case reported on the
# mutated one, which is a green over a defeated grader. CI=1 is set for its other effect:
# a `.only` left in a spec is refused rather than quietly narrowing what ran.
#
# The port is one the OPERATING SYSTEM says nothing is listening on, asked for per case, and
# not a number counted up from a constant. A fixed base makes every run's verdict a fact
# about the machine: anything holding 8941 - a killed run's orphaned fixture server, a second
# worktree grading in parallel - fails this script at start-up with "port already in use",
# and the report it prints is "the shipped page passes a grader it should have defeated",
# which is a lie about the page. That is the same defect S0069 took out of
# playwright.config.mjs, answered here the same way and for the same reason; the rule it was
# protecting is untouched, because a port nothing is on cannot be adopted.
freeport() {
  "$node" -e 'const s=require("node:net").createServer();s.listen(0,"127.0.0.1",()=>{process.stdout.write(String(s.address().port));s.close();});'
}
port=""
out=""; status=0
run_graders() {  # run_graders <case-title-regex>
  port="$(freeport)"
  set +e
  out="$(cd "$e2e" && CI=1 NO_COLOR=1 PATH="$goroot/bin:$PATH" \
    HOLDFAST_BROWSER="$browser" HOLDFAST_E2E_PORT="$port" \
    "$node" node_modules/@playwright/test/cli.js test --workers=1 --reporter=list -g "$1" 2>&1)"
  status=$?
  set -e
}

# expect <want-exit> <name> <title-regex> <must-mention-regex> <must-run-regex>
expect() {
  local want="$1" name="$2" title="$3" want_msg="$4" ran="$5"
  run_graders "$title"
  if [ "$status" -ne "$want" ]; then
    printf '::error::webui-graders selftest: %s - the graders exited %s, wanted %s\n' "$name" "$status" "$want" >&2
    printf '%s\n' "$out" | sed 's/^/       | /' >&2
    failed=$((failed + 1)); return
  fi
  if ! grep -qE -- "$ran" <<<"$out"; then
    printf '::error::webui-graders selftest: %s - the run reported no case at all (a filter that matched nothing exits non-zero too)\n' "$name" >&2
    printf '%s\n' "$out" | sed 's/^/       | /' >&2
    failed=$((failed + 1)); return
  fi
  if [ -n "$want_msg" ] && ! grep -qE -- "$want_msg" <<<"$out"; then
    printf '::error::webui-graders selftest: %s - exited %s (correct) but for the WRONG REASON: nothing matched /%s/\n' "$name" "$status" "$want_msg" >&2
    printf '%s\n' "$out" | sed 's/^/       | /' >&2
    failed=$((failed + 1)); return
  fi
  printf '  ok: %s\n' "$name"; pass=$((pass + 1))
}

# expectn <want-exit> <name> <title-regex> <must-run-regex> <must-mention-regex>...
#
# The same case as `expect` with more than one thing the output has to say. Three criteria in
# S0126 require the run to DECLARE what it measured - the views and the regions, each
# component group with its member count, the density set and the cells executed - and a
# declaration is only asserted by naming every line of it. One regex could not.
expectn() {
  local want="$1" name="$2" title="$3" ran="$4"; shift 4
  run_graders "$title"
  if [ "$status" -ne "$want" ]; then
    printf '::error::webui-graders selftest: %s - the graders exited %s, wanted %s\n' "$name" "$status" "$want" >&2
    printf '%s\n' "$out" | sed 's/^/       | /' >&2
    failed=$((failed + 1)); return
  fi
  if ! grep -qE -- "$ran" <<<"$out"; then
    printf '::error::webui-graders selftest: %s - the run reported no case at all (a filter that matched nothing exits non-zero too)\n' "$name" >&2
    printf '%s\n' "$out" | sed 's/^/       | /' >&2
    failed=$((failed + 1)); return
  fi
  local msg
  for msg in "$@"; do
    if ! grep -qE -- "$msg" <<<"$out"; then
      printf '::error::webui-graders selftest: %s - exited %s (correct) but nothing matched /%s/\n' "$name" "$status" "$msg" >&2
      printf '%s\n' "$out" | sed 's/^/       | /' >&2
      failed=$((failed + 1)); return
    fi
  done
  printf '  ok: %s\n' "$name"; pass=$((pass + 1))
}

THEME='the engine.s colour-scheme preference chooses the theme'
NAMES='the accessibility tree the engine computed names every control'
FOCUS='a real Tab reaches every control in reading order'

# --- 0. The unmutated page PASSES all three. Without this, every case below could be a
#        grader that reds on anything, which proves nothing about the page. ---------------
expect 0 "the shipped page passes all three engine-only graders" \
  "($THEME|$NAMES|$FOCUS)" "" '3 passed'

# --- 1. THE OPERATING SYSTEM'S COLOUR-SCHEME PREFERENCE. The dark palette is applied by a
#        media query and nothing else, so a condition that can never match leaves ONE
#        palette wearing both preferences - the page still renders, still passes every
#        source check, and answers the operating system's preference with a shrug. ---------
reset
sed -i 's|@media (prefers-color-scheme: dark) {|@media (prefers-color-scheme: dark) and (min-width: 99999px) {|' "$doc"
changed "one palette under both preferences"
expect 1 "the colour-scheme grader reds when the dark palette can never apply" \
  "$THEME" 'the same value|not two palettes|painted .* under both preferences' '1 failed'

# --- 2. THE ACCESSIBLE NAME THE ENGINE COMPUTES. Detach the label from the control it
#        names. Every character of the page is unchanged, the label is still on the screen,
#        and the engine now names the search box by its PLACEHOLDER - which disappears the
#        moment a value is typed into it. Only the engine's own tree says so. --------------
reset
sed -i 's|<label for="filter" class="lbl">|<label class="lbl">|' "$doc"
changed "a control named only by its placeholder"
expect 1 "the accessible-name grader reds when a control loses its label" \
  "$NAMES" 'named ONLY by its placeholder|exposes no accessible name' '1 failed'

# --- 3. THE FOCUS A REAL KEY PRESS MOVES. Take the filter out of the tab order. It is still
#        an input, still rendered, still enabled, still reachable with a mouse - and a
#        keyboard user can no longer get to it. Nothing in the document says so; only
#        pressing Tab for real does. ---------------------------------------------------------
reset
sed -i 's|<input id="filter" type="search"|<input id="filter" tabindex="-1" type="search"|' "$doc"
changed "a control taken out of the tab order"
expect 1 "the tab-order grader reds when a control cannot be reached by keyboard" \
  "$FOCUS" 'tabbing reached|reading order puts' '1 failed'

reset

# --- 4 and 5. AN ENGINE THAT CANNOT BE RESOLVED. These two grade AC-9 of
#        S0125-holdfast-hierarchy-density-graders: "IF no browser engine can be resolved ...
#        THEN THE SYSTEM SHALL fail the required-mode run with a message naming the engine
#        it tried and what it saw, and SHALL NOT report either clause as passed or as
#        skipped." Every grader that reads what the page SHOWS is worth what this resolution
#        is worth, so it is defeated with a pin that names a path no process can execute.
#        Under REQUIRED mode that has to be a failure naming the engine it tried, with
#        nothing reporting itself skipped; under the ordinary mode `make check` runs in, the
#        same tree and the same pin have to answer with a skip that names the engine, which
#        is what keeps the gate green on a machine with no browser. One mode passing where
#        the other fails is the whole claim.
NO_ENGINE="$work/not-an-engine"
: >"$NO_ENGINE"   # present, readable, and not executable: unresolvable, not absent
ENGINE_CASE='TestPlaywright_TheRenderedGradersRunInARealEngine'

run_without_an_engine() {  # run_without_an_engine <required-mode value>
  set +e
  out="$(cd "$repo" && NO_COLOR=1 PATH="$goroot/bin:$PATH" \
    HOLDFAST_BROWSER="$NO_ENGINE" HOLDFAST_WEBUI_REQUIRED="$1" \
    "$goroot/bin/go" test -v -count=1 -run "$ENGINE_CASE" ./internal/webui/ 2>&1)"
  status=$?
  set -e
}

engine_case() {  # engine_case <name> <want-exit> <must-mention-regex> <must-not-match-regex>
  local name="$1" want="$2" want_msg="$3" forbid="$4"
  if [ "$status" -ne "$want" ]; then
    printf '::error::webui-graders selftest: %s - go test exited %s, wanted %s\n' "$name" "$status" "$want" >&2
    printf '%s\n' "$out" | sed 's/^/       | /' >&2
    failed=$((failed + 1)); return
  fi
  if ! grep -qE -- "$want_msg" <<<"$out"; then
    printf '::error::webui-graders selftest: %s - exited %s (correct) but for the WRONG REASON: nothing matched /%s/\n' "$name" "$status" "$want_msg" >&2
    printf '%s\n' "$out" | sed 's/^/       | /' >&2
    failed=$((failed + 1)); return
  fi
  if [ -n "$forbid" ] && grep -qE -- "$forbid" <<<"$out"; then
    printf '::error::webui-graders selftest: %s - the output matched /%s/, which it must not\n' "$name" "$forbid" >&2
    printf '%s\n' "$out" | sed 's/^/       | /' >&2
    failed=$((failed + 1)); return
  fi
  printf '  ok: %s\n' "$name"; pass=$((pass + 1))
}

run_without_an_engine 1
engine_case "AC-9: an unresolvable engine FAILS the required-mode run and names what it tried" \
  1 "not-an-engine" 'SKIP'

run_without_an_engine ""
engine_case "AC-9: the same unresolvable engine SKIPS outside required mode and names what it tried" \
  0 "not-an-engine" ''

reset_all

# --- 6 onwards. INTERFACE-CRAFT C4 AND C7 (S0126) ------------------------------------------
#
# Every refusal those two graders carry is defeated here, ONE MUTATION PER REFUSAL, against
# the same copy of the tree. Both graders are otherwise routed at a run expected GREEN, which
# exercises their happy branch and nothing else: "the page draws one shadow depth" and "the
# page has no colour-only flag" are equally true of a grader that decided neither, and a
# grader that cannot fail is not evidence. Each case below therefore makes the property FALSE
# on purpose and requires the grader to go red AND to say what it saw, because a grader that
# reds for somebody else's reason - a runner that could not start, a page that never rendered
# - is as useless as one that passes.
#
# Two of them expect a GREEN run, and they are not the odd ones out. AC-2 is a claim about
# WHEN the reading is taken - a hover elevation and a focus ring are not a second depth - and
# the only way to assert it is to put both on the page and require the count not to move. The
# two declaration cases are the other kind: AC-5, AC-7 and AC-15 each oblige the run to REPORT
# what it measured, and a zero exit carries no declaration at all, so the lines themselves are
# what is asserted.
C4='ornament has a ceiling in every view the harness serves'
C7='every interactive component proves its whole state matrix'

# --- C4, first clause: a second shadow depth AT REST. -------------------------------------
reset_all
sed -i 's|</style>|.agg { box-shadow: 0 6px 18px rgba(0, 0, 0, 0.35); }\n</style>|' "$doc"
changed "a second shadow depth"
expect 1 "AC-1: the depth grader reds when a view draws two shadow depths" \
  "$C4" 'distinct shadow depths at rest' '1 failed'

# --- C4, first clause under AC-2: the same two treatments, reachable only by HOVERING or by
#        FOCUSING. Neither is a second depth, because no reader meets either beside the first,
#        and a grader that counted them would red on a correct page. The run must stay green
#        AND must still have measured something, which the declaration lines say. -----------
reset_all
sed -i 's|</style>|.agg:hover { box-shadow: 0 6px 18px rgba(0, 0, 0, 0.35); }\n:focus-visible { box-shadow: 0 0 0 6px rgba(0, 0, 0, 0.35); }\n</style>|' "$doc"
changed "a hover elevation and a focus shadow"
expectn 0 "AC-2: a hover elevation and a focus shadow are NOT counted as a second depth" \
  "$C4" '1 passed' \
  'c4: measured [0-9]+ view\(s\)' \
  'shadow depth\(s\)'

# --- C4, second clause: two neighbouring regions told apart TWICE - a border on the facing
#        edges and a different surface either side of it. ----------------------------------
reset_all
sed -i 's|</style>|.controls + .controls { border-top: var(--bw-hair) solid var(--border); background: var(--bg); }\n</style>|' "$doc"
changed "a double separation between two neighbours"
expect 1 "AC-3: the separation grader reds when two neighbours are told apart twice" \
  "$C4" 'separated TWICE' '1 failed'

# --- C4, third clause: a coloured strip down an element's left edge, on an element that says
#        nothing else - no text, no mark, no accessible name. The status dot is exactly that
#        element, which is why the page's own dots carry a SHAPE as well as a colour. -------
reset_all
sed -i 's|</style>|.dot { border-left: var(--bw-flag) solid var(--bad); }\n</style>|' "$doc"
changed "a colour-only left strip"
expect 1 "AC-4: the left-strip grader reds when a colour is an element's only signal" \
  "$C4" 'paints a left-side border' '1 failed'

# --- C4, AC-6, first half: a HARNESS that serves no view. The grader asks the fixture server
#        which scenarios it has; a server that answers none leaves it with nothing to measure,
#        and a grader with nothing to measure must fail rather than pass by default. --------
reset_all
sed -i 's|names = append(names, name)|_ = name|' "$harness"
changed_file "a harness that serves no view" "$harness" "$pristine_harness"
expect 1 "AC-6: the depth grader reds when the harness serves no view at all" \
  "$C4" 'served NO view at all' '1 failed'

# --- C4, AC-6, second half: a page with no REGION. Every border and every shadow taken away
#        and the two regions' names detached, so nothing on the page is a region - the page
#        still renders, still passes every source check, and there is nothing for any of the
#        three clauses to be decided over. -----------------------------------------------
reset_all
sed -i 's|</style>|* { border: 0 !important; box-shadow: none !important; }\n</style>|' "$doc"
sed -i 's| aria-labelledby="h-now"||; s| aria-labelledby="h-history"||' "$doc"
changed "a page with no region at all"
expect 1 "AC-6: the depth grader reds when a view renders no region at all" \
  "$C4" 'rendered no region at all' '1 failed'

# --- C4, AC-5: the run DECLARES what it measured. Three criteria require a declaration and
#        all three are routed at a run that exits zero either way, so the lines are asserted
#        here or by nothing. Unmutated on purpose: what is being proved is that a passing run
#        still says which views, which regions and which depths it read. --------------------
reset_all
expectn 0 "AC-5: the depth run declares the views and the regions it measured" \
  "$C4" '1 passed' \
  'c4: measured [0-9]+ view\(s\) - [0-9]+ fixture scenario\(s\)' \
  'c4: the "full" fixture .*: [0-9]+ region\(s\), [0-9]+ neighbouring pair\(s\)' \
  'left-strip subject\(s\), over [0-9]+ element\(s\)'

# --- C7, AC-8, first half: a control the engine puts in the TAB ORDER that the accessibility
#        tree does not call interactive. It is reachable by keyboard and belongs to no
#        component, which is precisely how a control escapes a state matrix. ----------------
reset_all
sed -i 's|<span id="conn">|<span id="conn" tabindex="0">|' "$doc"
changed "a focusable element outside the inventory"
expect 1 "AC-8: the inventory grader reds when a focusable element falls into no group" \
  "$C7" 'falls into no reported group|does not report it with an interactive role' '1 failed'

# --- C7, AC-8, second half: two elements the engine reports with DIFFERENT roles landing in
#        one group. The grouping key deliberately carries no role, so this can fire; a key
#        that carried one would make the refusal impossible and the check meaningless. ------
reset_all
sed -i 's|<button id="pause">|<button id="pause" role="link">|' "$doc"
changed "two roles in one component group"
expect 1 "AC-8: the grouping grader reds when one group holds two roles" \
  "$C7" 'different roles' '1 failed'

# --- C7, AC-9: an EMPTY inventory. Every control taken out of the tab order, repeatedly,
#        because the page rebuilds its rows on every render. Nothing is derived, so nothing
#        can be proved, and a matrix over nothing must never exit zero. --------------------
reset_all
sed -i 's|</body>|<script>setInterval(function(){for (const el of document.querySelectorAll("a,button,input,select,textarea")) el.setAttribute("tabindex","-1");},20);</script></body>|' "$doc"
changed "an empty component inventory"
expect 1 "AC-9: the matrix grader reds when the inventory is empty" \
  "$C7" 'derived NO interactive component at all' '1 failed'

# --- C7, AC-10: a component the engine DERIVES and the record does not name. This is the
#        refusal that stops a control entering the surface without entering the matrix. -----
reset_all
sed -i '/^| a\.doclink |/d' "$record"
changed_file "a component missing from the record" "$record" "$pristine_record"
expect 1 "AC-10: the record grader reds when a derived component is named nowhere in it" \
  "$C7" 'is named nowhere in the record' '1 failed'

# --- C7, AC-11, first half: a state with no entry at all. ----------------------------------
reset_all
sed -i '/^| button | hover |/d' "$record"
changed_file "a state with no entry" "$record" "$pristine_record"
expect 1 "AC-11: the record grader reds when a component is given no entry for a state" \
  "$C7" 'no entry for hover' '1 failed'

# --- C7, AC-11, second half: a not-applicable bought without a reason. A state declared away
#        with no sentence saying why is a state nobody decided. ----------------------------
reset_all
sed -i 's#^| button | loading | not applicable |.*#| button | loading | not applicable |  |#' "$record"
changed_file "a not-applicable with no reason" "$record" "$pristine_record"
expect 1 "AC-11: the record grader reds when a not-applicable carries no reason" \
  "$C7" 'gives no reason' '1 failed'

# --- C7, AC-12: one of the four states that may NEVER be declared away, declared away. ------
reset_all
sed -i 's#^| button | hover | proved |.*#| button | hover | not applicable | Nobody uses a pointer on this page. |#' "$record"
changed_file "a reachable state declared away" "$record" "$pristine_record"
expect 1 "AC-12: the record grader reds when a reachable state is declared not applicable" \
  "$C7" 'only disabled, loading and error may be declared away' '1 failed'

# --- C7, AC-13: a state DECLARED proved that renders exactly like the default. The rule that
#        draws the plain button's hover edge is aimed at a class nothing wears, so the record
#        still claims the state and the engine paints nothing. ----------------------------
reset_all
sed -i 's|button:hover:not(:disabled),|button.no-such-class:hover,|' "$doc"
changed "a proved state that renders identically to the default"
expect 1 "AC-13: the matrix grader reds when a proved state renders identically to the default" \
  "$C7" 'renders VISUALLY IDENTICAL to its default' '1 failed'

# --- C7, AC-14: THE ONE THAT DECIDES WHETHER THIS GRADER IS WORTH ANYTHING. The real
#        focus-visible treatment is removed and a focus style is left in its place that only
#        an INJECTED class could ever apply. A grader that entered the state by adding a
#        class, an attribute or an inline style would find the ring and pass; one that
#        presses Tab for real finds nothing, because a constructed KeyboardEvent is untrusted
#        and moves focus nowhere. The cell has to go red. --------------------------------
reset_all
sed -i 's|:focus-visible {|:focus-visible { outline: none !important; }\n.hf-injected-focus {|' "$doc"
changed "a focus treatment only an injection applies"
expect 1 "AC-14: the matrix grader reds when focus-visible is only reachable by injection" \
  "$C7" 'focus-visible .* renders VISUALLY IDENTICAL to its default' '1 failed'

# --- C7, AC-16: a second density that exists only as a NAME. The record declares it and says
#        how to enter it; the page renders exactly the same either way, so the two are one
#        density under two names and the run must refuse to count it. ---------------------
reset_all
sed -i 's#^| compact | the document as served |#| compact | the document as served |\n| comfortable | data-density="comfortable" on the document element |#' "$record"
changed_file "a density that exists only as a name" "$record" "$pristine_record"
expect 1 "AC-16: the density grader reds when two densities render identically" \
  "$C7" 'one density under two names' '1 failed'

# --- C7, AC-7 and AC-15: the run DECLARES its inventory, its density set and the cells it
#        executed. Unmutated, for the same reason the C4 declaration case is. ---------------
reset_all
expectn 0 "AC-7, AC-15: the matrix run declares its components, its densities and its cells" \
  "$C7" '1 passed' \
  'c7: derived [0-9]+ component group\(s\)' \
  'c7: component "button" \(button\): [0-9]+ instance\(s\), graded ' \
  'c7: densities declared built: [a-z]+.*; ran [0-9]+ cell\(s\)' \
  'c7: styling S4 asks for 2 densities'

reset_all

echo
# Report against the number of cases DECLARED, not the number that ran: "$pass/$pass" is
# N/N by construction and could never show a shortfall.
total=$((pass + failed))
if [ "$total" -ne "$declared" ]; then
  echo "::error::webui-graders selftest: ran $total case(s), expected $declared - a case did not execute" >&2
  exit 1
fi
if [ "$failed" -ne 0 ]; then
  echo "::error::webui-graders selftest: $failed of $declared case(s) did not bite - the dashboard's engine-only graders are not trustworthy" >&2
  exit 1
fi
echo "webui-graders selftest: $pass/$declared cases bite"
