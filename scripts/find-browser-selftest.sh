#!/usr/bin/env bash
# Prove scripts/find-browser.sh still BITES. Part of `make check`, and hermetic: every
# "browser" here is a shell script, so this runs on a machine with no browser at all.
#
# The failure it exists for is the one that happened. ci.yml's `dashboard` job asked
# `command -v` and printed `--version`, which is a question every candidate answers,
# including the one that then never paints - so the job resolved nothing, the graders were
# left to search for themselves, and two of them spent 90 seconds each waiting on a browser
# that was never going to answer. A resolver that accepts a browser it has not seen render
# is not a resolver; these cases are the ways it could go back to being one.
#
# A guard nobody tries to defeat is a guard nobody knows works.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
finder="$here/scripts/find-browser.sh"
[ -x "$finder" ] || { echo "::error::find-browser selftest: $finder is not executable" >&2; exit 1; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

declared=7
pass=0
failed=0

# --- the fake engines ---------------------------------------------------------------
#
# Each answers --version, because answering --version is precisely what does not settle
# the question. They differ only in what happens when asked to render.

# renderer prints a document and exits 0: a browser that works.
renderer() {
  cat > "$1" <<'SH'
#!/bin/sh
case "$*" in *--version*) echo "Fake Engine 1.0"; exit 0;; esac
echo "<html><body></body></html>"
exit 0
SH
  chmod +x "$1"
}

# refuser answers --version and then fails to render: the browser that is present, names
# itself, and cannot do the job.
refuser() {
  cat > "$1" <<'SH'
#!/bin/sh
case "$*" in *--version*) echo "Fake Engine 1.0"; exit 0;; esac
echo "cannot render here" >&2
exit 1
SH
  chmod +x "$1"
}

# hanger answers --version and then sits for ever: the snap shim with unhappy confinement,
# which is the one that cost the CI runs.
hanger() {
  cat > "$1" <<'SH'
#!/bin/sh
case "$*" in *--version*) echo "Fake Engine 1.0"; exit 0;; esac
sleep 300
SH
  chmod +x "$1"
}

# stage builds a PATH directory holding a named kind for every candidate name, so no real
# browser on this machine can be reached: the loop takes the first name it finds, and every
# name it looks for is here.
#
# Usage: stage <dir> [name=kind ...]; any candidate not named is a refuser.
stage() {
  local dir="$1"; shift
  mkdir -p "$dir"
  local n
  for n in google-chrome google-chrome-stable chrome chromium chromium-browser; do
    refuser "$dir/$n"
  done
  local spec
  for spec in "$@"; do
    "${spec#*=}" "$dir/${spec%%=*}"
  done
}

# run invokes the finder with PATH pointing at the staged directory FIRST. The rest of PATH
# stays so the script can still find mktemp, timeout and rm.
#
# HOLDFAST_BROWSER is UNSET before every case, and that is not tidiness. This selftest runs
# inside `make check`, and in CI's `build` job `make check` runs with HOLDFAST_BROWSER
# already exported to the engine the workflow proved - so without this the pin would satisfy
# every case, four of them would report the runner's own Chrome as their answer, and the
# PATH search these cases are about would never execute. The pin cases set it back
# themselves, through "$@".
run() {
  local dir="$1"; shift
  env -u HOLDFAST_BROWSER PATH="$dir:$PATH" HOLDFAST_BROWSER_PROBE_SECONDS=3 "$@" "$finder"
}

ok()   { pass=$((pass + 1)); echo "  ok: $1"; }
bad()  { failed=$((failed + 1)); echo "::error::find-browser selftest: $1" >&2; }

# --- case 1: a candidate that only answers --version is refused ----------------------
d="$work/c1"; stage "$d" "google-chrome=refuser" "chromium=renderer"
if out="$(run "$d" 2>/dev/null)" && [ "$out" = "$d/chromium" ]; then
  ok "a candidate that answers --version but cannot render is passed over"
else
  bad "case 1: took '${out:-<nothing>}', wanted $d/chromium - a --version answer was treated as a browser"
fi

# --- case 2: a candidate that HANGS is refused ---------------------------------------
d="$work/c2"; stage "$d" "google-chrome=hanger" "chromium=renderer"
if out="$(run "$d" 2>/dev/null)" && [ "$out" = "$d/chromium" ]; then
  ok "a candidate that never answers is passed over rather than waited on for ever"
else
  bad "case 2: took '${out:-<nothing>}', wanted $d/chromium - the snap-shim hang was not survived"
fi

# --- case 3: nothing renders is a LOUD failure, never a quiet empty answer ------------
d="$work/c3"; stage "$d"
set +e
out="$(run "$d" 2>"$work/c3.err")"; status=$?
set -e
if [ "$status" -ne 0 ] && [ -z "$out" ] && grep -q "no browser on this runner could render" "$work/c3.err"; then
  ok "a machine where nothing renders fails by name instead of resolving nothing"
else
  bad "case 3: exited $status printing '${out}' - a resolver that answers nothing quietly is a skipped grader"
fi

# --- case 4: an unusable PIN never falls back to a working browser on PATH ------------
d="$work/c4"; stage "$d" "google-chrome=renderer" "chromium=renderer"
refuser "$work/pinned-refuser"
set +e
out="$(run "$d" HOLDFAST_BROWSER="$work/pinned-refuser" 2>"$work/c4.err")"; status=$?
set -e
if [ "$status" -ne 0 ] && [ -z "$out" ] && grep -q "pinned-refuser" "$work/c4.err"; then
  ok "an engine pin that cannot render is unresolvable, not replaced by one on PATH"
else
  bad "case 4: exited $status printing '${out}' - the pin was silently replaced, which is the false green this layer exists for"
fi

# --- case 5: a usable PIN is taken exactly as given ----------------------------------
d="$work/c5"; stage "$d" "google-chrome=renderer"
renderer "$work/pinned-renderer"
if out="$(run "$d" HOLDFAST_BROWSER="$work/pinned-renderer" 2>/dev/null)" && [ "$out" = "$work/pinned-renderer" ]; then
  ok "a pinned engine that renders is the one that is measured"
else
  bad "case 5: took '${out:-<nothing>}', wanted $work/pinned-renderer"
fi

# --- case 6: a real vendor binary is preferred to `chromium` --------------------------
# Both render, so nothing but the ORDER can decide this - and the order is the knowledge
# that /usr/bin/chromium is a snap shim on the distributions this runs on.
d="$work/c6"; stage "$d" "google-chrome=renderer" "chromium=renderer"
if out="$(run "$d" 2>/dev/null)" && [ "$out" = "$d/google-chrome" ]; then
  ok "a vendor binary is preferred to chromium when both render"
else
  bad "case 6: took '${out:-<nothing>}', wanted $d/google-chrome"
fi

# --- case 7: this selftest is hermetic against the environment `make check` has ---------
# CI's `build` job exports HOLDFAST_BROWSER before running `make check`, so every case above
# runs with a real pin in the environment. Without `env -u` in run(), the pin would answer
# all of them and the PATH search they are about would never execute - which is not a
# hypothetical: it is what this file did on its first CI run, reporting the runner's own
# Chrome for four cases. This case IS that environment.
export HOLDFAST_BROWSER=/nonexistent/inherited-pin
d="$work/c7"; stage "$d" "chromium=renderer"
if out="$(run "$d" 2>/dev/null)" && [ "$out" = "$d/chromium" ]; then
  ok "a HOLDFAST_BROWSER inherited from the caller does not decide a PATH-search case"
else
  bad "case 7: took '${out:-<nothing>}', wanted $d/chromium - the caller's pin leaked in and every case above proved nothing"
fi
unset HOLDFAST_BROWSER

echo
# Report against the number of cases DECLARED, not the number that ran: "$pass/$pass" is
# N/N by construction and could never show a shortfall.
total=$((pass + failed))
if [ "$total" -ne "$declared" ]; then
  echo "::error::find-browser selftest: ran $total case(s), expected $declared - a case did not execute" >&2
  exit 1
fi
if [ "$failed" -ne 0 ]; then
  echo "::error::find-browser selftest: $failed of $declared case(s) did not bite - find-browser.sh is not trustworthy" >&2
  exit 1
fi
echo "find-browser selftest: $pass/$declared cases bite"
