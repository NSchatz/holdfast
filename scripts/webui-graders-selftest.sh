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
# Deliberately NOT part of `make check`: the mutations belong in their own target, and
# `check` must never rewrite - or in this case re-render - the tree it is grading. CI runs
# it beside the dashboard gate. A guard nobody tries to defeat is a guard nobody knows works.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)" || { echo "::error::webui-graders selftest: mktemp failed" >&2; exit 1; }
trap 'rm -rf "$work"' EXIT

declared=4
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
  for b in chromium chromium-browser google-chrome chrome; do
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

# Every mutation is asserted to have CHANGED the document. A sed that matched nothing would
# otherwise leave the pristine page behind and the case would be graded against it, which is
# this selftest's own version of the silent green it exists to catch.
changed() {  # changed <case-name>
  if cmp -s "$pristine" "$doc"; then
    echo "::error::webui-graders selftest: the mutation for '$1' changed nothing - that case did NOT run" >&2
    exit 1
  fi
}

# --- the harness --------------------------------------------------------------------------
# Each run gets a port of its own, so the runner starts the fixture server from THIS COPY
# and a run of the real suite happening beside it cannot be the one answering. A server
# built from another tree would serve the unmutated page while the case reported on the
# mutated one, which is a green over a defeated grader. CI=1 is set for its other effect:
# a `.only` left in a spec is refused rather than quietly narrowing what ran.
port=8940
out=""; status=0
run_graders() {  # run_graders <case-title-regex>
  port=$((port + 1))
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
