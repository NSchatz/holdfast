#!/usr/bin/env bash
# The dashboard's suites in REQUIRED mode (WEBUI-10).
#
# `make check` is this repo's gate and must stay green on a machine with no browser, so
# internal/webui's rendered graders and its node-run derivation units SKIP there, exactly
# as the docker gate does. That idiom has one failure mode: a suite that skips everywhere
# reports "ok" forever while measuring nothing.
#
# This target is the answer. HOLDFAST_WEBUI_REQUIRED=1 turns a missing runtime from a skip
# into a failure, and this script then refuses a run in which anything skipped anyway, or
# in which either half of the suite did not execute at all. It is what CI runs on every
# pull request, and what a human runs to see the dashboard actually measured.
set -euo pipefail

pkg="./internal/webui/..."
log="$(mktemp -t webui-check.XXXXXX)"
trap 'rm -f "$log"' EXIT

echo "webui-check: running $pkg with HOLDFAST_WEBUI_REQUIRED=1 (a missing runtime is a failure, not a skip)"

status=0
HOLDFAST_WEBUI_REQUIRED=1 go test -v -count=1 -timeout 20m "$pkg" >"$log" 2>&1 || status=$?
cat "$log"

if [ "$status" -ne 0 ]; then
  echo "webui-check: FAILED (go test exited $status)" >&2
  exit "$status"
fi

# Nothing may skip in required mode. A skip here is a runtime that was silently absent.
if grep -q -- '--- SKIP:' "$log"; then
  echo "webui-check: FAILED - a test reported itself SKIPPED under required mode:" >&2
  grep -- '--- SKIP:' "$log" >&2
  exit 1
fi

# Every half must have EXECUTED. A gate that passes because a suite was filtered out,
# renamed away or compiled out is not a gate.
#
# The third half is the Playwright project under internal/webui/e2e. It holds every
# grader that needs to OPERATE the engine rather than read the page - emulating the
# operating system's colour-scheme preference, reading the accessibility tree the engine
# computed, dispatching real key presses - and it decides them against the same served
# document, because its fixture server mounts the real webui.HandlerFor.
#
# There is exactly ONE engine driver in this repository and it is that runner's: the Go
# graders in the second half that need the browser OPERATED reach it through the same
# project (internal/webui/e2e/driver.mjs). Its Go wrapper additionally refuses a run whose
# JSON report shows a skip or too few cases, so "it executed" and "it decided something"
# are separate claims and both are checked.
missing=0
for half in \
  'TestUnit_:the derivation unit suite (node)' \
  'TestRendered_:the rendered graders (browser engine)' \
  'TestPlaywright_:the Playwright graders (against the real handler, in every theme and width)'
do
  prefix="${half%%:*}"
  what="${half#*:}"
  if ! grep -qE -- "^(=== RUN|--- PASS:|    --- PASS:) +${prefix}" "$log"; then
    echo "webui-check: FAILED - $what did not execute (no test matching ${prefix}* ran)" >&2
    missing=1
  fi
done
[ "$missing" -eq 0 ] || exit 1

# And the two graders that are a CASE inside the third half rather than a half of their own.
#
# "The suite executed" and "this grader inside it executed" are different claims, and the
# checks above only make the first: a case renamed, filtered out or quietly made conditional
# leaves the Playwright half running, the case count above its floor, and the clause nobody
# decided. Both of these announce themselves by REPORTING what they measured - which is
# something three of their criteria require of them anyway - so the declaration is what is
# looked for here. A grader that ran and found nothing to say would fail its own anti-vacuity
# refusal long before it reached this line.
for grader in \
  'c4: measured:interface-craft C4 (depth discipline)' \
  'c7: derived:interface-craft C7 (the state matrix)'
do
  mark="${grader%%:*}"
  what="${grader#*:}"
  if ! grep -qF -- "$mark" "$log"; then
    echo "webui-check: FAILED - $what did not execute (nothing in the run declared \"$mark\")" >&2
    missing=1
  fi
done
[ "$missing" -eq 0 ] || exit 1

echo "webui-check: OK - every half executed in required mode, nothing skipped"
