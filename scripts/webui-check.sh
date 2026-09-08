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
# It replaced a suite that drove the browser over --remote-debugging-pipe by hand. That
# suite is gone, and nothing here waits for it: every criterion it decided is decided in
# the project below, and every grader it carried is defeated on purpose there before it
# was allowed to leave. Its Go wrapper additionally refuses a run whose JSON report shows a
# skip or too few cases, so "it executed" and "it decided something" are separate claims
# and both are checked.
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

echo "webui-check: OK - every half executed in required mode, nothing skipped"
