#!/usr/bin/env bash
# The ONE browser resolution the workflows use. It prints the path on stdout; everything
# it says about how it decided goes to stderr, so the caller can do
#
#     echo "HOLDFAST_BROWSER=$(./scripts/find-browser.sh)" >> "$GITHUB_ENV"
#
# and the log still names the engine that was chosen.
#
# WHY THIS IS A SCRIPT AND NOT A LOOP IN A WORKFLOW STEP. It was a loop in a workflow step,
# twice: once in ci.yml's `build` job (and again in release.yml), where it PROVED a
# candidate could render, and once in ci.yml's `dashboard` job, where it asked `command -v`
# and printed `--version`. Two answers to one question, in one file, and the weaker one was
# on the job whose whole purpose is that the dashboard graders cannot come back green
# without a browser. On GitHub's runner the two answers differ: `build` measured
# /usr/bin/google-chrome, `dashboard` left the graders to find /usr/bin/chromium for
# themselves - and the graders' 90-second deadlines were the first thing in this repository
# to notice, eight minutes into every run. This repo has made the duplicated-value mistake
# with the ffmpeg pin, with the tool pins and with the base image; it is not making it with
# the measuring instrument.
#
# WHAT IT ASKS. Not "is there a file with this name" and not "does it print a version" - a
# browser that answers --version and then never paints is a browser that turns every grader
# into a deadline. Each candidate must actually RENDER a page, under a timeout, before it
# is accepted. On several distributions (Ubuntu among them) /usr/bin/chromium is a snap
# shim that can sit for ever instead of failing when its confinement is unhappy, which is
# why a real vendor binary is tried first and why the answer is a render.
#
# HOLDFAST_BROWSER, when set, is the ONLY candidate. An explicit pin that silently falls
# back to whatever is on PATH is the false green this whole layer exists to prevent: the
# gate would believe it measured the engine somebody named while measuring another, and
# nothing in the output would say so. Same rule as internal/webui's own resolver.
set -uo pipefail

probe="$(mktemp -d)"
trap 'rm -rf "$probe"' EXIT

# How long a candidate has to answer. The knob exists for find-browser-selftest.sh, which
# has to drive the hang and cannot wait a minute per case; shortening it can only make this
# script REFUSE a browser, never accept one, so it cannot buy a false green.
probe_seconds="${HOLDFAST_BROWSER_PROBE_SECONDS:-60}"

# renders reports whether the candidate can actually produce a document. A snap shim with
# unhappy confinement hangs here rather than failing, so the question is asked under a
# timeout and a candidate that does not answer is not a browser.
#
# about:blank is the subject on purpose: nothing is pending on it, so reading the document
# out of the browser's own exit settles exactly one thing - whether this binary paints.
# (That mode is forbidden to the graders, and for the opposite reason: their subject holds
# an SSE stream open, so the browser can never decide it is done. See
# internal/webui/render_idiom_test.go.)
renders() {
  HOME="$probe" timeout "$probe_seconds" "$1" --headless=new --disable-gpu --no-sandbox \
    --disable-dev-shm-usage --no-first-run --no-default-browser-check \
    --user-data-dir="$probe/profile" --dump-dom about:blank >/dev/null 2>&1
}

pinned="${HOLDFAST_BROWSER:-}"
if [ -n "$pinned" ]; then
  if renders "$pinned"; then
    echo "using the pinned engine: $pinned ($("$pinned" --version 2>/dev/null || echo 'version unknown'))" >&2
    echo "$pinned"
    exit 0
  fi
  echo "::error::HOLDFAST_BROWSER names '$pinned', which did not render a page within ${probe_seconds}s. An explicit engine pin is never replaced by a browser found on PATH, so there is nothing to fall back to" >&2
  exit 1
fi

# The order is not alphabetical: a real vendor binary comes before `chromium` for the snap
# reason above. It is the same list the graders' own resolvers carry, and the reason it is
# only a preference here is that every candidate has to render before it is taken.
for b in google-chrome google-chrome-stable chrome chromium chromium-browser; do
  p="$(command -v "$b" 2>/dev/null || true)"
  [ -n "$p" ] || continue
  if renders "$p"; then
    echo "using $b: $p ($("$p" --version 2>/dev/null || echo 'version unknown'))" >&2
    echo "$p"
    exit 0
  fi
  echo "  $p found but did not render within ${probe_seconds}s - trying the next candidate" >&2
done

echo "::error::no browser on this runner could render a page - the dashboard graders need one, and a grader that skips is a false green" >&2
exit 1
